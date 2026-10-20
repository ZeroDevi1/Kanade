"""把多条音轨（转换后的人声、伴奏、和声等）按增益混合为一个立体声 WAV。

各轨重采样到其中最高的采样率，长度取最长轨（短轨补零）。混合后峰值超过 PEAK 时整体衰减，不做硬削波。
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
import soundfile as sf
import soxr

from kanade_engine import Job

PEAK = 0.98


def _stereo(x: np.ndarray) -> np.ndarray:
    if x.ndim == 1:
        return np.stack([x, x], axis=1)
    if x.shape[1] == 1:
        return np.repeat(x, 2, axis=1)
    return x[:, :2]


def run_mix(job: Job) -> None:
    tracks = job.params.get("tracks") or []
    if not tracks:
        raise ValueError("没有要混合的音轨")
    loaded = []
    for t in tracks:
        path = Path(t.get("path") or "")
        if not path.is_file():
            raise ValueError(f"音轨不存在：{path}")
        data, sr = sf.read(str(path), always_2d=True, dtype="float32")
        loaded.append((_stereo(data), sr, 10 ** (float(t.get("gain_db") or 0) / 20)))
    job.progress(0.3, "重采样与混合")

    target = max(sr for _, sr, _ in loaded)
    resampled = [(soxr.resample(d, sr, target) if sr != target else d) * g for d, sr, g in loaded]
    length = max(len(d) for d in resampled)
    mix = np.zeros((length, 2), dtype=np.float32)
    for d in resampled:
        mix[: len(d)] += d
    peak = float(np.max(np.abs(mix)))
    if peak > PEAK:
        mix *= PEAK / peak

    name = (job.params.get("name") or job.id).replace("/", "_").replace("\\", "_")
    out = job.output_dir / f"{name}.wav"
    job.output_dir.mkdir(parents=True, exist_ok=True)
    sf.write(str(out), mix, target, subtype="PCM_16")
    job.progress(1.0, "完成")
    job.artifact(out, "audio")
