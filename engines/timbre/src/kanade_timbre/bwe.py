"""训练集的带宽扩展（高频补全）：测出每条音频的有效带宽，对窄带音频用 AP-BWE 生成截止频率以上的部分。

直播录屏、视频平台切片等音源常被低码率编码截掉高频（例如炫神数据集在约 7 kHz 处硬截止，之上只剩分离残留的底噪），
RVC 照着训练数据学频谱，训练出的模型同样发闷。补全只替换截止频率以上的频段，原有频段原样保留，避免改变音色主体。

AP-BWE（https://github.com/yxlu-0102/AP-BWE，代码与权重均为 MIT）是在 VCTK 朗读语音上训练的 GAN，按输入采样率分
8k / 12k / 16k / 24k → 48k 四个模型；权重取自 Hugging Face 的 safetensors 转存（rsxdalv/AP-BWE）。补出的高频是模型
推断的，不是说话人本人的齿音与气声细节，能缓解发闷，但比不上高码率的原始音源。

分两步运行：
- plan()（在 worker 中）：只用 numpy / soundfile 测带宽、选模型，不导入 torch，worker 不创建 CUDA 上下文（见 train.py）。
- main()（子进程 `python -m kanade_timbre.bwe <计划.json>`）：加载模型逐条处理，每条输出一行「bwe i/n」供进度解析。
"""

from __future__ import annotations

import json
import os
import shutil
import sys
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np

from .runtime import Asset

HR_RATE = 48000
# (模型输入的有效带宽 Hz, 模型名)：模型名中的前一个数是输入采样率，有效带宽为其一半。
MODELS = ((4000, "8kto48k"), (6000, "12kto48k"), (8000, "16kto48k"), (12000, "24kto48k"))
# 有效带宽不低于此值（Hz）的音频视为宽带、不处理：16 kHz 附近是 MP3 128 kbps 等常见编码的低通，人声在其上能量很少，
# 补全的收益小于引入伪影的风险。
WIDE_HZ = 15000
# 测带宽时判定为截止的落差（dB）：截止点下方 1 kHz 的平均电平比上方 1 kHz 高出至少这么多，且上方不再回升。
# 自然人声的平均频谱随频率平缓下降，1 kHz 内很少有超过 8 dB 的落差；炫神数据集的 7 kHz 截止处约 20 dB，
# 16 阶巴特沃斯低通约 15 dB。调高会漏掉较缓的低通，调低会把人声频谱本身的下降当作截止。
CLIFF_DB = 15.0
# 低于截止点的过渡带宽度（Hz）：其中原始音频与补全结果按频率线性交叉淡化，避免拼接处出现台阶。
CROSSOVER_HZ = 300
# 推理分块长度与两侧上下文（48 kHz 采样点，均为 hop 80 的整数倍）：AP-BWE 全卷积，感受野约 0.1 秒，
# 0.5 秒上下文足以让块边界与整段推理一致；分块把长音频的显存 / 内存占用限制在约 30 秒的量。
CHUNK = 30 * HR_RATE
CONTEXT = HR_RATE // 2
MANIFEST = "kanade-bwe.json"
_HF = "https://huggingface.co/rsxdalv/AP-BWE/resolve/main/weights"


@dataclass
class Item:
    """一条音频的处理计划。model 为空表示不补全（宽带或无法读取），原样链接到输出目录。"""

    src: str
    out: str
    bandwidth: float  # 测得的有效带宽（Hz）；0 表示无法读取
    model: str
    size: int
    mtime_ns: int


def assets(models: set[str]) -> list[Asset]:
    """所需模型的权重与配置。"""
    out = []
    for m in sorted(models):
        for f in (f"g_{m}.safetensors", "config.json"):
            out.append(Asset(f"{m}/{f}", f"rvc/models/bwe/{m}/{f}", base=_HF))
    return out


