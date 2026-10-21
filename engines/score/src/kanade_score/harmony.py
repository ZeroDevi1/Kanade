"""调号与和弦：基于 CQT 色度（chroma）的模板匹配。

调号：平均色度与 24 个大小调的调性轮廓求相关（Krumhansl–Schmuckler 算法），轮廓可选 Krumhansl–Kessler
（1982 年听辨实验）或 Temperley（2001 年，基于 Kostka–Payne 教材语料）；再加上「和弦时长落在该调自然音级三和弦上的
比例」。只用轮廓时，主调与属调（如 Db 与 Ab 大调）常常只差千分之几，和弦能区分：Ab 大调里没有 Ebm 与 Gb。
分段用同样的打分估计，用于提示转调。

和弦：在节拍同步的色度上，与各和弦的二值模板做余弦相似度，再用 HMM（自转移概率 self_prob）做 Viterbi 平滑，
避免逐拍抖动。能量过低的拍记为 N（无和弦）。定调后再解码一次，给该调的自然音级和弦少量加分，修正大小三和弦的
误判（三音被人声或乐器泛音掩盖时常见）。输入最好是去掉人声的伴奏（人声旋律音会干扰色度）。
"""

from __future__ import annotations

import numpy as np

NAMES_SHARP = ["C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"]
NAMES_FLAT = ["C", "Db", "D", "Eb", "E", "F", "Gb", "G", "Ab", "A", "Bb", "B"]

PROFILES = {
    "krumhansl": ([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88],
                  [6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17]),
    "temperley": ([5.0, 2.0, 3.5, 2.0, 4.5, 4.0, 2.0, 4.5, 2.0, 3.5, 1.5, 4.0],
                  [5.0, 2.0, 3.5, 4.5, 2.0, 4.0, 2.0, 4.5, 3.5, 2.0, 1.5, 4.0]),
}

# 和弦性质：名称后缀与相对根音的音程。basic 只有大三 / 小三；extended 加七和弦、减三与挂四。
QUALITIES = {
    "maj": ("", (0, 4, 7)),
    "min": ("m", (0, 3, 7)),
    "7": ("7", (0, 4, 7, 10)),
    "maj7": ("maj7", (0, 4, 7, 11)),
    "min7": ("m7", (0, 3, 7, 10)),
    "dim": ("dim", (0, 3, 6)),
    "sus4": ("sus4", (0, 5, 7)),
}
VOCABULARIES = {"basic": ("maj", "min"), "extended": ("maj", "min", "7", "maj7", "min7", "dim", "sus4")}
# 非三和弦的先验惩罚（余弦相似度上的减分）：四音模板与色度的重合更多，不惩罚会过度识别七和弦。经验值。
COMPLEX_PENALTY = 0.04
# 拍的色度能量低于整首中位数的该比例时视为无和弦（静音、前奏只有鼓等）。经验值。
SILENCE_RATIO = 0.1
# 发射概率的锐度（softmax 温度的倒数）：越大越信任逐拍的模板匹配，越小越依赖平滑。经验值。
EMISSION_SHARPNESS = 20.0

# 和弦对调号打分的权重：最终分数 = 轮廓相关系数 + KEY_CHORD_WEIGHT × 自然音级和弦的时长比例。
# 实测《我不想念》：Db / Ab 大调的相关系数差 0.008，和弦比例差约 0.09，权重 0.5 足以让和弦起决定作用；
# 轮廓差异明显（> 0.1）的歌，和弦比例通常与之一致，不会被反转。
KEY_CHORD_WEIGHT = 0.5
# 定调后第二遍解码时自然音级和弦的加分（余弦相似度）：同根大小三和弦的相似度通常相差 0.03–0.08。经验值。
DIATONIC_BONUS = 0.04
# 转调提示：窗口内整首调号的得分与该窗最佳得分相差不超过 SEGMENT_TOLERANCE 时，该窗仍算整首调号；
# 连续 MIN_SEGMENT_WINDOWS 个窗口（窗口为 key_segment_s 秒、步长半窗，默认约 1 分钟）都偏离时才报告转调。
# 实测《我不想念》（无转调）逐窗最佳调在 Db 大调、F 小调、Ab 大调、C# 小调之间跳动，但 Db 大调多在 0.1 以内。
SEGMENT_TOLERANCE = 0.1
MIN_SEGMENT_WINDOWS = 4

