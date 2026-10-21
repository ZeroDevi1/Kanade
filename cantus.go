package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/cantus"
	"kanade/internal/conductor"
	"kanade/internal/dataset"
	"kanade/internal/ui/fluent"
)

// voiceFromFile 是音色来源下拉框中「使用参考音频」一项。
const voiceFromFile = "使用参考音频文件"

var sourceTitles = []string{"ModelScope（国内）", "Hugging Face"}

// cantusState 是语音生成页的界面状态。
type cantusState struct {
	modelTitle string // 选中的模型，取 cantus.Model.Title；空为第一个
	modeIdx    int    // 当前模型可用模式中的序号
	source     string // 下载源，取 sourceTitles 之一

	text, promptText, instruct string
	promptWav, sourceWav       string
	voice                      string // 音色来源：voiceFromFile 或已保存音色的名称
	spkID                      string
	preset                     string // 指令预设，选中后填入 instruct
	speed                      float64
	textFrontend               bool

	// speakers 是已保存音色的缓存；speakersStale 为 true 时下次构建重新读取。
	speakers      []cantus.Speaker
	speakersStale bool
	newSpeaker    string
	// confirmDelete 是等待二次确认删除的音色 ID。
	confirmDelete string

	// models 是内置模型与训练出的 SFT 模型，speakersStale 置位时一并刷新。
	models []cantus.Model

	// synthMode 为 modeSmart 时推理模式按输入自动选择（cantus.SmartMode），只填音色、文本与可选风格。
	synthMode int
	style     string // 智能模式的风格，styleNone 或 cantus.InstructPresets 之一
	// 专业模式的随机种子与 LLM 采样参数。
	seedFixed   bool
	seed        float64
	samplingOn  bool
	sampling    cantus.Sampling
	samplingTop float64 // ui.NumberInput 只编辑 float64，提交时转为整数
	samplingWin float64

	// SFT 训练。sftMode 为 modeSmart 时一键完成准备与训练（缺文本用 cantus.SFTSmartASR 转写，层数按显存）。
	sftMode                      int
	sftName, sftDataset, sftASR  string
	sftPart, sftScope            string
	sftEpochs, sftLR, sftWorkers float64
	sftDevRatio                  float64
	sftSummary                   *dataset.Summary
	sftPrepareJob, sftTrainJob   string
	sftChain                     string // 智能模式：完成后自动开始训练的准备任务
	sftScanned                   string // 已为其刷新过模型列表的训练任务

	job       string            // 最近提交的合成任务
	jobName   string            // 最近提交任务结果的建议文件名
	downloads map[string]string // 模型 Key → 下载任务 ID
	results   resultSet
	err       string
}

// styleNone 是智能模式风格下拉框中的「不指定」。
const styleNone = "自然（不指定风格）"

func newCantusState() cantusState {
	return cantusState{speed: 1, textFrontend: true, voice: voiceFromFile, spkID: cantus.SFTSpeakers[0],
		source: sourceTitles[0], speakersStale: true, downloads: map[string]string{}, models: cantus.Models,
		sftASR: cantus.ASRModels[0], sftPart: cantus.SFTParts[0].Title, sftEpochs: 30, sftLR: 1e-5, sftWorkers: 4,
		sftDevRatio: 0.05, sftScope: cantus.SFTLLMScopes[0].Title, style: styleNone,
		sampling: cantus.DefaultSampling, samplingTop: float64(cantus.DefaultSampling.TopK),
		samplingWin: float64(cantus.DefaultSampling.WinSize), seed: 1986}
}

func (s *cantusState) model() cantus.Model {
	for _, m := range s.models {
		if m.Title == s.modelTitle {
			return m
		}
	}
	return cantus.Models[0]
}

// mode 返回当前推理模式：专业模式为用户所选，智能模式按音色、参考文本与风格自动选择。
func (s *cantusState) mode() cantus.Mode {
	m := s.model()
	if s.synthMode == modeSmart {
		hasText := strings.TrimSpace(s.promptText) != ""
		if s.voice != voiceFromFile {
			hasText = s.speaker() != nil && s.speaker().PromptText != ""
		}
		return cantus.SmartMode(m, hasText, s.style != styleNone)
	}
	modes := m.Modes()
	return modes[min(s.modeIdx, len(modes)-1)]
}

