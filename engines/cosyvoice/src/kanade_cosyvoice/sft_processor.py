from __future__ import annotations

import logging

import pyarrow.parquet as pq


def parquet_opener(data, mode: str = "train"):
    """
    覆盖上游的 parquet opener（来自 VoiceLab voicelab_cosyvoice/processor.py）。

    Why this exists:
    - When the number of parquet shards is smaller than DataLoader workers, CosyVoice's
      shard-level sampling can assign the same shard to multiple workers, duplicating
      samples within an epoch.
    - This opener splits rows within a shard by worker_id to avoid duplication.

    Expected sample fields (provided by CosyVoice DataList sampler):
    - src: parquet path
    - worker_id: int
    - num_workers: int
    """
    for sample in data:
        assert "src" in sample
        url = sample["src"]
        worker_id = int(sample.get("worker_id", 0))
        num_workers = int(sample.get("num_workers", 1))

        try:
            row_idx = 0
            for batch in pq.ParquetFile(url).iter_batches(batch_size=64):
                df = batch.to_pandas()
                for i in range(len(df)):
                    if num_workers > 1 and (row_idx % num_workers) != worker_id:
                        row_idx += 1
                        continue
                    sample.update(dict(df.loc[i]))
                    yield {**sample}
                    row_idx += 1
        except Exception as exc:
            logging.warning("Failed to open %s, ex=%s", url, exc)

