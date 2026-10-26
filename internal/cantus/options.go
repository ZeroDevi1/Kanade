package cantus

import (
	"fmt"
	"math"
)

// Option 是 Qwen3-TTS、VoxCPM、IndexTTS 引擎专有的一个推理参数：专业模式逐项显示，智能模式一律取 Default（推荐值）。
// CosyVoice 的语速、文本归一化与 ras 采样参数沿用 Request 中的专用字段，不在这里。
//
// Default 取自各上游的推荐配置（Qwen3-TTS 模型的 generation_config.json、VoxCPM README、IndexTTS infer_v2_5 的默认值），
// 引擎 tasks.py 在参数缺省时使用同样的值；两处须保持一致。
type Option struct {
	Key   string // 任务参数 options 中的键，与引擎 tasks.py 读取的名称一致
	Label string
	Hint  string // 一句说明，界面上作为提示
	// Min / Max / Step 是取值范围与步长；开关（Bool）为 0 / 1。
	Min, Max, Step float64
	Default        float64
	Bool           bool   // 开关：值为 0（关）或 1（开）
	Int            bool   // 整数：提交时取整
	Modes          []Mode // 只在这些模式下生效；为空时所有模式都生效
}

// qwenOptions 对应 Qwen3TTSModel.generate_* 转给 transformers generate 的采样参数；talker 生成第一层 codec，
// subtalker 生成其余各层（音色与音质细节）。
var qwenOptions = []Option{
	{Key: "temperature", Label: "温度", Hint: "越低越稳定、越单调；越高越有变化，也越容易读错", Min: 0.1, Max: 1.5, Step: 0.05, Default: 0.9},
	{Key: "top_p", Label: "top_p", Hint: "核采样累计概率，越小越保守", Min: 0.1, Max: 1, Step: 0.05, Default: 1.0},
	{Key: "top_k", Label: "top_k", Hint: "每步候选数，越小越保守", Min: 1, Max: 200, Step: 1, Default: 50, Int: true},
	{Key: "repetition_penalty", Label: "重复惩罚", Hint: "大于 1 时抑制重复与拖长，过大会吞字", Min: 1, Max: 2, Step: 0.01, Default: 1.05},
	{Key: "subtalker_temperature", Label: "细节温度", Hint: "subtalker（音质细节层）的温度", Min: 0.1, Max: 1.5, Step: 0.05, Default: 0.9},
	{Key: "subtalker_top_k", Label: "细节 top_k", Hint: "subtalker 的每步候选数", Min: 1, Max: 200, Step: 1, Default: 50, Int: true},
	{Key: "subtalker_top_p", Label: "细节 top_p", Hint: "subtalker 的核采样累计概率", Min: 0.1, Max: 1, Step: 0.05, Default: 1.0},
}

// voxOptions 对应 VoxCPM.generate 的参数。
var voxOptions = []Option{
	{Key: "cfg_value", Label: "CFG 强度", Hint: "越高越贴合文本与参考音色，过高会失真", Min: 1, Max: 4, Step: 0.1, Default: 2.0},
	{Key: "inference_timesteps", Label: "扩散步数", Hint: "越多音质越好、越慢", Min: 4, Max: 50, Step: 1, Default: 10, Int: true},
	{Key: "normalize", Label: "文本归一化（数字、符号读法）", Hint: "用 wetext 把数字、日期、符号转成读法", Bool: true, Default: 0, Max: 1},
	{Key: "retry_badcase", Label: "失控时自动重试", Hint: "音频与文本长度比异常时由模型自己重试", Bool: true, Default: 1, Max: 1},
	{Key: "retry_badcase_max_times", Label: "重试次数", Hint: "每段最多自动重试的次数", Min: 1, Max: 10, Step: 1, Default: 3, Int: true},
	{Key: "retry_badcase_ratio_threshold", Label: "失控阈值", Hint: "音频长度与文本长度之比超过它时视为失控", Min: 2, Max: 20, Step: 0.5, Default: 6.0},
}

// indexOptions 对应 IndexTTS2.infer 的参数与转给 GPT 生成的采样参数（infer_v2_5.infer_generator 的默认值）。
var indexOptions = []Option{
	{Key: "temperature", Label: "温度", Hint: "越低越稳定、越单调", Min: 0.1, Max: 1.5, Step: 0.05, Default: 0.8},
	{Key: "top_p", Label: "top_p", Hint: "核采样累计概率，越小越保守", Min: 0.1, Max: 1, Step: 0.05, Default: 0.8},
	{Key: "top_k", Label: "top_k", Hint: "每步候选数，越小越保守", Min: 1, Max: 200, Step: 1, Default: 30, Int: true},
	{Key: "num_beams", Label: "束搜索宽度", Hint: "越大越稳定、越慢；1 为不用束搜索", Min: 1, Max: 8, Step: 1, Default: 3, Int: true},
	{Key: "repetition_penalty", Label: "重复惩罚", Hint: "抑制重复与拖长", Min: 1, Max: 20, Step: 0.5, Default: 10.0},
	{Key: "text_normalization", Label: "文本归一化（数字、符号读法）", Hint: "上游的中英文文本归一化", Bool: true, Default: 1, Max: 1},
	{Key: "emo_alpha", Label: "情绪强度", Hint: "按文字推断的情绪混入的比例，上游建议 0.6 左右更自然", Min: 0, Max: 1, Step: 0.05, Default: 0.6,
		Modes: []Mode{ModeInstruct}},
}

// Options 返回模型在 mode 下可调的引擎专有参数；CosyVoice 返回 nil。
func (m Model) Options(mode Mode) []Option {
	var all []Option
	switch m.Engine() {
	case "qwentts":
		all = qwenOptions
	case "voxcpm":
		all = voxOptions
	case "indextts":
		all = indexOptions
	}
	var out []Option
	for _, o := range all {
		if len(o.Modes) == 0 || containsMode(o.Modes, mode) {
			out = append(out, o)
		}
	}
	return out
}

func containsMode(ms []Mode, m Mode) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

// options 返回提交给引擎的 options：每项取 values 中的值（缺省为推荐值）并校验范围。错误信息面向用户。
func (r Request) options() (map[string]any, error) {
	opts := r.Model.Options(r.Mode)
	if len(opts) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(opts))
	for _, o := range opts {
		v, ok := r.Options[o.Key]
		if !ok {
			v = o.Default
		}
		switch {
		case o.Bool:
			out[o.Key] = v != 0
			continue
		case math.IsNaN(v) || v < o.Min || v > o.Max:
			return nil, fmt.Errorf("%s须在 %g–%g 之间", o.Label, o.Min, o.Max)
		case o.Int:
			out[o.Key] = int(math.Round(v))
		default:
			out[o.Key] = v
		}
	}
	return out, nil
}
