"""CosyVoice 推理任务处理函数。

worker 同一时间只持有一个已加载的模型（显存约 2–4 GB）；切换模型时先释放旧模型。
推理以非流式方式逐句执行：文本先经上游 text_normalize 切分成句，每句完成后上报进度并检查取消，
最后把各句音频拼接为一个 16-bit WAV。
"""

from __future__ import annotations

import gc
import json
import logging
from pathlib import Path
from typing import Any, Callable, Iterable

from kanade_engine import Job

from . import download as dl

# 各模型版本支持的模式，与 Go 端 internal/cantus/models.go 保持一致。
# 依据上游 example.py 中各版本演示的接口：v1 基础模型无指令能力，SFT / Instruct 是单独的模型。
MODES = {
    "v1": {"zero_shot", "cross_lingual", "vc"},
    "v1-sft": {"sft"},
    "v1-instruct": {"instruct"},
    "v2": {"zero_shot", "cross_lingual", "instruct"},
    "v3": {"zero_shot", "cross_lingual", "instruct"},
}

# CosyVoice 3 的提示文本须以系统提示开头，见上游 example.py 的 cosyvoice3_example。
V3_SYSTEM = "You are a helpful assistant."
END = "<|endofprompt|>"

# 参考音频约束来自上游 frontend：speech tokenizer 只接受不超过 30 秒、采样率不低于 16 kHz 的音频。
MAX_PROMPT_SECONDS = 30
MIN_SAMPLE_RATE = 16000

log = logging.getLogger("kanade_cosyvoice")


def wetext_dir(model_dir: Path) -> Path:
    """wetext 资源目录：与各模型目录同级的 _wetext，所有模型共用。"""
    return model_dir.parent / "_wetext"

_loaded: tuple[str, Any] | None = None  # (model_dir, 模型实例)
_registered: set[str] = set()  # 已注册到当前模型 spk2info 的音色键


# ---------- 模型 ----------

def family(model_dir: Path) -> str:
    """按 yaml 与权重判断模型版本，返回 MODES 的键。"""
    if (model_dir / "cosyvoice3.yaml").exists():
        return "v3"
    if (model_dir / "cosyvoice2.yaml").exists():
        return "v2"
    if (model_dir / "cosyvoice.yaml").exists():
        name = model_dir.name.lower()
        if name.endswith("-sft"):
            return "v1-sft"
        if name.endswith("-instruct"):
            return "v1-instruct"
        return "v1"
    raise ValueError(f"{model_dir} 不是 CosyVoice 模型目录")


def load(job: Job, model_dir: Path, mode: str):
    """返回已加载的模型；模型未下载完成或不支持 mode 时抛出 ValueError。"""
    global _loaded
    if not dl.is_complete(model_dir):
        raise ValueError(f"模型尚未下载完成：{model_dir.name}")
    fam = family(model_dir)
    if mode not in MODES[fam]:
        raise ValueError(f"{model_dir.name} 不支持该模式（{mode}）")
    if _loaded and _loaded[0] == str(model_dir):
        return _loaded[1]

    import torch
    from cosyvoice.cli.cosyvoice import AutoModel

    if _loaded:
        _loaded = None
        _registered.clear()
        gc.collect()
        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    # 上游前端经 modelscope.snapshot_download 获取 wetext 资源，失败时静默关闭文本归一化；
    # 这里先用自己的下载器确保资源在本地，再让 wetext 直接使用本地目录。
    # 资源下载失败（如离线）时不中断合成：保留上游行为，由下方检查给出明确警告。
    wdir = wetext_dir(model_dir)
    try:
        dl.ensure_wetext(job, wdir)
    except Exception as exc:  # noqa: BLE001
        log.warning("下载 wetext 资源失败：%s", exc)
    if dl.is_complete(wdir):
        import wetext.wetext

        wetext.wetext.snapshot_download = lambda *args, **kwargs: str(wdir)

    device = "CUDA " + torch.cuda.get_device_name(0) if torch.cuda.is_available() else "CPU"
    job.progress(0.02, f"加载模型 {model_dir.name}（{device}）")
    # fp16 只在 CUDA 上生效，上游在无 CUDA 时会自动关闭。
    model = AutoModel(model_dir=str(model_dir), fp16=torch.cuda.is_available())
    if model.frontend.text_frontend == "":
        log.warning("文本归一化不可用：wetext 加载失败，数字与符号将按原样朗读")
        job.progress(0.03, "警告：文本归一化不可用，数字与符号将按原样朗读")
    _loaded = (str(model_dir), model)
    return model


