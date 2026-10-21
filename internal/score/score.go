// Package score 组装音乐分析引擎（engines/score）的任务参数、运行「分离伴奏 → 分析」流水线并解析结果。
package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/stems"
)

// Engine 是 Conductor 中音乐分析引擎的名称。
const Engine = "score"

// Options 是 score.analyze 的分析参数，零值字段使用引擎默认（见引擎 analyze.py）。
type Options struct {
	BeatMethod string // BeatMethods 之一
	// TempoMin / TempoMax 非零时把 BPM 按倍数折叠到区间内（半速 / 倍速误判时用）。
	TempoMin, TempoMax float64
	KeyProfile         string  // KeyProfiles 之一
	KeySegmentS        float64 // 分段估计调号的窗口（秒），0 为不分段
	ChordVocab         string  // ChordVocabs 之一
	ChordSelfProb      float64 // 和弦平滑（HMM 自转移概率），0.5–0.99
	ChordMinBeats      int     // 最短和弦（拍）
	Emotion            bool
}

// 可选值，与引擎一致。Title 用于界面。
var (
	BeatMethods = []struct{ Key, Title string }{{"beat_this", "beat_this（神经网络，推荐）"}, {"librosa", "librosa（快速，无需下载）"}}
	KeyProfiles = []struct{ Key, Title string }{{"krumhansl", "Krumhansl–Kessler"}, {"temperley", "Temperley"}}
	ChordVocabs = []struct{ Key, Title string }{{"basic", "大小三和弦"}, {"extended", "含七和弦、减三、挂四"}}
)

// DefaultOptions 是智能模式使用的参数（引擎默认值）。
func DefaultOptions() Options {
	return Options{BeatMethod: "beat_this", KeyProfile: "krumhansl", KeySegmentS: 30, ChordVocab: "basic",
		ChordSelfProb: 0.9, ChordMinBeats: 1, Emotion: true}
}

func (o Options) params() (map[string]any, error) {
	if o.ChordSelfProb != 0 && (o.ChordSelfProb < 0.5 || o.ChordSelfProb > 0.99) {
		return nil, errors.New("和弦平滑须在 0.5–0.99 之间")
	}
	if o.TempoMin != 0 || o.TempoMax != 0 {
		if o.TempoMin < 20 || o.TempoMax > 400 || o.TempoMax < o.TempoMin*2 {
			return nil, errors.New("BPM 范围须在 20–400 之间，且上限至少为下限的 2 倍（才能容纳倍速折叠）")
		}
	}
	if o.ChordMinBeats < 0 || o.KeySegmentS < 0 {
		return nil, errors.New("最短和弦与分段长度不能为负")
	}
	p := map[string]any{"emotion": o.Emotion, "key_segment_s": o.KeySegmentS}
	for k, v := range map[string]string{"beat_method": o.BeatMethod, "key_profile": o.KeyProfile, "chord_vocab": o.ChordVocab} {
		if v != "" {
			p[k] = v
		}
	}
	if o.ChordSelfProb != 0 {
		p["chord_self_prob"] = o.ChordSelfProb
	}
	if o.ChordMinBeats > 0 {
		p["chord_min_beats"] = o.ChordMinBeats
	}
	if o.TempoMin != 0 {
		p["tempo_range"] = []float64{o.TempoMin, o.TempoMax}
	}
	return p, nil
}

// Request 是一次分析的输入。Separate 为 true 时先用分轨引擎分离伴奏，调号与和弦基于伴奏分析
// （人声旋律音会干扰色度，分离后和弦更准，但要多跑一次 GPU 分离）。
type Request struct {
	Input      string
	Separate   bool
	StemModels map[string]string // 分离伴奏用的模型，见 stems.Recommended
	Options    Options
}

// Params 返回 score.analyze 的任务参数；harmonic 为分离出的伴奏（可为空）。
func (r Request) Params(harmonic string) (map[string]any, error) {
	if r.Input == "" {
		return nil, errors.New("请选择要分析的歌曲")
	}
	opts, err := r.Options.params()
	if err != nil {
		return nil, err
	}
	p := map[string]any{"input": r.Input, "options": opts}
	if harmonic != "" {
		p["harmonic_input"] = harmonic
	}
	return p, nil
}

// ModelDir 是分析模型目录，与引擎的 MODEL_DIR 一致。
func ModelDir(l paths.Layout) string { return filepath.Join(l.Models, "score") }

// ModelGroup 是一组需要下载的模型文件。
type ModelGroup struct {
	Key, Title string
	Files      []string // 相对 ModelDir
	SizeMB     int
}

// ModelGroups 与引擎 models.GROUPS 一致。
var ModelGroups = []ModelGroup{
	{"beat_this", "节拍（beat_this）", []string{"beat_this/final0.ckpt"}, 81},
	{"clap", "情绪（CLAP）", []string{"clap/clap-htsat-unfused/pytorch_model.bin", "clap/clap-htsat-unfused/config.json"}, 615},
}

