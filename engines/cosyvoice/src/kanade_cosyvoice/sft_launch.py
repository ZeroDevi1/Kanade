"""以单进程运行上游 cosyvoice/bin/train.py（torch_ddp 引擎，world_size=1）。

用法：python -m kanade_cosyvoice.sft_launch <train.py 的参数...>

上游在模块顶层 import deepspeed，但 deepspeed 只在 Linux 上可安装；torch_ddp 引擎只用到其
add_config_arguments（注册命令行参数）。这里在导入前注入占位模块：其余入口一旦被调用就明确报错，
不会悄悄改变训练行为。分布式环境变量由调用方（sft.py）设置，免去 torchrun。
"""

from __future__ import annotations

import argparse
import runpy
import sys
import types
from pathlib import Path

VENDOR = Path(__file__).resolve().parents[2] / "vendor"


def _install_deepspeed_stub() -> None:
    try:
        import deepspeed  # noqa: F401
        return
    except ImportError:
        pass

    def unavailable(*_args, **_kwargs):
        raise RuntimeError("deepspeed 不可用：Kanade 的 SFT 训练只支持 --train_engine torch_ddp")

    def add_config_arguments(parser: argparse.ArgumentParser) -> argparse.ArgumentParser:
        group = parser.add_argument_group("DeepSpeed（不可用，仅为兼容上游参数解析）")
        group.add_argument("--deepspeed", action="store_true", default=False)
        group.add_argument("--deepspeed_config", type=str, default=None)
        return parser

    ds = types.ModuleType("deepspeed")
    ds.add_config_arguments = add_config_arguments
    ds.init_distributed = unavailable
    ds.initialize = unavailable
    runtime = types.ModuleType("deepspeed.runtime")
    zero = types.ModuleType("deepspeed.runtime.zero")
    stage = types.ModuleType("deepspeed.runtime.zero.stage_1_and_2")
    stage.estimate_zero2_model_states_mem_needs_all_live = unavailable
    ds.runtime, runtime.zero, zero.stage_1_and_2 = runtime, zero, stage
    sys.modules.update({"deepspeed": ds, "deepspeed.runtime": runtime, "deepspeed.runtime.zero": zero,
                        "deepspeed.runtime.zero.stage_1_and_2": stage})


def main() -> None:
    sys.path[:0] = [str(VENDOR)]
    _install_deepspeed_stub()
    script = VENDOR / "cosyvoice" / "bin" / sys.argv[1]
    sys.argv = [str(script), *sys.argv[2:]]
    runpy.run_path(str(script), run_name="__main__")


if __name__ == "__main__":
    main()
