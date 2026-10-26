// Package cantus 描述语音生成（Cantus）的模型、模式与已保存音色，并把界面输入组装为引擎任务参数。
//
// 模型来自四个引擎：CosyVoice、Qwen3-TTS、VoxCPM 与 IndexTTS，模型版本（Family）决定由哪个引擎执行。
// 模型与模式的对应关系须与各引擎 tasks.py 的 MODES 保持一致（engines/<引擎>/src/kanade_<引擎>/tasks.py）。
package cantus

import (
	"encoding/json"
	"os"
	"path/filepath"

	"kanade/internal/paths"
)

// Engine 是 Conductor 中 CosyVoice 引擎的名称；说话人微调（sft.go）只由它执行。其他引擎见 Family.Engine。
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

	// Qwen3-TTS 的三类模型（config.json 的 tts_model_type）：Base 克隆、VoiceDesign 文字设计音色、CustomVoice 预设音色。
	FamilyQwenBase   Family = "qwen3-base"
	FamilyQwenDesign Family = "qwen3-design"
	FamilyQwenCustom Family = "qwen3-custom"
	// FamilyVoxCPM2 支持只用参考音频克隆、括号风格控制与音色设计；FamilyVoxCPM（1.5）只能参考音频 + 文本续写克隆。
	FamilyVoxCPM2 Family = "voxcpm2"
	FamilyVoxCPM  Family = "voxcpm"
	// FamilyIndexTTS 是 IndexTTS-2.5：只用参考音频克隆，情绪可按文字描述控制，可调语速。
	FamilyIndexTTS Family = "indextts2.5"
)

// Engine 返回执行该版本模型的引擎名（Conductor 中的引擎，也是任务名前缀与模型目录的上级目录名）。
func (f Family) Engine() string {
	switch f {
	case FamilyQwenBase, FamilyQwenDesign, FamilyQwenCustom:
		return "qwentts"
	case FamilyVoxCPM2, FamilyVoxCPM:
		return "voxcpm"
	case FamilyIndexTTS:
		return "indextts"
	}
	return Engine
}

// Mode 是推理模式，同时是引擎任务名的后缀（cosyvoice.<Mode>）。
type Mode string

const (
	ModeZeroShot     Mode = "zero_shot"
	ModeCrossLingual Mode = "cross_lingual"
	ModeSFT          Mode = "sft"
	ModeInstruct     Mode = "instruct"
	ModeVC           Mode = "vc"
	// ModeDesign 不用参考音频，按文字描述生成音色（Qwen3-TTS VoiceDesign、VoxCPM2）。
	ModeDesign Mode = "voice_design"
)

