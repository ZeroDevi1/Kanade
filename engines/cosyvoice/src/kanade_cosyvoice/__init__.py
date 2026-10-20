"""Kanade 的 CosyVoice 引擎（Cantus）。

任务与参数（均由 Go 端 internal/cantus 组装）：

- cosyvoice.download      {source, repo, model_dir}
- cosyvoice.zero_shot     {model_dir, text, prompt_wav, prompt_text | speaker_dir, speed, text_frontend}
- cosyvoice.cross_lingual {model_dir, text, prompt_wav | speaker_dir, speed, text_frontend}
- cosyvoice.sft           {model_dir, text, spk_id, speed, text_frontend}
- cosyvoice.instruct      {model_dir, text, instruct, spk_id（CosyVoice 1）| prompt_wav / speaker_dir（2、3）, speed}
- cosyvoice.vc            {model_dir, source_wav, prompt_wav | speaker_dir}

模型版本由 model_dir 中的 yaml 判断，各版本支持的模式见 MODES；Go 端按同一张表限制界面选项。
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

from kanade_engine import serve

ENGINE = "cosyvoice"
VERSION = "0.1.0"

# 上游源码（vendor/cosyvoice、vendor/matcha）不是可安装的包，需加入 sys.path。
_VENDOR = Path(__file__).resolve().parents[2] / "vendor"
sys.path.insert(0, str(_VENDOR))

# wetext 与 modelscope 默认缓存到 ~/.cache；改到数据根目录的 models 下，便于用户统一管理与迁移。
_models = os.environ.get("KANADE_MODELS_DIR")
if _models:
    os.environ.setdefault("MODELSCOPE_CACHE", str(Path(_models) / "_cache" / "modelscope"))
    os.environ.setdefault("HF_HOME", str(Path(_models) / "_cache" / "huggingface"))


def main() -> None:
    from . import tasks

    serve(ENGINE, VERSION, {
        "cosyvoice.download": tasks.download_model,
        "cosyvoice.zero_shot": tasks.zero_shot,
        "cosyvoice.cross_lingual": tasks.cross_lingual,
        "cosyvoice.sft": tasks.sft,
        "cosyvoice.instruct": tasks.instruct,
        "cosyvoice.vc": tasks.vc,
    })
