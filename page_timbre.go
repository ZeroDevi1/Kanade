package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/dataset"
	"kanade/internal/stems"
	"kanade/internal/timbre"
	"kanade/internal/ui/fluent"
)

// 音色转换页的标签。
const (
	timbreTabConvert = iota
	timbreTabCover
	timbreTabTrain
	timbreTabModels
)

// 翻唱的分轨模型选择（专业模式）。
var coverStemChoices = []string{"评估推荐（推荐）", "默认（与 VoiceLab 一致）", "与「分轨」页相同"}

// rateAuto 是训练采样率下拉框中的自动选项。
const rateAuto = "自动"

// timbreState 是音色转换页的界面状态。
type timbreState struct {
	tab int

	// models 是模型列表缓存；modelsStale 为 true 时下次构建重新扫描（训练完成、导入、删除后置位）。
	models      []timbre.Model
	modelsStale bool
	modelName   string // 选中的模型名

	// 推理参数（转换与翻唱共用）。inferMode 为 modeSmart 时只用 content 与 shift。
	inferMode int
	content   string // timbre.ContentKinds 的 Title（仅「转换」；翻唱固定为歌声）
	shift     string // timbre.VoiceShifts 的 Title
	// 专业模式参数。
	preset         string // timbre.Preset.Title
	pitch          float64
	f0             string
	indexRate      float64
	protect        float64
	volumeEnvelope float64
	stereo         string // timbre.StereoModes 的 Title
	normalize      bool

	input      string
	job        string
	convertOut resultSet

	// 翻唱。
	coverSource  int // timbre.CoverSource
	song         string
	vocal        string
	inst         string
	harmonyFile  string
	harmonyMode  string // timbre.HarmonyModes 之一
	stemChoice   string // coverStemChoices 之一
	keepDryLevel bool
	mix          timbre.MixOptions
	cover        *timbre.Cover
	coverName    string // 本次翻唱的曲名，用于结果的建议文件名
	coverOut     resultSet

	// 训练。trainMode 为 modeSmart 时采样率、轮数、batch 与保存间隔交给引擎自动选择。
	trainMode        int
	trainName        string
	trainDataset     string
	trainSummary     *dataset.Summary
	trainRate        string
	trainEpochs      float64
	trainBatch       float64
	trainSaveEvery   float64
	trainF0          string
	trainCores       float64 // 0 为自动
	trainCut         string
	trainChunk       float64
	trainOverlap     float64
	trainNorm        string
	trainIndex       string
	trainCacheGPU    string // trainCacheTitles 之一
	trainPrecision   string // timbre.Precisions 之一
	trainCheckpoint  bool
	trainSkipPre     bool
	trainSkipExtract bool
	trainJob         string
	trainScanned     string // 已为其刷新过模型列表的训练任务

	// 导入。
	importName, importWeights, importIndex string
	confirmDelete                          string

	err string
}

func newTimbreState() timbreState {
	s := timbreState{modelsStale: true, normalize: true, stereo: timbre.StereoModes[1].Title,
		content: timbre.ContentKinds[0].Title, shift: timbre.VoiceShifts[0].Title,
		harmonyMode: timbre.HarmonyModes[0], stemChoice: coverStemChoices[0], mix: timbre.DefaultMix(),
		trainRate: "48000", trainEpochs: 200, trainBatch: 4, trainSaveEvery: 10, trainF0: "rmvpe",
		trainCut: timbre.CutModes[0], trainChunk: 3, trainOverlap: 0.3, trainNorm: timbre.Normalizations[0],
		trainIndex: timbre.IndexAlgorithms[0], trainCacheGPU: trainCacheTitles[0], trainPrecision: timbre.Precisions[0]}
	s.applyPreset(timbre.Presets[2])
	return s
}

func (s *timbreState) applyPreset(p timbre.Preset) {
	s.preset, s.f0, s.indexRate, s.protect, s.volumeEnvelope = p.Title, p.F0, p.IndexRate, p.Protect, p.VolumeEnvelope
}

func (s *timbreState) model() (timbre.Model, bool) {
	for _, m := range s.models {
		if m.Name == s.modelName {
			return m, true
		}
	}
	return timbre.Model{}, false
}

func (s *timbreState) stereoKey() string {
	for _, m := range timbre.StereoModes {
		if m.Title == s.stereo {
			return m.Key
		}
	}
	return "mono"
}