func (s *cantusState) speaker() *cantus.Speaker {
	for i := range s.speakers {
		if s.speakers[i].Name == s.voice {
			return &s.speakers[i]
		}
	}
	return nil
}

// cantusPage 是 CosyVoice 语音生成页：模型管理、各推理模式的输入、结果预览与音色库。
func (a *app) cantusPage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "语音生成", "Cantus · CosyVoice：零样本克隆、跨语种复刻、预训练音色、自然语言控制与声音转换。")
	if a.backendUnavailable(c, tk) {
		return
	}
	s := &a.cantus
	if s.speakersStale {
		s.speakersStale = false
		list, err := cantus.ListSpeakers(a.be.layout)
		s.speakers, s.models = list, cantus.AllModels(a.be.layout)
		if err != nil {
			s.err = "读取音色库失败：" + err.Error()
		}
	}
	a.cantusModelCard(c, tk)
	a.cantusInputCard(c, tk)
	card(c, tk, "结果", func() { a.cantusResults(c, tk) })
	a.cantusSpeakersCard(c, tk)
	a.cantusSFTCard(c, tk)
}

// cantusModelCard 选择模型并管理下载。
func (a *app) cantusModelCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.cantus
	l := a.be.layout
	card(c, tk, "模型", func() {
		titles := make([]string, len(s.models))
		for i, m := range s.models {
			titles[i] = m.Title
		}
		m := s.model()
		if s.modelTitle == "" {
			s.modelTitle = m.Title
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Select(c, &s.modelTitle, titles).Label("模型").Width(300).Changed() {
				s.modeIdx, s.err = 0, ""
			}
			m = s.model()
			dl := a.downloadJob(m)
			switch {
			case m.Installed(l):
				ui.Text(c, "已下载").FontSize(12).TextColor(t.Success)
			case dl != nil && !dl.State.Finished():
				ui.Textf(c, "下载中 %.0f%%", dl.Progress*100).FontSize(12).TextColor(t.TextMuted)
				if ui.Button(c, "取消").Clicked() {
					a.be.cond.Cancel(dl.ID)
				}
			default:
				ui.Textf(c, "未下载 · 约 %.1f GB", m.SizeGB).FontSize(12).TextColor(t.TextMuted)
				ui.Select(c, &s.source, sourceTitles).Label("下载源").Width(180)
				if ui.PrimaryButton(c, "下载").Clicked() {
					a.downloadModel(m)
				}
			}
		})
		if dl := a.downloadJob(m); dl != nil && !m.Installed(l) {
			if !dl.State.Finished() {
				ui.Progress(c, dl.Progress)
				ui.Text(c, dl.Message).FontSize(12).TextColor(t.TextMuted)
			} else if dl.Err != "" {
				ui.Text(c, "下载失败："+dl.Err).FontSize(12).TextColor(t.Danger).Selectable()
			}
		}
		ui.Text(c, "模型保存在 "+cantus.Root(l)).FontSize(12).TextColor(t.TextMuted).Selectable()
	})
}

// downloadJob 返回模型最近一次下载任务的快照；没有时为 nil。
func (a *app) downloadJob(m cantus.Model) *conductor.Job {
	id := a.cantus.downloads[m.Key]
	if id == "" {
		return nil
	}
	j, ok := a.be.cond.Job(id)
	if !ok {
		return nil
	}
	return &j
}

func (a *app) downloadModel(m cantus.Model) {
	s := &a.cantus
	src := cantus.SourceModelScope
	if s.source == sourceTitles[1] {
		src = cantus.SourceHuggingFace
	}
	id, err := a.be.cond.Submit(cantus.Engine, cantus.Engine+".download", cantus.DownloadParams(a.be.layout, m, src))
	if err != nil {
		s.err = err.Error()
		return
	}
	s.downloads[m.Key] = id
}

