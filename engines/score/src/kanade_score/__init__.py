"""Kanade 的音乐分析引擎（Score）：节拍 / 拍号 / BPM、调号、和弦与情绪。

任务（参数由 Go 端 internal/score 组装）：

- score.analyze  {input, harmonic_input?, melody_input?, options?}：分析一首歌，输出 analysis.json（字段见 analyze.py）与
                 和弦标注 chords.lab。harmonic_input 是去掉人声的伴奏、melody_input 是去掉和声的主唱（均由 Go 端先经
                 分轨引擎得到）：调号与和弦用伴奏分析，旋律线用主唱识别（SOME），节拍与情绪仍用原曲。
- score.download {models?}：预下载模型（beat_this、clap、some）并上报进度。

设备：beat_this、CLAP 与 SOME 有 CUDA 时用 CUDA，否则 CPU（两者在 MPS 上未经验证，macOS 只保证流程可用）；
调号与和弦是 librosa 的信号处理，只用 CPU。
"""

from __future__ import annotations

import os
from pathlib import Path

from kanade_engine import serve

ENGINE = "score"
VERSION = "0.1.0"

_models = os.environ.get("KANADE_MODELS_DIR")
MODEL_DIR = Path(_models) / "score" if _models else Path.home() / ".cache" / "kanade" / "score"


def main() -> None:
    from . import analyze, models

    serve(ENGINE, VERSION, {
        "score.analyze": analyze.run_analyze,
        "score.download": models.run_download,
    })
