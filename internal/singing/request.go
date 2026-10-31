// Package singing describes optional melody-conditioned lyric replacement.
package singing

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const Engine = "yingmusic"
const Task = "yingmusic.edit"

// Request keeps Ying's separately provisioned environment out of RVC's environment.
// RefText must transcribe RefAudio (or MelodyAudio when RefAudio is empty).
type Request struct {
	MelodyAudio, RefAudio, AccompanimentAudio string
	RefText, TargetText                       string
	PythonPath, SourceDir, ModelDir           string
}

func existing(path, label string, directory bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("请填写%s", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() != directory {
		return "", fmt.Errorf("%s不存在或类型错误：%s", label, path)
	}
	return abs, nil
}

func lyrics(s, label string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"))
	s = strings.ReplaceAll(s, "\n", "|")
	if s == "" {
		return "", fmt.Errorf("请填写%s（用 | 分句）", label)
	}
	parts := strings.Split(s, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return "", fmt.Errorf("%s不能包含空句", label)
		}
	}
	return strings.Join(parts, "|"), nil
}

// Params validates only local inputs; it never installs dependencies or downloads weights.
func (r Request) Params() (map[string]any, error) {
	p := map[string]any{}
	for _, field := range []struct {
		key, value, label string
		dir               bool
	}{
		{"python_path", r.PythonPath, "Ying 独立 Python 路径", false},
		{"source_dir", r.SourceDir, "Ying 源码目录", true},
		{"model_dir", r.ModelDir, "本地模型快照目录", true},
		{"melody_audio", r.MelodyAudio, "旋律干声", false},
	} {
		value, err := existing(field.value, field.label, field.dir)
		if err != nil {
			return nil, err
		}
		p[field.key] = value
	}
	ref := r.RefAudio
	if strings.TrimSpace(ref) == "" {
		ref = r.MelodyAudio
	}
	value, err := existing(ref, "音色参考干声", false)
	if err != nil {
		return nil, err
	}
	p["ref_audio"] = value
	if strings.TrimSpace(r.AccompanimentAudio) != "" {
		value, err = existing(r.AccompanimentAudio, "伴奏", false)
		if err != nil {
			return nil, err
		}
		p["accompaniment_audio"] = value
	}
	for _, field := range []struct{ key, value, label string }{
		{"ref_text", r.RefText, "参考干声原词"}, {"target_text", r.TargetText, "目标新词"},
	} {
		value, err := lyrics(field.value, field.label)
		if err != nil {
			return nil, err
		}
		p[field.key] = value
	}
	return p, nil
}