func (s *timbreState) shiftPitch() int {
	for _, v := range timbre.VoiceShifts {
		if v.Title == s.shift {
			return v.Pitch
		}
	}
	return 0
}

// inferRequest 按当前参数模式组装推理参数。singing 为 true 时（翻唱）智能模式固定使用歌声预设。
func (s *timbreState) inferRequest(input string, singing bool) timbre.InferRequest {
	m, _ := s.model()
	if s.inferMode == modeSmart {
		preset := timbre.ContentKinds[0].Preset
		for _, k := range timbre.ContentKinds {
			if k.Title == s.content && !singing {
				preset = k.Preset
			}
		}
		r := timbre.SmartInfer(m, preset, s.shiftPitch())
		r.Input = input
		return r
	}
	return timbre.InferRequest{Model: m, Input: input, Pitch: int(s.pitch), F0: s.f0, IndexRate: s.indexRate,
		Protect: s.protect, VolumeEnvelope: s.volumeEnvelope, Stereo: s.stereoKey(), Normalize: s.normalize}
}

// timbrePage 是音色转换页：RVC 转换、一键翻唱、训练与模型管理。
func (a *app) timbrePage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "音色转换", "Timbre · RVC：把人声或歌声转换为目标音色，可用自己的数据集训练模型。")
	if a.backendUnavailable(c, tk) {
		return
	}
	s := &a.timbre
	if s.modelsStale {
		s.modelsStale = false
		s.models = timbre.ListModels(a.be.layout)
		if _, ok := s.model(); !ok && len(s.models) > 0 {
			s.modelName = s.models[0].Name
		}
	}
	ui.Segmented(c, &s.tab, "转换", "翻唱", "训练", "模型").Label("功能")
	switch s.tab {
	case timbreTabConvert:
		a.timbreConvert(c, tk)
	case timbreTabCover:
		a.timbreCover(c, tk)
	case timbreTabTrain:
		a.timbreTrain(c, tk)
	default:
		a.timbreModels(c, tk)
	}
	if s.err != "" {
		ui.Text(c, s.err).FontSize(12).TextColor(c.Theme().Danger)
	}
}

// timbreModelSelect 选择模型；没有模型时提示去训练或导入。返回是否有可用模型。
func (a *app) timbreModelSelect(c *ui.Context) bool {
	s := &a.timbre
	if len(s.models) == 0 {
		ui.Text(c, "还没有音色模型：在「训练」中训练，或在「模型」中导入已有的 .pth / .index。").TextColor(c.Theme().TextMuted)
		return false
	}
	names := titles(s.models, func(m timbre.Model) string { return m.Name })
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Select(c, &s.modelName, names).Label("音色模型").Width(240)
		if m, ok := s.model(); ok {
			info := "导入"
			if m.Trained {
				info = fmt.Sprintf("训练 %d 轮", m.Epoch)
			}
			if m.Index == "" {
				info += " · 无索引"
			}
			ui.Text(c, info).FontSize(12).TextColor(c.Theme().TextMuted)
		}
	})
	return true
}

