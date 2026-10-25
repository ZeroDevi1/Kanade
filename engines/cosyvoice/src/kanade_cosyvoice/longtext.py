"""长文本合成：分段、逐段合成与检查、按标点停顿拼接，并保存每段音频以便单独重新生成。

上游的做法（frontend.text_normalize + inference_*）对长文本有几个问题：
- 中文先删掉所有换行，段落结构与段间停顿都丢了；
- 只按句末标点切分并合并到约 80 字，只有逗号的长句整句交给 LLM，越长越容易拖长、重复或提前结束；
- 文本含 <|…|> 时整段跳过归一化与切分——CosyVoice 3 跨语种要在正文前加系统提示（含 <|endofprompt|>），
  因此此前 CosyVoice 3 的长文本是一整段合成的；
- 各句直接首尾相接，停顿长短取决于每句模型自带的静音。

这里的流程：
1. plan：按换行分段落；每段用上游 text_normalize 归一化并切句（关闭归一化或含标记时按标点自行切句），
   超过 MAX_ZH_CHARS / MAX_EN_WORDS 的句子再按逗号切开。每段记下其后的停顿类型（逗号 / 句末 / 换行 / 空行）。
2. 逐段合成：每段以 text_frontend=False 交给上游（已归一化，不再二次处理、不再切分），模式所需的前缀
   （CosyVoice 3 的系统提示）由调用方按段加上。时长偏离按字数估计的范围（拖长或过短）时换随机种子重试，
   最多 MAX_RETRIES 次，都不合格时取最接近估计的一次。
3. 拼接：每段去掉首尾静音（保留 EDGE_KEEP_S），按停顿类型插入静音（乘以 pause_scale）。
4. 输出：最终 WAV（artifact kind audio）与 segments.json（kind segments，每段的文本、文件、时长、停顿与重试次数）；
   取消时把已合成的段落拼成部分结果登记后再结束，重新生成单段见 regenerate。
"""

from __future__ import annotations

import json
import random
import re
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable, Iterable

import numpy as np

from kanade_engine import Cancelled, Job

# 句子上限：中文按字数、其他语言按词数；超过时按逗号切开。中文 70 字约 15 秒语音，CosyVoice 在这个长度内较少失控
# （上游合并上限为 80 字，只有逗号的长句不切）；英文 45 词同理。
MAX_ZH_CHARS = 70
MAX_EN_WORDS = 45
# 按逗号切开时，按分句依次装入、每块不超过 PIECE_ZH 字 / PIECE_EN 词（单个分句本身更长时整句成块），
# 尽量少切：切得太碎，每块都按句末语调收尾，听起来一顿一顿。
PIECE_ZH = 50
PIECE_EN = 30

# 停顿（秒，pause_scale 为 1 时）：逗号切开处、句末、段内换行、空行分隔的段落。经验值，接近朗读的常见节奏。
PAUSES = {"comma": 0.15, "sentence": 0.32, "line": 0.5, "paragraph": 0.75, "end": 0.0}
# 去首尾静音：帧 RMS 低于峰值 −40 dB 视为静音，保留 EDGE_KEEP_S 的边缘（避免切掉轻声的起音与尾音）。
SILENCE_DB = -40.0
EDGE_KEEP_S = 0.06

# 时长检查：按每字 / 每词的朗读时长估计（语速 1 时中文约 0.22 秒 / 字，英文约 0.36 秒 / 词，经验值）。
# 超过估计的 RUNAWAY_RATIO 倍且多出 RUNAWAY_EXTRA_S 秒视为拖长，低于 TOO_SHORT_RATIO 倍视为过短。
SEC_PER_ZH_CHAR = 0.22
SEC_PER_EN_WORD = 0.36
RUNAWAY_RATIO, RUNAWAY_EXTRA_S = 2.5, 3.0
TOO_SHORT_RATIO = 0.35
MAX_RETRIES = 2

_ZH = re.compile(r"[一-鿿]")
_SENT_END = "。！？!?；;…"
_COMMAS = "，,、：:"
_TAG = re.compile(r"<\|[^|]*\|>|\[[a-z_]+\]|</?[a-z]+>")


