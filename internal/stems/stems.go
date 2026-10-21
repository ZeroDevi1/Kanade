// Package stems 描述分轨引擎的人声净化链并组装任务参数。
//
// 步骤定义须与 engines/stems/src/kanade_stems/chain.py 的 STEPS 保持一致。
package stems

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	// Alternatives 是该步可替换的其他模型（Model 为默认），见 docs/stems-evaluation.md。
	Alternatives []ModelOption
}

// ModelOption 是一步可选的模型。
type ModelOption struct {
	File   string
	Title  string
	SizeMB int
}

// Output 是一个输出文件。
type Output struct {
	Key   string
	Title string
}

// Steps 是 VoiceLab MSST 链的五步：伴奏、干声、去和声、去混响、降噪。默认模型与 VoiceLab 一致。
var Steps = []Step{
	{"other", "伴奏", "melband_roformer_inst_v1e.ckpt", "", []Output{{"other", "伴奏"}}, 913,
		[]ModelOption{{recInst, "BS-Roformer-Viperx-1297（评估最优）", 640}}},
	{"vocals", "干声", "melband_roformer_big_beta5e.ckpt", "", []Output{{"vocals", "干声"}}, 1480,
		[]ModelOption{{recVocals, "BS-Roformer-SW（评估最优）", 650}}},
	{"karaoke", "去和声", "mel_band_roformer_karaoke_aufr33_viperx_sdr_10.1956.ckpt", "vocals",
		[]Output{{"vocals_karaoke", "主唱（去和声）"}, {"vocals_other", "和声"}}, 913,
		[]ModelOption{{recKaraoke, "Karaoke V2 · Gabox（评估最优）", 900}}},
	{"noreverb", "去混响", "dereverb_mel_band_roformer_anvuew_sdr_19.1729.ckpt", "karaoke",
		[]Output{{"vocals_karaoke_noreverb", "主唱（去混响）"}}, 913, nil},
	{"dry", "降噪", "denoise_mel_band_roformer_aufr33_sdr_27.9959.ckpt", "noreverb",
		[]Output{{"vocals_karaoke_noreverb_dry", "主唱干声（降噪）"}}, 913, nil},
}

const (
	recInst    = "model_bs_roformer_ep_317_sdr_12.9755.ckpt"
	recVocals  = "BS-Roformer-SW.ckpt"
	recKaraoke = "mel_band_roformer_karaoke_gabox_v2.ckpt"
)

// Recommended 是 docs/stems-evaluation.md 的评估最优组合（去混响与降噪保持默认），按步骤 ID 覆盖默认模型。
// 同一首歌上，伴奏里残留的原唱（伴奏与人声的相关系数）从 0.046 降到 0.012。
var Recommended = map[string]string{"other": recInst, "vocals": recVocals, "karaoke": recKaraoke}

// Options 返回该步可选的模型，默认模型在前。
func (s Step) Options() []ModelOption {
	return append([]ModelOption{{s.Model, "默认（与 VoiceLab 一致）", s.SizeMB}}, s.Alternatives...)
}

// Pick 返回 models 为该步选择的模型；未选择或不在可选列表中时为默认模型。
func (s Step) Pick(models map[string]string) ModelOption {
	opts := s.Options()
	for _, o := range opts {
		if o.File == models[s.ID] {
			return o
		}
	}
	return opts[0]
}

// ModelInstalled 报告模型文件是否已下载（引擎下载完成后才把 .part 改名为正式文件名）。
func ModelInstalled(l paths.Layout, file string) bool {
	st, err := os.Stat(filepath.Join(ModelDir(l), file))
	return err == nil && st.Size() > 0
}

// Missing 返回执行 steps（会补齐前置步骤）在 models 选择下尚未下载的模型及其总大小（MB）。
func Missing(l paths.Layout, steps []string, models map[string]string) ([]string, int) {
	var files []string
	total := 0
	for _, s := range Steps {
		if !slices.Contains(Resolve(steps), s.ID) {
			continue
		}
		if o := s.Pick(models); !ModelInstalled(l, o.File) {
			files, total = append(files, o.File), total+o.SizeMB
		}
	}
	return files, total
}

// AllSteps 是全部步骤的 ID。
func AllSteps() []string {
	ids := make([]string, len(Steps))
	for i, s := range Steps {
		ids[i] = s.ID
	}
	return ids
}

// DryKey 是净化链最终输出（用于 RVC 翻唱的干净主唱）的后缀。
const DryKey = "vocals_karaoke_noreverb_dry"

// ModelDir 是分轨模型目录，与引擎的 MODEL_DIR 一致。
func ModelDir(l paths.Layout) string { return filepath.Join(l.Models, "stems") }

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

// ChainParams 返回 stems.chain 的任务参数。models 按步骤 ID 覆盖默认模型（可为 nil），只传与默认不同的项。
func ChainParams(input string, steps []string, models map[string]string) (map[string]any, error) {
	if input == "" {
		return nil, errors.New("请选择要处理的音频")
	}
	if len(steps) == 0 {
		return nil, errors.New("请至少选择一个步骤")
	}
	ids := Resolve(steps)
	p := map[string]any{"input": input, "steps": ids}
	over := map[string]any{}
	for _, s := range Steps {
		if o := s.Pick(models); slices.Contains(ids, s.ID) && o.File != s.Model {
			over[s.ID] = o.File
		}
	}
	if len(over) > 0 {
		p["models"] = over
	}
	return p, nil
}

// DownloadParams 返回 stems.download 预下载 files 的任务参数。
func DownloadParams(files []string) map[string]any {
	list := make([]any, len(files))
	for i, f := range files {
		list[i] = f
	}
	return map[string]any{"models": list}
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
