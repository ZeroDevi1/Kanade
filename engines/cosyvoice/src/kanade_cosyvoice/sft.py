"""CosyVoice 3 说话人 SFT（对应 VoiceLab workflows/cosyvoice 的 xuan / xingtong / dream 流程）。

- cosyvoice.sft_prepare {name, dataset_dir, list?, base_model_dir, dev_ratio?, asr_model?, instruct?}
  读取 .list 文本（缺文本时可用 Whisper 转写）→ wetext 归一化 → campplus 提取说话人向量 → 写 parquet 与 data.list。
- cosyvoice.sft_assemble {name, base_model_dir?}：用已保存的 checkpoint 平均并组装（训练中途失败后使用）。
- cosyvoice.sft_train {name, base_model_dir, part (llm|flow|both), epochs, lr?, num_workers?, llm_layers?}
  需要 NVIDIA CUDA。llm_layers > 0 时 LLM 只训练 Qwen 顶部这么多层（8 GB 显存用 12，见 sft_launch._install_freeze_hook）；
  0 或缺省为全量训练，约需 12 GB 以上显存；-1 为按显存自动选择（见 auto_llm_layers）。依次训练所选部分 → val_best 平均 checkpoint → 组装模型目录（含 spk2info.pt）。

工作目录：<models>/cosyvoice/_sft/<name>/{data/train, data/dev, exp, tensorboard}。
产出模型：<models>/cosyvoice/<name>-SFT/，与基础模型同版本，Cantus 中以「预训练音色」模式使用，音色名即 name。

与 VoiceLab 的差异：不经过 kaldi 文本文件（wav.scp 按空白切分，路径含空格会出错），直接由 Python 数据结构写 parquet；
不使用 torchrun（单卡单进程，环境变量由这里设置）；Windows 用 gloo 后端（NCCL 不支持 Windows）。
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
from pathlib import Path

from kanade_engine import Cancelled, Job, annotations

from . import download as dl

INSTRUCT_DEFAULT = "You are a helpful assistant.<|endofprompt|>"
# 时长范围：太短的片段没有有效信息；上限与上游 filter 的 max_length（6000 帧 ≈ 60 秒）相比留出余量。
MIN_SECONDS, MAX_SECONDS = 0.5, 30.0
# 每个 parquet 分片的条数：VoiceLab 经验值，让分片数不少于 num_workers 的 2–4 倍，多 worker 读取更均匀。
UTTS_PER_PARQUET = 50
_NAME = re.compile(r"^[\w\-一-鿿]+$")


def _models_root() -> Path:
    return Path(os.environ.get("KANADE_MODELS_DIR", Path.home() / ".cache" / "kanade")) / "cosyvoice"


def work_dir(name: str) -> Path:
    return _models_root() / "_sft" / name


def output_model_dir(name: str) -> Path:
    return _models_root() / f"{name}-SFT"


def _check_name(name: str) -> str:
    name = (name or "").strip()
    if not _NAME.match(name):
        raise ValueError("音色名只能包含字母、数字、中文、下划线与连字符")
    return name


# ---------- 数据准备 ----------

def _normalizer(base_model_dir: Path, job: Job):
    """返回中文文本归一化函数；wetext 资源不可用时退回只做空白清理，并在进度中提示。"""
    from .tasks import wetext_dir

    wdir = wetext_dir(base_model_dir)
    try:
        dl.ensure_wetext(job, wdir)
        import wetext.wetext

        wetext.wetext.snapshot_download = lambda *a, **k: str(wdir)
        zh = wetext.wetext.Normalizer(remove_erhua=False)
        return lambda s: zh.normalize(s)
    except Exception as exc:  # noqa: BLE001
        job.progress(job.fraction, f"警告：文本归一化不可用（{exc}），按原文训练")
        return lambda s: s


def _transcriber(model_name: str, job: Job):
    """openai-whisper 转写器（openai-whisper 已是 CosyVoice 的依赖）。首次使用会下载 Whisper 模型。"""
    import whisper

    from kanade_engine import pick_device

    dev = pick_device(allow_mps=False)  # whisper 的部分算子在 MPS 上不受支持
    job.progress(job.fraction, f"加载 Whisper {model_name}（{dev.label}）")
    root = _models_root().parent / "_cache" / "whisper"
    model = whisper.load_model(model_name, device=dev.name, download_root=str(root))
    return lambda path: model.transcribe(path, fp16=dev.supports_half)["text"].strip()


def run_prepare(job: Job) -> None:
    p = job.params
    name = _check_name(p.get("name"))
    dataset = Path(p.get("dataset_dir") or "")
    base = Path(p.get("base_model_dir") or "")
    if not dataset.is_dir():
        raise ValueError(f"数据集目录不存在：{dataset}")
    if not (base / "cosyvoice3.yaml").is_file() or not dl.is_complete(base):
        raise ValueError("SFT 需要已下载完成的 Fun-CosyVoice 3 基础模型")
    # 校验通过后再导入重量级依赖，参数错误能立刻返回。
    import numpy as np
    import onnxruntime
    import pandas as pd
    import soundfile as sf
    import torch
    import torchaudio
    import torchaudio.compliance.kaldi as kaldi

    instruct = p.get("instruct") or INSTRUCT_DEFAULT
    dev_ratio = float(p.get("dev_ratio") or 0.05)
    asr_model = p.get("asr_model") or ""

    # 1. 收集「音频 + 文本」：优先 .list 文本；没有文本的条目在给出 asr_model 时转写，否则跳过。
    list_path = Path(p["list"]) if p.get("list") else annotations.find_list(dataset)
    if list_path:
        found, missing = annotations.match(annotations.parse(list_path), dataset)
        items = [(audio, e.text) for e, audio in found]
        job.progress(0.01, f"{list_path.name}：{len(found)} 条找到音频，{len(missing)} 条缺失")
    else:
        items = [(a, "") for a in annotations.audio_files(dataset)]
    transcribe = None
    norm = _normalizer(base, job)
    records, skipped = [], {"no_text": 0, "duration": 0}
    for i, (audio, text) in enumerate(items):
        job.progress(0.02 + 0.38 * i / max(len(items), 1), f"整理数据 {i + 1}/{len(items)}")
        info = sf.info(str(audio))
        if not (MIN_SECONDS <= info.duration <= MAX_SECONDS):
            skipped["duration"] += 1
            continue
        if not text:
            if not asr_model:
                skipped["no_text"] += 1
                continue
            transcribe = transcribe or _transcriber(asr_model, job)
            text = transcribe(str(audio))
        text = norm(text).strip()
        if not text:
            skipped["no_text"] += 1
            continue
        utt = f"{name}_{audio.stem}"
        records.append({"utt": utt, "wav": str(audio), "text": text})
    if len(records) < 2:
        raise ValueError(f"可用数据不足：{len(records)} 条（无文本 {skipped['no_text']}，时长不符 {skipped['duration']}）")

    # 2. 说话人向量：campplus 对每条提取，再取训练集平均作为音色向量（与上游 tools/extract_embedding.py 相同的特征）。
    opt = onnxruntime.SessionOptions()
    opt.graph_optimization_level = onnxruntime.GraphOptimizationLevel.ORT_ENABLE_ALL
    opt.intra_op_num_threads = 1
    sess = onnxruntime.InferenceSession(str(base / "campplus.onnx"), sess_options=opt, providers=["CPUExecutionProvider"])
    for i, r in enumerate(records):
        job.progress(0.4 + 0.4 * i / len(records), f"提取说话人向量 {i + 1}/{len(records)}")
        audio, sr = torchaudio.load(r["wav"], backend="soundfile")
        audio = audio.mean(dim=0, keepdim=True)
        if sr != 16000:
            audio = torchaudio.transforms.Resample(orig_freq=sr, new_freq=16000)(audio)
        feat = kaldi.fbank(audio, num_mel_bins=80, dither=0, sample_frequency=16000)
        feat = feat - feat.mean(dim=0, keepdim=True)
        r["utt_embedding"] = sess.run(None, {sess.get_inputs()[0].name: feat.unsqueeze(0).numpy()})[0].flatten().tolist()

    # 3. 按 utt 的哈希稳定划分训练 / 验证集（重跑结果一致），验证集至少 1 条。
    def is_dev(utt: str) -> bool:
        return int(hashlib.sha1(utt.encode()).hexdigest()[:8], 16) / 0xFFFFFFFF < dev_ratio

    dev = [r for r in records if is_dev(r["utt"])] or [records[-1]]
    train = [r for r in records if r not in dev]
    spk_emb = np.mean([r["utt_embedding"] for r in train], axis=0).tolist()

    # 4. 写 parquet 分片与 data.list（列与上游 tools/make_parquet_list.py 一致）。
    wd = work_dir(name)
    for split, rows in (("train", train), ("dev", dev)):
        out = wd / "data" / split
        shutil.rmtree(out, ignore_errors=True)
        out.mkdir(parents=True)
        shards = []
        for s in range(0, len(rows), UTTS_PER_PARQUET):
            chunk = rows[s:s + UTTS_PER_PARQUET]
            df = pd.DataFrame({
                "utt": [r["utt"] for r in chunk],
                "audio_data": [Path(r["wav"]).read_bytes() for r in chunk],
                "wav": [r["wav"] for r in chunk],
                "text": [r["text"] for r in chunk],
                "spk": [name] * len(chunk),
                "utt_embedding": [r["utt_embedding"] for r in chunk],
                "spk_embedding": [spk_emb] * len(chunk),
                "instruct": [instruct] * len(chunk),
            })
            shard = out / f"parquet_{len(shards):06d}.tar"
            df.to_parquet(shard)
            shards.append(str(shard))
        (out / "data.list").write_text("\n".join(shards) + "\n", encoding="utf-8")
    torch.save({name: spk_emb}, wd / "data" / "spk2embedding.pt")
    meta = {"name": name, "base_model_dir": str(base), "train": len(train), "dev": len(dev), "skipped": skipped,
            "instruct": instruct, "dataset_dir": str(dataset), "list": str(list_path) if list_path else ""}
    (wd / "prepare.json").write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    job.progress(1.0, f"完成：训练 {len(train)} 条，验证 {len(dev)} 条（跳过：无文本 {skipped['no_text']}，时长不符 {skipped['duration']}）")


# ---------- 训练 ----------

def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _kill_tree(pid: int) -> None:
    import psutil

    try:
        parent = psutil.Process(pid)
    except psutil.NoSuchProcess:
        return
    for proc in parent.children(recursive=True) + [parent]:
        try:
            proc.kill()
        except psutil.NoSuchProcess:
            pass


_EPOCH_CKPT = re.compile(r"epoch_(\d+)_whole\.pt$")


def _run(job: Job, args: list[str], lo: float, hi: float, label: str, epochs: int = 0, exp_dir: Path | None = None,
         extra_env: dict[str, str] | None = None) -> None:
    """运行 sft_launch 子进程；训练阶段按 exp_dir 中已保存的 epoch_N_whole.pt 计算进度。extra_env 追加到子进程环境。"""
    env = dict(os.environ, **(extra_env or {}))
    vendor = Path(__file__).resolve().parents[2] / "vendor"
    env["PYTHONPATH"] = os.pathsep.join([str(vendor), str(Path(__file__).resolve().parents[1]), env.get("PYTHONPATH", "")])
    env.update({"RANK": "0", "LOCAL_RANK": "0", "WORLD_SIZE": "1", "LOCAL_WORLD_SIZE": "1",
                "MASTER_ADDR": "127.0.0.1", "MASTER_PORT": str(_free_port()), "PYTHONIOENCODING": "UTF-8"})
    proc = subprocess.Popen([sys.executable, "-m", "kanade_cosyvoice.sft_launch", *args], env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, encoding="utf-8",
                            errors="replace", bufsize=1, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    canceled = threading.Event()

    def watch() -> None:
        while proc.poll() is None:
            if job.cancelled:
                canceled.set()
                _kill_tree(proc.pid)
                return
            if epochs and exp_dir is not None:
                done = max((int(m.group(1)) + 1 for f in exp_dir.glob("epoch_*_whole.pt") if (m := _EPOCH_CKPT.search(f.name))), default=0)
                job.progress(lo + (hi - lo) * min(done / epochs, 1.0), f"{label}：已完成 {done}/{epochs} 轮")
            threading.Event().wait(5)

    threading.Thread(target=watch, daemon=True).start()
    job.progress(lo, label)
    tail: list[str] = []
    assert proc.stdout is not None
    for line in proc.stdout:
        print(line.rstrip(), file=sys.stderr)
        tail = (tail + [line.rstrip()])[-20:]
    code = proc.wait()
    if canceled.is_set():
        raise Cancelled()
    if code != 0:
        raise RuntimeError(f"{label}失败（退出码 {code}）：" + " / ".join(tail[-3:]))


def run_train(job: Job) -> None:
    import torch

    p = job.params
    name = _check_name(p.get("name"))
    if not torch.cuda.is_available():
        raise RuntimeError("CosyVoice SFT 训练需要 NVIDIA CUDA（上游训练代码只支持 CUDA）；数据准备可在本机完成，训练请在 Windows / Linux GPU 机器上进行")
    wd = work_dir(name)
    meta_path = wd / "prepare.json"
    if not meta_path.is_file():
        raise ValueError("请先完成数据准备")
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    base = Path(p.get("base_model_dir") or meta["base_model_dir"])
    parts = {"llm": ["llm"], "flow": ["flow"], "both": ["llm", "flow"]}[p.get("part") or "llm"]
    epochs = int(p.get("epochs") or 30)
    lr = float(p.get("lr") or 1e-5)
    workers = int(p.get("num_workers") or 4)
    llm_layers = int(p.get("llm_layers") or 0)
    if llm_layers < 0:
        llm_layers = auto_llm_layers()
        job.progress(0.0, "LLM 训练范围：" + ("全部层" if llm_layers == 0 else f"顶部 {llm_layers} 层") + "（按显存自动选择）")

    # 训练时在线提取语音 token 需要批量版 tokenizer，推理下载按 _SKIP 跳过了它（约 1 GB），这里按需补下。
    dl.ensure_files(job, base, ["speech_tokenizer_v3.batch.onnx"])

    template = (Path(__file__).parent / "configs" / "cosyvoice3_sft.yaml").read_text(encoding="utf-8")
    cfg = wd / "cosyvoice3_sft.yaml"
    cfg.write_text(template.replace("{max_epoch}", str(epochs)).replace("{lr}", str(lr)), encoding="utf-8")
    backend = "gloo" if sys.platform == "win32" else "nccl"

    span = 0.9 / len(parts)
    for i, part in enumerate(parts):
        exp = wd / "exp" / part
        exp.mkdir(parents=True, exist_ok=True)
        # 参数与 VoiceLab 文档第 6 步一致；--checkpoint 为基础模型权重。
        _run(job, ["train.py", "--train_engine", "torch_ddp", "--config", str(cfg),
                   "--train_data", str(wd / "data" / "train" / "data.list"), "--cv_data", str(wd / "data" / "dev" / "data.list"),
                   "--qwen_pretrain_path", str(base / "CosyVoice-BlankEN"), "--onnx_path", str(base),
                   "--model", part, "--checkpoint", str(base / f"{part}.pt"), "--model_dir", str(exp),
                   "--tensorboard_dir", str(wd / "tensorboard" / part), "--ddp.dist_backend", backend,
                   "--num_workers", str(workers), "--prefetch", "2", "--use_amp"],
             i * span, (i + 0.95) * span, f"训练 {part}", epochs, exp, {"KANADE_SFT_LLM_LAYERS": str(llm_layers)})
        _average(job, exp, part, (i + 0.95) * span, (i + 1) * span)

    job.progress(0.92, "组装模型目录")
    out = assemble(base, name, wd)
    job.artifact(out, "cosyvoice-model")
    job.progress(1.0, f"完成：{out.name}")


def _average(job: Job, exp: Path, part: str, lo: float, hi: float) -> None:
    """取验证损失最好的若干 checkpoint 平均：llm 3 个、flow 5 个（VoiceLab 文档第 7 步）。
    轮数少于该数量时取全部：average_model.py 断言实际取到的个数等于 --num（只统计 epoch_*_whole.yaml，见其 --val_best）。"""
    saved = len(list(exp.glob("epoch_*_whole.yaml")))
    num = min(3 if part == "llm" else 5, saved)
    if num == 0:
        raise RuntimeError(f"{part} 训练没有产出 checkpoint")
    _run(job, ["average_model.py", "--dst_model", str(exp / f"{part}_avg.pt"), "--src_path", str(exp),
               "--num", str(num), "--val_best"], lo, hi, f"平均 {part} checkpoint（{num} 个）")


def saved_parts(name: str) -> list[str]:
    """已有保存的 checkpoint 的训练部分（llm / flow）。"""
    wd = work_dir(name)
    return [p for p in ("llm", "flow") if any((wd / "exp" / p).glob("epoch_*_whole.yaml"))]


def run_assemble(job: Job) -> None:
    """用已保存的 checkpoint 平均并组装模型：训练中途失败（如显卡驱动重置）或提前取消后，不必重新训练。

    只处理有 checkpoint 的部分；平均不需要 CUDA（average_model.py 在 CPU 上加载权重）。
    """
    p = job.params
    name = _check_name(p.get("name"))
    wd = work_dir(name)
    meta_path = wd / "prepare.json"
    if not meta_path.is_file():
        raise ValueError("找不到该音色的训练目录")
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    base = Path(p.get("base_model_dir") or meta["base_model_dir"])
    parts = saved_parts(name)
    if not parts:
        raise ValueError("没有已保存的 checkpoint，请先训练")
    for i, part in enumerate(parts):
        _average(job, wd / "exp" / part, part, 0.9 * i / len(parts), 0.9 * (i + 1) / len(parts))
    job.progress(0.92, "组装模型目录")
    out = assemble(base, name, wd)
    job.artifact(out, "cosyvoice-model")
    job.progress(1.0, f"完成：{out.name}（{'、'.join(parts)}）")


def auto_llm_layers() -> int:
    """显存 12 GB 及以上全量训练 LLM，否则只训顶部 12 层（8 GB 显卡实测可行，见 docs/windows-test-plan.md）。"""
    import torch

    gb = torch.cuda.get_device_properties(0).total_memory / 2**30
    # 「12 GB」显卡报告的总显存约 11.6–12.0 GiB，阈值留出余量。
    return 0 if gb >= 11.5 else 12


def assemble(base: Path, name: str, wd: Path) -> Path:
    """组装 SFT 模型目录：基础模型文件硬链接（不占额外空间，失败时复制），替换训练过的权重，写入 spk2info.pt。"""
    import torch

    out = output_model_dir(name)
    shutil.rmtree(out, ignore_errors=True)
    for src in base.rglob("*"):
        rel = src.relative_to(base)
        dst = out / rel
        if src.is_dir():
            dst.mkdir(parents=True, exist_ok=True)
            continue
        if rel.name in (dl.COMPLETE_MARKER, "spk2info.pt"):
            continue
        dst.parent.mkdir(parents=True, exist_ok=True)
        try:
            os.link(src, dst)
        except OSError:
            shutil.copy2(src, dst)
    for part in ("llm", "flow"):
        avg = wd / "exp" / part / f"{part}_avg.pt"
        if avg.is_file():
            (out / f"{part}.pt").unlink(missing_ok=True)
            shutil.copy2(avg, out / f"{part}.pt")
    emb = torch.load(wd / "data" / "spk2embedding.pt")[name]
    torch.save({name: {"embedding": torch.tensor(emb, dtype=torch.float32).reshape(1, -1)}}, out / "spk2info.pt")
    (out / "kanade-sft.json").write_text(json.dumps({"spk_id": name, "base": base.name}, ensure_ascii=False), encoding="utf-8")
    (out / dl.COMPLETE_MARKER).write_text("{}", encoding="utf-8")
    return out
