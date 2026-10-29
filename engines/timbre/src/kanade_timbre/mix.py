"""翻唱混音：响度对齐、人声混响发送与母带输出。

参数（Go 端 internal/timbre/mix.go 组装）::

    tracks: [{path, role, ref?, gain_db?}]
        role 为 vocal（主唱）、harmony（和声）、inst（伴奏）或 other（原样相加）。
        ref 是对齐响度用的参考轨：转换后的主唱 / 和声给出原曲对应分轨，演唱段 RMS 对齐到参考；
        没有 ref 时按智能推荐（见 _recommended_db）。gain_db 是在推荐电平之上的偏移。
    widen:  {enabled, width, delay_ms, detune_cents} 和声加宽（见 widen），在对齐响度之前处理
    reverb: {enabled, rt60, predelay_ms, wet_db}   对 vocal / harmony 做混响发送，wet_db 为湿声相对干声的电平
    master: {peak_db, mp3}                         成品整体增益到峰值 peak_db（默认 −1 dBFS），24 bit WAV，可选再导出 MP3
    name:   输出文件名（不含扩展名）

输出：<name>.wav（及 <name>.mp3），以及 <name>.json 报告实际应用的电平，供界面显示。
做法与数值来自 2026-10-20 手工后期的实测，见 docs/roadmap.md 历史版本与 docs/stems-evaluation.md。
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path

import numpy as np
import soundfile as sf
import soxr

from kanade_engine import Job

ROLES = ("vocal", "harmony", "inst", "other")

# 演唱段检测：50 ms 帧、25 ms 步长；帧 RMS 高于全曲最响帧 −40 dB（且不低于 −60 dBFS）视为有声。
# −40 dB 的动态范围覆盖主歌弱唱，又能排除分轨残留（无人声段残留在 −60 dB 以下，见 stems-evaluation.md 第 2 节）。
FRAME_MS, HOP_MS = 50, 25
GATE_RANGE_DB, GATE_FLOOR_DB = 40.0, -60.0

# 没有参考轨时的推荐电平（演唱段 RMS 相对值）：
# - 主唱与伴奏在演唱段等响（0 dB），流行混音的常见起点，可用 gain_db 微调；
# - 和声比主唱低 13 dB：实测听感接近原曲时主唱 −16.0、和声 −29.2 dBFS。
VOCAL_OVER_INST_DB = 0.0
HARMONY_UNDER_VOCAL_DB = 13.2
# 连伴奏也没有时主唱的目标电平，取实测对齐值。
VOCAL_ABS_DB = -16.0

# 混响默认值：RT60 1.6 s、预延迟 25 ms、湿声比干声低 12 dB；和声的湿声再多 3 dB，让和声退到主唱后面。
REVERB_DEFAULT = {"enabled": True, "rt60": 1.6, "predelay_ms": 25.0, "wet_db": -12.0}
HARMONY_EXTRA_WET_DB = 3.0
REVERB_LOWPASS_HZ = 6000.0
MASTER_PEAK_DB = -1.0

# 和声加宽默认值：左右宽度 0.8、平均晚于主唱 12 ms、音高摆动 ±6 cent。
# 同一模型转换出的和声与主唱音色、节奏几乎一致，叠在中间听起来像主唱变厚而不像合唱；
# 让左右声道各自错位（时间与音高都轻微不同）是人工双轨（ADT）的常见做法。12 ms 在哈斯效应范围内，
# 听感上不会是"晚了"；±6 cent 在常见的 5～15 cent 双轨失谐范围内，再大会像跑调。均为经验值。
WIDEN_DEFAULT = {"enabled": True, "width": 0.8, "delay_ms": 12.0, "detune_cents": 6.0}
# 左右两路延迟调制的 LFO 频率（Hz）：取不成整数比的两个慢速值，两路的错位不会周期性地重新对齐。
WIDEN_LFO_HZ = (0.45, 0.61)


def _stereo(x: np.ndarray) -> np.ndarray:
    if x.ndim == 1:
        return np.stack([x, x], axis=1)
    if x.shape[1] == 1:
        return np.repeat(x, 2, axis=1)
    return x[:, :2]


def _db(x: float) -> float:
    return 20 * np.log10(max(x, 1e-10))


def _frames_rms(x: np.ndarray, sr: int) -> np.ndarray:
    """单声道帧 RMS（线性），每 HOP_MS 一帧。"""
    mono = x.mean(axis=1) if x.ndim == 2 else x
    win, hop = int(sr * FRAME_MS / 1000), int(sr * HOP_MS / 1000)
    if len(mono) < win:
        return np.array([np.sqrt(np.mean(mono ** 2))]) if len(mono) else np.zeros(1)
    n = (len(mono) - win) // hop + 1
    # 用累积和计算滑动窗口能量，避免逐帧循环。
    c = np.concatenate([[0.0], np.cumsum(mono.astype(np.float64) ** 2)])
    starts = np.arange(n) * hop
    return np.sqrt((c[starts + win] - c[starts]) / win)


def active_mask(x: np.ndarray, sr: int) -> np.ndarray:
    """演唱段（有声帧）掩码。"""
    rms = _frames_rms(x, sr)
    peak = float(rms.max()) if rms.size else 0.0
    if peak <= 0:
        return np.zeros_like(rms, dtype=bool)
    thr = max(_db(peak) - GATE_RANGE_DB, GATE_FLOOR_DB)
    return 20 * np.log10(np.maximum(rms, 1e-10)) > thr


def active_rms_db(x: np.ndarray, sr: int, mask: np.ndarray | None = None) -> float | None:
    """mask 帧上的 RMS（dBFS）；mask 为空时用 x 自身的演唱段。没有有声帧时返回 None。"""
    rms = _frames_rms(x, sr)
    m = active_mask(x, sr) if mask is None else mask
    n = min(len(rms), len(m))
    sel = rms[:n][m[:n]]
    if sel.size == 0:
        return None
    return _db(float(np.sqrt(np.mean(sel ** 2))))


def reverb_ir(sr: int, rt60: float, predelay_ms: float, seed: int = 0) -> np.ndarray:
    """人工混响脉冲响应：指数衰减白噪声（−60 dB 用时 rt60），左右声道独立，6 kHz 低通，能量归一化为 1。"""
    from scipy.signal import butter, sosfilt

    rng = np.random.default_rng(seed)
    n = max(int(sr * rt60 * 1.2), 1)
    t = np.arange(n) / sr
    env = np.exp(-6.9078 * t / max(rt60, 0.05))  # ln(1000) = 6.9078：rt60 秒衰减 60 dB
    noise = rng.standard_normal((n, 2)) * env[:, None]
    sos = butter(4, min(REVERB_LOWPASS_HZ, sr * 0.45), btype="low", fs=sr, output="sos")
    noise = sosfilt(sos, noise, axis=0)
    pre = np.zeros((int(sr * predelay_ms / 1000), 2))
    ir = np.concatenate([pre, noise])
    return (ir / np.sqrt(np.sum(ir ** 2) / 2)).astype(np.float32)


def add_reverb(dry: np.ndarray, sr: int, ir: np.ndarray, wet_db: float) -> np.ndarray:
    """返回干声 + 湿声；湿声在演唱段的 RMS 比干声低 -wet_db dB。输出长度为干声加混响尾巴。"""
    from scipy.signal import oaconvolve

    mono = dry.mean(axis=1)
    wet = np.stack([oaconvolve(mono, ir[:, ch]) for ch in range(2)], axis=1).astype(np.float32)
    mask = active_mask(dry, sr)
    d, w = active_rms_db(dry, sr, mask), active_rms_db(wet[: len(dry)], sr, mask)
    if d is None or w is None:
        return dry
    wet *= 10 ** ((d + wet_db - w) / 20)
    out = wet
    out[: len(dry)] += dry
    return out


def widen(x: np.ndarray, sr: int, width: float, delay_ms: float, detune_cents: float) -> np.ndarray:
    """立体声 x 的加宽版本：左右声道各过一条慢速调制的延迟线，再按 width 缩放侧声道。

    延迟线 d(t) = delay + depth·(1 + sin(2πft + φ))，平均延迟 delay + depth。延迟变化率 d'(t) 带来的
    音高偏移为 1 − d'(t)，峰值 2πf·depth，由此按 detune_cents 反推 depth；延迟有界，所以不会累积漂移。
    左右用不同的 f 与 φ，两侧波形不再相同，原有的左右响度比例（推理的 pan 模式套回的声像）随声道保留。
    width 为侧声道（L−R）/2 的倍数：1 保持两路各自错位，0 为单声道。输出比输入长出最大延迟。
    """
    n = len(x)
    base = delay_ms / 1000 * sr
    depths = [(2 ** (detune_cents / 1200) - 1) / (2 * np.pi * f) * sr for f in WIDEN_LFO_HZ]  # 采样数
    out_n = n + int(np.ceil(base + 2 * max(depths))) + 1
    t, tout = np.arange(n, dtype=np.float64), np.arange(out_n, dtype=np.float64)
    y = np.zeros((out_n, 2), dtype=np.float32)
    for ch, (f, depth, phase) in enumerate(zip(WIDEN_LFO_HZ, depths, (0.0, np.pi / 2))):
        d = base + depth * (1 + np.sin(2 * np.pi * f * tout / sr + phase))
        # 分数延迟用线性插值读取 x[n − d(n)]；超出原信号的位置取 0。
        y[:, ch] = np.interp(tout - d, t, x[:, ch], left=0.0, right=0.0)
    mid, side = (y[:, 0] + y[:, 1]) / 2, (y[:, 0] - y[:, 1]) / 2 * np.float32(width)
    return np.stack([mid + side, mid - side], axis=1)


def _recommended_db(role: str, x: np.ndarray, sr: int, levels: dict[str, float], placed: dict[str, np.ndarray]) -> float | None:
    """没有参考轨时 x 的目标电平（dBFS）。levels / placed 是已定电平的各角色的演唱段电平与音频。

    主唱对齐的是伴奏在主唱演唱段内的电平（不含前奏、间奏），返回 None 表示保持原电平。
    """
    if role == "vocal":
        if "inst" in placed:
            inst = active_rms_db(placed["inst"], sr, active_mask(x, sr))
            if inst is not None:
                return inst + VOCAL_OVER_INST_DB
        return VOCAL_ABS_DB
    if role == "harmony":
        if "vocal" in levels:
            return levels["vocal"] - HARMONY_UNDER_VOCAL_DB
        return VOCAL_ABS_DB - HARMONY_UNDER_VOCAL_DB
    return None


def run_mix(job: Job) -> None:
    p = job.params
    specs = p.get("tracks") or []
    if not specs:
        raise ValueError("没有要混合的音轨")
    for t in specs:
        if (t.get("role") or "other") not in ROLES:
            raise ValueError(f"未知的音轨角色：{t.get('role')}")
        for key in ("path", "ref"):
            if t.get(key) and not Path(t[key]).is_file():
                raise ValueError(f"音轨不存在：{t[key]}")
        if not t.get("path"):
            raise ValueError("音轨缺少路径")

    # 1. 读取并统一采样率（取各轨最高）。
    job.progress(0.05, "读取音轨")
    raw = [sf.read(t["path"], always_2d=True, dtype="float32") for t in specs]
    sr = max(r[1] for r in raw)

    def load(data: np.ndarray, rate: int) -> np.ndarray:
        x = _stereo(data)
        return soxr.resample(x, rate, sr).astype(np.float32) if rate != sr else x

    tracks = [load(d, r) for d, r in raw]

    # 加宽放在对齐响度之前，让对齐量按加宽后的和声计算。
    wd = dict(WIDEN_DEFAULT, **(p.get("widen") or {}))
    if wd.get("enabled"):
        for i, t in enumerate(specs):
            if t.get("role") == "harmony":
                tracks[i] = widen(tracks[i], sr, float(wd["width"]), float(wd["delay_ms"]), float(wd["detune_cents"]))
                job.check_cancelled()

    # 2. 响度：伴奏先定（保持原电平），再定主唱、和声。有参考轨时对齐参考在演唱段的 RMS。
    job.progress(0.2, "对齐响度")
    order = sorted(range(len(specs)), key=lambda i: ("inst", "vocal", "harmony", "other").index(specs[i].get("role") or "other"))
    levels: dict[str, float] = {}
    placed: dict[str, np.ndarray] = {}
    report = []
    for i in order:
        t, x = specs[i], tracks[i]
        role = t.get("role") or "other"
        ref = t.get("ref") or ""
        if ref:
            rd, rr = sf.read(ref, always_2d=True, dtype="float32")
            refx = load(rd, rr)
            mask = active_mask(refx, sr)
            target, basis = active_rms_db(refx, sr, mask), "原曲分轨"
            current = active_rms_db(x, sr, mask)
        else:
            target, basis = _recommended_db(role, x, sr, levels, placed), "智能推荐"
            current = active_rms_db(x, sr)
        offset = float(t.get("gain_db") or 0)
        gain = offset
        if target is not None and current is not None:
            gain += target - current
        else:
            basis = "原电平"
        tracks[i] = x * np.float32(10 ** (gain / 20))
        level = active_rms_db(tracks[i], sr)
        if level is not None and role not in levels:
            levels[role], placed[role] = level, tracks[i]
        report.append({"path": t["path"], "role": role, "basis": basis, "offset_db": offset,
                       "gain_db": round(gain, 2), "level_dbfs": None if level is None else round(level, 2)})
        job.check_cancelled()

    # 3. 人声混响发送。
    rv = dict(REVERB_DEFAULT, **(p.get("reverb") or {}))
    if rv.get("enabled"):
        job.progress(0.45, "添加混响")
        ir = reverb_ir(sr, float(rv["rt60"]), float(rv["predelay_ms"]))
        for i, t in enumerate(specs):
            role = t.get("role")
            if role in ("vocal", "harmony"):
                wet = float(rv["wet_db"]) + (HARMONY_EXTRA_WET_DB if role == "harmony" else 0)
                tracks[i] = add_reverb(tracks[i], sr, ir, wet)
                job.check_cancelled()

    # 4. 相加与母带：整体增益到峰值 peak_db，导出 24 bit。
    job.progress(0.75, "混合与母带")
    length = max(len(x) for x in tracks)
    mix = np.zeros((length, 2), dtype=np.float32)
    for x in tracks:
        mix[: len(x)] += x
    master = p.get("master") or {}
    peak_db = float(master.get("peak_db", MASTER_PEAK_DB))
    peak = float(np.max(np.abs(mix))) if mix.size else 0.0
    master_gain = 0.0
    if peak > 0:
        master_gain = peak_db - _db(peak)
        mix *= np.float32(10 ** (master_gain / 20))

    name = (p.get("name") or job.id).replace("/", "_").replace("\\", "_")
    # 每个任务一个子目录：同名的多次混音互不覆盖（输出目录是会话暂存区，保存时再由用户命名）。
    out_dir = job.output_dir / job.id
    out_dir.mkdir(parents=True, exist_ok=True)
    out = out_dir / f"{name}.wav"
    sf.write(str(out), mix, sr, subtype="PCM_24")
    job.artifact(out, "audio")
    if master.get("mp3"):
        job.progress(0.9, "导出 MP3")
        mp3 = out.with_suffix(".mp3")
        export_mp3(out, mp3)
        job.artifact(mp3, "audio")
    info = {"sample_rate": sr, "master_gain_db": round(master_gain, 2), "peak_dbfs": peak_db,
            "reverb": rv if rv.get("enabled") else None, "widen": wd if wd.get("enabled") else None, "tracks": report}
    rep = out.with_suffix(".json")
    rep.write_text(json.dumps(info, ensure_ascii=False, indent=2), encoding="utf-8")
    job.artifact(rep, "mix-report")
    job.progress(1.0, "完成")


def export_mp3(src: Path, dst: Path) -> None:
    import imageio_ffmpeg

    cmd = [imageio_ffmpeg.get_ffmpeg_exe(), "-y", "-v", "error", "-i", str(src), "-codec:a", "libmp3lame", "-b:a", "320k", str(dst)]
    r = subprocess.run(cmd, capture_output=True, text=True, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    if r.returncode != 0:
        raise RuntimeError(f"导出 MP3 失败：{r.stderr.strip()[-300:]}")
