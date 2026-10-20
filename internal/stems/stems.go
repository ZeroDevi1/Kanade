// Package stems 描述分轨引擎的人声净化链并组装任务参数。
//
// 步骤定义须与 engines/stems/src/kanade_stems/chain.py 的 STEPS 保持一致。
package stems

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"kanade/internal/paths"
)

// Engine 是 Conductor 中分轨引擎的名称。
const Engine = "stems"

// Step 是净化链的一步。
type Step struct {
	ID    string
	Title string
	// Model 是 audio-separator 的模型文件名，下载到 models/stems/。
	Model string
	// Needs 是必须先执行的步骤（输入来自它的输出）。
	Needs string
	// Outputs 是输出文件的后缀键（文件名为 <曲名>_<键>.wav）与说明。
	Outputs []Output
	// SizeMB 是模型大小（2026-10 查询发布页所得），用于下载提示。
	SizeMB int
}

// Output 是一个输出文件。
type Output struct {
	Key   string
	Title string
}

// Steps 是 VoiceLab MSST 链的五步：伴奏、干声、去和声、去混响、降噪。
var Steps = []Step{
	{"other", "伴奏", "melband_roformer_inst_v1e.ckpt", "", []Output{{"other", "伴奏"}}, 913},
	{"vocals", "干声", "melband_roformer_big_beta5e.ckpt", "", []Output{{"vocals", "干声"}}, 1480},
	{"karaoke", "去和声", "mel_band_roformer_karaoke_aufr33_viperx_sdr_10.1956.ckpt", "vocals",
		[]Output{{"vocals_karaoke", "主唱（去和声）"}, {"vocals_other", "和声"}}, 913},
	{"noreverb", "去混响", "dereverb_mel_band_roformer_anvuew_sdr_19.1729.ckpt", "karaoke",
		[]Output{{"vocals_karaoke_noreverb", "主唱（去混响）"}}, 913},
	{"dry", "降噪", "denoise_mel_band_roformer_aufr33_sdr_27.9959.ckpt", "noreverb",
		[]Output{{"vocals_karaoke_noreverb_dry", "主唱干声（降噪）"}}, 913},
}

// DryKey 是净化链最终输出（用于 RVC 翻唱的干净主唱）的后缀。
const DryKey = "vocals_karaoke_noreverb_dry"

// ModelDir 是分轨模型目录，与引擎的 MODEL_DIR 一致。
func ModelDir(l paths.Layout) string { return filepath.Join(l.Models, "stems") }

// Installed 报告步骤的模型是否已下载（引擎下载完成后才把 .part 改名为正式文件名）。
func (s Step) Installed(l paths.Layout) bool {
	st, err := os.Stat(filepath.Join(ModelDir(l), s.Model))
	return err == nil && st.Size() > 0
}

// Resolve 返回执行 ids 所需的全部步骤 ID（补齐前置步骤），按链顺序。
func Resolve(ids []string) []string {
	need := map[string]bool{}
	for _, id := range ids {
		need[id] = true
	}
	for i := len(Steps) - 1; i >= 0; i-- {
		if need[Steps[i].ID] && Steps[i].Needs != "" {
			need[Steps[i].Needs] = true
		}
	}
	var out []string
	for _, s := range Steps {
		if need[s.ID] {
			out = append(out, s.ID)
		}
	}
	return out
}

// ChainParams 返回 stems.chain 的任务参数。
func ChainParams(input string, steps []string) (map[string]any, error) {
	if input == "" {
		return nil, errors.New("请选择要处理的音频")
	}
	if len(steps) == 0 {
		return nil, errors.New("请至少选择一个步骤")
	}
	return map[string]any{"input": input, "steps": Resolve(steps)}, nil
}

// OutputTitle 根据输出文件名的后缀返回说明；无法识别时返回空串。
func OutputTitle(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	best, title := 0, ""
	for _, s := range Steps {
		for _, o := range s.Outputs {
			// 后缀互为前缀（vocals 与 vocals_karaoke），取最长匹配。
			if strings.HasSuffix(name, "_"+o.Key) && len(o.Key) > best {
				best, title = len(o.Key), o.Title
			}
		}
	}
	return title
}
