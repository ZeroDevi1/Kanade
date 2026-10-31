package main

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"kanade/internal/conductor"
	"kanade/internal/singing"
	"kanade/internal/ui/fluent"
)

// Each model retains independent inputs, jobs and outputs when the user changes tabs.
type singingModelJobs struct {
	job, doctorJob, doctorSnapshot, err string
	results                             resultSet
}
type soulxSingingState struct {
	request singing.SoulXRequest
	jobs    singingModelJobs
}
type vevo2SingingState struct {
	request singing.Vevo2Request
	jobs    singingModelJobs
}

func (a *app) singingJobsActive(s *singingModelJobs) bool {
	if a.be.cond == nil {
		return false
	}
	for _, id := range []string{s.job, s.doctorJob} {
		if j, ok := a.be.cond.Job(id); ok && !j.State.Finished() {
			return true
		}
	}
	return false
}
func (a *app) singingAnyActive() bool {
	return a.singingActive() || a.singingJobsActive(&a.singing.soulx.jobs) || a.singingJobsActive(&a.singing.vevo2.jobs)
}
func soulxRuntimeKey(r singing.SoulXRequest) string {
	return strings.Join([]string{r.PythonPath, r.SourceDir, r.ModelDir, r.Device}, "\x00")
}
func vevo2RuntimeKey(r singing.Vevo2Request) string {
	return strings.Join([]string{r.PythonPath, r.SourceDir, r.ModelDir, r.WhisperPath, r.WhisperStatsPath, strconv.FormatBool(r.LicenseAccepted)}, "\x00")
}

func (a *app) submitSingingModel(engine string, doctor bool) {
	if a.be.cond == nil || a.singingAnyActive() {
		return
	}
	var s *singingModelJobs
	var p map[string]any
	var err error
	var snapshot string
	switch engine {
	case "soulx":
		s = &a.singing.soulx.jobs
		r := a.singing.soulx.request
		snapshot = soulxRuntimeKey(r)
		if doctor {
			p, err = r.DoctorParams()
		} else {
			p, err = r.Params()
		}
	case "vevo2":
		s = &a.singing.vevo2.jobs
		r := a.singing.vevo2.request
		snapshot = vevo2RuntimeKey(r)
		if doctor {
			p, err = r.DoctorParams()
		} else {
			p, err = r.Params()
		}
	default:
		return
	}
	if err == nil {
		task := engine + ".edit"
		if doctor {
			task = engine + ".doctor"
		}
		var id string
		id, err = a.be.cond.Submit(engine, task, p)
		if err == nil {
			if doctor {
				s.doctorJob = id
				s.doctorSnapshot = snapshot
			} else {
				s.job = id
			}
		}
	}
	s.err = errText(err)
}

// Visible labels are separate Text widgets; TextInput.Label is accessibility-only.
func singingPathInput(c *ui.Context, key, label, hint string, value *string) {
	ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
	ui.TextInput(c.Key(key), value).Label(label).Placeholder(hint)
}
func (a *app) singingModelSelector(c *ui.Context) {
	ui.Row(c.Key("singing-model-selector")).Wrap().Gap(8).Children(func() {
		for _, choice := range []struct{ key, title string }{{"", "YingMusic"}, {"soulx", "SoulX SVS"}, {"vevo2", "Vevo2 AR+FM"}} {
			if ui.Button(c.Key(choice.key), choice.title).Disabled(a.singingAnyActive()).Clicked() {
				a.singing.selectedBackend = choice.key
				if choice.key == "soulx" && a.singing.soulx.request.Control == "" {
					a.singing.soulx.request.Control = "score"
				}
				if choice.key == "soulx" && a.singing.soulx.request.Device == "" {
					a.singing.soulx.request.Device = "cuda"
				}
			}
		}
	})
	if a.singingAnyActive() {
		ui.Text(c, "当前任务使用提交时的输入；请先取消或等待完成，再切换模型。").FontSize(12).TextColor(c.Theme().TextMuted)
	}
}

