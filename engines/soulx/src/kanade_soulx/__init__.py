"""SDK-only transport; SVS runs in the user's separately provisioned environment."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

from kanade_engine.runtime import Job, serve
from .validation import request


def run(job: Job, *, doctor=False):
    p = request(job.params, job.output_dir, doctor=doctor)
    job.progress(.05, "检查 SoulX SVS 本地环境" if doctor else "加载 SoulX SVS；目标歌词来自校准后的音素元数据")
    job.output_dir.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1", HF_DATASETS_OFFLINE="1", PYTHONUNBUFFERED="1")
    with tempfile.TemporaryDirectory(prefix="soulx-", dir=job.output_dir) as tmp:
        req, result = Path(tmp)/"request.json", Path(tmp)/"result.json"
        p["result_path"] = str(result)
        req.write_text(json.dumps(p, ensure_ascii=False), encoding="utf-8")
        log_path = job.output_dir/"soulx.log"
        with log_path.open("w", encoding="utf-8") as log:
            proc = subprocess.Popen([p["python_path"], str(Path(__file__).with_name("worker.py").resolve()), str(req)],
                cwd=p["source_dir"], env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
            try:
                while True:
                    job.check_cancelled()
                    try:
                        code = proc.wait(timeout=.15)
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
        if code != 0 or not result.is_file():
            raise RuntimeError(f"SoulX SVS 子进程失败（{code}），请查看 {log_path}")
        data = json.loads(result.read_text(encoding="utf-8"))
        if doctor:
            job.progress(1, data["message"])
            return
        output = job.output_dir/"vocal.wav"
        if not output.is_file() or output.stat().st_size <= 44:
            raise RuntimeError("SoulX SVS 未生成有效 vocal.wav")
        job.artifact(output, "audio")
        job.progress(1, "SoulX SVS 完成 · 24000 Hz；请试听检查改词、节奏和音色")


def main():
    serve("soulx", "0.1.0", {"soulx.edit": run, "soulx.doctor": lambda job: run(job, doctor=True)})
