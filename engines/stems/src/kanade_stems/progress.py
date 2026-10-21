"""把 audio-separator 分块推理的 tqdm 进度转发给任务进度。

audio-separator 在各架构的分块循环里直接用模块级的 tqdm（mdxc_separator：Roformer / MDX23C，mdx_separator，
vr_separator），不提供进度回调。这里把这些模块中的 tqdm 名字替换为一个只做计数的迭代包装，在 reporting()
上下文内把完成比例交给回调。回调里调用 job.progress，任务被取消时它抛出 Cancelled，于是取消在分块之间即可生效，
不必等整首分离完。

依赖 audio-separator 0.47.1 的模块结构（各架构文件 `from tqdm import tqdm`）；升级后找不到模块时只是没有进度，
不影响分离。
"""

from __future__ import annotations

import importlib
from contextlib import contextmanager
from typing import Callable, Iterable, Iterator, TypeVar

T = TypeVar("T")

_MODULES = (
    "audio_separator.separator.architectures.mdxc_separator",
    "audio_separator.separator.architectures.mdx_separator",
    "audio_separator.separator.architectures.vr_separator",
)

_report: Callable[[float], None] | None = None


def _counting(iterable: Iterable[T], *args, **kwargs) -> Iterator[T]:
    """替代 tqdm(iterable)：逐项产出，并在每项完成后回报 已完成 / 总数。"""
    items = iterable if hasattr(iterable, "__len__") else list(iterable)
    total = len(items)  # type: ignore[arg-type]
    for i, item in enumerate(items):
        yield item
        if _report is not None and total:
            _report((i + 1) / total)


def install() -> None:
    """替换各架构模块中的 tqdm；须在导入 audio_separator 之后、分离之前调用，可重复调用。"""
    for name in _MODULES:
        try:
            mod = importlib.import_module(name)
        except ImportError:
            continue
        if hasattr(mod, "tqdm"):
            mod.tqdm = _counting


@contextmanager
def reporting(fn: Callable[[float], None]):
    """在上下文内把分块进度（0–1）交给 fn。VR 架构有多个依次执行的循环，进度会多次从 0 走到 1。"""
    global _report
    install()
    _report = fn
    try:
        yield
    finally:
        _report = None
