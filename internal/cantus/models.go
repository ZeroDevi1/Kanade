// Package cantus 描述 CosyVoice 语音生成（Cantus）的模型、模式与已保存音色，并把界面输入组装为引擎任务参数。
//
// 模型与模式的对应关系须与 engines/cosyvoice/src/kanade_cosyvoice/tasks.py 的 MODES 保持一致。
package cantus

import (
	"encoding/json"
	"os"
	"path/filepath"

	"kanade/internal/paths"
)

// Engine 是 Conductor 中 CosyVoice 引擎的名称。
const Engine = "cosyvoice"

// Family 是模型版本，决定可用模式与提示文本格式。
type Family string

const (
	FamilyV1         Family = "v1"
	FamilyV1SFT      Family = "v1-sft"
	FamilyV1Instruct Family = "v1-instruct"
	FamilyV2         Family = "v2"
	FamilyV3         Family = "v3"
	// FamilyV3SFT 是在 Kanade 中微调出的 CosyVoice 3 说话人模型（目录含 kanade-sft.json），只有预训练音色模式。
	FamilyV3SFT Family = "v3-sft"
)

// Mode 是推理模式，同时是引擎任务名的后缀（cosyvoice.<Mode>）。
type Mode string

const (
	ModeZeroShot     Mode = "zero_shot"
	ModeCrossLingual Mode = "cross_lingual"
	ModeSFT          Mode = "sft"
	ModeInstruct     Mode = "instruct"
	ModeVC           Mode = "vc"
)

// ModeTitle 是模式的界面名称，沿用上游 WebUI 的叫法。
var ModeTitle = map[Mode]string{
	ModeZeroShot:     "零样本克隆",
	ModeCrossLingual: "跨语种复刻",
	ModeSFT:          "预训练音色",
	ModeInstruct:     "自然语言控制",
	ModeVC:           "声音转换",
}

// familyModes 依据上游 example.py 中各版本演示的接口：CosyVoice 1 的预训练音色与指令能力是单独的 SFT / Instruct 模型，
// 声音转换只在 CosyVoice 1 基础模型上有官方示例。
var familyModes = map[Family][]Mode{
	FamilyV1:         {ModeZeroShot, ModeCrossLingual, ModeVC},
	FamilyV1SFT:      {ModeSFT},
	FamilyV1Instruct: {ModeInstruct},
	FamilyV2:         {ModeZeroShot, ModeCrossLingual, ModeInstruct},
	FamilyV3:         {ModeZeroShot, ModeCrossLingual, ModeInstruct},
	FamilyV3SFT:      {ModeSFT},
}

// Source 是模型下载源。
type Source string

const (
	SourceModelScope  Source = "modelscope"
	SourceHuggingFace Source = "huggingface"
)

// Model 是一个可下载的 CosyVoice 模型。
type Model struct {
	// Key 同时是模型在数据根目录中的目录名；引擎按其后缀区分 CosyVoice 1 的 SFT / Instruct 模型。
	Key         string
	Title       string
	Family      Family
	ModelScope  string // ModelScope 仓库 ID
	HuggingFace string // Hugging Face 仓库 ID
	// SizeGB 是只下载推理所需文件时的大小（2026-10 查询 ModelScope 文件列表所得），用于下载前提示。
	SizeGB float64
	// Speakers 是 SFT 模型内置的音色；为空时 CosyVoice 1 SFT / Instruct 模型使用 SFTSpeakers。
	Speakers []string
}

