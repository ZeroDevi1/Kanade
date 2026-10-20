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

// SFTLLMScopes 是 LLM 的训练范围：Layers 为只训练的 Qwen 顶部层数，0 表示全量。
// 全量 fp32 Adam 需要约 10 GB 显存加激活（实测见 docs/windows-test-plan.md）；顶部 12 层可在 8 GB 显卡上训练。
var SFTLLMScopes = []struct {
	Layers int
	Title  string
}{
	{12, "顶部 12 层（8 GB 显存）"},
	{0, "全部层（12 GB 以上显存）"},
}

var sftNameRE = regexp.MustCompile(`^[\p{Han}\w-]+$`)

// SFTPrepareParams 返回 cosyvoice.sft_prepare 的任务参数。asr 为 ASRModels 中的一项。
func SFTPrepareParams(l paths.Layout, name, datasetDir, asr string) (map[string]any, error) {
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
	return p, nil
}

// SFTPrepared 报告名为 name 的 SFT 数据是否已准备好。
func SFTPrepared(l paths.Layout, name string) bool {
	_, err := os.Stat(filepath.Join(Root(l), "_sft", strings.TrimSpace(name), "prepare.json"))
	return err == nil
}

// SFTTrainParams 返回 cosyvoice.sft_train 的任务参数。llmLayers 取自 SFTLLMScopes（0 为全量训练）。
func SFTTrainParams(l paths.Layout, name, part string, epochs, llmLayers int) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if !SFTPrepared(l, name) {
		return nil, errors.New("请先完成数据准备")
	}
	if part != "llm" && part != "both" {
		return nil, fmt.Errorf("未知的训练部分 %q", part)
	}
	if epochs < 1 || epochs > 1000 {
		return nil, errors.New("轮数须在 1–1000 之间")
	}
	if llmLayers < 0 {
		return nil, errors.New("LLM 训练层数不能为负")
	}
	return map[string]any{"name": name, "base_model_dir": SFTBase().Dir(l), "part": part, "epochs": epochs, "llm_layers": llmLayers}, nil
}
