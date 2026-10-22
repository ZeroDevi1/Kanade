"""模型文件的下载与定位。

- beat_this：CPJKU 发布的 final0 检查点（在除 GTZAN 外的全部数据上训练，上游默认），约 81 MB。
  上游通过 torch.hub 下载到用户缓存目录，这里改用 SDK 的断点续传下载到 MODEL_DIR，再以本地路径加载。
- clap：laion/clap-htsat-unfused（Apache-2.0，LAION 官方转换的 CLAP），约 615 MB；固定到提交以保证可复现，
  经 KANADE_HF_ENDPOINT 可走镜像。未用音乐专用的 laion/larger_clap_music：它在 transformers 中的权重 logit_scale
  约为 1、音频与文本嵌入几乎正交（2026-10-21 用真实歌曲实测相似度均在 ±0.02 内），零样本分类退化为均匀分布。
- btc：BTC-ISMIR19 的大词汇量和弦模型 btc_model_large_voca.pt（MIT），约 12 MB，固定到上游提交；
  经 KANADE_GITHUB_MIRROR 可走镜像。
- some：openvpi/SOME 的 v1.0.0-baseline 权重（MIT），GitHub release 上的 zip（约 435 MB），下载后解压并删除 zip；
  经 KANADE_GITHUB_MIRROR 可走镜像。
- lyrics：jonatasgrosman/wav2vec2-large-xlsr-53-chinese-zh-cn（Apache-2.0，中文字级 CTC），约 1.28 GB，固定到提交；
  用于把 LRC 歌词逐字对齐到主唱（见 lyrics.py），经 KANADE_HF_ENDPOINT 可走镜像。
- game：openvpi/GAME 的 GAME-1.0-medium（约 50M 参数），GitHub release v1.0.0 上的 zip（约 185 MB），下载后解压并删除 zip；
  经 KANADE_GITHUB_MIRROR 可走镜像。权重为 CC BY-NC-SA 4.0（非商用），只在用户选择 GAME 连音时下载。
  取 medium 作为体积与精度的折中（small 约 47 MB、large 约 366 MB）。
- muscriptor：MuScriptor/muscriptor-medium（307M 参数，多乐器转录，见 transcribe.py），约 1.23 GB，固定到提交。
  权重为 CC BY-NC 4.0（非商用），仓库门控：须先在 Hugging Face 页面登录并接受许可，再在设置中填写访问令牌
  （SDK 经 KANADE_HF_TOKEN 带上鉴权头）；只在用户开启多乐器转录时下载。取 medium 是上游默认的速度与精度折中
  （small 103M 适合纯 CPU，large 1.4B 需要 GPU）。
"""

from __future__ import annotations

import zipfile
from dataclasses import dataclass
from pathlib import Path

from kanade_engine import Job
from kanade_engine.download import HTTPStatusError, fetch

from . import MODEL_DIR


@dataclass(frozen=True)
class Asset:
    url: str
    local: str  # 相对 MODEL_DIR
    size: int = 0  # 已知大小（字节），用于进度；0 为以响应头为准
    # unpacks 非空时 local 是 zip：下载后解压到其所在目录并删除 zip，以 unpacks（相对 MODEL_DIR）是否存在判断已安装。
    unpacks: str = ""

    @property
    def done(self) -> str:
        return self.unpacks or self.local


BEAT_THIS = [Asset("https://cloud.cp.jku.at/public.php/dav/files/7ik4RrBKTS273gp/final0.ckpt",
                   "beat_this/final0.ckpt", 81058141)]

_CLAP_REPO = "https://huggingface.co/laion/clap-htsat-unfused/resolve/8fa0f1c6d0433df6e97c127f64b2a1d6c0dcda8a"
CLAP_DIR = "clap/clap-htsat-unfused"
CLAP = [Asset(f"{_CLAP_REPO}/{f}", f"{CLAP_DIR}/{f}", size) for f, size in (
    ("config.json", 5390), ("preprocessor_config.json", 541), ("tokenizer_config.json", 384),
    ("special_tokens_map.json", 280), ("vocab.json", 798293), ("merges.txt", 456356),
    ("tokenizer.json", 2108746), ("pytorch_model.bin", 614525833),
)]

SOME_CKPT = "some/0119_continuous256_5spk/model_ckpt_steps_100000_simplified.ckpt"
SOME = [Asset("https://github.com/openvpi/SOME/releases/download/v1.0.0-baseline/0119_continuous128_5spk.zip",
              "some/some.zip", 435401333, unpacks=SOME_CKPT)]

