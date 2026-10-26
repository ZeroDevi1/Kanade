"""语音合成引擎共用的参考音色解析：已保存音色（speaker_dir）或参考音频文件（prompt_wav / prompt_text）。

音色库由 Go 端 internal/cantus/speakers.go 管理：每个音色是一个目录，含 speaker.json（name、prompt_text、audio）
与参考音频副本。依赖 soundfile（使用本模块的引擎自行声明）。
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

# 参考音频约束：各模型的提示编码器都按 16 kHz 以上的语音训练；超过 30 秒的参考既慢又容易让自回归模型失控
# （CosyVoice 的 speech tokenizer 硬性限制 30 秒，其他模型沿用同一上限）。
MAX_PROMPT_SECONDS = 30
MIN_SAMPLE_RATE = 16000


@dataclass(frozen=True)
class Voice:
    wav: str  # 参考音频路径（已校验可读）
    text: str  # 参考音频的文本；未提供时为空串
    key: str  # 缓存键：同一音色（含修改时间）或同一文件路径 + 文本时相同，用于复用提取好的特征


def check_audio(path: str, what: str = "参考音频") -> str:
    """校验音频可读、采样率与（参考音频的）时长满足要求，返回原路径。"""
    import soundfile as sf

    p = Path(path)
    if not p.is_file():
        raise ValueError(f"{what}不存在：{path}")
    try:
        info = sf.info(str(p))
    except Exception as exc:
        raise ValueError(f"无法读取{what}（支持 WAV / FLAC / OGG / MP3）：{p.name}") from exc
    if info.samplerate < MIN_SAMPLE_RATE:
        raise ValueError(f"{what}采样率 {info.samplerate} Hz 低于 {MIN_SAMPLE_RATE} Hz")
    if what == "参考音频" and info.duration > MAX_PROMPT_SECONDS:
        raise ValueError(f"参考音频 {info.duration:.1f} 秒，超过 {MAX_PROMPT_SECONDS} 秒上限，请裁剪后再用")
    return str(p)


def resolve(params: Mapping[str, Any], need_text: bool = False) -> Voice:
    """按任务参数解析参考音色。need_text 为 True 且没有参考文本时抛出 ValueError（面向用户的提示）。"""
    sdir = params.get("speaker_dir")
    if sdir:
        meta_path = Path(sdir) / "speaker.json"
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        wav = check_audio(str(Path(sdir) / meta["audio"]))
        text = (meta.get("prompt_text") or "").strip()
        key = f"speaker:{Path(sdir).name}:{meta_path.stat().st_mtime_ns}"
        if need_text and not text:
            raise ValueError(f"音色「{meta.get('name', Path(sdir).name)}」没有参考文本，不能用于这一模式")
    else:
        wav = check_audio(params.get("prompt_wav") or "")
        text = (params.get("prompt_text") or "").strip()
        key = f"file:{wav}:{Path(wav).stat().st_mtime_ns}"
        if need_text and not text:
            raise ValueError("这一模式需要填写参考音频对应的文本")
    return Voice(wav, text, f"{key}:{text}")
