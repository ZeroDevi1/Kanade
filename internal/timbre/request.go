package timbre

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Preset 是一组推理参数，取自 VoiceLab docs/workflows/rvc 的三档预设。
type Preset struct {
	Key, Title, Hint string
	IndexRate        float64
	Protect          float64
	VolumeEnvelope   float64 // 即 RVC 的 rms_mix_rate：1 为保留转换结果的响度包络，越小越贴近输入
	F0               string
}

// Presets：说话默认 Speech；更像目标音色 MoreTarget；歌声推荐 MoreClean（撕裂风险更低）。
var Presets = []Preset{
	{"speech", "说话", "普通人声的默认参数", 0.8, 0.33, 0.25, "rmvpe"},
	{"more_target", "更像目标", "提高检索比例，音色更接近目标，但可能带来电音感", 0.9, 0.33, 0.25, "rmvpe"},
	{"more_clean", "更干净（歌声推荐）", "降低检索比例、加强辅音保护，减少撕裂", 0.65, 0.4, 0.25, "rmvpe"},
}

// F0Methods 是支持的音高提取算法：rmvpe 速度与质量平衡；crepe 更稳但更慢（歌声常用）；fcpe 最快。
var F0Methods = []string{"rmvpe", "crepe", "fcpe"}

// StereoModes 是立体声输入的处理方式，见引擎 infer.py。
var StereoModes = []struct{ Key, Title string }{
	{"mono", "下混为单声道（最快）"},
	{"pan", "保留声像（推荐用于人声）"},
	{"dual", "左右声道分别转换（最慢）"},
}

// InferRequest 是一次转换的输入。
type InferRequest struct {
	Model          Model
	Input          string
	Pitch          int // 半音；男转女常用 +12，女转男 -3 ~ -6，歌声先从 0 开始
	F0             string
	IndexRate      float64
	Protect        float64
	VolumeEnvelope float64
	Stereo         string
	Normalize      bool
}

// 参数范围与 Applio / RVC WebUI 的滑块一致。
const (
	MinPitch = -24
	MaxPitch = 24
)

// Params 校验输入并返回 timbre.infer 的任务参数。
func (r InferRequest) Params() (map[string]any, error) {
	if r.Model.Weights == "" {
		return nil, errors.New("请选择音色模型")
	}
	if r.Input == "" {
		return nil, errors.New("请选择要转换的音频")
	}
	if r.Pitch < MinPitch || r.Pitch > MaxPitch {
		return nil, fmt.Errorf("变调须在 %d 到 %d 半音之间", MinPitch, MaxPitch)
	}
	if !contains(F0Methods, r.F0) {
		return nil, fmt.Errorf("不支持的音高算法 %q", r.F0)
	}
	for _, v := range []float64{r.IndexRate, r.Protect, r.VolumeEnvelope} {
		if v < 0 || v > 1 {
			return nil, errors.New("检索比例、辅音保护与响度包络须在 0–1 之间")
		}
	}
	stereo := r.Stereo
	if stereo == "" {
		stereo = "mono"
	}
	return map[string]any{
		"model": r.Model.Weights, "index": r.Model.Index, "input": r.Input, "pitch": r.Pitch, "f0_method": r.F0,
		"index_rate": r.IndexRate, "protect": r.Protect, "volume_envelope": r.VolumeEnvelope,
		"stereo_mode": stereo, "normalize": r.Normalize,
	}, nil
}

// TrainRequest 是一次训练的输入。参数含义与默认值见 VoiceLab docs/workflows/rvc（v2 + 48k + f0）。
type TrainRequest struct {
	Name       string
	Dataset    string
	List       string // 为空时按同名规则自动查找
	SampleRate int
	F0         string
	Epochs     int
	BatchSize  int
	SaveEvery  int
	// SkipPreprocess / SkipExtract 用于续训：已完成的阶段不再重复。
	SkipPreprocess, SkipExtract bool
}

// SampleRates 是可选的训练采样率（与 Applio 的预训练底模对应）。
var SampleRates = []int{48000, 40000, 32000}

// Params 校验输入并返回 timbre.train 的任务参数。
func (r TrainRequest) Params() (map[string]any, error) {
	if !ValidName(strings.TrimSpace(r.Name)) {
		return nil, errors.New("模型名只能包含字母、数字、中文、下划线与连字符")
	}
	if !r.SkipPreprocess {
		if r.Dataset == "" {
			return nil, errors.New("请选择数据集目录")
		}
		if st, err := os.Stat(r.Dataset); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("数据集目录不存在：%s", r.Dataset)
		}
	}
	if !containsInt(SampleRates, r.SampleRate) {
		return nil, errors.New("采样率须为 48000、40000 或 32000")
	}
	if !contains(F0Methods, r.F0) {
		return nil, fmt.Errorf("不支持的音高算法 %q", r.F0)
	}
	if r.Epochs < 1 || r.Epochs > 10000 || r.BatchSize < 1 || r.BatchSize > 64 || r.SaveEvery < 1 {
		return nil, errors.New("轮数须在 1–10000、batch 在 1–64 之间，保存间隔至少为 1")
	}
	return map[string]any{
		"name": strings.TrimSpace(r.Name), "dataset_dir": r.Dataset, "list": r.List, "sample_rate": r.SampleRate,
		"f0_method": r.F0, "epochs": r.Epochs, "batch_size": r.BatchSize, "save_every": r.SaveEvery,
		"skip_preprocess": r.SkipPreprocess, "skip_extract": r.SkipExtract,
	}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