def choose_model(bandwidth: float) -> str:
    """有效带宽对应的模型：取输入带宽不超过实测带宽的最大一个，使模型的输入频段都有真实内容；
    比最窄的模型还窄时仍用最窄的（截止点以下由原始音频保留）。宽带返回空字符串。"""
    if bandwidth >= WIDE_HZ:
        return ""
    fit = [m for bw, m in MODELS if bw <= bandwidth + 200]
    return fit[-1] if fit else MODELS[0][1]


# 频谱统一插值到 0–24 kHz、间隔 GRID_HZ 的网格上，不同采样率的音频可以累加；高于奈奎斯特频率的格点为 NaN。
GRID_HZ = 25
GRID = np.arange(0, HR_RATE / 2 + GRID_HZ, GRID_HZ)
# 逐条核对时，截止点上下各 1 kHz（避开中间 ±100 Hz）的平均电平落差不小于此值（dB）才算该条也有这个截止。
# 单条短音频的频谱起伏大，阈值比 CLIFF_DB 低；自然人声在 1 kHz 内平均下降很少超过 8 dB。
FILE_DROP_DB = 8.0


@dataclass
class Spectrum:
    """一条音频较响的一半帧的平均功率谱（dB，落在 GRID 上）与帧数。只看较响的帧，避免静音段的底噪拉平频谱。"""

    db: np.ndarray
    frames: int
    nyquist: float