// timbreParams 显示推理参数。智能模式只有内容类型（翻唱时省略）与音高方向；
// 专业模式为预设、音高算法、立体声、变调与检索比例等全部参数。
func (a *app) timbreParams(c *ui.Context, singing bool) {
	t := c.Theme()
	s := &a.timbre
	modeSwitch(c, "infer-mode", &s.inferMode, "按内容类型套用推荐预设，音高只需选择是否跨性别。")
	if s.inferMode == modeSmart {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if !singing {
				ui.Select(c, &s.content, titles(timbre.ContentKinds, func(k timbre.ContentKind) string { return k.Title })).
					Label("内容").Width(120)
			}
			ui.Select(c, &s.shift, titles(timbre.VoiceShifts, func(v timbre.VoiceShift) string { return v.Title })).Label("音高").Width(240)
		})
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.Select(c, &s.preset, titles(timbre.Presets, func(p timbre.Preset) string { return p.Title })).Label("预设").Width(200).Changed() {
			for _, p := range timbre.Presets {
				if p.Title == s.preset {
					s.applyPreset(p)
				}
			}
		}
		ui.Select(c, &s.f0, timbre.F0Methods).Label("音高算法").Width(120)
		modes := make([]string, len(timbre.StereoModes))
		for i, m := range timbre.StereoModes {
			modes[i] = m.Title
		}
		ui.Select(c, &s.stereo, modes).Label("立体声").Width(220)
	})
	for _, p := range timbre.Presets {
		if p.Title == s.preset {
			ui.Text(c, p.Hint+"。变调：男转女常用 +12，女转男 −3 ~ −12，歌声先从 0 开始。").FontSize(12).TextColor(t.TextMuted)
		}
	}
	labeledSlider(c, "变调（半音）", &s.pitch, timbre.MinPitch, timbre.MaxPitch, "%+.0f")
	s.pitch = float64(int(s.pitch + 0.5*sign(s.pitch))) // 变调按整数半音
	labeledSlider(c, "检索比例", &s.indexRate, 0, 1, "%.2f")
	labeledSlider(c, "辅音保护", &s.protect, 0, 0.5, "%.2f")
	labeledSlider(c, "响度包络", &s.volumeEnvelope, 0, 1, "%.2f")
	if !singing {
		// 翻唱的电平由混音对齐原曲，不做峰值归一化。
		ui.Checkbox(c, &s.normalize, "输出峰值归一化")
	}
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// jobStatus 显示任务进度、取消按钮与失败原因，返回任务是否进行中。
func (a *app) jobStatus(c *ui.Context, id string) (conductor.Job, bool) {
	t := c.Theme()
	if id == "" {
		return conductor.Job{}, false
	}
	j, _ := a.be.cond.Job(id)
	active := !j.State.Finished()
	if active {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "取消").Clicked() {
				a.be.cond.Cancel(id)
			}
			ui.Spinner(c)
			ui.Text(c, j.Message).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
		})
		ui.Progress(c, j.Progress)
	}
	if j.State == conductor.JobFailed {
		ui.Text(c, "失败："+j.Err).FontSize(12).TextColor(t.Danger).Selectable()
	}
	return j, active
}

func (a *app) timbreConvert(c *ui.Context, tk *fluent.Tokens) {
	s := &a.timbre
	cardRow(c, cardMinWidth, func() ui.Element { return a.timbreConvertCard(c, tk) },
		func() ui.Element {
			return card(c, tk, "结果", func() { s.convertOut.show(c, tk, a, "转换结果会显示在这里。") })
		})
}

// timbreConvertCard 是转换表单：模型、输入人声与参数，提交 timbre.infer 并把完成的结果加入结果列表。
func (a *app) timbreConvertCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	s := &a.timbre
	return card(c, tk, "转换", func() {
		if !a.timbreModelSelect(c) {
			return
		}
		a.audioPicker(c, "输入人声", &s.input, directAudioExts)
		ui.Text(c, "整首歌请用「翻唱」，或先用「分轨」得到干净主唱。").FontSize(12).TextColor(c.Theme().TextMuted)
		a.timbreParams(c, false)
		_, active := a.jobStatusPeek(s.job)
		if ui.PrimaryButton(c, "开始转换").Disabled(active).AlignSelf(ui.Start).Clicked() {
			params, err := s.inferRequest(s.input, false).Params()
			if err == nil {
				s.job, err = a.be.cond.Submit(timbre.Engine, "timbre.infer", params)
			}
			s.err = errText(err)
		}
		j, _ := a.jobStatus(c, s.job)
		if j.State == conductor.JobDone && len(j.Artifacts) > 0 {
			in := strings.TrimSuffix(filepath.Base(s.input), filepath.Ext(s.input))
			m, _ := s.model()
			s.convertOut.add(a, j.Artifacts[0], in+" → "+m.Name, fmt.Sprintf("%s_%s", in, m.Name), true)
		}
	})
}

// jobStatusPeek 返回任务快照与是否进行中，不构建界面。
func (a *app) jobStatusPeek(id string) (conductor.Job, bool) {
	if id == "" {
		return conductor.Job{}, false
	}
	j, _ := a.be.cond.Job(id)
	return j, !j.State.Finished()
}

// coverStemModels 返回翻唱使用的分轨模型：智能模式为评估推荐，专业模式按选择。
func (a *app) coverStemModels() map[string]string {
	s := &a.timbre
	if s.inferMode == modeSmart || s.stemChoice == coverStemChoices[0] {
		return stems.Recommended
	}
	if s.stemChoice == coverStemChoices[2] {
		return a.stems.models
	}
	return nil
}