@dataclass
class Segment:
    text: str  # 已归一化、交给模型的文本（不含模式前缀）
    pause: str  # 之后的停顿类型，PAUSES 的键
    file: str = ""  # 合成后的音频文件名（相对 segments.json 所在目录）
    duration: float = 0.0  # 去首尾静音后的时长（秒）
    retries: int = 0
    suspicious: bool = False  # 重试后仍超出时长范围


# ---------- 分段 ----------

def plan(model, text: str, text_frontend: bool) -> list[Segment]:
    """把文本切成待合成的段落（见文件头第 1 步）。没有可读的内容时返回空列表。"""
    lines = [ln.strip() for ln in text.replace("\r\n", "\n").split("\n")]
    out: list[Segment] = []
    blank = False
    for ln in lines:
        if not ln:
            blank = True
            continue
        if out:
            out[-1].pause = "paragraph" if blank else "line"
        blank = False
        for sent in _sentences(model, ln, text_frontend):
            for piece in _split_long(sent):
                out.append(Segment(piece, "comma" if piece[-1:] in _COMMAS else "sentence"))
            if out:
                out[-1].pause = "sentence"
    if out:
        out[-1].pause = "end"
    return out


def _sentences(model, line: str, text_frontend: bool) -> list[str]:
    """一行文本的句子：可归一化时用上游 text_normalize（含数字、符号读法）；否则按句末标点切开，保留标记。"""
    if text_frontend and not ("<|" in line and "|>" in line):
        segs = model.frontend.text_normalize(line, split=True, text_frontend=True)
        return [s for s in (x.strip() for x in segs) if _readable(s)]
    parts, cur = [], ""
    for k, ch in enumerate(line):
        cur += ch
        # 英文句点只在其后是空白或行尾时算句末（排除 3.5、e.g 中间等）。
        if ch in _SENT_END or (ch == "." and (k + 1 == len(line) or line[k + 1].isspace())):
            parts.append(cur.strip())
            cur = ""
    if cur.strip():
        parts.append(cur.strip())
    return [p for p in parts if _readable(p)]


def _readable(s: str) -> bool:
    """去掉标记与标点后还有文字。"""
    return bool(re.sub(r"[\W_]+", "", _TAG.sub("", s)))


def _split_long(sent: str) -> list[str]:
    """超过上限的句子按逗号切开：分句依次装入当前块，装下会超过 PIECE_* 时另起一块。没有逗号时原样返回
    （不在词中间硬切）；单个分句超过 PIECE_* 时整句成块。"""
    zh = bool(_ZH.search(sent))
    size = (lambda s: len(_TAG.sub("", s))) if zh else (lambda s: len(_TAG.sub("", s).split()))
    limit, piece = (MAX_ZH_CHARS, PIECE_ZH) if zh else (MAX_EN_WORDS, PIECE_EN)
    if size(sent) <= limit:
        return [sent]
    clauses, cur = [], ""
    for ch in sent:
        cur += ch
        if ch in _COMMAS:
            clauses.append(cur)
            cur = ""
    if cur:
        clauses.append(cur)
    pieces, block = [], ""
    for cl in clauses:
        if block and size(block + cl) > piece:
            pieces.append(block)
            block = ""
        block += cl
    if block:
        pieces.append(block)
    return [p.strip() for p in pieces if p.strip()]


def expected_seconds(text: str, speed: float) -> float:
    """按字 / 词数估计朗读时长（秒）。"""
    plain = _TAG.sub("", text)
    zh = len(_ZH.findall(plain))
    words = len(re.findall(r"[A-Za-z0-9']+", plain))
    return (zh * SEC_PER_ZH_CHAR + words * SEC_PER_EN_WORD) / max(speed, 0.1)


# ---------- 合成 ----------

Generate = Callable[[str], Iterable[dict]]


