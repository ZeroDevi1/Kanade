"""IndexTTS-2.5 推理任务处理函数。

worker 同一时间只持有一个已加载的模型（GPT 约 1.5B 参数 + s2mel + BigVGAN + w2v-bert，CUDA bf16 下约 6–8 GB 显存；
情绪模型 QwenEmotion 约 0.6B，只在情绪控制时按需加载）。
各模式只提供「一段文本 → 波形」的函数，分段、逐段检查与重试、按标点停顿拼接见 kanade_engine.longtext；
上游自带的分句（max_text_tokens_per_segment）在短段上不会再切开。
"""

from __future__ import annotations

import gc
import logging
import os
import re
from pathlib import Path
from typing import Any

import numpy as np

from kanade_engine import Job, hub, longtext, pick_device, voice

log = logging.getLogger("kanade_indextts")

# 上游 infer_generator 固定以 22050 Hz 输出（BigVGAN 22 kHz 声码器）。
SAMPLE_RATE = 22050
# 推理参数的推荐值：采样参数取自 infer_v2_5.infer_generator 的默认值；情绪强度按上游 README 的建议（按文字推断情绪时
# 0.6 左右更自然）。任务参数 options 可逐项覆盖，须与 Go 端 internal/cantus/options.go 的 indexOptions 一致。
GENERATION_DEFAULTS = {"temperature": 0.8, "top_p": 0.8, "top_k": 30, "num_beams": 3, "repetition_penalty": 10.0}
EMO_TEXT_ALPHA = 0.6
# duration_factor 的取值范围（上游 README：0.5–2.0，大于 1 变慢）。
MIN_DURATION, MAX_DURATION = 0.5, 2.0

# 附属模型：上游 IndexTTS2 初始化时在 <model_dir>/hf_cache 下按以下扁平布局查找，缺失时才自行下载
# （见 indextts/utils/model_download.ensure_models_available）。这里由 download 任务预先下载，推理时不再联网。
# 每项：(本地相对路径, Hugging Face 仓库, ModelScope 仓库（None 表示 ModelScope 上没有，统一走 Hugging Face / 镜像）, 仓库内路径)。
# 仓库映射取自上游 HF_TO_MODELSCOPE_REPO_MAP；MaskGCT 的 semantic codec 只有 IndexTTS-2 使用，2.5 不需要。
AUX_FILES = [
    ("hf_cache/w2v-bert-2.0/config.json", "facebook/w2v-bert-2.0", "AI-ModelScope/w2v-bert-2.0", "config.json"),
    ("hf_cache/w2v-bert-2.0/preprocessor_config.json", "facebook/w2v-bert-2.0", "AI-ModelScope/w2v-bert-2.0", "preprocessor_config.json"),
    ("hf_cache/w2v-bert-2.0/model.safetensors", "facebook/w2v-bert-2.0", "AI-ModelScope/w2v-bert-2.0", "model.safetensors"),
    ("hf_cache/campplus_cn_common.bin", "funasr/campplus", "iic/speech_campplus_sv_zh-cn_16k-common", "campplus_cn_common.bin"),
    ("hf_cache/bigvgan/config.json", "nvidia/bigvgan_v2_22khz_80band_256x", None, "config.json"),
    ("hf_cache/bigvgan/bigvgan_generator.pt", "nvidia/bigvgan_v2_22khz_80band_256x", None, "bigvgan_generator.pt"),
]

_KANA = re.compile(r"[぀-ヿ]")
_HAN = re.compile(r"[一-鿿]")

_loaded: tuple[str, Any] | None = None  # (model_dir, IndexTTS2)


def load(job: Job, model_dir: Path, emotion: bool):
    """返回已加载的模型；emotion 为 True 时确保情绪模型 QwenEmotion 已加载。模型未下载完成时抛出 ValueError。"""
    global _loaded
    if not hub.is_complete(model_dir) or not (model_dir / "config.yaml").is_file():
        raise ValueError(f"模型尚未下载完成：{model_dir.name}")
    if not (_loaded and _loaded[0] == str(model_dir)):
        import torch

        if _loaded:
            _loaded = None
            gc.collect()
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
        # 上游 infer_v2_5 在导入时把 HF_HUB_CACHE 改成相对路径 ./checkpoints/hf_cache；附属模型已在模型目录内，
        # 导入后恢复原值，避免影响同进程中其他 huggingface 组件的缓存位置。
        saved = os.environ.get("HF_HUB_CACHE")
        from indextts.infer_v2_5 import IndexTTS2

        if saved is None:
            os.environ.pop("HF_HUB_CACHE", None)
        else:
            os.environ["HF_HUB_CACHE"] = saved
        dev = pick_device()
        job.progress(0.02, f"加载模型 {model_dir.name}（{dev.label}）")
        # bf16 只在 CUDA 上使用（上游在 MPS / CPU 上也会关闭）；不用 BigVGAN 自定义 CUDA 核（需要现场编译）、
        # DeepSpeed 与 torch.compile（Windows 上不可用或需要额外工具链）。
        tts = IndexTTS2(cfg_path=str(model_dir / "config.yaml"), model_dir=str(model_dir), use_bf16=dev.supports_half,
                        device=dev.name, use_cuda_kernel=False, use_deepspeed=False, use_torch_compile=False,
                        use_qwen_emo=False)
        _loaded = (str(model_dir), tts)
    tts = _loaded[1]
    if emotion and tts.qwen_emo is None:
        from indextts.infer_v2_5 import QwenEmotion

        job.progress(0.04, "加载情绪模型")
        tts.qwen_emo = QwenEmotion(os.path.join(str(model_dir), tts.cfg.qwen_emo_path))
    return tts


