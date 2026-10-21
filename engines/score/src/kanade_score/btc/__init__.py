"""BTC（Bi-directional Transformer for Chord recognition，Park et al., ISMIR 2019）大词汇量和弦识别的推理。

模型定义 vendor 自 https://github.com/jayg996/BTC-ISMIR19（MIT，见 model.py 头部与同目录 LICENSE），
权重为上游发布的 btc_model_large_voca.pt（约 12 MB）：在 Isophonics、RWC、uspop、Robbie Williams 等标注上训练，
输出 170 类：12 个根音 × 14 种性质（maj、min、dim、aug、6、m6、7、maj7、m7、mMaj7、dim7、m7b5、sus2、sus4），
外加 N（无和弦）与 X（无法归类）。

特征与上游 audio_file_to_features 一致：22.05 kHz 单声道，每 10 秒一段求 CQT（144 bin、每八度 24 bin、hop 2048），
取 log(|CQT| + 1e-6)，用检查点里的均值 / 标准差归一化；每段 10 秒正好 108 帧，与模型的 timestep 对齐。
上游逐帧取 argmax；这里返回逐帧概率，由 harmony.decode_btc 在节拍上聚合并平滑。
"""

from __future__ import annotations

import numpy as np

SR = 22050
N_BINS = 144
BINS_PER_OCTAVE = 24
HOP = 2048
INST_LEN = 10.0  # 秒，上游 run_config.yaml 的 mp3.inst_len
CONFIG = {
    "feature_size": 144, "hidden_size": 128, "num_layers": 8, "num_heads": 4, "total_key_depth": 128,
    "total_value_depth": 128, "filter_size": 128, "timestep": 108, "input_dropout": 0.2, "layer_dropout": 0.2,
    "attention_dropout": 0.2, "relu_dropout": 0.2, "num_chords": 170, "probs_out": False,
}
# 上游 quality_list 的顺序（类别号 = 根音 × 14 + 性质序号），映射为 harmony.QUALITIES 的键。
QUALITY_KEYS = ["min", "maj", "dim", "aug", "min6", "6", "min7", "minmaj7", "maj7", "7", "dim7", "hdim7", "sus2", "sus4"]
X_INDEX, N_INDEX = 168, 169
FRAME_S = INST_LEN / CONFIG["timestep"]  # 每帧秒数（约 0.0926）


def features(y_mono: np.ndarray, sr: int) -> np.ndarray:
    """返回 (帧数, 144) 的 log-CQT，分段方式与上游一致。"""
    import librosa
    import soxr

    y = soxr.resample(y_mono, sr, SR).astype(np.float32) if sr != SR else y_mono.astype(np.float32)
    step = int(SR * INST_LEN)
    parts = []
    for start in range(0, max(len(y), 1), step):
        seg = y[start:start + step]
        if len(seg) == 0:
            break
        parts.append(librosa.cqt(seg, sr=SR, n_bins=N_BINS, bins_per_octave=BINS_PER_OCTAVE, hop_length=HOP))
    return np.log(np.abs(np.concatenate(parts, axis=1)) + 1e-6).T


def frame_probs(y_mono: np.ndarray, sr: int, ckpt: str, device: str) -> np.ndarray:
    """返回 (帧数, 170) 的逐帧和弦概率；第 i 帧起于 i × FRAME_S 秒。"""
    import torch

    from .model import BTC_model

    from numpy._core import multiarray

    # 检查点是 PyTorch 1.x 的旧格式，mean / std 以 numpy 标量保存：只放行 numpy 标量与 dtype 的重建，
    # 仍以 weights_only 加载，不执行任意代码。pickle 里记录的是 NumPy 1 的模块路径 numpy.core.multiarray。
    allowed = [(multiarray.scalar, "numpy.core.multiarray.scalar"), np.dtype, np.dtypes.Float64DType, np.dtypes.Float32DType]
    with torch.serialization.safe_globals(allowed):
        state = torch.load(ckpt, map_location=device, weights_only=True)
    model = BTC_model(CONFIG).to(device).eval()
    model.load_state_dict(state["model"], strict=True)
    feat = (features(y_mono, sr) - state["mean"]) / state["std"]
    n = CONFIG["timestep"]
    frames = feat.shape[0]
    feat = np.pad(feat, ((0, (-frames) % n), (0, 0)))
    x = torch.tensor(feat, dtype=torch.float32, device=device).unsqueeze(0)
    out = []
    with torch.no_grad():
        for t in range(feat.shape[0] // n):
            hidden, _ = model.self_attn_layers(x[:, t * n:(t + 1) * n, :])
            out.append(torch.softmax(model.output_layer.output_projection(hidden), dim=-1)[0].cpu().numpy())
    return np.concatenate(out, axis=0)[:frames]
