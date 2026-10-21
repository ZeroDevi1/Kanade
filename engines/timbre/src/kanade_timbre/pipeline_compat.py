"""修正 Applio 长音频分段转换时的累积时间漂移。

Pipeline.pipeline 把超过 x_max 秒的输入在静音处切成约 x_center 秒的段，每段前后各带 t_pad（1 秒）上下文，
转换后固定裁掉两端 t_pad_tgt 再拼接。段的输出长度由 voice_conversion 的
p_len = min(len(audio0) // window, ContentVec 帧数 × 2) 决定：ContentVec 步长 320、感受野 400，
段长为 (t - s) + 2·t_pad + window 时，帧数为偶数的段比 len // window 少 2 帧，裁剪后比应保留的 (t - s) 短 1 帧（10 ms）。
误差逐段累积：4 分半的歌在 120 秒后人声提前约 30 ms（2026-10-21《我不想念》阿梓 40k 实测；炫狗 48k 为 20 ms）。

这里包装 voice_conversion，把每段输出补零 / 截断到 (len(audio0) // window - 1) 帧：裁掉两端 t_pad_tgt 后恰好保留
(t - s) 对应的长度。补的零只落在随后被裁掉的尾部上下文里（缺口最多 2 帧，远小于 t_pad_tgt），不进入成品。
最后一段（audio_pad[t:]，不带额外的 window）因此在全曲末尾少 1 帧，不影响之前的对齐。

vendor 中的 Applio 代码不修改（见 vendor/VENDOR.md）；install() 须在导入 rvc.infer 之后、转换之前调用。
升级 Applio 后若上游改了分段或 p_len 的算法，需重新核对这里的期望长度。
"""

from __future__ import annotations

import numpy as np


def expected_len(n_in: int, window: int, tgt_sr: int, sample_rate: int) -> int:
    """段输入 n_in 个采样（16 kHz）时，voice_conversion 应返回的输出采样数。"""
    return (n_in // window - 1) * (tgt_sr * window // sample_rate)


def fit(out: np.ndarray, n: int) -> np.ndarray:
    """把一维音频补零或截断到 n 个采样。"""
    if len(out) >= n:
        return out[:n]
    return np.concatenate([out, np.zeros(n - len(out), dtype=out.dtype)])


def install() -> None:
    from rvc.infer.pipeline import Pipeline

    if getattr(Pipeline, "_kanade_aligned", False):
        return
    original = Pipeline.voice_conversion

    def voice_conversion(self, model, net_g, sid, audio0, *args, **kwargs):
        out = original(self, model, net_g, sid, audio0, *args, **kwargs)
        return fit(out, expected_len(len(audio0), self.window, self.tgt_sr, self.sample_rate))

    Pipeline.voice_conversion = voice_conversion
    Pipeline._kanade_aligned = True