func (a *app) coverRequest() timbre.CoverRequest {
	s := &a.timbre
	r := timbre.CoverRequest{Source: timbre.CoverSource(s.coverSource), Song: s.song, Vocal: s.vocal, Inst: s.inst,
		Harmony: s.harmonyFile, StemModels: a.coverStemModels(), Infer: s.inferRequest("", true), Mix: s.mix}
	if s.inferMode == modeSmart {
		// 智能模式：和声转换后混入、混响用默认值，只保留人声音量与 MP3 两项。
		def := timbre.DefaultMix()
		def.VocalGainDB, def.MP3 = s.mix.VocalGainDB, s.mix.MP3
		r.Mix, r.HarmonyMode = def, timbre.HarmonyConvert
		return r
	}
	for i, m := range timbre.HarmonyModes {
		if m == s.harmonyMode {
			r.HarmonyMode = timbre.HarmonyMode(i)
		}
	}
	r.KeepDryLevel = s.keepDryLevel
	return r
}

func (a *app) timbreCover(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.timbre
	var st timbre.CoverState
	if s.cover != nil {
		st = s.cover.Snapshot()
	}
	// 结果卡片在表单之后构建：表单在本帧开始翻唱时更新 st。
	cardRow(c, cardMinWidth, func() ui.Element {
		return card(c, tk, "一键翻唱", func() {
			ui.Text(c, "整首歌：分轨净化 → 主唱与和声转换音色 → 对齐原曲响度、加混响后与伴奏混音。纯干声：跳过分轨，直接转换并混音。").
				FontSize(12).TextColor(t.TextMuted)
			if !a.timbreModelSelect(c) {
				return
			}
			ui.Segmented(c, &s.coverSource, "整首歌（自动分轨）", "纯干声").Label("输入类型")
			if timbre.CoverSource(s.coverSource) == timbre.SourceSong {
				a.audioPicker(c, "歌曲", &s.song, anyAudioExts())
			} else {
				a.audioPicker(c, "主唱干声", &s.vocal, directAudioExts)
				a.audioPicker(c, "伴奏（可选）", &s.inst, directAudioExts)
				a.audioPicker(c, "和声（可选）", &s.harmonyFile, directAudioExts)
			}
			a.timbreParams(c, true)
			a.coverMixParams(c)
			if timbre.CoverSource(s.coverSource) == timbre.SourceSong {
				if files, mb := stems.Missing(a.be.layout, []string{"other", "dry"}, a.coverStemModels()); len(files) > 0 {
					ui.Textf(c, "首次运行会下载 %d 个分轨模型（约 %.1f GB）。", len(files), float64(mb)/1000).FontSize(12).TextColor(t.TextMuted)
				}
			}
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				if ui.PrimaryButton(c, "开始翻唱").Disabled(s.cover != nil && st.Running()).Clicked() {
					r := a.coverRequest()
					cv, err := timbre.StartCover(a.be.cond, r, func() { a.post(func() {}) })
					if err == nil {
						src := r.Song
						if r.Source == timbre.SourceDry {
							src = r.Vocal
						}
						s.cover, s.coverName = cv, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
						st = cv.Snapshot()
					}
					s.err = errText(err)
				}
			})
			if s.cover == nil {
				return
			}
			a.coverStages(c, st)
		})
	}, func() ui.Element {
		return card(c, tk, "结果", func() {
			a.registerCoverOutputs(st)
			s.coverOut.show(c, tk, a, "翻唱成品会显示在这里。")
			if rep := st.Report; rep != nil {
				coverReport(c, rep)
			}
		})
	})
}