// cantusInputCard 显示当前模型支持的模式与对应输入，并提交合成任务。
func (a *app) cantusInputCard(c *ui.Context, tk *fluent.Tokens) {
	s := &a.cantus
	m := s.model()
	card(c, tk, "合成", func() {
		modeSwitch(c, "synth-mode", &s.synthMode, "只需选音色、填文本；按是否有参考文本与风格自动选择克隆方式。")
		if s.synthMode == modeSmart {
			a.cantusSmartInput(c, tk, m)
		} else {
			a.cantusProInput(c, tk, m)
		}
		if s.mode() != cantus.ModeVC {
			ui.TextArea(c, &s.text).Label("要合成的文本").Placeholder("输入文本，长文本会按句切分后依次合成").Height(120)
		}
		if s.synthMode == modePro {
			a.cantusProTuning(c)
		}
		a.cantusSubmitRow(c)
	})
}

// cantusSmartInput 是智能模式的输入：音色（或预训练音色）与可选风格。
func (a *app) cantusSmartInput(c *ui.Context, tk *fluent.Tokens, m cantus.Model) {
	t := c.Theme()
	s := &a.cantus
	if m.Supports(cantus.ModeSFT) || m.Family == cantus.FamilyV1Instruct {
		spks := m.SpeakerList()
		if !slices.Contains(spks, s.spkID) {
			s.spkID = spks[0]
		}
		if len(spks) > 1 {
			ui.Select(c, &s.spkID, spks).Label("预训练音色").Width(200)
		}
	} else {
		a.cantusVoice(c, tk, cantus.ModeZeroShot)
	}
	if m.Supports(cantus.ModeInstruct) {
		styles := append([]string{styleNone}, cantus.InstructPresets...)
		if m.Family == cantus.FamilyV1Instruct {
			styles = styles[1:] // CosyVoice 1 指令模型必须给出指令
			if s.style == styleNone {
				s.style = styles[0]
			}
		}
		ui.Select(c, &s.style, styles).Label("风格").Width(300)
	}
	ui.Text(c, "当前将使用："+cantus.ModeTitle[s.mode()]+"。"+smartHint(s.mode())).FontSize(12).TextColor(t.TextMuted)
}

// smartHint 说明智能模式为何选了该模式，以及怎样切换到效果更好的模式。
func smartHint(mode cantus.Mode) string {
	switch mode {
	case cantus.ModeCrossLingual:
		return "填写参考音频说的话后会改用零样本克隆，同语种时音色更像。"
	case cantus.ModeZeroShot:
		return "参考音频与目标文本语种不同时，清空参考文本可改用跨语种复刻。"
	case cantus.ModeInstruct:
		return "按所选风格朗读。"
	}
	return ""
}

// cantusProInput 是专业模式的输入：手动选择推理模式与对应的全部输入。
func (a *app) cantusProInput(c *ui.Context, tk *fluent.Tokens, m cantus.Model) {
	t := c.Theme()
	s := &a.cantus
	modes := m.Modes()
	labels := make([]string, len(modes))
	for i, md := range modes {
		labels[i] = cantus.ModeTitle[md]
	}
	s.modeIdx = min(s.modeIdx, len(modes)-1)
	if ui.Segmented(c, &s.modeIdx, labels...).Label("推理模式").Changed() {
		s.err = ""
	}
	mode := s.mode()
	ui.Text(c, modeHint(m, mode)).FontSize(12).TextColor(t.TextMuted)

	usesSpk := mode == cantus.ModeSFT || (mode == cantus.ModeInstruct && m.Family == cantus.FamilyV1Instruct)
	if usesSpk {
		spks := m.SpeakerList()
		if !slices.Contains(spks, s.spkID) {
			s.spkID = spks[0]
		}
		ui.Select(c, &s.spkID, spks).Label("预训练音色").Width(200)
		if m.Family == cantus.FamilyV3SFT {
			ui.TextInput(c, &s.instruct).Label("风格引导（可选）").Placeholder("例如：请非常开心地说一句话。留空则只用音色本身")
		}
	} else {
		a.cantusVoice(c, tk, mode)
	}
	if mode == cantus.ModeVC {
		a.audioPicker(c, "源音频", &s.sourceWav, directAudioExts)
	}
	if mode == cantus.ModeInstruct {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &s.instruct).Label("指令").Placeholder("例如：请用四川话表达。").Grow(1)
			presets := append([]string{"常用指令…"}, cantus.InstructPresets...)
			if s.preset == "" {
				s.preset = presets[0]
			}
			if ui.Select(c, &s.preset, presets).Label("常用指令").Width(220).Changed() && s.preset != presets[0] {
				s.instruct = s.preset
			}
		})
	}
}

