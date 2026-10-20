"""Kanade 的分轨引擎（Stems），基于 python-audio-separator。

任务：

- stems.chain    {input, steps?, models?, output_dir?}：人声净化链（与 VoiceLab 的 MSST 链一致），
                 依次产出伴奏、干声、去和声干声 / 和声、去混响、降噪，文件名后缀见 chain.STEPS；
                 models 按步骤 ID 覆盖默认模型（{"vocals": "<模型文件名>"}）。
- stems.separate {input, model, single_stem?, output_dir?}：用任意 audio-separator 支持的模型分离。
- stems.download {models}：预下载模型并上报进度。

设备由 audio-separator 自行选择（CUDA → MPS → CPU），与 kanade_engine.pick_device 的顺序一致。
"""

from __future__ import annotations

import os
from pathlib import Path

from kanade_engine import serve

ENGINE = "stems"
VERSION = "0.1.0"

_models = os.environ.get("KANADE_MODELS_DIR")
MODEL_DIR = Path(_models) / "stems" if _models else Path.home() / ".cache" / "kanade" / "stems"


def main() -> None:
    from . import chain, ffmpeg

    ffmpeg.install_on_path()
    serve(ENGINE, VERSION, {
        "stems.chain": chain.run_chain,
        "stems.separate": chain.run_separate,
        "stems.download": chain.run_download,
    })