# 和弦性质归类为三和弦家族，用于判断是否为自然音级和弦。
_FAMILY = {"maj": "maj", "7": "maj", "maj7": "maj", "sus4": "maj", "min": "min", "min7": "min", "dim": "dim"}
# 自然音级三和弦（相对主音的根音, 家族）。小调用和声小调的 V（大三），与流行音乐的习惯一致。
_DIATONIC = {
    "major": {(0, "maj"), (2, "min"), (4, "min"), (5, "maj"), (7, "maj"), (9, "min"), (11, "dim")},
    "minor": {(0, "min"), (2, "dim"), (3, "maj"), (5, "min"), (7, "min"), (7, "maj"), (8, "maj"), (10, "maj")},
}


def is_diatonic(root: int, quality: str, tonic: int, mode: str) -> bool:
    return ((root - tonic) % 12, _FAMILY[quality]) in _DIATONIC[mode]


def chord_fit(chords: list[dict], tonic: int, mode: str, t0: float = 0.0, t1: float = float("inf")) -> float:
    """[t0, t1) 内和弦时长落在该调自然音级三和弦上的比例；没有和弦时为 0。"""
    total = fit = 0.0
    for c in chords:
        if c.get("root") is None:
            continue
        d = max(0.0, min(c["end"], t1) - max(c["start"], t0))
        total += d
        if is_diatonic(c["root"], c["quality"], tonic, mode):
            fit += d
    return fit / total if total > 0 else 0.0


# 用降号拼写的调（大调主音；小调按其关系大调判断）。
_FLAT_MAJOR = {5, 10, 3, 8, 1, 6}  # F Bb Eb Ab Db Gb


def uses_flats(tonic: int, mode: str) -> bool:
    major = tonic if mode == "major" else (tonic + 3) % 12
    return major in _FLAT_MAJOR


def key_label(tonic: int, mode: str, flats: bool | None = None) -> str:
    names = NAMES_FLAT if (uses_flats(tonic, mode) if flats is None else flats) else NAMES_SHARP
    return f"{names[tonic]} {'大调' if mode == 'major' else '小调'}"


def chroma(y: np.ndarray, sr: int, hop: int = 512) -> np.ndarray:
    """谐波部分的 CQT 色度，形状 (12, 帧数)。先做 HPSS 去掉打击乐，减少鼓对色度的污染。"""
    import librosa

    mono = y.mean(axis=1) if y.ndim == 2 else y
    harmonic = librosa.effects.harmonic(mono, margin=4)
    return librosa.feature.chroma_cqt(y=harmonic, sr=sr, hop_length=hop, bins_per_octave=36)


def _key_scores(c: np.ndarray, profile: str, chords: list[dict] | None = None, t0: float = 0.0,
                t1: float = float("inf")) -> list[tuple[float, float, int, str]]:
    """24 个调的 (综合分, 轮廓相关系数, 主音, 调式)，按综合分降序。chords 为空时综合分即相关系数。"""
    v = c.mean(axis=1) if c.ndim == 2 else c
    out = []
    for mode, prof in zip(("major", "minor"), PROFILES[profile]):
        p = np.asarray(prof)
        for t in range(12):
            r = float(np.corrcoef(v, np.roll(p, t))[0, 1])
            r = r if np.isfinite(r) else -1.0
            fit = chord_fit(chords, t, mode, t0, t1) if chords else 0.0
            out.append((r + KEY_CHORD_WEIGHT * fit, r, t, mode))
    return sorted(out, reverse=True)


def initial_key(c: np.ndarray, profile: str) -> tuple[int, str]:
    """只用色度轮廓的调号，用于第一遍和弦识别的拼写（升号 / 降号）。"""
    _, _, t, mode = _key_scores(c, profile)[0]
    return t, mode


