"""运行目录与底模资源。

目录布局（ROOT = <数据根目录>/models/rvc）：

    ROOT/                      Applio 的工作目录（os.getcwd()）
    ROOT/rvc/models/...        预测器、embedder、预训练底模（Applio 按相对路径读取）
    ROOT/assets/               Applio 写入的运行时文件
    ROOT/logs/<模型名>/         训练目录（APPLIO_LOGS_DIR），含权重 *.pth 与索引 *.index
    ROOT/logs/reference/       训练时生成试听样本用的参考特征（从 vendor 复制）

底模来自 Applio 在 Hugging Face 上发布的资源（IAHispano/Applio），可经 KANADE_HF_ENDPOINT 走镜像。
"""

from __future__ import annotations

import os
import shutil
import sys
from dataclasses import dataclass
from pathlib import Path

from kanade_engine import Job
from kanade_engine.download import fetch

VENDOR = Path(__file__).resolve().parents[2] / "vendor"
_models = os.environ.get("KANADE_MODELS_DIR")
ROOT = (Path(_models) if _models else Path.home() / ".cache" / "kanade") / "rvc"
LOGS = ROOT / "logs"

_HF = "https://huggingface.co/IAHispano/Applio/resolve/main/Resources"


def setup() -> None:
    """创建运行目录、切换工作目录并让 vendor 中的 rvc 包可导入。须在导入任何 rvc 模块之前调用。"""
    for d in ("rvc/models/predictors", "rvc/models/embedders/contentvec", "rvc/models/pretraineds/hifi-gan",
              "rvc/models/formant", "assets", "logs"):
        (ROOT / d).mkdir(parents=True, exist_ok=True)
    ref = LOGS / "reference"
    if not ref.exists():
        shutil.copytree(VENDOR / "logs" / "reference", ref)
    os.environ["APPLIO_LOGS_DIR"] = str(LOGS)
    os.environ["APPLIO_CODE_ROOT"] = str(VENDOR)
    os.chdir(ROOT)
    sys.path.insert(0, str(VENDOR))
    from . import faiss_compat

    faiss_compat.install()


def subprocess_env() -> dict[str, str]:
    """训练子进程的环境：脚本以文件路径启动，sys.path[0] 是脚本所在目录，需经 PYTHONPATH 找到 vendor 中的 rvc 包。"""
    env = dict(os.environ)
    env["PYTHONPATH"] = str(VENDOR) + os.pathsep + env.get("PYTHONPATH", "")
    return env


@dataclass(frozen=True)
class Asset:
    remote: str  # base 下的相对路径
    local: str  # ROOT 下的相对路径
    base: str = _HF  # 默认为 Applio 的 Resources/


# 推理必需：contentvec 特征提取器；按 f0 方法另需对应预测器（crepe 的权重随 torchcrepe 包提供）。
EMBEDDER = [
    Asset("embedders/contentvec/pytorch_model.bin", "rvc/models/embedders/contentvec/pytorch_model.bin"),
    Asset("embedders/contentvec/config.json", "rvc/models/embedders/contentvec/config.json"),
]
PREDICTORS = {
    "rmvpe": [Asset("predictors/rmvpe.pt", "rvc/models/predictors/rmvpe.pt")],
    "fcpe": [Asset("predictors/fcpe.pt", "rvc/models/predictors/fcpe.pt")],
    "crepe": [],
}


def pretrained(sample_rate: int) -> list[Asset]:
    """HiFi-GAN 预训练底模（f0 版），训练时作为初始化权重。"""
    tag = {32000: "32k", 40000: "40k", 48000: "48k"}[sample_rate]
    return [Asset(f"pretrained_v2/f0{x}{tag}.pth", f"rvc/models/pretraineds/hifi-gan/f0{x}{tag}.pth") for x in ("G", "D")]


def pretrained_paths(sample_rate: int) -> tuple[str, str]:
    g, d = pretrained(sample_rate)
    return str(ROOT / g.local), str(ROOT / d.local)


def ensure(job: Job, assets: list[Asset], lo: float, hi: float) -> None:
    """下载缺少的资源，进度映射到 [lo, hi]。"""
    missing = [a for a in assets if not (ROOT / a.local).is_file()]
    for i, a in enumerate(missing):
        def report(have: int, total: int, i=i, a=a) -> None:
            frac = (i + (have / total if total else 0)) / len(missing)
            job.progress(lo + (hi - lo) * frac, f"下载 {Path(a.remote).name}：{have / 1e6:.0f}" + (f" / {total / 1e6:.0f} MB" if total else " MB"))

        fetch(f"{a.base}/{a.remote}", ROOT / a.local, report)
