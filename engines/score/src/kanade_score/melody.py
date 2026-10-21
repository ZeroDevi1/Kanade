"""旋律线：用 SOME 把（分离出的）主唱转成音符。

输入应是去掉伴奏与和声的主唱干声：SOME 在歌声数据上训练，直接用混音时伴奏会产生大量错误音符。
流程与上游 infer.py 一致：44.1 kHz 单声道 → 按静音切片 → 每片求梅尔谱并推理 → 解码为（音高, 时长, 是否休止）序列，
再按切片偏移拼回全曲时间轴。音高是连续值（可含微分音），由 Go 端量化到半音并排到拍点上。
"""

from __future__ import annotations

from pathlib import Path

import numpy as np

from kanade_engine import Job, pick_device

from . import models

# SOME 的音符置信度阈值等参数取自其 config.yaml；短于该时长的音符多为滑音或噪声，丢弃（秒）。
MIN_NOTE_S = 0.06


def _load(dev: str):
    import torch
    import yaml

    from .some.model import midi_conforms
    from .some.spec import MelSpectrogram

    ckpt = models.path(models.SOME_CKPT)
    config = yaml.safe_load(ckpt.with_name("config.yaml").read_text(encoding="utf-8"))
    model = midi_conforms(config).eval().to(dev)
    # 检查点是 PyTorch Lightning 保存的；weights_only=True 只反序列化张量与基本类型，不执行任意代码。
    state = torch.load(str(ckpt), map_location=dev, weights_only=True)["state_dict"]
    model.load_state_dict({k[len("model."):]: v for k, v in state.items() if k.startswith("model.")}, strict=True)
    mel = MelSpectrogram(n_mel_channels=config["units_dim"], sampling_rate=config["audio_sample_rate"],
                         win_length=config["win_size"], hop_length=config["hop_size"],
                         mel_fmin=config["fmin"], mel_fmax=config["fmax"]).to(dev)
    return config, model, mel


def transcribe(job: Job, y_mono: np.ndarray, sr: int, lo: float, hi: float) -> list[dict]:
    """返回音符列表 [{start, end, pitch}]（秒、MIDI 音高，浮点）。"""
    import soxr
    import torch

    from .some.decode import decode_bounds_to_alignment, decode_gaussian_blurred_probs, decode_note_sequence
    from .some.slicer import Slicer

    d = pick_device(allow_mps=False)
    job.progress(lo, f"旋律识别：加载 SOME（{d.label}）")
    config, model, mel = _load(d.name)
    target = config["audio_sample_rate"]
    y = soxr.resample(y_mono, sr, target).astype(np.float32) if sr != target else y_mono.astype(np.float32)
    chunks = Slicer(sr=target, max_sil_kept=1000).slice(y)
    step = config["hop_size"] / target
    notes: list[dict] = []
    for i, ch in enumerate(chunks):
        job.check_cancelled()
        job.progress(lo + (hi - lo) * i / max(len(chunks), 1), f"旋律识别 {i + 1}/{len(chunks)}")
        wav = torch.from_numpy(np.asarray(ch["waveform"], dtype=np.float32)).unsqueeze(0).to(d.name)
        with torch.no_grad():
            units = mel(wav).transpose(1, 2)
            masks = torch.ones(units.shape[:2], dtype=torch.bool, device=d.name)
            probs, bounds = model(x=units, f0=torch.zeros(units.shape[:2], device=d.name), mask=masks, sig=True)
            probs = probs * masks[..., None]
            bounds = bounds * masks
            unit2note = decode_bounds_to_alignment(bounds) * masks
            midi, rest = decode_gaussian_blurred_probs(probs, vmin=config["midi_min"], vmax=config["midi_max"],
                                                       deviation=config["midi_prob_deviation"],
                                                       threshold=config["rest_threshold"])
            note_midi, note_dur, note_mask = decode_note_sequence(unit2note, midi, ~rest & masks)
        t = float(ch["offset"])  # 切片起点（秒）
        for m, dur, ok in zip(note_midi.squeeze(0).cpu().numpy(), note_dur.squeeze(0).cpu().numpy() * step,
                              note_mask.squeeze(0).cpu().numpy()):
            if ok and dur >= MIN_NOTE_S:
                notes.append({"start": round(t, 3), "end": round(t + float(dur), 3), "pitch": round(float(m), 2)})
            t += float(dur)
    return notes
