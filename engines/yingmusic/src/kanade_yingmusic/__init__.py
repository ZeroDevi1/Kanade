"""Lightweight SDK adapter; the upstream ML environment runs in a child process."""
from __future__ import annotations

import json
import math
import os
from pathlib import Path
import subprocess
import tempfile

from kanade_engine.runtime import Job, serve


def _file(value, label):
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"请配置 {label}")
    path = Path(value).expanduser().resolve()
    if not path.is_file():
        raise ValueError(f"{label} 文件不存在：{path}")
    return str(path)


def request(params, output_dir, *, doctor=False):
    """Validate before spawning any ML process (and never accept remote model IDs)."""
    p = dict(params)
    p["python_path"] = _file(p.get("python_path"), "YingMusic Python")
    for key in ("source_dir", "model_dir"):
        value = p.get(key)
        if not isinstance(value, str) or not value.strip():
            raise ValueError(f"请配置 {key}")
        p[key] = str(Path(value).expanduser().resolve())
    _file(str(Path(p["source_dir"]) / "src/YingMusicSinger/infer/YingMusicSinger.py"), "YingMusic 源码")
    _file(str(Path(p["model_dir"]) / "config.json"), "本地模型 config.json")
    _file(str(Path(p["model_dir"]) / "model.safetensors"), "本地模型 model.safetensors")
    p["doctor"] = doctor
    p["output_dir"] = str(Path(output_dir).resolve())
    if doctor:
        return p
    p["melody_audio"] = _file(p.get("melody_audio"), "纯人声旋律参考")
    p["ref_audio"] = _file(p.get("ref_audio") or p["melody_audio"], "音色参考")
    if p.get("accompaniment_audio"):
        p["accompaniment_audio"] = _file(p["accompaniment_audio"], "伴奏")
    for key in ("ref_text", "target_text"):
        value = p.get(key)
        if not isinstance(value, str) or not value.strip() or len(value) > 10000:
            raise ValueError(f"{key} 需要非空歌词（最多 10000 字符）")
        parts = value.strip().replace("\r\n", "\n").replace("\r", "\n").replace("\n", "|").split("|")
        if any(not part.strip() for part in parts):
            raise ValueError(f"{key} 不能包含空句")
        p[key] = "|".join(part.strip() for part in parts)
    for key, default, low, high, integer in (
        ("seed", -1, -1, 2147483647, True),
        ("nfe_step", 32, 1, 128, True),
        ("cfg_strength", 3.0, 0, 10, False),
    ):
        value = p.get(key, default)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
            raise ValueError(f"{key} 必须是有效数字")
        if not low <= value <= high or (integer and int(value) != value):
            raise ValueError(f"{key} 范围：{low}–{high}")
        p[key] = int(value) if integer else float(value)
    return p


def run(job: Job, *, doctor=False):
    p = request(job.params, job.output_dir, doctor=doctor)
    job.progress(0.05, "检查独立 YingMusic 环境" if doctor else "启动离线歌声改词；首次加载可能较慢")
    job.output_dir.mkdir(parents=True, exist_ok=True)
    worker = Path(__file__).with_name("worker.py").resolve()
    env = dict(os.environ, HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1", HF_DATASETS_OFFLINE="1", PYTHONUNBUFFERED="1")
    # Request files keep long/private lyrics out of process arguments. No shell execution.
    with tempfile.TemporaryDirectory(prefix="yingmusic-", dir=job.output_dir) as tmp:
        req = Path(tmp) / "request.json"
        result = Path(tmp) / "result.json"
        p["result_path"] = str(result)
        req.write_text(json.dumps(p, ensure_ascii=False), encoding="utf-8")
        log_path = job.output_dir / "yingmusic.log"
        with log_path.open("w", encoding="utf-8") as log:
            proc = subprocess.Popen([p["python_path"], str(worker), str(req)], cwd=p["source_dir"], env=env,
                                    stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
            try:
                while True:
                    job.check_cancelled()
                    try:
                        code = proc.wait(timeout=0.15)
                        break
                    except subprocess.TimeoutExpired:
                        pass
                job.check_cancelled()
            finally:
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
        if code != 0:
            raise RuntimeError(f"YingMusic 子进程失败（{code}）；请查看 {log_path}")
        if not result.is_file():
            raise RuntimeError(f"YingMusic 未返回结果；请查看 {log_path}")
        data = json.loads(result.read_text(encoding="utf-8"))
        if doctor:
            job.progress(1, data["message"])
            return
        for name in ("vocal.wav", "mixed.wav"):
            if name == "mixed.wav" and not p.get("accompaniment_audio"):
                continue
            path = job.output_dir / name
            if not path.is_file() or path.stat().st_size <= 44:
                raise RuntimeError(f"YingMusic 未生成有效输出：{path}")
            job.artifact(path, "audio")
        job.progress(1, f"改词完成 · 44100 Hz · seed {data['seed']}；音色与旋律保真需试听确认")


def main():
    serve("yingmusic", "0.1.0", {"yingmusic.edit": run, "yingmusic.doctor": lambda job: run(job, doctor=True)})
