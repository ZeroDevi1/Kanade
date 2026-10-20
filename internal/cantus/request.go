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
