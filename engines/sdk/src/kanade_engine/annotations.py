"""`.list` 标注文件（GPT-SoVITS 格式）：`audio_path|speaker|lang|text`，规则与 VoiceLab docs/datasets/list_annotations.md 一致。

- 空行与 # 开头的行忽略；超过 4 列时多余的 | 并入 text。
- 同名优先：<dir>/<dir名>.list → <dir>/<dir名小写>.list → 目录下唯一的 *.list。
- audio_path 可为任意平台的绝对路径或相对路径，按 basename / stem 匹配数据集目录中的音频。
Go 端 internal/dataset 实现同一套规则，两边须保持一致。
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

AUDIO_EXTS = {".wav", ".flac", ".mp3", ".ogg", ".m4a", ".aac", ".opus"}


@dataclass(frozen=True)
class Entry:
    audio: str
    speaker: str
    lang: str
    text: str

    @property
    def basename(self) -> str:
        # 兼容 Windows 路径（C:\\...）在 macOS / Linux 上解析。
        return re.split(r"[\\/]", self.audio)[-1]


def parse(path: Path) -> list[Entry]:
    entries = []
    for raw in path.read_text(encoding="utf-8-sig").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split("|", 3)
        parts += [""] * (4 - len(parts))
        entries.append(Entry(parts[0].strip(), parts[1].strip(), parts[2].strip(), parts[3].strip()))
    return entries


def find_list(dataset_dir: Path) -> Path | None:
    """按同名优先规则查找数据集目录中的 .list；找不到时返回 None。"""
    for cand in (dataset_dir / f"{dataset_dir.name}.list", dataset_dir / f"{dataset_dir.name.lower()}.list"):
        if cand.is_file():
            return cand
    lists = sorted(dataset_dir.glob("*.list"))
    return lists[0] if len(lists) == 1 else None


def audio_files(dataset_dir: Path) -> list[Path]:
    return sorted(p for p in dataset_dir.iterdir() if p.is_file() and p.suffix.lower() in AUDIO_EXTS)


def match(entries: list[Entry], dataset_dir: Path) -> tuple[list[tuple[Entry, Path]], list[Entry]]:
    """把标注匹配到数据集目录中的音频：先比 basename，再比 stem（扩展名不同也能匹配）。

    返回 (匹配成功的 (标注, 音频路径) 列表, 未找到音频的标注)。
    """
    files = audio_files(dataset_dir)
    by_name = {p.name: p for p in files}
    by_stem = {p.stem: p for p in files}
    found, missing = [], []
    for e in entries:
        name = e.basename
        p = by_name.get(name) or by_stem.get(Path(name).stem)
        if p is None:
            missing.append(e)
        else:
            found.append((e, p))
    return found, missing