def synthesize(job: Job, model, segs: list[Segment], generate: Generate, speed: float, pause_scale: float,
               fixed_seed: int | None) -> None:
    """逐段合成并输出（见文件头第 2–4 步）。generate(text) 对一段文本产出上游的推理结果（非流式时恰好一项）。

    fixed_seed 为 None 时每段的首次合成沿用调用方设置的随机状态；重试总是换新的随机种子。
    取消时登记已合成部分的拼接结果，然后抛出 Cancelled。
    """
    if not segs:
        raise ValueError("文本为空或只有标点")
    work = job.output_dir / job.id
    work.mkdir(parents=True, exist_ok=True)
    total_chars = sum(max(len(s.text), 1) for s in segs)
    done_chars, t0 = 0, time.monotonic()
    job.progress(0.1, f"共 {len(segs)} 段")
    audio: list[np.ndarray] = []
    try:
        for i, seg in enumerate(segs):
            clip = _synthesize_one(job, model, seg, generate, speed, fixed_seed, i)
            _save(work / f"seg_{i:03d}.wav", clip, model.sample_rate)
            seg.file, seg.duration = f"seg_{i:03d}.wav", round(len(clip) / model.sample_rate, 3)
            audio.append(clip)
            done_chars += max(len(seg.text), 1)
            elapsed = time.monotonic() - t0
            eta = elapsed / done_chars * (total_chars - done_chars)
            note = f"已合成 {i + 1}/{len(segs)} 段"
            if i + 1 < len(segs):
                note += f" · 预计还需 {_clock(eta)}"
            if seg.retries:
                note += f" · 第 {i + 1} 段重试 {seg.retries} 次"
            job.progress(0.1 + 0.88 * done_chars / total_chars, note)
    except Cancelled:
        if audio:
            _finish(job, work, model.sample_rate, segs[:len(audio)], audio, pause_scale, partial=True)
        raise
    except Exception as exc:
        # 取消时 LLM 被提前截断，残缺的 token 序列可能让后续的 flow / 声码器报错；此时按取消处理而不是失败。
        if job.cancelled:
            if audio:
                _finish(job, work, model.sample_rate, segs[:len(audio)], audio, pause_scale, partial=True)
            raise Cancelled() from exc
        raise
    _finish(job, work, model.sample_rate, segs, audio, pause_scale)


def _synthesize_one(job: Job, model, seg: Segment, generate: Generate, speed: float, fixed_seed: int | None,
                    index: int) -> np.ndarray:
    """合成一段并检查时长，不合格时换种子重试；返回去首尾静音后的单声道 float32。"""
    import torch

    want = expected_seconds(seg.text, speed)
    best, best_err = None, float("inf")
    for attempt in range(MAX_RETRIES + 1):
        if attempt:
            seed = random.randrange(2**31) if fixed_seed is None else fixed_seed + 7919 * attempt + index
            random.seed(seed)
            np.random.seed(seed % 2**32)
            torch.manual_seed(seed)
        chunks = [out["tts_speech"] for out in generate(seg.text)]
        job.check_cancelled()
        if not chunks:
            continue
        clip = trim(torch.cat(chunks, dim=1).squeeze(0).cpu().float().numpy(), model.sample_rate)
        dur = len(clip) / model.sample_rate
        ok, err = judge(dur, want)
        if err < best_err:
            best, best_err = clip, err
        seg.retries = attempt
        if ok:
            seg.suspicious = False
            return clip
        seg.suspicious = True
    if best is None:
        raise RuntimeError(f"第 {index + 1} 段没有输出音频：{seg.text[:30]}")
    return best


def judge(dur: float, want: float) -> tuple[bool, float]:
    """判断实际时长 dur 是否在估计 want 的合理范围内，并返回偏离程度（对数比，越小越好）。估计不到 1 秒的
    短段（只有几个字）不判过短：句首语气词等本来就短。"""
    if want <= 0:
        return dur > 0, 0.0
    err = abs(np.log(max(dur, 1e-3) / want))
    if dur > max(want * RUNAWAY_RATIO, want + RUNAWAY_EXTRA_S):
        return False, err
    if want >= 1.0 and dur < want * TOO_SHORT_RATIO:
        return False, err
    return True, err


