package main

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/dataset"
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

// timbreState 是音色转换页的界面状态。
type timbreState struct {
	tab int

	// models 是模型列表缓存；modelsStale 为 true 时下次构建重新扫描（训练完成、导入、删除后置位）。
	models      []timbre.Model
	modelsStale bool
	modelName   string // 选中的模型名

	// 推理参数（转换与翻唱共用）。
	preset         string // timbre.Preset.Title
	pitch          float64
	f0             string
	indexRate      float64
	protect        float64
	volumeEnvelope float64
	stereo         string // timbre.StereoModes 的 Title
	normalize      bool

	input   string
	job     string
	preview audioPreview

	// 翻唱。
	song          string
	backing       bool
	backingGain   float64
	cover         *timbre.Cover
	coverPreview  audioPreview
	coverShownMix string

	// 训练。
	trainName        string
	trainDataset     string
	trainSummary     *dataset.Summary
	trainRate        string
	trainEpochs      string
	trainBatch       string
	trainSaveEvery   string
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
	s := timbreState{modelsStale: true, normalize: true, stereo: timbre.StereoModes[1].Title, backing: true, backingGain: -3,
		trainRate: "48000", trainEpochs: "200", trainBatch: "4", trainSaveEvery: "3"}
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

func (s *timbreState) inferRequest(input string) timbre.InferRequest {
	m, _ := s.model()
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
	names := make([]string, len(s.models))
	for i, m := range s.models {
		names[i] = m.Name
	}
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

// timbreParams 显示推理参数：预设、变调、音高算法，以及可微调的检索比例、辅音保护与响度包络。
func (a *app) timbreParams(c *ui.Context) {
	t := c.Theme()
	s := &a.timbre
	titles := make([]string, len(timbre.Presets))
	for i, p := range timbre.Presets {
		titles[i] = p.Title
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.Select(c, &s.preset, titles).Label("预设").Width(200).Changed() {
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
			ui.Text(c, p.Hint+"。变调：男转女常用 +12，女转男 −3 ~ −6，歌声先从 0 开始。").FontSize(12).TextColor(t.TextMuted)
		}
	}
	slider := func(label string, v *float64, lo, hi float64, format string) {
		ui.Row(c.Key("slider-" + label)).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, label).FontSize(12).Width(72)
			ui.Slider(c, v, lo, hi).Label(label).Width(240)
			ui.Textf(c, format, *v).FontSize(12).Font("monospace").Width(64)
		})
	}
	slider("变调（半音）", &s.pitch, timbre.MinPitch, timbre.MaxPitch, "%+.0f")
	s.pitch = float64(int(s.pitch + 0.5*sign(s.pitch))) // 变调按整数半音
	slider("检索比例", &s.indexRate, 0, 1, "%.2f")
	slider("辅音保护", &s.protect, 0, 0.5, "%.2f")
	slider("响度包络", &s.volumeEnvelope, 0, 1, "%.2f")
	ui.Checkbox(c, &s.normalize, "输出峰值归一化")
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
	card(c, tk, "转换", func() {
		if !a.timbreModelSelect(c) {
			return
		}
		a.audioPicker(c, "输入人声", &s.input, directAudioExts)
		ui.Text(c, "整首歌请先用「分轨」得到干净主唱，或直接用「翻唱」。").FontSize(12).TextColor(c.Theme().TextMuted)
		a.timbreParams(c)
		_, active := a.jobStatusPeek(s.job)
		if ui.PrimaryButton(c, "开始转换").Disabled(active).Clicked() {
			params, err := s.inferRequest(s.input).Params()
			if err == nil {
				s.job, err = a.be.cond.Submit(timbre.Engine, "timbre.infer", params)
			}
			s.err = errText(err)
		}
		j, _ := a.jobStatus(c, s.job)
		if j.State == conductor.JobDone && len(j.Artifacts) > 0 {
			s.preview.load(a, j.Artifacts[0])
		}
	})
	card(c, tk, "结果", func() { s.preview.show(c, tk, "转换结果会显示在这里。") })
}

// jobStatusPeek 返回任务快照与是否进行中，不构建界面。
func (a *app) jobStatusPeek(id string) (conductor.Job, bool) {
	if id == "" {
		return conductor.Job{}, false
	}
	j, _ := a.be.cond.Job(id)
	return j, !j.State.Finished()
}