def lang_of(text: str) -> str:
    """按文字判断上游的 lang 参数：含假名为日语，含汉字为中文，否则英文。上游另支持 ES / AR，需要时由文本自行判断。"""
    if _KANA.search(text):
        return "JA"
    if _HAN.search(text):
        return "ZH"
    return "EN"


def _duration(job: Job) -> float:
    """界面的语速倍率换成上游的 duration_factor（时长倍数，大于 1 变慢）。"""
    speed = float(job.params.get("speed") or 1.0)
    return min(max(1.0 / speed, MIN_DURATION), MAX_DURATION)


def _wave(result) -> np.ndarray | None:
    """上游非流式结果为 (采样率, int16 数组 [N, 1])；转为单声道 float32。"""
    if not result:
        return None
    sr, data = result
    if sr != SAMPLE_RATE:
        raise RuntimeError(f"模型输出采样率 {sr} Hz，与预期的 {SAMPLE_RATE} Hz 不一致")
    return np.asarray(data, dtype=np.float32).reshape(-1) / 32767.0


def _run(job: Job, emotion_text: str | None) -> None:
    p = job.params
    tts = load(job, Path(p["model_dir"]), emotion_text is not None)
    v = voice.resolve(p)
    duration = _duration(job)
    opts = p.get("options") or {}
    # 推荐值叠加 options 中的同名项，按推荐值的类型转换（top_k、num_beams 为整数）。
    kw = {k: type(d)(opts[k]) if k in opts else d for k, d in GENERATION_DEFAULTS.items()}
    kw["text_normalization"] = bool(opts.get("text_normalization", True))
    if emotion_text:
        kw.update(use_emo_text=True, emo_text=emotion_text, emo_alpha=float(opts.get("emo_alpha", EMO_TEXT_ALPHA)))
    longtext.run(job, p.get("text") or "", SAMPLE_RATE, lambda seg: _wave(tts.infer(
        spk_audio_prompt=v.wav, text=seg, output_path=None, lang=lang_of(seg), duration_factor=duration,
        verbose=False, **kw)))


# ---------- 任务 ----------

def download_model(job: Job) -> None:
    """下载 IndexTTS-2.5 主模型（约 5.5 GB）与附属模型（约 2.8 GB），全部完成后才写入完成标记。"""
    p = job.params
    source = p.get("source") or "modelscope"
    model_dir = Path(p["model_dir"])
    job.progress(0, "获取模型文件列表")
    main = hub.list_files(source, p["repo"], lambda path: not path.endswith((".md", ".png", ".jpg"))
                          and path not in (".gitattributes", "qwen0.6bemo4-merge/Modelfile"))
    if not main:
        raise IOError(f"{p['repo']} 中没有需要下载的文件")
    hub.fetch_files(job, main, model_dir, "", 0.0, 0.66)
    # 附属模型逐个列出所需文件，直接下载到上游期望的本地路径（不同仓库有同名的 config.json，不能按仓库内路径保存）；
    # ModelScope 上没有的仓库走 Hugging Face（设置页的镜像生效）。
    aux = []
    for local, hf_repo, ms_repo, remote in AUX_FILES:
        src, repo = (source, ms_repo) if source == "modelscope" and ms_repo else ("huggingface", hf_repo)
        files = hub.list_files(src, repo, lambda path, remote=remote: path == remote)
        if not files:
            raise IOError(f"{repo} 中找不到 {remote}")
        aux.append((local, files[0]))
    total = sum(f.size for _, f in aux) or 1
    done = 0
    for local, f in aux:
        lo = 0.66 + 0.34 * done / total
        done += f.size
        hub.fetch_files(job, [f], model_dir, "附属模型", lo, 0.66 + 0.34 * done / total, rename=lambda _p, local=local: local)
    hub.write_marker(model_dir, source, p["repo"], [f.path for f in main] + [local for local, _ in aux])
    job.progress(1, "下载完成")


def cross_lingual(job: Job) -> None:
    _run(job, None)


def instruct(job: Job) -> None:
    desc = (job.params.get("instruct") or "").strip()
    if not desc:
        raise ValueError("请填写情绪描述，例如：非常开心地说")
    _run(job, desc)