# ---------- 输入 ----------

def check_audio(path: str, what: str) -> str:
    """校验参考 / 源音频可读、时长与采样率满足要求，返回原路径。"""
    import soundfile as sf

    p = Path(path)
    if not p.is_file():
        raise ValueError(f"{what}不存在：{path}")
    try:
        info = sf.info(str(p))
    except Exception as exc:
        raise ValueError(f"无法读取{what}（支持 WAV / FLAC / OGG / MP3）：{p.name}") from exc
    if info.samplerate < MIN_SAMPLE_RATE:
        raise ValueError(f"{what}采样率 {info.samplerate} Hz 低于 {MIN_SAMPLE_RATE} Hz")
    if what == "参考音频" and info.duration > MAX_PROMPT_SECONDS:
        raise ValueError(f"参考音频 {info.duration:.1f} 秒，超过 {MAX_PROMPT_SECONDS} 秒上限，请裁剪后再用")
    return str(p)


def prompt_text_for(fam: str, text: str) -> str:
    """CosyVoice 3 的零样本提示文本需带系统提示前缀；其他版本原样返回。"""
    if fam == "v3" and END not in text:
        return f"{V3_SYSTEM}{END}{text}"
    return text


def instruct_text_for(fam: str, instruct: str) -> str:
    """把用户的自然语言指令包装为各版本要求的格式。已带 <|endofprompt|> 的视为完整指令，原样使用。"""
    instruct = instruct.strip()
    if END in instruct:
        return instruct
    if fam == "v3":
        return f"{V3_SYSTEM} {instruct}{END}"
    return f"{instruct}{END}"


def speaker(job: Job, model, fam: str, params: dict) -> tuple[str, str, str]:
    """解析参考音色，返回 (prompt_wav, prompt_text, zero_shot_spk_id)。

    提供 speaker_dir（已保存的音色）时，首次使用会把其特征注册进模型的 spk2info，之后复用，省去每次重新提取；
    注册键包含 speaker.json 的修改时间，音色更新后自动重新注册。否则使用 prompt_wav / prompt_text。
    """
    sdir = params.get("speaker_dir")
    if sdir:
        meta_path = Path(sdir) / "speaker.json"
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        wav = check_audio(str(Path(sdir) / meta["audio"]), "参考音频")
        text = prompt_text_for(fam, meta.get("prompt_text", ""))
        key = f"kanade:{Path(sdir).name}:{meta_path.stat().st_mtime_ns}"
        if key not in _registered:
            job.progress(0.05, f"注册音色 {meta.get('name', Path(sdir).name)}")
            model.add_zero_shot_spk(text, wav, key)
            _registered.add(key)
        return wav, text, key
    wav = check_audio(params.get("prompt_wav") or "", "参考音频")
    return wav, prompt_text_for(fam, params.get("prompt_text") or ""), ""


# ---------- 推理与输出 ----------

def synthesize(job: Job, model, text: str, text_frontend: bool, run: Callable[[], Iterable[dict]]) -> None:
    """执行 run 返回的推理生成器，逐句上报进度，拼接后写出 WAV。

    句数由上游 text_normalize 以同样参数预先计算；非流式推理对每句恰好产出一段音频。
    """
    if not text.strip():
        raise ValueError("文本为空")
    segments = model.frontend.text_normalize(text, split=True, text_frontend=text_frontend)
    total = max(len(segments), 1)
    job.progress(0.1, f"共 {total} 句")
    chunks = []
    for i, out in enumerate(run()):
        chunks.append(out["tts_speech"])
        job.progress(0.1 + 0.88 * min(i + 1, total) / total, f"已合成 {min(i + 1, total)}/{total} 句")
    write_output(job, model, chunks)