// coverMixParams 显示混音参数：智能模式只有人声音量与 MP3；专业模式有分轨模型、和声处理、各轨偏移与混响。
func (a *app) coverMixParams(c *ui.Context) {
	t := c.Theme()
	s := &a.timbre
	m := &s.mix
	ui.Text(c, "混音").FontWeight(600)
	if s.inferMode == modeSmart {
		ui.Text(c, "主唱与和声自动对齐原曲的演唱段响度，加人声混响，成品峰值 −1 dBFS、24 bit。").FontSize(12).TextColor(t.TextMuted)
		labeledSlider(c, "人声音量", &m.VocalGainDB, -6, 6, "%+.1f dB")
		ui.Checkbox(c, &m.MP3, "同时导出 MP3")
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if timbre.CoverSource(s.coverSource) == timbre.SourceSong {
			ui.Select(c, &s.stemChoice, coverStemChoices).Label("分轨模型").Width(220)
		}
		ui.Select(c, &s.harmonyMode, timbre.HarmonyModes).Label("和声").Width(200)
	})
	if timbre.CoverSource(s.coverSource) == timbre.SourceDry {
		ui.Checkbox(c, &s.keepDryLevel, "保持输入干声与伴奏的原有比例（三者来自同一首歌的分轨时勾选）")
	}
	ui.Text(c, "以下增益都是相对智能推荐电平的偏移，0 即推荐值。").FontSize(12).TextColor(t.TextMuted)
	labeledSlider(c, "主唱", &m.VocalGainDB, -timbre.MaxGainOffsetDB, timbre.MaxGainOffsetDB, "%+.1f dB")
	labeledSlider(c, "和声", &m.HarmonyGainDB, -timbre.MaxGainOffsetDB, timbre.MaxGainOffsetDB, "%+.1f dB")
	labeledSlider(c, "伴奏", &m.InstGainDB, -timbre.MaxGainOffsetDB, timbre.MaxGainOffsetDB, "%+.1f dB")
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Checkbox(c, &m.Reverb, "人声混响")
		ui.Checkbox(c, &m.MP3, "同时导出 MP3")
		if ui.Button(c, "恢复推荐值").Clicked() {
			mp3 := m.MP3
			*m = timbre.DefaultMix()
			m.MP3 = mp3
		}
	})
	if m.Reverb {
		labeledSlider(c, "湿声电平", &m.ReverbWetDB, timbre.MinWetDB, timbre.MaxWetDB, "%.1f dB")
		labeledSlider(c, "混响时间", &m.RT60, timbre.MinRT60, timbre.MaxRT60, "%.2f s")
		labeledSlider(c, "预延迟", &m.PreDelayMS, 0, timbre.MaxPreDelayMS, "%.0f ms")
		ui.Text(c, "和声的湿声比主唱多 3 dB，让和声退到主唱后面。").FontSize(12).TextColor(t.TextMuted)
	}
}

// coverStages 显示流水线各阶段的状态与当前任务进度。
func (a *app) coverStages(c *ui.Context, st timbre.CoverState) {
	t := c.Theme()
	for i, title := range timbre.StageTitles[:timbre.StageDone] {
		mark, color := "○", t.TextMuted
		switch {
		case st.Skipped[i]:
			mark, title = "–", title+"（跳过）"
		case st.Err != "" && i == st.Stage:
			mark, color = "✕", t.Danger
		case i < st.Stage:
			mark, color = "✓", t.Success
		case i == st.Stage:
			mark, color = "●", t.Accent
		}
		ui.Row(c.Key("stage-" + title)).Gap(8).Children(func() {
			ui.Text(c, mark).TextColor(color).Width(16)
			ui.Text(c, title).TextColor(color)
		})
	}
	if st.Job != "" && st.Running() {
		a.jobStatus(c, st.Job)
	}
	if st.Err != "" {
		ui.Text(c, st.Err).FontSize(12).TextColor(t.Danger).Selectable()
	}
}

// coverOutputs 是翻唱各产物在结果列表中的标题，成品在最后登记，使预览停在成品上。
var coverOutputs = []struct{ key, title string }{
	{"dry", "原曲干声"}, {"other", "伴奏"}, {"backing", "原曲和声"},
	{"converted", "转换后的主唱"}, {"converted_backing", "转换后的和声"},
	{"mix_mp3", "成品（MP3）"}, {"mix", "成品"},
}

// registerCoverOutputs 把流水线新出现的产物登记到结果列表；干声模式下用户自己的输入文件不登记。
func (a *app) registerCoverOutputs(st timbre.CoverState) {
	s := &a.timbre
	for _, o := range coverOutputs {
		p := st.Outputs[o.key]
		if p == "" || st.Skipped[timbre.StageStems] && (o.key == "dry" || o.key == "other" || o.key == "backing") {
			continue
		}
		name := s.coverName + "_" + o.key
		if o.key == "mix" || o.key == "mix_mp3" {
			name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		}
		s.coverOut.add(a, p, o.title, name, o.key == "mix")
	}
}

