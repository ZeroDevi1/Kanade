"""显卡状态：显存与 PCIe 链路，经 nvidia-smi 查询。

不用 torch.cuda：在 worker 进程里调用它会创建 CUDA 上下文（数百 MB 显存），而训练在子进程中运行，
worker 占着的显存会让 8 GB 显卡更早回退到共享内存。nvidia-smi 随 NVIDIA 驱动安装（Windows 在 System32），
没有时（macOS、无 NVIDIA 显卡）返回 None，调用方按无数据处理。

PCIe 链路用于判断外接显卡（Oculink、雷电）的带宽：Oculink 通常是 PCIe 4.0 ×4（约 7.9 GB/s，台式机 ×16 插槽的
四分之一）。空闲时显卡会降速到 1.1 代，只有训练中读到的代数与宽度才有意义。
"""

from __future__ import annotations

import shutil
import subprocess
from dataclasses import dataclass

_FIELDS = ("name", "memory.total", "memory.used", "pcie.link.gen.current", "pcie.link.gen.max",
           "pcie.link.width.current", "pcie.link.width.max", "utilization.gpu")


@dataclass(frozen=True)
class Status:
    name: str
    total_mb: int
    used_mb: int
    gen: int  # 当前 PCIe 代数（空闲时会降低）
    gen_max: int
    width: int  # 当前链路宽度（×N）
    width_max: int
    util: int  # GPU 利用率（%）

    def link(self) -> str:
        return f"PCIe {self.gen}.0 ×{self.width}"

    def memory(self) -> str:
        return f"显存 {self.used_mb / 1024:.1f}/{self.total_mb / 1024:.1f} GB"


def query(index: int = 0) -> Status | None:
    """第 index 块 NVIDIA 显卡的状态；查询失败时为 None。"""
    exe = shutil.which("nvidia-smi")
    if exe is None:
        return None
    try:
        out = subprocess.run([exe, f"--query-gpu={','.join(_FIELDS)}", "--format=csv,noheader,nounits", "-i", str(index)],
                             capture_output=True, text=True, timeout=10,
                             creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0)).stdout
    except (OSError, subprocess.SubprocessError):
        return None
    parts = [p.strip() for p in out.strip().split(",")]
    if len(parts) != len(_FIELDS):
        return None
    try:
        nums = [int(float(p)) for p in parts[1:]]
    except ValueError:  # 不支持的字段返回 "[N/A]"
        return None
    return Status(parts[0], *nums)
