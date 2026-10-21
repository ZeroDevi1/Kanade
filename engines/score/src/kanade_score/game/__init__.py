"""GAME（Generative Adaptive MIDI Extractor）推理所需的最小代码，vendor 自 https://github.com/openvpi/GAME
（代码 MIT，见同目录 LICENSE），提交 f4239345f0fed21890d362c74a35fd857724aa82（2026-08-30）。

注意：上游发布的预训练权重（GAME-1.0 系列）使用 CC BY-NC-SA 4.0 许可，仅限非商业用途；本引擎只在用户于专业模式
选择 GAME 时下载与使用。

只取推理路径：me_infer.py（原 inference/me_infer.py）、模型 midi_extraction.py 及其依赖 d3pm.py / decoding.py /
functional.py / common_layers.py（原 modules/commons/）/ mel.py（原 lib/feature/mel.py）/ backbones/（只取 config.yaml
用到的 EBF 与 JEBF 及其依赖）。改动仅限：绝对导入改为包内相对导入；deployment.context 换成 _export.py（导出模式
恒为关闭）；lib.reflection 的按类名构造换成 midi_extraction 里的映射表；去掉 pydantic 配置类型标注（配置用
SimpleNamespace 传入）。训练、ONNX 导出与 lightning 推理框架未引入；字与音符的对应 align_notes_to_words 移植自
inference/utils.py（见本文件）。
"""

from __future__ import annotations

import json
from pathlib import Path
from types import SimpleNamespace

import numpy as np

# 推理参数取上游 infer.py align 的默认值：边界阈值 0.2、边界半径 0.02 s、音符存在阈值 0.2，D3PM t0 = 0、8 步。
SEG_THRESHOLD = 0.2
SEG_RADIUS_S = 0.02
EST_THRESHOLD = 0.2
D3PM_STEPS = 8


def _ns(d):
    """把 config.yaml 的嵌套 dict 转成属性访问的 SimpleNamespace；kwargs 保持 dict（作为 ** 参数展开）。"""
    if isinstance(d, dict):
        return SimpleNamespace(**{k: (v if k == "kwargs" else _ns(v)) for k, v in d.items()})
    return d


class Model:
    """加载好的 GAME 模型。创建时读取 model_dir 下的 config.yaml、lang_map.json 与 model.pt 并移到 device。"""

    def __init__(self, model_dir: Path, device: str):
        import torch
        import yaml

        from .me_infer import SegmentationEstimationInferenceModel

        cfg = yaml.safe_load((model_dir / "config.yaml").read_text(encoding="utf-8"))
        model_cfg, inf_cfg = _ns(cfg["model"]), _ns(cfg["inference"])
        inf_cfg.features.timestep = inf_cfg.features.hop_size / inf_cfg.features.audio_sample_rate
        self.sr = inf_cfg.features.audio_sample_rate
        self.timestep = inf_cfg.features.timestep
        self.lang_map: dict[str, int] = json.loads((model_dir / "lang_map.json").read_text(encoding="utf-8"))
        net = SegmentationEstimationInferenceModel(model_config=model_cfg, inference_config=inf_cfg)
        # 检查点是 reduce.py 精简后的 {state_dict}；weights_only=True 只反序列化张量与基本类型，不执行任意代码。
        ckpt = torch.load(str(model_dir / "model.pt"), map_location="cpu", weights_only=True)
        state = ckpt.get("state_dict", {})
        state.update(ckpt.get("ema_state_dict") or {})
        net.load_state_dict(state, strict=True)
        self.net = net.eval().to(device)
        self.device = device

    def infer(self, wav: np.ndarray, word_dur: list[float], language: str) -> tuple[list[float], list[bool], list[float]]:
        """对一段音频（self.sr Hz 单声道）按已知的字时长切分音符，返回（音符时长秒, 是否有声, 音高 MIDI）。

        word_dur 之和应等于音频时长；字边界是不可移动的音符边界，模型只在字内部再细分。D3PM 会随机去掉边界，
        这里在固定种子的局部随机状态中推理，结果可复现且不影响全局随机数。
        """
        import torch

        dev = self.device
        lang = self.lang_map.get(language, 0)
        cuda = [torch.cuda.current_device()] if dev.startswith("cuda") else []
        with torch.no_grad(), torch.random.fork_rng(devices=cuda):
            torch.manual_seed(0)
            durations, presence, scores = self.net(
                waveform=torch.from_numpy(wav.astype(np.float32)).unsqueeze(0).to(dev),
                known_durations=torch.tensor([word_dur], dtype=torch.float32, device=dev),
                language=torch.tensor([lang], dtype=torch.long, device=dev),
                t=torch.tensor([i / D3PM_STEPS for i in range(D3PM_STEPS)], dtype=torch.float32, device=dev),
                boundary_threshold=torch.tensor(SEG_THRESHOLD, device=dev),
                boundary_radius=torch.tensor(round(SEG_RADIUS_S / self.timestep), dtype=torch.long, device=dev),
                score_threshold=torch.tensor(EST_THRESHOLD, device=dev),
            )
        d, p, s = durations[0].cpu().numpy(), presence[0].cpu().numpy(), scores[0].cpu().numpy()
        ok = d > 0
        return [float(x) for x in d[ok]], [bool(x) for x in p[ok]], [float(x) for x in s[ok]]


def align_notes_to_words(word_dur: list[float], note_pitch: list[float | None], note_dur: list[float],
                         tol: float = 0.01) -> list[list[tuple[float | None, float]]]:
    """把音符分到字上，返回每个字的音符 [(音高或 None 表示休止, 时长)]，每个字至少一个。

    移植自上游 inference/utils.py 的 align_notes_to_words（uv_note_cond=predict）：每个字取起点最近的音符到终点最近的
    音符，首尾音符裁到字边界。与上游的差别：上游只合并音名（含音分）完全相同的相邻音，这里合并四舍五入后半音相同的
    相邻音（时长加权平均音高），避免同一个音因音分抖动被拆成几个连音。
    """
    word_start = np.cumsum([0.0] + word_dur[:-1])
    word_end = np.cumsum(word_dur)
    note_start = np.cumsum([0.0] + note_dur[:-1])
    note_end = np.cumsum(note_dur)
    out = []
    for w in range(len(word_dur)):
        i0 = int(np.argmin(np.abs(note_start - word_start[w])))
        if word_start[w] < note_start[i0] - tol:
            i0 = max(0, i0 - 1)
        i1 = int(np.argmin(np.abs(note_end[i0:] - word_end[w]))) + i0
        if word_end[w] > note_end[i1] + tol:
            i1 = min(len(note_end) - 1, i1 + 1)
        notes: list[tuple[float | None, float]] = []
        for i in range(i0, i1 + 1):
            start = word_start[w] if i == i0 else note_start[i]
            end = word_end[w] if i == i1 else note_end[i]
            p = note_pitch[i]
            if notes and (notes[-1][0] is None and p is None or
                          notes[-1][0] is not None and p is not None and round(notes[-1][0]) == round(p)):
                q, d = notes[-1]
                notes[-1] = (None if q is None else (q * d + p * (end - start)) / max(d + end - start, 1e-9), d + end - start)
            else:
                notes.append((p, end - start))
        out.append([(None if q is None else float(q), float(d)) for q, d in notes])
    return out
