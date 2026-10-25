"""RVC 训练：预处理 → 提取 f0 与特征 → 训练（自动续训）→ 建索引，各阶段以子进程运行 Applio 的脚本。

与 VoiceLab rvc_train.py 的行为对应：
- 数据集目录中有同名 .list（或显式给出 list）时，只用其中列出的音频（白名单）；做法是在实验目录下建
  _dataset_input/，硬链接（不行则复制）选中的音频，再交给预处理。
- 实验目录已有 G_*.pth / D_*.pth 时，Applio 训练脚本会自动续训，总轮数可以比上次大。
- skip_preprocess / skip_extract 用于续训时跳过已完成的阶段。

- 智能模式：sample_rate / epochs / batch_size / save_every 传 0 表示自动（见 auto_* 函数），选定的值写入
  实验目录的 kanade-train.json，续训时沿用其中的采样率。

进度分配：预处理 0–8%，提取 8–20%，训练 20–97%，索引 97–100%。训练进度从脚本每轮输出的
「<名称> | epoch=N | step=…」行解析。

训练速度（8 GB 显卡、经 Oculink 等 PCIe 4.0 ×4 外接时尤其明显）：
- worker 由 Conductor 为每次训练新启动（engine.json 的 fresh_tasks），且本进程不调用 torch.cuda（显存经 nvidia-smi
  查询，见 gpu.py），训练子进程独占显存；否则推理时缓存的模型与 CUDA 上下文会与训练争用显存，超出后驱动回退到
  共享内存，速度降到几分之一。
- 数据缓存到显存（cache_gpu）默认按数据量自动决定：缓存后每步不再经 PCIe 拷贝批数据、也不经 DataLoader 的工作进程
  （Windows 上以 spawn 启动，进程间传递张量开销大）。
- 精度写入本次训练专用的 Applio 设置文件（APPLIO_CONFIG_FILE），不读用户另装的 Applio 的设置
  （那里若设成 fp32，训练会慢一倍以上）。默认 fp16，与此前实测一致。
- 训练中每 15 秒查询一次显卡：进度中显示 PCIe 链路、显存与每轮耗时，显存将满或链路降级时提示。
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import threading
from pathlib import Path

from kanade_engine import Cancelled, Job, annotations

from . import gpu, runtime

SAMPLE_RATES = (32000, 40000, 48000)
CUT_MODES = ("Automatic", "Simple", "Skip")
NORMALIZATION = ("none", "pre", "post")
INDEX_ALGORITHMS = ("Auto", "Faiss", "KMeans")
TRAIN_META = "kanade-train.json"
PRECISIONS = ("fp16", "bf16", "fp32")
# APPLIO_SETTINGS 是实验目录中本次训练专用的 Applio 设置文件（只写 precision，其余沿用 config_template.json）。
APPLIO_SETTINGS = "kanade-applio.json"
_EPOCH = re.compile(r"\| epoch=(\d+) \| step=(\d+)")
_SPEED = re.compile(r"training_speed=(\d+):(\d+):(\d+)")
# 显存余量低于此值（MB）时视为将满：驱动开始把显存分配回退到共享内存（Windows 默认的 Sysmem Fallback Policy）。
VRAM_FULL_MARGIN_MB = 300
# 自动缓存的数据量上限：显存的 10%（8 GB 约 0.8 GB，48 kHz 下约 12 分钟切片）。batch 4 的训练本身已用去 8 GB 卡的
# 大部分显存，缓存过多反而触发共享内存回退；余量由训练中的显存监视核对。
CACHE_GPU_FRACTION = 0.10
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


def run_script(job: Job, args: list[str], lo: float, hi: float, label: str, on_line=None, env: dict | None = None,
               status=None) -> None:
    """以子进程运行 vendor 中的脚本，逐行转发输出到日志，并在取消时结束整个进程树。

    on_line(line) 可返回 [0, 1] 的阶段内进度；返回 None 表示该行不含进度。env 是追加的环境变量。
    status() 返回附在进度说明后的状态文字（例如显卡监视的结果），每次更新进度时调用。
    """
    cmd = [sys.executable, *args]
    job.progress(lo, label)
    proc_env = runtime.subprocess_env()
    proc_env.update(env or {})
    proc = subprocess.Popen(cmd, cwd=runtime.ROOT, env=proc_env, stdout=subprocess.PIPE,
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
                extra = status() if status else ""
                job.progress(lo + (hi - lo) * frac, label + (f"（{extra}）" if extra else "") + "：" + _status_text(line))
    code = proc.wait()
    if canceled.is_set():
        raise Cancelled()
    if code != 0:
        raise RuntimeError(f"{label}失败（退出码 {code}）：" + " / ".join(tail[-3:]))


def _status_text(line: str) -> str:
    """去掉同一输出行里 tqdm 进度条的残留，返回适合界面显示的部分（最多 120 字符）。

    tqdm 以 \\r 刷新且不换行，训练脚本的「<名称> | epoch=N | …」紧跟在进度条的「…s/it]」之后输出，
    按行读取时两者在同一行。
    """
    text = line.split("\r")[-1]
    i = text.find(" | epoch=")
    if i > 0:
        text = text[text.rfind("]", 0, i) + 1:]
    return text.strip()[-120:]


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


def _flag(p: dict, key: str, default: bool) -> str:
    return "True" if p.get(key, default) else "False"


def run_train(job: Job) -> None:
    p = job.params
    name = (p.get("name") or "").strip()
    if not _NAME.match(name):
        raise ValueError("模型名只能包含字母、数字、中文、下划线与连字符")
    dataset = Path(p.get("dataset_dir") or "")
    if not dataset.is_dir() and not p.get("skip_preprocess"):
        raise ValueError(f"数据集目录不存在：{dataset}")
    exp_dir = runtime.LOGS / name
    exp_dir.mkdir(parents=True, exist_ok=True)
    meta_path = exp_dir / TRAIN_META
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.is_file() else {}
    f0 = p.get("f0_method") or "rmvpe"
    cores = int(p.get("cpu_cores") or min(os.cpu_count() or 4, 8))
    cut = p.get("cut_mode") or "Automatic"
    norm = p.get("normalization") or "none"
    index_algo = p.get("index_algorithm") or "Auto"
    if cut not in CUT_MODES or norm not in NORMALIZATION or index_algo not in INDEX_ALGORITHMS:
        raise ValueError("切片方式、响度归一化或索引算法取值无效")
    precision = p.get("precision") or "fp16"
    if precision not in PRECISIONS:
        raise ValueError(f"训练精度须为 {PRECISIONS} 之一")
    cache_mode = p.get("cache_gpu", "auto")
    if cache_mode not in ("auto", True, False):
        raise ValueError("数据缓存到显存须为 auto、true 或 false")
    chunk = float(p.get("chunk_len") or 3.0)
    overlap = float(p.get("overlap_len") if p.get("overlap_len") is not None else 0.3)
    vendor = runtime.VENDOR / "rvc" / "train"

    # 数据集先整理（白名单），自动参数要按实际使用的音频估计。
    src = dataset
    if not p.get("skip_preprocess"):
        list_path = Path(p["list"]) if p.get("list") else None
        src = stage_dataset(job, dataset, list_path, exp_dir)

    sr = int(p.get("sample_rate") or 0) or int(meta.get("sample_rate") or 0) or (auto_sample_rate(src) if src.is_dir() else 40000)
    if sr not in SAMPLE_RATES:
        raise ValueError(f"采样率须为 {SAMPLE_RATES} 之一")
    if meta.get("sample_rate") and int(meta["sample_rate"]) != sr:
        raise ValueError(f"该模型此前以 {meta['sample_rate']} Hz 训练，续训须使用相同采样率；换采样率请换一个模型名")
    trained = latest_epoch(exp_dir, name)
    epochs = int(p.get("epochs") or 0)
    if not epochs:
        epochs = auto_epochs(dataset_minutes(src) if src.is_dir() else float(meta.get("minutes") or 0), trained)
    batch = int(p.get("batch_size") or 0) or auto_batch()
    save_every = int(p.get("save_every") or 0) or max(5, min(epochs // 10, 20))
    if trained and epochs <= trained:
        raise ValueError(f"已训练 {trained} 轮，续训的总轮数须大于 {trained}")
    meta.update({"sample_rate": sr, "epochs": epochs, "batch_size": batch, "save_every": save_every, "f0_method": f0})
    if src.is_dir():
        meta["minutes"] = round(dataset_minutes(src), 2)
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    resume = f"，从第 {trained} 轮续训" if trained else ""
    job.progress(0.0, f"采样率 {sr} Hz · {epochs} 轮 · batch {batch} · 每 {save_every} 轮保存{resume}")

    runtime.ensure(job, runtime.EMBEDDER + runtime.PREDICTORS.get(f0, []) + runtime.pretrained(sr), 0.0, 0.03)

    if not p.get("skip_preprocess"):
        # 切片默认沿用 Applio：自动切分、3 秒片段、0.3 秒重叠，不做额外音频效果与响度归一化。
        run_script(job, [str(vendor / "preprocess" / "preprocess.py"), str(exp_dir), str(src), str(sr), str(cores),
                         cut, _flag(p, "process_effects", False), str(chunk), str(overlap), norm], 0.03, 0.08, "预处理")
    if not p.get("skip_extract"):
        # gpus 传 "0"：有 CUDA 用第 0 块卡；"-" 强制 CPU。macOS 上用 CPU：Applio 在 MPS 上提取 embedding 时
        # 会因 float64 张量失败，且只打印警告、退出码仍为 0（2026-10 的 Applio 5171e1d 实测）。训练阶段仍使用 MPS。
        gpus = "-" if sys.platform == "darwin" else "0"
        run_script(job, [str(vendor / "extract" / "extract.py"), str(exp_dir), f0, str(cores), gpus, str(sr),
                         "contentvec", "None", "2"], 0.08, 0.20, "提取音高与特征")
    check_extracted(exp_dir)

    pg, pd = runtime.pretrained_paths(sr)
    status0 = gpu.query()
    cache_gpu = cache_mode is True or (cache_mode == "auto" and auto_cache_gpu(exp_dir, sr, status0))
    settings = exp_dir / APPLIO_SETTINGS
    settings.write_text(json.dumps({"precision": precision}), encoding="utf-8")
    meta.update({"precision": precision, "cache_gpu": cache_gpu})
    if status0:
        meta["gpu"] = status0.name
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    job.progress(0.20, f"精度 {precision} · 数据{'已' if cache_gpu else '不'}缓存到显存")

    monitor = TrainMonitor()
    epoch_secs: list[int] = []

    def epoch_progress(line: str) -> float | None:
        if sp := _SPEED.search(line):
            epoch_secs.append(int(sp.group(1)) * 3600 + int(sp.group(2)) * 60 + int(sp.group(3)))
            monitor.epoch_seconds = epoch_secs[-1]
        m = _EPOCH.search(line)
        return min(int(m.group(1)) / epochs, 1.0) if m else None

    # 参数顺序见 vendor/rvc/train/train.py 顶部的 sys.argv 解析：
    # 名称、每 N 轮保存、总轮数、G 底模、D 底模、GPU、batch、采样率、只保留最新、每次保存导出权重、
    # 数据缓存到 GPU、清除旧训练、声码器、梯度检查点。
    run_script(job, [str(vendor / "train.py"), name, str(save_every), str(epochs), pg, pd, "0", str(batch), str(sr),
                     "True", "True", str(cache_gpu), "False", "HiFi-GAN", _flag(p, "checkpointing", False)],
               0.20, 0.97, "训练", epoch_progress, env={"APPLIO_CONFIG_FILE": str(settings)}, status=monitor.start())
    monitor.stop()
    # 记录本次的速度与链路，便于比较设置（例如换线缆、开关缓存）前后的变化。
    if epoch_secs:
        meta["epoch_seconds"] = sorted(epoch_secs)[len(epoch_secs) // 2]
    if monitor.peak:
        meta["pcie"], meta["vram_peak_mb"] = monitor.peak.link(), monitor.peak_used
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")
    # 经 faiss_compat 启动：模型名含中文时 faiss 原生的 write_index 无法写入（见 faiss_compat 说明）。
    run_script(job, ["-m", "kanade_timbre.faiss_compat", str(vendor / "process" / "extract_index.py"), str(exp_dir), index_algo],
               0.97, 0.995, "生成索引")

    weights = latest_weights(exp_dir, name)
    if weights is None:
        raise RuntimeError(f"训练结束但没有找到导出的权重：{exp_dir}")
    job.artifact(weights, "rvc-model")
    index = exp_dir / f"{name}.index"
    if index.is_file():
        job.artifact(index, "rvc-index")
    job.progress(1.0, f"完成：{weights.name}")


# ---------- 智能模式的自动参数 ----------

def _durations(src: Path) -> list[tuple[float, int]]:
    """数据集中各音频的 (时长秒, 采样率)；soundfile 读不了的格式（如 M4A）跳过。"""
    import soundfile as sf

    out = []
    for f in annotations.audio_files(src):
        try:
            info = sf.info(str(f))
        except Exception:  # noqa: BLE001
            continue
        out.append((info.duration, info.samplerate))
    return out


def dataset_minutes(src: Path) -> float:
    return sum(d for d, _ in _durations(src)) / 60


def auto_sample_rate(src: Path) -> int:
    """按数据集的采样率中位数选底模：≥ 48 kHz 用 48k；44.1 kHz 等用 40k（不上采样虚增高频）；更低用 32k。"""
    rates = sorted(r for _, r in _durations(src))
    if not rates:
        return 40000
    mid = rates[len(rates) // 2]
    return 48000 if mid >= 48000 else 40000 if mid >= 40000 else 32000


def auto_epochs(minutes: float, trained: int) -> int:
    """按数据时长取总轮数：数据越少每轮步数越少，需要更多轮；经验值来自 Applio / AI Hub 的训练建议。

    已训练到 trained 轮时，在其基础上再训目标轮数的四分之一（至少 20 轮）。
    """
    target = 300 if minutes < 5 else 200 if minutes < 15 else 150 if minutes < 30 else 100
    if trained >= target:
        return trained + max(target // 4, 20)
    return target


def auto_batch() -> int:
    """按显存选 batch：8 GB 显卡用 4（2026-10-20 RTX 5060 实测），6 GB 以下用 2；MPS 用 4，CPU 用 2。

    显存经 nvidia-smi 查询（见 gpu.py：不在 worker 中创建 CUDA 上下文）。
    """
    import torch

    if (st := gpu.query()) is not None:
        gb = st.total_mb / 1024
        return 2 if gb < 7 else 4 if gb < 11 else 8 if gb < 20 else 12
    if torch.cuda.is_available():
        gb = torch.cuda.get_device_properties(0).total_memory / 2**30
        return 2 if gb < 7 else 4 if gb < 11 else 8 if gb < 20 else 12
    if getattr(torch.backends, "mps", None) and torch.backends.mps.is_available():
        return 4
    return 2


def latest_epoch(exp_dir: Path, name: str) -> int:
    """已导出权重中的最大轮数；没有时为 0。"""
    w = latest_weights(exp_dir, name)
    m = re.search(r"_(\d+)e_\d+s\.pth$", w.name) if w else None
    return int(m.group(1)) if m else 0


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


# ---------- 显存缓存与显卡监视 ----------

def cache_bytes(exp_dir: Path, sr: int) -> int:
    """估计把训练数据缓存到显存所需的字节数：每秒切片约为波形（float32）、线性频谱（n_fft/2+1 个频点 × 100 帧）
    与 768 维内容特征（100 帧）之和，再加两成按批补齐的余量。三种采样率的帧率都是 100 帧/秒
    （hop 分别为 480 / 400 / 320）。"""
    bins = 513 if sr == 32000 else 1025
    per_sec = 4 * (sr + bins * 100 + 768 * 100 + 2 * 100)
    seconds = sum(d for d, _ in _durations(exp_dir / "sliced_audios"))
    return int(seconds * per_sec * 1.2)


def auto_cache_gpu(exp_dir: Path, sr: int, st: "gpu.Status | None") -> bool:
    """数据量不超过显存的 CACHE_GPU_FRACTION 时缓存到显存；没有 NVIDIA 显卡（MPS / CPU）时不缓存。"""
    if st is None:
        return False
    return cache_bytes(exp_dir, sr) <= st.total_mb * 2**20 * CACHE_GPU_FRACTION


class TrainMonitor:
    """训练期间每 15 秒查询一次显卡，给出附在进度后的状态：PCIe 链路、显存、每轮耗时，以及显存将满或链路降级的提示。
    start() 返回供 run_script 调用的 status 函数；stop() 结束查询线程。peak 是显存占用最高时的状态。"""

    INTERVAL = 15.0

    def __init__(self) -> None:
        self.latest: gpu.Status | None = None
        self.peak: gpu.Status | None = None
        self.peak_used = 0
        self.epoch_seconds = 0
        self._stop = threading.Event()

    def start(self):
        threading.Thread(target=self._run, daemon=True).start()
        return self.status

    def stop(self) -> None:
        self._stop.set()

    def _run(self) -> None:
        while not self._stop.is_set():
            st = gpu.query()
            if st is not None:
                self.latest = st
                if st.used_mb >= self.peak_used:
                    self.peak, self.peak_used = st, st.used_mb
            self._stop.wait(self.INTERVAL)

    def status(self) -> str:
        parts = []
        if (st := self.latest) is not None:
            parts += [st.link(), st.memory()]
        if self.epoch_seconds:
            parts.append(f"每轮 {self.epoch_seconds} 秒")
        if st is not None:
            if st.total_mb - st.used_mb < VRAM_FULL_MARGIN_MB:
                parts.append("显存将满，可能已回退到共享内存（外接显卡上会明显变慢）：可降低 batch、关闭数据缓存或开启梯度检查点")
            # 链路只在负载下有意义（空闲时降速）；Oculink 正常为 4.0 ×4，低于 3.0 或 ×4 多半是线缆或 BIOS 设置问题。
            if st.util >= 50 and (st.gen < 3 or st.width < 4):
                parts.append(f"PCIe 链路只有 {st.link()}：检查 Oculink 线缆与 BIOS 中该接口的 PCIe 代数设置")
        return " · ".join(parts)
