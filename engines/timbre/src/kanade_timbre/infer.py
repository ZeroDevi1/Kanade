"""RVC 推理。

在 Applio VoiceConverter.convert_audio 之外补上 VoiceLab 推理脚本的两项处理：
- 立体声：mono（下混后转换，默认）、pan（按 mono 转换，再把原输入左右声道的响度包络作为增益套回输出，
  保留人声声像）、dual（左右声道分别转换，最慢，且两声道可能不一致）。
- 峰值归一化：避免输出过小或削波爆音。
"""

from __future__ import annotations

import tempfile
from pathlib import Path

import numpy as np
import soundfile as sf

from kanade_engine import Job

from . import runtime

F0_METHODS = ("rmvpe", "crepe", "fcpe")
# 峰值归一化目标（约 -0.4 dBFS），留出余量避免重采样或编码后削波。
PEAK = 0.95

_converter = None


def converter():
    """VoiceConverter 内部缓存已加载的模型与 embedder，跨任务复用。"""
    global _converter
    if _converter is None:
        from rvc.infer.infer import VoiceConverter

        _converter = VoiceConverter()
    return _converter


def convert(src: Path, dst: Path, p: dict) -> None:
    converter().convert_audio(
        audio_input_path=str(src),
        audio_output_path=str(dst),
        model_path=p["model"],
        index_path=p.get("index") or "",
        pitch=int(p.get("pitch") or 0),
        f0_method=p.get("f0_method") or "rmvpe",
        index_rate=float(p.get("index_rate", 0.8)),
        volume_envelope=float(p.get("volume_envelope", 0.25)),
        protect=float(p.get("protect", 0.33)),
        export_format="WAV",
    )


def pan_gains(stereo: np.ndarray, sr: int, window_ms: float = 50, hop_ms: float = 10, strength: float = 1.0) -> tuple[np.ndarray, np.ndarray]:
    """计算左右声道相对于下混的响度增益曲线（每 hop 一个值），参数沿用 VoiceLab 的默认值。

    stereo 形状为 (采样数, 2)。返回 (times 秒, gains 形状 (帧数, 2))。
    """
    win, hop = max(int(sr * window_ms / 1000), 1), max(int(sr * hop_ms / 1000), 1)
    mono = stereo.mean(axis=1)
    n = max((len(mono) - win) // hop + 1, 1)
    gains = np.ones((n, 2), dtype=np.float32)
    for i in range(n):
        seg = slice(i * hop, i * hop + win)
        m = np.sqrt(np.mean(mono[seg] ** 2)) + 1e-8
        for ch in range(2):
            c = np.sqrt(np.mean(stereo[seg, ch] ** 2))
            gains[i, ch] = (c / m) ** strength
    # 静音段的比值不可靠，限制在 [0, 2]，避免放大噪声。
    np.clip(gains, 0.0, 2.0, out=gains)
    times = (np.arange(n) * hop + win / 2) / sr
    return times, gains


def apply_pan(mono_out: np.ndarray, out_sr: int, times: np.ndarray, gains: np.ndarray) -> np.ndarray:
    t = np.arange(len(mono_out)) / out_sr
    gl = np.interp(t, times, gains[:, 0])
    gr = np.interp(t, times, gains[:, 1])
    return np.stack([mono_out * gl, mono_out * gr], axis=1)


def normalize(audio: np.ndarray) -> np.ndarray:
    peak = float(np.max(np.abs(audio))) if audio.size else 0.0
    return audio * (PEAK / peak) if peak > 0 else audio


def run_infer(job: Job) -> None:
    p = job.params
    src = Path(p.get("input") or "")
    model = Path(p.get("model") or "")
    if not src.is_file():
        raise ValueError(f"输入文件不存在：{src}")
    if not model.is_file():
        raise ValueError(f"模型不存在：{model}")
    if p.get("index") and not Path(p["index"]).is_file():
        raise ValueError(f"索引不存在：{p['index']}")
    method = p.get("f0_method") or "rmvpe"
    if method not in F0_METHODS:
        raise ValueError(f"不支持的 f0 方法：{method}")
    runtime.ensure(job, runtime.EMBEDDER + runtime.PREDICTORS[method], 0.0, 0.2)

    data, sr = sf.read(str(src), always_2d=True, dtype="float32")
    mode = p.get("stereo_mode") or "mono"
    stereo = data.shape[1] >= 2 and mode in ("pan", "dual")
    out = job.output_dir / f"{job.id}.wav"
    job.output_dir.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(dir=job.output_dir) as tmp:
        tmpd = Path(tmp)
        if stereo and mode == "dual":
            chans = []
            for ch in range(2):
                job.progress(0.25 + 0.35 * ch, f"转换{'左' if ch == 0 else '右'}声道")
                cin, cout = tmpd / f"in{ch}.wav", tmpd / f"out{ch}.wav"
                sf.write(str(cin), data[:, ch], sr)
                convert(cin, cout, p)
                y, out_sr = sf.read(str(cout), dtype="float32")
                chans.append(y)
            n = min(len(c) for c in chans)
            result = np.stack([c[:n] for c in chans], axis=1)
        else:
            job.progress(0.25, "转换中（加载模型与提取音高）")
            mono_in = tmpd / "in.wav"
            sf.write(str(mono_in), data.mean(axis=1), sr)
            mono_out = tmpd / "out.wav"
            convert(mono_in, mono_out, p)
            result, out_sr = sf.read(str(mono_out), dtype="float32")
            if stereo:
                times, gains = pan_gains(data[:, :2], sr)
                result = apply_pan(result, out_sr, times, gains)
        job.check_cancelled()

    if p.get("normalize", True):
        result = normalize(result)
    sf.write(str(out), result, out_sr, subtype="PCM_16")
    job.progress(1.0, "完成")
    job.artifact(out, "audio")
