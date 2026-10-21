"""情绪识别：CLAP（laion/clap-htsat-unfused）的零样本分类。

把整首歌切成 WINDOW_S 秒的片段，求每段音频嵌入与各情绪描述文本嵌入的相似度，softmax 得到每段的情绪分布，
再取平均作为整首的结果；每段的最高项构成情绪时间线。效价（积极—消极）与唤醒度（激烈—舒缓）用成对的
描述做二选一，取「积极」「激烈」一侧的概率（0–1）。
"""

from __future__ import annotations

import numpy as np

from kanade_engine import Job, pick_device

from . import models

# CLAP 的输入为 48 kHz、10 秒（模型配置的 max_length_s）。
SR = 48000
WINDOW_S = 10.0
# 最多分析的片段数：超过时在全曲均匀抽取，控制长曲在 CPU 上的耗时（每段约 0.3–1 秒）。
MAX_WINDOWS = 30

# 情绪标签：键、中文名、英文描述（CLAP 以英文训练）。
MOODS = [
    ("happy", "欢快", "happy, cheerful, upbeat music"),
    ("sad", "悲伤", "sad, melancholic, sorrowful music"),
    ("calm", "平静", "calm, peaceful, relaxing music"),
    ("energetic", "激昂", "energetic, powerful, exciting music"),
    ("romantic", "温柔", "romantic, tender, sweet music"),
    ("dark", "阴郁", "dark, tense, ominous music"),
    ("nostalgic", "怀旧", "nostalgic, wistful, sentimental music"),
    ("angry", "愤怒", "angry, aggressive, intense music"),
    ("dreamy", "梦幻", "dreamy, ethereal, atmospheric music"),
    ("epic", "宏大", "epic, grand, cinematic music"),
]
VALENCE = ("positive, happy, bright music", "negative, sad, dark music")
AROUSAL = ("high energy, intense, fast music", "low energy, soft, slow music")


def _windows(y: np.ndarray) -> list[tuple[float, np.ndarray]]:
    n = int(WINDOW_S * SR)
    starts = list(range(0, max(len(y) - n // 2, 1), n))
    if len(starts) > MAX_WINDOWS:
        starts = [starts[int(i)] for i in np.linspace(0, len(starts) - 1, MAX_WINDOWS)]
    return [(s / SR, y[s:s + n]) for s in starts]


def analyze(job: Job, y_mono: np.ndarray, sr: int, lo: float, hi: float) -> dict:
    """y_mono 为单声道音频。返回 {moods, valence, arousal, segments}。"""
    import soxr
    import torch
    from transformers import ClapModel, ClapProcessor

    dev = pick_device(allow_mps=False)
    job.progress(lo, f"加载 CLAP（{dev.label}）")
    model_dir = str(models.path(models.CLAP_DIR))
    model = ClapModel.from_pretrained(model_dir).to(dev.name).eval()
    proc = ClapProcessor.from_pretrained(model_dir)

    y = soxr.resample(y_mono, sr, SR) if sr != SR else y_mono
    wins = _windows(y.astype(np.float32))
    texts = [m[2] for m in MOODS] + list(VALENCE) + list(AROUSAL)
    def features(out) -> torch.Tensor:
        # transformers 5 的 get_*_features 返回输出对象，投影并归一化后的嵌入在 pooler_output；4.x 直接返回张量。
        x = out.pooler_output if hasattr(out, "pooler_output") else out
        return torch.nn.functional.normalize(x, dim=-1)

    with torch.inference_mode():
        t_in = proc(text=texts, return_tensors="pt", padding=True).to(dev.name)
        t_emb = features(model.get_text_features(**t_in))
        a_embs = []
        for i, (_, w) in enumerate(wins):
            job.check_cancelled()
            job.progress(lo + (hi - lo) * (0.1 + 0.9 * i / len(wins)), f"情绪识别 {i + 1}/{len(wins)}")
            a_in = proc(audio=[w], sampling_rate=SR, return_tensors="pt").to(dev.name)
            a_embs.append(features(model.get_audio_features(**a_in)))
        a_emb = torch.cat(a_embs)
        logits = (a_emb @ t_emb.T * model.logit_scale_a.exp()).cpu().numpy()  # (片段数, 文本数)

    def softmax(x: np.ndarray) -> np.ndarray:
        e = np.exp(x - x.max(axis=1, keepdims=True))
        return e / e.sum(axis=1, keepdims=True)

    k = len(MOODS)
    mood_p = softmax(logits[:, :k])
    val_p = softmax(logits[:, k:k + 2])[:, 0]
    aro_p = softmax(logits[:, k + 2:k + 4])[:, 0]
    mean = mood_p.mean(axis=0)
    order = np.argsort(-mean)
    segs = []
    for (start, w), p in zip(wins, mood_p):
        top = MOODS[int(np.argmax(p))][1]
        end = start + len(w) / SR
        if segs and segs[-1]["label"] == top and abs(segs[-1]["end"] - start) < 1e-3:
            segs[-1]["end"] = round(end, 2)
        else:
            segs.append({"start": round(start, 2), "end": round(end, 2), "label": top})
    return {
        "moods": [{"key": MOODS[i][0], "label": MOODS[i][1], "score": round(float(mean[i]), 4)} for i in order],
        "valence": round(float(val_p.mean()), 3),
        "arousal": round(float(aro_p.mean()), 3),
        "segments": segs,
        "windows": len(wins),
    }
