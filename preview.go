package main

import (
	"path/filepath"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
	"kanade/internal/ui/waveform"
)

// audioPreview 是带波形与播放控制的音频预览，用于展示推理输出。
//
// 状态只在 UI 线程修改；解码在后台 goroutine 进行，结果经 app.post 回到 UI 线程。
type audioPreview struct {
	handle ui.Handle
	// path 是正在显示（或正在解码）的文件；loading 为 true 时 peaks / player 仍是上一个文件的。
	path    string
	loading bool
	err     string
	peaks   *waveform.Peaks
	player  *audio.Player
}

// load 开始在后台解码 path；与当前文件相同时不做任何事。
func (pv *audioPreview) load(a *app, path string) {
	if path == "" || path == pv.path {
		return
	}
	pv.path, pv.loading, pv.err = path, true, ""
	go func() {
		la, err := loadAudio(path)
		a.post(func() {
			if pv.path != path { // 解码期间又切换了文件，丢弃过期结果
				if err == nil {
					la.player.Stop()
				}
				return
			}
			pv.loading = false
			if err != nil {
				pv.err = err.Error()
				return
			}
			if pv.player != nil {
				pv.player.Stop()
			}
			pv.peaks, pv.player = &la.peaks, la.player
		})
	}()
}

// show 构建预览：文件名、播放按钮、时间与波形。没有文件时显示 empty。
func (pv *audioPreview) show(c *ui.Context, tk *fluent.Tokens, empty string) {
	t := c.Theme()
	if pv.path == "" {
		ui.Text(c, empty).TextColor(t.TextMuted)
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, filepath.Base(pv.path)).Font("monospace").FontSize(12).Grow(1).SingleLine()
		if pv.loading {
			ui.Spinner(c)
		}
		if p := pv.player; p != nil && !pv.loading {
			label := "播放"
			if p.Playing() {
				label = "暂停"
				c.After(100 * time.Millisecond)
			}
			ui.Textf(c, "%s / %s", clock(p.Position()), clock(p.Duration())).Font("monospace").FontSize(12).TextColor(t.TextMuted)
			if ui.Button(c, label).Width(72).Clicked() {
				if p.Playing() {
					p.Pause()
				} else {
					p.Play()
				}
			}
		}
		if ui.Button(c, "在文件夹中显示").Clicked() {
			dir := filepath.Dir(pv.path)
			go func() { _ = mygo.Shell.OpenPath(dir) }()
		}
	})
	if pv.err != "" {
		ui.Text(c, pv.err).FontSize(12).TextColor(t.Danger)
		return
	}
	var pb waveform.Playback
	if pv.player != nil {
		pb = playback{pv.player}
	}
	waveform.Show(c, &pv.handle, pv.peaks, pb).Height(96).Background(tk.Card).
		Border(1, tk.CardStroke).Radius(fluent.OverlayRadius)
}