// cantusProTuning 是专业模式的语速、文本归一化、随机种子与 LLM 采样参数。
func (a *app) cantusProTuning(c *ui.Context) {
	t := c.Theme()
	s := &a.cantus
	mode := s.mode()
	ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "语速").FontSize(12)
		ui.Slider(c, &s.speed, cantus.MinSpeed, cantus.MaxSpeed).Label("语速").Width(200)
		ui.Textf(c, "%.2f×", s.speed).FontSize(12).Font("monospace").Width(48)
		if mode != cantus.ModeVC {
			ui.Checkbox(c, &s.textFrontend, "文本归一化（数字、符号读法）")
		}
	})
	if mode == cantus.ModeVC {
		return
	}
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Checkbox(c, &s.seedFixed, "固定随机种子")
		if s.seedFixed {
			ui.Field(c, "种子", func() { ui.NumberInput(c, &s.seed, 0, 2147483647, 1).Width(140) })
		}
		ui.Checkbox(c, &s.samplingOn, "自定义采样参数")
	})
	ui.Text(c, "不固定种子时，微调模型默认用种子 1986，其他模型每次随机；固定后同样的输入得到同样的结果，便于对比参数。").
		FontSize(12).TextColor(t.TextMuted)
	if !s.samplingOn {
		return
	}
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "top_p", func() { ui.NumberInput(c, &s.sampling.TopP, 0.05, 1, 0.05).Width(100) })
		ui.Field(c, "top_k", func() { ui.NumberInput(c, &s.samplingTop, 1, 200, 1).Width(100) })
		ui.Field(c, "重复窗口", func() { ui.NumberInput(c, &s.samplingWin, 1, 100, 1).Width(100) })
		ui.Field(c, "重复阈值 tau_r", func() { ui.NumberInput(c, &s.sampling.TauR, 0, 1, 0.05).Width(110) })
		if ui.Button(c, "模型默认").Clicked() {
			s.setSampling(cantus.DefaultSampling)
		}
		if ui.Button(c, "微调保守").Clicked() {
			s.setSampling(cantus.SFTSampling)
		}
	})
	ui.Text(c, "top_p / top_k 越小越稳定、越单调；重复窗口内同一 token 占比超过 tau_r 时改为随机采样，防止卡在重复上。").
		FontSize(12).TextColor(t.TextMuted)
}

func (s *cantusState) setSampling(v cantus.Sampling) {
	s.sampling, s.samplingTop, s.samplingWin = v, float64(v.TopK), float64(v.WinSize)
}

// modeHint 是各模式的一句用法说明，内容取自上游 WebUI 与 example.py。
func modeHint(m cantus.Model, mode cantus.Mode) string {
	switch mode {
	case cantus.ModeZeroShot:
		return "上传 3–30 秒参考音频并填写其文本，用该音色朗读目标文本（同语种效果最好）。"
	case cantus.ModeCrossLingual:
		if m.Family == cantus.FamilyV1 {
			return "参考音频与目标文本语种不同时使用，无需参考文本；可在文本前加 <|zh|><|en|><|ja|><|yue|><|ko|> 指定语种。"
		}
		return "无需参考文本；文本中可插入 [laughter]、[breath] 等细粒度控制标记。日语需写成片假名。"
	case cantus.ModeSFT:
		return "使用模型内置的预训练音色朗读文本。"
	case cantus.ModeInstruct:
		if m.Family == cantus.FamilyV1Instruct {
			return "选择预训练音色并用自然语言描述说话人风格；文本中可用 <strong></strong>、<laughter></laughter> 标记。"
		}
		return "用参考音色朗读，并按指令控制方言、情绪、语速等。"
	case cantus.ModeVC:
		return "把源音频的说话内容转换成参考音色（CosyVoice 1 基础模型）。"
	}
	return ""
}

