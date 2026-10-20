package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/ui/fluent"
)

// jobStateLabel 是任务状态的显示文本。
var jobStateLabel = map[conductor.JobState]string{
	conductor.JobQueued:    "排队中",
	conductor.JobPreparing: "准备中",
	conductor.JobRunning:   "运行中",
	conductor.JobDone:      "已完成",
	conductor.JobFailed:    "失败",
	conductor.JobCanceled:  "已取消",
}

// backendUnavailable 在后端未启动（启动失败或测试环境）时显示原因，并返回 true。
func (a *app) backendUnavailable(c *ui.Context, tk *fluent.Tokens) bool {
	if a.be.cond != nil {
		return false
	}
	msg := "推理后端未启动。"
	if a.be.err != nil {
		msg = "推理后端启动失败：" + a.be.err.Error()
	}
	card(c, tk, "后端不可用", func() {
		ui.Text(c, msg).TextColor(c.Theme().Danger)
	})
	return true
}

// tasksPage 列出 Conductor 的任务队列，最新的在前。
func (a *app) tasksPage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "任务", "所有推理任务按提交顺序逐个执行，同一时间占用一个 GPU 槽位。")
	if a.backendUnavailable(c, tk) {
		return
	}
	jobs := a.be.cond.Jobs()
	if len(jobs) == 0 {
		card(c, tk, "暂无任务", func() {
			ui.Text(c, "在各功能页或「M0 验证」页提交任务后会显示在这里。").TextColor(c.Theme().TextMuted)
		})
		return
	}
	for i := len(jobs) - 1; i >= 0; i-- {
		a.jobCard(c, tk, jobs[i])
	}
}

// jobCard 显示一个任务：引擎与任务名、状态、进度、错误与输出文件。
func (a *app) jobCard(c *ui.Context, tk *fluent.Tokens, j conductor.Job) {
	t := c.Theme()
	ui.Column(c.Key("job-"+j.ID)).Padding(12, 16).Gap(8).AlignItems(ui.Stretch).Radius(fluent.OverlayRadius).
		Background(tk.Card).Border(1, tk.CardStroke).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, j.Engine+" · "+j.Task).FontWeight(600)
			ui.Text(c, j.ID).FontSize(12).TextColor(t.TextMuted).Font("monospace")
			ui.Box(c).Grow(1)
			state := ui.Text(c, jobStateLabel[j.State]).FontSize(12)
			switch j.State {
			case conductor.JobFailed:
				state.TextColor(t.Danger)
			case conductor.JobDone:
				state.TextColor(t.Success)
			default:
				state.TextColor(t.TextMuted)
			}
			if !j.State.Finished() && ui.Button(c, "取消").Clicked() {
				a.be.cond.Cancel(j.ID)
			}
		})
		if !j.State.Finished() {
			ui.Progress(c, j.Progress)
			if j.Message != "" {
				ui.Textf(c, "%s · %.0f%%", j.Message, j.Progress*100).FontSize(12).TextColor(t.TextMuted)
			}
		}
		if j.Err != "" {
			ui.Text(c, j.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		for _, p := range j.Artifacts {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, filepath.Base(p)).FontSize(12).Font("monospace").Selectable()
				if !j.Ended.IsZero() {
					ui.Text(c, fmt.Sprintf("用时 %s", j.Ended.Sub(j.Created).Round(100*time.Millisecond))).FontSize(12).TextColor(t.TextMuted)
				}
			})
		}
	})
}
