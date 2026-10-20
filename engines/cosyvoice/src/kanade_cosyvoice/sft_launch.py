"""以单进程运行上游 cosyvoice/bin/train.py（torch_ddp 引擎，world_size=1）。

用法：python -m kanade_cosyvoice.sft_launch <train.py 的参数...>

上游在模块顶层 import deepspeed，但 deepspeed 只在 Linux 上可安装，这里在导入前提供占位包（_stubs/deepspeed）。
分布式环境变量由调用方（sft.py）设置，免去 torchrun。
"""

from __future__ import annotations

import os
import runpy
import sys
from pathlib import Path

VENDOR = Path(__file__).resolve().parents[2] / "vendor"


def _install_deepspeed_stub() -> None:
    """没有真正的 deepspeed 时，把占位包所在目录（_stubs，见其 __init__ 说明）加到 sys.path 末尾。"""
    try:
        import deepspeed  # noqa: F401
        return
    except ImportError:
        pass
    sys.path.append(str(Path(__file__).resolve().parent / "_stubs"))


def _install_freeze_hook() -> None:
    """环境变量 KANADE_SFT_LLM_LAYERS=N（N > 0）时，训练 LLM 只更新 Qwen 顶部 N 层、末层 norm、
    speech_embedding 与 llm_decoder，其余参数冻结。

    CosyVoice 3 的 LLM 有约 6.4 亿参数，fp32 全量 Adam（权重 + 梯度 + 两份动量）约需 10 GB 显存，8 GB 显卡放不下；
    冻结的参数没有梯度、Adam 也不为其分配状态。冻结发生在 train.py 加载 checkpoint 之后、建优化器之前：
    train.py 以 `from cosyvoice.utils.train_utils import wrap_cuda_model` 导入，这里在 runpy 执行前替换模块属性即可生效。
    文本 embedding 与 lm_head 在说话人 SFT 中不需要更新（输出走 llm_decoder），因此总是冻结。
    """
    layers = int(os.environ.get("KANADE_SFT_LLM_LAYERS") or 0)
    if layers <= 0:
        return
    from cosyvoice.utils import train_utils

    original = train_utils.wrap_cuda_model

    def wrap_cuda_model(args, model):
        if args.model == "llm":
            blocks = model.llm.model.model.layers
            keep = {f"llm.model.model.layers.{i}." for i in range(max(len(blocks) - layers, 0), len(blocks))}
            trainable = 0
            for name, p in model.named_parameters():
                p.requires_grad = (not name.startswith("llm.model.")) or name.startswith("llm.model.model.norm.") \
                    or any(name.startswith(k) for k in keep)
                trainable += p.numel() if p.requires_grad else 0
            total = sum(p.numel() for p in model.parameters())
            print(f"[kanade] 只训练 LLM 顶部 {layers}/{len(blocks)} 层：可训练参数 {trainable / 1e6:.0f}M / {total / 1e6:.0f}M", flush=True)
        return original(args, model)

    train_utils.wrap_cuda_model = wrap_cuda_model


def _install_join_patch() -> None:
    """单进程训练时跳过 cosyvoice_join 的跨进程 barrier。

    上游实现读取 group_join.options._timeout，torch 2.7 的 ProcessGroup 已没有 options 属性（上游按 torch 2.3 编写），
    每个 batch 都会抛 AttributeError。该 barrier 只用于多卡间检测负载不均，world_size 为 1 时恒返回 False 即与上游语义一致。
    executor.py 以 from-import 取得该函数，须在 runpy 执行 train.py（首次导入 executor）之前替换。
    """
    from cosyvoice.utils import train_utils

    original = train_utils.cosyvoice_join

    def cosyvoice_join(group_join, info_dict):
        if int(os.environ.get("WORLD_SIZE", 1)) == 1:
            return False
        return original(group_join, info_dict)

    train_utils.cosyvoice_join = cosyvoice_join


def main() -> None:
    sys.path[:0] = [str(VENDOR)]
    _install_deepspeed_stub()
    if sys.argv[1] == "train.py":
        _install_freeze_hook()
        _install_join_patch()
    script = VENDOR / "cosyvoice" / "bin" / sys.argv[1]
    sys.argv = [str(script), *sys.argv[2:]]
    runpy.run_path(str(script), run_name="__main__")


if __name__ == "__main__":
    main()
