// Package dataset 读取训练数据集目录与 `.list` 标注（GPT-SoVITS 格式 audio_path|speaker|lang|text）。
//
// 规则与 engines/sdk/src/kanade_engine/annotations.py 及 VoiceLab docs/datasets/list_annotations.md 一致：
// 同名优先查找 .list；audio_path 按 basename、再按 stem 匹配目录中的音频。界面用它在提交训练前给出统计与提示，
// 实际训练时由引擎按同一规则再解析一次。
package dataset

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AudioExts 是视为音频的扩展名（小写）。
var AudioExts = map[string]bool{".wav": true, ".flac": true, ".mp3": true, ".ogg": true, ".m4a": true, ".aac": true, ".opus": true}

// Entry 是 .list 的一行。
type Entry struct {
	Audio, Speaker, Lang, Text string
}

// Basename 返回 Audio 的文件名部分，兼容 Windows 与 Unix 路径分隔符。
func (e Entry) Basename() string {
	i := strings.LastIndexAny(e.Audio, `/\`)
	return e.Audio[i+1:]
}

// Parse 读取 .list 文件：忽略空行与 # 注释；超过 4 列时多余的 | 并入文本；兼容 UTF-8 BOM。
func Parse(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.SplitN(line, "|", 4)
		for len(p) < 4 {
			p = append(p, "")
		}
		out = append(out, Entry{strings.TrimSpace(p[0]), strings.TrimSpace(p[1]), strings.TrimSpace(p[2]), strings.TrimSpace(p[3])})
	}
	return out, sc.Err()
}

// FindList 按同名优先规则返回数据集目录中的 .list 路径；找不到时返回空串。
func FindList(dir string) string {
	base := filepath.Base(dir)
	for _, name := range []string{base + ".list", strings.ToLower(base) + ".list"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	lists, _ := filepath.Glob(filepath.Join(dir, "*.list"))
	if len(lists) == 1 {
		return lists[0]
	}
	return ""
}

// AudioFiles 返回目录中的音频文件（不递归），按名称排序。
func AudioFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && AudioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Summary 是数据集目录的统计。
type Summary struct {
	Dir   string
	Audio int // 目录中的音频数
	// List 为使用的 .list（空表示没有，训练将使用目录中全部音频）。
	List     string
	Listed   int // .list 中的条目数
	Matched  int // 找到音频的条目数
	WithText int // 有文本的条目数（SFT / TTS 训练需要）
	// Missing 是找不到音频的条目（最多保留 10 条，用于提示）。
	Missing []string
}

// Inspect 统计数据集目录；list 为空时按同名规则自动查找。
func Inspect(dir, list string) (Summary, error) {
	s := Summary{Dir: dir}
	files, err := AudioFiles(dir)
	if err != nil {
		return s, err
	}
	s.Audio = len(files)
	if list == "" {
		list = FindList(dir)
	}
	if list == "" {
		return s, nil
	}
	s.List = list
	entries, err := Parse(list)
	if err != nil {
		return s, err
	}
	byName, byStem := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		name := filepath.Base(f)
		byName[name] = true
		byStem[strings.TrimSuffix(name, filepath.Ext(name))] = true
	}
	s.Listed = len(entries)
	for _, e := range entries {
		name := e.Basename()
		if byName[name] || byStem[strings.TrimSuffix(name, filepath.Ext(name))] {
			s.Matched++
		} else if len(s.Missing) < 10 {
			s.Missing = append(s.Missing, name)
		}
		if e.Text != "" {
			s.WithText++
		}
	}
	return s, nil
}
