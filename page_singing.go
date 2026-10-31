package main

import (
	"path/filepath"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/singing"
	"kanade/internal/ui/fluent"
)

// singingState is independent of RVC: lyric editing synthesizes new vocals.
type singingState struct {
	selectedBackend     string
	soulx               soulxSingingState
	vevo2               vevo2SingingState
	request             singing.Request
	job, doctorJob, err string
	results             resultSet
}

func (a *app) singingActive() bool {
	if a.be.cond == nil {
		return false
	}
	for _, id := range []string{a.singing.job, a.singing.doctorJob} {
		if j, ok := a.be.cond.Job(id); ok && !j.State.Finished() {
			return true
		}
	}
	return false
}

// singingRuntimeMatches compares against the submitted immutable job snapshot,
// so editing paths cannot leave an old successful check attached to new inputs.
func singingRuntimeMatches(j conductor.Job, r singing.Request) bool {
	return j.Params["python_path"] == r.PythonPath &&
		j.Params["source_dir"] == r.SourceDir && j.Params["model_dir"] == r.ModelDir
}

// submitSinging also guards programmatic/repeated clicks while a task is queued.
func (a *app) submitSinging(doctor bool) {
	s := &a.singing
	if a.be.cond == nil || a.singingAnyActive() {
		return
	}
	r := s.request
	if doctor {
		id, err := a.be.cond.Submit(singing.Engine, "yingmusic.doctor", map[string]any{
			"python_path": r.PythonPath, "source_dir": r.SourceDir, "model_dir": r.ModelDir,
		})
		s.doctorJob, s.err = id, errText(err)
		return
	}
	params, err := r.Params()
	if err == nil {
		s.job, err = a.be.cond.Submit(singing.Engine, "yingmusic.edit", params)
	}
	s.err = errText(err)
}

