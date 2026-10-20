"""推理设备选择：CUDA（Windows / Linux NVIDIA）优先，其次 Apple Silicon 的 MPS，最后 CPU。

引擎统一通过 pick() 取设备，不直接判断 torch.cuda，保证同一份代码在 Windows（CUDA）与 macOS（MPS / CPU）上都能运行。
环境变量 KANADE_DEVICE 可强制指定（cuda、cuda:1、mps、cpu），用于排查设备相关问题；指定的设备不可用时回退自动选择并记录警告。
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass

log = logging.getLogger("kanade_engine.device")


@dataclass(frozen=True)
class Device:
    # name 可直接传给 torch.device()，例如 "cuda:0"、"mps"、"cpu"。
    name: str
    # label 是给界面显示的描述，例如 "CUDA NVIDIA GeForce RTX 4090"。
    label: str

    @property
    def kind(self) -> str:
        return self.name.split(":")[0]

    @property
    def supports_half(self) -> bool:
        """是否适合 FP16 推理：只在 CUDA 上开启。MPS 的 FP16 在不少音频模型上会产生 NaN。"""
        return self.kind == "cuda"


def _available(torch, name: str) -> bool:
    kind = name.split(":")[0]
    if kind == "cuda":
        if not torch.cuda.is_available():
            return False
        idx = int(name.split(":")[1]) if ":" in name else 0
        return idx < torch.cuda.device_count()
    if kind == "mps":
        return bool(getattr(torch.backends, "mps", None) and torch.backends.mps.is_available())
    return kind == "cpu"


def _describe(torch, name: str) -> Device:
    kind = name.split(":")[0]
    if kind == "cuda":
        idx = int(name.split(":")[1]) if ":" in name else 0
        return Device(f"cuda:{idx}", "CUDA " + torch.cuda.get_device_name(idx))
    if kind == "mps":
        return Device("mps", "Apple MPS")
    return Device("cpu", "CPU")


def pick(allow_mps: bool = True) -> Device:
    """返回本次运行使用的设备。

    allow_mps=False 用于上游实现不兼容 MPS 的模型（例如部分算子未实现），此时 macOS 直接用 CPU。
    """
    import torch

    forced = os.environ.get("KANADE_DEVICE", "").strip().lower()
    if forced:
        if _available(torch, forced) and (allow_mps or not forced.startswith("mps")):
            return _describe(torch, forced)
        log.warning("KANADE_DEVICE=%s 不可用，改为自动选择", forced)
    if _available(torch, "cuda"):
        return _describe(torch, "cuda:0")
    if allow_mps and _available(torch, "mps"):
        return _describe(torch, "mps")
    return _describe(torch, "cpu")
