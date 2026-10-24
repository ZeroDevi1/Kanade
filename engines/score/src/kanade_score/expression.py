"""主唱的表情特征：供 Go 端把原唱的音高、响度、气声、张力与发声写成 Synthesizer V 的参数曲线（见 internal/score/svexpr.go）。

输入是分离出的主唱干声（与旋律识别相同的 _vocals_karaoke），统一重采样到 16 kHz 单声道，按 10 ms 一帧给出：

- f0：基频（Hz），librosa.yin（65–1100 Hz）。yin 每帧都给出一个值，不判断有无声；Go 端只取音符内、周期性足够的帧，
  并把八度误差折回音符附近。不用 pyin：在 4 分 24 秒的歌上实测 188 秒，yin 约 10 秒。
- periodicity：基频周期处的归一化自相关（0–1），越高越接近纯周期的浊音；低值对应气声、清音或噪声。
- loudness：帧 RMS（dBFS）。
- tilt：1.5–5 kHz 与 80–1.5 kHz 的能量比（dB），频谱越「亮」越高，用来估计张力。
- noise：5–8 kHz 与 80 Hz–5 kHz 的能量比（dB），高频噪声越多越高，与 periodicity 一起估计气声。

这些都是声学特征而不是 Synthesizer V 的参数值：换算（按整首歌归一化、平滑、按强度缩放）在 Go 端做，便于在界面上调节而不必重算。
"""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from kanade_engine import Job

SR = 16000
HOP = 160  # 10 ms
FRAME = 1024


def _band(S: np.ndarray, freqs: np.ndarray, lo: float, hi: float) -> np.ndarray:
    return S[(freqs >= lo) & (freqs < hi)].sum(axis=0)


def features(y: np.ndarray) -> dict:
    """计算 16 kHz 单声道 y 的逐帧特征（各数组等长，帧 i 的中心在 i × 10 ms）。"""
    import librosa

    f0 = librosa.yin(y, fmin=65, fmax=1100, sr=SR, frame_length=FRAME, hop_length=HOP)
    n = len(f0)

    # 周期性：每帧取前半帧与其后移一个基频周期的片段，求归一化相关系数。
    frames = librosa.util.frame(np.pad(y, (FRAME // 2, FRAME // 2)), frame_length=FRAME, hop_length=HOP).T[:n]
    lag = np.clip(np.round(SR / np.maximum(f0, 1)).astype(int), 2, FRAME // 2 - 1)
    w = FRAME // 2
    a = frames[:, :w]
    b = np.take_along_axis(frames, np.arange(w)[None, :] + lag[:, None], axis=1)
    periodicity = (a * b).sum(axis=1) / np.sqrt((a * a).sum(axis=1) * (b * b).sum(axis=1) + 1e-12)

    S = np.abs(librosa.stft(y, n_fft=FRAME, hop_length=HOP)) ** 2
    S = S[:, :n]
    freqs = librosa.fft_frequencies(sr=SR, n_fft=FRAME)
    low, mid, high = _band(S, freqs, 80, 1500), _band(S, freqs, 1500, 5000), _band(S, freqs, 5000, SR / 2 + 1)
    eps = 1e-10
    rms = librosa.feature.rms(y=y, frame_length=FRAME, hop_length=HOP)[0][:n]
    return {
        "f0": np.round(f0, 1),
        "periodicity": np.round(np.clip(periodicity, 0, 1), 3),
        "loudness": np.round(np.maximum(20 * np.log10(rms + eps), -90), 1),
        "tilt": np.round(10 * np.log10((mid + eps) / (low + eps)), 1),
        "noise": np.round(10 * np.log10((high + eps) / (low + mid + eps)), 1),
    }


def run_expression(job: Job) -> None:
    import soundfile as sf
    import soxr

    src = Path(job.params.get("input") or "")
    if not src.is_file():
        raise ValueError(f"主唱文件不存在：{src}")
    job.progress(0.05, "读取主唱")
    y, sr = sf.read(str(src), dtype="float32", always_2d=True)
    y = soxr.resample(y.mean(axis=1), sr, SR).astype(np.float32)
    job.check_cancelled()
    job.progress(0.2, "提取音高与音色特征")
    feats = features(y)
    job.check_cancelled()
    out_dir = job.output_dir / job.id
    out_dir.mkdir(parents=True, exist_ok=True)
    out = out_dir / "expression.json"
    result = {"version": 1, "hop": HOP / SR, "source": str(src)}
    result.update({k: v.tolist() for k, v in feats.items()})
    out.write_text(json.dumps(result, separators=(",", ":")), encoding="utf-8")
    job.artifact(out, "expression")
    job.progress(1.0, f"完成：{len(feats['f0'])} 帧")
