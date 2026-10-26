"""Qwen3-TTS 推理任务处理函数。

worker 同一时间只持有一个已加载的模型（1.7B 在 CUDA bf16 下约 4–6 GB 显存）；切换模型时先释放旧模型。
各模式只提供「一段文本 → 波形」的函数，分段、逐段检查与重试、按标点停顿拼接见 kanade_engine.longtext。
上游没有文本归一化，分段按标点进行；语言统一交给模型自动识别（language="Auto"）。
"""

from __future__ import annotations

import gc
import json
import logging
from pathlib import Path
from typing import Any

from kanade_engine import Job, hub, longtext, pick_device, voice

log = logging.getLogger("kanade_qwentts")

# config.json 的 tts_model_type → 支持的模式，与 Go 端 internal/cantus/models.go 的 familyModes 一致。
MODES = {
    "base": {"zero_shot", "cross_lingual"},
    "voice_design": {"voice_design"},
    "custom_voice": {"sft"},
}

# 每段最多生成的 codec 帧数：tokenizer 每帧 1920 个采样（24 kHz 下 12.5 帧 / 秒）。上游默认 8192 帧（约 11 分钟），
# 失控时一段要跑几分钟；这里按字数估计时长的 3 倍再加 10 秒封顶，足够正常朗读，超出部分由 longtext 判为拖长后重试。
FRAMES_PER_SECOND = 12.5
# tokenizer 解码为 24 kHz（speech_tokenizer/config.json 的 output_sample_rate）；longtext 需要在合成前知道采样率，
# _wave 逐段校验，上游变更时报错而不是写出变调的音频。
SAMPLE_RATE = 24000

# 采样参数的推荐值（Qwen3-TTS 各模型 generation_config.json），任务参数 options 可逐项覆盖。
# 须与 Go 端 internal/cantus/options.go 的 qwenOptions 一致。
SAMPLING_DEFAULTS = {
    "temperature": 0.9, "top_p": 1.0, "top_k": 50, "repetition_penalty": 1.05,
    "subtalker_temperature": 0.9, "subtalker_top_k": 50, "subtalker_top_p": 1.0,
}
_INT_KEYS = {"top_k", "subtalker_top_k"}

_loaded: tuple[str, Any] | None = None  # (model_dir, Qwen3TTSModel)
_prompts: dict[str, Any] = {}  # 音色缓存键 → create_voice_clone_prompt 的结果，随模型切换清空


def model_type(model_dir: Path) -> str:
    """返回 config.json 的 tts_model_type（base / voice_design / custom_voice）。"""
    try:
        return json.loads((model_dir / "config.json").read_text(encoding="utf-8"))["tts_model_type"]
    except (OSError, KeyError, ValueError) as exc:
        raise ValueError(f"{model_dir} 不是 Qwen3-TTS 模型目录") from exc


def load(job: Job, model_dir: Path, mode: str):
    """返回已加载的模型；模型未下载完成或不支持 mode 时抛出 ValueError。"""
    global _loaded
    if not hub.is_complete(model_dir):
        raise ValueError(f"模型尚未下载完成：{model_dir.name}")
    kind = model_type(model_dir)
    if mode not in MODES.get(kind, ()):
        raise ValueError(f"{model_dir.name} 不支持该模式（{mode}）")
    if _loaded and _loaded[0] == str(model_dir):
        return _loaded[1]

    import torch
    from qwen_tts import Qwen3TTSModel

    if _loaded:
        _loaded = None
        _prompts.clear()
        gc.collect()
        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    dev = pick_device()
    job.progress(0.02, f"加载模型 {model_dir.name}（{dev.label}）")
    # bf16 只在 CUDA 上使用；MPS / CPU 用 fp32（MPS 的半精度在音频模型上容易出现 NaN）。
    # 注意力用 PyTorch 自带的 sdpa：flash-attn 在 Windows 上没有官方预编译包。
    dtype = torch.bfloat16 if dev.supports_half else torch.float32
    model = Qwen3TTSModel.from_pretrained(str(model_dir), device_map=dev.name, dtype=dtype, attn_implementation="sdpa")
    _loaded = (str(model_dir), model)
    return model