// cantusVoice 选择参考音色：已保存音色，或参考音频文件（可另存为音色）。
func (a *app) cantusVoice(c *ui.Context, tk *fluent.Tokens, mode cantus.Mode) {
	s := &a.cantus
	options := []string{voiceFromFile}
	for _, sp := range s.speakers {
		options = append(options, sp.Name)
	}
	if s.voice != voiceFromFile && s.speaker() == nil {
		s.voice = voiceFromFile
	}
	ui.Select(c, &s.voice, options).Label("音色来源").Width(300)
	if s.voice != voiceFromFile {
		if sp := s.speaker(); sp != nil && sp.PromptText != "" {
			ui.Text(c, "参考文本："+sp.PromptText).FontSize(12).TextColor(c.Theme().TextMuted)
		}
		return
	}
	a.audioPicker(c, "参考音频", &s.promptWav, directAudioExts)
	if s.synthMode == modeSmart {
		ui.TextInput(c, &s.promptText).Label("参考音频的文本").Placeholder("可选：参考音频里说的话，填写后同语种克隆更像")
	} else if mode == cantus.ModeZeroShot || mode == cantus.ModeInstruct {
		ui.TextInput(c, &s.promptText).Label("参考音频的文本").Placeholder("参考音频里说的话，零样本克隆必填")
	}
	if s.promptWav != "" {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c, &s.newSpeaker).Label("音色名称").Placeholder("保存为音色，方便复用").Width(240)
			if ui.Button(c, "保存为音色").Clicked() {
				sp, err := cantus.SaveSpeaker(a.be.layout, s.newSpeaker, s.promptText, s.promptWav)
				if err != nil {
					s.err = err.Error()
					return
				}
				s.err, s.newSpeaker, s.speakersStale, s.voice = "", "", true, sp.Name
			}
		})
	}
}

// directAudioExts 是 CosyVoice 与 RVC 推理直接读取（soundfile）时支持的格式，与 tasks.check_audio 的提示一致。
var directAudioExts = []string{"wav", "flac", "mp3", "ogg"}

// anyAudioExts 返回经分轨引擎 ffmpeg 预转换后可用的全部格式（不带点、已排序），与 dataset.AudioExts 一致。
func anyAudioExts() []string {
	exts := make([]string, 0, len(dataset.AudioExts))
	for e := range dataset.AudioExts {
		exts = append(exts, e[1:])
	}
	slices.Sort(exts)
	return exts
}

// audioPicker 显示已选文件并提供选择按钮；对话框在后台 goroutine 中打开，选择结果经 post 写回 *path。
// exts 是对话框筛选的扩展名（directAudioExts 或 anyAudioExts()）。
func (a *app) audioPicker(c *ui.Context, label string, path *string, exts []string) {
	t := c.Theme()
	ui.Row(c.Key("pick-" + label)).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).Width(64)
		name := "未选择"
		if *path != "" {
			name = filepath.Base(*path)
		}
		ui.Text(c, name).FontSize(12).Font("monospace").TextColor(t.TextMuted).Grow(1).SingleLine().Tooltip(*path)
		if ui.Button(c, "选择文件…").Clicked() {
			win := a.win
			go func() {
				files, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
					Parent: win, Title: "选择" + label,
					Filters: []mygo.FileFilter{{Name: "音频", Extensions: exts}},
				})
				if err != nil || len(files) == 0 {
					return
				}
				a.post(func() { *path = files[0] })
			}()
		}
	})
}

