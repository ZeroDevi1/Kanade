"""Runs only inside the user-provisioned upstream Python environment.

API verified against ASLP-lab/YingMusic-Singer-Plus at
baa409c2e7e5e775f09b4e92a220808f4827d2cc. No upstream CLI/download helper is invoked.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import random
import sys


def mix_audio(vocal, accompaniment, torch):
    """Pad the shorter track instead of truncating the original accompaniment."""
    if vocal.shape[0] != accompaniment.shape[0]:
        channels = max(vocal.shape[0], accompaniment.shape[0])
        if vocal.shape[0] == 1:
            vocal = vocal.expand(channels, -1)
        if accompaniment.shape[0] == 1:
            accompaniment = accompaniment.expand(channels, -1)
        if vocal.shape[0] != accompaniment.shape[0]:
            raise ValueError("混音只支持单声道或立体声")
    length = max(vocal.shape[1], accompaniment.shape[1])
    mixed = torch.zeros((vocal.shape[0], length), dtype=vocal.dtype)
    mixed[:, :vocal.shape[1]] += vocal
    mixed[:, :accompaniment.shape[1]] += accompaniment
    peak = mixed.abs().max()
    if peak > 1:
        mixed /= peak
    return mixed


def execute(p):
    for key in ("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE", "HF_DATASETS_OFFLINE"):
        os.environ[key] = "1"
    source = Path(p["source_dir"]).resolve()
    if Path.cwd().resolve() != source:
        raise ValueError("Worker cwd must be the upstream source checkout")
    sys.path.insert(0, str(source))
    model_dir = Path(p["model_dir"])
    config = json.loads((model_dir / "config.json").read_text(encoding="utf-8"))
    for key in ("model_cfg_path", "vae_config_path"):
        if not config.get(key) or not Path(config[key]).is_file():
            raise ValueError(f"模型配置的 {key} 不存在；请使用匹配的 upstream 源码和模型")
    import torch
    import torchaudio
    from src.YingMusicSinger.infer.YingMusicSinger import YingMusicSinger

    device = "cuda:0" if torch.cuda.is_available() else "cpu"
    if p.get("doctor"):
        return {"message": f"YingMusic 离线配置可读 · torch {torch.__version__} · {device}（尚未加载模型或验证推理）"}
    # Fail before allocating the large model on corrupt/unsupported audio.
    for key in ("melody_audio", "ref_audio", "accompaniment_audio"):
        if not p.get(key):
            continue
        audio, sr = torchaudio.load(p[key])
        if sr <= 0 or audio.ndim != 2 or audio.shape[0] not in (1, 2) or audio.shape[1] == 0:
            raise ValueError(f"{key} 需要非空单声道或立体声音频")
        if not torch.isfinite(audio).all():
            raise ValueError(f"{key} 含无效采样值")
    model = YingMusicSinger.from_pretrained(str(model_dir), local_files_only=True).to(device)
    model.eval()
    seed = p["seed"] if p["seed"] >= 0 else random.SystemRandom().randrange(2**31)
    with torch.inference_mode():
        vocal, sr = model(ref_audio_path=p["ref_audio"], melody_audio_path=p["melody_audio"],
                          ref_text=p["ref_text"], target_text=p["target_text"],
                          lrc_align_mode="sentence_level", sil_len_to_end=0.5, t_shift=0.5,
                          nfe_step=p["nfe_step"], cfg_strength=p["cfg_strength"], seed=seed)
    vocal = vocal.to("cpu")
    if sr != 44100 or vocal.ndim != 2 or not vocal.shape[1] or not torch.isfinite(vocal).all():
        raise ValueError("模型返回了无效音频")
    output = Path(p["output_dir"])
    # Atomic replace prevents a failed save from presenting a partial WAV as successful.
    def save(name, audio):
        tmp = output / (name + ".tmp.wav")
        try:
            torchaudio.save(str(tmp), audio, sample_rate=sr)
            os.replace(tmp, output / name)
        finally:
            tmp.unlink(missing_ok=True)
    save("vocal.wav", vocal)
    if p.get("accompaniment_audio"):
        accompaniment, accomp_sr = torchaudio.load(p["accompaniment_audio"])
        if accomp_sr != sr:
            accompaniment = torchaudio.functional.resample(accompaniment, accomp_sr, sr)
        save("mixed.wav", mix_audio(vocal, accompaniment, torch))
    return {"seed": seed, "sample_rate": sr}


def main():
    p = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    result = execute(p)
    Path(p["result_path"]).write_text(json.dumps(result, ensure_ascii=False), encoding="utf-8")


if __name__ == "__main__":
    main()
