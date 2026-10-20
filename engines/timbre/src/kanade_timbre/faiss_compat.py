"""让 faiss 的索引读写支持非 ASCII 路径（Windows）。

faiss 的 write_index / read_index 在 C++ 里用 fopen 打开路径，Windows 上按 ANSI 代码页解释，
模型名含中文时（如 logs/炫狗/炫狗.index）写入报「No such file or directory」，读取同样失败——
而 Applio 的推理把读取失败当作「没有索引」继续转换，检索比例会悄悄失效。
这里改为 faiss 在内存中序列化、由 Python 读写文件（Python 正确处理 Unicode 路径），文件格式与原函数相同。

install() 在 worker 进程内调用（runtime.setup）；训练子进程脚本经 `python -m kanade_timbre.faiss_compat <脚本> <参数...>`
启动，先安装补丁再执行脚本（vendor 中的 Applio 代码不修改，见 vendor/VENDOR.md）。
"""

from __future__ import annotations

import runpy
import sys
from pathlib import Path


def install() -> None:
    import faiss
    import numpy as np

    if getattr(faiss, "_kanade_unicode_io", False):
        return
    original_read = faiss.read_index

    def write_index(index, path, *args):
        if not isinstance(path, (str, Path)) or args:
            return faiss._kanade_write_index(index, path, *args)  # 文件句柄或 IOWriter 等其他重载保持原行为
        Path(path).write_bytes(faiss.serialize_index(index).tobytes())

    def read_index(path, *args):
        if not isinstance(path, (str, Path)) or args:
            return original_read(path, *args)  # 带 IO 标志（如 mmap）的调用保持原行为
        return faiss.deserialize_index(np.fromfile(str(path), dtype=np.uint8))

    faiss._kanade_write_index = faiss.write_index
    faiss.write_index, faiss.read_index = write_index, read_index
    faiss._kanade_unicode_io = True


def main() -> None:
    install()
    script = sys.argv[1]
    sys.argv = sys.argv[1:]
    # 与 `python <脚本>` 一致：脚本所在目录在 sys.path 最前（runpy.run_path 不会自动加入）。
    sys.path.insert(0, str(Path(script).resolve().parent))
    runpy.run_path(script, run_name="__main__")


if __name__ == "__main__":
    main()