// coverReport 显示混音实际应用的电平，说明智能推荐做了什么。
func coverReport(c *ui.Context, rep *timbre.MixReport) {
	t := c.Theme()
	role := map[string]string{timbre.RoleVocal: "主唱", timbre.RoleHarmony: "和声", timbre.RoleInst: "伴奏", timbre.RoleOther: "原曲和声"}
	ui.Text(c, "混音电平").FontSize(12).FontWeight(600)
	for _, tr := range rep.Tracks {
		level := "—"
		if tr.LevelDBFS != nil {
			level = fmt.Sprintf("%.1f dBFS", *tr.LevelDBFS)
		}
		ui.Textf(c, "%s：%s，增益 %+.1f dB（其中偏移 %+.1f dB），演唱段电平 %s", role[tr.Role], tr.Basis, tr.GainDB, tr.OffsetDB, level).
			FontSize(12).TextColor(t.TextMuted)
	}
	ui.Textf(c, "母带增益 %+.1f dB → 峰值 −1 dBFS", rep.MasterGainDB).FontSize(12).TextColor(t.TextMuted)
}

func (a *app) timbreTrain(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.timbre
	card(c, tk, "训练 RVC 模型", func() {
		ui.Text(c, "v2 + f0 模型。数据集为一个目录的干净人声（建议先用「分轨」净化）；目录中有同名 .list 时只使用其中列出的音频。").
			FontSize(12).TextColor(t.TextMuted)
		modeSwitch(c, "train-mode", &s.trainMode, "采样率按数据集、轮数按数据时长、batch 按显存自动选择。")
		ui.Field(c, "模型名", func() {
			ui.TextInput(c, &s.trainName).Placeholder("例如 xingtong_v2_48k").Width(320)
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "数据集").FontSize(12).Width(64)
			name := "未选择"
			if s.trainDataset != "" {
				name = s.trainDataset
			}
			ui.Text(c, name).FontSize(12).Font("monospace").TextColor(t.TextMuted).Grow(1).SingleLine()
			if ui.Button(c, "选择目录…").Clicked() {
				a.pickDir("选择数据集目录", func(dir string) {
					s.trainDataset = dir
					sum, err := dataset.Inspect(dir, "")
					s.trainSummary, s.err = &sum, errText(err)
					if s.trainName == "" {
						s.trainName = filepath.Base(dir)
					}
				})
			}
		})
		if sum := s.trainSummary; sum != nil {
			text := fmt.Sprintf("%d 个音频文件", sum.Audio)
			if sum.List != "" {
				text += fmt.Sprintf(" · 使用 %s：%d 条，其中 %d 条找到音频", filepath.Base(sum.List), sum.Listed, sum.Matched)
			} else {
				text += " · 未找到 .list，将使用全部音频"
			}
			ui.Text(c, text).FontSize(12).TextColor(t.TextMuted)
			if len(sum.Missing) > 0 {
				ui.Textf(c, "找不到音频的条目（前 %d 条）：%v", len(sum.Missing), sum.Missing).FontSize(12).TextColor(t.Warning)
			}
		}
		for _, m := range s.models {
			if m.Trained && m.Name == strings.TrimSpace(s.trainName) {
				ui.Textf(c, "已有同名模型（%d 轮）：将从上次的检查点续训。", m.Epoch).FontSize(12).TextColor(t.Warning)
			}
		}
		if s.trainMode == modePro {
			a.timbreTrainPro(c)
		}
		_, active := a.jobStatusPeek(s.trainJob)
		if ui.PrimaryButton(c, "开始训练").Disabled(active).AlignSelf(ui.Start).Clicked() {
			params, err := s.trainRequest().Params()
			if err == nil {
				s.trainJob, err = a.be.cond.Submit(timbre.Engine, "timbre.train", params)
			}
			s.err = errText(err)
		}
		j, _ := a.jobStatus(c, s.trainJob)
		if j.State == conductor.JobDone {
			ui.Text(c, "训练完成，模型已出现在列表中。").FontSize(12).TextColor(t.Success)
			// 每个训练任务完成后只重新扫描一次模型目录。
			if s.trainScanned != s.trainJob {
				s.trainScanned, s.modelsStale = s.trainJob, true
			}
		}
		ui.Text(c, "训练目录："+filepath.Join(timbre.Root(a.be.layout), "logs")).FontSize(12).TextColor(t.TextMuted).Selectable()
	})
}

