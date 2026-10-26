"""Kanade 的 VoxCPM 引擎（Cantus）。

任务与参数（均由 Go 端 internal/cantus 组装）：

- voxcpm.download      {source, repo, model_dir}
- voxcpm.zero_shot     {model_dir, text, prompt_wav, prompt_text | speaker_dir}：参考音频 + 文本续写克隆（音色最像）
- voxcpm.cross_lingual {model_dir, text, prompt_wav | speaker_dir}：只用参考音频克隆音色（VoxCPM2）
- voxcpm.instruct      {model_dir, text, instruct, prompt_wav | speaker_dir}：克隆音色并按文字控制语速、情绪、风格（VoxCPM2）
- voxcpm.voice_design  {model_dir, text, instruct}：不用参考音频，按文字描述生成音色（VoxCPM2）

合成任务都可另带 options（generate 参数，见 tasks.GENERATE_DEFAULTS）、seed、pause_scale、regenerate，
长文本分段见 kanade_engine.longtext；产物为拼接后的 WAV（kind audio）与 segments.json（kind segments）。
模型版本由 model_dir/config.json 的 architecture 判断。
"""

from __future__ import annotations

import os
from pathlib import Path

from kanade_engine import serve

ENGINE = "voxcpm"
VERSION = "0.1.0"

# huggingface_hub / modelscope 默认缓存到 ~/.cache；改到数据根目录的 models 下，便于用户统一管理与迁移。
_models = os.environ.get("KANADE_MODELS_DIR")
if _models:
    os.environ.setdefault("MODELSCOPE_CACHE", str(Path(_models) / "_cache" / "modelscope"))
    os.environ.setdefault("HF_HOME", str(Path(_models) / "_cache" / "huggingface"))


def main() -> None:
    from . import tasks

    serve(ENGINE, VERSION, {
        "voxcpm.download": tasks.download_model,
        "voxcpm.zero_shot": tasks.zero_shot,
        "voxcpm.cross_lingual": tasks.cross_lingual,
        "voxcpm.instruct": tasks.instruct,
        "voxcpm.voice_design": tasks.voice_design,
    })
