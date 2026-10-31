package singing

import (
	"errors"
	"strings"
)

const SoulXEngine = "soulx"
const Vevo2Engine = "vevo2"

// SoulXRequest uses corrected note/phoneme metadata, not a text-only edit.
// PromptAudio must be cropped to the single prompt metadata segment starting at 0.
type SoulXRequest struct {
	PythonPath, SourceDir, ModelDir             string
	PromptAudio, PromptMetadata, TargetMetadata string
	Control, Device                             string
}

func runtimeParams(python, source, model string) (map[string]any, error) {
	p := map[string]any{}
	for _, f := range []struct {
		key, value, label string
		dir               bool
	}{
		{"python_path", python, "独立 Python 解释器", false},
		{"source_dir", source, "上游源码目录", true},
		{"model_dir", model, "本地模型目录", true},
	} {
		v, e := existing(f.value, f.label, f.dir)
		if e != nil {
			return nil, e
		}
		p[f.key] = v
	}
	return p, nil
}

func (r SoulXRequest) DoctorParams() (map[string]any, error) {
	p, e := runtimeParams(r.PythonPath, r.SourceDir, r.ModelDir)
	if e != nil {
		return nil, e
	}
	device := r.Device
	if device == "" {
		device = "cuda"
	}
	if device != "cuda" && device != "cpu" {
		return nil, errors.New("SoulX 设备须为 cuda 或 cpu")
	}
	p["device"] = device
	return p, nil
}

func (r SoulXRequest) Params() (map[string]any, error) {
	p, e := r.DoctorParams()
	if e != nil {
		return nil, e
	}
	control := r.Control
	if control == "" {
		control = "score"
	}
	if control != "score" && control != "melody" {
		return nil, errors.New("SoulX 控制模式须为 score 或 melody")
	}
	p["control"] = control
	for _, f := range []struct{ key, value, label string }{
		{"prompt_audio", r.PromptAudio, "已裁剪的参考干声"},
		{"prompt_metadata", r.PromptMetadata, "参考 metadata JSON"},
		{"target_metadata", r.TargetMetadata, "已校准的新词 metadata JSON"},
	} {
		v, e := existing(f.value, f.label, false)
		if e != nil {
			return nil, e
		}
		p[f.key] = v
	}
	return p, nil
}

// Vevo2Request selects the AR+FM editing path. The weight license is separate
// from Amphion's code license; acceptance is explicit rather than preselected.
type Vevo2Request struct {
	PythonPath, SourceDir, ModelDir                           string
	WhisperPath, WhisperStatsPath                             string
	SourceAudio, OriginalText, TargetText, AccompanimentAudio string
	LicenseAccepted                                           bool
}

func (r Vevo2Request) DoctorParams() (map[string]any, error) {
	if !r.LicenseAccepted {
		return nil, errors.New("请先阅读并确认 Vevo2 权重 CC-BY-NC-ND 4.0 使用限制")
	}
	p, e := runtimeParams(r.PythonPath, r.SourceDir, r.ModelDir)
	if e != nil {
		return nil, e
	}
	v, e := existing(r.WhisperPath, "本地 Whisper medium.pt", false)
	if e != nil {
		return nil, e
	}
	p["whisper_path"] = v
	if strings.TrimSpace(r.WhisperStatsPath) != "" {
		v, e := existing(r.WhisperStatsPath, "本地 Whisper 统计文件", false)
		if e != nil {
			return nil, e
		}
		p["whisper_stats_path"] = v
	}
	p["license_accepted"] = true
	return p, nil
}

func (r Vevo2Request) Params() (map[string]any, error) {
	p, e := r.DoctorParams()
	if e != nil {
		return nil, e
	}
	v, e := existing(r.SourceAudio, "原曲干净人声", false)
	if e != nil {
		return nil, e
	}
	p["source_audio"] = v
	for _, f := range []struct{ key, value, label string }{
		{"original_text", r.OriginalText, "原曲实际歌词"}, {"target_text", r.TargetText, "目标新歌词"},
	} {
		v := strings.TrimSpace(f.value)
		if v == "" {
			return nil, errors.New("请填写" + f.label)
		}
		p[f.key] = v
	}
	if strings.TrimSpace(r.AccompanimentAudio) != "" {
		v, e := existing(r.AccompanimentAudio, "伴奏", false)
		if e != nil {
			return nil, e
		}
		p["accompaniment_audio"] = v
	}
	return p, nil
}
