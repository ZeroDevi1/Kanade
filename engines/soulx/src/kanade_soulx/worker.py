"""Real SoulX-Singer SVS API, verified against 81aeb3ae772c70093c3de74dc23c92d983801ae4.

Only a trusted local model.pt is loaded. Upstream build_model uses torch.load
(weights_only=False); do not use untrusted checkpoints. No SVC or preprocessor.
"""
import copy
import json
import os
from pathlib import Path
import sys


def validate_prompt_audio(sf, np, path, duration):
    info = sf.info(path)
    if info.channels not in (1, 2) or info.samplerate <= 0 or info.duration + .1 < duration:
        raise ValueError("参考干声须为单/双声道且覆盖完整参考元数据段")
    audio, rate = sf.read(path, frames=int(duration * info.samplerate), dtype="float32", always_2d=True)
    if rate != info.samplerate or not audio.size or not np.isfinite(audio).all():
        raise ValueError("参考干声包含空数据或非有限采样值")


def execute(p):
    for key in ("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE", "HF_DATASETS_OFFLINE"):
        os.environ[key] = "1"
    source = Path(p["source_dir"]).resolve()
    if Path.cwd().resolve() != source:
        raise ValueError("SoulX worker cwd must be the upstream checkout")
    sys.path.insert(0, str(source))
    import numpy as np
    import soundfile as sf
    import torch
    from cli.inference import build_model
    from soulxsinger.utils.file_utils import load_config
    from soulxsinger.utils.data_processor import DataProcessor

    if p["device"] == "cuda" and not torch.cuda.is_available():
        raise ValueError("未检测到 CUDA；请配置可用环境或显式选择 CPU（很慢，未实测）")
    config = load_config(p["config_path"])
    if config.audio.sample_rate != 24000 or config.audio.hop_size != 480:
        raise ValueError("该适配器仅支持已核验的24000Hz/480 hop SoulX SVS配置")
    if p.get("doctor"):
        return {"message": f"SoulX SVS 配置/依赖可读 · torch {torch.__version__} · {p['device']}；未加载权重或验证合成质量"}
    prompt = copy.deepcopy(p["prompt_metadata_data"])[0]
    targets = copy.deepcopy(p["target_metadata_data"])
    validate_prompt_audio(sf, np, p["prompt_audio"], prompt["time"][1]/1000)
    processor = DataProcessor(hop_size=480, sample_rate=24000, phoneset_path=p["phoneset_path"], device=p["device"])
    prompt_data = processor.process(prompt, p["prompt_audio"])
    model = build_model(model_path=p["model_path"], config=config, device=p["device"], use_fp16=False)
    merged = np.zeros(int(targets[-1]["time"][1] * 24), dtype=np.float32)
    for target in targets:
        start, end = (int(t * 24) for t in target["time"])
        target_data = processor.process(target, None)
        with torch.no_grad():
            audio = model.infer({"prompt": prompt_data, "target": target_data}, auto_shift=False,
                pitch_shift=p["pitch_shift"], n_steps=config.infer.n_steps, cfg=config.infer.cfg,
                control=p["control"], use_fp16=False)
        audio = audio.squeeze().cpu().numpy().reshape(-1)
        if not audio.size or not np.isfinite(audio).all():
            raise ValueError("SoulX SVS 返回了无效音频")
        count = min(len(audio), end-start)
        merged[start:start+count] = audio[:count]
    output = Path(p["output_dir"])
    temporary = output/"vocal.tmp.wav"
    try:
        sf.write(str(temporary), merged, 24000, subtype="PCM_16")
        os.replace(temporary, output/"vocal.wav")
    finally:
        temporary.unlink(missing_ok=True)
    return {"sample_rate": 24000, "segments": len(targets), "control": p["control"]}


def main():
    p = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
    result = execute(p)
    Path(p["result_path"]).write_text(json.dumps(result, ensure_ascii=False), encoding="utf-8")


if __name__ == "__main__":
    main()