def write_output(job: Job, model, chunks: list) -> None:
    import soundfile as sf
    import torch

    if not chunks:
        raise RuntimeError("模型没有输出音频")
    audio = torch.cat(chunks, dim=1).squeeze(0).cpu().float().numpy()
    out = job.output_dir / f"{job.id}.wav"
    sf.write(str(out), audio, model.sample_rate, subtype="PCM_16")
    job.artifact(out, "audio")


def _common(job: Job, mode: str):
    p = job.params
    model_dir = Path(p["model_dir"])
    model = load(job, model_dir, mode)
    return p, model, family(model_dir), float(p.get("speed") or 1.0), bool(p.get("text_frontend", True))


# ---------- 任务 ----------

def download_model(job: Job) -> None:
    p = job.params
    model_dir = Path(p["model_dir"])
    dl.download(job, p.get("source") or "modelscope", p["repo"], model_dir, wetext_dir(model_dir))


def zero_shot(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "zero_shot")
    wav, prompt_text, spk = speaker(job, model, fam, p)
    if not spk and not p.get("prompt_text"):
        raise ValueError("零样本克隆需要填写参考音频对应的文本")
    text = p["text"]
    synthesize(job, model, text, tf, lambda: model.inference_zero_shot(
        text, prompt_text, wav, zero_shot_spk_id=spk, stream=False, speed=speed, text_frontend=tf))


def cross_lingual(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "cross_lingual")
    wav, _, spk = speaker(job, model, fam, p)
    text = p["text"]
    if fam == "v3" and END not in text:
        text = f"{V3_SYSTEM}{END}{text}"
    synthesize(job, model, text, tf, lambda: model.inference_cross_lingual(
        text, wav, zero_shot_spk_id=spk, stream=False, speed=speed, text_frontend=tf))


def sft(job: Job) -> None:
    p, model, _, speed, tf = _common(job, "sft")
    spk_id = p.get("spk_id") or ""
    if spk_id not in model.list_available_spks():
        raise ValueError(f"预训练音色不存在：{spk_id}，可用：{'、'.join(model.list_available_spks())}")
    text = p["text"]
    synthesize(job, model, text, tf, lambda: model.inference_sft(text, spk_id, stream=False, speed=speed, text_frontend=tf))


def instruct(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "instruct")
    instr = instruct_text_for(fam, p.get("instruct") or "")
    if instr == END or not instr.strip():
        raise ValueError("请填写指令文本")
    text = p["text"]
    if fam == "v1-instruct":
        spk_id = p.get("spk_id") or ""
        if spk_id not in model.list_available_spks():
            raise ValueError(f"预训练音色不存在：{spk_id}")
        synthesize(job, model, text, tf, lambda: model.inference_instruct(
            text, spk_id, instr, stream=False, speed=speed, text_frontend=tf))
        return
    wav, _, spk = speaker(job, model, fam, p)
    synthesize(job, model, text, tf, lambda: model.inference_instruct2(
        text, instr, wav, zero_shot_spk_id=spk, stream=False, speed=speed, text_frontend=tf))


def vc(job: Job) -> None:
    p, model, fam, speed, _ = _common(job, "vc")
    source = check_audio(p.get("source_wav") or "", "源音频")
    wav, _, _ = speaker(job, model, fam, p)
    job.progress(0.1, "转换中")
    chunks = [out["tts_speech"] for out in model.inference_vc(source, wav, stream=False, speed=speed)]
    job.progress(0.98, "写出音频")
    write_output(job, model, chunks)