func (a *app) singingPage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "歌声改词", "选择歌词编辑模型重唱参考旋律；不同于仅替换音色的 RVC。")
	if a.backendUnavailable(c, tk) {
		return
	}
	a.singingModelSelector(c)
	switch a.singing.selectedBackend {
	case "soulx":
		a.soulxSingingPage(c, tk)
		return
	case "vevo2":
		a.vevo2SingingPage(c, tk)
		return
	}
	s := &a.singing
	r := &s.request
	t := c.Theme()
	active := a.singingActive()
	if active {
		ui.Text(c, "当前任务使用提交时的输入；编辑字段只影响下一次任务。").FontSize(12).TextColor(t.TextMuted)
	}
	card(c, tk, "独立运行环境（本地配置）", func() {
		ui.Text(c, "需单独准备上游源码、Python 3.10 环境及本地模型；不会自动下载模型权重。").TextColor(t.TextMuted)
		ui.Text(c, "上游 Python 解释器路径").FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c.Key("singing-python"), &r.PythonPath).Label("上游 Python 解释器路径").Placeholder("独立环境中的 python / python.exe 绝对路径")
		ui.Text(c, "YingMusic-Singer-Plus 源码目录").FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c.Key("singing-source"), &r.SourceDir).Label("YingMusic-Singer-Plus 源码目录")
		ui.Text(c, "本地模型目录").FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c.Key("singing-model"), &r.ModelDir).Label("本地模型目录").Placeholder("包含 config.json 和 model.safetensors 的本地快照目录")
		ui.Text(c, "使用前请阅读 docs/singing-editing.md 的安装与许可说明；上游主模型和 Stability VAE 的许可不同。").FontSize(12).TextColor(t.TextMuted)
		if ui.Button(c.Key("singing-doctor"), "检查本地环境").Disabled(active).Clicked() {
			a.submitSinging(true)
		}
		if j, ok := a.be.cond.Job(s.doctorJob); ok {
			if !singingRuntimeMatches(j, *r) {
				ui.Text(c, "运行环境配置已更改，请重新检查。").TextColor(t.TextMuted)
			} else if j.State == conductor.JobDone {
				ui.Text(c, "本地环境检查通过；尚未验证实际生成质量。").TextColor(t.Success)
			} else if j.State == conductor.JobFailed {
				ui.Text(c, "环境检查失败："+j.Err).TextColor(t.Danger).Selectable()
			}
		}
	})
	cardRow(c, cardMinWidth, func() ui.Element {
		return card(c, tk, "参考与歌词", func() {
			ui.Text(c, "请先在「分轨」中提取干净主唱。模型会重新合成，旋律、时长与音色均不保证逐样本不变。").FontSize(12).TextColor(t.TextMuted)
			a.audioPicker(c.Key("singing-melody"), "旋律干声", &r.MelodyAudio, anyAudioExts())
			a.audioPicker(c.Key("singing-reference"), "音色干声", &r.RefAudio, anyAudioExts())
			ui.Text(c, "音色参考留空时使用旋律干声；填写对应参考音频实际唱出的原词。").FontSize(12).TextColor(t.TextMuted)
			if r.RefAudio != "" && ui.Button(c.Key("singing-clear-reference"), "改用旋律干声音色").Clicked() {
				r.RefAudio = ""
				r.RefText = "" // A different reference requires a fresh matching transcription.
			}
			ui.Text(c, "音色参考原歌词（必填）").FontSize(12).TextColor(t.TextMuted)
			ui.TextArea(c.Key("singing-ref-text"), &r.RefText).Label("音色参考原歌词（必填）").Placeholder("与音色参考音频实际唱词一致；音色参考留空时填写旋律参考原词。用 | 分句。").Height(100)
			ui.Text(c, "目标新歌词（必填）").FontSize(12).TextColor(t.TextMuted)
			ui.TextArea(c.Key("singing-target-text"), &r.TargetText).Label("目标新歌词（必填）").Placeholder("输入要唱的新词，用 | 分句；先尝试短片段。").Height(120)
			a.audioPicker(c.Key("singing-accompaniment"), "伴奏（可选）", &r.AccompanimentAudio, anyAudioExts())
			if r.AccompanimentAudio != "" && ui.Button(c.Key("singing-clear-accompaniment"), "移除伴奏").Clicked() {
				r.AccompanimentAudio = ""
			}
			ui.Text(c, "仅使用获得授权的音频和音色；生成结果请先试听，再保存。").FontSize(12).TextColor(t.TextMuted)
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				if ui.PrimaryButton(c.Key("singing-submit"), "生成改词歌声").Disabled(active).Clicked() {
					a.submitSinging(false)
				}
				if active {
					if ui.Button(c.Key("singing-cancel"), "取消").Clicked() {
						for _, id := range []string{s.job, s.doctorJob} {
							if id != "" {
								a.be.cond.Cancel(id)
							}
						}
					}
					ui.Spinner(c)
				}
			})
			for _, id := range []string{s.job, s.doctorJob} {
				if j, ok := a.be.cond.Job(id); ok {
					if !j.State.Finished() {
						ui.Text(c.Key("status-"+id), j.Message).FontSize(12).TextColor(t.TextMuted)
						ui.Progress(c.Key("progress-"+id), j.Progress)
					} else if j.State == conductor.JobCanceled {
						ui.Text(c.Key("canceled-"+id), "任务已取消。")
					} else if j.State == conductor.JobFailed && id == s.job {
						ui.Text(c, "生成失败："+j.Err).TextColor(t.Danger).Selectable()
					}
				}
			}
			if s.err != "" {
				ui.Text(c, s.err).TextColor(t.Danger).Selectable()
			}
		})
	}, func() ui.Element {
		return card(c, tk, "试听与保存", func() {
			if j, ok := a.be.cond.Job(s.job); ok {
				for _, p := range j.Artifacts {
					s.results.add(a, p, filepath.Base(p), "", false)
				}
			}
			s.results.show(c, tk, a, "生成的人声及可选混音会显示在这里。")
		})
	})
}