def detect_key(c: np.ndarray, sr: int, hop: int, profile: str, segment_s: float, chords: list[dict]) -> dict:
    """整首调号、候选与分段调号。chords 是第一遍识别的和弦（带 root / quality）。"""
    scores = _key_scores(c, profile, chords)
    (s1, r1, t, mode), s2 = scores[0], scores[1][0]
    rel = ((t + 9) % 12, "minor") if mode == "major" else ((t + 3) % 12, "major")
    flats = uses_flats(t, mode)
    # 置信度：最佳与次佳综合分之差。大于 0.1 时结论明确，小于 0.04 时两者难分。
    margin = s1 - s2
    level = "高" if margin > 0.1 else "中" if margin > 0.04 else "低"
    result = {
        "tonic": NAMES_FLAT[t] if flats else NAMES_SHARP[t], "tonic_pc": t, "mode": mode, "label": key_label(t, mode),
        "correlation": round(r1, 3), "chord_fit": round(chord_fit(chords, t, mode), 3), "margin": round(margin, 3),
        "confidence": level, "relative": key_label(*rel, flats=flats), "flats": flats,
        "candidates": [{"label": key_label(tt, m), "score": round(sc, 3), "correlation": round(r, 3)}
                       for sc, r, tt, m in scores[:4]],
        "segments": [],
    }
    # 分段：segment_s 秒的窗口、半窗步长逐窗打分。每窗代表其中间的半窗时长，相邻窗口不重叠。
    frames = int(segment_s * sr / hop)
    duration = c.shape[1] * hop / sr
    if frames > 0 and c.shape[1] > frames * 2:
        wins = []
        for s in range(0, c.shape[1] - frames // 2, frames // 2):
            start, end = s * hop / sr, min(s + frames, c.shape[1]) * hop / sr
            sc = _key_scores(c[:, s:s + frames], profile, chords, start, end)
            best = sc[0]
            own = next(x[0] for x in sc if x[2] == t and x[3] == mode)
            label = result["label"] if best[0] - own <= SEGMENT_TOLERANCE else key_label(best[2], best[3])
            wins.append([label, start + segment_s / 4, start + segment_s * 3 / 4])
        wins[0][1], wins[-1][2] = 0.0, duration
        for a, b in zip(wins[:-1], wins[1:]):
            a[2] = b[1]
        runs: list[list] = []
        for w in wins:
            if runs and runs[-1][0] == w[0]:
                runs[-1][2], runs[-1][3] = w[2], runs[-1][3] + 1
            else:
                runs.append(w + [1])
        # 偏离整首调号但不够长的段改回整首调号，再合并相邻同调段。
        merged: list[list] = []
        for r in runs:
            if r[0] != result["label"] and r[3] < MIN_SEGMENT_WINDOWS:
                r[0] = result["label"]
            if merged and merged[-1][0] == r[0]:
                merged[-1][2] = r[2]
            else:
                merged.append(r)
        if len(merged) > 1:
            # 有转调时整首调号是各段的折中（G→A 的歌整首最像 D 大调），各段按自身的整段范围重新定调。
            for r in merged:
                a, b = int(r[1] * sr / hop), max(int(r[2] * sr / hop), int(r[1] * sr / hop) + 1)
                _, _, tt, m = _key_scores(c[:, a:b], profile, chords, r[1], r[2])[0]
                r[0] = key_label(tt, m)
            segs: list[list] = []
            for r in merged:
                if segs and segs[-1][0] == r[0]:
                    segs[-1][2] = r[2]
                else:
                    segs.append(r)
            if len(segs) > 1:
                result["segments"] = [{"start": round(r[1], 2), "end": round(r[2], 2), "label": r[0]} for r in segs]
    return result


def _templates(vocab: str, flats: bool) -> tuple[list[tuple[str, int, str]], np.ndarray, np.ndarray]:
    """返回 [(标签, 根音, 性质)]、归一化模板与先验惩罚。"""
    names = NAMES_FLAT if flats else NAMES_SHARP
    labels, temps, penalty = [], [], []
    for q in VOCABULARIES[vocab]:
        suffix, intervals = QUALITIES[q]
        for root in range(12):
            v = np.zeros(12)
            for i in intervals:
                v[(root + i) % 12] = 1.0
            v[root] += 0.5  # 根音加权：区分 C6 / Am7 等同音集合时偏向根音明显的解释
            labels.append((names[root] + suffix, root, q))
            temps.append(v / np.linalg.norm(v))
            penalty.append(COMPLEX_PENALTY if len(intervals) > 3 or q in ("dim", "sus4") else 0.0)
    return labels, np.array(temps), np.array(penalty)


def detect_chords(c: np.ndarray, sr: int, hop: int, beats: np.ndarray, duration: float, vocab: str, flats: bool,
                  self_prob: float, min_beats: int, key: tuple[int, str] | None = None) -> list[dict]:
    """节拍同步的和弦序列，相邻同名合并为段。beats 少于 4 个时按 0.5 秒定长分段。

    key=(主音, 调式) 时给该调的自然音级和弦加 DIATONIC_BONUS。返回的段带 root（N 为 None）与 quality。
    """
    import librosa

    if len(beats) >= 4:
        bounds = np.concatenate([[0.0], beats, [duration]])
    else:
        bounds = np.arange(0.0, duration + 0.5, 0.5)
    bounds = np.unique(np.clip(bounds, 0, duration))
    frames = np.unique(librosa.time_to_frames(bounds, sr=sr, hop_length=hop))
    frames = frames[frames < c.shape[1]]
    sync = librosa.util.sync(c, frames, aggregate=np.median)  # (12, 段数)
    times = librosa.frames_to_time(frames, sr=sr, hop_length=hop)
    ends = np.append(times[1:], duration)

    labels, temps, penalty = _templates(vocab, flats)
    energy = np.linalg.norm(sync, axis=0)
    norm = sync / np.maximum(energy, 1e-9)
    bonus = np.array([DIATONIC_BONUS if key and is_diatonic(r, q, *key) else 0.0 for _, r, q in labels])
    sim = temps @ norm - penalty[:, None] + bonus[:, None]  # (和弦数, 段数)
    silent = energy < SILENCE_RATIO * np.median(energy[energy > 0]) if np.any(energy > 0) else np.ones_like(energy, bool)

    # 发射概率：模板相似度的 softmax，外加 N 状态（无声段概率高，其余段很低）。
    n = len(labels)
    logits = np.vstack([sim, np.where(silent, 1.0, -1.0)[None, :]]) * EMISSION_SHARPNESS
    prob = np.exp(logits - logits.max(axis=0))
    prob /= prob.sum(axis=0)
    trans = librosa.sequence.transition_loop(n + 1, self_prob)
    path = librosa.sequence.viterbi(prob, trans)

    segs: list[dict] = []
    for i, state in enumerate(path):
        label, root, q = ("N", None, None) if state == n else labels[state]
        if segs and segs[-1]["label"] == label:
            segs[-1]["end"] = float(ends[i])
            segs[-1]["beats"] += 1
        else:
            segs.append({"start": float(times[i]), "end": float(ends[i]), "label": label, "root": root, "quality": q, "beats": 1})
    # 短于 min_beats 的段并入前一段（经过与弦、过渡音造成的短暂误判）。
    if min_beats > 1:
        merged: list[dict] = []
        for s in segs:
            if merged and s["beats"] < min_beats:
                merged[-1]["end"] = s["end"]
                merged[-1]["beats"] += s["beats"]
            elif merged and merged[-1]["label"] == s["label"]:
                merged[-1]["end"], merged[-1]["beats"] = s["end"], merged[-1]["beats"] + s["beats"]
            else:
                merged.append(dict(s))
        segs = merged
    return [{"start": round(s["start"], 3), "end": round(s["end"], 3), "label": s["label"], "root": s["root"],
             "quality": s["quality"]} for s in segs]
