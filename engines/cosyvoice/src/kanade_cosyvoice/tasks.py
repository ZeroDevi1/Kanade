"""CosyVoice 推理任务处理函数。

worker 同一时间只持有一个已加载的模型（显存约 2–4 GB）；切换模型时先释放旧模型。
推理以非流式方式逐段执行：分段、逐段检查与重试、按标点停顿拼接、保存各段音频见 kanade_engine.longtext。各模式只提供
「一段已归一化的文本 → 上游推理」的函数（模式所需的前缀按段加上）。取消在句子内部也能生效：LLM 逐 token 采样时
检查取消并返回结束 token（见 configure_sampling），当前句随即结束，已合成的部分作为部分结果输出。

带 regenerate {manifest, index} 时只重新合成此前结果（segments.json）中的一段并重新拼接，其余参数须与当时相同。
"""

from __future__ import annotations

import gc
import json
import logging
from pathlib import Path
from typing import Any, Callable, Iterable

from kanade_engine import Cancelled, Job, longtext

from . import download as dl

# 各模型版本支持的模式，与 Go 端 internal/cantus/models.go 保持一致。
# 依据上游 example.py 中各版本演示的接口：v1 基础模型无指令能力，SFT / Instruct 是单独的模型。
MODES = {
    "v1": {"zero_shot", "cross_lingual", "vc"},
    "v1-sft": {"sft"},
    "v1-instruct": {"instruct"},
    "v2": {"zero_shot", "cross_lingual", "instruct"},
    "v3": {"zero_shot", "cross_lingual", "instruct"},
    # Kanade 训练的 CosyVoice 3 说话人 SFT 模型（目录中有 kanade-sft.json），见 sft.py。
    "v3-sft": {"sft"},
}

# SFT 推理的保守采样参数，取自 VoiceLab cosyvoice_sft_param_playbook（sampling06k10）：降低韵律随机性与崩坏概率。
SFT_SAMPLING = {"top_p": 0.6, "top_k": 10, "win_size": 10, "tau_r": 1.0}
SFT_SEED = 1986
# 任务参数 sampling 可覆盖的 ras_sampling 参数；未给出的项用模型 yaml 的默认（上游各版本均为 0.8 / 25 / 10 / 0.1）。
SAMPLING_KEYS = {"top_p": float, "top_k": int, "win_size": int, "tau_r": float}

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
        return "v3-sft" if (model_dir / "kanade-sft.json").exists() else "v3"
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

def synthesize(job: Job, model, text: str, text_frontend: bool, generate: Callable[[str], Iterable[dict]]) -> None:
    """长文本合成（kanade_engine.longtext）：generate(段落文本) 返回该段的上游推理生成器。带 regenerate 参数时只重做一段。

    可归一化时用上游 text_normalize 切句（含数字、符号读法）；关闭归一化或含 <|…|> 标记时按标点切句，保留标记。
    """
    import torch

    def split(line: str) -> list[str]:
        if text_frontend and not ("<|" in line and "|>" in line):
            return model.frontend.text_normalize(line, split=True, text_frontend=True)
        return longtext.split_punct(line)

    def wave(seg: str):
        chunks = [out["tts_speech"] for out in generate(seg)]
        return torch.cat(chunks, dim=1).squeeze(0).cpu().float().numpy() if chunks else None

    longtext.run(job, text, model.sample_rate, wave, split)


def write_output(job: Job, model, chunks: list) -> None:
    import soundfile as sf
    import torch

    if not chunks:
        raise RuntimeError("模型没有输出音频")
    audio = torch.cat(chunks, dim=1).squeeze(0).cpu().float().numpy()
    out = job.output_dir / f"{job.id}.wav"
    sf.write(str(out), audio, model.sample_rate, subtype="PCM_16")
    job.artifact(out, "audio")


def configure_sampling(job: Job, model, fam: str, p: dict) -> None:
    """按任务参数设置 LLM 采样与随机种子，每个任务都重新设置（同一模型实例跨任务复用）。

    - sampling {top_p, top_k, win_size, tau_r}：覆盖 ras_sampling 参数；缺省时微调模型用 SFT_SAMPLING，其他模型恢复 yaml 默认。
    - seed：非负整数时固定 random / numpy / torch 的种子，便于复现与 A/B 对比；缺省时微调模型用 SFT_SEED，其他模型不固定。
    - 取消：每个 token 采样前检查 job.cancelled，已取消时返回结束 token，让上游的生成循环立即结束
      （各版本的循环在采样结果等于 eos_token 或属于 stop_token_ids 时退出，eos_token 都在其中）。
    """
    import random

    import numpy as np
    import torch
    from cosyvoice.utils.common import ras_sampling

    llm = model.model.llm
    if not hasattr(llm, "_kanade_default_sampling"):
        llm._kanade_default_sampling = llm.sampling
    sampling = p.get("sampling")
    if sampling is None and fam == "v3-sft":
        sampling = SFT_SAMPLING
    if sampling:
        kw = {k: conv(sampling[k]) for k, conv in SAMPLING_KEYS.items() if sampling.get(k) is not None}
        base = lambda scores, decoded, s: ras_sampling(scores, decoded, s, **kw)  # noqa: E731
    else:
        base = llm._kanade_default_sampling

    def guarded(scores, decoded, s):
        if job.cancelled:
            return llm.eos_token
        return base(scores, decoded, s)

    llm.sampling = guarded
    seed = p.get("seed")
    if seed is None and fam == "v3-sft":
        seed = SFT_SEED
    if seed is not None and int(seed) >= 0:
        random.seed(int(seed))
        np.random.seed(int(seed))
        torch.manual_seed(int(seed))


