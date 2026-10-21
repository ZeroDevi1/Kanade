package score

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"kanade/internal/paths"
)

// 分析记录：每次分析成功后把产物（analysis.json、chords.lab、预览用的 audio.wav）与分离出的分轨移到
// HistoryDir 下的独立目录，并写 record.json。重启后 LoadHistory 读回为已完成的 Run，可直接查看与导出；
// 同一首歌（路径、大小与修改时间都相同）再次分析且分轨模型相同时，复用记录里已分离的分轨，跳过 GPU 分离。

// recordFile 是记录目录中描述该记录的文件名。
const recordFile = "record.json"

// HistoryDir 是分析记录的根目录。
func HistoryDir(l paths.Layout) string { return filepath.Join(l.Outputs, "score-history") }

// record 是 record.json 的内容。文件字段只存文件名（相对记录目录），整个数据根目录迁移后仍然有效。
type record struct {
	Version int       `json:"version"`
	Created time.Time `json:"created"`
	// Input / InputSize / InputMod 标识输入文件，用于判断能否复用分轨。
	Input      string            `json:"input"`
	InputSize  int64             `json:"input_size"`
	InputMod   time.Time         `json:"input_mod"`
	Separate   bool              `json:"separate"`
	Melody     bool              `json:"melody"`
	Lyrics     string            `json:"lyrics,omitempty"` // LRC 歌词的路径（原位置，不复制）
	StemModels map[string]string `json:"stem_models,omitempty"`
	Options    Options           `json:"options"`
	// Files 是分析产物：analysis、chords、audio。
	Files map[string]string `json:"files"`
	// Stems 是本次分离出的分轨：harmonic（伴奏）、melody（主唱）。复用别的记录的分轨时不重复保存，此处为空。
	Stems map[string]string `json:"stems,omitempty"`
}

// stemSource 是可复用的分轨：伴奏与主唱的绝对路径（没有则为空）。
type stemSource struct{ harmonic, melody string }

// findStems 在 root 下查找与 r 同一输入、同一分轨模型的记录中已分离的分轨。找不到或输入文件不可读时返回零值。
func findStems(root string, r Request) stemSource {
	st, err := os.Stat(r.Input)
	if err != nil || root == "" {
		return stemSource{}
	}
	var found stemSource
	for dir, rec := range readRecords(root) {
		if rec.Input != r.Input || rec.InputSize != st.Size() || !rec.InputMod.Equal(st.ModTime()) ||
			!maps.Equal(rec.StemModels, r.StemModels) {
			continue
		}
		if h := rec.Stems["harmonic"]; h != "" && found.harmonic == "" && fileExists(filepath.Join(dir, h)) {
			found.harmonic = filepath.Join(dir, h)
		}
		if m := rec.Stems["melody"]; m != "" && found.melody == "" && fileExists(filepath.Join(dir, m)) {
			found.melody = filepath.Join(dir, m)
		}
	}
	return found
}

// readRecords 读取 root 下各记录目录的 record.json；无法解析的目录跳过。
func readRecords(root string) map[string]record {
	out := map[string]record{}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, recordFile))
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(data, &rec) == nil && rec.Files["analysis"] != "" {
			out[dir] = rec
		}
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// save 把本次的产物与新分离的分轨移到 root 下的新记录目录，返回目录与移动后的路径（键与 files / stems 相同）。
// 失败时尽量删除半成品目录，调用方继续使用原路径。
func save(root string, r Request, created time.Time, files, stems map[string]string) (string, map[string]string, map[string]string, error) {
	name := strings.TrimSuffix(filepath.Base(r.Input), filepath.Ext(r.Input))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", nil, nil, err
	}
	// 同一秒内分析同一首歌（复用分轨时分析只需几十秒，连续点击也可能）会撞名：已存在时追加序号。
	base := filepath.Join(root, created.Format("20060102-150405")+"-"+safeName(name))
	dir := base
	for i := 2; ; i++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			break
		}
		if !os.IsExist(err) || i > 99 {
			return "", nil, nil, err
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}
	rec := record{Version: 1, Created: created, Input: r.Input, Separate: r.Separate, Melody: r.Melody,
		Lyrics: r.Lyrics, StemModels: r.StemModels, Options: r.Options, Files: map[string]string{}, Stems: map[string]string{}}
	if st, err := os.Stat(r.Input); err == nil {
		rec.InputSize, rec.InputMod = st.Size(), st.ModTime()
	}
	moved := func(src map[string]string, dst map[string]string) (map[string]string, error) {
		out := map[string]string{}
		for k, p := range src {
			if p == "" {
				continue
			}
			base := filepath.Base(p)
			if err := moveFile(p, filepath.Join(dir, base)); err != nil {
				return nil, err
			}
			dst[k], out[k] = base, filepath.Join(dir, base)
		}
		return out, nil
	}
	newFiles, err := moved(files, rec.Files)
	var newStems map[string]string
	if err == nil {
		newStems, err = moved(stems, rec.Stems)
	}
	if err == nil {
		var data []byte
		data, err = json.MarshalIndent(rec, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, recordFile), data, 0o644)
		}
	}
	if err != nil {
		// 已移动的文件随目录一起删掉会丢失本次结果：先移回原位置再删目录。
		for k, p := range newFiles {
			_ = moveFile(p, files[k])
		}
		for k, p := range newStems {
			_ = moveFile(p, stems[k])
		}
		_ = os.RemoveAll(dir)
		return "", nil, nil, err
	}
	return dir, newFiles, newStems, nil
}

// safeName 去掉文件名中不能用于目录名的字符，并限制长度（Windows 路径上限 260）。
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > 40 {
		s = string(rs[:40])
	}
	return strings.TrimRight(s, ". ")
}

// moveFile 移动文件；跨卷（暂存区在系统临时目录、数据根目录在别的盘）时改为复制后删除。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}

// LoadHistory 读取 root 下的分析记录，作为已完成的 Run 返回，最新在前。结果文件缺失或损坏的记录跳过。
func LoadHistory(root string) []*Run {
	var runs []*Run
	for dir, rec := range readRecords(root) {
		res, err := ReadAnalysis(filepath.Join(dir, rec.Files["analysis"]))
		if err != nil {
			continue
		}
		files := map[string]string{}
		for k, f := range rec.Files {
			if k != "audio" {
				files[k] = filepath.Join(dir, f)
			}
		}
		audio := ""
		if f := rec.Files["audio"]; f != "" {
			audio = filepath.Join(dir, f)
		}
		req := Request{Input: rec.Input, Separate: rec.Separate, Melody: rec.Melody, Lyrics: rec.Lyrics,
			StemModels: rec.StemModels, Options: rec.Options}
		runs = append(runs, &Run{Request: req, stage: StageDone, result: res, audio: audio, files: files,
			skipSep: !rec.Separate && !rec.Melody, dir: dir, created: rec.Created, cancel: func() {}, onChange: func() {}})
	}
	slices.SortFunc(runs, func(a, b *Run) int { return b.created.Compare(a.created) })
	return runs
}

// Delete 删除已保存的记录（连同其中的分轨）。未保存或仍在运行的分析返回错误。
func (r *Run) Delete() error {
	r.mu.Lock()
	dir, stage := r.dir, r.stage
	r.mu.Unlock()
	if dir == "" || stage != StageDone {
		return errors.New("该分析没有保存的记录")
	}
	if !fileExists(filepath.Join(dir, recordFile)) {
		return fmt.Errorf("不是分析记录目录：%s", dir)
	}
	return os.RemoveAll(dir)
}
