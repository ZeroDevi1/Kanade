"""score.analyze：一首歌的完整分析。

参数::

    input:           歌曲（任意 ffmpeg 可解码的格式）
    harmonic_input:  可选，去掉人声的伴奏；有时调号与和弦用它分析
    melody_input:    可选，分离出的主唱（去和声）；有时用 SOME 识别旋律线
    lyrics_input:    可选，LRC 歌词；与 melody_input 同时给出时把歌词逐字对到旋律音符上（见 lyrics.py）
    options: {
        beat_method:   "beat_this"（默认）| "librosa"
        tempo_range:   [lo, hi]，可选；BPM 按倍数折叠到区间内（半速 / 倍速误判时用）
        key_profile:   "krumhansl"（默认）| "temperley"
        key_segment_s: 分段估计调号的窗口（秒），默认 30，0 为不分段
        chord_vocab:   "full"（默认，BTC 神经网络：14 种和弦性质与转位）| "basic"（模板匹配，大小三和弦）
                       | "extended"（模板匹配，加七和弦、减三、挂四）
        chord_self_prob: HMM 自转移概率（0.5–0.99，默认 0.9，越大和弦越不易切换）
        chord_min_beats: 最短和弦（拍），默认 1
        slur_model:    有歌词时字与音符的对应方法："align"（默认，SOME 音符 + 动态规划分配）| "game"（GAME 按字切分音符，
                       权重为 CC BY-NC-SA 4.0 非商用许可）
        emotion:       是否做情绪识别，默认 true
    }

melody 为 {notes: [{start, end, pitch, lyric?, slur?}], source, lyrics?}，没有 melody_input 时为 null。有歌词时
lyric 是该音唱的字、slur 为 true 表示同一个字的后续音（连音）；lyrics 为 {source（LRC 路径）, words, matched, note}：
words 是逐字对齐结果 [{text, line, start, end}]，matched 是分到音符的字数，note 非空时说明对齐放宽了哪些约束，
method 是字与音符的对应方法（同 options.slur_model）。

输出（任务目录下）：audio.wav（原曲解码为 44.1 kHz WAV，供界面预览）、analysis.json（结构见 run_analyze 末尾的 result）与 chords.lab（MIREX 和弦标注格式：
起止秒与和弦名，制表符分隔，可导入 Sonic Visualiser 等工具）。
"""

from __future__ import annotations

import json
import subprocess
import tempfile
from pathlib import Path

import numpy as np
import soundfile as sf

from kanade_engine import Job, pick_device

from . import btc, emotion, harmony, lyrics, melody, models, rhythm

SR = 44100
HOP = 512


