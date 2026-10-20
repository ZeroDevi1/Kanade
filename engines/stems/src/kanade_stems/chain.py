"""分轨任务实现。

净化链与 VoiceLab workflows/msst 的处理链一致（模型、overlap 与输出后缀相同），区别是底层换成 MIT 许可的
audio-separator（MSST-WebUI 为 AGPL-3.0）。每一步都新建 Separator：overlap 是 Separator 级参数，各步不同。
"""

from __future__ import annotations

import logging
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

from kanade_engine import Job

from . import MODEL_DIR
from .download import patch_downloader

log = logging.getLogger("kanade_stems")


@dataclass(frozen=True)
class Step:
    id: str
    title: str
    model: str
    # source 是输入：「input」为预转换后的原曲，其余为前一步输出的键。
    source: str
    overlap: int
    # outputs 把模型的 stem 名映射到输出键；输出文件名为 <曲名>_<输出键>.wav。
    outputs: dict[str, str] = field(default_factory=dict)


# 参数取自 VoiceLab workflows/msst/README.md（chunk_size 等其余参数沿用模型自带配置）。
STEPS: list[Step] = [
    Step("other", "伴奏", "melband_roformer_inst_v1e.ckpt", "input", 4, {"other": "other"}),
    Step("vocals", "干声", "melband_roformer_big_beta5e.ckpt", "input", 2, {"vocals": "vocals"}),
    # karaoke 模型的 Vocals 是主唱，Instrumental 在纯人声输入下即和声。
    Step("karaoke", "去和声", "mel_band_roformer_karaoke_aufr33_viperx_sdr_10.1956.ckpt", "vocals", 4,
         {"Vocals": "vocals_karaoke", "Instrumental": "vocals_other"}),
    Step("noreverb", "去混响", "dereverb_mel_band_roformer_anvuew_sdr_19.1729.ckpt", "vocals_karaoke", 4,
         {"noreverb": "vocals_karaoke_noreverb"}),
    Step("dry", "降噪", "denoise_mel_band_roformer_aufr33_sdr_27.9959.ckpt", "vocals_karaoke_noreverb", 4,
         {"dry": "vocals_karaoke_noreverb_dry"}),
]
_BY_ID = {s.id: s for s in STEPS}


def resolve_steps(ids: list[str] | None) -> list[Step]:
    """返回要执行的步骤（按链顺序），自动补上前置步骤；ids 为空表示全部。"""
    if not ids:
        return list(STEPS)
    unknown = [i for i in ids if i not in _BY_ID]
    if unknown:
        raise ValueError(f"未知步骤：{unknown}")
    need = set(ids)
    # 逆序补齐依赖：某步的 source 来自哪一步，那一步也必须执行。
    for s in reversed(STEPS):
        if s.id in need and s.source != "input":
            need |= {p.id for p in STEPS if s.source in p.outputs.values()}
    return [s for s in STEPS if s.id in need]


def _separator(output_dir: Path, overlap: int | None, single_stem: str | None):
    from audio_separator.separator import Separator

    params = {"segment_size": 256, "override_model_segment_size": False, "batch_size": 1, "overlap": overlap, "pitch_shift": 0}
    return Separator(
        model_file_dir=str(MODEL_DIR),
        output_dir=str(output_dir),
        output_format="WAV",
        output_single_stem=single_stem,
        use_soundfile=True,
        mdxc_params=params,
        log_level=logging.WARNING,
    )


def preconvert(src: Path, dst_dir: Path) -> Path:
    """用 ffmpeg 转成 44.1 kHz 立体声 WAV（与模型配置一致，也统一了 MP3 / M4A 等输入）。"""
    dst_dir.mkdir(parents=True, exist_ok=True)
    out = dst_dir / f"{src.stem}.wav"
    cmd = ["ffmpeg", "-y", "-v", "error", "-i", str(src), "-ar", "44100", "-ac", "2", str(out)]
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError(f"ffmpeg 转换失败：{p.stderr.strip()[-500:]}")
    return out


def _output_dir(job: Job, src: Path) -> Path:
    custom = job.params.get("output_dir")
    return Path(custom) if custom else job.output_dir / f"{job.id}-{src.stem}"


def run_chain(job: Job) -> None:
    src = Path(job.params.get("input") or "")
    if not src.is_file():
        raise ValueError(f"输入文件不存在：{src}")
    steps = resolve_steps(job.params.get("steps"))
    out_dir = _output_dir(job, src)
    patch_downloader(job)

    job.progress(0.01, "转换为 44.1 kHz 立体声 WAV")
    files: dict[str, Path] = {"input": preconvert(src, out_dir / "_input")}
    stem = src.stem
    for i, step in enumerate(steps):
        base = i / len(steps)
        job.progress(base + 0.01, f"[{i + 1}/{len(steps)}] {step.title}：加载模型 {step.model}")
        single = next(iter(step.outputs)) if len(step.outputs) == 1 else None
        sep = _separator(out_dir, step.overlap, single)
        sep.load_model(model_filename=step.model)
        job.progress(base + 0.02, f"[{i + 1}/{len(steps)}] {step.title}：分离中")
        names = {k: f"{stem}_{v}" for k, v in step.outputs.items()}
        sep.separate(str(files[step.source]), custom_output_names=names)
        for key in step.outputs.values():
            path = out_dir / f"{stem}_{key}.wav"
            if not path.is_file() or path.stat().st_size == 0:
                raise RuntimeError(f"缺少输出：{path.name}")
            files[key] = path
            job.artifact(path, "audio")
        job.check_cancelled()
    job.progress(1.0, "完成")


def run_separate(job: Job) -> None:
    src = Path(job.params.get("input") or "")
    model = job.params.get("model") or ""
    if not src.is_file():
        raise ValueError(f"输入文件不存在：{src}")
    if not model:
        raise ValueError("请指定模型")
    out_dir = _output_dir(job, src)
    patch_downloader(job)
    job.progress(0.02, f"加载模型 {model}")
    sep = _separator(out_dir, None, job.params.get("single_stem") or None)
    sep.load_model(model_filename=model)
    job.progress(0.1, "分离中")
    for name in sep.separate(str(src)):
        path = Path(name)
        job.artifact(path if path.is_absolute() else out_dir / path, "audio")
    job.progress(1.0, "完成")


def run_download(job: Job) -> None:
    """预下载模型。models 为空时下载净化链的全部模型。"""
    models = job.params.get("models") or [s.model for s in STEPS]
    patch_downloader(job)
    for i, m in enumerate(models):
        job.progress(i / len(models), f"下载 {m}")
        # Separator 构造时会创建 output_dir；预下载不产生输出，指向任务输出目录即可。
        sep = _separator(job.output_dir, None, None)
        # download_model_files 只下载与解析模型元数据，不加载权重。
        sep.download_model_files(m)
        job.check_cancelled()
    job.progress(1.0, "下载完成")


def model_ready(model: str) -> bool:
    return (MODEL_DIR / model).is_file()

