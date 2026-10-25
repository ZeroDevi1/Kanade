package cantus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 长文本合成的分段结果（引擎 longtext.py 写出的 segments.json）：每段的文本、音频与时长，界面据此列出各段、
// 单独试听，并重新生成其中一段（Request.Regenerate）。

// Segments 是 segments.json 的内容。
type Segments struct {
	SampleRate int  `json:"sample_rate"`
	Partial    bool `json:"partial"` // 取消时只拼接了已合成的段
	// Audio 是最新一次拼接出的音频文件名（与 segments.json 同目录）；重新生成一段后换成新文件。
	Audio    string    `json:"audio"`
	Revision int       `json:"revision"`
	Items    []Segment `json:"segments"`
	dir      string
}

// Segment 是一段的合成结果。
type Segment struct {
	Text       string  `json:"text"`
	Pause      string  `json:"pause"` // 之后的停顿：comma、sentence、line、paragraph、end
	File       string  `json:"file"`
	Duration   float64 `json:"duration"` // 秒，已去首尾静音
	Retries    int     `json:"retries"`
	Suspicious bool    `json:"suspicious"` // 重试后时长仍超出估计范围，可能读错、拖长或漏读
}

// ReadSegments 读取 segments.json。
func ReadSegments(path string) (*Segments, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Segments
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s：%w", filepath.Base(path), err)
	}
	s.dir = filepath.Dir(path)
	return &s, nil
}

// AudioPath 返回最新拼接音频的路径。
func (s *Segments) AudioPath() string { return filepath.Join(s.dir, s.Audio) }

// FilePath 返回第 i 段音频的路径。
func (s *Segments) FilePath(i int) string { return filepath.Join(s.dir, s.Items[i].File) }

var (
	cjk  = regexp.MustCompile(`\p{Han}`)
	word = regexp.MustCompile(`[A-Za-z0-9']+`)
	// sentEnd 匹配一处句末：连续的句末标点、英文句点（其后是空白）与换行算作一处。
	sentEnd   = regexp.MustCompile(`(?:[。！？!?；;…]|\.(?:\s|$)|\n)+`)
	markupTag = regexp.MustCompile(`<\|[^|]*\|>|\[[a-z_]+\]|</?[a-z]+>`)
)

// 朗读时长的粗略估计（语速 1）：中文每字约 0.22 秒、英文每词约 0.36 秒，与引擎 longtext.expected_seconds 一致。
const (
	secPerHan  = 0.22
	secPerWord = 0.36
)

// TextEstimate 是文本的粗略统计：字数（不含空白与标记）、大致句数与语音时长（秒）。句数按句末标点与换行估计，
// 与引擎实际的分段（含长句按逗号切开）可能略有出入。
func TextEstimate(text string, speed float64) (chars, sentences int, seconds float64) {
	plain := markupTag.ReplaceAllString(text, "")
	chars = len([]rune(strings.Join(strings.Fields(plain), "")))
	if speed <= 0 {
		speed = 1
	}
	seconds = (float64(len(cjk.FindAllString(plain, -1)))*secPerHan + float64(len(word.FindAllString(plain, -1)))*secPerWord) / speed
	if strings.TrimSpace(plain) == "" {
		return chars, 0, seconds
	}
	sentences = len(sentEnd.FindAllString(strings.TrimSpace(plain)+"\n", -1))
	return chars, max(sentences, 1), seconds
}
