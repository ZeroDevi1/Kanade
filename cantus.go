package main

import (
	"fmt"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/cantus"
	"kanade/internal/conductor"
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

	job       string            // 最近提交的合成任务
	downloads map[string]string // 模型 Key → 下载任务 ID
	results   []string          // 本次运行生成的文件，最新在前
	preview   audioPreview
	err       string
}

func newCantusState() cantusState {
	return cantusState{speed: 1, textFrontend: true, voice: voiceFromFile, spkID: cantus.SFTSpeakers[0],
		source: sourceTitles[0], speakersStale: true, downloads: map[string]string{}}
}

func (s *cantusState) model() cantus.Model {
	for _, m := range cantus.Models {
		if m.Title == s.modelTitle {
			return m
		}
	}
	return cantus.Models[0]
}

func (s *cantusState) mode() cantus.Mode {
	modes := s.model().Modes()
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
		s.speakers = list
		if err != nil {
			s.err = "读取音色库失败：" + err.Error()
		}
	}
	a.cantusModelCard(c, tk)
	a.cantusInputCard(c, tk)
	card(c, tk, "结果", func() { a.cantusResults(c, tk) })
	a.cantusSpeakersCard(c, tk)
}

// cantusModelCard 选择模型并管理下载。
func (a *app) cantusModelCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.cantus
	l := a.be.layout
	card(c, tk, "模型", func() {
		titles := make([]string, len(cantus.Models))
		for i, m := range cantus.Models {
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
	t := c.Theme()
	s := &a.cantus
	m := s.model()
	card(c, tk, "合成", func() {
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
			ui.Select(c, &s.spkID, cantus.SFTSpeakers).Label("预训练音色").Width(200)
		} else {
			a.cantusVoice(c, tk, mode)
		}
		if mode == cantus.ModeVC {
			a.audioPicker(c, "源音频", &s.sourceWav)
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
		if mode != cantus.ModeVC {
			ui.TextArea(c, &s.text).Label("要合成的文本").Placeholder("输入文本，长文本会按句切分后依次合成").Height(120)
		}
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "语速").FontSize(12)
			ui.Slider(c, &s.speed, cantus.MinSpeed, cantus.MaxSpeed).Label("语速").Width(200)
			ui.Textf(c, "%.2f×", s.speed).FontSize(12).Font("monospace").Width(48)
			if mode != cantus.ModeVC {
				ui.Checkbox(c, &s.textFrontend, "文本归一化（数字、符号读法）")
			}
		})
		a.cantusSubmitRow(c)
	})
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
	a.audioPicker(c, "参考音频", &s.promptWav)
	if mode == cantus.ModeZeroShot || mode == cantus.ModeInstruct {
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

// audioPicker 显示已选文件并提供选择按钮；对话框在后台 goroutine 中打开，选择结果经 post 写回 *path。
func (a *app) audioPicker(c *ui.Context, label string, path *string) {
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
					Filters: []mygo.FileFilter{{Name: "音频", Extensions: []string{"wav", "flac", "mp3", "ogg"}}},
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
	// 只在任务刚完成时登记一次结果：结果列表首项已是该输出则跳过。
	if job.State == conductor.JobDone && len(job.Artifacts) > 0 {
		out := job.Artifacts[0]
		if len(s.results) == 0 || s.results[0] != out {
			s.results = append([]string{out}, s.results...)
			s.preview.load(a, out)
		}
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
}

// cantusResults 显示当前预览与本次运行生成的文件列表，点击切换预览。
func (a *app) cantusResults(c *ui.Context, tk *fluent.Tokens) {
	s := &a.cantus
	s.preview.show(c, tk, "生成的音频会显示在这里。")
	if len(s.results) < 2 {
		return
	}
	ui.Text(c, "本次生成").FontSize(12).TextColor(c.Theme().TextMuted)
	for _, p := range s.results {
		b := ui.Button(c.Key("result-"+p), filepath.Base(p))
		if p == s.preview.path {
			b = b.Disabled(true)
		}
		if b.Clicked() {
			s.preview.load(a, p)
		}
	}
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
