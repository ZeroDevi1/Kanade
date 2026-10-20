// Package timbre 管理 RVC 音色模型并组装音色转换引擎（engines/timbre）的任务参数。
//
// 模型来源有两种：在 Kanade 中训练的（models/rvc/logs/<名称>/，由引擎写入）与导入的
// （models/rvc/imported/<名称>/，例如 VoiceLab 或其他 RVC WebUI 训练出的 .pth + .index）。
package timbre

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kanade/internal/paths"
)

// Engine 是 Conductor 中音色转换引擎的名称。
const Engine = "timbre"

// Root 是 RVC 数据目录，与引擎 runtime.ROOT 一致。
func Root(l paths.Layout) string { return filepath.Join(l.Models, "rvc") }

func logsDir(l paths.Layout) string     { return filepath.Join(Root(l), "logs") }
func importedDir(l paths.Layout) string { return filepath.Join(Root(l), "imported") }

// Model 是可用于推理的音色模型。
type Model struct {
	Name    string
	Weights string // 推理权重 .pth
	Index   string // 特征索引 .index，可为空
	// Trained 为 true 表示在 Kanade 中训练（Dir 为训练目录，可续训），否则为导入。
	Trained  bool
	Dir      string
	Epoch    int // 训练模型的轮数；导入模型为 0
	Modified time.Time
}

// weightsRE 匹配 Applio 导出的推理权重 <名称>_<轮>e_<步>s.pth；G_ / D_ 开头的是训练检查点。
var weightsRE = regexp.MustCompile(`^(.+)_(\d+)e_(\d+)s\.pth$`)

// ListModels 返回全部模型：训练模型取每个训练目录中最新导出的权重，按修改时间倒序。
func ListModels(l paths.Layout) []Model {
	var out []Model
	if entries, err := os.ReadDir(logsDir(l)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if m, ok := trainedModel(filepath.Join(logsDir(l), e.Name())); ok {
					out = append(out, m)
				}
			}
		}
	}
	if entries, err := os.ReadDir(importedDir(l)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if m, ok := importedModel(filepath.Join(importedDir(l), e.Name())); ok {
					out = append(out, m)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

func trainedModel(dir string) (Model, bool) {
	name := filepath.Base(dir)
	files, _ := filepath.Glob(filepath.Join(dir, name+"_*e_*s.pth"))
	m := Model{Name: name, Trained: true, Dir: dir}
	bestStep := -1
	for _, f := range files {
		sm := weightsRE.FindStringSubmatch(filepath.Base(f))
		if sm == nil || sm[1] != name {
			continue
		}
		ep, _ := strconv.Atoi(sm[2])
		st, _ := strconv.Atoi(sm[3])
		if ep > m.Epoch || (ep == m.Epoch && st > bestStep) {
			m.Epoch, bestStep, m.Weights = ep, st, f
		}
	}
	if m.Weights == "" {
		return m, false
	}
	if idx := filepath.Join(dir, name+".index"); exists(idx) {
		m.Index = idx
	}
	m.Modified = modTime(m.Weights)
	return m, true
}

func importedModel(dir string) (Model, bool) {
	m := Model{Name: filepath.Base(dir), Dir: dir}
	pths, _ := filepath.Glob(filepath.Join(dir, "*.pth"))
	idxs, _ := filepath.Glob(filepath.Join(dir, "*.index"))
	if len(pths) == 0 {
		return m, false
	}
	m.Weights = pths[0]
	if len(idxs) > 0 {
		m.Index = idxs[0]
	}
	m.Modified = modTime(m.Weights)
	return m, true
}

// nameRE 与引擎训练任务的模型名规则一致。
var nameRE = regexp.MustCompile(`^[\p{Han}\w-]+$`)

// ValidName 报告 name 是否可用作模型名（也是目录名）。
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Import 把外部训练的 .pth（及可选 .index）复制为导入模型 name。同名模型已存在时返回错误。
func Import(l paths.Layout, name, weights, index string) (Model, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return Model{}, errors.New("模型名只能包含字母、数字、中文、下划线与连字符")
	}
	if !strings.EqualFold(filepath.Ext(weights), ".pth") {
		return Model{}, errors.New("请选择 .pth 权重文件")
	}
	if index != "" && !strings.EqualFold(filepath.Ext(index), ".index") {
		return Model{}, errors.New("索引文件须为 .index")
	}
	dir := filepath.Join(importedDir(l), name)
	if exists(dir) || exists(filepath.Join(logsDir(l), name)) {
		return Model{}, fmt.Errorf("已存在名为「%s」的模型", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Model{}, err
	}
	copies := [][2]string{{weights, filepath.Join(dir, name+".pth")}}
	if index != "" {
		copies = append(copies, [2]string{index, filepath.Join(dir, name+".index")})
	}
	for _, c := range copies {
		if err := copyFile(c[0], c[1]); err != nil {
			os.RemoveAll(dir)
			return Model{}, err
		}
	}
	m, _ := importedModel(dir)
	return m, nil
}

// DeleteImported 删除导入模型；训练模型（含检查点）不在界面中删除，避免误删长时间训练的成果。
func DeleteImported(l paths.Layout, m Model) error {
	if m.Trained || filepath.Dir(m.Dir) != importedDir(l) {
		return errors.New("只能删除导入的模型")
	}
	return os.RemoveAll(m.Dir)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func modTime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
