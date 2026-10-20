"""开发用模拟引擎：只用标准库生成 WAV，模拟真实推理的进度节奏。"""

from __future__ import annotations

import math
import struct
import time
import wave

from kanade_engine import Job, serve

ENGINE = "dev-mock"
VERSION = "0.1.0"


def mock_tts(job: Job) -> None:
    """按文本长度生成一段「语音」：每个字符一个音节（短促的滑音），16-bit 单声道 WAV。

    params:
        text: 文本，默认 "Kanade"。
        steps: 模拟推理步数（每步约 40 ms），默认 50。
    """
    text = str(job.params.get("text") or "Kanade")
    steps = int(job.params.get("steps") or 50)
    for i in range(steps):
        time.sleep(0.04)
        job.progress((i + 1) / (steps + 1), f"推理步骤 {i + 1}/{steps}")

    rate = 24000
    syllable = int(rate * 0.18)
    frames = bytearray()
    for idx, ch in enumerate(text):
        base = 180 + (ord(ch) % 12) * 18
        for n in range(syllable):
            t = n / rate
            env = math.sin(math.pi * n / syllable) ** 2
            f = base * (1 + 0.15 * n / syllable)
            s = 0.5 * env * (math.sin(2 * math.pi * f * t) + 0.3 * math.sin(4 * math.pi * f * t))
            frames += struct.pack("<h", int(max(-1.0, min(1.0, s)) * 32767))
        frames += b"\x00\x00" * int(rate * 0.05)
        if idx % 8 == 0:
            job.check_cancelled()

    out = job.output_dir / f"{job.id}.wav"
    with wave.open(str(out), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(bytes(frames))
    job.progress(1.0, "完成")
    job.artifact(out, "audio")


def main() -> None:
    serve(ENGINE, VERSION, {"mock.tts": mock_tts})
