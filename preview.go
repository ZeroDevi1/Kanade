package main

import (
	"path/filepath"

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
	// autoplay 为 true 时，正在解码的文件解码完成后立即播放（随后清除）。
	autoplay bool
	// clock 非 nil 时由页面统一控制播放（例如音乐分析页底部的控制条）：波形的播放头与点击跳转跟随 clock 返回的
	// 播放器（可能不是 player，例如 K 歌时的伴奏），预览自身不显示控制条。
	clock func() *audio.Player
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
			if pv.autoplay {
				pv.autoplay = false
				pv.player.Play()
			}
		})
	}()
}

// show 构建预览：文件名、波形与播放控制条（transport；设置了 clock 时不显示）。没有文件时显示 empty。
// actions 在文件名右侧构建额外的按钮（例如保存），可为 nil。
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
		if actions != nil {
			actions()
		}
	})
	if pv.err != "" {
		ui.Text(c, pv.err).FontSize(12).TextColor(t.Danger)
		return
	}
	p := pv.player
	if pv.clock != nil {
		p = pv.clock()
	}
	var pb waveform.Playback
	if p != nil {
		pb = playback{p}
	}
	waveform.Show(c, &pv.handle, pv.peaks, pb).Height(96).Background(tk.Card).
		Border(1, tk.CardStroke).Radius(fluent.OverlayRadius)
	if pv.clock == nil && p != nil && !pv.loading {
		transport(c, tk, "preview", p)
	}
}

// reset 停止播放并清空预览（显示的文件已被删除时使用）；正在后台解码的结果会因 path 不符而被丢弃。
func (pv *audioPreview) reset() {
	if pv.player != nil {
		pv.player.Stop()
	}
	if pv.original != nil && pv.original != pv.player {
		pv.original.Stop()
	}
	*pv = audioPreview{handle: pv.handle, title: pv.title, clock: pv.clock}
}

// swap 把播放的音源换成 snd（例如合成的和弦伴奏），保留播放位置、播放状态、循环与音量；波形仍显示原文件。
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
	loop, vol := pv.player.Loop(), pv.player.Volume()
	pv.player.Pause()
	if pv.player != pv.original {
		pv.player.Stop()
	}
	next.SetLoop(loop).SetVolume(vol) // 控制条上的循环与音量属于播放器，换音源时带过去
	next.Seek(min(pos, next.Duration()))
	if playing {
		next.Play()
	}
	pv.player = next
}