// cantusSubmitRow 提交合成任务并显示其进度；任务完成后把输出加入结果列表并载入预览。
func (a *app) cantusSubmitRow(c *ui.Context) {
	t := c.Theme()
	s := &a.cantus
	var job conductor.Job
	active := false
	if s.job != "" {
		job, _ = a.be.cond.Job(s.job)
		active = !job.State.Finished()
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.PrimaryButton(c, "生成").Disabled(active).Clicked() {
			a.submitCantus()
		}
		if active {
			if ui.Button(c, "取消").Clicked() {
				a.be.cond.Cancel(s.job)
			}
			ui.Spinner(c)
			ui.Text(c, job.Message).FontSize(12).TextColor(t.TextMuted)
		}
	})
	if active {
		ui.Progress(c, job.Progress)
	}
	if job.State == conductor.JobFailed {
		ui.Text(c, "生成失败："+job.Err).FontSize(12).TextColor(t.Danger).Selectable()
	}
	if s.err != "" {
		ui.Text(c, s.err).FontSize(12).TextColor(t.Danger)
	}
	// 任务完成后登记结果（resultSet 按路径去重，只登记一次）。
	if job.State == conductor.JobDone && len(job.Artifacts) > 0 {
		s.results.add(a, job.Artifacts[0], s.jobName, s.jobName, true)
	}
}

func (a *app) submitCantus() {
	s := &a.cantus
	r := cantus.Request{
		Model: s.model(), Mode: s.mode(), Text: s.text, Speaker: nil,
		PromptWav: s.promptWav, PromptText: s.promptText, SpkID: s.spkID, Instruct: s.instruct,
		SourceWav: s.sourceWav, Speed: s.speed, TextFrontend: s.textFrontend,
	}
	if s.voice != voiceFromFile {
		r.Speaker = s.speaker()
	}
	if s.synthMode == modeSmart {
		// 智能模式：语速 1×、文本归一化开、采样与种子用引擎默认；风格作为指令。
		r.Speed, r.TextFrontend, r.Instruct = 1, true, ""
		if r.Mode == cantus.ModeInstruct {
			r.Instruct = s.style
		}
	} else {
		if s.seedFixed {
			seed := int(s.seed)
			r.Seed = &seed
		}
		if s.samplingOn {
			sp := s.sampling
			sp.TopK, sp.WinSize = int(s.samplingTop), int(s.samplingWin)
			r.Sampling = &sp
		}
	}
	params, err := r.Params(a.be.layout)
	if err != nil {
		s.err = err.Error()
		return
	}
	id, err := a.be.cond.Submit(cantus.Engine, r.Task(), params)
	if err != nil {
		s.err = err.Error()
		return
	}
	s.job, s.err = id, ""
	s.jobName = cantusResultName(r)
}

// cantusResultName 是合成结果的建议文件名：<音色>_<文本开头>。
func cantusResultName(r cantus.Request) string {
	voice := r.SpkID
	switch {
	case r.Speaker != nil:
		voice = r.Speaker.Name
	case r.Mode != cantus.ModeSFT && r.PromptWav != "":
		voice = strings.TrimSuffix(filepath.Base(r.PromptWav), filepath.Ext(r.PromptWav))
	}
	text := []rune(strings.Join(strings.Fields(r.Text), ""))
	if r.Mode == cantus.ModeVC {
		text = []rune(strings.TrimSuffix(filepath.Base(r.SourceWav), filepath.Ext(r.SourceWav)))
	}
	if len(text) > 16 {
		text = text[:16]
	}
	return voice + "_" + string(text)
}

// cantusResults 显示当前预览与本次运行生成的文件列表。
func (a *app) cantusResults(c *ui.Context, tk *fluent.Tokens) {
	a.cantus.results.show(c, tk, a, "生成的音频会显示在这里。")
}

