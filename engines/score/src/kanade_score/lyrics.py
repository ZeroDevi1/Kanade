"""歌词对齐：把 LRC 歌词逐字对到主唱上，再把字分配给 SOME 识别出的音符（一字多音即连音 / 转音）。

流程：
1. 解析 LRC：逐行时间戳 + 文本，去掉标题、作词作曲等署名与版权提示行（这些行不唱）。每个汉字、每个拉丁单词是一个「字」（一个音节）；
   日文的假名按拍成字，汉字按 [kana:] 注音的拍数占时长。
2. 字级强制对齐：中文 wav2vec2 CTC 模型（jonatasgrosman/wav2vec2-large-xlsr-53-chinese-zh-cn）逐帧给出字符概率，
   用 CTC Viterbi 求整首歌词的最优路径，得到每个字的起点。两条约束来自 2026-10-21 用真实歌曲的实测：
   - LRC 行首作锚点：每行首字只能落在该行时间戳 ±ANCHOR_S 内。不加锚点时，置信度低的长音处会把下一行首字
     「挤」到上一行末尾（实测偏差可达 3.5 s）；加锚点后 47 行的行首偏差都在 0.3 s 内。
   - 静音帧不许出现字：分离出的干声在句间常是数字静音，模型在纯静音上的输出没有意义，不约束时字会被塞进静音段。
3. 字 → 音符，两种方法：
   - attach（默认）：每个字在「吸附到 SOME 音符的起点」「在音符内部拆开（同音高换字）」「丢弃」中选一个，
     用动态规划求总代价最小且时间递增的方案；没有分到字的音符接在前一个字后面，即连音（slur）。
   - attach_game：把字边界交给 GAME（见 game/），由模型在每个字内部切分音符并估计音高；按歌词行切段推理，
     段内的音符整体换成 GAME 的结果，歌词以外的部分保留 SOME 的音符。GAME 保证每个有声的字至少一个音，
     但权重为非商用许可，只在用户选择时使用。
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

import numpy as np

from kanade_engine import Job, pick_device

from . import models

ANCHOR_S = 0.5    # 行首字相对 LRC 时间戳的允许偏差（秒）；实测 LRC 与实际演唱差 0.3 s 以内
SILENT_DB = -50.0  # 帧 RMS 低于它（dBFS）视为静音
CHUNK_S = 20      # 求 CTC 概率的分块长度（秒）：wav2vec2 的自注意力随长度平方增长，整首一次算显存不够
CONTEXT_S = 1     # 分块两侧附带的上下文（秒），只取中间部分的输出，避免块边界处的概率失真
SR = 16000

# 字 → 音符的代价（经验值，用上面那首歌调过）：吸附距离每差 SNAP_SIGMA 秒代价 +1，拆音固定 SPLIT_COST，
# 丢字 LOST_COST；调大 LOST_COST 会更倾向于勉强匹配，调大 SPLIT_COST 会更少拆音。
SNAP_S = 0.3
SNAP_SIGMA = 0.08
SPLIT_COST = 1.5
LOST_COST = 6.0
MIN_SEG_S = 0.06  # 拆开后每段的最短时长（秒）
SLUR_GAP_S = 0.3  # 连音音符与前一个音之间允许的最长休止（秒），更长的休止后的音符视为无词

# GAME 按行切段：段首在首字前留 LEAD_S 秒（作为无声的 SP 字），段尾到下一行首字前 TAIL_GAP_S 秒，
# 最长到末字后 MAX_TAIL_S 秒（行末长音一般不超过它，更长的部分多为间奏）。
GAME_LEAD_S = 0.2
GAME_TAIL_GAP_S = 0.05
GAME_MAX_TAIL_S = 4.0


@dataclass
class Line:
    time: float  # LRC 时间戳（秒）
    text: str
    # words 是字：汉字（日文按注音合并的几个汉字，如「今日」）、拉丁单词、数字，或日文的一拍假名（拗音小字、
    # 促音、长音并入前一个假名）。morae 与之一一对应：这个字唱几拍（日文汉字为注音的拍数，没有注音时为 1；其余为 1）。
    words: list[str]
    morae: list[int] = field(default_factory=list)
    # japanese 为 True 表示行内有假名：对齐时不用中文模型对这些字的概率（见 align）。
    japanese: bool = False


_TAG = re.compile(r"\[(\d+):(\d+(?:\.\d+)?)\]")
_OFFSET = re.compile(r"\[offset:\s*([+-]?\d+)\]", re.I)
# 字：拉丁单词、汉字（含「々」）、单个假名、数字。
_WORD = re.compile(r"[A-Za-z']+|[㐀-鿿豈-﫿々]|[぀-ヿ]|\d")
_KANJI = re.compile(r"[㐀-鿿豈-﫿々]")
_KANA = re.compile(r"[぀-ヿ]")
# 并入前一个假名的字：拗音小字不占拍，促音与长音占一拍但不单独成字（唱的时候通常不另起一个音）。
_YOON = set("ゃゅょぁぃぅぇぉゎャュョァィゥェォヮ")
_ATTACH = _YOON | set("っッー")
# [kana:1はる1ひ1かげ…]：按出现顺序给每个汉字（或连续几个汉字）注读音，数字为这段读音覆盖的汉字个数。
_KANA_TAG = re.compile(r"\[kana:([^\]]*)\]")
_KANA_PAIR = re.compile(r"(\d+)([^\d]+)")
# 整行包在一对【】或〔〕里的提示（「【未经著作权人许可不得翻唱翻录或使用】」）。圆括号里的整行通常是和声歌词
# （「（せつなくて いとおしい）」），照常对齐。
_BRACKETED = re.compile(r"[【〔].*[】〕]")
# 版权提示的常见用语：平台在歌词开头插入的这类行不唱。
_NOTICE_WORDS = ("未经", "著作权", "版权所有", "翻唱翻录", "禁止翻唱", "商业使用")


def _is_meta(body: str) -> bool:
    """报告一行是否不是唱出来的歌词：含全角或半角冒号的署名行（「词：」「作曲:」「OP:」等）、整行包在【】里的提示
    或含版权提示用语的行。"""
    if "：" in body or ":" in body:
        return True
    if _BRACKETED.fullmatch(body):
        return True
    return any(k in body for k in _NOTICE_WORDS)


def _mora_count(kana: str) -> int:
    """假名串的拍数：拗音小字不计，其余（含促音、长音、拨音）各一拍；至少为 1。"""
    return max(1, sum(1 for ch in kana if _KANA.match(ch) and ch not in _YOON))


def _tokenize(body: str, readings: dict[int, tuple[int, str]]) -> tuple[list[str], list[int]]:
    """把一行切成字与每个字的拍数。readings 为 {汉字在 body 中的位置: (覆盖的汉字个数, 读音)}。"""
    words: list[str] = []
    morae: list[int] = []
    skip_to = -1
    for m in _WORD.finditer(body):
        i, w = m.start(), m.group()
        if i < skip_to:
            continue  # 已并入前一个按注音合并的汉字
        if _KANA.match(w):
            # 接在假名或汉字（其读音的最后一拍）后面的拗音小字、促音、长音并入前一个字。
            if w in _ATTACH and words and (_KANA.match(words[-1][-1]) or _KANJI.match(words[-1][-1])):
                words[-1] += w
                if w not in _YOON:
                    morae[-1] += 1
                continue
            words.append(w)
            morae.append(1)
            continue
        if _KANJI.match(w) and i in readings:
            k, reading = readings[i]
            end = i + k
            words.append(body[i:end])
            morae.append(_mora_count(reading))
            skip_to = end
            continue
        words.append(w)
        morae.append(1)
    return words, morae


def parse_lrc(text: str) -> list[Line]:
    """解析 LRC 文本，返回按时间排序的歌词行。

    支持一行多个时间戳（副歌复用）与 [offset:毫秒]（正值表示歌词提前）。去掉的行：没有字的行、署名与版权提示行
    （_is_meta），以及标题行——0 秒处含 " - " 的行，或 0 秒处、按时间紧随其后的一行是署名或提示行的行（常见的
    「歌名 / 作词… / 作曲… / 【提示】」开头）。这些行不唱，留下会被强制对齐硬塞进第一句之前，挤乱后面的字。

    日文歌词：假名按拍切成字；有 [kana:] 注音时给汉字配上读音，按读音的拍数决定这个字在对齐时占几拍。注音按
    文件中带时间戳的各行（含被去掉的标题、署名行）里汉字出现的顺序依次消耗。
    """
    offset = 0.0
    pairs: list[tuple[int, str]] = []
    if m := _KANA_TAG.search(text):
        pairs = [(int(k), r) for k, r in _KANA_PAIR.findall(m[1])]
    next_pair = 0
    # (时间, 文本, 是否署名或提示行, 字, 拍数)
    entries: list[tuple[float, str, bool, list[str], list[int]]] = []
    for raw in text.splitlines():
        if m := _OFFSET.search(raw):
            offset = int(m[1]) / 1000
            continue
        stamps = list(_TAG.finditer(raw))
        if not stamps:
            continue
        body = raw[stamps[-1].end():].strip()
        if not body:
            continue
        # 按出现顺序给本行的汉字配注音（每个文件行只消耗一次，与时间戳个数无关）。
        readings: dict[int, tuple[int, str]] = {}
        i = 0
        while i < len(body):
            if _KANJI.match(body[i]) and next_pair < len(pairs):
                k, reading = pairs[next_pair]
                next_pair += 1
                run = 1
                while run < k and i + run < len(body) and _KANJI.match(body[i + run]):
                    run += 1
                readings[i] = (run, reading)
                i += run
                continue
            i += 1
        words, morae = _tokenize(body, readings)
        meta = _is_meta(body)
        for s in stamps:
            t = int(s[1]) * 60 + float(s[2]) - offset
            entries.append((max(t, 0.0), body, meta, words, morae))
    entries.sort(key=lambda e: e[0])
    lines: list[Line] = []
    for i, (t, body, meta, words, morae) in enumerate(entries):
        if meta:
            continue
        if t < 0.5 and (" - " in body or (i + 1 < len(entries) and entries[i + 1][2])):
            continue
        if words:
            lines.append(Line(t, body, words, morae, japanese=bool(_KANA.search(body))))
    return lines


def _viterbi(lp: np.ndarray, toks: list[int], anchors: dict[int, tuple[int, int]], blank: int,
             blocked: np.ndarray | None) -> list[tuple[int, int]]:
    """带约束的 CTC Viterbi 强制对齐，返回每个 token 的 [起始帧, 结束帧)。

    lp 是 [T, V] 的 log 概率；toks 中的 -1 是通配符（模型词表里没有的字，取非 blank 的最大概率）；anchors 把第 j 个
    token 的起始帧限定在 [a, b]；blocked 为 True 的帧不能处在任何 token 上。状态按 CTC 惯例为 2L+1 个（blank 与
    token 交替）。约束无法满足时抛 ValueError。
    """
    T, L = lp.shape[0], len(toks)
    S = 2 * L + 1
    wild = np.max(np.delete(lp, blank, axis=1), axis=1)
    em = np.empty((T, S), np.float64)
    em[:, 0::2] = lp[:, blank:blank + 1]
    em[:, 1::2] = np.stack([wild if k < 0 else lp[:, k] for k in toks], 1)
    if blocked is not None:
        em[blocked, 1::2] = -1e9
    # 锚点换算成每个状态可占用的帧区间：token j 起始不早于 a → 状态 ≥ 2j+1 在 a 之前不可达；
    # 起始不晚于 b → 状态 ≤ 2j 在 b 之后不可达。
    early = np.zeros(S, np.int64)
    late = np.full(S, T - 1, np.int64)
    for j, (a, b) in anchors.items():
        early[2 * j + 1:] = np.maximum(early[2 * j + 1:], a)
        late[:2 * j + 1] = np.minimum(late[:2 * j + 1], b)
    # 从 s-2 跳入（跳过中间的 blank）只允许发生在两个不同的 token 之间。
    skip = np.zeros(S, bool)
    for j in range(1, L):
        skip[2 * j + 1] = toks[j] != toks[j - 1] or toks[j] < 0
    NEG = -1e30
    dp = np.full(S, NEG)
    dp[:2] = em[0, :2]
    dp[(early > 0) | (late < 0)] = NEG
    back = np.zeros((T, S), np.int8)  # 0：停留，1：来自 s-1，2：来自 s-2
    idx = np.arange(S)
    for t in range(1, T):
        cand = np.stack([dp, np.concatenate(([NEG], dp[:-1])),
                         np.where(skip, np.concatenate(([NEG, NEG], dp[:-2])), NEG)])
        arg = np.argmax(cand, 0)
        dp = cand[arg, idx] + em[t]
        dp[(t < early) | (t > late)] = NEG
        back[t] = arg
    s = int(S - 1 if dp[S - 1] >= dp[S - 2] else S - 2)
    if dp[s] < -1e8:
        raise ValueError("约束下没有可行的对齐")
    spans = [[-1, -1] for _ in range(L)]
    for t in range(T - 1, -1, -1):
        if s % 2:
            j = s // 2
            spans[j][1] = max(spans[j][1], t + 1)
            spans[j][0] = t
        s -= int(back[t, s])
    return [(a, b) for a, b in spans]


def align(job: Job, y_mono: np.ndarray, sr: int, lines: list[Line], lo: float, hi: float) -> tuple[list[dict], str]:
    """把 lines 逐字对齐到主唱 y_mono，返回 ([{start, end, text, line}]（秒）, 说明)。

    说明非空时表示放宽了约束（锚点或静音约束无法满足），结果可信度较低。
    """
    import soxr
    import torch
    from transformers import Wav2Vec2ForCTC, Wav2Vec2Processor

    d = pick_device(allow_mps=False)
    job.progress(lo, f"歌词对齐：加载 wav2vec2（{d.label}）")
    path = str(models.path(models.LYRICS_DIR))
    proc = Wav2Vec2Processor.from_pretrained(path)
    model = Wav2Vec2ForCTC.from_pretrained(path).eval().to(d.name)
    vocab = proc.tokenizer.get_vocab()
    blank = vocab[proc.tokenizer.pad_token]
    y = soxr.resample(y_mono, sr, SR).astype(np.float32) if sr != SR else y_mono.astype(np.float32)

    # 1. 分块求逐帧 log 概率（约 50 帧 / 秒），拼回整首。
    chunk, ctx = CHUNK_S * SR, CONTEXT_S * SR
    parts = []
    starts = range(0, len(y), chunk)
    with torch.no_grad():
        for i, s in enumerate(starts):
            job.check_cancelled()
            job.progress(lo + (hi - lo) * 0.8 * i / len(starts), f"歌词对齐 {i + 1}/{len(starts)}")
            a, b = max(0, s - ctx), min(len(y), s + chunk + ctx)
            x = proc(y[a:b], sampling_rate=SR, return_tensors="pt").input_values.to(d.name)
            lp = torch.log_softmax(model(x).logits[0].float(), -1).cpu().numpy()
            fps = lp.shape[0] / ((b - a) / SR)
            i0 = round((s - a) / SR * fps)
            parts.append(lp[i0:i0 + round(min(chunk, len(y) - s) / SR * fps)])
    lp = np.concatenate(parts)
    T = lp.shape[0]
    fps = T / (len(y) / SR)

    # 2. token 序列：汉字一个 token，拉丁单词按字母（词表为大写）；行首 token 加锚点。
    #    日文行（模型是中文的，假名不在词表里，汉字的中文读音也与演唱不符）：每个字按拍数放几个通配 token，
    #    只要求那几帧有人声，字的时长按拍数分配。
    toks: list[int] = []
    owner: list[int] = []  # token → 字序号
    words: list[dict] = []
    anchors: dict[int, tuple[int, int]] = {}
    for li, line in enumerate(lines):
        for wi, w in enumerate(line.words):
            if wi == 0:
                anchors[len(toks)] = (max(0, round((line.time - ANCHOR_S) * fps)), round((line.time + ANCHOR_S) * fps))
            if line.japanese:
                ids = [-1] * (line.morae[wi] if wi < len(line.morae) else 1)
            else:
                ids = [vocab.get(ch, -1) for ch in (w.upper() if w.isascii() else w)]
            for tid in ids:
                toks.append(tid)
                owner.append(len(words))
            words.append({"text": w, "line": li})
    hop = SR / fps
    rms = np.array([np.sqrt(np.mean(y[int(i * hop):int((i + 1) * hop)] ** 2)) for i in range(T)])
    silent = 20 * np.log10(rms + 1e-12) < SILENT_DB

    # 3. Viterbi；约束无法满足（LRC 时间与音频不符、锚点落在静音里等）时依次放宽。
    job.progress(lo + (hi - lo) * 0.85, "歌词对齐：求解")
    note = ""
    try:
        spans = _viterbi(lp, toks, anchors, blank, silent)
    except ValueError:
        try:
            spans, note = _viterbi(lp, toks, anchors, blank, None), "静音约束无法满足，已忽略"
        except ValueError:
            spans, note = _viterbi(lp, toks, {}, blank, None), "LRC 时间戳与音频对不上，已忽略行时间"
    for w in words:
        w["start"] = None
    for (a, b), k in zip(spans, owner):
        w = words[k]
        if w["start"] is None:
            w["start"], w["end"] = a / fps, b / fps
        w["end"] = b / fps
    for w in words:
        w["start"], w["end"] = round(w["start"], 3), round(w["end"], 3)
    return words, note


def attach(notes: list[dict], words: list[dict]) -> tuple[list[dict], int]:
    """把字分配给音符，返回 (新音符列表, 丢弃的字数)。

    notes 为 [{start, end, pitch}]（不修改），words 为 align 的结果。返回的音符多了 lyric 与 slur：
    lyric 是该音唱的字（字的第一个音），slur 为 True 表示同一个字的后续音（连音），两者都没有的音符是无词的
    （和声、哼唱或歌词里没有的部分）。字的起点落在某个音符中间时把音符拆成两个同音高的音。
    """
    ns = sorted((dict(n) for n in notes), key=lambda n: n["start"])

    # 1. 每个字的候选：(时间, 是否拆音, 音符下标, 代价)。
    cands: list[list[tuple[float, bool, int, float]]] = []
    for w in words:
        o, cs = w["start"], []
        for i, n in enumerate(ns):
            if abs(n["start"] - o) <= SNAP_S:
                cs.append((n["start"], False, i, abs(n["start"] - o) / SNAP_SIGMA))
            if n["start"] + MIN_SEG_S < o < n["end"] - MIN_SEG_S:
                cs.append((o, True, i, SPLIT_COST))
        cands.append(cs)

    # 2. 动态规划：状态是（最后选定的时间, 最后占用的音符），按字推进。拆音可以与前一个字共用音符（再拆一次），
    #    吸附必须用更靠后的音符；时间至少递增 MIN_SEG_S。丢弃一个字不改变状态。
    #    steps[k] 记录每个状态的（累计代价, 上一步状态, 选中的候选；None 为丢弃），最后回溯。
    states: dict[tuple[float, int], float] = {(-1.0, -1): 0.0}
    steps: list[dict[tuple[float, int], tuple[float, tuple[float, int], int | None]]] = []
    for cs in cands:
        nxt: dict[tuple[float, int], tuple[float, tuple[float, int], int | None]] = {}
        for key, cost in states.items():
            t, used = key
            options = [(key, cost + LOST_COST, None)]
            for j, (ct, split, i, cc) in enumerate(cs):
                if ct >= t + MIN_SEG_S and (i >= used if split else i > used):
                    options.append(((ct, i), cost + cc, j))
            for nk, nc, j in options:
                if nk not in nxt or nc < nxt[nk][0]:
                    nxt[nk] = (nc, key, j)
        steps.append(nxt)
        states = {k: v[0] for k, v in nxt.items()}
    key = min(states, key=states.get)
    choice: list[int | None] = [None] * len(cands)
    for k in range(len(cands) - 1, -1, -1):
        _, key, choice[k] = steps[k][key]

    # 3. 应用：按音符收集分配到的字（吸附的落在音符起点，拆音的落在中间），再按时间切开。
    marks: dict[int, list[tuple[float, int]]] = {}
    for k, j in enumerate(choice):
        if j is not None:
            ct, _, i, _ = cands[k][j]
            marks.setdefault(i, []).append((ct, k))
    out: list[dict] = []
    for i, n in enumerate(ns):
        cuts = sorted(marks.get(i, []))
        if not cuts or cuts[0][0] > n["start"]:
            cuts.insert(0, (n["start"], -1))
        for p, (s, k) in enumerate(cuts):
            e = cuts[p + 1][0] if p + 1 < len(cuts) else n["end"]
            m = {"start": round(s, 3), "end": round(e, 3), "pitch": n["pitch"]}
            if k >= 0:
                m["lyric"] = words[k]["text"]
            out.append(m)

    # 4. 连音：没有字的音符接在前一个有字（或连音）的音后，中间休止不超过 SLUR_GAP_S。
    prev = None
    for m in out:
        if "lyric" in m:
            prev = m
        elif prev is not None and m["start"] - prev["end"] <= SLUR_GAP_S:
            m["slur"] = True
            prev = m
        else:
            prev = None
    return out, sum(j is None for j in choice)


def attach_game(job: Job, y_mono: np.ndarray, sr: int, words: list[dict], notes: list[dict],
                lo: float, hi: float) -> tuple[list[dict], int]:
    """用 GAME 按字切分音符，返回 (新音符列表, 没有有声音符的字数)，音符格式与 attach 相同。

    notes 是 SOME 的音符（不修改）：每个歌词行的时间段内换成 GAME 的结果，段外原样保留（无字）。
    """
    import soxr
    import torch

    from . import game

    d = pick_device(allow_mps=False)
    job.progress(lo, f"连音识别：加载 GAME（{d.label}）")
    model = game.Model(models.path(models.GAME_DIR), d.name)
    y = soxr.resample(y_mono, sr, model.sr).astype(np.float32) if sr != model.sr else y_mono.astype(np.float32)
    duration = len(y) / model.sr

    lines: dict[int, list[dict]] = {}
    for w in words:
        lines.setdefault(w["line"], []).append(w)
    order = sorted(lines)
    out: list[dict] = []
    spans: list[tuple[float, float]] = []
    lost = 0
    prev_end = 0.0
    for n, li in enumerate(order):
        job.check_cancelled()
        job.progress(lo + (hi - lo) * n / len(order), f"连音识别 {n + 1}/{len(order)}")
        ws = lines[li]
        a = max(prev_end, ws[0]["start"] - GAME_LEAD_S)
        b = ws[-1]["start"] + GAME_MAX_TAIL_S
        if n + 1 < len(order):
            b = min(b, lines[order[n + 1]][0]["start"] - GAME_TAIL_GAP_S)
        b = min(b, duration)
        if b <= ws[-1]["start"] + MIN_SEG_S:
            b = min(duration, ws[-1]["start"] + MIN_SEG_S)
        prev_end = b
        # 字时长：首字前的 SP 加上相邻字起点之差，末字到段尾；之和等于段长。
        starts = [w["start"] for w in ws]
        word_dur = [starts[0] - a] + [t1 - t0 for t0, t1 in zip(starts, starts[1:])] + [b - starts[-1]]
        dur, voiced, pitch = model.infer(y[int(a * model.sr):int(b * model.sr)], word_dur, "zh")
        per_word = game.align_notes_to_words(word_dur, [p if v else None for p, v in zip(pitch, voiced)], dur)
        t = a + sum(x[1] for x in per_word[0])  # 跳过 SP
        for w, ns in zip(ws, per_word[1:]):
            first = True
            for p, dd in ns:
                if p is not None and dd > 0:
                    m = {"start": round(t, 3), "end": round(t + dd, 3), "pitch": round(p, 2)}
                    if first:
                        m["lyric"] = w["text"]
                    else:
                        m["slur"] = True
                    first = False
                    out.append(m)
                t += dd
            lost += first
        spans.append((a, b))
    del model
    if d.name.startswith("cuda"):
        torch.cuda.empty_cache()

    # 段外保留 SOME 的音符（与段重叠的整个丢掉，段内已由 GAME 覆盖）。
    def outside(m: dict) -> bool:
        return all(m["end"] <= a or m["start"] >= b for a, b in spans)

    out += [dict(m) for m in notes if outside(m)]
    out.sort(key=lambda m: m["start"])
    return out, lost
