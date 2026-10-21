package timbre

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
	"kanade/internal/stems"
)

// Jobs 是翻唱流水线需要的 Conductor 能力，测试中可替换。
type Jobs interface {
	Submit(engine, task string, params map[string]any) (string, error)
	Wait(ctx context.Context, id string) (conductor.Job, error)
	Cancel(id string)
}

// CoverSource 是翻唱的输入类型。
type CoverSource int

const (
	// SourceSong 是整首歌：先分轨净化得到主唱、和声与伴奏。
	SourceSong CoverSource = iota
	// SourceDry 是已经分好的纯干声：跳过分轨，可另给伴奏与和声。
	SourceDry
)

// HarmonyMode 决定和声如何进入成品。
type HarmonyMode int

const (
	// HarmonyConvert 用与主唱相同的模型与参数转换和声后混入（默认：成品里听不到原唱的声音）。
	HarmonyConvert HarmonyMode = iota
	// HarmonyOriginal 混入原曲和声（有的歌和声不是主唱本人）。
	HarmonyOriginal
	// HarmonyNone 不混入和声。
	HarmonyNone
)

// HarmonyModes 是界面上的和声选项，顺序与 HarmonyMode 一致。
var HarmonyModes = []string{"转换后混入（推荐）", "混入原曲和声", "不混入和声"}

// CoverRequest 是一次翻唱的输入：主唱（及和声）用 Infer 的模型与参数转换，再与伴奏混音。
type CoverRequest struct {
	Source CoverSource
	// Song 是整首歌（SourceSong）；StemModels 按步骤 ID 覆盖分轨模型，见 stems.Recommended。
	Song       string
	StemModels map[string]string
	// Vocal / Inst / Harmony 是干声模式的输入：主唱干声必填，伴奏与和声可选。
	Vocal, Inst, Harmony string
	// KeepDryLevel 只用于干声模式：为 true 时转换结果对齐输入干声 / 和声的电平（三者来自同一次分轨时保持原比例），
	// 否则按伴奏智能推荐（适合自己录的干声与另找的伴奏）。没有伴奏时总是对齐输入。
	KeepDryLevel bool

	Infer       InferRequest // Input 由流水线填入；Normalize 被忽略（混音按原曲对齐响度）
	HarmonyMode HarmonyMode
	Mix         MixOptions
}

// 流水线阶段。
const (
	StageStems = iota
	StageConvert
	StageHarmony
	StageMix
	StageDone
)

// StageTitles 是各阶段的显示名称。
var StageTitles = []string{"分轨与人声净化", "转换主唱", "转换和声", "混音", "完成"}

// Cover 是运行中的翻唱流水线。状态由后台 goroutine 更新，Snapshot 可从任意 goroutine 读取。
type Cover struct {
	jobs     Jobs
	onChange func()
	cancel   context.CancelFunc

	mu      sync.Mutex
	stage   int
	skipped map[int]bool // 被跳过的阶段（干声模式的分轨、无和声时的和声转换）
	job     string       // 当前阶段的任务 ID
	err     string
	// outputs 记录各阶段产物：主唱干声 dry、伴奏 other、原曲和声 backing、转换后的主唱 converted、
	// 转换后的和声 converted_backing、成品 mix（及 mix_mp3）。
	outputs map[string]string
	report  *MixReport
}

// CoverState 是 Cover 的快照。
type CoverState struct {
	Stage   int
	Skipped map[int]bool
	Job     string
	Err     string
	Outputs map[string]string
	Report  *MixReport
}

// Running 报告流水线是否仍在进行。
func (s CoverState) Running() bool { return s.Err == "" && s.Stage < StageDone }

// Validate 检查输入是否完整，返回面向用户的错误。
func (r CoverRequest) Validate() error {
	probe := r.Infer
	switch r.Source {
	case SourceSong:
		if r.Song == "" {
			return errors.New("请选择歌曲")
		}
		probe.Input = r.Song
	case SourceDry:
		if r.Vocal == "" {
			return errors.New("请选择主唱干声")
		}
		probe.Input = r.Vocal
	default:
		return errors.New("未知的输入类型")
	}
	// 只用于校验模型与参数，实际输入由流水线填入。
	if _, err := probe.Params(); err != nil {
		return err
	}
	return r.Mix.validate()
}

// StartCover 校验输入并在后台启动流水线；onChange 在状态变化时调用（可为 nil）。
func StartCover(jobs Jobs, r CoverRequest, onChange func()) (*Cover, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cover{jobs: jobs, onChange: onChange, cancel: cancel, outputs: map[string]string{}, skipped: map[int]bool{}}
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
	skipped := make(map[int]bool, len(c.skipped))
	for k, v := range c.skipped {
		skipped[k] = v
	}
	return CoverState{Stage: c.stage, Skipped: skipped, Job: c.job, Err: c.err, Outputs: out, Report: c.report}
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

func (c *Cover) skip(stage int) { c.set(func() { c.skipped[stage] = true }) }

// step 提交一个任务并等待完成，返回其产物。
func (c *Cover) step(ctx context.Context, stage int, engine, task string, params map[string]any) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("已取消")
	}
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

// convert 用 r.Infer 转换 input，返回转换结果。输出不做峰值归一化：电平由混音对齐到原曲。
func (c *Cover) convert(ctx context.Context, stage int, r CoverRequest, input string) (string, error) {
	inf := r.Infer
	inf.Input, inf.Normalize = input, false
	ip, err := inf.Params()
	if err != nil {
		return "", err
	}
	arts, err := c.step(ctx, stage, Engine, "timbre.infer", ip)
	if err != nil {
		return "", err
	}
	if len(arts) == 0 {
		return "", errors.New("转换没有输出")
	}
	return arts[0], nil
}

