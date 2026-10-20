"""CosyVoice 模型下载：只下载推理所需文件，支持断点续传与按字节上报进度。

不使用 modelscope.snapshot_download：它的进度只输出到 tqdm，无法转给 Conductor，且会下载 TensorRT /
JIT 等推理用不到的文件（Fun-CosyVoice3 全量 9.75 GB，筛选后约 5.4 GB）。
"""

from __future__ import annotations

import json
import re
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from kanade_engine import Job
from kanade_engine.download import fetch, mirror

# 推理用不到的文件：JIT/TensorRT 导出、批量版 tokenizer、RL 版 LLM 权重、flow 缓存、文档图片。
_SKIP = re.compile(r"(\.zip$|estimator.*\.onnx$|\.batch\.onnx$|llm\.rl\.pt$|flow\.cache\.pt$|^asset/|\.md$|\.png$|\.jpg$|^\.gitattributes$)")

# wetext 文本归一化资源（只在 ModelScope 上发布）。CosyVoice 只用中英文 TN 的 tagger / verbalizer。
WETEXT_REPO = "pengzhendong/wetext"
_WETEXT_KEEP = re.compile(r"^(zh|en)/tn/(tagger|verbalizer)\.fst$")

COMPLETE_MARKER = ".kanade-complete"
_TIMEOUT = 60


@dataclass
class RemoteFile:
    path: str
    size: int
    url: str


def _get_json(url: str):
    req = urllib.request.Request(mirror(url), headers={"User-Agent": "Kanade"})
    with urllib.request.urlopen(req, timeout=_TIMEOUT) as resp:
        return json.load(resp)


def list_files(source: str, repo: str, keep: Callable[[str], bool] | None = None) -> list[RemoteFile]:
    """列出仓库中需要下载的文件。source 为 "modelscope" 或 "huggingface"；keep 为空时按 _SKIP 排除推理用不到的文件。"""
    quoted = urllib.parse.quote(repo, safe="/")
    if source == "modelscope":
        data = _get_json(f"https://www.modelscope.cn/api/v1/models/{quoted}/repo/files?Recursive=true")
        entries = [(f["Path"], int(f.get("Size") or 0)) for f in data["Data"]["Files"] if f["Type"] == "blob"]
        base = f"https://www.modelscope.cn/models/{quoted}/resolve/master/"
    elif source == "huggingface":
        data = _get_json(f"https://huggingface.co/api/models/{quoted}/tree/main?recursive=true")
        entries = [(f["path"], int(f.get("size") or 0)) for f in data if f["type"] == "file"]
        base = f"https://huggingface.co/{quoted}/resolve/main/"
    else:
        raise ValueError(f"未知下载源：{source}")
    if keep is None:
        keep = lambda p: not _SKIP.search(p)  # noqa: E731
    return [RemoteFile(p, s, base + urllib.parse.quote(p)) for p, s in entries if keep(p)]


def is_complete(model_dir: Path) -> bool:
    return (model_dir / COMPLETE_MARKER).exists()


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
    """下载 repo 中 keep 选中的文件到 model_dir，完成后写入 COMPLETE_MARKER。"""
    job.progress(0, f"获取{label or '模型'}文件列表")
    files = list_files(source, repo, keep)
    if not files:
        raise IOError(f"{repo} 中没有需要下载的文件")
    total = sum(f.size for f in files) or 1
    done = 0
    model_dir.mkdir(parents=True, exist_ok=True)

    for f in files:
        dst = model_dir / f.path
        if dst.exists() and f.size and dst.stat().st_size != f.size:
            dst.unlink()  # 大小不符的旧文件（例如上游版本更新）重新下载
        if dst.exists():
            done += f.size
            continue

        def report(n: int, _total: int, f=f) -> None:
            job.progress(min((done + n) / total, 0.999), f"下载{label} {f.path}（{(done + n) / 1e9:.2f} / {total / 1e9:.2f} GB）")

        fetch(f.url, dst, report, f.size)
        done += f.size

    (model_dir / COMPLETE_MARKER).write_text(json.dumps({"source": source, "repo": repo, "files": [f.path for f in files]}, ensure_ascii=False))