// Installed 报告模型组是否已下载。
func (g ModelGroup) Installed(l paths.Layout) bool {
	for _, f := range g.Files {
		if st, err := os.Stat(filepath.Join(ModelDir(l), f)); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// Jobs 是流水线需要的 Conductor 能力，测试中可替换。
type Jobs interface {
	Submit(engine, task string, params map[string]any) (string, error)
	Wait(ctx context.Context, id string) (conductor.Job, error)
	Cancel(id string)
}

// 流水线阶段。
const (
	StageSeparate = iota
	StageAnalyze
	StageDone
)

// StageTitles 是各阶段的显示名称。
var StageTitles = []string{"分离伴奏", "分析", "完成"}

// Run 是运行中的分析流水线。状态由后台 goroutine 更新，Snapshot 可从任意 goroutine 读取。
type Run struct {
	Request  Request
	jobs     Jobs
	onChange func()
	cancel   context.CancelFunc

	mu      sync.Mutex
	stage   int
	job     string
	err     string
	result  *Analysis
	audio   string // 预览用的解码 WAV
	files   map[string]string
	skipSep bool
}

// RunState 是 Run 的快照。
type RunState struct {
	Stage   int
	Job     string
	Err     string
	Skipped bool // 未分离伴奏
	Result  *Analysis
	Audio   string
	// Files 是导出用的文件：analysis（JSON）与 chords（.lab）。
	Files map[string]string
}

// Running 报告流水线是否仍在进行。
func (s RunState) Running() bool { return s.Err == "" && s.Stage < StageDone }

// Start 校验输入并在后台启动流水线；onChange 在状态变化时调用（可为 nil）。
func Start(jobs Jobs, r Request, onChange func()) (*Run, error) {
	if _, err := r.Params(""); err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{Request: r, jobs: jobs, onChange: onChange, cancel: cancel, files: map[string]string{}, skipSep: !r.Separate}
	go run.run(ctx)
	return run, nil
}

// Snapshot 返回当前状态。
func (r *Run) Snapshot() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	files := make(map[string]string, len(r.files))
	for k, v := range r.files {
		files[k] = v
	}
	return RunState{Stage: r.stage, Job: r.job, Err: r.err, Skipped: r.skipSep, Result: r.result, Audio: r.audio, Files: files}
}

// Cancel 取消当前任务并停止流水线。
func (r *Run) Cancel() {
	r.mu.Lock()
	job := r.job
	r.mu.Unlock()
	r.cancel()
	if job != "" {
		r.jobs.Cancel(job)
	}
}

func (r *Run) set(fn func()) {
	r.mu.Lock()
	fn()
	r.mu.Unlock()
	r.onChange()
}

func (r *Run) step(ctx context.Context, stage int, engine, task string, params map[string]any) ([]string, error) {
	if ctx.Err() != nil {
		return nil, errors.New("已取消")
	}
	id, err := r.jobs.Submit(engine, task, params)
	if err != nil {
		return nil, err
	}
	r.set(func() { r.stage, r.job = stage, id })
	j, err := r.jobs.Wait(ctx, id)
	if err != nil {
		return nil, err
	}
	switch j.State {
	case conductor.JobDone:
		return j.Artifacts, nil
	case conductor.JobCanceled:
		return nil, errors.New("已取消")
	default:
		return nil, fmt.Errorf("%s失败：%s", StageTitles[stage], j.Err)
	}
}

func (r *Run) run(ctx context.Context) {
	fail := func(err error) { r.set(func() { r.err = err.Error() }) }
	harmonic := ""
	if r.Request.Separate {
		params, _ := stems.ChainParams(r.Request.Input, []string{"other"}, r.Request.StemModels)
		arts, err := r.step(ctx, StageSeparate, stems.Engine, "stems.chain", params)
		if err != nil {
			fail(err)
			return
		}
		for _, a := range arts {
			if strings.HasSuffix(a, "_other.wav") {
				harmonic = a
			}
		}
		if harmonic == "" {
			fail(errors.New("分轨结果缺少伴奏"))
			return
		}
	}
	params, err := r.Request.Params(harmonic)
	if err != nil {
		fail(err)
		return
	}
	arts, err := r.step(ctx, StageAnalyze, Engine, "score.analyze", params)
	if err != nil {
		fail(err)
		return
	}
	files := map[string]string{}
	audio := ""
	for _, a := range arts {
		switch filepath.Ext(a) {
		case ".json":
			files["analysis"] = a
		case ".lab":
			files["chords"] = a
		case ".wav":
			audio = a
		}
	}
	res, err := ReadAnalysis(files["analysis"])
	if err != nil {
		fail(fmt.Errorf("读取分析结果：%w", err))
		return
	}
	r.set(func() { r.result, r.audio, r.files, r.stage, r.job = res, audio, files, StageDone, "" })
}

// Analysis 是引擎 analysis.json 的内容。
type Analysis struct {
	Duration float64 `json:"duration"`
	Tempo    struct {
		BPM         *float64     `json:"bpm"`
		Stable      *bool        `json:"stable"`
		SteadyShare float64      `json:"steady_share"`
		Curve       [][2]float64 `json:"curve"` // [小节起点秒, 局部 BPM]
		Folded      bool         `json:"folded"`
	} `json:"tempo"`
	Meter struct {
		Label       *string `json:"label"`
		BeatsPerBar int     `json:"beats_per_bar"`
		Confidence  float64 `json:"confidence"`
	} `json:"meter"`
	Beats     []float64 `json:"beats"`
	Downbeats []float64 `json:"downbeats"`
	Key       struct {
		// Label 是整首的调号；有转调时（Segments 多于一段）它是各段的折中，界面以 Segments 为准。
		Label       string  `json:"label"`
		Relative    string  `json:"relative"`
		Confidence  string  `json:"confidence"` // 高 / 中 / 低
		Correlation float64 `json:"correlation"`
		Candidates  []struct {
			Label       string  `json:"label"`
			Score       float64 `json:"score"` // 轮廓相关系数 + 和弦吻合度
			Correlation float64 `json:"correlation"`
		} `json:"candidates"`
		Segments []Segment `json:"segments"`
	} `json:"key"`
	Chords      []Segment `json:"chords"`
	ChordSource string    `json:"chord_source"` // accompaniment / mix
	Emotion     *struct {
		Moods []struct {
			Key   string  `json:"key"`
			Label string  `json:"label"`
			Score float64 `json:"score"`
		} `json:"moods"`
		Valence  float64   `json:"valence"`
		Arousal  float64   `json:"arousal"`
		Segments []Segment `json:"segments"`
	} `json:"emotion"`
}

// Segment 是一段带标签的时间区间（秒）。
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Label string  `json:"label"`
}

