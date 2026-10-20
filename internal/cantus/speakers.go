package cantus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kanade/internal/paths"
)

// Speaker 是保存下来的参考音色：一段参考音频及其文本，可在零样本、跨语种、指令与声音转换中反复使用。
//
// 每个音色是 speakers/<ID>/ 目录，内含 speaker.json 与参考音频副本；引擎首次使用时提取特征并缓存在内存中。
// 音色与模型无关：同一音色可用于任何支持参考音频的模型，各模型分别提取自己的特征。
type Speaker struct {
	ID         string    `json:"-"`
	Name       string    `json:"name"`
	PromptText string    `json:"prompt_text"`
	Audio      string    `json:"audio"` // 目录内的参考音频文件名
	Created    time.Time `json:"created"`
}

// SpeakersDir 返回音色库目录。
func SpeakersDir(l paths.Layout) string { return filepath.Join(Root(l), "speakers") }

// Dir 返回音色目录。
func (s Speaker) Dir(l paths.Layout) string { return filepath.Join(SpeakersDir(l), s.ID) }

// ListSpeakers 返回所有音色，按创建时间倒序。目录不存在时返回空列表；单个音色损坏时跳过。
func ListSpeakers(l paths.Layout) ([]Speaker, error) {
	entries, err := os.ReadDir(SpeakersDir(l))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Speaker
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(SpeakersDir(l), e.Name(), "speaker.json"))
		if err != nil {
			continue
		}
		var s Speaker
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		s.ID = e.Name()
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// SaveSpeaker 把 audioPath 复制进音色库并保存为名为 name 的音色。
//
// name 与 audioPath 不能为空；promptText 可为空，但零样本克隆需要它（跨语种与声音转换不需要）。
// 音频的格式与时长由引擎在使用时校验。
func SaveSpeaker(l paths.Layout, name, promptText, audioPath string) (Speaker, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Speaker{}, errors.New("请填写音色名称")
	}
	if audioPath == "" {
		return Speaker{}, errors.New("请先选择参考音频")
	}
	s := Speaker{
		ID:         time.Now().Format("20060102-150405.000"),
		Name:       name,
		PromptText: strings.TrimSpace(promptText),
		Audio:      "prompt" + strings.ToLower(filepath.Ext(audioPath)),
		Created:    time.Now(),
	}
	dir := s.Dir(l)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Speaker{}, err
	}
	if err := copyFile(audioPath, filepath.Join(dir, s.Audio)); err != nil {
		os.RemoveAll(dir)
		return Speaker{}, fmt.Errorf("复制参考音频: %w", err)
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "speaker.json"), data, 0o644); err != nil {
		os.RemoveAll(dir)
		return Speaker{}, err
	}
	return s, nil
}

// DeleteSpeaker 删除音色目录。
func DeleteSpeaker(l paths.Layout, id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("无效的音色 ID %q", id)
	}
	return os.RemoveAll(filepath.Join(SpeakersDir(l), id))
}

func copyFile(src, dst string) error {
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
		return err
	}
	return out.Close()
}
