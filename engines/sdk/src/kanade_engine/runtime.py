"""worker 主循环：读取 Conductor 请求、执行任务处理函数、上报事件。"""

from __future__ import annotations

import json
import os
import sys
import threading
import traceback
from pathlib import Path
from typing import Any, Callable, Mapping

PROTOCOL_VERSION = 1


class Cancelled(Exception):
    """任务处理函数在检查点发现任务已被取消时抛出，由主循环转为 canceled 事件。"""


class _Channel:
    """协议输出通道。

    启动时把真正的 stdout 描述符复制一份专用于协议，再把描述符 1 指向 stderr：
    这样第三方库（包括 C 扩展）的 print / printf 只会进入日志，不会混入协议流。
    """

    def __init__(self) -> None:
        sys.stdout.flush()
        proto_fd = os.dup(1)
        os.dup2(2, 1)
        sys.stdout = sys.stderr
        self._out = os.fdopen(proto_fd, "w", encoding="utf-8", buffering=1, newline="\n")
        self._lock = threading.Lock()

    def send(self, event: str, job: str | None = None, data: Mapping[str, Any] | None = None) -> None:
        msg: dict[str, Any] = {"event": event}
        if job is not None:
            msg["job"] = job
        if data is not None:
            msg["data"] = data
        line = json.dumps(msg, ensure_ascii=False)
        with self._lock:
            self._out.write(line + "\n")
            self._out.flush()


class Job:
    """传给任务处理函数的上下文：参数、输出目录、进度上报与取消检查。"""

    def __init__(self, channel: _Channel, job_id: str, task: str, params: Mapping[str, Any], output_dir: str) -> None:
        self.id = job_id
        self.task = task
        self.params = params
        self.output_dir = Path(output_dir)
        self._channel = channel
        self._cancel = threading.Event()
        # fraction 是最近一次上报的进度，供只想更新消息、不改变进度的调用方使用。
        self.fraction = 0.0

    def progress(self, fraction: float, message: str = "") -> None:
        """上报进度并检查取消；fraction 会被裁剪到 [0, 1]。已取消时抛出 Cancelled。"""
        self.check_cancelled()
        self.fraction = min(max(fraction, 0.0), 1.0)
        self._channel.send("progress", self.id, {"fraction": self.fraction, "message": message})

    def artifact(self, path: str | os.PathLike[str], kind: str = "file") -> None:
        """登记一个输出文件；path 应位于 output_dir 内。"""
        self._channel.send("artifact", self.id, {"path": str(path), "kind": kind})

    def check_cancelled(self) -> None:
        if self._cancel.is_set():
            raise Cancelled()

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()


Handler = Callable[[Job], None]


def doctor(allow_mps: bool = True) -> Handler:
    """返回环境检查处理函数：报告 Python、torch、计算设备与 CUDA 版本，结果写在最终进度消息中。

    serve() 默认为每个引擎注册 doctor()；不使用 MPS 的引擎（如 CosyVoice）自行注册 doctor(allow_mps=False)。
    """
    return lambda job: _doctor(job, allow_mps)


def _doctor(job: Job, allow_mps: bool) -> None:
    import platform

    parts = [f"Python {platform.python_version()}"]
    try:
        import torch

        from .device import pick

        dev = pick(allow_mps=allow_mps)
        parts.append(f"torch {torch.__version__}")
        if torch.version.cuda:
            parts.append(f"CUDA {torch.version.cuda}")
        parts.append(f"设备 {dev.label}")
    except ImportError:
        parts.append("未安装 torch")
    job.progress(1.0, " · ".join(parts))


def serve(engine: str, version: str, handlers: Mapping[str, Handler]) -> None:
    """运行 worker 直到收到 shutdown 或 stdin 关闭。

    handlers 把任务名映射到处理函数（另自动提供 <engine>.doctor 环境检查）；处理函数在后台线程执行，主线程持续读取 stdin，
    以便随时收到 cancel。处理函数抛出的异常转为 error 事件，不会终止 worker。
    """
    channel = _Channel()
    handlers = {f"{engine}.doctor": doctor(), **handlers}
    channel.send("hello", data={"engine": engine, "version": version, "tasks": sorted(handlers), "protocol": PROTOCOL_VERSION})

    current: Job | None = None
    worker_thread: threading.Thread | None = None

    def run(job: Job) -> None:
        handler = handlers.get(job.task)
        try:
            if handler is None:
                raise ValueError(f"未知任务：{job.task}")
            job.output_dir.mkdir(parents=True, exist_ok=True)
            handler(job)
            job.check_cancelled()
            channel.send("done", job.id)
        except Cancelled:
            channel.send("canceled", job.id)
        except Exception as exc:  # noqa: BLE001 - 任何失败都要上报给 Conductor
            traceback.print_exc(file=sys.stderr)
            channel.send("error", job.id, {"message": f"{type(exc).__name__}: {exc}"})

    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            req = json.loads(raw)
        except json.JSONDecodeError:
            print(f"无法解析的请求：{raw!r}", file=sys.stderr)
            continue
        method = req.get("method")
        if method == "run":
            if worker_thread is not None and worker_thread.is_alive():
                channel.send("error", req.get("job"), {"message": "worker 正忙：同一时间只执行一个任务"})
                continue
            current = Job(channel, req["job"], req["task"], req.get("params") or {}, req["output_dir"])
            worker_thread = threading.Thread(target=run, args=(current,), daemon=True)
            worker_thread.start()
        elif method == "cancel":
            if current is not None and current.id == req.get("job"):
                current._cancel.set()
        elif method == "shutdown":
            break
        else:
            print(f"未知请求：{method!r}", file=sys.stderr)

    # stdin 关闭或收到 shutdown：通知正在运行的任务取消，最多等待 5 秒后退出。
    if current is not None:
        current._cancel.set()
    if worker_thread is not None:
        worker_thread.join(timeout=5)
