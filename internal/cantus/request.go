package cantus

import (
	"errors"
	"fmt"
	"strings"

	"kanade/internal/paths"
)

// Request 是一次合成的界面输入。按模式只使用其中部分字段，见 Params。
type Request struct {
	Model Model
	Mode  Mode
	// Text 是要合成的文本（声音转换不使用）。
	Text string
	// 参考音色：Speaker 非空时使用已保存音色，否则使用 PromptWav（及零样本克隆所需的 PromptText）。
	Speaker    *Speaker
	PromptWav  string
	PromptText string
	// SpkID 是预训练音色名，用于预训练音色模式与 CosyVoice 1 指令模型。
	SpkID string
	// Instruct 是自然语言指令，例如「请用四川话表达。」。
	Instruct string
	// SourceWav 是声音转换的源音频。
	SourceWav string
	// Speed 是语速倍率，0 视为 1。
	Speed float64
	// TextFrontend 控制是否做文本归一化（数字、符号读法等）；关闭可复现上游演示或保留特殊标记。
	TextFrontend bool
	// Seed 非 nil 且非负时固定随机种子；nil 时微调模型用引擎默认种子，其他模型每次随机。
	Seed *int
	// Sampling 覆盖 LLM 采样参数；nil 时使用引擎默认（微调模型为保守采样，其他为模型自带）。
	Sampling *Sampling
}

// Sampling 是 CosyVoice LLM 的 ras_sampling 参数（专业模式）。
type Sampling struct {
	TopP    float64 // 核采样累计概率，0–1
	TopK    int     // 候选 token 数
	WinSize int     // 重复检测窗口（token 数）
	TauR    float64 // 窗口内重复比例超过它时改为随机采样，0–1
}

// DefaultSampling 是上游各版本 yaml 中的默认采样参数；SFTSampling 是微调模型的保守采样
// （VoiceLab cosyvoice_sft_param_playbook 的 sampling06k10，与引擎 tasks.SFT_SAMPLING 一致）。
var (
	DefaultSampling = Sampling{TopP: 0.8, TopK: 25, WinSize: 10, TauR: 0.1}
	SFTSampling     = Sampling{TopP: 0.6, TopK: 10, WinSize: 10, TauR: 1.0}
)

func (s Sampling) validate() error {
	if s.TopP <= 0 || s.TopP > 1 || s.TopK < 1 || s.TopK > 200 || s.WinSize < 1 || s.WinSize > 100 || s.TauR < 0 || s.TauR > 1 {
		return errors.New("采样参数超出范围：top_p 0–1、top_k 1–200、窗口 1–100、重复阈值 0–1")
	}
	return nil
}

// SmartMode 返回智能模式下的推理模式：微调与预训练音色模型用预训练音色；填写了风格（instruct 非空）且模型支持时
// 用自然语言控制；有参考文本时用零样本克隆（同语种音色最像），否则用跨语种复刻（不需要参考文本）。
func SmartMode(m Model, hasPromptText, hasStyle bool) Mode {
	modes := m.Modes()
	switch {
	case m.Supports(ModeSFT):
		return ModeSFT
	case hasStyle && m.Supports(ModeInstruct):
		return ModeInstruct
	case hasPromptText && m.Supports(ModeZeroShot):
		return ModeZeroShot
	case m.Supports(ModeCrossLingual):
		return ModeCrossLingual
	}
	return modes[0]
}

// Speed 的取值范围，与上游 WebUI 的滑块一致。
const (
	MinSpeed = 0.5
	MaxSpeed = 2.0
)

// Task 返回引擎任务名。
func (r Request) Task() string { return Engine + "." + string(r.Mode) }

// Params 校验输入并返回引擎任务参数。错误信息面向用户。
func (r Request) Params(l paths.Layout) (map[string]any, error) {
	if !r.Model.Supports(r.Mode) {
		return nil, fmt.Errorf("%s 不支持%s", r.Model.Title, ModeTitle[r.Mode])
	}
	if !r.Model.Installed(l) {
		return nil, fmt.Errorf("请先下载模型 %s", r.Model.Title)
	}
	speed := r.Speed
	if speed == 0 {
		speed = 1
	}
	if speed < MinSpeed || speed > MaxSpeed {
		return nil, fmt.Errorf("语速须在 %.1f–%.1f 之间", MinSpeed, MaxSpeed)
	}
	p := map[string]any{"model_dir": r.Model.Dir(l), "speed": speed, "text_frontend": r.TextFrontend}
	if r.Seed != nil {
		p["seed"] = *r.Seed
	}
	if r.Sampling != nil {
		if err := r.Sampling.validate(); err != nil {
			return nil, err
		}
		p["sampling"] = map[string]any{"top_p": r.Sampling.TopP, "top_k": r.Sampling.TopK, "win_size": r.Sampling.WinSize, "tau_r": r.Sampling.TauR}
	}

	if r.Mode != ModeVC {
		if strings.TrimSpace(r.Text) == "" {
			return nil, errors.New("请填写要合成的文本")
		}
		p["text"] = r.Text
	}

	needSpk := r.Mode == ModeSFT || (r.Mode == ModeInstruct && r.Model.Family == FamilyV1Instruct)
	if needSpk {
		if r.SpkID == "" {
			return nil, errors.New("请选择预训练音色")
		}
		p["spk_id"] = r.SpkID
	} else if err := r.voice(l, p); err != nil {
		return nil, err
	}

	switch r.Mode {
	case ModeSFT:
		// 微调模型可选的风格引导（注入为 CosyVoice 3 的提示文本），为空时只注入结束标记。
		if r.Model.Family == FamilyV3SFT && strings.TrimSpace(r.Instruct) != "" {
			p["instruct"] = r.Instruct
		}
	case ModeInstruct:
		if strings.TrimSpace(r.Instruct) == "" {
			return nil, errors.New("请填写指令")
		}
		p["instruct"] = r.Instruct
	case ModeVC:
		if r.SourceWav == "" {
			return nil, errors.New("请选择要转换的源音频")
		}
		p["source_wav"] = r.SourceWav
	}
	return p, nil
}

// voice 填入参考音色参数：已保存音色优先，否则使用参考音频。零样本克隆还需要参考音频的文本。
func (r Request) voice(l paths.Layout, p map[string]any) error {
	if r.Speaker != nil {
		if r.Mode == ModeZeroShot && r.Speaker.PromptText == "" {
			return fmt.Errorf("音色「%s」没有参考文本，不能用于零样本克隆；可改用跨语种复刻", r.Speaker.Name)
		}
		p["speaker_dir"] = r.Speaker.Dir(l)
		return nil
	}
	if r.PromptWav == "" {
		return errors.New("请选择参考音频或已保存的音色")
	}
	p["prompt_wav"] = r.PromptWav
	if r.Mode == ModeZeroShot {
		if strings.TrimSpace(r.PromptText) == "" {
			return errors.New("零样本克隆需要填写参考音频对应的文本")
		}
		p["prompt_text"] = r.PromptText
	}
	return nil
}

// DownloadParams 返回下载模型的任务参数（任务名 cosyvoice.download）。
func DownloadParams(l paths.Layout, m Model, s Source) map[string]any {
	return map[string]any{"source": string(s), "repo": m.Repo(s), "model_dir": m.Dir(l)}
}
