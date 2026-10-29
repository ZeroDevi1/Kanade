"""Opt-in bandwidth extension and reproducible training inputs (no torch in worker)."""
from __future__ import annotations

import hashlib
import json
import math
import shutil
from dataclasses import asdict
from pathlib import Path

from kanade_engine import annotations

from . import bwe, runtime

MODES = ("off", "auto", "manual")
PIPELINE_VERSION = 1


def options(p: dict) -> dict:
    mode = p.get("bwe_mode") or "off"
    if mode not in MODES:
        raise ValueError("高频补全模式须为 off、auto 或 manual")
    cutoff = float(p.get("bwe_cutoff_hz") or 0)
    if not math.isfinite(cutoff) or (mode == "manual" and not 2000 <= cutoff <= 14999):
        raise ValueError("手动截止频率须在 2000–14999 Hz 之间")
    return {"mode": mode, "cutoff_hz": cutoff if mode == "manual" else 0.0}


def selected_files(dataset: Path, list_path: Path | None = None) -> list[Path]:
    if not dataset.is_dir():
        raise ValueError(f"数据集目录不存在：{dataset}")
    list_path = list_path or annotations.find_list(dataset)
    if list_path:
        found, missing = annotations.match(annotations.parse(list_path), dataset)
        if missing:
            raise ValueError(f"标注中有 {len(missing)} 条音频未找到，请先修正标注")
        files = sorted({src.resolve() for _, src in found})
    else:
        files = [f.resolve() for f in annotations.audio_files(dataset)]
    if not files:
        raise ValueError("没有可用的训练音频")
    return files


def make_plan(files: list[Path], out_dir: Path, opts: dict) -> list[bwe.Item]:
    items = bwe.plan(files, out_dir)
    if opts["mode"] == "manual":
        for item in items:
            # Do not turn an unreadable source into a claimed repaired sample.
            if item.bandwidth == 0:
                raise ValueError(f"无法读取音频带宽：{item.src}；请先转换为 WAV")
            item.bandwidth = opts["cutoff_hz"]
            item.model = bwe.choose_model(item.bandwidth)
            item.out = str(Path(item.out).with_suffix(".wav"))
    return items


def stage_selected(files: list[Path], out_dir: Path) -> Path:
    if any(out_dir.resolve() == f.parent.resolve() or out_dir.resolve() in f.parents for f in files):
        raise ValueError("不能把训练缓存目录用作原始数据集")
    if out_dir.is_symlink():
        raise ValueError("训练缓存不能是符号链接")
    shutil.rmtree(out_dir, ignore_errors=True)
    out_dir.mkdir(parents=True)
    used = set()
    for f in files:
        name = f.name
        n = 1
        while name.casefold() in used:
            name = f"{f.stem}_{n}{f.suffix}"
            n += 1
        used.add(name.casefold())
        shutil.copy2(f, out_dir / name)
    return out_dir