// cantusSpeakersCard 列出已保存音色，可删除。
func (a *app) cantusSpeakersCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.cantus
	card(c, tk, fmt.Sprintf("音色库（%d）", len(s.speakers)), func() {
		if len(s.speakers) == 0 {
			ui.Text(c, "选择参考音频后可「保存为音色」，之后在任意模型中直接选用。").TextColor(t.TextMuted)
			return
		}
		for _, sp := range s.speakers {
			ui.Row(c.Key("spk-" + sp.ID)).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, sp.Name).FontWeight(600).Width(160).SingleLine()
				text := sp.PromptText
				if text == "" {
					text = "（无参考文本，仅可用于跨语种、指令与声音转换）"
				}
				ui.Text(c, text).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
				// 删除不可撤销：第一次点击进入确认状态，再次点击才删除。
				if s.confirmDelete != sp.ID {
					if ui.Button(c, "删除").Clicked() {
						s.confirmDelete = sp.ID
					}
					return
				}
				if ui.Button(c, "取消").Clicked() {
					s.confirmDelete = ""
				}
				if ui.Button(c, "确认删除").TextColor(t.Danger).Clicked() {
					if err := cantus.DeleteSpeaker(a.be.layout, sp.ID); err != nil {
						s.err = err.Error()
					}
					s.confirmDelete, s.speakersStale = "", true
				}
			})
		}
	})
}

// cantusSFTCard 是 CosyVoice 3 说话人微调：准备数据（文本取自 .list，缺失时可用 Whisper 转写）→ 训练 → 组装模型。
// 训练需要 NVIDIA CUDA；微调出的模型以「<音色名>（微调音色）」出现在模型列表，使用预训练音色模式。
// 智能模式一键完成两步：准备任务成功后自动提交训练。
func (a *app) cantusSFTCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.cantus
	card(c, tk, "说话人微调（SFT）", func() {
		ui.Text(c, "用几百条带文本的干净人声，把一个说话人微调进 Fun-CosyVoice 3。数据集目录中的同名 .list 提供文本；"+
			"训练需要 NVIDIA CUDA（数据准备可在本机完成）。").FontSize(12).TextColor(t.TextMuted)
		modeSwitch(c, "sft-mode", &s.sftMode, "缺文本时用 Whisper medium 转写，只训 LLM，训练层数按显存自动选择。")
		ui.Field(c, "音色名", func() { ui.TextInput(c, &s.sftName).Placeholder("例如 xuan").Width(240) })
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "数据集").FontSize(12).Width(64)
			name := "未选择"
			if s.sftDataset != "" {
				name = s.sftDataset
			}
			ui.Text(c, name).FontSize(12).Font("monospace").TextColor(t.TextMuted).Grow(1).SingleLine()
			if ui.Button(c, "选择目录…").Clicked() {
				a.pickDir("选择数据集目录", func(dir string) {
					s.sftDataset = dir
					sum, err := dataset.Inspect(dir, "")
					s.sftSummary, s.err = &sum, errText(err)
					if s.sftName == "" {
						s.sftName = filepath.Base(dir)
					}
				})
			}
		})
		if sum := s.sftSummary; sum != nil {
			text := fmt.Sprintf("%d 个音频文件", sum.Audio)
			if sum.List != "" {
				text += fmt.Sprintf(" · %s：%d 条，%d 条找到音频，%d 条有文本", filepath.Base(sum.List), sum.Listed, sum.Matched, sum.WithText)
			} else {
				text += " · 未找到 .list，需要用 Whisper 转写文本"
			}
			ui.Text(c, text).FontSize(12).TextColor(t.TextMuted)
		}
		_, preparing := a.jobStatusPeek(s.sftPrepareJob)
		_, training := a.jobStatusPeek(s.sftTrainJob)
		if s.sftMode == modeSmart {
			if ui.PrimaryButton(c, "开始微调").Disabled(preparing || training || s.sftChain != "").Clicked() {
				a.submitSFTPrepare(true)
			}
		} else {
			a.cantusSFTPro(c, preparing, training)
		}
		pj, _ := a.jobStatus(c, s.sftPrepareJob)
		if pj.State == conductor.JobDone {
			ui.Text(c, pj.Message).FontSize(12).TextColor(t.Success)
		}
		// 智能模式的第二步：准备成功后自动提交训练；准备失败或取消时结束链条。
		if s.sftChain != "" && s.sftChain == s.sftPrepareJob && pj.State.Finished() {
			s.sftChain = ""
			if pj.State == conductor.JobDone {
				a.submitSFTTrain(false)
			}
		}
		if j, _ := a.jobStatus(c, s.sftTrainJob); j.State == conductor.JobDone && s.sftScanned != s.sftTrainJob {
			s.sftScanned, s.speakersStale = s.sftTrainJob, true
			ui.Text(c, "微调完成，模型已出现在模型列表中。").FontSize(12).TextColor(t.Success)
		}
	})
}

