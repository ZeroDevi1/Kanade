"""模型仓库下载（ModelScope / Hugging Face）：列出仓库文件、按需筛选、断点续传与按字节上报进度。

不使用 modelscope / huggingface_hub 的 snapshot_download：它们的进度只输出到 tqdm，无法转给 Conductor，
也无法按文件筛选掉推理用不到的大文件。下载完成后在模型目录写入 COMPLETE_MARKER（记录下载源、仓库与文件清单），
Go 端据此判断模型是否可用。
"""

from __future__ import annotations

import json
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .download import fetch, mirror, urlopen
from .runtime import Job

COMPLETE_MARKER = ".kanade-complete"
_TIMEOUT = 60


@dataclass
class RemoteFile:
    path: str
    size: int
    url: str


def _get_json(url: str):
    req = urllib.request.Request(mirror(url), headers={"User-Agent": "Kanade"})
    with urlopen(req, timeout=_TIMEOUT) as resp:
        return json.load(resp)


def list_files(source: str, repo: str, keep: Callable[[str], bool] | None = None) -> list[RemoteFile]:
    """列出仓库中 keep 选中的文件（keep 为空时全部）。source 为 "modelscope" 或 "huggingface"。"""
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
    return [RemoteFile(p, s, base + urllib.parse.quote(p)) for p, s in entries if keep is None or keep(p)]


def is_complete(model_dir: Path) -> bool:
    return (model_dir / COMPLETE_MARKER).exists()


def fetch_files(job: Job, files: list[RemoteFile], model_dir: Path, label: str = "", lo: float = 0.0, hi: float = 1.0,
                rename: Callable[[str], str] | None = None) -> None:
    """下载 files 到 model_dir（rename 把仓库内路径映射为本地相对路径，默认不变），进度映射到 [lo, hi)。

    已存在且大小一致的文件跳过（大小不符的旧文件重新下载），未完成的 .part 文件按 Range 续传。
    """
    total = sum(f.size for f in files) or 1
    done = 0
    model_dir.mkdir(parents=True, exist_ok=True)
    for f in files:
        dst = model_dir / (rename(f.path) if rename else f.path)
        if dst.exists() and f.size and dst.stat().st_size != f.size:
            dst.unlink()  # 大小不符的旧文件（例如上游版本更新）重新下载
        if dst.exists():
            done += f.size
            continue

        def report(n: int, _total: int, f=f) -> None:
            frac = lo + (hi - lo) * min((done + n) / total, 0.999)
            job.progress(frac, f"下载{label} {f.path}（{(done + n) / 1e9:.2f} / {total / 1e9:.2f} GB）")

        fetch(f.url, dst, report, f.size)
        done += f.size


def fetch_repo(job: Job, source: str, repo: str, model_dir: Path, keep: Callable[[str], bool] | None, label: str = "",
               lo: float = 0.0, hi: float = 1.0) -> None:
    """下载 repo 中 keep 选中的文件到 model_dir，完成后写入 COMPLETE_MARKER。取消时保留已下载部分。"""
    job.progress(lo, f"获取{label or '模型'}文件列表")
    files = list_files(source, repo, keep)
    if not files:
        raise IOError(f"{repo} 中没有需要下载的文件")
    fetch_files(job, files, model_dir, label, lo, hi)
    write_marker(model_dir, source, repo, [f.path for f in files])


def write_marker(model_dir: Path, source: str, repo: str, files: list[str]) -> None:
    """写入 COMPLETE_MARKER。须在模型所需的全部文件（含附属模型）下载完成后调用。"""
    (model_dir / COMPLETE_MARKER).write_text(json.dumps({"source": source, "repo": repo, "files": files},
                                                        ensure_ascii=False))


def ensure_files(job: Job, model_dir: Path, names: list[str]) -> None:
    """补下载 model_dir 中缺少的 names（例如推理下载时跳过、训练才需要的文件）。

    下载源与仓库取自 COMPLETE_MARKER；模型不是由 Kanade 下载的（没有标记）时抛出 FileNotFoundError。
    补下的文件不写回标记中的文件清单，标记只描述推理所需的集合。
    """
    missing = [n for n in names if not (model_dir / n).is_file()]
    if not missing:
        return
    try:
        meta = json.loads((model_dir / COMPLETE_MARKER).read_text(encoding="utf-8"))
        source, repo = meta["source"], meta["repo"]
    except (OSError, KeyError, ValueError) as exc:
        raise FileNotFoundError(f"{model_dir} 缺少 {', '.join(missing)}，且无法确定下载源，请手动放入") from exc
    files = list_files(source, repo, lambda p: p in missing)
    if len(files) != len(missing):
        raise FileNotFoundError(f"{repo} 中找不到 {', '.join(missing)}")
    for f in files:
        def report(n: int, total: int, f=f) -> None:
            job.progress(job.fraction, f"下载训练所需的 {f.path}（{n / 1e6:.0f} / {total / 1e6:.0f} MB）")

        fetch(f.url, model_dir / f.path, report, f.size)