def source_sample_rate(files: list[Path]) -> int:
    import soundfile as sf
    rates = []
    for f in files:
        try:
            rates.append(sf.info(str(f)).samplerate)
        except Exception:
            continue
    mid = sorted(rates)[len(rates) // 2] if rates else 40000
    return 48000 if mid >= 48000 else 40000 if mid >= 40000 else 32000


def source_signature(files: list[Path]) -> list[dict]:
    return [{"path": str(f), "sha256": bwe.file_hash(f)} for f in files]


def fingerprint(files: list[Path], opts: dict, preprocessing: dict, items: list[bwe.Item]) -> tuple[str, dict]:
    models = {i.model for i in items if i.model}
    details = {"version": PIPELINE_VERSION, "sources": source_signature(files), "bwe": opts,
               "preprocessing": preprocessing,
               "bwe_algorithm": bwe.ALGORITHM_VERSION if opts["mode"] != "off" else None,
               "models": bwe.model_signatures(runtime.ROOT / "rvc/models/bwe", models)}
    return hashlib.sha256(json.dumps(details, sort_keys=True).encode()).hexdigest(), details


def guard_resume(exp_dir: Path, meta: dict, signature: str, skip_preprocess: bool, skip_extract: bool) -> None:
    old = meta.get("data_fingerprint")
    checkpoints = any(exp_dir.glob("G_*.pth")) or any(exp_dir.glob("D_*.pth")) or any(exp_dir.glob("*_e*.pth"))
    checkpoints = checkpoints or any(exp_dir.glob("*e_*s.pth"))
    if old and old != signature:
        raise ValueError("数据、补全模型或预处理设置已改变。请使用新的模型名，避免复用旧切片、特征和检查点")
    if not old and (checkpoints or skip_preprocess or skip_extract):
        raise ValueError("该实验没有可验证的数据指纹。请使用新模型名并重新预处理、提取特征")
    if (skip_preprocess or skip_extract) and not meta.get("data_ready", False):
        raise ValueError("上次数据准备未完成，请重新预处理和提取特征")
    if skip_extract and not skip_preprocess:
        raise ValueError("重新预处理时不能跳过特征提取；请同时重做这两个阶段")


def clear_prepared(exp_dir: Path, preprocess: bool) -> None:
    """Only disposable vendor caches, never source WAVs, BWE outputs or model checkpoints."""
    names = ["f0", "f0_voiced", "extracted"]
    if preprocess:
        names.append("sliced_audios")
    for name in names:
        path = exp_dir / name
        if path.is_symlink():
            raise ValueError(f"拒绝清理符号链接缓存：{path}")
        if path.exists():
            shutil.rmtree(path)
    for name in ("filelist.txt", "total_fea.npy"):
        (exp_dir / name).unlink(missing_ok=True)


def process(job, items: list[bwe.Item], plan_path: Path, run_script, lo: float, hi: float) -> None:
    plan_path.parent.mkdir(parents=True, exist_ok=True)
    plan_path.write_text(json.dumps([asdict(i) for i in items], ensure_ascii=False, indent=2), encoding="utf-8")
    def progress(line):
        if line.startswith("bwe "):
            parts = line.split()
            if len(parts) > 1 and "/" in parts[1]:
                a, b = parts[1].split("/")
                return int(a) / max(int(b), 1)
        return None
    run_script(job, ["-m", "kanade_timbre.bwe", str(plan_path), str(runtime.ROOT / "rvc/models/bwe")],
               lo, hi, "高频补全（生成估计，不是真实细节恢复）", on_line=progress)
    missing = [i.out for i in items if not Path(i.out).is_file()]
    if missing:
        raise RuntimeError(f"高频补全缺少 {len(missing)} 个输出，未进入训练")


def preview(job, run_script) -> None:
    opts = options(job.params)
    if opts["mode"] == "off":
        raise ValueError("请先选择自动或手动高频补全")
    count = int(job.params.get("preview_count", 3))
    if not 1 <= count <= 5:
        raise ValueError("试听数量须在 1–5 之间")
    files = selected_files(Path(job.params.get("dataset_dir") or ""),
                           Path(job.params["list"]) if job.params.get("list") else None)
    job.progress(0.05, "检测数据带宽；试听不会训练模型")
    out = job.output_dir
    out.mkdir(parents=True, exist_ok=True)
    items = make_plan(files, out / "enhanced", opts)
    candidates = [i for i in items if i.model]
    # Evenly spaced candidates rather than all adjacent slices of one utterance.
    chosen = [candidates[round(i * (len(candidates) - 1) / max(min(count, len(candidates)) - 1, 1))]
              for i in range(min(count, len(candidates)))]
    if chosen:
        runtime.ensure(job, bwe.assets({i.model for i in chosen}), 0.1, 0.3)
        process(job, chosen, out / "preview-plan.json", run_script, 0.3, 0.9)
        import soundfile as sf
        for n, item in enumerate(chosen, 1):
            job.check_cancelled()
            original = out / f"original-{n:02d}.wav"
            x, sr = sf.read(item.src, dtype="float32")
            sf.write(original, x, sr, subtype="FLOAT")
            enhanced = out / f"enhanced-{n:02d}.wav"
            shutil.copy2(item.out, enhanced)
            job.artifact(original, "audio")
            job.artifact(enhanced, "audio")
    report = out / "bandwidth-report.json"
    report.write_text(json.dumps({"options": opts, "summary": bwe.summary(items),
                                 "items": [asdict(i) for i in items], "previewed": [i.src for i in chosen],
                                 "warning": "生成高频不是原说话人的真实高频；请等响度试听，勿仅凭频谱判断"},
                                ensure_ascii=False, indent=2), encoding="utf-8")
    job.artifact(report, "file")
    job.progress(1.0, bwe.summary(items) + f"；已生成 {len(chosen)} 组试听，未训练")
