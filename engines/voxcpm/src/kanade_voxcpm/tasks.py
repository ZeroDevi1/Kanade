"""VoxCPM 推理任务处理函数。

worker 同一时间只持有一个已加载的模型（VoxCPM2 约 2B 参数，CUDA bf16 下约 6–8 GB 显存）；切换模型时先释放旧模型。
各模式只提供「一段文本 → 波形」的函数，分段、逐段检查与重试、按标点停顿拼接见 kanade_engine.longtext。

VoxCPM 的控制方式是在正文前加括号描述，见上游 README：
- 音色设计：不给参考音频，"(年轻女性，温柔甜美)正文"；
- 风格控制：给参考音频（reference_wav_path），"(稍快，开心)正文"。
"""

from __future__ import annotations

import gc
import json
import logging
from pathlib import Path
from typing import Any

from kanade_engine import Job, hub, longtext, pick_device, voice

log = logging.getLogger("kanade_voxcpm")

# config.json 的 architecture → 支持的模式，与 Go 端 internal/cantus/models.go 的 familyModes 一致。
# reference_wav_path（只用参考音频克隆、风格控制）与括号描述只有 VoxCPM2 支持；1.5 只能续写克隆。
MODES = {
    "voxcpm2": {"zero_shot", "cross_lingual", "instruct", "voice_design"},
    "voxcpm": {"zero_shot"},
}

# generate 参数的推荐值（上游 README：cfg 2.0、10 步，调高质量更好但更慢；失控重试为上游默认），任务参数 options
# 可逐项覆盖。须与 Go 端 internal/cantus/options.go 的 voxOptions 一致。
GENERATE_DEFAULTS = {
    "cfg_value": 2.0, "inference_timesteps": 10, "normalize": False,
    "retry_badcase": True, "retry_badcase_max_times": 3, "retry_badcase_ratio_threshold": 6.0,
}

_loaded: tuple[str, Any] | None = None  # (model_dir, VoxCPM)


def architecture(model_dir: Path) -> str:
    try:
        return json.loads((model_dir / "config.json").read_text(encoding="utf-8")).get("architecture", "voxcpm").lower()
    except (OSError, ValueError) as exc:
        raise ValueError(f"{model_dir} 不是 VoxCPM 模型目录") from exc


def load(job: Job, model_dir: Path, mode: str):
    """返回已加载的模型；模型未下载完成或不支持 mode 时抛出 ValueError。"""
    global _loaded
    if not hub.is_complete(model_dir):
        raise ValueError(f"模型尚未下载完成：{model_dir.name}")
    arch = architecture(model_dir)
    if mode not in MODES.get(arch, ()):
        raise ValueError(f"{model_dir.name} 不支持该模式（{mode}）")
    if _loaded and _loaded[0] == str(model_dir):
        return _loaded[1]

    import torch
    from voxcpm import VoxCPM

    if _loaded:
        _loaded = None
        gc.collect()
        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    dev = pick_device()
    job.progress(0.02, f"加载模型 {model_dir.name}（{dev.label}）")
    # 精度由上游按设备选择（CUDA 用 config 的 bf16，MPS 改 fp32）。不启用参考音频降噪（需要 modelscope 的
    # ZipEnhancer，依赖已排除），不用 torch.compile（Windows 上没有 triton，编译失败会退回但白等几分钟）。
    model = VoxCPM(voxcpm_model_path=str(model_dir), zipenhancer_model_path=None, enable_denoiser=False,
                   optimize=False, device=dev.name)
    _loaded = (str(model_dir), model)
    return model


def _settings(job: Job) -> dict:
    """推荐值叠加任务参数 options 中的同名项，按推荐值的类型转换。"""
    opts = job.params.get("options") or {}
    return {k: type(v)(opts[k]) if k in opts else v for k, v in GENERATE_DEFAULTS.items()}


def _run(job: Job, model, make_text, **prompt) -> None:
    # retry_badcase：上游按音频 / 文本长度比检测失控并重试，与 longtext 的时长检查互补（上游重试更早截断，代价更小）。
    kw = _settings(job)
    longtext.run(job, job.params.get("text") or "", model.tts_model.sample_rate,
                 lambda seg: model.generate(text=make_text(seg), **kw, **prompt))


def _paren(desc: str) -> str:
    """括号描述：去掉用户可能自带的括号，统一用半角括号包起来。"""
    desc = desc.strip().strip("()（）").strip()
    return f"({desc})" if desc else ""


# ---------- 任务 ----------

def download_model(job: Job) -> None:
    p = job.params
    hub.fetch_repo(job, p.get("source") or "modelscope", p["repo"], Path(p["model_dir"]),
                   lambda path: not path.endswith((".md", ".png", ".jpg")) and path != ".gitattributes")
    job.progress(1, "下载完成")


def zero_shot(job: Job) -> None:
    p = job.params
    model = load(job, Path(p["model_dir"]), "zero_shot")
    v = voice.resolve(p, need_text=True)
    prompt = {"prompt_wav_path": v.wav, "prompt_text": v.text}
    if architecture(Path(p["model_dir"])) == "voxcpm2":
        # 上游 README 的「极致克隆」：同一段参考同时作为续写提示与参考音色，音色相似度更高。
        prompt["reference_wav_path"] = v.wav
    _run(job, model, lambda seg: seg, **prompt)


def cross_lingual(job: Job) -> None:
    p = job.params
    model = load(job, Path(p["model_dir"]), "cross_lingual")
    v = voice.resolve(p)
    _run(job, model, lambda seg: seg, reference_wav_path=v.wav)


def instruct(job: Job) -> None:
    p = job.params
    model = load(job, Path(p["model_dir"]), "instruct")
    style = _paren(p.get("instruct") or "")
    if not style:
        raise ValueError("请填写风格描述，例如：稍快，开心")
    v = voice.resolve(p)
    _run(job, model, lambda seg: style + seg, reference_wav_path=v.wav)


def voice_design(job: Job) -> None:
    p = job.params
    model = load(job, Path(p["model_dir"]), "voice_design")
    desc = _paren(p.get("instruct") or "")
    if not desc:
        raise ValueError("请用文字描述想要的音色")
    # 每段都按同一描述重新生成音色，段与段之间音色可能略有差异；固定随机种子可减小差异。
    _run(job, model, lambda seg: desc + seg)