def _common(job: Job, mode: str):
    p = job.params
    model_dir = Path(p["model_dir"])
    model = load(job, model_dir, mode)
    fam = family(model_dir)
    if mode != "vc":  # 声音转换不经过 LLM
        configure_sampling(job, model, fam, p)
    return p, model, fam, float(p.get("speed") or 1.0), bool(p.get("text_frontend", True))


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
    # 各段以 text_frontend=False 合成（已归一化），参考文本在这里先归一化一次。
    prompt_text = model.frontend.text_normalize(prompt_text, split=False, text_frontend=tf)
    synthesize(job, model, p.get("text") or "", tf, lambda seg: model.inference_zero_shot(
        seg, prompt_text, wav, zero_shot_spk_id=spk, stream=False, speed=speed, text_frontend=False))


def cross_lingual(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "cross_lingual")
    wav, _, spk = speaker(job, model, fam, p)

    def generate(seg: str):
        # CosyVoice 3 的系统提示按段加：加在整段正文前会让上游因 <|…|> 跳过归一化与切分（整篇一次合成）。
        if fam == "v3" and END not in seg:
            seg = f"{V3_SYSTEM}{END}{seg}"
        return model.inference_cross_lingual(seg, wav, zero_shot_spk_id=spk, stream=False, speed=speed, text_frontend=False)

    synthesize(job, model, p.get("text") or "", tf, generate)


def sft(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "sft")
    spk_id = p.get("spk_id") or ""
    if spk_id not in model.list_available_spks():
        raise ValueError(f"预训练音色不存在：{spk_id}，可用：{'、'.join(model.list_available_spks())}")
    text = p.get("text") or ""
    if fam == "v3-sft":
        prompt = _sft_v3_prompt(model, Path(p["model_dir"]), p.get("instruct") or "", tf)
        synthesize(job, model, text, tf, lambda seg: _sft_v3(model, seg, spk_id, prompt, speed))
        return
    synthesize(job, model, text, tf, lambda seg: model.inference_sft(seg, spk_id, stream=False, speed=speed, text_frontend=False))


def _sft_v3_prompt(model, model_dir: Path, guide: str, tf: bool):
    """CosyVoice 3 SFT 的提示 token：LLM 要求 <|endofprompt|> 出现在提示或正文中，上游 inference_sft 不提供提示，
    因此注入 prompt_text。

    训练时每条数据的 instruct 位于正文之前（上游 llm.py 的 prepare_lm_input_target），推理时 prompt_text 占据同一位置，
    两者不一致时 LLM 的条件偏离训练分布，长句更容易拖长或失控。因此 guide 为空时使用训练时的 instruct
    （kanade-sft.json 的 instruct；此前组装的模型没有该字段，按 sft.INSTRUCT_DEFAULT 即系统提示 + 标记）；
    guide 可写情绪 / 风格引导，按 CosyVoice 3 指令格式加上同一系统提示。"""
    if guide.strip():
        prompt = instruct_text_for("v3", guide)
    else:
        meta = json.loads((model_dir / "kanade-sft.json").read_text(encoding="utf-8"))
        prompt = meta.get("instruct") or f"{V3_SYSTEM}{END}"
    prompt_norm = model.frontend.text_normalize(prompt, split=False, text_frontend=tf)
    return model.frontend._extract_text_token(prompt_norm)


def _sft_v3(model, seg: str, spk_id: str, prompt, speed: float):
    """CosyVoice 3 SFT 合成一段。采样参数与随机种子已由 configure_sampling 设置。"""
    model_input = model.frontend.frontend_sft(seg, spk_id)
    model_input["prompt_text"], model_input["prompt_text_len"] = prompt
    yield from model.model.tts(**model_input, stream=False, speed=speed)


def instruct(job: Job) -> None:
    p, model, fam, speed, tf = _common(job, "instruct")
    instr = instruct_text_for(fam, p.get("instruct") or "")
    if instr == END or not instr.strip():
        raise ValueError("请填写指令文本")
    text = p.get("text") or ""
    if fam == "v1-instruct":
        spk_id = p.get("spk_id") or ""
        if spk_id not in model.list_available_spks():
            raise ValueError(f"预训练音色不存在：{spk_id}")
        synthesize(job, model, text, tf, lambda seg: model.inference_instruct(
            seg, spk_id, instr, stream=False, speed=speed, text_frontend=False))
        return
    # 不传已注册的 zero_shot_spk_id：上游 frontend_zero_shot 在给定 spk_id 时整体取用 spk2info 中缓存的输入，
    # 其中 prompt_text 是参考音频的文本，instruct 文本被忽略；frontend_instruct2 又删掉了参考语音 token，
    # 结果模型会把参考文本也念出来。直接用参考音频提取特征，每次多花一点提取时间。
    wav, _, _ = speaker(job, model, fam, p)
    synthesize(job, model, text, tf, lambda seg: model.inference_instruct2(
        seg, instr, wav, zero_shot_spk_id="", stream=False, speed=speed, text_frontend=False))


def vc(job: Job) -> None:
    p, model, fam, speed, _ = _common(job, "vc")
    source = check_audio(p.get("source_wav") or "", "源音频")
    wav, _, _ = speaker(job, model, fam, p)
    job.progress(0.1, "转换中")
    chunks = [out["tts_speech"] for out in model.inference_vc(source, wav, stream=False, speed=speed)]
    job.progress(0.98, "写出音频")
    write_output(job, model, chunks)