def spectrum(x: np.ndarray, sr: int) -> Spectrum | None:
    """x 为单声道波形；太短（不足约 0.2 秒）时返回 None。"""
    n = 2048
    if len(x) < n * 4:
        return None
    frames = np.lib.stride_tricks.sliding_window_view(x, n)[::n // 4] * np.hanning(n)
    energy = frames.std(axis=1)
    frames = frames[energy >= np.median(energy)]
    power = (np.abs(np.fft.rfft(frames, axis=1)) ** 2).mean(axis=0)
    freqs = np.fft.rfftfreq(n, 1 / sr)
    db = np.interp(GRID, freqs, 10 * np.log10(power + 1e-12), right=np.nan)
    return Spectrum(db, len(frames), sr / 2)


def find_cutoff(db: np.ndarray) -> float:
    """有效带宽（Hz）：平均频谱中第一个之上不再回升的断崖；没有断崖时为频谱的最高频率（奈奎斯特频率）。

    db 是 GRID 上的电平（高于奈奎斯特频率为 NaN）。先做 200 Hz 滑动平均去掉谐波的尖峰，再从 2 kHz 往上找
    下方 1 kHz 均值比上方 1 kHz 均值高出 CLIFF_DB、且再往上的最大值不超过下方均值 − CLIFF_DB/2 的位置。
    断崖的落差分布在一段频率上：取其后 1 kHz 内落差最大处为断崖中心，截止频率取中心以下、电平开始比断崖前
    的平台低 3 dB 的位置（宁可偏低：截止以上由模型补全，偏高会在截止附近留下原始音频衰减后的凹陷）。
    """
    valid = db[~np.isnan(db)]
    top = (len(valid) - 1) * GRID_HZ
    smooth = np.convolve(valid, np.ones(8) / 8, mode="same")
    win = 1000 // GRID_HZ
    csum = np.concatenate(([0.0], np.cumsum(smooth)))
    suffix_max = np.maximum.accumulate(smooth[::-1])[::-1]
    drop = np.full(len(smooth), -np.inf)
    for i in range(win, len(smooth) - win):
        drop[i] = (csum[i] - csum[i - win]) / win - (csum[i + win] - csum[i]) / win
    for i in range(2000 // GRID_HZ, len(smooth) - 2 * win):
        below = (csum[i] - csum[i - win]) / win
        if drop[i] >= CLIFF_DB and suffix_max[i + win] <= below - CLIFF_DB / 2:
            center = i + int(np.argmax(drop[i:i + win]))
            plateau = float(np.mean(smooth[center - win:center - win // 2]))
            edge = center - win // 2 + int(np.argmax(smooth[center - win // 2:center + 1] < plateau - 3))
            return float(edge * GRID_HZ)
    return float(top)


def has_cutoff(s: Spectrum, cutoff: float) -> bool:
    """该条音频在 cutoff 处是否也有截止（见 FILE_DROP_DB）；奈奎斯特频率本身低于 cutoff + 1 kHz 时视为有。"""
    if s.nyquist < cutoff + 1000:
        return True
    lo, hi = int(cutoff // GRID_HZ), int(1000 // GRID_HZ)
    gap = 100 // GRID_HZ
    return float(np.mean(s.db[lo - hi:lo - gap]) - np.mean(s.db[lo + gap:lo + hi])) >= FILE_DROP_DB


def _read_mono(path: Path) -> tuple[np.ndarray, int]:
    import soundfile as sf

    x, sr = sf.read(str(path), dtype="float32", always_2d=True)
    return x.mean(axis=1), sr


def plan(files: list[Path], out_dir: Path) -> list[Item]:
    """为每条音频测带宽并选模型。

    同一数据集通常来自同一音源，截止频率在全部音频的合并频谱上测（单条短音频的频谱起伏大，直接测容易把元音
    共振峰之间的低谷误判为截止）；再逐条核对，混入的宽带音频不处理。soundfile 读不了的格式（M4A、AAC 等）与
    太短的音频不补全，原样交给预处理。
    """
    specs: list[Spectrum | None] = []
    for f in files:
        try:
            specs.append(spectrum(*_read_mono(f)))
        except Exception:  # noqa: BLE001
            specs.append(None)
    # 按帧数加权合并功率（不是 dB）；各条的奈奎斯特频率不同时，每个格点只合并覆盖到它的音频。
    power = np.zeros(len(GRID))
    weight = np.zeros(len(GRID))
    for s in specs:
        if s is not None:
            ok = ~np.isnan(s.db)
            power[ok] += 10 ** (s.db[ok] / 10) * s.frames
            weight[ok] += s.frames
    cutoff = find_cutoff(np.where(weight > 0, 10 * np.log10(power / np.maximum(weight, 1) + 1e-12), np.nan)) \
        if weight.any() else 0.0
    items, used = [], set()
    for f, s in zip(files, specs):
        # 输出统一为 .wav；不同扩展名的同名文件（a.wav 与 a.mp3）用扩展名区分。
        out = f.stem if f.stem not in used else f"{f.stem}_{f.suffix[1:]}"
        used.add(out)
        bw = min(cutoff, s.nyquist) if s is not None else 0.0
        model = choose_model(bw) if s is not None and has_cutoff(s, bw) else ""
        st = f.stat()
        name = out + ".wav" if model else out + f.suffix
        items.append(Item(str(f), str(out_dir / name), round(bw), model, st.st_size, st.st_mtime_ns))
    return items


def summary(items: list[Item]) -> str:
    """进度与 kanade-train.json 中显示的一句话说明。"""
    done = [i for i in items if i.model]
    if not done:
        return "数据集为宽带音频，不需要补全高频"
    bws = sorted(i.bandwidth for i in done)
    models = sorted({i.model for i in done})
    return f"{len(done)}/{len(items)} 条音频有效带宽约 {bws[len(bws) // 2] / 1000:.1f} kHz，用 AP-BWE {'、'.join(models)} 补全到 24 kHz"


# ---------- 子进程：推理 ----------

class _Generator:
    """AP-BWE 生成器（APNet_BWE_Model），结构与参数名取自上游 models/model.py（提交 751710f），只保留推理部分。"""

    def __init__(self, weights: Path, config: dict, device):
        import torch
        from torch import nn
        from safetensors.torch import load_file

        ch, layers, bins = config["ConvNeXt_channels"], config["ConvNeXt_layers"], config["n_fft"] // 2 + 1

        class Block(nn.Module):
            def __init__(self):
                super().__init__()
                self.dwconv = nn.Conv1d(ch, ch, 7, padding=3, groups=ch)
                self.norm = nn.LayerNorm(ch, eps=1e-6)
                self.pwconv1 = nn.Linear(ch, ch * 3)
                self.act = nn.GELU()
                self.pwconv2 = nn.Linear(ch * 3, ch)
                self.gamma = nn.Parameter(torch.ones(ch))

            def forward(self, x):
                y = self.dwconv(x).transpose(1, 2)
                y = self.gamma * self.pwconv2(self.act(self.pwconv1(self.norm(y))))
                return x + y.transpose(1, 2)

        class Model(nn.Module):
            def __init__(self):
                super().__init__()
                self.conv_pre_mag = nn.Conv1d(bins, ch, 7, padding=3)
                self.norm_pre_mag = nn.LayerNorm(ch, eps=1e-6)
                self.conv_pre_pha = nn.Conv1d(bins, ch, 7, padding=3)
                self.norm_pre_pha = nn.LayerNorm(ch, eps=1e-6)
                self.convnext_mag = nn.ModuleList(Block() for _ in range(layers))
                self.convnext_pha = nn.ModuleList(Block() for _ in range(layers))
                self.norm_post_mag = nn.LayerNorm(ch, eps=1e-6)
                self.norm_post_pha = nn.LayerNorm(ch, eps=1e-6)
                self.linear_post_mag = nn.Linear(ch, bins)
                self.linear_post_pha_r = nn.Linear(ch, bins)
                self.linear_post_pha_i = nn.Linear(ch, bins)

            def forward(self, mag, pha):
                xm = self.norm_pre_mag(self.conv_pre_mag(mag).transpose(1, 2)).transpose(1, 2)
                xp = self.norm_pre_pha(self.conv_pre_pha(pha).transpose(1, 2)).transpose(1, 2)
                for bm, bp in zip(self.convnext_mag, self.convnext_pha):
                    xm = xm + xp
                    xp = xp + xm
                    xm, xp = bm(xm), bp(xp)
                mag_wb = mag + self.linear_post_mag(self.norm_post_mag(xm.transpose(1, 2))).transpose(1, 2)
                xp = self.norm_post_pha(xp.transpose(1, 2))
                pha_wb = torch.atan2(self.linear_post_pha_i(xp), self.linear_post_pha_r(xp)).transpose(1, 2)
                return mag_wb, pha_wb

        self.torch, self.device, self.cfg = torch, device, config
        self.model = Model()
        self.model.load_state_dict(load_file(str(weights)))
        self.model.to(device).eval()
        self.window = torch.hann_window(config["win_size"], device=device)

    def __call__(self, x):
        """x：48 kHz、已限带到模型输入采样率的波形（一维张量）；返回同长度的 48 kHz 宽带波形。"""
        torch, c = self.torch, self.cfg
        spec = torch.stft(x, c["n_fft"], c["hop_size"], c["win_size"], self.window, center=True, pad_mode="reflect",
                          return_complex=True)
        mag, pha = self.model(torch.log(spec.abs() + 1e-4)[None], spec.angle()[None])
        out = torch.polar(torch.exp(mag[0]), pha[0])
        return torch.istft(out, c["n_fft"], c["hop_size"], c["win_size"], self.window, center=True, length=len(x))


def _device():
    import torch

    if torch.cuda.is_available():
        return torch.device("cuda")
    if getattr(torch.backends, "mps", None) and torch.backends.mps.is_available():
        return torch.device("mps")
    return torch.device("cpu")


def extend(gen: _Generator, x48: np.ndarray, bandwidth: float) -> np.ndarray:
    """补全一条 48 kHz 单声道音频：模型输出只取 bandwidth 以上的频段，以下保留原始音频（CROSSOVER_HZ 过渡带交叉淡化）。"""
    import soxr

    torch = gen.torch
    lr_rate = int(gen.cfg["lr_sampling_rate"])
    # 与上游 inference_48k.py 相同：先降到模型输入采样率再升回 48 kHz，使输入频段与训练时一致。
    lr = soxr.resample(soxr.resample(x48, HR_RATE, lr_rate), lr_rate, HR_RATE)[: len(x48)]
    lr = np.pad(lr, (0, len(x48) - len(lr)))
    wide = np.empty_like(x48)
    with torch.inference_mode():
        for s in range(0, len(lr), CHUNK):
            a, b = max(0, s - CONTEXT), min(len(lr), s + CHUNK + CONTEXT)
            seg = torch.from_numpy(np.ascontiguousarray(lr[a:b])).to(gen.device)
            y = gen(seg).float().cpu().numpy()
            wide[s:min(len(lr), s + CHUNK)] = y[s - a: s - a + min(CHUNK, len(lr) - s)]
    # 频域拼接：n_fft 2048 在 48 kHz 下每个频点约 23 Hz，过渡带有十余个频点。
    n, hop = 2048, 512
    win = torch.hann_window(n)
    X = torch.stft(torch.from_numpy(x48), n, hop, window=win, return_complex=True)
    Y = torch.stft(torch.from_numpy(wide), n, hop, window=win, return_complex=True)
    freqs = torch.fft.rfftfreq(n, 1 / HR_RATE)
    w = ((freqs - (bandwidth - CROSSOVER_HZ)) / CROSSOVER_HZ).clamp(0, 1)[:, None]
    out = torch.istft(X * (1 - w) + Y * w, n, hop, window=win, length=len(x48)).numpy()
    peak = float(np.abs(out).max())
    # 补出的高频可能把峰值推过满刻度：整条按比例压回，避免写入 16 bit 时削波。
    return out * (0.999 / peak) if peak > 0.999 else out


def _link(src: Path, dst: Path) -> None:
    dst.unlink(missing_ok=True)
    try:
        os.link(src, dst)
    except OSError:
        shutil.copy2(src, dst)


def run(plan_path: Path, models_dir: Path) -> None:
    """按计划处理，输出到计划中的路径。输出目录中的 MANIFEST 记录每条输出对应的源文件与模型，
    源文件、模型都未变化且输出仍在时跳过（重新训练同一数据集时不重复推理）。"""
    import soundfile as sf
    import soxr

    items = [Item(**d) for d in json.loads(plan_path.read_text(encoding="utf-8"))]
    out_dir = Path(items[0].out).parent if items else plan_path.parent
    out_dir.mkdir(parents=True, exist_ok=True)
    manifest_path = out_dir / MANIFEST
    manifest = json.loads(manifest_path.read_text(encoding="utf-8")) if manifest_path.is_file() else {}
    keep = {Path(i.out).name for i in items} | {MANIFEST}
    for f in out_dir.iterdir():
        if f.name not in keep:
            f.unlink()
    device = _device()
    print(f"bwe device={device}", flush=True)
    gens: dict[str, _Generator] = {}
    for n, it in enumerate(items, 1):
        out = Path(it.out)
        key = Path(it.out).name
        sig = {k: v for k, v in asdict(it).items() if k != "out"}
        if manifest.get(key) == sig and out.is_file():
            print(f"bwe {n}/{len(items)} cached", flush=True)
            continue
        if not it.model:
            _link(Path(it.src), out)
        else:
            if it.model not in gens:
                d = models_dir / it.model
                cfg = json.loads((d / "config.json").read_text(encoding="utf-8"))
                gens[it.model] = _Generator(d / f"g_{it.model}.safetensors", cfg, device)
            x, sr = _read_mono(Path(it.src))
            x48 = soxr.resample(x, sr, HR_RATE) if sr != HR_RATE else x
            sf.write(str(out), extend(gens[it.model], x48.astype(np.float32), it.bandwidth), HR_RATE, "PCM_16")
        manifest[key] = sig
        print(f"bwe {n}/{len(items)} {Path(it.src).name}", flush=True)
    manifest = {k: v for k, v in manifest.items() if k in keep}
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=1), encoding="utf-8")


if __name__ == "__main__":
    run(Path(sys.argv[1]), Path(sys.argv[2]))
