"""提供名为 ffmpeg 的可执行文件。

audio-separator 启动时执行 `ffmpeg -version` 检查，pydub 也按名字调用 ffmpeg；用户机器不一定装有 ffmpeg。
这里把 imageio-ffmpeg 自带的静态二进制以 ffmpeg(.exe) 的名字放到虚拟环境内的目录，并加到 PATH 最前面。
"""

from __future__ import annotations

import os
import shutil
import sys
from pathlib import Path

import imageio_ffmpeg


def binary() -> Path:
    """返回 ffmpeg 可执行文件路径（首次调用时创建）。"""
    name = "ffmpeg.exe" if sys.platform == "win32" else "ffmpeg"
    dst = Path(sys.prefix) / "kanade-bin" / name
    if not dst.exists():
        src = Path(imageio_ffmpeg.get_ffmpeg_exe())
        dst.parent.mkdir(parents=True, exist_ok=True)
        try:
            # 硬链接不占额外空间；跨盘或文件系统不支持时退回复制。
            os.link(src, dst)
        except OSError:
            shutil.copy2(src, dst)
        dst.chmod(0o755)
    return dst


def install_on_path() -> None:
    os.environ["PATH"] = str(binary().parent) + os.pathsep + os.environ.get("PATH", "")