// cantusSFTPro 是专业模式的两步：准备数据（转写模型、验证集比例）与训练（部分、层数、轮数、学习率、进程数）。
func (a *app) cantusSFTPro(c *ui.Context, preparing, training bool) {
	s := &a.cantus
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "缺文本时转写", func() { ui.Select(c, &s.sftASR, cantus.ASRModels).Width(140) })
		ui.Field(c, "验证集比例", func() { ui.NumberInput(c, &s.sftDevRatio, 0.01, 0.5, 0.01).Width(100) })
		if ui.Button(c, "1. 准备数据").Disabled(preparing).Clicked() {
			a.submitSFTPrepare(false)
		}
	})
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "训练部分", func() {
			ui.Select(c, &s.sftPart, titles(cantus.SFTParts, func(p struct{ Key, Title string }) string { return p.Title })).Width(180)
		})
		ui.Field(c, "LLM 训练范围", func() {
			ui.Select(c, &s.sftScope, titles(cantus.SFTLLMScopes, func(sc struct {
				Layers int
				Title  string
			}) string {
				return sc.Title
			})).Width(220)
		})
	})
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "轮数", func() { ui.NumberInput(c, &s.sftEpochs, 1, 1000, 1).Width(100) })
		ui.Field(c, "学习率", func() { ui.NumberInput(c, &s.sftLR, 1e-7, 1e-3, 1e-6).Width(120) })
		ui.Field(c, "数据加载进程", func() { ui.NumberInput(c, &s.sftWorkers, 1, 32, 1).Width(100) })
		if ui.PrimaryButton(c, "2. 开始训练").Disabled(training).Clicked() {
			a.submitSFTTrain(true)
		}
	})
}

// submitSFTPrepare 提交数据准备；chain 为 true（智能模式）时准备成功后自动开始训练。
func (a *app) submitSFTPrepare(chain bool) {
	s := &a.cantus
	asr, dev := s.sftASR, s.sftDevRatio
	if chain {
		asr, dev = cantus.SFTSmartASR, 0
	}
	params, err := cantus.SFTPrepareParams(a.be.layout, s.sftName, s.sftDataset, asr, dev)
	if err == nil {
		s.sftPrepareJob, err = a.be.cond.Submit(cantus.Engine, "cosyvoice.sft_prepare", params)
	}
	s.err = errText(err)
	if err == nil && chain {
		s.sftChain = s.sftPrepareJob
	}
}

// submitSFTTrain 提交训练。pro 为 false 时使用智能模式的参数：只训 LLM、按显存选层数、30 轮。
func (a *app) submitSFTTrain(pro bool) {
	s := &a.cantus
	t := cantus.SFTTrain{Name: s.sftName, Part: "llm", Epochs: 30, LLMLayers: -1}
	if pro {
		for _, p := range cantus.SFTParts {
			if p.Title == s.sftPart {
				t.Part = p.Key
			}
		}
		for _, sc := range cantus.SFTLLMScopes {
			if sc.Title == s.sftScope {
				t.LLMLayers = sc.Layers
			}
		}
		t.Epochs, t.LR, t.Workers = int(s.sftEpochs), s.sftLR, int(s.sftWorkers)
	}
	params, err := cantus.SFTTrainParams(a.be.layout, t)
	if err == nil {
		s.sftTrainJob, err = a.be.cond.Submit(cantus.Engine, "cosyvoice.sft_train", params)
	}
	s.err = errText(err)
}