// trainCacheTitles 是「数据缓存到显存」的选项，与 timbre.CacheGPUModes 一一对应。
var trainCacheTitles = []string{"自动（按数据量）", "开", "关"}

// timbreTrainPro 是训练的专业参数。
func (a *app) timbreTrainPro(c *ui.Context) {
	t := c.Theme()
	s := &a.timbre
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		rates := []string{rateAuto}
		for _, r := range timbre.SampleRates {
			rates = append(rates, strconv.Itoa(r))
		}
		ui.Field(c, "采样率", func() { ui.Select(c, &s.trainRate, rates).Width(120) })
		ui.Field(c, "总轮数", func() { ui.NumberInput(c, &s.trainEpochs, 1, 10000, 10).Width(120) })
		ui.Field(c, "batch", func() { ui.NumberInput(c, &s.trainBatch, 1, 64, 1).Width(100) })
		ui.Field(c, "每 N 轮保存", func() { ui.NumberInput(c, &s.trainSaveEvery, 1, 1000, 1).Width(110) })
	})
	ui.Text(c, "显存不足（如 6 GB）时把 batch 降到 2 或开启梯度检查点。续训时总轮数需大于已训练轮数，采样率须与上次相同。").
		FontSize(12).TextColor(t.TextMuted)
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "音高算法", func() { ui.Select(c, &s.trainF0, timbre.F0Methods).Width(110) })
		ui.Field(c, "CPU 进程数（0 自动）", func() { ui.NumberInput(c, &s.trainCores, 0, 64, 1).Width(110) })
		ui.Field(c, "索引算法", func() { ui.Select(c, &s.trainIndex, timbre.IndexAlgorithms).Width(110) })
	})
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "切片方式", func() { ui.Select(c, &s.trainCut, timbre.CutModes).Width(120) })
		ui.Field(c, "切片长度（秒）", func() { ui.NumberInput(c, &s.trainChunk, 0.5, 10, 0.5).Width(110) })
		ui.Field(c, "切片重叠（秒）", func() { ui.NumberInput(c, &s.trainOverlap, 0, 1, 0.1).Width(110) })
		ui.Field(c, "响度归一化", func() { ui.Select(c, &s.trainNorm, timbre.Normalizations).Width(110) })
	})
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "数据缓存到显存", func() { ui.Select(c, &s.trainCacheGPU, trainCacheTitles).Width(200) })
		ui.Field(c, "训练精度", func() { ui.Select(c, &s.trainPrecision, timbre.Precisions).Width(110) })
	})
	ui.Text(c, "缓存到显存后每步不再经 PCIe 传输数据，Oculink 等外接显卡上提速明显；自动时数据量不超过显存的 10% 才缓存。"+
		"bf16 需要 RTX 30 及以后的显卡，fp32 最慢。").FontSize(12).TextColor(t.TextMuted)
	ui.Row(c).Gap(16).Children(func() {
		ui.Checkbox(c, &s.trainCheckpoint, "梯度检查点（省显存）")
		ui.Checkbox(c, &s.trainSkipPre, "跳过预处理（续训）")
		ui.Checkbox(c, &s.trainSkipExtract, "跳过特征提取（续训）")
	})
}

// trainRequest 按当前模式组装训练参数：智能模式的采样率、轮数、batch 与保存间隔为 0（引擎自动选择）。
func (s *timbreState) trainRequest() timbre.TrainRequest {
	r := timbre.TrainRequest{Name: s.trainName, Dataset: s.trainDataset, F0: "rmvpe"}
	if s.trainMode == modeSmart {
		return r
	}
	r.SampleRate, _ = strconv.Atoi(s.trainRate) // 「自动」解析失败即 0
	r.Epochs, r.BatchSize, r.SaveEvery = int(s.trainEpochs), int(s.trainBatch), int(s.trainSaveEvery)
	r.F0, r.CPUCores, r.IndexAlgorithm = s.trainF0, int(s.trainCores), s.trainIndex
	r.CutMode, r.ChunkLen, r.OverlapLen, r.Normalization = s.trainCut, s.trainChunk, s.trainOverlap, s.trainNorm
	r.CacheGPU = timbre.CacheGPUModes[max(0, slices.Index(trainCacheTitles, s.trainCacheGPU))]
	r.Precision, r.Checkpointing = s.trainPrecision, s.trainCheckpoint
	r.SkipPreprocess, r.SkipExtract = s.trainSkipPre, s.trainSkipExtract
	return r
}

