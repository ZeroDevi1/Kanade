package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"kanade/internal/conductor"
)

// 主唱的表情特征（引擎 score.expression，字段见 engines/score/src/kanade_score/expression.py）：
// 导出 Synthesizer V 工程时换算成参数曲线（svexpr.go）。特征按记录缓存为记录目录下的 expression.json，
// 第一次导出时从记录复用的主唱分轨提取（约 10 秒，只用 CPU），之后直接读取。

// expressionFile 是记录目录中缓存表情特征的文件名。
const expressionFile = "expression.json"

// Expression 是逐帧的主唱特征，各数组等长，帧 i 的中心在 i × Hop 秒。
type Expression struct {
	Version     int       `json:"version"`
	Hop         float64   `json:"hop"`         // 帧移（秒）
	F0          []float64 `json:"f0"`          // 基频（Hz），yin 每帧都有值，不代表有声
	Periodicity []float64 `json:"periodicity"` // 0–1，越高越接近纯周期的浊音
	Loudness    []float64 `json:"loudness"`    // 帧 RMS（dBFS）
	Tilt        []float64 `json:"tilt"`        // 1.5–5 kHz 与 80 Hz–1.5 kHz 的能量比（dB）
	Noise       []float64 `json:"noise"`       // 5–8 kHz 与 80 Hz–5 kHz 的能量比（dB）
}

// ReadExpression 读取 expression.json；数组不等长或帧移无效时返回错误。
func ReadExpression(path string) (*Expression, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Expression
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	n := len(e.F0)
	if e.Hop <= 0 || len(e.Periodicity) != n || len(e.Loudness) != n || len(e.Tilt) != n || len(e.Noise) != n {
		return nil, fmt.Errorf("%s 的特征数组不完整", filepath.Base(path))
	}
	return &e, nil
}

// MelodyStem 返回该记录用来识别旋律的主唱分轨（_vocals_karaoke.wav）的路径：本记录或同一首歌的其他记录中已分离的分轨。
// 记录未保存、分析时没有分离主唱，或原曲已移动 / 修改（无法确认是同一首歌）时返回空串。
func (r *Run) MelodyStem() string {
	r.mu.Lock()
	dir := r.dir
	r.mu.Unlock()
	if dir == "" || !r.Request.Melody {
		return ""
	}
	return findStems(filepath.Dir(dir), r.Request).melody
}

// Expression 返回该记录的主唱表情特征：已缓存时直接读取，否则经 jobs 提交 score.expression 提取，
// 结果移入记录目录缓存。阻塞直到完成或 ctx 取消，应在后台 goroutine 调用。
func (r *Run) Expression(ctx context.Context, jobs Jobs) (*Expression, error) {
	r.mu.Lock()
	dir := r.dir
	r.mu.Unlock()
	if dir == "" {
		return nil, errors.New("分析结果没有保存为记录，无法提取主唱表情")
	}
	cached := filepath.Join(dir, expressionFile)
	if fileExists(cached) {
		if e, err := ReadExpression(cached); err == nil {
			return e, nil
		}
		// 缓存损坏（例如写到一半退出）：重新提取覆盖。
	}
	stem := r.MelodyStem()
	if stem == "" {
		return nil, errors.New("找不到分离出的主唱（原曲已移动或修改，或分析时没有识别旋律线）")
	}
	id, err := jobs.Submit(Engine, "score.expression", map[string]any{"input": stem})
	if err != nil {
		return nil, err
	}
	j, err := jobs.Wait(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case j.State == conductor.JobCanceled:
		return nil, errors.New("已取消")
	case j.State != conductor.JobDone || len(j.Artifacts) == 0:
		return nil, fmt.Errorf("提取主唱表情失败：%s", j.Err)
	}
	if err := moveFile(j.Artifacts[0], cached); err != nil {
		return nil, fmt.Errorf("保存主唱表情：%w", err)
	}
	return ReadExpression(cached)
}