def trim(x: np.ndarray, sr: int) -> np.ndarray:
    """去掉首尾低于峰值 SILENCE_DB 的静音（10 ms 帧），两端各保留 EDGE_KEEP_S。全静音时原样返回。"""
    hop = max(sr // 100, 1)
    n = len(x) // hop
    if n == 0:
        return x
    rms = np.sqrt((x[: n * hop].reshape(n, hop) ** 2).mean(axis=1) + 1e-12)
    loud = np.nonzero(20 * np.log10(rms / (rms.max() + 1e-12)) > SILENCE_DB)[0]
    if len(loud) == 0:
        return x
    keep = int(EDGE_KEEP_S * sr)
    return x[max(loud[0] * hop - keep, 0): min((loud[-1] + 1) * hop + keep, len(x))]


def join(clips: list[np.ndarray], segs: list[Segment], sr: int, pause_scale: float) -> np.ndarray:
    """按每段之后的停顿类型插入静音并拼接；每段首尾各 5 ms 淡入淡出，避免接缝处的爆音。"""
    fade = max(int(0.005 * sr), 1)
    ramp = np.linspace(0, 1, fade, dtype=np.float32)
    parts = []
    for clip, seg in zip(clips, segs):
        c = clip.astype(np.float32).copy()
        if len(c) > 2 * fade:
            c[:fade] *= ramp
            c[-fade:] *= ramp[::-1]
        parts.append(c)
        gap = PAUSES.get(seg.pause, 0.3) * pause_scale
        if seg is not segs[-1] and gap > 0:
            parts.append(np.zeros(int(gap * sr), dtype=np.float32))
    return np.concatenate(parts) if parts else np.zeros(0, dtype=np.float32)


def _finish(job: Job, work: Path, sr: int, segs: list[Segment], clips: list[np.ndarray], pause_scale: float,
            partial: bool = False) -> None:
    """拼接并写出最终音频与 segments.json，登记为产物。"""
    audio = join(clips, segs, sr, pause_scale)
    peak = float(np.abs(audio).max()) if len(audio) else 0.0
    if peak > 0.99:  # 各段响度不同，拼接后防止削波
        audio = audio * (0.99 / peak)
    out = work / (f"{job.id}-partial.wav" if partial else f"{job.id}.wav")
    _save(out, audio, sr)
    manifest = {"version": 1, "sample_rate": sr, "pause_scale": pause_scale, "partial": partial, "audio": out.name,
                "segments": [asdict(s) for s in segs]}
    (work / "segments.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    job.artifact(out, "audio")
    job.artifact(work / "segments.json", "segments")


def regenerate(job: Job, model, manifest_path: Path, index: int, generate: Generate, speed: float,
               fixed_seed: int | None) -> None:
    """重新合成 segments.json 中的第 index 段（文本不变、换随机种子），替换该段音频并重新拼接，输出新的最终音频
    （文件名带序号，避免界面播放器沿用旧文件）。segments.json 原地更新。"""
    import soundfile as sf

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    segs = [Segment(**s) for s in manifest["segments"]]
    if not 0 <= index < len(segs):
        raise ValueError(f"没有第 {index + 1} 段")
    work = manifest_path.parent
    sr = int(manifest["sample_rate"])
    if sr != model.sample_rate:
        raise ValueError("重新生成须使用合成时的同一个模型")
    seg = segs[index]
    job.progress(0.1, f"重新生成第 {index + 1} 段")
    # 重新生成的目的就是换一次结果：总是换随机种子（固定种子时在其上偏移）。
    seed = random.randrange(2**31) if fixed_seed is None else fixed_seed + 104729 * (seg.retries + 1)
    random.seed(seed)
    np.random.seed(seed % 2**32)
    import torch

    torch.manual_seed(seed)
    clip = _synthesize_one(job, model, seg, generate, speed, None, index)
    version = int(manifest.get("revision", 0)) + 1
    seg.file = f"seg_{index:03d}_r{version}.wav"
    _save(work / seg.file, clip, sr)
    seg.duration = round(len(clip) / sr, 3)
    clips = [sf.read(str(work / s.file), dtype="float32")[0] for s in segs]
    audio = join(clips, segs, sr, float(manifest.get("pause_scale", 1.0)))
    peak = float(np.abs(audio).max()) if len(audio) else 0.0
    if peak > 0.99:
        audio = audio * (0.99 / peak)
    stem = Path(manifest["audio"]).stem.split("-r")[0]
    out = work / f"{stem}-r{version}.wav"
    _save(out, audio, sr)
    manifest.update({"revision": version, "audio": out.name, "segments": [asdict(s) for s in segs]})
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    job.artifact(out, "audio")
    job.artifact(manifest_path, "segments")
    job.progress(1.0, f"第 {index + 1} 段已重新生成（{seg.duration:.1f} 秒）")


def _save(path: Path, audio: np.ndarray, sr: int) -> None:
    import soundfile as sf

    sf.write(str(path), audio, sr, subtype="PCM_16")


def _clock(sec: float) -> str:
    sec = int(sec + 0.5)
    return f"{sec // 60} 分 {sec % 60:02d} 秒" if sec >= 60 else f"{sec} 秒"
