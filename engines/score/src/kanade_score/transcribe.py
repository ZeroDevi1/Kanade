"""多乐器转录：用 MuScriptor（Kyutai × Mirelo）把原曲转成按乐器分组的音符。

MuScriptor 是在混音上训练的 Transformer 解码器（MT3 式的事件词表，34 个乐器组加鼓），直接吃原曲，
不需要先分轨。上游按 16 kHz 单声道、5 秒一段顺序生成（prelude forcing：每段开头延续上一段未结束的音，
因此不能批量并行），是整个分析中最慢的一步。

乐器提示：MuScriptor 按 5 秒一段独立判断音色，音色相近的乐器容易整首认错（《我不想念》的钢琴伴奏在自动模式下
几乎全被识别成原声吉他，另有少量失真电吉他）。给出 instruments 时按上游的做法既把它作为条件输入模型，又在解码时
屏蔽其余乐器的事件，转录只会在这些乐器中选择。

时间校正：上游观察到转录出的起音整体比拍点晚至多约 25 ms，用拍点网格测出这一偏差后统一减去
（与上游 transcribe_and_postprocess 相同，只是网格换成本引擎规整后的拍点，与界面和 MIDI 导出一致）。
"""

from __future__ import annotations

import numpy as np

from kanade_engine import Job, pick_device

from . import models

# 鼓是瞬时的敲击（模型只给出起音），写成固定长度的短音，便于 MIDI 导出与显示；50 ms 约为 120 BPM 下的 1/20 拍。
DRUM_HIT_S = 0.05
# 非鼓音符的最短时长（秒），与上游 MINIMUM_NOTE_DURATION_SEC 一致。
MIN_NOTE_S = 0.01


def transcribe(job: Job, y_mono: np.ndarray, sr: int, beats: np.ndarray, downbeats: np.ndarray,
               lo: float, hi: float, instruments: list[str] | None = None) -> dict:
    """转录原曲，进度映射到 [lo, hi]。

    beats / downbeats 是规整后的拍点与小节线（秒），用于测量并扣除起音的整体延迟；拍点不足时不校正。
    instruments 为上游乐器组名的列表时只在这些乐器中识别（见文件头），None 为不限；含未知组名时抛出 ValueError。
    返回 {model, onset_delay, hint, instruments: [{name, program, drum, notes: [{start, end, pitch}]}]}：
    hint 是所给的 instruments（未给时为 None），name 是上游的乐器组名（如 electric_bass），program 是该组的代表 GM 音色（0–127，鼓为 -1），
    乐器按音符数从多到少排列。
    """
    import torch
    from muscriptor import TranscriptionModel
    from muscriptor.events import NoteEndEvent, NoteStartEvent, ProgressEvent
    from muscriptor.tokenizer.mt3 import MT3_FULL_PLUS_GROUP_NAMES
    from muscriptor.utils.beats import BeatGrid, fit_tempo

    # 上游在 MPS 上默认用 FP16（解码受内存带宽限制，条件网络仍保持 FP32），这里沿用其默认精度。
    if instruments:
        unknown = [n for n in instruments if n not in MT3_FULL_PLUS_GROUP_NAMES]
        if unknown:
            raise ValueError(f"未知的乐器组：{', '.join(unknown)}")
    d = pick_device()
    job.progress(lo, f"多乐器转录：加载 MuScriptor（{d.label}）")
    model = TranscriptionModel.load_model(str(models.path(models.MUSCRIPTOR_WEIGHTS)), device=d.name)
    job.check_cancelled()

    # 1. 逐段生成：ProgressEvent 在每段结束时给出（已完成段数, 总段数）。
    open_notes: dict[int, NoteStartEvent] = {}
    raw: list[tuple[str, float, float, int]] = []  # (乐器, 起, 止, 音高)
    wav = torch.from_numpy(np.ascontiguousarray(y_mono, dtype=np.float32))
    for ev in model.transcribe((wav, sr), instruments=instruments or None):
        if isinstance(ev, ProgressEvent):
            job.check_cancelled()
            job.progress(lo + (hi - lo) * 0.95 * ev.completed / max(ev.total, 1),
                         f"多乐器转录 {ev.completed}/{ev.total} 段")
        elif isinstance(ev, NoteStartEvent):
            open_notes[ev.index] = ev
        elif isinstance(ev, NoteEndEvent):
            s = open_notes.pop(ev.start_event.index, ev.start_event)
            raw.append((s.instrument, s.start_time, ev.end_time, s.pitch))
    # 乐器组的代表 GM 音色取组内第一个音色，与上游写 MIDI 时一致。
    programs = {name: model._program_for_instrument(name) for name in {r[0] for r in raw} if name != "drums"}
    del model
    if d.kind == "cuda":
        torch.cuda.empty_cache()

    # 2. 起音延迟：拍点足够时按上游的圆周平均法测量（起音集中在某一拍细分上才校正，否则为 0）。
    delay = 0.0
    if len(beats) >= 2 and raw:
        bpm, _ = fit_tempo(np.asarray(beats))
        grid = BeatGrid(bpm=bpm, beats_per_bar=None,
                        first_downbeat=float(downbeats[0]) if len(downbeats) else float(beats[0]),
                        beats=np.asarray(beats)).with_onset_delay([r[1] for r in raw])
        delay = grid.onset_delay or 0.0

    # 3. 按乐器分组，扣除延迟并规整时长。
    groups: dict[str, list[dict]] = {}
    for name, start, end, pitch in raw:
        start = max(0.0, start - delay)
        end = start + DRUM_HIT_S if name == "drums" else max(end - delay, start + MIN_NOTE_S)
        groups.setdefault(name, []).append({"start": round(start, 3), "end": round(end, 3), "pitch": int(pitch)})
    found = []
    for name, notes in sorted(groups.items(), key=lambda kv: -len(kv[1])):
        notes.sort(key=lambda n: (n["start"], n["pitch"]))
        drum = name == "drums"
        found.append({"name": name, "program": -1 if drum else int(programs[name]), "drum": drum, "notes": notes})
    job.progress(hi, f"多乐器转录：{len(found)} 种乐器，{len(raw)} 个音符")
    return {"model": "muscriptor-medium", "onset_delay": round(delay, 4), "instruments": found,
            "hint": instruments or None}

