package timbre

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"kanade/internal/conductor"
	"kanade/internal/stems"
)

// Jobs 是翻唱流水线需要的 Conductor 能力，测试中可替换。
type Jobs interface {
	Submit(engine, task string, params map[string]any) (string, error)
	Wait(ctx context.Context, id string) (conductor.Job, error)
	Cancel(id string)
}

// CoverRequest 是一次翻唱的输入：歌曲经分轨得到干净主唱，用 Infer 的模型与参数转换，再与伴奏混合。
type CoverRequest struct {
	Song  string
	Infer InferRequest // Input 由流水线填入
	// 混音增益（dB）。Backing 为 true 时把原曲的和声一并混入。
	VocalGainDB, InstGainDB float64
	Backing                 bool
	BackingGainDB           float64
}

// 流水线阶段。
const (
	StageStems = iota
	StageConvert
	StageMix
	StageDone
)

// StageTitles 是各阶段的显示名称。
var StageTitles = []string{"分轨与人声净化", "音色转换", "混音", "完成"}

// Cover 是运行中的翻唱流水线。状态由后台 goroutine 更新，Snapshot 可从任意 goroutine 读取。
type Cover struct {
	jobs     Jobs
	onChange func()
	cancel   context.CancelFunc

	mu    sync.Mutex
	stage int
	job   string // 当前阶段的任务 ID
	err   string
	// outputs 记录各阶段产物：干声 dry、伴奏 other、和声 backing、转换结果 converted、成品 mix。
	outputs map[string]string
}

// CoverState 是 Cover 的快照。
type CoverState struct {
	Stage   int
	Job     string
	Err     string
	Outputs map[string]string
}

// Running 报告流水线是否仍在进行。
func (s CoverState) Running() bool { return s.Err == "" && s.Stage < StageDone }

// StartCover 校验输入并在后台启动流水线；onChange 在状态变化时调用（可为 nil）。
func StartCover(jobs Jobs, r CoverRequest, onChange func()) (*Cover, error) {
	if r.Song == "" {
		return nil, errors.New("请选择歌曲")
	}
	probe := r.Infer
	probe.Input = r.Song // 只用于校验模型与参数，实际输入是分轨得到的干声
	if _, err := probe.Params(); err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cover{jobs: jobs, onChange: onChange, cancel: cancel, outputs: map[string]string{}}
	go c.run(ctx, r)
	return c, nil
}

// Snapshot 返回当前状态。
func (c *Cover) Snapshot() CoverState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.outputs))
	for k, v := range c.outputs {
		out[k] = v
	}
	return CoverState{Stage: c.stage, Job: c.job, Err: c.err, Outputs: out}
}

// Cancel 取消当前阶段的任务并停止流水线。
func (c *Cover) Cancel() {
	c.mu.Lock()
	job := c.job
	c.mu.Unlock()
	c.cancel()
	if job != "" {
		c.jobs.Cancel(job)
	}
}

func (c *Cover) set(fn func()) {
	c.mu.Lock()
	fn()
	c.mu.Unlock()
	c.onChange()
}

func (c *Cover) fail(err error) { c.set(func() { c.err = err.Error() }) }

// step 提交一个任务并等待完成，返回其产物。
func (c *Cover) step(ctx context.Context, stage int, engine, task string, params map[string]any) ([]string, error) {
	id, err := c.jobs.Submit(engine, task, params)
	if err != nil {
		return nil, err
	}
	c.set(func() { c.stage, c.job = stage, id })
	j, err := c.jobs.Wait(ctx, id)
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

func (c *Cover) run(ctx context.Context, r CoverRequest) {
	// 分轨只需要伴奏与降噪后的主唱（及其前置步骤，会顺带产出和声）。
	params, _ := stems.ChainParams(r.Song, []string{"other", "dry"})
	arts, err := c.step(ctx, StageStems, stems.Engine, "stems.chain", params)
	if err != nil {
		c.fail(err)
		return
	}
	for _, a := range arts {
		switch {
		case strings.HasSuffix(a, "_"+stems.DryKey+".wav"):
			c.set(func() { c.outputs["dry"] = a })
		case strings.HasSuffix(a, "_vocals_other.wav"):
			c.set(func() { c.outputs["backing"] = a })
		case strings.HasSuffix(a, "_other.wav"):
			c.set(func() { c.outputs["other"] = a })
		}
	}
	snap := c.Snapshot()
	if snap.Outputs["dry"] == "" || snap.Outputs["other"] == "" {
		c.fail(errors.New("分轨结果缺少干声或伴奏"))
		return
	}

	inf := r.Infer
	inf.Input = snap.Outputs["dry"]
	ip, _ := inf.Params()
	arts, err = c.step(ctx, StageConvert, Engine, "timbre.infer", ip)
	if err != nil || len(arts) == 0 {
		c.fail(firstErr(err, errors.New("转换没有输出")))
		return
	}
	converted := arts[0]
	c.set(func() { c.outputs["converted"] = converted })

	tracks := []map[string]any{
		{"path": converted, "gain_db": r.VocalGainDB},
		{"path": snap.Outputs["other"], "gain_db": r.InstGainDB},
	}
	if r.Backing && snap.Outputs["backing"] != "" {
		tracks = append(tracks, map[string]any{"path": snap.Outputs["backing"], "gain_db": r.BackingGainDB})
	}
	song := strings.TrimSuffix(filepath.Base(r.Song), filepath.Ext(r.Song))
	name := fmt.Sprintf("%s_cover_%s_pitch%+d", song, r.Infer.Model.Name, r.Infer.Pitch)
	arts, err = c.step(ctx, StageMix, Engine, "timbre.mix", map[string]any{"tracks": tracks, "name": name})
	if err != nil || len(arts) == 0 {
		c.fail(firstErr(err, errors.New("混音没有输出")))
		return
	}
	c.set(func() { c.outputs["mix"], c.stage, c.job = arts[0], StageDone, "" })
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