func (a *app) timbreCover(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.timbre
	var st timbre.CoverState
	if s.cover != nil {
		st = s.cover.Snapshot()
	}
	card(c, tk, "一键翻唱", func() {
		ui.Text(c, "分轨与人声净化 → 用干净主唱转换音色 → 与伴奏（及和声）混音。").FontSize(12).TextColor(t.TextMuted)
		if !a.timbreModelSelect(c) {
			return
		}
		a.audioPicker(c, "歌曲", &s.song, anyAudioExts())
		a.timbreParams(c)
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Checkbox(c, &s.backing, "混入原曲和声")
			if s.backing {
				ui.Slider(c, &s.backingGain, -24, 6).Label("和声增益").Width(160)
				ui.Textf(c, "%+.0f dB", s.backingGain).FontSize(12).Font("monospace")
			}
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c, "开始翻唱").Disabled(st.Running() && s.cover != nil).Clicked() {
				cv, err := timbre.StartCover(a.be.cond, timbre.CoverRequest{Song: s.song, Infer: s.inferRequest(""),
					Backing: s.backing, BackingGainDB: s.backingGain}, func() { a.post(func() {}) })
				if err == nil {
					s.cover, s.coverShownMix = cv, ""
				}
				s.err = errText(err)
			}
			if s.cover != nil && st.Running() {
				if ui.Button(c, "取消").Clicked() {
					s.cover.Cancel()
				}
			}
		})
		if s.cover == nil {
			return
		}
		for i, title := range timbre.StageTitles[:timbre.StageDone] {
			mark, color := "○", t.TextMuted
			switch {
			case st.Err != "" && i == st.Stage:
				mark, color = "✕", t.Danger
			case i < st.Stage:
				mark, color = "✓", t.Success
			case i == st.Stage:
				mark, color = "●", t.Accent
			}
			ui.Row(c.Key("stage-" + title)).Gap(8).Children(func() {
				ui.Text(c, mark).TextColor(color).Width(16)
				ui.Text(c, title)
			})
		}
		if st.Job != "" && st.Running() {
			a.jobStatus(c, st.Job)
		}
		if st.Err != "" {
			ui.Text(c, st.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		if mix := st.Outputs["mix"]; mix != "" && mix != s.coverShownMix {
			s.coverShownMix = mix
			s.coverPreview.load(a, mix)
		}
	})
	card(c, tk, "结果", func() {
		s.coverPreview.show(c, tk, "翻唱成品会显示在这里。")
		for _, k := range []struct{ key, title string }{{"mix", "成品"}, {"converted", "转换后的人声"}, {"dry", "原曲干声"}, {"other", "伴奏"}, {"backing", "和声"}} {
			if p := st.Outputs[k.key]; p != "" {
				b := ui.Button(c.Key("cover-"+k.key), k.title)
				if p == s.coverPreview.path {
					b = b.Disabled(true)
				}
				if b.Clicked() {
					s.coverPreview.load(a, p)
				}
			}
		}
	})
}

func (a *app) timbreTrain(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.timbre
	card(c, tk, "训练 RVC 模型", func() {
		ui.Text(c, "v2 + f0 模型。数据集为一个目录的干净人声（建议先用「分轨」净化）；目录中有同名 .list 时只使用其中列出的音频。").
			FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c, &s.trainName).Label("模型名").Placeholder("例如 xingtong_v2_48k").Width(320)
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
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			rates := make([]string, len(timbre.SampleRates))
			for i, r := range timbre.SampleRates {
				rates[i] = strconv.Itoa(r)
			}
			ui.Select(c, &s.trainRate, rates).Label("采样率").Width(120)
			ui.TextInput(c, &s.trainEpochs).Label("总轮数").Width(100)
			ui.TextInput(c, &s.trainBatch).Label("batch").Width(80)
			ui.TextInput(c, &s.trainSaveEvery).Label("每 N 轮保存").Width(110)
		})
		ui.Text(c, "显存不足（如 6 GB）时把 batch 降到 2。已有同名模型时会从上次的检查点续训，总轮数需大于已训练轮数。").
			FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(16).Children(func() {
			ui.Checkbox(c, &s.trainSkipPre, "跳过预处理（续训）")
			ui.Checkbox(c, &s.trainSkipExtract, "跳过特征提取（续训）")
		})
		_, active := a.jobStatusPeek(s.trainJob)
		if ui.PrimaryButton(c, "开始训练").Disabled(active).Clicked() {
			rate, _ := strconv.Atoi(s.trainRate)
			epochs, err1 := strconv.Atoi(s.trainEpochs)
			batch, err2 := strconv.Atoi(s.trainBatch)
			every, err3 := strconv.Atoi(s.trainSaveEvery)
			if err := firstNonNil(err1, err2, err3); err != nil {
				s.err = "轮数、batch 与保存间隔须为整数"
			} else {
				params, err := timbre.TrainRequest{Name: s.trainName, Dataset: s.trainDataset, SampleRate: rate, F0: "rmvpe",
					Epochs: epochs, BatchSize: batch, SaveEvery: every, SkipPreprocess: s.trainSkipPre, SkipExtract: s.trainSkipExtract}.Params()
				if err == nil {
					s.trainJob, err = a.be.cond.Submit(timbre.Engine, "timbre.train", params)
				}
				s.err = errText(err)
			}
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

func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
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
	t := c.Theme()
	s := &a.timbre
	card(c, tk, fmt.Sprintf("模型（%d）", len(s.models)), func() {
		if ui.Button(c, "刷新").Clicked() {
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
	card(c, tk, "导入模型", func() {
		ui.Text(c, "导入 RVC v2 模型（.pth，可附带 .index），例如 VoiceLab 或 RVC WebUI 训练的模型。").FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c, &s.importName).Label("模型名").Width(240)
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
		if ui.PrimaryButton(c, "导入").Clicked() {
			m, err := timbre.Import(a.be.layout, s.importName, s.importWeights, s.importIndex)
			s.err = errText(err)
			if err == nil {
				s.importName, s.importWeights, s.importIndex, s.modelsStale, s.modelName = "", "", "", true, m.Name
			}
		}
	})
}