BTC_CKPT = "btc/btc_model_large_voca.pt"
BTC = [Asset("https://github.com/jayg996/BTC-ISMIR19/raw/2682317be668032e6e4b269ded36adaa2ad57df0/test/btc_model_large_voca.pt",
             BTC_CKPT, 12229576)]

_LYRICS_REPO = "https://huggingface.co/jonatasgrosman/wav2vec2-large-xlsr-53-chinese-zh-cn/resolve/99ccb2737be22b8bb50dcfcc39ad4d567fb90cfd"
LYRICS_DIR = "lyrics/wav2vec2-large-xlsr-53-chinese-zh-cn"
LYRICS = [Asset(f"{_LYRICS_REPO}/{f}", f"{LYRICS_DIR}/{f}", size) for f, size in (
    ("config.json", 1567), ("preprocessor_config.json", 158), ("special_tokens_map.json", 85),
    ("vocab.json", 44357), ("pytorch_model.bin", 1276296151),
)]

GAME_DIR = "game/GAME-1.0-medium"
GAME = [Asset("https://github.com/openvpi/GAME/releases/download/v1.0.0/GAME-1.0-medium.zip",
              "game/game.zip", 184550485, unpacks=f"{GAME_DIR}/model.pt")]

MUSCRIPTOR_PAGE = "https://huggingface.co/MuScriptor/muscriptor-medium"
_MUSCRIPTOR_REPO = f"{MUSCRIPTOR_PAGE}/resolve/f32236969308476e01fd3aae67357de5feb05a2d"
MUSCRIPTOR_DIR = "muscriptor/muscriptor-medium"
# 加载器从权重旁的 config.json 读取模型结构，两个文件须在同一目录。
MUSCRIPTOR_WEIGHTS = f"{MUSCRIPTOR_DIR}/model.safetensors"
MUSCRIPTOR = [Asset(f"{_MUSCRIPTOR_REPO}/{f}", f"{MUSCRIPTOR_DIR}/{f}", size) for f, size in (
    ("config.json", 126), ("model.safetensors", 1228144472),
)]

GROUPS = {"beat_this": BEAT_THIS, "clap": CLAP, "some": SOME, "btc": BTC, "lyrics": LYRICS, "game": GAME,
          "muscriptor": MUSCRIPTOR}
# 需要 Hugging Face 登录并接受许可的模型组：401 / 403 时提示用户去接受许可并填写令牌。
GATED = {"muscriptor": MUSCRIPTOR_PAGE}


def path(asset_local: str) -> Path:
    return MODEL_DIR / asset_local


def installed(group: str) -> bool:
    return all(path(a.done).is_file() for a in GROUPS[group])


def ensure(job: Job, groups: list[str], lo: float, hi: float) -> None:
    """下载 groups 中缺少的文件，进度映射到 [lo, hi]（按字节）。"""
    missing = [a for g in groups for a in GROUPS[g] if not path(a.done).is_file()]
    total = sum(a.size for a in missing) or 1
    done = 0
    for a in missing:
        def report(have: int, size: int, a=a, base=done) -> None:
            frac = (base + have) / total
            job.progress(lo + (hi - lo) * min(frac, 1.0), f"下载 {Path(a.local).name}：{have / 1e6:.0f} / {(size or a.size) / 1e6:.0f} MB")

        try:
            fetch(a.url, path(a.local), report, a.size)
        except HTTPStatusError as exc:
            page = next((GATED[g] for g in groups if g in GATED and a in GROUPS[g]), "")
            if page and exc.code in (401, 403):
                raise RuntimeError(f"下载 {Path(a.local).name} 需要授权：请先登录 {page} 接受许可，"
                                   "再在设置中填写 Hugging Face 访问令牌（Read 权限即可）") from exc
            raise
        if a.unpacks:
            job.progress(job.fraction, f"解压 {Path(a.local).name}")
            with zipfile.ZipFile(path(a.local)) as z:
                z.extractall(path(a.local).parent)
            path(a.local).unlink()
            if not path(a.unpacks).is_file():
                raise RuntimeError(f"{Path(a.local).name} 中缺少 {a.unpacks}")
        done += a.size


def run_download(job: Job) -> None:
    # 未指定时下载全部不需要授权的模型；门控模型须显式指定（没有令牌时必然失败）。
    groups = job.params.get("models") or [g for g in GROUPS if g not in GATED]
    unknown = [g for g in groups if g not in GROUPS]
    if unknown:
        raise ValueError(f"未知模型：{unknown}")
    ensure(job, groups, 0.0, 1.0)
    job.progress(1.0, "下载完成")
