"""RVC 训练：预处理 → 提取 f0 与特征 → 训练（自动续训）→ 建索引，各阶段以子进程运行 Applio 的脚本。

与 VoiceLab rvc_train.py 的行为对应：
- 数据集目录中有同名 .list（或显式给出 list）时，只用其中列出的音频（白名单）；做法是在实验目录下建
  _dataset_input/，硬链接（不行则复制）选中的音频，再交给预处理。
- 实验目录已有 G_*.pth / D_*.pth 时，Applio 训练脚本会自动续训，总轮数可以比上次大。
- skip_preprocess / skip_extract 用于续训时跳过已完成的阶段。

进度分配：预处理 0–8%，提取 8–20%，训练 20–97%，索引 97–100%。训练进度从脚本每轮输出的
「<名称> | epoch=N | step=…」行解析。
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import threading
from pathlib import Path

from kanade_engine import Cancelled, Job, annotations

from . import runtime

SAMPLE_RATES = (32000, 40000, 48000)
_EPOCH = re.compile(r"\| epoch=(\d+) \| step=(\d+)")
_NAME = re.compile(r"^[\w\-一-鿿]+$")


def run_prepare(job: Job) -> None:
    """预下载推理所需资源；train=True 时同时下载指定采样率的训练底模。"""
    assets = runtime.EMBEDDER + runtime.PREDICTORS["rmvpe"]
    if job.params.get("train"):
        assets += runtime.pretrained(int(job.params.get("sample_rate") or 48000))
    runtime.ensure(job, assets, 0.0, 1.0)
    job.progress(1.0, "完成")


def stage_dataset(job: Job, dataset: Path, list_path: Path | None, exp_dir: Path) -> Path:
    """返回交给预处理的目录：有标注时只包含标注中的音频，否则直接使用数据集目录。"""
    if list_path is None:
        list_path = annotations.find_list(dataset)
    if list_path is None:
        if not annotations.audio_files(dataset):
            raise ValueError(f"数据集目录中没有音频：{dataset}")
        return dataset
    found, missing = annotations.match(annotations.parse(list_path), dataset)
    if not found:
        raise ValueError(f"{list_path.name} 中的音频在 {dataset} 里都找不到")
    staged = exp_dir / "_dataset_input"
    shutil.rmtree(staged, ignore_errors=True)
    staged.mkdir(parents=True)
    for _, src in found:
        dst = staged / src.name
        try:
            os.link(src, dst)
        except OSError:
            shutil.copy2(src, dst)
    note = f"，{len(missing)} 条未找到" if missing else ""
    job.progress(job.fraction, f"按 {list_path.name} 选用 {len(found)} 条音频{note}")
    return staged


def run_script(job: Job, args: list[str], lo: float, hi: float, label: str, on_line=None) -> None:
    """以子进程运行 vendor 中的脚本，逐行转发输出到日志，并在取消时结束整个进程树。

    on_line(line) 可返回 [0, 1] 的阶段内进度；返回 None 表示该行不含进度。
    """
    cmd = [sys.executable, *args]
    job.progress(lo, label)
    proc = subprocess.Popen(cmd, cwd=runtime.ROOT, env=runtime.subprocess_env(), stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, encoding="utf-8", errors="replace", bufsize=1,
                            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    canceled = threading.Event()

    def watch() -> None:
        # 子进程阻塞在训练时不会产生输出行，取消需要独立线程轮询。
        while proc.poll() is None:
            if job.cancelled:
                canceled.set()
                _kill_tree(proc.pid)
                return
            threading.Event().wait(0.5)

    threading.Thread(target=watch, daemon=True).start()
    tail: list[str] = []
    assert proc.stdout is not None
    for line in proc.stdout:
        line = line.rstrip()
        print(line, file=sys.stderr)
        tail = (tail + [line])[-20:]
        if on_line:
            frac = on_line(line)
            if frac is not None and not canceled.is_set():
                job.progress(lo + (hi - lo) * frac, label + "：" + line[-120:])
    code = proc.wait()
    if canceled.is_set():
        raise Cancelled()
    if code != 0:
        raise RuntimeError(f"{label}失败（退出码 {code}）：" + " / ".join(tail[-3:]))


def _kill_tree(pid: int) -> None:
    import psutil

    try:
        parent = psutil.Process(pid)
    except psutil.NoSuchProcess:
        return
    procs = parent.children(recursive=True) + [parent]
    for p in procs:
        try:
            p.kill()
        except psutil.NoSuchProcess:
            pass


def run_train(job: Job) -> None:
    p = job.params
    name = (p.get("name") or "").strip()
    if not _NAME.match(name):
        raise ValueError("模型名只能包含字母、数字、中文、下划线与连字符")
    dataset = Path(p.get("dataset_dir") or "")
    if not dataset.is_dir() and not p.get("skip_preprocess"):
        raise ValueError(f"数据集目录不存在：{dataset}")
    sr = int(p.get("sample_rate") or 48000)
    if sr not in SAMPLE_RATES:
        raise ValueError(f"采样率须为 {SAMPLE_RATES} 之一")
    f0 = p.get("f0_method") or "rmvpe"
    epochs = int(p.get("epochs") or 50)
    batch = int(p.get("batch_size") or 4)
    save_every = int(p.get("save_every") or 5)
    cores = int(p.get("cpu_cores") or min(os.cpu_count() or 4, 8))
    exp_dir = runtime.LOGS / name
    exp_dir.mkdir(parents=True, exist_ok=True)
    vendor = runtime.VENDOR / "rvc" / "train"

    runtime.ensure(job, runtime.EMBEDDER + runtime.PREDICTORS.get(f0, []) + runtime.pretrained(sr), 0.0, 0.03)

    if not p.get("skip_preprocess"):
        list_path = Path(p["list"]) if p.get("list") else None
        src = stage_dataset(job, dataset, list_path, exp_dir)
        # 切片参数沿用 Applio 默认：自动切分、3 秒片段、0.3 秒重叠，不做额外音频效果与响度归一化。
        run_script(job, [str(vendor / "preprocess" / "preprocess.py"), str(exp_dir), str(src), str(sr), str(cores),
                         "Automatic", "False", "3.0", "0.3", "none"], 0.03, 0.08, "预处理")
    if not p.get("skip_extract"):
        # gpus 传 "0"：有 CUDA 用第 0 块卡；"-" 强制 CPU。macOS 上用 CPU：Applio 在 MPS 上提取 embedding 时
        # 会因 float64 张量失败，且只打印警告、退出码仍为 0（2026-10 的 Applio 5171e1d 实测）。训练阶段仍使用 MPS。
        gpus = "-" if sys.platform == "darwin" else "0"
        run_script(job, [str(vendor / "extract" / "extract.py"), str(exp_dir), f0, str(cores), gpus, str(sr),
                         "contentvec", "None", "2"], 0.08, 0.20, "提取音高与特征")
    check_extracted(exp_dir)

    pg, pd = runtime.pretrained_paths(sr)

    def epoch_progress(line: str) -> float | None:
        m = _EPOCH.search(line)
        return min(int(m.group(1)) / epochs, 1.0) if m else None

    # 参数顺序见 vendor/rvc/train/train.py 顶部的 sys.argv 解析：
    # 名称、每 N 轮保存、总轮数、G 底模、D 底模、GPU、batch、采样率、只保留最新、每次保存导出权重、
    # 数据缓存到 GPU、清除旧训练、声码器、梯度检查点。
    run_script(job, [str(vendor / "train.py"), name, str(save_every), str(epochs), pg, pd, "0", str(batch), str(sr),
                     "True", "True", "False", "False", "HiFi-GAN", "False"], 0.20, 0.97, "训练", epoch_progress)
    run_script(job, [str(vendor / "process" / "extract_index.py"), str(exp_dir), "Auto"], 0.97, 0.995, "生成索引")

    weights = latest_weights(exp_dir, name)
    if weights is None:
        raise RuntimeError(f"训练结束但没有找到导出的权重：{exp_dir}")
    job.artifact(weights, "rvc-model")
    index = exp_dir / f"{name}.index"
    if index.is_file():
        job.artifact(index, "rvc-index")
    job.progress(1.0, f"完成：{weights.name}")


def check_extracted(exp_dir: Path) -> None:
    """核对每个切片都有特征文件。Applio 的提取脚本在部分失败时只打印警告，不以非零退出码结束，
    这里提前给出明确错误，避免带着缺失的特征进入训练后才失败。"""
    sliced = {f.stem for f in (exp_dir / "sliced_audios").glob("*.wav")}
    extracted = {f.stem for f in (exp_dir / "extracted").glob("*.npy")}
    if not sliced:
        raise RuntimeError("预处理没有产生切片，请检查数据集音频")
    missing = sliced - extracted
    if missing:
        raise RuntimeError(f"特征提取不完整：{len(missing)}/{len(sliced)} 个切片缺少特征，详见引擎日志 timbre.log")


def latest_weights(exp_dir: Path, name: str) -> Path | None:
    """返回最新导出的推理权重（<名称>_<轮>e_<步>s.pth）；G_ / D_ 开头的是训练检查点，不是推理权重。"""
    pat = re.compile(re.escape(name) + r"_(\d+)e_(\d+)s\.pth$")
    best, key = None, (-1, -1)
    for f in exp_dir.glob(f"{name}_*e_*s.pth"):
        m = pat.search(f.name)
        if m and (int(m.group(1)), int(m.group(2))) > key:
            best, key = f, (int(m.group(1)), int(m.group(2)))
    return best
