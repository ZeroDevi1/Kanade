"""Kanade 推理引擎 SDK：实现与 Go 端 Conductor 的 stdio JSON-lines 协议。

协议（每行一个 JSON 对象，UTF-8）：

Conductor -> worker（stdin）::

    {"method": "run", "job": "<id>", "task": "<name>", "params": {...}, "output_dir": "<path>"}
    {"method": "cancel", "job": "<id>"}
    {"method": "shutdown"}

worker -> Conductor（启动时 dup 出的原 stdout 描述符）::

    {"event": "hello", "data": {"engine": ..., "version": ..., "tasks": [...], "protocol": 1}}
    {"event": "progress", "job": id, "data": {"fraction": 0.5, "message": "..."}}
    {"event": "artifact", "job": id, "data": {"path": "...", "kind": "audio"}}
    {"event": "done", "job": id}
    {"event": "error", "job": id, "data": {"message": "..."}}
    {"event": "canceled", "job": id}

音频等大数据只经文件路径传递。worker 同一时间只执行一个任务；stdin 关闭（父进程退出）时 worker 自行退出。
"""

from .runtime import PROTOCOL_VERSION, Cancelled, Job, serve

__all__ = ["Cancelled", "Job", "serve", "PROTOCOL_VERSION"]
