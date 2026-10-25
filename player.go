package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// 播放控制条（transport）：各页的试听、卷帘与放大对话框共用的播放控件。
// 第一行居中放循环、后退、播放 / 暂停（强调色胶囊按钮）、前进与音量，第二行是当前时间、进度条与总时长。
// 循环与音量是 audio.Player 自身的状态，audioPreview.swap 换音源时一并带过去。

// transportSkip 是后退 / 前进按钮跳过的时长（经验值：约一个乐句，方便回听）。
const transportSkip = 5 * time.Second

var (
	iconLoop    = icon(`<path d="M2.5 7.5V6.5a2 2 0 0 1 2-2h9l-2-2M13.5 8.5v1a2 2 0 0 1-2 2h-9l2 2"/>`)
	iconBack    = icon(`<path d="M3.5 3v10"/><path d="M12.5 3.5v9L6 8z" fill="currentColor"/>`)
	iconForward = icon(`<path d="M12.5 3v10"/><path d="M3.5 3.5v9L10 8z" fill="currentColor"/>`)
	iconPlay    = icon(`<path d="M5 3v10l8-5z" fill="currentColor"/>`)
	iconPause   = icon(`<path d="M5.5 3.5v9M10.5 3.5v9" stroke-width="2.2"/>`)
	iconVolume  = icon(`<path d="M2 6h2.5L8 3v10L4.5 10H2z"/><path d="M10.5 5.5a3.5 3.5 0 0 1 0 5M12.5 3.5a6.3 6.3 0 0 1 0 9"/>`)
	iconMuted   = icon(`<path d="M2 6h2.5L8 3v10L4.5 10H2z"/><path d="M10.5 6l3.5 4M14 6l-3.5 4"/>`)
)

// transport 构建 p 的播放控制条。key 区分同一界面上控制同一播放器的多个控制条（例如试听卡片与卷帘）。
// 播放中每 100 ms 重建一次，更新时间与进度条。
func transport(c *ui.Context, tk *fluent.Tokens, key string, p *audio.Player) {
	t := c.Theme()
	playing, pos, dur := p.Playing(), p.Position(), p.Duration()
	if playing {
		c.After(100 * time.Millisecond)
	}
	ui.Column(c.Key("transport-" + key)).Gap(4).AlignItems(ui.Stretch).Children(func() {
		// 按钮行：两侧的循环与音量对称排列，播放按钮居中。
		ui.Row(c).Gap(20).Center().Children(func() {
			loop := p.Loop()
			if transportButton(c, tk, key+"-loop", iconLoop, map[bool]string{true: "关闭循环", false: "循环播放"}[loop], loop).Clicked() {
				p.SetLoop(!loop)
			}
			if transportButton(c, tk, key+"-back", iconBack, fmt.Sprintf("后退 %.0f 秒", transportSkip.Seconds()), false).Clicked() {
				p.Seek(max(pos-transportSkip, 0))
			}
			playButton(c, key, p, playing)
			if transportButton(c, tk, key+"-fwd", iconForward, fmt.Sprintf("前进 %.0f 秒", transportSkip.Seconds()), false).Clicked() {
				p.Seek(min(pos+transportSkip, dur))
			}
			volumeButton(c, tk, key, p)
		})
		// 进度行：拖动或点击进度条跳转。
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, playerClock(pos)).FontSize(12).Font("monospace").TextColor(t.TextMuted)
			at := pos.Seconds()
			if ui.Slider(c.Key(key+"-progress"), &at, 0, max(dur.Seconds(), 0.001)).Label("播放进度").Grow(1).Changed() {
				p.Seek(time.Duration(at * float64(time.Second)))
			}
			ui.Text(c, playerClock(dur)).FontSize(12).Font("monospace").TextColor(t.TextMuted)
		})
	})
}

// transportButton 是控制条上的透明图标按钮；on 为 true 时（例如开启了循环）图标用强调色并保留底色。
func transportButton(c *ui.Context, tk *fluent.Tokens, key string, ic *ui.SVG, label string, on bool) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c.Key(key)).Size(36, 32).Radius(fluent.ControlRadius).Center().Label(label).Tooltip(label)
	switch {
	case b.Pressed():
		b.Background(tk.SubtlePressed)
	case b.Hovered() || on:
		b.Background(tk.SubtleHover)
	}
	if on {
		b.TextColor(t.Accent)
	}
	b.Children(func() { ui.Icon(c, ic).Size(18, 18) })
	return b
}

// playButton 是居中的播放 / 暂停按钮：强调色胶囊。
func playButton(c *ui.Context, key string, p *audio.Player, playing bool) {
	t := c.Theme()
	label, ic := "播放", iconPlay
	if playing {
		label, ic = "暂停", iconPause
	}
	b := ui.ButtonBase(c.Key(key+"-play")).Size(56, 36).Radius(18).Center().Label(label).Tooltip(label).
		TextColor(t.AccentText)
	switch {
	case b.Pressed():
		b.Background(t.AccentPressed)
	case b.Hovered():
		b.Background(t.AccentHover)
	default:
		b.Background(t.Accent)
	}
	b.Children(func() { ui.Icon(c, ic).Size(16, 16) })
	if b.Clicked() {
		if playing {
			p.Pause()
		} else {
			p.Play()
		}
	}
}

// volumeButton 是音量按钮：点击弹出音量滑块，音量为 0 时显示静音图标。
func volumeButton(c *ui.Context, tk *fluent.Tokens, key string, p *audio.Player) {
	t := c.Theme()
	vol := p.Volume()
	ic := iconVolume
	if vol == 0 {
		ic = iconMuted
	}
	b := transportButton(c, tk, key+"-vol", ic, fmt.Sprintf("音量 %.0f%%", vol*100), false)
	open := ui.Local(b, "open", func() bool { return false })
	if b.Clicked() {
		*open = !*open
	}
	ui.Popover(c, b, open, func() {
		ui.Row(c).Gap(8).Padding(8).AlignItems(ui.Center).Children(func() {
			if ui.Slider(c.Key(key+"-vol-slider"), &vol, 0, 1).Label("音量").Width(140).Changed() {
				p.SetVolume(vol)
			}
			ui.Textf(c, "%.0f%%", vol*100).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(40)
		})
	})
}

// playerClock 把时长格式化为「分:秒」（例如 03:07），用于控制条两端。
func playerClock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}
