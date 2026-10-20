"""deepspeed 占位包：只用于 SFT 训练子进程（见 kanade_cosyvoice.sft_launch）。

上游 train.py / train_utils.py 在模块顶层 import deepspeed，但 deepspeed 只在 Linux 上可安装；torch_ddp 引擎只用到
add_config_arguments（注册命令行参数）。其余入口一旦被调用就明确报错，不会悄悄改变训练行为。
以真实文件而不是 sys.modules 注入的形式提供：Windows 上 DataLoader worker 以 spawn 启动并重新导入 train.py，
子进程继承父进程的 sys.path，能找到这里；真实模块也带有 __spec__，transformers 的可选依赖探测
（find_spec 后再查包元数据，元数据不存在即判定未安装）不会出错。
"""

import argparse


def _unavailable(*_args, **_kwargs):
    raise RuntimeError("deepspeed 不可用：Kanade 的 SFT 训练只支持 --train_engine torch_ddp")


def add_config_arguments(parser: argparse.ArgumentParser) -> argparse.ArgumentParser:
    group = parser.add_argument_group("DeepSpeed（不可用，仅为兼容上游参数解析）")
    group.add_argument("--deepspeed", action="store_true", default=False)
    group.add_argument("--deepspeed_config", type=str, default=None)
    return parser


init_distributed = _unavailable
initialize = _unavailable