def decode(src: Path, out_dir: Path, name: str) -> tuple[np.ndarray, int]:
    """用 ffmpeg 解码为 44.1 kHz 立体声（写出 out_dir/<name>.wav，16 bit），返回 float32 采样，统一各种输入格式。"""
    import imageio_ffmpeg

    out = out_dir / f"{name}.wav"
    cmd = [imageio_ffmpeg.get_ffmpeg_exe(), "-y", "-v", "error", "-i", str(src), "-ar", str(SR), "-ac", "2", str(out)]
    p = subprocess.run(cmd, capture_output=True, text=True, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    if p.returncode != 0:
        raise ValueError(f"无法解码 {src.name}：{p.stderr.strip()[-300:]}")
    y, sr = sf.read(str(out), dtype="float32", always_2d=True)
    return y, sr


def run_analyze(job: Job) -> None:
    p = job.params
    src = Path(p.get("input") or "")
    if not src.is_file():
        raise ValueError(f"输入文件不存在：{src}")
    harm_src = Path(p["harmonic_input"]) if p.get("harmonic_input") else None
    if harm_src is not None and not harm_src.is_file():
        raise ValueError(f"伴奏文件不存在：{harm_src}")
    mel_src = Path(p["melody_input"]) if p.get("melody_input") else None
    if mel_src is not None and not mel_src.is_file():
        raise ValueError(f"主唱文件不存在：{mel_src}")
    lrc_src = Path(p["lyrics_input"]) if p.get("lyrics_input") and mel_src else None
    lines = []
    if lrc_src is not None:
        if not lrc_src.is_file():
            raise ValueError(f"歌词文件不存在：{lrc_src}")
        # LRC 常见 UTF-8（可带 BOM）与 GBK 两种编码。
        raw = lrc_src.read_bytes()
        try:
            text = raw.decode("utf-8-sig")
        except UnicodeDecodeError:
            text = raw.decode("gbk", errors="replace")
        lines = lyrics.parse_lrc(text)
        if not lines:
            raise ValueError(f"{lrc_src.name} 中没有带时间戳的歌词行")
    o = p.get("options") or {}
    beat_method = o.get("beat_method") or "beat_this"
    if beat_method not in ("beat_this", "librosa"):
        raise ValueError(f"未知的节拍算法：{beat_method}")
    profile = o.get("key_profile") or "krumhansl"
    vocab = o.get("chord_vocab") or "full"
    if profile not in harmony.PROFILES or vocab not in harmony.CHORD_VOCABS:
        raise ValueError("调性轮廓或和弦词汇取值无效")
    self_prob = float(o.get("chord_self_prob") or 0.9)
    if not 0.5 <= self_prob <= 0.99:
        raise ValueError("和弦平滑须在 0.5–0.99 之间")
    min_beats = int(o.get("chord_min_beats") or 1)
    segment_s = float(o.get("key_segment_s", 30) or 0)
    fold = tuple(o["tempo_range"]) if o.get("tempo_range") else None
    want_emotion = o.get("emotion", True)
    slur_model = o.get("slur_model") or "align"
    if slur_model not in ("align", "game"):
        raise ValueError(f"未知的连音方法：{slur_model}")
    use_game = bool(lines) and slur_model == "game"

    # 1. 模型（按需下载）与解码。
    groups = (["beat_this"] if beat_method == "beat_this" else []) + (["clap"] if want_emotion else []) + \
        (["some"] if mel_src else []) + (["btc"] if vocab == "full" else []) + (["lyrics"] if lines else []) + \
        (["game"] if use_game else [])
    models.ensure(job, groups, 0.0, 0.15)
    out_dir = job.output_dir / job.id
    out_dir.mkdir(parents=True, exist_ok=True)
    job.progress(0.15, "解码音频")
    # 原曲的解码结果保留为 audio.wav，供界面预览播放（界面的播放器只支持 WAV，不支持 FLAC / M4A）。
    y, sr = decode(src, out_dir, "audio")
    with tempfile.TemporaryDirectory(dir=out_dir) as tmp:
        hy, hsr = decode(harm_src, Path(tmp), "harmonic") if harm_src else (y, sr)
        vy, vsr = decode(mel_src, Path(tmp), "melody") if mel_src else (None, 0)
    duration = len(y) / sr
    job.check_cancelled()

    # 2. 节拍、BPM 与拍号（原曲：人声也携带节奏信息）。
    job.progress(0.2, "节拍检测")
    if beat_method == "beat_this":
        beats, downbeats = rhythm.track_beat_this(job, y, sr)
    else:
        beats, downbeats = rhythm.track_librosa(job, y, sr)
    beats = rhythm.regularize(beats)
    downbeats = rhythm.snap_downbeats(beats, downbeats)
    job.check_cancelled()

    # 3. 调号与和弦（有伴奏时用伴奏）。
    job.progress(0.4, "调号与和弦分析（色度提取）")
    c = harmony.chroma(hy, hsr, HOP)
    job.check_cancelled()
    # 两遍：先按色度轮廓的初步调号识别和弦，用和弦与色度共同定调，再带调性先验重新识别和弦。
    job.progress(0.5, "和弦识别")
    t0, m0 = harmony.initial_key(c, profile)
    if vocab == "full":
        # BTC 的逐帧概率只算一次：两遍解码只差和弦名的升降号拼写。
        d = pick_device(allow_mps=False)
        job.progress(0.5, f"和弦识别（BTC，{d.label}）")
        probs = btc.frame_probs(hy.mean(axis=1), hsr, str(models.path(models.BTC_CKPT)), d.name)
        rough = harmony.decode_btc(probs, btc.FRAME_S, beats, duration, harmony.uses_flats(t0, m0), self_prob, min_beats)
        key = harmony.detect_key(c, hsr, HOP, profile, segment_s, rough)
        chords = harmony.decode_btc(probs, btc.FRAME_S, beats, duration, key["flats"], self_prob, min_beats,
                                    (key["tonic_pc"], key["mode"]))
        chords = harmony.add_inversions(chords, harmony.bass_spectrum(hy, hsr, HOP), hsr, HOP, key["flats"])
    else:
        rough = harmony.detect_chords(c, hsr, HOP, beats, duration, vocab, harmony.uses_flats(t0, m0), self_prob, min_beats)
        key = harmony.detect_key(c, hsr, HOP, profile, segment_s, rough)
        chords = harmony.detect_chords(c, hsr, HOP, beats, duration, vocab, key["flats"], self_prob, min_beats,
                                       (key["tonic_pc"], key["mode"]))
    job.check_cancelled()

    # 4. 旋律（主唱）与情绪（原曲）。
    tune = None
    if vy is not None:
        tune = {"notes": melody.transcribe(job, vy.mean(axis=1), vsr, 0.58, 0.66 if lines else 0.7), "source": "lead_vocal"}
        if lines:
            words, note = lyrics.align(job, vy.mean(axis=1), vsr, lines, 0.66, 0.68 if use_game else 0.7)
            if use_game:
                tune["notes"], lost = lyrics.attach_game(job, vy.mean(axis=1), vsr, words, tune["notes"], 0.68, 0.7)
            else:
                tune["notes"], lost = lyrics.attach(tune["notes"], words)
            tune["lyrics"] = {"source": str(lrc_src), "words": words, "matched": len(words) - lost, "note": note,
                              "method": slur_model}
    mood = None
    if want_emotion:
        mood = emotion.analyze(job, y.mean(axis=1), sr, 0.7, 0.97)

    result = {
        "version": 1,
        "input": str(src),
        "duration": round(duration, 3),
        "tempo": rhythm.tempo(beats, downbeats, fold),
        "meter": rhythm.meter(beats, downbeats),
        "beats": [round(float(b), 3) for b in beats],
        "downbeats": [round(float(b), 3) for b in downbeats],
        "key": key,
        "chords": chords,
        "chord_source": "accompaniment" if harm_src else "mix",
        "emotion": mood,
        "melody": tune,
        "options": {"beat_method": beat_method, "key_profile": profile, "chord_vocab": vocab,
                    "chord_self_prob": self_prob, "chord_min_beats": min_beats, "slur_model": slur_model},
    }
    js = out_dir / "analysis.json"
    js.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    lab = out_dir / "chords.lab"
    lab.write_text("".join(f"{c['start']:.3f}\t{c['end']:.3f}\t{c['label']}\n" for c in chords), encoding="utf-8")
    job.artifact(out_dir / "audio.wav", "audio")
    job.artifact(js, "analysis")
    job.artifact(lab, "chords")
    t = result["tempo"]["bpm"]
    job.progress(1.0, f"完成：{key['label']} · {result['meter']['label'] or '拍号未知'} · {t or '—'} BPM")
