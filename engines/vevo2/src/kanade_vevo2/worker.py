"""Local-only AR+FM worker for Amphion 26f6883110181f1dbfe95c70a7c7dbaf4de5f42a.

The official wrapper downloads an entire HF snapshot; never import/call it. The
upstream utility hardcodes Whisper 'medium', so redirect only that loader inside
this disposable process to an explicit local checkpoint, then restore it.
"""
from __future__ import annotations

from contextlib import contextmanager
import json
import os
from pathlib import Path
import random
import sys

AR = "contentstyle_modeling/posttrained"
FM = "acoustic_modeling/fm_emilia101k_singnet7k_repa"


def mix_audio(vocal, accompaniment, torch):
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


def pipeline_paths(model_dir):
    root = Path(model_dir)
    return dict(prosody_tokenizer_ckpt_path=str(root / "tokenizer/prosody_fvq512_6.25hz"),
                content_style_tokenizer_ckpt_path=str(root / "tokenizer/contentstyle_fvq16384_12.5hz"),
                ar_cfg_path=str(root / AR / "amphion_config.json"), ar_ckpt_path=str(root / AR),
                fmt_cfg_path=str(root / FM / "config.json"), fmt_ckpt_path=str(root / FM),
                vocoder_cfg_path=str(root / "vocoder/config.json"), vocoder_ckpt_path=str(root / "vocoder"))


@contextmanager
def local_loaders(upstream, whisper, p):
    """Constrain loader changes to the isolated child and restore even on failure."""
    old_whisper, old_ar, old_config = whisper.load_model, upstream.build_ar_model, upstream.load_config
    fm_path = (Path(p["model_dir"]) / FM / "config.json").resolve()
    ar_path = (Path(p["model_dir"]) / AR).resolve()

    def whisper_local(name, *args, **kwargs):
        if name != "medium":
            raise ValueError(f"不允许自动下载 Whisper 模型：{name}")
        checkpoint = Path(p["whisper_path"]).resolve()
        if not checkpoint.is_file():
            raise ValueError("本地 Whisper medium.pt 不存在")
        return old_whisper(str(checkpoint), *args, **kwargs)

    def ar_local(path, device):
        if Path(path).resolve() != ar_path:
            raise ValueError("AR 模型必须使用已配置的本地目录")
        # Official checkpoint architecture is built-in Qwen2ForCausalLM; never
        # fetch or execute model-side custom Python code.
        model = upstream.AutoModelForCausalLM.from_pretrained(
            str(ar_path), device_map=device, torch_dtype="auto",
            local_files_only=True, trust_remote_code=False)
        model.eval()
        return model

    def config_local(path, *args, **kwargs):
        cfg = old_config(path, *args, **kwargs)  # upstream JSON5 parser, not json.loads
        if Path(path).resolve() == fm_path:
            if getattr(cfg.model.flow_matching_transformer, "use_text_as_condition", False):
                raise ValueError("仅支持官方非 text-conditioned FM 配置；不加载外部 tokenizer")
            cfg.model.coco.whisper_stats_path = p["whisper_stats_path"]
        return cfg

    whisper.load_model, upstream.build_ar_model, upstream.load_config = whisper_local, ar_local, config_local
    try:
        yield
    finally:
        whisper.load_model, upstream.build_ar_model, upstream.load_config = old_whisper, old_ar, old_config


def execute(p):
    for key in ("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE", "HF_DATASETS_OFFLINE"):
        os.environ[key] = "1"
    source = Path(p["source_dir"]).resolve()
    if Path.cwd().resolve() != source:
        raise ValueError("Worker cwd must be the Amphion checkout")
    sys.path.insert(0, str(source))
    import torch
    import torchaudio
    import whisper
    from models.svc.vevo2 import vevo2_utils as upstream

    paths = pipeline_paths(p["model_dir"])
    device = torch.device("cuda") if torch.cuda.is_available() else torch.device("cpu")
    with local_loaders(upstream, whisper, p):
        # This reads JSON5 and resolves the hardcoded statistics path without
        # allocating any model. Doctor deliberately does not claim inference QA.
        upstream.load_config(paths["fmt_cfg_path"])
        if p.get("doctor"):
            return {"message": f"Vevo2 本地配置及依赖可读 · {device}（未加载权重，未验证推理）"}
        for key in ("source_audio", "accompaniment_audio"):
            if not p.get(key):
                continue
            audio, sr = torchaudio.load(p[key])
            if sr <= 0 or audio.ndim != 2 or audio.shape[0] not in (1, 2) or not audio.shape[1] or not torch.isfinite(audio).all():
                raise ValueError(f"{key} 需要有效非空单声道或立体声音频")
        random.seed(p["seed"])
        torch.manual_seed(p["seed"])
        import numpy as np
        np.random.seed(p["seed"])
        pipeline = upstream.Vevo2InferencePipeline(**paths, device=device, use_vllm=False)
        with torch.inference_mode():
            vocal = pipeline.inference_ar_and_fm(
                target_text=p["target_text"], prosody_wav_path=p["source_audio"],
                style_ref_wav_path=p["source_audio"], style_ref_wav_text=p["original_text"],
                timbre_ref_wav_path=p["source_audio"], use_prosody_code=True,
                use_pitch_shift=False, flow_matching_steps=p["flow_matching_steps"], display_audio=False)
    vocal = vocal.detach().to("cpu")
    if vocal.ndim != 2 or vocal.shape[0] not in (1, 2) or not vocal.shape[1] or not torch.isfinite(vocal).all():
        raise ValueError("Vevo2 返回无效音频")
    peak = vocal.abs().max()
    if peak > 1:
        vocal = vocal / peak
    output = Path(p["output_dir"])
    def save(name, audio):
        tmp = output / (name + ".tmp.wav")
        try:
            torchaudio.save(str(tmp), audio, sample_rate=24000)
            os.replace(tmp, output / name)
        finally:
            tmp.unlink(missing_ok=True)
    save("vocal.wav", vocal)
    if p.get("accompaniment_audio"):
        accompaniment, sr = torchaudio.load(p["accompaniment_audio"])
        if sr != 24000:
            accompaniment = torchaudio.functional.resample(accompaniment, sr, 24000)
        save("mixed.wav", mix_audio(vocal, accompaniment, torch))
    return {"sample_rate": 24000, "seed": p["seed"]}


def main():
    p = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    result = execute(p)
    Path(p["result_path"]).write_text(json.dumps(result, ensure_ascii=False), encoding="utf-8")


if __name__ == "__main__":
    main()