// ReadAnalysis 读取并解析 analysis.json。
func ReadAnalysis(path string) (*Analysis, error) {
	if path == "" {
		return nil, errors.New("没有分析结果文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Analysis
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ChordAt 返回 t 秒处的和弦与其下标；不在任何和弦内时返回 -1。
func (a *Analysis) ChordAt(t float64) int {
	for i, c := range a.Chords {
		if t >= c.Start && t < c.End {
			return i
		}
	}
	return -1
}

// BarAt 返回 t 秒所在的小节号（从 1 开始）与拍号（从 1 开始）；第一个小节线之前返回 0, 0。
func (a *Analysis) BarAt(t float64) (bar, beat int) {
	for i, d := range a.Downbeats {
		if d > t {
			break
		}
		bar = i + 1
	}
	if bar == 0 {
		return 0, 0
	}
	start := a.Downbeats[bar-1]
	for _, b := range a.Beats {
		if b >= start-1e-3 && b <= t {
			beat++
		}
	}
	return bar, beat
}

// Bar 是和弦谱的一小节：起止时间与小节内依次出现的和弦（相邻重复的只记一次）。
type Bar struct {
	Start, End float64
	Chords     []string
}

// Bars 按小节线把和弦序列排成和弦谱。第一个小节线之前的部分（弱起）作为第 0 小节并入第一小节。
func (a *Analysis) Bars() []Bar {
	if len(a.Downbeats) == 0 {
		return nil
	}
	bounds := append(append([]float64{}, a.Downbeats...), a.Duration)
	bounds[0] = 0
	bars := make([]Bar, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		b := Bar{Start: bounds[i], End: bounds[i+1]}
		for _, c := range a.Chords {
			// 和弦与小节有重叠（且不只是边界上的几十毫秒）才计入。
			if c.End > b.Start+0.05 && c.Start < b.End-0.05 {
				if n := len(b.Chords); n == 0 || b.Chords[n-1] != c.Label {
					b.Chords = append(b.Chords, c.Label)
				}
			}
		}
		bars = append(bars, b)
	}
	return bars
}

// ChordChart 返回纯文本和弦谱：每行 4 小节，行首为小节号与时间，便于导出与打印。
func (a *Analysis) ChordChart(title string) string {
	var sb strings.Builder
	key, meter, bpm := a.Key.Label, "", ""
	if a.Meter.Label != nil {
		meter = *a.Meter.Label
	}
	if a.Tempo.BPM != nil {
		bpm = fmt.Sprintf("%.0f BPM", *a.Tempo.BPM)
	}
	fmt.Fprintf(&sb, "%s\n%s · %s · %s\n\n", title, key, meter, bpm)
	bars := a.Bars()
	for i := 0; i < len(bars); i += 4 {
		fmt.Fprintf(&sb, "%3d  %s  |", i+1, Clock(bars[i].Start))
		for _, b := range bars[i:min(i+4, len(bars))] {
			cell := strings.Join(b.Chords, " ")
			if cell == "" {
				cell = "N"
			}
			fmt.Fprintf(&sb, " %-12s|", cell)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Clock 把秒数格式化为 m:ss。
func Clock(sec float64) string {
	s := int(sec + 0.5)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
