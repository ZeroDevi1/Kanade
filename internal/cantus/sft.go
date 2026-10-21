package cantus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"kanade/internal/paths"
)

// SFT（说话人微调）以 Fun-CosyVoice 3 为基础模型，流程与参数见 engines/cosyvoice/src/kanade_cosyvoice/sft.py。

// SFTBase 是 SFT 的基础模型。
func SFTBase() Model { return Models[0] }

// ASRModels 是数据缺少文本时可用的 Whisper 转写模型；第一项表示不转写（跳过无文本的条目）。
var ASRModels = []string{"不转写", "small", "medium", "large-v3"}

// SFTParts 是可训练的部分：只训 LLM（推荐，小样本下 Flow 易过拟合），或 LLM 与 Flow 都训。
var SFTParts = []struct{ Key, Title string }{
	{"llm", "只训练 LLM（推荐）"},
	{"both", "LLM + Flow"},
}

// SFTLLMScopes 是 LLM 的训练范围：Layers 为只训练的 Qwen 顶部层数，0 表示全量，-1 表示按显存自动选择。
// 全量 fp32 Adam 需要约 10 GB 显存加激活（实测见 docs/windows-test-plan.md）；顶部 12 层可在 8 GB 显卡上训练。
var SFTLLMScopes = []struct {
	Layers int
	Title  string
}{
	{-1, "按显存自动选择"},
	{12, "顶部 12 层（8 GB 显存）"},
	{0, "全部层（12 GB 以上显存）"},
}

// SFTSmartASR 是智能模式在 .list 缺文本时使用的 Whisper 模型：medium 在中文上比 small 准确得多，
// 约 5 GB 显存，8 GB 显卡可用（large-v3 约需 10 GB）。
const SFTSmartASR = "medium"

// SFTTrain 是一次 SFT 训练的参数。零值的 LR、Workers 使用引擎默认（1e-5、4）。
type SFTTrain struct {
	Name      string
	Part      string // llm / both
	Epochs    int
	LLMLayers int // 取自 SFTLLMScopes
	LR        float64
	Workers   int
}

var sftNameRE = regexp.MustCompile(`^[\p{Han}\w-]+$`)

// SFTPrepareParams 返回 cosyvoice.sft_prepare 的任务参数。asr 为 ASRModels 中的一项；
// devRatio 为验证集比例（0 为引擎默认 0.05）。
func SFTPrepareParams(l paths.Layout, name, datasetDir, asr string, devRatio float64) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if !sftNameRE.MatchString(name) {
		return nil, errors.New("音色名只能包含字母、数字、中文、下划线与连字符")
	}
	if st, err := os.Stat(datasetDir); datasetDir == "" || err != nil || !st.IsDir() {
		return nil, errors.New("请选择数据集目录")
	}
	base := SFTBase()
	if !base.Installed(l) {
		return nil, fmt.Errorf("请先下载基础模型 %s", base.Title)
	}
	p := map[string]any{"name": name, "dataset_dir": datasetDir, "base_model_dir": base.Dir(l)}
	if asr != "" && asr != ASRModels[0] {
		p["asr_model"] = asr
	}
	if devRatio < 0 || devRatio > 0.5 {
		return nil, errors.New("验证集比例须在 0–0.5 之间")
	}
	if devRatio > 0 {
		p["dev_ratio"] = devRatio
	}
	return p, nil
}

// SFTPrepared 报告名为 name 的 SFT 数据是否已准备好。
func SFTPrepared(l paths.Layout, name string) bool {
	_, err := os.Stat(filepath.Join(Root(l), "_sft", strings.TrimSpace(name), "prepare.json"))
	return err == nil
}

// SFTTrainParams 返回 cosyvoice.sft_train 的任务参数。
func SFTTrainParams(l paths.Layout, t SFTTrain) (map[string]any, error) {
	name := strings.TrimSpace(t.Name)
	if !SFTPrepared(l, name) {
		return nil, errors.New("请先完成数据准备")
	}
	if t.Part != "llm" && t.Part != "both" {
		return nil, fmt.Errorf("未知的训练部分 %q", t.Part)
	}
	if t.Epochs < 1 || t.Epochs > 1000 {
		return nil, errors.New("轮数须在 1–1000 之间")
	}
	if t.LLMLayers < -1 {
		return nil, errors.New("LLM 训练层数无效")
	}
	if t.LR < 0 || t.LR > 1e-2 || t.Workers < 0 || t.Workers > 32 {
		return nil, errors.New("学习率须在 0–0.01 之间，数据加载进程数 0–32")
	}
	p := map[string]any{"name": name, "base_model_dir": SFTBase().Dir(l), "part": t.Part, "epochs": t.Epochs, "llm_layers": t.LLMLayers}
	if t.LR > 0 {
		p["lr"] = t.LR
	}
	if t.Workers > 0 {
		p["num_workers"] = t.Workers
	}
	return p, nil
}

// SFTCheckpoints 返回名为 name 的 SFT 已保存 checkpoint 的训练部分（llm / flow）与轮数合计，
// 与引擎 sft.saved_parts 的判断一致（exp/<部分>/epoch_*_whole.yaml）。
func SFTCheckpoints(l paths.Layout, name string) (parts []string, epochs int) {
	for _, part := range []string{"llm", "flow"} {
		files, _ := filepath.Glob(filepath.Join(Root(l), "_sft", strings.TrimSpace(name), "exp", part, "epoch_*_whole.yaml"))
		if len(files) > 0 {
			parts, epochs = append(parts, part), epochs+len(files)
		}
	}
	return parts, epochs
}

// SFTAssembleParams 返回 cosyvoice.sft_assemble 的任务参数：用已保存的 checkpoint 平均并组装模型。
func SFTAssembleParams(l paths.Layout, name string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if parts, _ := SFTCheckpoints(l, name); len(parts) == 0 {
		return nil, errors.New("没有已保存的 checkpoint，请先训练")
	}
	return map[string]any{"name": name, "base_model_dir": SFTBase().Dir(l)}, nil
}
