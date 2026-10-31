"""Validate the SVS metadata actually consumed by upstream DataProcessor."""
import json
import math
from pathlib import Path


def file(value, label):
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"请配置 {label}")
    p = Path(value).expanduser().resolve()
    if not p.is_file():
        raise ValueError(f"{label} 文件不存在：{p}")
    return str(p)


def metadata(path, *, prompt=False, control="score", phones=None):
    if Path(path).stat().st_size > 16 * 1024 * 1024:
        raise ValueError("元数据不能超过 16 MiB")
    rows = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(rows, list) or not rows or len(rows) > 1000:
        raise ValueError("元数据必须是非空 JSON 段落数组（最多1000段）")
    if prompt and len(rows) != 1:
        raise ValueError("参考元数据须为单段；请先裁剪并校准参考音频")
    last_end = 0
    for i, row in enumerate(rows):
        if not isinstance(row, dict):
            raise ValueError(f"元数据第{i+1}段必须是对象")
        time = row.get("time")
        if not isinstance(time, list) or len(time) != 2 or any(isinstance(x, bool) or not isinstance(x, (int, float)) or not math.isfinite(x) for x in time):
            raise ValueError("time 必须是毫秒 [start,end]")
        start, end = time
        if start < last_end or end <= start or end > 3600000 or end-start > 120000:
            raise ValueError("段落须按时间排序且不重叠；单段最多120秒，总长最多1小时")
        if prompt and start != 0:
            raise ValueError("参考音频需裁剪到该段起点，参考元数据 time 必须从0开始")
        last_end = end
        keys = ("phoneme", "duration", "note_pitch", "note_type")
        if any(not isinstance(row.get(k), str) or not row[k].strip() for k in keys):
            raise ValueError("每段必须包含非空字符串 phoneme/duration/note_pitch/note_type")
        values = [row[k].split() for k in keys]
        if len({len(v) for v in values}) != 1:
            raise ValueError("音素、时长、音高和音符类型数量必须一致；请手工校准元数据")
        durations = [float(v) for v in values[1]]
        if any(not math.isfinite(d) or d <= 0 for d in durations):
            raise ValueError("duration 必须是有限正数（秒）")
        total = sum(durations)
        if abs(total - (end-start)/1000) > .1:
            raise ValueError("duration 总和必须与 time 段长匹配（容差0.1秒）")
        if total < .02:
            raise ValueError("段落至少需要一帧（20ms）")
        for group in values[2:]:
            if any(not 0 <= int(v) <= 255 for v in group):
                raise ValueError("note_pitch/note_type 必须是0–255整数")
        if phones is not None:
            expanded = []
            for ph in values[0]:
                ph = ph.replace("<AP>", "<SP>")
                expanded.extend(["en_"+v for v in ph[3:].split("-")] if ph.startswith("en_") else [ph])
            if any(ph not in phones for ph in expanded):
                raise ValueError("元数据包含当前上游 phoneset 不支持的音素")
        f0 = row.get("f0", "")
        if not isinstance(f0, str):
            raise ValueError("f0 必须是空格分隔数字字符串")
        if control == "melody" and not f0.strip():
            raise ValueError("melody 模式的参考和目标元数据都必须包含 F0")
        if f0.strip():
            curve = [float(v) for v in f0.split()]
            if any(not math.isfinite(v) or v < 0 for v in curve):
                raise ValueError("F0 必须是非负有限数")
            if abs(len(curve)-int(total*50)) > 2:
                raise ValueError("F0 必须与段长按50Hz帧率对齐；请重新校准")
    return rows


def request(params, output_dir, *, doctor=False):
    p = dict(params)
    p["python_path"] = file(p.get("python_path"), "SoulX 独立 Python")
    for key in ("source_dir", "model_dir"):
        value = p.get(key)
        if not isinstance(value, str) or not value.strip():
            raise ValueError(f"请配置 {key}")
        p[key] = str(Path(value).expanduser().resolve())
    source = Path(p["source_dir"])
    file(str(source/"cli/inference.py"), "SoulX SVS 源码")
    p["config_path"] = file(str(source/"soulxsinger/config/soulxsinger.yaml"), "SoulX 配置")
    p["phoneset_path"] = file(str(source/"soulxsinger/utils/phoneme/phone_set.json"), "SoulX phoneset")
    p["model_path"] = file(str(Path(p["model_dir"])/"model.pt"), "SoulX SVS model.pt（不是model-svc.pt）")
    p["device"] = p.get("device", "cuda")
    if p["device"] not in ("cpu", "cuda"):
        raise ValueError("device 仅支持 cpu 或 cuda")
    p["doctor"], p["output_dir"] = doctor, str(Path(output_dir).resolve())
    if doctor:
        return p
    p["control"] = p.get("control", "score")
    if p["control"] not in ("score", "melody"):
        raise ValueError("control 仅支持 score 或 melody（不是SVC）")
    p["pitch_shift"] = p.get("pitch_shift", 0)
    if isinstance(p["pitch_shift"], bool) or not isinstance(p["pitch_shift"], int) or not -24 <= p["pitch_shift"] <= 24:
        raise ValueError("pitch_shift 须为-24到24整数半音")
    p["prompt_audio"] = file(p.get("prompt_audio"), "参考干声")
    phones = set(json.loads(Path(p["phoneset_path"]).read_text(encoding="utf-8")))
    for key in ("prompt_metadata", "target_metadata"):
        p[key] = file(p.get(key), key)
        p[key+"_data"] = metadata(p[key], prompt=key=="prompt_metadata", control=p["control"], phones=phones)
    return p