// Models 是支持的模型，推荐的排在前面。
var Models = []Model{
	{"Fun-CosyVoice3-0.5B", "Fun-CosyVoice 3 · 0.5B（推荐）", FamilyV3, "FunAudioLLM/Fun-CosyVoice3-0.5B-2512", "FunAudioLLM/Fun-CosyVoice3-0.5B-2512", 5.4, nil},
	{"CosyVoice2-0.5B", "CosyVoice 2 · 0.5B", FamilyV2, "iic/CosyVoice2-0.5B", "FunAudioLLM/CosyVoice2-0.5B", 4.1, nil},
	{"CosyVoice-300M", "CosyVoice 1 · 300M 基础", FamilyV1, "iic/CosyVoice-300M", "FunAudioLLM/CosyVoice-300M", 2.3, nil},
	{"CosyVoice-300M-SFT", "CosyVoice 1 · 300M 预训练音色", FamilyV1SFT, "iic/CosyVoice-300M-SFT", "FunAudioLLM/CosyVoice-300M-SFT", 2.3, nil},
	{"CosyVoice-300M-Instruct", "CosyVoice 1 · 300M 指令", FamilyV1Instruct, "iic/CosyVoice-300M-Instruct", "FunAudioLLM/CosyVoice-300M-Instruct", 2.3, nil},
}

// AllModels 返回内置模型与数据根目录中训练出的 SFT 模型（<名称>-SFT/kanade-sft.json）。
func AllModels(l paths.Layout) []Model {
	out := append([]Model(nil), Models...)
	dirs, _ := filepath.Glob(filepath.Join(Root(l), "*-SFT", "kanade-sft.json"))
	for _, meta := range dirs {
		data, err := os.ReadFile(meta)
		if err != nil {
			continue
		}
		var info struct {
			SpkID string `json:"spk_id"`
			Base  string `json:"base"`
		}
		if json.Unmarshal(data, &info) != nil || info.SpkID == "" {
			continue
		}
		key := filepath.Base(filepath.Dir(meta))
		out = append(out, Model{Key: key, Title: info.SpkID + "（微调音色）", Family: FamilyV3SFT, Speakers: []string{info.SpkID}})
	}
	return out
}

// SpeakerList 返回预训练音色模式可选的音色。
func (m Model) SpeakerList() []string {
	if len(m.Speakers) > 0 {
		return m.Speakers
	}
	return SFTSpeakers
}

// Modes 返回模型支持的模式，顺序即界面顺序。
func (m Model) Modes() []Mode { return familyModes[m.Family] }

// Supports 报告模型是否支持 mode。
func (m Model) Supports(mode Mode) bool {
	for _, x := range m.Modes() {
		if x == mode {
			return true
		}
	}
	return false
}

// Repo 返回模型在 source 上的仓库 ID。
func (m Model) Repo(s Source) string {
	if s == SourceHuggingFace {
		return m.HuggingFace
	}
	return m.ModelScope
}

// Root 是 CosyVoice 模型与音色在数据根目录中的位置。
func Root(l paths.Layout) string { return filepath.Join(l.Models, "cosyvoice") }

// Dir 返回模型目录。
func (m Model) Dir(l paths.Layout) string { return filepath.Join(Root(l), m.Key) }

// completeMarker 由引擎在全部文件下载完成后写入，见 engines/cosyvoice/src/kanade_cosyvoice/download.py。
const completeMarker = ".kanade-complete"

// Installed 报告模型是否已完整下载。
func (m Model) Installed(l paths.Layout) bool {
	_, err := os.Stat(filepath.Join(m.Dir(l), completeMarker))
	return err == nil
}

// SFTSpeakers 是 CosyVoice-300M-SFT / -Instruct 内置的预训练音色（上游 README 所列；引擎执行时会再校验）。
var SFTSpeakers = []string{"中文女", "中文男", "日语男", "粤语女", "英文女", "英文男", "韩语女"}

// InstructPresets 是常用指令，取自上游 cosyvoice/utils/common.py 的 instruct_list（去掉系统提示与结束标记，
// 由引擎按模型版本重新包装）。CosyVoice 1 Instruct 模型更适合描述人物风格的英文指令，界面另给示例。
var InstructPresets = []string{
	"请用广东话表达。", "请用四川话表达。", "请用东北话表达。", "请用上海话表达。", "请用闽南话表达。",
	"请非常开心地说一句话。", "请非常伤心地说一句话。", "请非常生气地说一句话。",
	"请用尽可能慢地语速说一句话。", "请用尽可能快地语速说一句话。",
	"Please say a sentence as loudly as possible.", "Please say a sentence in a very soft voice.",
	"你可以尝试用机器人的方式解答吗？",
}
