package vocal

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 歌姬预设：导出 Synthesizer V 工程时按所选歌姬套用的音色设置与表情强度，保存在用户数据目录的 sv-presets.json
// （键为歌姬名）。没有保存过的歌姬用 DefaultSVPreset。各参数对不同声库的效果差异很大（论坛反馈：同一张力值
// 在新版 AI 声库上主要表现为音量变化），因此默认值只是保守的起点，由用户试听后按歌姬调整保存。

// SVPresetsFile 是预设文件在用户数据目录中的名称。
const SVPresetsFile = "sv-presets.json"

// SVPreset 是一个歌姬的预设。
type SVPreset struct {
	// 音色（写入音轨 voice 的默认值，整首生效）。Gender / Breathiness / Tension 为 −1–1，ToneShift 为音分（±1200），
	// Loudness 为 dB（−12–12）。
	Gender      float64 `json:"gender"`
	ToneShift   float64 `json:"toneShift"`
	Loudness    float64 `json:"loudness"`
	Breathiness float64 `json:"breathiness"`
	Tension     float64 `json:"tension"`
	// VocalModes 是演唱模式的强度，形如 "Clear=60, Belt=20"（名称见 SV 中该声库的演唱模式面板，0–150）；空为沿用声库默认。
	VocalModes string `json:"vocalModes"`
	// AutoTranspose 为 true 时按歌姬音域以八度移调（SuggestTranspose），否则用 Transpose（半音）。
	AutoTranspose bool `json:"autoTranspose"`
	Transpose     int  `json:"transpose"`
	// Breaths 为 true 时在乐句前插入 br 换气音符（SVExport.Breaths）；需要声库有 br 音素，默认关闭。
	Breaths bool `json:"breaths"`
	// 表情强度（0–1）：从原唱提取的曲线乘以强度后写入，0 为不写该曲线（Synthesizer V 自行生成）。
	PitchStrength    float64 `json:"pitchStrength"`
	DynamicsStrength float64 `json:"dynamicsStrength"`
	BreathStrength   float64 `json:"breathStrength"`
	TensionStrength  float64 `json:"tensionStrength"`
	VoicingStrength  float64 `json:"voicingStrength"`
}

// DefaultSVPreset 是没有保存过预设的歌姬所用的设置：音色不变，自动移调，不写任何表情曲线（音高、动态等全部由 SV
// 自行生成）。原唱表情曲线是未标定的经验映射，《我不想念》用 Weina 渲染的两版（音高覆盖 + 动态 / 气声 / 张力）听起来
// 都像憋着唱、不如不写（2026-10-24 实测，见 docs/roadmap.md 第 3 节），因此表情强度只作为实验选项由用户手动打开。
func DefaultSVPreset() SVPreset {
	return SVPreset{AutoTranspose: true}
}

// Modes 解析 VocalModes：名称到强度（0–150）。格式错误的项返回错误。
func (p SVPreset) Modes() (map[string]float64, error) {
	out := map[string]float64{}
	for item := range strings.SplitSeq(strings.ReplaceAll(p.VocalModes, "，", ","), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, val, ok := strings.Cut(item, "=")
		v, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if name = strings.TrimSpace(name); !ok || name == "" || err != nil || v < 0 || v > 150 {
			return nil, errors.New("演唱模式的格式应为「名称=强度」（0–150），用逗号分隔，例如 Clear=60, Belt=20")
		}
		out[name] = v
	}
	return out, nil
}

// LoadSVPresets 读取 dir 下的预设文件；不存在时返回空表且无错误。
func LoadSVPresets(dir string) (map[string]SVPreset, error) {
	out := map[string]SVPreset{}
	data, err := os.ReadFile(filepath.Join(dir, SVPresetsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(data, &out)
}

// SaveSVPresets 把预设写入 dir：先写临时文件再重命名，避免写到一半崩溃留下损坏的文件。
func SaveSVPresets(dir string, presets map[string]SVPreset) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(presets, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, SVPresetsFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, SVPresetsFile))
}
