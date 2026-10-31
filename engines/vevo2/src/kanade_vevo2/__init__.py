"""SDK-only adapter; all ML dependencies stay in the selected upstream environment."""
from __future__ import annotations

import json
import math
import os
from pathlib import Path
import subprocess
import tempfile

from kanade_engine.runtime import Job, serve

AR = "contentstyle_modeling/posttrained"
FM = "acoustic_modeling/fm_emilia101k_singnet7k_repa"
REQUIRED = (
    AR + "/amphion_config.json", AR + "/config.json", AR + "/model.safetensors",
    AR + "/tokenizer.json", AR + "/tokenizer_config.json",
    AR + "/added_tokens.json", AR + "/special_tokens_map.json", AR + "/merges.txt", AR + "/vocab.json",
    FM + "/config.json", FM + "/model.safetensors",
    "tokenizer/contentstyle_fvq16384_12.5hz/model.safetensors",
    "tokenizer/prosody_fvq512_6.25hz/model.safetensors",
    "vocoder/config.json", "vocoder/model.safetensors",
)


def local_file(value, label, *, nonempty=False):
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"请配置 {label}")
    path = Path(value.strip()).expanduser().resolve()
    if not path.is_file() or (nonempty and path.stat().st_size == 0):
        raise ValueError(f"{label} 文件不存在或为空：{path}")
    return str(path)


def request(params, output_dir, *, doctor=False):
    p = dict(params)
    if p.get("license_accepted") is not True:
        raise ValueError("请先确认已阅读 Vevo2 权重 CC-BY-NC-ND 4.0 许可；不得视为商用授权")
    p["python_path"] = local_file(p.get("python_path"), "Vevo2 独立 Python")
    for key in ("source_dir", "model_dir"):
        value = p.get(key)
        if not isinstance(value, str) or not value.strip() or not Path(value.strip()).expanduser().is_dir():
            raise ValueError(f"请配置存在的本地 {key} 目录")
        p[key] = str(Path(value.strip()).expanduser().resolve())
    local_file(str(Path(p["source_dir"]) / "models/svc/vevo2/vevo2_utils.py"), "Amphion 源码")
    for name in REQUIRED:
        local_file(str(Path(p["model_dir"]) / name), "Vevo2 " + name, nonempty=True)
    p["whisper_path"] = local_file(p.get("whisper_path"), "Whisper medium.pt", nonempty=True)
    p["whisper_stats_path"] = local_file(p.get("whisper_stats_path") or str(Path(p["model_dir"]) / FM / "whisper_stats.pt"), "Whisper 统计文件", nonempty=True)
    p["output_dir"], p["doctor"] = str(Path(output_dir).resolve()), doctor
    if doctor:
        return p
    p["source_audio"] = local_file(p.get("source_audio"), "原唱干声", nonempty=True)
    if p.get("accompaniment_audio"):
        p["accompaniment_audio"] = local_file(p["accompaniment_audio"], "伴奏", nonempty=True)
    for key in ("original_text", "target_text"):
        value = p.get(key)
        if not isinstance(value, str) or not value.strip() or len(value) > 10000:
            raise ValueError(f"{key} 需要非空歌词（最多 10000 字符）")
        p[key] = " ".join(value.split())
    for key, default, low, high in (("seed", 0, 0, 2147483647), ("flow_matching_steps", 32, 1, 128)):
        value = p.get(key, default)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or int(value) != value or not low <= value <= high:
            raise ValueError(f"{key} 必须是 {low}–{high} 的整数")
        p[key] = int(value)
    return p


def run(job: Job, *, doctor=False):
    p = request(job.params, job.output_dir, doctor=doctor)
    job.progress(0.05, "检查 Vevo2 本地依赖" if doctor else "启动 Vevo2 AR + FM 改词；请先测试短片段")
    job.output_dir.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1", HF_DATASETS_OFFLINE="1", PYTHONUNBUFFERED="1")
    with tempfile.TemporaryDirectory(prefix="vevo2-", dir=job.output_dir) as tmp:
        req, result = Path(tmp) / "request.json", Path(tmp) / "result.json"
        p["result_path"] = str(result)
        req.write_text(json.dumps(p, ensure_ascii=False), encoding="utf-8")
        log_path = job.output_dir / "vevo2.log"
        with log_path.open("w", encoding="utf-8") as log:
            proc = subprocess.Popen([p["python_path"], str(Path(__file__).with_name("worker.py").resolve()), str(req)],
                                    cwd=p["source_dir"], env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
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
        if code != 0 or not result.is_file():
            raise RuntimeError(f"Vevo2 子进程失败或未返回结果（{code}）；查看 {log_path}")
        data = json.loads(result.read_text(encoding="utf-8"))
        if doctor:
            job.progress(1, data["message"])
            return
        for name in ("vocal.wav", "mixed.wav"):
            if name == "mixed.wav" and not p.get("accompaniment_audio"):
                continue
            path = job.output_dir / name
            if not path.is_file() or path.stat().st_size <= 44:
                raise RuntimeError(f"Vevo2 未生成有效输出：{path}")
            job.artifact(path, "audio")
        job.progress(1, "Vevo2 AR + FM 完成 · 24000 Hz；请试听确认改词与旋律对齐")


def main():
    serve("vevo2", "0.1.0", {"vevo2.edit": run, "vevo2.doctor": lambda job: run(job, doctor=True)})
