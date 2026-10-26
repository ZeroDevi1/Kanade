"""Kanade 的 Qwen3-TTS 引擎（Cantus）。

任务与参数（均由 Go 端 internal/cantus 组装）：

- qwentts.download      {source, repo, model_dir}
- qwentts.zero_shot     {model_dir, text, prompt_wav, prompt_text | speaker_dir}：Base 模型，参考音频 + 文本续写克隆
- qwentts.cross_lingual {model_dir, text, prompt_wav | speaker_dir}：Base 模型，只用说话人向量（不需要参考文本）
- qwentts.voice_design  {model_dir, text, instruct}：VoiceDesign 模型，按文字描述生成音色
- qwentts.sft           {model_dir, text, spk_id, instruct?}：CustomVoice 模型，预设音色，可选风格指令

合成任务都可另带 options（采样参数，见 tasks.SAMPLING_DEFAULTS）、seed、pause_scale、regenerate，
长文本分段见 kanade_engine.longtext；产物为拼接后的 WAV（kind audio）与 segments.json（kind segments）。
模型类型由 model_dir/config.json 的 tts_model_type 判断。
"""

from __future__ import annotations

import os
from pathlib import Path

from kanade_engine import serve

ENGINE = "qwentts"
VERSION = "0.1.0"

# huggingface_hub / modelscope 默认缓存到 ~/.cache；改到数据根目录的 models 下，便于用户统一管理与迁移。
_models = os.environ.get("KANADE_MODELS_DIR")
if _models:
    os.environ.setdefault("MODELSCOPE_CACHE", str(Path(_models) / "_cache" / "modelscope"))
    os.environ.setdefault("HF_HOME", str(Path(_models) / "_cache" / "huggingface"))


def main() -> None:
    from . import tasks

    serve(ENGINE, VERSION, {
        "qwentts.download": tasks.download_model,
        "qwentts.zero_shot": tasks.zero_shot,
        "qwentts.cross_lingual": tasks.cross_lingual,
        "qwentts.voice_design": tasks.voice_design,
        "qwentts.sft": tasks.sft,
    })