func (c *Cover) run(ctx context.Context, r CoverRequest) {
	// 1. 输入：整首歌经分轨得到净化后的主唱、和声与伴奏；干声模式直接使用用户给的文件。
	dry, inst, backing := r.Vocal, r.Inst, r.Harmony
	if r.Source == SourceSong {
		// 分轨只需要伴奏与降噪后的主唱（及其前置步骤，会顺带产出和声）。
		params, _ := stems.ChainParams(r.Song, []string{"other", "dry"}, r.StemModels)
		arts, err := c.step(ctx, StageStems, stems.Engine, "stems.chain", params)
		if err != nil {
			c.fail(err)
			return
		}
		dry, inst, backing = "", "", ""
		for _, a := range arts {
			switch {
			case strings.HasSuffix(a, "_"+stems.DryKey+".wav"):
				dry = a
			case strings.HasSuffix(a, "_vocals_other.wav"):
				backing = a
			case strings.HasSuffix(a, "_other.wav"):
				inst = a
			}
		}
		if dry == "" || inst == "" {
			c.fail(errors.New("分轨结果缺少干声或伴奏"))
			return
		}
	} else {
		c.skip(StageStems)
	}
	c.set(func() { c.outputs["dry"], c.outputs["other"], c.outputs["backing"] = dry, inst, backing })

	// 2. 转换主唱。
	converted, err := c.convert(ctx, StageConvert, r, dry)
	if err != nil {
		c.fail(err)
		return
	}
	c.set(func() { c.outputs["converted"] = converted })

	// 3. 和声：与主唱同一模型、同一组参数转换，或原样混入。
	convertedBacking := ""
	if backing != "" && r.HarmonyMode == HarmonyConvert {
		if convertedBacking, err = c.convert(ctx, StageHarmony, r, backing); err != nil {
			c.fail(err)
			return
		}
		c.set(func() { c.outputs["converted_backing"] = convertedBacking })
	} else {
		c.skip(StageHarmony)
	}

	// 4. 混音：转换后的主唱 / 和声对齐原曲对应分轨的演唱段响度，再加混响与母带处理。
	vocalRef, backingRef := dry, backing
	if r.Source == SourceDry && inst != "" && !r.KeepDryLevel {
		vocalRef, backingRef = "", "" // 由混音按伴奏推荐电平
	}
	tracks := []MixTrack{{Path: converted, Role: RoleVocal, Ref: vocalRef, GainDB: r.Mix.VocalGainDB}}
	if inst != "" {
		tracks = append(tracks, MixTrack{Path: inst, Role: RoleInst, GainDB: r.Mix.InstGainDB})
	}
	switch {
	case convertedBacking != "":
		tracks = append(tracks, MixTrack{Path: convertedBacking, Role: RoleHarmony, Ref: backingRef, GainDB: r.Mix.HarmonyGainDB})
	case backing != "" && r.HarmonyMode == HarmonyOriginal:
		// 原曲和声本身带混响且已在原混音电平上，原样相加。
		tracks = append(tracks, MixTrack{Path: backing, Role: RoleOther, GainDB: r.Mix.HarmonyGainDB})
	}
	arts, err := c.step(ctx, StageMix, Engine, "timbre.mix", r.Mix.Params(tracks, r.coverName()))
	if err != nil {
		c.fail(err)
		return
	}
	mix, mp3, report := splitMixArtifacts(arts)
	if mix == "" {
		c.fail(errors.New("混音没有输出"))
		return
	}
	rep, _ := ReadMixReport(report)
	c.set(func() {
		c.outputs["mix"], c.stage, c.job, c.report = mix, StageDone, "", rep
		if mp3 != "" {
			c.outputs["mix_mp3"] = mp3
		}
	})
}

// coverName 是成品的建议文件名：<曲名>_cover_<模型>_pitch<变调>。
func (r CoverRequest) coverName() string {
	src := r.Song
	if r.Source == SourceDry {
		src = r.Vocal
	}
	song := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	return fmt.Sprintf("%s_cover_%s_pitch%+d", song, r.Infer.Model.Name, r.Infer.Pitch)
}

// splitMixArtifacts 按扩展名区分 timbre.mix 的成品 WAV、MP3 与报告 JSON。
func splitMixArtifacts(arts []string) (wav, mp3, report string) {
	for _, a := range arts {
		switch strings.ToLower(filepath.Ext(a)) {
		case ".wav":
			wav = a
		case ".mp3":
			mp3 = a
		case ".json":
			report = a
		}
	}
	return
}

// MixReport 是 timbre.mix 写出的报告：各轨实际应用的增益与演唱段电平。
type MixReport struct {
	SampleRate   int     `json:"sample_rate"`
	MasterGainDB float64 `json:"master_gain_db"`
	Tracks       []struct {
		Path      string   `json:"path"`
		Role      string   `json:"role"`
		Basis     string   `json:"basis"` // 原曲分轨 / 智能推荐 / 原电平
		OffsetDB  float64  `json:"offset_db"`
		GainDB    float64  `json:"gain_db"`
		LevelDBFS *float64 `json:"level_dbfs"`
	} `json:"tracks"`
}

// ReadMixReport 读取混音报告；path 为空时返回 nil 与错误。
func ReadMixReport(path string) (*MixReport, error) {
	if path == "" {
		return nil, errors.New("没有混音报告")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r MixReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
