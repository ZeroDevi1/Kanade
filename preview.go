package main

import (
	"path/filepath"
	"time"

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
	path string
	// title 非空时代替文件名显示（例如分析页播放的是解码出的 audio.wav，显示原曲名更直观）。
	title   string
	loading bool
	err     string
	peaks   *waveform.Peaks
	player  *audio.Player
	// samples / rate / ch 是当前文件解码后的采样（见 loadedAudio），供在其上叠加合成音；多声道文件为 nil。
	samples  []float32
	rate, ch int
	// original 是文件本身的播放器；swap 换成其他音源后，restore 用它换回。
	original *audio.Player
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
			if pv.original != nil && pv.original != pv.player {
				pv.original.Stop()
			}
			pv.peaks, pv.player, pv.original = &la.peaks, la.player, la.player
			pv.samples, pv.rate, pv.ch = la.samples, la.rate, la.ch
		})
	}()
}

// show 构建预览：文件名、播放按钮、时间与波形。没有文件时显示 empty。
// actions 在播放按钮之后构建额外的按钮（例如保存），可为 nil。
func (pv *audioPreview) show(c *ui.Context, tk *fluent.Tokens, empty string, actions func()) {
	t := c.Theme()
	if pv.path == "" {
		ui.Text(c, empty).TextColor(t.TextMuted)
		return
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		name := filepath.Base(pv.path)
		if pv.title != "" {
			name = pv.title
		}
		ui.Text(c, name).Font("monospace").FontSize(12).Grow(1).SingleLine()
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
		if actions != nil {
			actions()
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

// reset 停止播放并清空预览（显示的文件已被删除时使用）；正在后台解码的结果会因 path 不符而被丢弃。
func (pv *audioPreview) reset() {
	if pv.player != nil {
		pv.player.Stop()
	}
	if pv.original != nil && pv.original != pv.player {
		pv.original.Stop()
	}
	*pv = audioPreview{handle: pv.handle, title: pv.title}
}

// swap 把播放的音源换成 snd（例如合成的和弦伴奏），保留播放位置与播放状态；波形仍显示原文件。
// snd 为 nil 时换回原文件。
func (pv *audioPreview) swap(snd *audio.Sound) {
	if pv.player == nil {
		return
	}
	next := pv.original
	if snd != nil {
		next = snd.NewPlayer()
	}
	if next == pv.player {
		return
	}
	pos, playing := pv.player.Position(), pv.player.Playing()
	pv.player.Pause()
	if pv.player != pv.original {
		pv.player.Stop()
	}
	next.Seek(min(pos, next.Duration()))
	if playing {
		next.Play()
	}
	pv.player = next
}
