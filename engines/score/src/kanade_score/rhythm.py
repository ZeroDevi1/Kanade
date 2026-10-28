"""节拍、小节线、BPM 与拍号。

- beat_this（默认）：直接输出拍点与小节线（强拍），拍号由相邻小节线之间的拍数取众数得到。
- librosa（快速、无需下载）：只输出拍点；小节长度用拍同步的起音强度做自相关，在 2 / 3 / 4 拍中取最强的周期。

BPM 取拍间隔中位数（对个别漏拍、多拍稳健）；按小节计算局部 BPM 作为速度曲线，用于判断是否变速。
"""

from __future__ import annotations

from collections import Counter

import numpy as np

from kanade_engine import Job, pick_device

from . import models

# 匀速判定：至少 STABLE_SHARE 的小节，其局部 BPM 在中位数 ±STABLE_TOL 以内。±5% 约为人耳对速度变化的分辨阈值；
# 留 10% 余量给分轨边界、渐慢收尾与偶发的跟踪错误。
STABLE_TOL = 0.05
STABLE_SHARE = 0.9


# 拍点规整的阈值（相对中位拍间隔）：小于 MERGE 的两拍视为重复，大于 GAP 的空档视为漏拍。
MERGE_RATIO = 0.4
GAP_RATIO = 1.5


def regularize(beats: np.ndarray) -> np.ndarray:
    """合并重复拍、补齐漏拍。

    beat_this 的 minimal 后处理逐帧取峰，实测会在相隔约 0.08 秒处给出两拍（《我不想念》224.04 / 224.12 秒），
    也会在弱拍处漏拍；上游更稳健的 DBN 后处理依赖 madmom（模型为非商用许可），因此在这里做轻量规整。
    """
    if len(beats) < 4:
        return beats
    ibi = float(np.median(np.diff(beats)))
    merged = [float(beats[0])]
    for b in beats[1:]:
        if b - merged[-1] < MERGE_RATIO * ibi:
            merged[-1] = (merged[-1] + float(b)) / 2
        else:
            merged.append(float(b))
    out = [merged[0]]
    for b in merged[1:]:
        gap = b - out[-1]
        if gap > GAP_RATIO * ibi:
            n = int(round(gap / ibi))
            # 起点须先取出：生成器会在 extend 过程中惰性读取 out[-1]，补拍逐个累加后越过 b，拍点失去单调。
            start = out[-1]
            out.extend([start + gap * k / n for k in range(1, n)])
        out.append(b)
    return np.asarray(out)


def snap_downbeats(beats: np.ndarray, downbeats: np.ndarray) -> np.ndarray:
    """把小节线吸附到最近的拍点并去重。beat_this 的 minimal 后处理分别取峰值，小节线偶尔与拍点相差一两帧，
    或在同一位置给出两条（实测《我不想念》225.0 秒处），直接按区间数拍会得到 1 拍、5 拍的小节。"""
    if len(beats) == 0 or len(downbeats) == 0:
        return downbeats
    idx = np.unique(np.abs(beats[None, :] - downbeats[:, None]).argmin(axis=1))
    return beats[idx]


def track_beat_this(job: Job, y: np.ndarray, sr: int) -> tuple[np.ndarray, np.ndarray]:
    """返回 (拍点, 小节线)，单位秒。"""
    from beat_this.inference import Audio2Beats

    dev = pick_device(allow_mps=False)
    job.progress(job.fraction, f"节拍检测（beat_this，{dev.label}）")
    tracker = Audio2Beats(checkpoint_path=str(models.path(models.BEAT_THIS[0].local)), device=dev.name,
                          float16=dev.supports_half)
    beats, downbeats = tracker(y, sr)
    return np.asarray(beats, dtype=float), np.asarray(downbeats, dtype=float)


def track_librosa(job: Job, y: np.ndarray, sr: int) -> tuple[np.ndarray, np.ndarray]:
    """librosa 拍点跟踪，小节线按自相关估计的小节长度从最强的一拍起排布。"""
    import librosa

    job.progress(job.fraction, "节拍检测（librosa）")
    mono = y.mean(axis=1) if y.ndim == 2 else y
    onset = librosa.onset.onset_strength(y=mono, sr=sr)
    _, frames = librosa.beat.beat_track(onset_envelope=onset, sr=sr)
    beats = librosa.frames_to_time(frames, sr=sr)
    if len(frames) < 8:
        return beats, np.array([])
    strength = onset[np.minimum(frames, len(onset) - 1)]
    s = strength - strength.mean()
    # 自相关在 2 / 3 / 4 拍的滞后上取最大，即最可能的小节长度；再取使强拍平均起音最强的相位。
    period = max((2, 3, 4), key=lambda k: float(np.dot(s[:-k], s[k:])) / (len(s) - k))
    phase = max(range(period), key=lambda p: float(strength[p::period].mean()))
    return beats, beats[phase::period]


def tempo(beats: np.ndarray, downbeats: np.ndarray, fold: tuple[float, float] | None) -> dict:
    """BPM、按小节的速度曲线与是否匀速。fold=(lo, hi) 时把 BPM 按倍数折叠到区间内（处理半速 / 倍速误判）。"""
    if len(beats) < 2:
        return {"bpm": None, "stable": None, "curve": []}
    ibi = np.diff(beats)
    bpm = 60.0 / float(np.median(ibi))
    factor = 1.0
    if fold:
        lo, hi = fold
        while bpm * factor < lo and bpm * factor * 2 <= hi:
            factor *= 2
        while bpm * factor > hi and bpm * factor / 2 >= lo:
            factor /= 2
    # 速度曲线：每小节（没有小节线时每 4 拍）的拍间隔中位数，对个别漏拍、多拍稳健。
    marks = downbeats if len(downbeats) >= 3 else beats[::4]
    curve = []
    for a, b in zip(marks[:-1], marks[1:]):
        inside = ibi[(beats[:-1] >= a - 1e-3) & (beats[:-1] < b - 1e-3)]
        if len(inside):
            curve.append([round(float(a), 3), round(60.0 / float(np.median(inside)) * factor, 2)])
    local = np.array([c[1] for c in curve]) if curve else np.array([bpm * factor])
    center = float(np.median(local))
    share = float(np.mean(np.abs(local - center) <= STABLE_TOL * center)) if center > 0 else 1.0
    return {"bpm": round(bpm * factor, 2), "stable": share >= STABLE_SHARE, "steady_share": round(share, 3),
            "curve": curve, "folded": factor != 1.0}


def meter(beats: np.ndarray, downbeats: np.ndarray) -> dict:
    """拍号：相邻小节线之间拍数的众数；置信度为符合众数的小节比例。

    beat_this 跟踪的是听感上的拍（复拍子 6/8 通常按附点四分音符计为 2 拍），因此 2 拍记为「2/4（或 6/8）」。
    """
    if len(downbeats) < 3 or len(beats) < 4:
        return {"label": None, "beats_per_bar": None, "confidence": 0.0}
    counts = [int(((beats >= a - 1e-3) & (beats < b - 1e-3)).sum()) for a, b in zip(downbeats[:-1], downbeats[1:])]
    counts = [c for c in counts if c > 0]
    if not counts:
        return {"label": None, "beats_per_bar": None, "confidence": 0.0}
    n, hits = Counter(counts).most_common(1)[0]
    label = {2: "2/4（或 6/8）", 3: "3/4", 4: "4/4", 6: "6/8", 12: "12/8"}.get(n, f"{n}/4")
    return {"label": label, "beats_per_bar": n, "confidence": round(hits / len(counts), 3)}