def _max_frames(text: str) -> int:
    want = longtext.expected_seconds(text, 1.0)
    return int(FRAMES_PER_SECOND * max(want * 3, want + 10))


def _wave(result) -> Any:
    wavs, sr = result
    if sr != SAMPLE_RATE:
        raise RuntimeError(f"模型输出采样率 {sr} Hz，与预期的 {SAMPLE_RATE} Hz 不一致")
    return wavs[0] if wavs else None


def _common(job: Job, mode: str):
    """加载模型并返回 (任务参数, 模型, 采样参数)。采样参数为推荐值叠加任务参数 options 中的同名项。"""
    p = job.params
    model = load(job, Path(p["model_dir"]), mode)
    opts = p.get("options") or {}
    kw = {k: (int(opts[k]) if k in _INT_KEYS else float(opts[k])) if k in opts else v for k, v in SAMPLING_DEFAULTS.items()}
    return p, model, kw


# ---------- 任务 ----------

def download_model(job: Job) -> None:
    p = job.params
    # 仓库只含推理所需文件（权重、配置与 speech_tokenizer/），全部下载；README 与图片不需要。
    hub.fetch_repo(job, p.get("source") or "modelscope", p["repo"], Path(p["model_dir"]),
                   lambda path: not path.endswith((".md", ".png", ".jpg")) and path != ".gitattributes")
    job.progress(1, "下载完成")


def _clone(job: Job, xvec_only: bool) -> None:
    p, model, kw = _common(job, "cross_lingual" if xvec_only else "zero_shot")
    v = voice.resolve(p, need_text=not xvec_only)
    key = f"{v.key}:{xvec_only}"
    if key not in _prompts:
        # 参考音频的特征（说话人向量与 codec）只提取一次，各段复用。
        job.progress(0.05, "提取参考音色")
        _prompts[key] = model.create_voice_clone_prompt(ref_audio=v.wav, ref_text=v.text or None, x_vector_only_mode=xvec_only)
    prompt = _prompts[key]
    longtext.run(job, p.get("text") or "", SAMPLE_RATE, lambda seg: _wave(model.generate_voice_clone(
        text=seg, language="Auto", voice_clone_prompt=prompt, max_new_tokens=_max_frames(seg), **kw)))


def zero_shot(job: Job) -> None:
    _clone(job, xvec_only=False)


def cross_lingual(job: Job) -> None:
    _clone(job, xvec_only=True)


def voice_design(job: Job) -> None:
    p, model, kw = _common(job, "voice_design")
    desc = (p.get("instruct") or "").strip()
    if not desc:
        raise ValueError("请用文字描述想要的音色")
    # 每段都按同一描述重新生成音色，段与段之间音色可能略有差异；固定随机种子可减小差异。
    # 需要整篇完全一致时，先用一句话设计出满意的音色、保存为音色后用 Base 模型克隆。
    longtext.run(job, p.get("text") or "", SAMPLE_RATE, lambda seg: _wave(model.generate_voice_design(
        text=seg, instruct=desc, language="Auto", max_new_tokens=_max_frames(seg), **kw)))


def sft(job: Job) -> None:
    p, model, kw = _common(job, "sft")
    spk = (p.get("spk_id") or "").strip().lower()  # 上游的音色名不区分大小写，按小写比对
    supported = model.get_supported_speakers() or []
    if spk not in supported:
        raise ValueError(f"预设音色不存在：{p.get('spk_id')}，可用：{'、'.join(supported)}")
    style = (p.get("instruct") or "").strip() or None
    longtext.run(job, p.get("text") or "", SAMPLE_RATE, lambda seg: _wave(model.generate_custom_voice(
        text=seg, speaker=spk, language="Auto", instruct=style, max_new_tokens=_max_frames(seg), **kw)))