// pickDir 在后台打开目录选择框，选定后在 UI 线程调用 fn。
func (a *app) pickDir(title string, fn func(dir string)) {
	win := a.win
	go func() {
		dirs, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: win, Title: title, Directory: true})
		if err != nil || len(dirs) == 0 {
			return
		}
		a.post(func() { fn(dirs[0]) })
	}()
}

// pickFile 在后台打开文件选择框，选定后在 UI 线程调用 fn。
func (a *app) pickFile(title string, filter mygo.FileFilter, fn func(path string)) {
	win := a.win
	go func() {
		files, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: win, Title: title, Filters: []mygo.FileFilter{filter}})
		if err != nil || len(files) == 0 {
			return
		}
		a.post(func() { fn(files[0]) })
	}()
}

func (a *app) timbreModels(c *ui.Context, tk *fluent.Tokens) {
	cardRow(c, cardMinWidth, func() ui.Element { return a.timbreModelList(c, tk) },
		func() ui.Element { return a.timbreImportCard(c, tk) })
}

// timbreModelList 列出已有的 RVC 模型，可删除。
func (a *app) timbreModelList(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.timbre
	return card(c, tk, fmt.Sprintf("模型（%d）", len(s.models)), func() {
		if ui.Button(c, "刷新").AlignSelf(ui.Start).Clicked() {
			s.modelsStale = true
		}
		for _, m := range s.models {
			ui.Row(c.Key("rvc-" + m.Name)).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, m.Name).FontWeight(600).Width(180).SingleLine()
				kind := "导入"
				if m.Trained {
					kind = fmt.Sprintf("训练 · %d 轮", m.Epoch)
				}
				idx := "有索引"
				if m.Index == "" {
					idx = "无索引"
				}
				ui.Text(c, kind+" · "+idx+" · "+filepath.Base(m.Weights)).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
				if ui.Button(c, "打开目录").Clicked() {
					dir := m.Dir
					go func() { _ = mygo.Shell.OpenPath(dir) }()
				}
				if m.Trained {
					return
				}
				// 删除不可撤销：第一次点击进入确认状态。
				if s.confirmDelete != m.Name {
					if ui.Button(c, "删除").Clicked() {
						s.confirmDelete = m.Name
					}
					return
				}
				if ui.Button(c, "取消").Clicked() {
					s.confirmDelete = ""
				}
				if ui.Button(c, "确认删除").TextColor(t.Danger).Clicked() {
					s.err = errText(timbre.DeleteImported(a.be.layout, m))
					s.confirmDelete, s.modelsStale = "", true
				}
			})
		}
	})
}

// timbreImportCard 导入外部训练的 RVC v2 模型（.pth 与可选的 .index）。
func (a *app) timbreImportCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.timbre
	return card(c, tk, "导入模型", func() {
		ui.Text(c, "导入 RVC v2 模型（.pth，可附带 .index），例如 VoiceLab 或 RVC WebUI 训练的模型。").FontSize(12).TextColor(t.TextMuted)
		ui.Field(c, "模型名", func() { ui.TextInput(c, &s.importName).Width(240) })
		filePicker := func(label string, path *string, ext string) {
			ui.Row(c.Key("imp-" + label)).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).FontSize(12).Width(64)
				name := "未选择"
				if *path != "" {
					name = filepath.Base(*path)
				}
				ui.Text(c, name).FontSize(12).Font("monospace").TextColor(t.TextMuted).Grow(1).SingleLine()
				if ui.Button(c, "选择文件…").Clicked() {
					a.pickFile("选择"+label, mygo.FileFilter{Name: label, Extensions: []string{ext}}, func(p string) {
						*path = p
						if s.importName == "" && ext == "pth" {
							base := filepath.Base(p)
							s.importName = base[:len(base)-len(filepath.Ext(base))]
						}
					})
				}
			})
		}
		filePicker("权重", &s.importWeights, "pth")
		filePicker("索引", &s.importIndex, "index")
		if ui.PrimaryButton(c, "导入").AlignSelf(ui.Start).Clicked() {
			m, err := timbre.Import(a.be.layout, s.importName, s.importWeights, s.importIndex)
			s.err = errText(err)
			if err == nil {
				s.importName, s.importWeights, s.importIndex, s.modelsStale, s.modelName = "", "", "", true, m.Name
			}
		}
	})
}
