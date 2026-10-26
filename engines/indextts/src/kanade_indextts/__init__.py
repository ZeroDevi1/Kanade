"""Kanade 的 IndexTTS 引擎（Cantus），使用 IndexTTS-2.5（上游 indextts.infer_v2_5）。

任务与参数（均由 Go 端 internal/cantus 组装）：

- indextts.download      {source, repo, model_dir}：主模型与附属模型（w2v-bert-2.0、CAMPPlus、BigVGAN）
- indextts.cross_lingual {model_dir, text, prompt_wav | speaker_dir, speed}：用参考音频的音色朗读（不需要参考文本），
  情绪跟随参考音频
- indextts.instruct      {model_dir, text, instruct, prompt_wav | speaker_dir, speed}：音色取自参考音频，情绪按 instruct
  的文字描述推断（上游 QwenEmotion）

合成任务都可另带 options（采样参数、文本归一化与情绪强度，见 tasks.GENERATION_DEFAULTS）、seed、pause_scale、regenerate，
长文本分段见 kanade_engine.longtext；产物为拼接后的 WAV（kind audio）与 segments.json（kind segments）。

许可：上游代码与权重使用 bilibili Model Use License Agreement，商用需联系 indexspeech@bilibili.com（见上游 LICENSE）。
"""

from __future__ import annotations

import os
from pathlib import Path

from kanade_engine import serve

ENGINE = "indextts"
VERSION = "0.1.0"

# huggingface_hub / modelscope 默认缓存到 ~/.cache；改到数据根目录的 models 下，便于用户统一管理与迁移。
_models = os.environ.get("KANADE_MODELS_DIR")
if _models:
    os.environ.setdefault("MODELSCOPE_CACHE", str(Path(_models) / "_cache" / "modelscope"))
    os.environ.setdefault("HF_HOME", str(Path(_models) / "_cache" / "huggingface"))


def main() -> None:
    from . import tasks

    serve(ENGINE, VERSION, {
        "indextts.download": tasks.download_model,
        "indextts.cross_lingual": tasks.cross_lingual,
        "indextts.instruct": tasks.instruct,
    })
