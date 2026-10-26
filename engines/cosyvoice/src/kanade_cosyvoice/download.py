"""CosyVoice 模型下载：只下载推理所需文件，支持断点续传与按字节上报进度（通用部分见 kanade_engine.hub）。

不使用 modelscope.snapshot_download：它的进度只输出到 tqdm，无法转给 Conductor，且会下载 TensorRT /
JIT 等推理用不到的文件（Fun-CosyVoice3 全量 9.75 GB，筛选后约 5.4 GB）。
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Callable

from kanade_engine import Job
from kanade_engine import hub
from kanade_engine.hub import COMPLETE_MARKER, ensure_files, is_complete  # noqa: F401（sft.py / tasks.py 经本模块使用）

# 推理用不到的文件：JIT/TensorRT 导出、批量版 tokenizer、RL 版 LLM 权重、flow 缓存、文档图片。
_SKIP = re.compile(r"(\.zip$|estimator.*\.onnx$|\.batch\.onnx$|llm\.rl\.pt$|flow\.cache\.pt$|^asset/|\.md$|\.png$|\.jpg$|^\.gitattributes$)")

# wetext 文本归一化资源（只在 ModelScope 上发布）。CosyVoice 只用中英文 TN 的 tagger / verbalizer。
WETEXT_REPO = "pengzhendong/wetext"
_WETEXT_KEEP = re.compile(r"^(zh|en)/tn/(tagger|verbalizer)\.fst$")


def download(job: Job, source: str, repo: str, model_dir: Path, wetext_dir: Path) -> None:
    """下载模型 repo 到 model_dir，并确保 wetext 资源在 wetext_dir。

    已存在且大小一致的文件跳过，未完成的 .part 文件按 Range 续传。模型全部完成后写入 COMPLETE_MARKER，
    Go 端据此判断模型是否可用。取消时保留已下载部分。
    """
    ensure_wetext(job, wetext_dir)
    fetch_repo(job, source, repo, model_dir, None)
    job.progress(1, "下载完成")


def ensure_wetext(job: Job, wetext_dir: Path) -> None:
    """下载 wetext 的中英文 TN 资源（约数 MB）；已完整时直接返回。"""
    if is_complete(wetext_dir):
        return
    fetch_repo(job, "modelscope", WETEXT_REPO, wetext_dir, lambda p: bool(_WETEXT_KEEP.match(p)), label="文本归一化资源")


def fetch_repo(job: Job, source: str, repo: str, model_dir: Path, keep: Callable[[str], bool] | None, label: str = "") -> None:
    """下载 repo 中 keep 选中的文件到 model_dir，完成后写入 COMPLETE_MARKER；keep 为空时按 _SKIP 排除推理用不到的文件。"""
    if keep is None:
        keep = lambda p: not _SKIP.search(p)  # noqa: E731
    hub.fetch_repo(job, source, repo, model_dir, keep, label)
