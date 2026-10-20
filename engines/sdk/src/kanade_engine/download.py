"""引擎共用的文件下载：断点续传、按字节回调进度、下载源镜像。

镜像由 Go 端经环境变量传入（设置页的下载镜像选项）：
- KANADE_HF_ENDPOINT：替换 https://huggingface.co（例如 https://hf-mirror.com）。
- KANADE_GITHUB_MIRROR：作为前缀加在 https://github.com/ 地址前（例如 https://ghfast.top/）。
"""

from __future__ import annotations

import http.client
import os
import ssl
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable

CHUNK = 1 << 20
TIMEOUT = 60
# 进度回调的最小间隔（字节），避免事件过多。
REPORT_EVERY = 16 * CHUNK
# 网络瞬断（连接重置、SSL EOF、超时、5xx / 429）时的重试次数与首次等待秒数，之后每次翻倍：2、4、8、16、32 秒。
RETRIES = 5
BACKOFF = 2.0


def mirror(url: str) -> str:
    """按环境变量把下载地址改写到镜像。"""
    hf = os.environ.get("KANADE_HF_ENDPOINT", "").strip().rstrip("/")
    if hf and url.startswith("https://huggingface.co/"):
        return hf + url[len("https://huggingface.co"):]
    gh = os.environ.get("KANADE_GITHUB_MIRROR", "").strip()
    if gh and url.startswith("https://github.com/"):
        return gh.rstrip("/") + "/" + url
    return url


class HTTPStatusError(IOError):
    def __init__(self, url: str, code: int):
        super().__init__(f"下载失败（HTTP {code}）：{url}")
        self.code = code


def fetch(url: str, dst: Path, on_bytes: Callable[[int, int], None] | None = None, size: int = 0) -> None:
    """下载 url 到 dst：先写 dst.part，按 Range 续传，长度校验通过后改名。

    on_bytes(已下载字节, 总字节) 至少每 REPORT_EVERY 字节调用一次，结束时再调用一次；回调可抛出异常中止下载
    （例如任务取消），已下载部分保留在 .part 中供下次续传。size 为已知文件大小（0 表示以响应头为准）。
    dst 已存在时直接返回。网络瞬断时自动续传重试 RETRIES 次；4xx 错误立即抛出 HTTPStatusError。
    """
    for attempt in range(RETRIES + 1):
        try:
            _fetch_once(url, dst, on_bytes, size)
            return
        except HTTPStatusError as exc:
            if exc.code != 429 and exc.code < 500 or attempt == RETRIES:
                raise
        except (urllib.error.URLError, http.client.HTTPException, ConnectionError, TimeoutError, ssl.SSLError) as exc:
            if attempt == RETRIES:
                raise IOError(f"下载 {dst.name} 失败（已重试 {RETRIES} 次）：{exc}") from exc
        time.sleep(BACKOFF * (2 ** attempt))


def _fetch_once(url: str, dst: Path, on_bytes: Callable[[int, int], None] | None, size: int) -> None:
    if dst.exists():
        return
    dst.parent.mkdir(parents=True, exist_ok=True)
    part = dst.with_name(dst.name + ".part")
    have = part.stat().st_size if part.exists() else 0
    headers = {"User-Agent": "Kanade"}
    if have:
        headers["Range"] = f"bytes={have}-"
    try:
        resp = urllib.request.urlopen(urllib.request.Request(mirror(url), headers=headers), timeout=TIMEOUT)
    except urllib.error.HTTPError as exc:
        if exc.code == 416 and size and have == size:
            os.replace(part, dst)  # 上次已下载完整但未改名
            return
        raise HTTPStatusError(url, exc.code) from exc
    with resp:
        if have and resp.status != 206:
            have = 0  # 服务器不支持续传，从头下载
        total = size or (have + int(resp.headers.get("Content-Length") or 0))
        with open(part, "ab" if have else "wb") as out:
            last = have
            while chunk := resp.read(CHUNK):
                out.write(chunk)
                have += len(chunk)
                if on_bytes and have - last >= REPORT_EVERY:
                    last = have
                    on_bytes(have, total)
    if total and have != total:
        raise IOError(f"{dst.name} 大小不符：期望 {total}，实际 {have}")
    os.replace(part, dst)
    if on_bytes:
        on_bytes(have, total)
