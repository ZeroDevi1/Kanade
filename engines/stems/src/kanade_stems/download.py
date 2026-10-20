"""替换 audio-separator 的文件下载，改用 SDK 的断点续传下载并上报进度。

上游 Separator.download_file_if_not_exists 直接写目标文件，下载中断会留下残缺文件且下次被当作已下载。
HTTP 错误转为 RuntimeError，保持上游「依次尝试多个地址」的回退行为。
"""

from __future__ import annotations

from pathlib import Path

from kanade_engine import Job
from kanade_engine.download import HTTPStatusError, fetch


def patch_downloader(job: Job) -> None:
    from audio_separator.separator import Separator

    def download_file_if_not_exists(self, url: str, output_path: str) -> None:
        dst = Path(output_path)
        if dst.is_file():
            return

        def report(have: int, total: int) -> None:
            # 只更新消息、不改变任务进度比例（进度由调用方按步骤控制）；同时检查取消。
            size = f" / {total / 1e6:.0f}" if total else ""
            job.progress(job.fraction, f"下载 {dst.name}：{have / 1e6:.0f}{size} MB")

        try:
            fetch(url, dst, report)
        except HTTPStatusError as exc:
            raise RuntimeError(f"Failed to download file from {url}, response code: {exc.code}") from exc

    Separator.download_file_if_not_exists = download_file_if_not_exists