// ModeTitle 是模式的界面名称，沿用上游 WebUI 的叫法。
var ModeTitle = map[Mode]string{
	ModeZeroShot:     "零样本克隆",
	ModeCrossLingual: "跨语种复刻",
	ModeSFT:          "预训练音色",
	ModeInstruct:     "自然语言控制",
	ModeVC:           "声音转换",
	ModeDesign:       "音色设计",
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
	FamilyQwenBase:   {ModeZeroShot, ModeCrossLingual},
	FamilyQwenDesign: {ModeDesign},
	FamilyQwenCustom: {ModeSFT},
	FamilyVoxCPM2:    {ModeZeroShot, ModeCrossLingual, ModeInstruct, ModeDesign},
	FamilyVoxCPM:     {ModeZeroShot},
	FamilyIndexTTS:   {ModeCrossLingual, ModeInstruct},
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

// Models 是支持的模型，按引擎分组，组内推荐的排在前面。Models[0] 同时是说话人微调的基础模型（SFTBase）。
var Models = []Model{
	{"Fun-CosyVoice3-0.5B", "Fun-CosyVoice 3 · 0.5B（推荐）", FamilyV3, "FunAudioLLM/Fun-CosyVoice3-0.5B-2512", "FunAudioLLM/Fun-CosyVoice3-0.5B-2512", 5.4, nil},
	{"CosyVoice2-0.5B", "CosyVoice 2 · 0.5B", FamilyV2, "iic/CosyVoice2-0.5B", "FunAudioLLM/CosyVoice2-0.5B", 4.1, nil},
	{"CosyVoice-300M", "CosyVoice 1 · 300M 基础", FamilyV1, "iic/CosyVoice-300M", "FunAudioLLM/CosyVoice-300M", 2.3, nil},
	{"CosyVoice-300M-SFT", "CosyVoice 1 · 300M 预训练音色", FamilyV1SFT, "iic/CosyVoice-300M-SFT", "FunAudioLLM/CosyVoice-300M-SFT", 2.3, nil},
	{"CosyVoice-300M-Instruct", "CosyVoice 1 · 300M 指令", FamilyV1Instruct, "iic/CosyVoice-300M-Instruct", "FunAudioLLM/CosyVoice-300M-Instruct", 2.3, nil},
	// Qwen3-TTS（2026-01，Apache-2.0）：ModelScope 与 Hugging Face 仓库 ID 相同。
	{"Qwen3-TTS-12Hz-1.7B-Base", "Qwen3-TTS · 1.7B 音色克隆", FamilyQwenBase, "Qwen/Qwen3-TTS-12Hz-1.7B-Base", "Qwen/Qwen3-TTS-12Hz-1.7B-Base", 4.5, nil},
	{"Qwen3-TTS-12Hz-1.7B-VoiceDesign", "Qwen3-TTS · 1.7B 音色设计", FamilyQwenDesign, "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign", "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign", 4.5, nil},
	{"Qwen3-TTS-12Hz-1.7B-CustomVoice", "Qwen3-TTS · 1.7B 预设音色", FamilyQwenCustom, "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice", "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice", 4.5, QwenSpeakers},
	{"Qwen3-TTS-12Hz-0.6B-Base", "Qwen3-TTS · 0.6B 音色克隆（低显存）", FamilyQwenBase, "Qwen/Qwen3-TTS-12Hz-0.6B-Base", "Qwen/Qwen3-TTS-12Hz-0.6B-Base", 2.5, nil},
	{"Qwen3-TTS-12Hz-0.6B-CustomVoice", "Qwen3-TTS · 0.6B 预设音色（低显存）", FamilyQwenCustom, "Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice", "Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice", 2.5, QwenSpeakers},
	// VoxCPM（OpenBMB，Apache-2.0）：ModelScope 组织名为 OpenBMB，Hugging Face 为 openbmb。
	{"VoxCPM2", "VoxCPM2 · 2B（48 kHz）", FamilyVoxCPM2, "OpenBMB/VoxCPM2", "openbmb/VoxCPM2", 5.0, nil},
	{"VoxCPM1.5", "VoxCPM 1.5 · 0.75B（中英文）", FamilyVoxCPM, "OpenBMB/VoxCPM1.5", "openbmb/VoxCPM1.5", 2.0, nil},
	// IndexTTS-2.5：主模型 5.5 GB + 附属模型（w2v-bert-2.0、CAMPPlus、BigVGAN）2.8 GB。bilibili 模型使用协议，商用需另行授权。
	{"IndexTTS-2.5", "IndexTTS-2.5（非商用许可）", FamilyIndexTTS, "IndexTeam/IndexTTS-2.5", "IndexTeam/IndexTTS-2.5", 8.3, nil},
}

// QwenSpeakers 是 Qwen3-TTS CustomVoice 模型的预设音色（上游 README；引擎执行时按模型实际支持的列表再校验）。
// 前五个的母语为中文（Dylan 北京话、Eric 四川话），Ryan / Aiden 为英语，Ono_Anna 为日语，Sohee 为韩语。
var QwenSpeakers = []string{"Vivian", "Serena", "Uncle_Fu", "Dylan", "Eric", "Ryan", "Aiden", "Ono_Anna", "Sohee"}

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

// Root 是 CosyVoice 模型、音色库与说话人微调在数据根目录中的位置；音色库与模型无关，各引擎共用这里的 speakers/。
func Root(l paths.Layout) string { return filepath.Join(l.Models, Engine) }

// Engine 返回执行该模型的引擎名。
func (m Model) Engine() string { return m.Family.Engine() }

// Dir 返回模型目录：<models>/<引擎>/<Key>。
func (m Model) Dir(l paths.Layout) string { return filepath.Join(l.Models, m.Engine(), m.Key) }

// ModeTitle 返回模式在该模型上的界面名称：CosyVoice 沿用上游 WebUI 的叫法，其他引擎按实际行为命名
// （它们的「跨语种复刻」就是只用参考音频克隆，「自然语言控制」分别是风格与情绪控制）。
func (m Model) ModeTitle(mode Mode) string {
	switch {
	case m.Engine() == Engine:
	case mode == ModeCrossLingual:
		return "音频克隆"
	case mode == ModeInstruct && m.Family == FamilyIndexTTS:
		return "情绪控制"
	case mode == ModeInstruct:
		return "风格控制"
	case mode == ModeSFT:
		return "预设音色"
	}
	return ModeTitle[mode]
}

// HasSpeed 报告模型能否调节语速：CosyVoice 各版本与 IndexTTS-2.5 有语速参数，Qwen3-TTS 与 VoxCPM 没有。
func (m Model) HasSpeed() bool { return m.Engine() == Engine || m.Family == FamilyIndexTTS }

// HasTuning 报告模型是否有 CosyVoice 专有的文本归一化开关与 LLM ras 采样参数。
func (m Model) HasTuning() bool { return m.Engine() == Engine }

// completeMarker 由引擎在全部文件下载完成后写入，见 engines/cosyvoice/src/kanade_cosyvoice/download.py。
const completeMarker = ".kanade-complete"

// Installed 报告模型是否已完整下载。
func (m Model) Installed(l paths.Layout) bool {
	_, err := os.Stat(filepath.Join(m.Dir(l), completeMarker))
	return err == nil
}

// SFTSpeakers 是 CosyVoice-300M-SFT / -Instruct 内置的预训练音色（上游 README 所列；引擎执行时会再校验）。
var SFTSpeakers = []string{"中文女", "中文男", "日语男", "粤语女", "英文女", "英文男", "韩语女"}

// DesignPresets 是音色设计的示例描述（Qwen3-TTS VoiceDesign 与 VoxCPM2 都接受中文自然语言描述）。
var DesignPresets = []string{
	"年轻女性，声音温柔甜美，语速适中",
	"中年男性，嗓音低沉浑厚，沉稳有磁性",
	"少年，声音清亮有活力，语气轻快",
	"老年男性，声音沙哑缓慢，慈祥温和",
	"新闻播音员，字正腔圆，语气庄重",
}

// StylePresets 是 VoxCPM2 风格控制的示例（作为括号描述加在每段正文前）。
var StylePresets = []string{"稍快，开心", "语速放慢，温柔", "激动，大声", "低声细语", "悲伤，带哭腔"}

// EmotionPresets 是 IndexTTS 情绪控制的示例（上游 QwenEmotion 从文字推断八种情绪的强度）。
var EmotionPresets = []string{"非常开心地说", "愤怒地质问", "伤心低落地说", "害怕、紧张地说", "惊讶地说", "平静地叙述"}

// InstructPresets 是常用指令，取自上游 cosyvoice/utils/common.py 的 instruct_list（去掉系统提示与结束标记，
// 由引擎按模型版本重新包装）。CosyVoice 1 Instruct 模型更适合描述人物风格的英文指令，界面另给示例。
var InstructPresets = []string{
	"请用广东话表达。", "请用四川话表达。", "请用东北话表达。", "请用上海话表达。", "请用闽南话表达。",
	"请非常开心地说一句话。", "请非常伤心地说一句话。", "请非常生气地说一句话。",
	"请用尽可能慢地语速说一句话。", "请用尽可能快地语速说一句话。",
	"Please say a sentence as loudly as possible.", "Please say a sentence in a very soft voice.",
	"你可以尝试用机器人的方式解答吗？",
}