func (a *app) singingModelEnvironment(c *ui.Context, tk *fluent.Tokens, engine, runtimeKey string, s *singingModelJobs, fields func()) {
	card(c, tk, "独立环境与本地权重", func() {
		ui.Text(c, "每个模型使用独立配置的 Python 环境与本地权重，不自动下载。环境检查不代表 GPU 推理或音质验证通过。").FontSize(12).TextColor(c.Theme().TextMuted)
		fields()
		if ui.Button(c.Key(engine+"-doctor"), "检查本地环境").Disabled(a.singingAnyActive()).Clicked() {
			a.submitSingingModel(engine, true)
		}
		if j, ok := a.be.cond.Job(s.doctorJob); ok {
			if s.doctorSnapshot != runtimeKey {
				ui.Text(c, "运行环境配置已更改，请重新检查。").TextColor(c.Theme().TextMuted)
			} else if j.State == conductor.JobDone {
				ui.Text(c, "本地环境检查通过；尚未验证实际生成质量。").TextColor(c.Theme().Success)
			} else if j.State == conductor.JobFailed {
				ui.Text(c, "环境检查失败："+j.Err).TextColor(c.Theme().Danger).Selectable()
			}
		}
	})
}
func (a *app) singingModelActions(c *ui.Context, engine string, s *singingModelJobs) {
	ui.Row(c).Gap(8).Children(func() {
		if ui.PrimaryButton(c.Key(engine+"-submit"), "生成改词歌声").Disabled(a.singingAnyActive()).Clicked() {
			a.submitSingingModel(engine, false)
		}
		if a.singingJobsActive(s) && ui.Button(c.Key(engine+"-cancel"), "取消").Clicked() {
			for _, id := range []string{s.job, s.doctorJob} {
				if id != "" {
					a.be.cond.Cancel(id)
				}
			}
		}
	})
	for _, id := range []string{s.job, s.doctorJob} {
		if j, ok := a.be.cond.Job(id); ok {
			if !j.State.Finished() {
				ui.Text(c.Key("message-"+id), j.Message)
				ui.Progress(c.Key("progress-"+id), j.Progress)
			} else if j.State == conductor.JobCanceled {
				ui.Text(c.Key("cancel-"+id), "任务已取消。")
			} else if j.State == conductor.JobFailed && id == s.job {
				ui.Text(c, "生成失败："+j.Err).TextColor(c.Theme().Danger).Selectable()
			}
		}
	}
	if s.err != "" {
		ui.Text(c, s.err).TextColor(c.Theme().Danger).Selectable()
	}
}
func (a *app) singingModelResults(c *ui.Context, tk *fluent.Tokens, s *singingModelJobs) ui.Element {
	return card(c, tk, "试听与保存", func() {
		if j, ok := a.be.cond.Job(s.job); ok {
			for _, p := range j.Artifacts {
				s.results.add(a, p, filepath.Base(p), "", false)
			}
		}
		s.results.show(c, tk, a, "此模型生成的人声及可选混音会显示在这里。")
	})
}
func (a *app) soulxSingingPage(c *ui.Context, tk *fluent.Tokens) {
	s := &a.singing.soulx
	r := &s.request
	ui.Text(c, "SoulX-Singer SVS · 高级模式 · 需手工校准元数据；不是 SVC 换音色。").FontWeight(600)
	a.singingModelEnvironment(c, tk, "soulx", soulxRuntimeKey(*r), &s.jobs, func() {
		singingPathInput(c, "soulx-python", "SoulX Python 解释器", "独立环境 python / python.exe 绝对路径", &r.PythonPath)
		singingPathInput(c, "soulx-source", "SoulX 源码目录", "SoulX-Singer 源码根目录", &r.SourceDir)
		singingPathInput(c, "soulx-model", "SoulX SVS 权重目录", "包含 SVS model.pt，不是 model-svc.pt", &r.ModelDir)
		ui.Text(c, "推理设备（默认 CUDA；CPU 性能与质量未验证）").FontSize(12)
		ui.Select(c.Key("soulx-device"), &r.Device, []string{"cuda", "cpu"}).Label("SoulX 推理设备")
	})
	cardRow(c, cardMinWidth, func() ui.Element {
		return card(c, tk, "高级模式：参考片段与目标元数据", func() {
			ui.Text(c, "先在「分轨」提取主唱，再准备与手工校准 JSON。新歌词必须改为目标 metadata 的 phoneme，并对齐 duration / note_pitch / note_type；仅改 text 字段无效。").FontSize(12).TextColor(c.Theme().TextMuted)
			a.audioPicker(c.Key("soulx-prompt"), "参考干声", &r.PromptAudio, anyAudioExts())
			ui.Text(c, "参考音频须预裁为与参考 metadata 第一段一致的片段，时间从 0 开始。目标 metadata 的时间用于全轨放置；请先检查原曲节拍与音符。").FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Text(c, "参考 metadata JSON").FontSize(12)
			a.filePicker(c.Key("soulx-prompt-meta"), "参考 JSON", "JSON", &r.PromptMetadata, []string{"json"})
			ui.Text(c, "目标 metadata JSON（包含新歌词音素）").FontSize(12)
			a.filePicker(c.Key("soulx-target-meta"), "目标 JSON", "JSON", &r.TargetMetadata, []string{"json"})
			ui.Text(c, "控制模式（score 默认；melody 需要参考与目标都提供 f0）").FontSize(12)
			// An empty value is displayed as score by initialization when opening this tab.
			ui.Select(c.Key("soulx-control"), &r.Control, []string{"score", "melody"}).Label("SoulX 控制模式")
			ui.Text(c, "默认不自动移调。模型不保证音色或旋律完全不变；元数据校准不是自动改词步骤。安装、许可和字段格式见 docs/singing-backends.md。").FontSize(12).TextColor(c.Theme().TextMuted)
			a.singingModelActions(c, "soulx", &s.jobs)
		})
	}, func() ui.Element { return a.singingModelResults(c, tk, &s.jobs) })
}
func (a *app) vevo2SingingPage(c *ui.Context, tk *fluent.Tokens) {
	s := &a.singing.vevo2
	r := &s.request
	ui.Text(c, "Vevo2 AR+FM · 使用原曲韵律、音色及新歌词重唱；不是 FM-only 音色转换。").FontWeight(600)
	a.singingModelEnvironment(c, tk, "vevo2", vevo2RuntimeKey(*r), &s.jobs, func() {
		singingPathInput(c, "vevo2-python", "Vevo2 Python 解释器", "独立 Amphion 环境 python / python.exe", &r.PythonPath)
		singingPathInput(c, "vevo2-source", "Amphion 源码目录", "与支持版本匹配的 Amphion 根目录", &r.SourceDir)
		singingPathInput(c, "vevo2-model", "Vevo2 本地模型目录", "RMSnow/Vevo2 的完整推理权重布局", &r.ModelDir)
		singingPathInput(c, "vevo2-whisper", "本地 Whisper medium.pt（必需）", "显式选择本地文件，避免 Whisper 自动下载", &r.WhisperPath)
		singingPathInput(c, "vevo2-stats", "Whisper stats（可选覆盖）", "留空使用模型目录内的官方 whisper_stats.pt", &r.WhisperStatsPath)
		ui.Text(c, "Vevo2 权重许可：CC-BY-NC-ND 4.0（非商业、禁止演绎）。本功能不分发权重，不代表取得商业或衍生模型使用授权；请先核对上游许可。见 docs/singing-backends.md。").FontSize(12).TextColor(c.Theme().TextMuted)
		ui.Checkbox(c.Key("vevo2-license"), &r.LicenseAccepted, "已阅读并确认遵守 Vevo2 权重许可")
	})
	cardRow(c, cardMinWidth, func() ui.Element {
		return card(c, tk, "原曲与新歌词", func() {
			ui.Text(c, "先在「分轨」获得干净主唱。此工作流将同一原曲作为韵律、风格和音色参考；保真与发音质量须试听确认。").FontSize(12).TextColor(c.Theme().TextMuted)
			a.audioPicker(c.Key("vevo2-source-audio"), "原曲干声", &r.SourceAudio, anyAudioExts())
			ui.Text(c, "原曲原歌词（必填）").FontSize(12)
			ui.TextArea(c.Key("vevo2-original"), &r.OriginalText).Label("Vevo2 原歌词").Height(100)
			ui.Text(c, "目标新歌词（必填）").FontSize(12)
			ui.TextArea(c.Key("vevo2-target"), &r.TargetText).Label("Vevo2 目标歌词").Height(100)
			a.audioPicker(c.Key("vevo2-accompaniment"), "伴奏（可选）", &r.AccompanimentAudio, anyAudioExts())
			if r.AccompanimentAudio != "" && ui.Button(c.Key("vevo2-clear-accompaniment"), "移除伴奏").Clicked() {
				r.AccompanimentAudio = ""
			}
			a.singingModelActions(c, "vevo2", &s.jobs)
		})
	}, func() ui.Element { return a.singingModelResults(c, tk, &s.jobs) })
}
