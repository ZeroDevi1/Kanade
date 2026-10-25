package main

import (
	"fmt"
	"maps"
	"path/filepath"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/stems"
	"kanade/internal/ui/fluent"
)

// stemsState 是分轨页的界面状态。
type stemsState struct {
	input string
	// disabled 记录用户取消勾选的步骤；未出现的键视为勾选（默认执行全部）。
	disabled map[string]bool
	// models 是各步选中的模型文件（步骤 ID → 文件名）；未出现的步骤使用默认模型。
	models  map[string]string
	job     string
	dlJob   string
	results resultSet
	err     string
}

func (s *stemsState) selected() []string {
	var ids []string
	for _, st := range stems.Steps {
		if !s.disabled[st.ID] {
			ids = append(ids, st.ID)
		}
	}
	return ids
}

// stemsPage 是分轨页：选择音频、勾选净化步骤、运行并试听各输出。
func (a *app) stemsPage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "分轨", "Stems · 伴奏 / 干声分离、去和声、去混响、降噪。得到的干净主唱可直接用于音色转换。")
	if a.backendUnavailable(c, tk) {
		return
	}
	a.stemsModelsCard(c, tk)
	cardRow(c, cardMinWidth, func() ui.Element { return a.stemsRunCard(c, tk) },
		func() ui.Element { return card(c, tk, "结果", func() { a.stemsResults(c, tk) }) })
}

// stemsModelsCard 选择各步骤的模型并显示下载状态。默认与 VoiceLab 一致，「评估推荐」一键切换到评估最优组合。
func (a *app) stemsModelsCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.stems
	l := a.be.layout
	card(c, tk, "模型", func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "评估推荐").Tooltip("伴奏、干声、去和声换成 docs/stems-evaluation.md 的评估最优模型").Clicked() {
				s.models = maps.Clone(stems.Recommended)
			}
			if ui.Button(c, "恢复默认").Clicked() {
				s.models = nil
			}
			ui.Text(c, "评估推荐的伴奏里残留的原唱约为默认链的 1/4。").FontSize(12).TextColor(t.TextMuted)
		})
		for _, st := range stems.Steps {
			ui.Row(c.Key("model-" + st.ID)).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, st.Title).Width(72)
				opts := st.Options()
				cur := st.Pick(s.models).Title
				if len(opts) > 1 {
					if ui.Select(c, &cur, titles(opts, func(o stems.ModelOption) string { return o.Title })).Label(st.Title + "模型").Width(300).Changed() {
						for _, o := range opts {
							if o.Title == cur {
								if s.models == nil {
									s.models = map[string]string{}
								}
								s.models[st.ID] = o.File
							}
						}
					}
				} else {
					ui.Box(c).Width(300).Shrink(0) // 与有下拉框的行对齐
				}
				o := st.Pick(s.models)
				ui.Text(c, o.File).Font("monospace").FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine().Tooltip(o.File)
				if stems.ModelInstalled(l, o.File) {
					ui.Text(c, "已下载").FontSize(12).TextColor(t.Success)
				} else {
					ui.Textf(c, "未下载 · %d MB", o.SizeMB).FontSize(12).TextColor(t.TextMuted)
				}
			})
		}
		missing, missingMB := stems.Missing(l, stems.AllSteps(), s.models)
		var dl conductor.Job
		active := false
		if s.dlJob != "" {
			dl, _ = a.be.cond.Job(s.dlJob)
			active = !dl.State.Finished()
		}
		if len(missing) > 0 || active {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				if ui.PrimaryButton(c, fmt.Sprintf("下载缺少的模型（约 %.1f GB）", float64(missingMB)/1000)).Disabled(active).Clicked() {
					id, err := a.be.cond.Submit(stems.Engine, "stems.download", stems.DownloadParams(missing))
					s.dlJob, s.err = id, errText(err)
				}
				if active {
					ui.Text(c, dl.Message).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
				}
			})
			ui.Text(c, "未下载的模型也会在首次运行时自动下载。").FontSize(12).TextColor(t.TextMuted)
		}
		if dl.State == conductor.JobFailed {
			ui.Text(c, "下载失败："+dl.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		ui.Text(c, "模型保存在 "+stems.ModelDir(l)).FontSize(12).TextColor(t.TextMuted).Selectable()
	})
}

// stemsRunCard 选择输入与步骤并提交任务。
func (a *app) stemsRunCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.stems
	return card(c, tk, "处理", func() {
		a.audioPicker(c, "输入音频", &s.input, anyAudioExts())
		ui.Text(c, "步骤（勾选后会自动补上前置步骤）").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(16).Children(func() {
			for _, st := range stems.Steps {
				on := !s.disabled[st.ID]
				if ui.Checkbox(c.Key("step-"+st.ID), &on, st.Title).Changed() {
					if s.disabled == nil {
						s.disabled = map[string]bool{}
					}
					s.disabled[st.ID] = !on
				}
			}
		})
		var job conductor.Job
		active := false
		if s.job != "" {
			job, _ = a.be.cond.Job(s.job)
			active = !job.State.Finished()
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c, "开始处理").Disabled(active).Clicked() {
				params, err := stems.ChainParams(s.input, s.selected(), s.models)
				if err == nil {
					s.job, err = a.be.cond.Submit(stems.Engine, "stems.chain", params)
				}
				s.err = errText(err)
			}
			if active {
				if ui.Button(c, "取消").Clicked() {
					a.be.cond.Cancel(s.job)
				}
				ui.Spinner(c)
				ui.Text(c, job.Message).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
			}
		})
		if active {
			ui.Progress(c, job.Progress)
		}
		if job.State == conductor.JobFailed {
			ui.Text(c, "处理失败："+job.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(t.Danger)
		}
	})
}

// stemsResults 登记最近一次任务的输出（处理中会逐步出现）并显示结果列表。
func (a *app) stemsResults(c *ui.Context, tk *fluent.Tokens) {
	s := &a.stems
	if s.job != "" {
		j, _ := a.be.cond.Job(s.job)
		for _, p := range j.Artifacts {
			title := stems.OutputTitle(p)
			if title == "" {
				title = filepath.Base(p)
			}
			s.results.add(a, p, title, "", false)
		}
	}
	s.results.show(c, tk, a, "处理结果会显示在这里。")
}

// errText 把可能为 nil 的错误转为界面文本。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
