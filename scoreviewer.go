package main

import (
	"fmt"
	"math"
	"sort"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 卷帘的放大查看（只读，类似 Cubase 的 Key Editor）：在接近整个窗口大小的对话框里查看一条音轨。
// 左侧是钢琴键盘（鼓为鼓件名称），上方是小节标尺；音符块按音高排在半音行上，宽度够时标出音名，
// 正在发声的音高亮；播放头与试听同步。滑块调节横向（可见秒数）与纵向（行高）缩放；
// 拖动画布平移（横向移动时间、纵向移动音高，并关闭跟随），单击跳转到该处。

// 放大查看的布局尺寸（DIP）。
const (
	zoomKeysWidth  = 64 // 左侧键盘 / 鼓件名称栏
	zoomRulerH     = 24 // 上方小节标尺
	zoomMinSpan    = 2.0
	zoomMinRowH    = 6.0
	zoomMaxRowH    = 40.0
	zoomDialogEdge = 48 // 对话框与窗口边缘的距离（每边）
)

// drumRowNames 是鼓轨各行的名称，与 drumRow 的分类一致（从下往上）。
var drumRowNames = []string{"底鼓", "军鼓", "通鼓", "踩镲", "镲片"}

// zoomState 是放大查看的状态，属于 rollState。
type zoomState struct {
	open   bool
	track  int     // 查看的音轨（sheetState.tracks 的序号）
	span   float64 // 可见时长（秒）
	rowH   float64 // 每行（半音或鼓件）的高度
	start  float64 // 不跟随时窗口的起点（秒）
	top    float64 // 顶行的音高（MIDI，拖动时连续变化）；鼓与原曲不用
	follow bool
	// dragged 记录本次按下期间是否拖动过：拖动过的按下松开时不当作单击跳转。
	dragged bool
	handle  ui.Handle // 画布，读取上一帧尺寸
}

// openZoom 打开第 i 条音轨的放大查看：时间窗口保持当前位置，纵向居中到音轨的音域。
func (a *app) openZoom(i int) {
	s := &a.score.sheet
	z := &s.roll.zoom
	z.open, z.track, z.follow = true, i, s.roll.follow
	if z.span == 0 {
		z.span, z.rowH = 12, 14
	}
	pos, _ := a.scorePosition()
	z.start = max(pos-z.span*0.2, 0)
	z.top = 0 // 首次绘制时按画布高度居中（见 zoomCanvas）
}

// scoreZoomDialog 构建放大查看对话框（关闭时什么也不做）。
func (a *app) scoreZoomDialog(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	s := &a.score.sheet
	z := &s.roll.zoom
	if !z.open || z.track >= len(s.tracks) {
		z.open = false
		return
	}
	t := c.Theme()
	b := a.root.Bounds(c)
	w, h := max(b.W-2*zoomDialogEdge, 640), max(b.H-2*zoomDialogEdge, 420)
	ui.Modal(c, &z.open, func() {
		ui.Column(c).Width(w - 40).Height(h - 40).Gap(12).AlignItems(ui.Stretch).Children(func() {
			// 顶栏：音轨、缩放与跟随、关闭；播放控制条在画布下方。
			ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
				names := make([]string, len(s.tracks))
				for i, tr := range s.tracks {
					names[i] = tr.Title
				}
				sel := names[z.track]
				if ui.Select(c, &sel, names).Label("查看的音轨").Width(200).Changed() {
					for i, n := range names {
						if n == sel {
							z.track, z.top = i, 0
						}
					}
				}
				ui.Text(c, "横向").FontSize(12).TextColor(t.TextMuted)
				// 滑块向右为放大：可见时长从全曲缩到 2 秒（对数刻度，便于在短窗口间细调）。
				full := max(res.Duration, zoomMinSpan*2)
				zoomX := 1 - math.Log(z.span/zoomMinSpan)/math.Log(full/zoomMinSpan)
				if ui.Slider(c, &zoomX, 0, 1).Label("横向缩放").Width(160).Changed() {
					z.span = zoomMinSpan * math.Pow(full/zoomMinSpan, 1-zoomX)
				}
				ui.Textf(c, "%.0f 秒", z.span).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(52)
				ui.Text(c, "纵向").FontSize(12).TextColor(t.TextMuted)
				ui.Slider(c, &z.rowH, zoomMinRowH, zoomMaxRowH).Label("纵向缩放").Width(120)
				ui.Checkbox(c, &z.follow, "播放时跟随")
				ui.Box(c).Grow(1)
				if ui.Button(c.Key("zoom-close"), "关闭").Clicked() {
					z.open = false
				}
			})
			a.zoomCanvas(c, tk, res)
			// 对话框遮住了页面底部的控制条，在画布下方另放一个，控制同一个播放器（scoreClock）。
			if p := a.scoreClock(); p != nil {
				transport(c, tk, "zoom", p)
			}
			ui.Text(c, "拖动画布平移，单击跳转；Esc 或点击对话框外关闭。").FontSize(12).TextColor(t.TextMuted)
		})
	})
}

// zoomView 返回 pos 时可见的时间范围：跟随时播放头位于 20% 处，否则从 start 起。
func (z *zoomState) zoomView(pos, duration float64) (v0, v1 float64) {
	span := min(z.span, max(duration, zoomMinSpan))
	v0 = z.start
	if z.follow && pos >= 0 {
		v0 = pos - span*0.2
	}
	v0 = min(max(v0, 0), max(duration-span, 0))
	return v0, v0 + span
}

// zoomCanvas 是放大查看的画布：标尺、键盘与音符网格画在同一个元素里，位置在绘制时读取以逐帧跟随播放。
func (a *app) zoomCanvas(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	s := &a.score.sheet
	z := &s.roll.zoom
	pv := &a.score.preview
	tr := s.tracks[z.track]
	color := t.TextMuted
	if z.track != score.TrackOriginal {
		color = rollColor(z.track)
	}
	box := ui.Box(c.Key("zoom-canvas")).Grow(1).MinHeight(0).Bind(&z.handle).Radius(fluent.ControlRadius).
		Background(rollPaper.white).Border(1, tk.CardStroke).Clip().Cursor(ui.CursorPointer).Label(tr.Title + "放大卷帘")
	bounds := z.handle.Bounds(c)
	gridW := float64(bounds.W - zoomKeysWidth)
	gridH := float64(bounds.H - zoomRulerH)
	pos, seek := a.scorePosition()
	measures := res.Measures()

	// 纵向范围：有音高的音轨 0–127 每个半音一行，鼓 5 行，原曲画波形不用行。
	pitched := z.track != score.TrackOriginal && !tr.Drum
	if pitched && z.top == 0 && gridH > 0 && len(tr.Notes) > 0 {
		lo, hi := tr.Notes[0].MIDI, tr.Notes[0].MIDI
		for _, n := range tr.Notes {
			lo, hi = min(lo, n.MIDI), max(hi, n.MIDI)
		}
		z.top = float64(lo+hi)/2 + gridH/z.rowH/2 // 音域的中点放在画布垂直中央
	}
	if pitched {
		visible := gridH / z.rowH
		z.top = min(max(z.top, visible-0.5), 127.5)
	}

	// 滚轮（与 Cubase 相同）：Ctrl 纵向缩放、Ctrl+Shift 横向缩放，都以指针所在的音高 / 时刻为中心；
	// 不按 Ctrl 时滚动：纵向移动音高，Shift 横向移动时间（并关闭跟随）。
	box.HandleInput(func(ev ui.InputEvent) bool {
		if gridW <= 0 || gridH <= 0 {
			return false
		}
		return z.wheel(ev, gridW, pos, res.Duration, pitched)
	})

	// 拖动平移：横向按像素换算秒数，纵向按行高换算半音；拖动即关闭跟随。单击（没有拖动）跳转。
	if dx, dy, ok := box.Dragged(); ok && (dx != 0 || dy != 0) {
		v0, v1 := z.zoomView(pos, res.Duration)
		if gridW > 0 {
			z.start = v0 - float64(dx)/gridW*(v1-v0)
		}
		if pitched {
			z.top += float64(dy) / z.rowH
		}
		z.follow, z.dragged = false, true
	}
	if box.Clicked() {
		if x, _, _ := box.PointerPosition(); !z.dragged && x > zoomKeysWidth && gridW > 0 {
			v0, v1 := z.zoomView(pos, res.Duration)
			seek(v0 + float64(x-zoomKeysWidth)/gridW*(v1-v0))
		}
		z.dragged = false
	}

	box.Draw(func(p *ui.Painter, r ui.Rect) {
		pos := -1.0
		playing := false
		if pl := a.scoreClock(); pl != nil {
			pos, playing = pl.Position().Seconds(), pl.Playing()
		}
		v0, v1 := z.zoomView(pos, res.Duration)
		grid := ui.Rect{X: r.X + zoomKeysWidth, Y: r.Y + zoomRulerH, W: r.W - zoomKeysWidth, H: r.H - zoomRulerH}
		xOf := func(sec float64) float32 { return grid.X + float32((sec-v0)/(v1-v0))*grid.W }
		rowH := float32(z.rowH)

		// rowTop 是音高 m 所在行的上沿（行高 rowH，顶行的中心是音高 z.top）。
		rowTop := func(m int) float32 { return grid.Y + float32(z.top-float64(m))*rowH - rowH/2 }
		// 可见的音高范围（多算一行，保证边缘的白键画全）。
		hiPitch := min(int(math.Ceil(z.top))+1, 127)
		loPitch := max(int(math.Floor(z.top-float64(grid.H/rowH)))-1, 0)

		// 网格背景：白键行浅、黑键行深（Cubase 的配色），行间细线，每个 C 下沿画八度线；鼓按 5 行交替。
		p.Clip(grid, 0, func() {
			p.Fill(grid, rollPaper.white, 0)
			switch {
			case pitched:
				for m := hiPitch; m >= loPitch; m-- {
					y := rowTop(m)
					if isBlackKey(m) {
						p.Fill(ui.Rect{X: grid.X, Y: y, W: grid.W, H: rowH}, rollPaper.black, 0)
					}
					line := rollPaper.rowLine
					if m%12 == 0 {
						line = rollPaper.cLine
					}
					p.Line(grid.X, y+rowH, grid.X+grid.W, y+rowH, 1, line)
				}
			case tr.Drum:
				dh := grid.H / float32(len(drumRowNames))
				for k := range drumRowNames {
					y := grid.Y + grid.H - float32(k+1)*dh
					if k%2 == 1 {
						p.Fill(ui.Rect{X: grid.X, Y: y, W: grid.W, H: dh}, rollPaper.black, 0)
					}
					p.Line(grid.X, y, grid.X+grid.W, y, 1, rollPaper.rowLine)
				}
			}
			// 拍线与小节线。
			for _, bt := range res.Beats {
				if bt >= v0 && bt < v1 {
					p.Line(xOf(bt), grid.Y, xOf(bt), grid.Y+grid.H, 1, rollPaper.beat)
				}
			}
			for _, d := range res.Downbeats {
				if d >= v0 && d < v1 {
					p.Line(xOf(d), grid.Y, xOf(d), grid.Y+grid.H, 1, rollPaper.bar)
				}
			}
			// 内容：原曲波形，或音符块。
			switch {
			case z.track == score.TrackOriginal:
				rollWaveform(p, grid, pv, v0, v1, rollPaper.wave)
			case tr.Drum:
				zoomNotes(p, tr, v0, v1, pos, color, xOf, func(m int) (float32, float32) {
					dh := grid.H / float32(len(drumRowNames))
					return grid.Y + grid.H - float32(drumRow(m)+1)*dh + dh*0.2, dh * 0.6
				})
			default:
				zoomNotes(p, tr, v0, v1, pos, color, xOf, func(m int) (float32, float32) {
					return rowTop(m) + 1, max(rowH-2, 2)
				})
			}
			if pos >= v0 && pos < v1 {
				p.Fill(ui.Rect{X: xOf(pos) - 1, Y: grid.Y, W: 2, H: grid.H}, rollPaper.playhead, 1)
			}
		})

		// 标尺：深色底、浅色小节号（与乐谱的编号一致），写在小节起点处；起点在窗口左侧之外的小节也写，编号随之移出。
		p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W, H: zoomRulerH}, rollPaper.ruler, 0)
		p.Clip(ui.Rect{X: grid.X, Y: r.Y, W: grid.W, H: zoomRulerH}, 0, func() {
			for _, bt := range res.Beats {
				if bt >= v0 && bt < v1 {
					p.Line(xOf(bt), r.Y+zoomRulerH-4, xOf(bt), r.Y+zoomRulerH, 1, rollPaper.rulerText.Alpha(0.5))
				}
			}
			for _, m := range measures {
				if m.End >= v0 && m.Start < v1 {
					p.Line(xOf(m.Start), r.Y+zoomRulerH-10, xOf(m.Start), r.Y+zoomRulerH, 1, rollPaper.rulerText)
					p.Text(xOf(m.Start)+4, r.Y+4, fmt.Sprint(m.Number), 11, rollPaper.rulerText)
				}
			}
			if pos >= v0 && pos < v1 {
				p.Fill(ui.Rect{X: xOf(pos) - 1, Y: r.Y, W: 2, H: zoomRulerH}, rollPaper.rulerText, 1)
			}
		})

		// 左侧键盘：与真实钢琴相同，白键等分八度、占满宽度，黑键短（60% 宽）且压在白键之上；C 标音名。
		// 鼓写鼓件名称，原曲写「波形」。
		keys := ui.Rect{X: r.X, Y: grid.Y, W: zoomKeysWidth - 1, H: grid.H}
		p.Fill(ui.Rect{X: r.X, Y: r.Y, W: zoomKeysWidth, H: zoomRulerH}, rollPaper.ruler, 0)
		p.Fill(keys, rollPaper.keyWhite, 0)
		p.Clip(keys, 0, func() {
			switch {
			case pitched:
				drawKeyboard(p, keys, loPitch, hiPitch, rowH, rowTop)
			case tr.Drum:
				dh := grid.H / float32(len(drumRowNames))
				for k, name := range drumRowNames {
					y := grid.Y + grid.H - float32(k+1)*dh
					p.Line(keys.X, y, keys.X+keys.W, y, 1, rollPaper.keyEdge)
					p.Text(keys.X+8, y+(dh-12)/2, name, 12, rollPaper.keyText)
				}
			default:
				p.Text(keys.X+8, grid.Y+grid.H/2-6, "波形", 12, rollPaper.keyText)
			}
		})
		p.Line(grid.X, r.Y, grid.X, r.Y+r.H, 1, rollPaper.keyEdge)
		if playing {
			p.AnimationFrame()
		}
	})
}

// drawKeyboard 在 keys 中画 [lo, hi] 音高的钢琴键盘。行的位置与网格一致（rowTop）。白键等分一个八度：
// 相邻黑键时边界在黑键行的中线，否则（E–F、B–C）在行的边界；黑键只占自己那一行，宽 60%，画在白键之后。
func drawKeyboard(p *ui.Painter, keys ui.Rect, lo, hi int, rowH float32, rowTop func(int) float32) {
	for m := lo; m <= hi; m++ {
		if isBlackKey(m) {
			continue
		}
		top, bottom := rowTop(m), rowTop(m)+rowH // 屏幕上沿对应更高的音
		if m+1 <= 127 && isBlackKey(m+1) {
			top = rowTop(m+1) + rowH/2
		}
		if m-1 >= 0 && isBlackKey(m-1) {
			bottom = rowTop(m-1) + rowH/2
		}
		p.Fill(ui.Rect{X: keys.X, Y: top, W: keys.W, H: bottom - top}, rollPaper.keyWhite, 0)
		p.Line(keys.X, bottom, keys.X+keys.W, bottom, 1, rollPaper.keyEdge)
		if m%12 == 0 && bottom-top >= 12 {
			p.Text(keys.X+keys.W-26, bottom-13, midiNoteName(m), 10, rollPaper.keyText)
		}
	}
	for m := lo; m <= hi; m++ {
		if isBlackKey(m) {
			p.Fill(ui.Rect{X: keys.X, Y: rowTop(m), W: keys.W * 0.6, H: rowH}, rollPaper.keyBlack, 1.5)
		}
	}
	p.Line(keys.X+keys.W, keys.Y, keys.X+keys.W, keys.Y+keys.H, 1, rollPaper.keyEdge)
}

// zoomNotes 画 [v0, v1) 内的音符块：yOf 给出音高所在行的上沿与块高；正在发声的描边加粗，宽度够时写音名。
func zoomNotes(p *ui.Painter, tr score.Track, v0, v1, pos float64, color ui.Color,
	xOf func(float64) float32, yOf func(int) (float32, float32)) {
	end := sort.Search(len(tr.Notes), func(k int) bool { return tr.Notes[k].Start >= v1 })
	for _, n := range tr.Notes[:end] {
		if n.End < v0 {
			continue
		}
		y, h := yOf(n.MIDI)
		x0, x1 := xOf(max(n.Start, v0)), xOf(min(n.End, v1))
		w := max(x1-x0, 3)
		rollNote(p, ui.Rect{X: x0, Y: y, W: w, H: h}, color, false, pos >= n.Start && pos < n.End, 2)
		if !tr.Drum && w >= 30 && h >= 11 {
			p.Text(x0+4, y+(h-10)/2, midiNoteName(n.MIDI), 10, ui.RGB(20, 20, 20))
		}
	}
}

// wheel 处理画布上的滚轮（ev 的坐标相对画布，gridW 为网格宽度，pos 为播放位置）。返回是否处理了。
func (z *zoomState) wheel(ev ui.InputEvent, gridW, pos, duration float64, pitched bool) bool {
	if ev.Kind != ui.InputScroll {
		return false
	}
	d := float64(ev.DY)
	if d == 0 {
		d = float64(ev.DX)
	}
	v0, v1 := z.zoomView(pos, duration)
	at := v0 + float64(ev.X-zoomKeysWidth)/gridW*(v1-v0) // 指针处的时刻
	ctrl, shift := ev.Mods&ui.Ctrl != 0, ev.Mods&ui.Shift != 0
	switch {
	case ctrl && shift:
		span := clampF(z.span*wheelFactor(d), zoomMinSpan, max(duration, zoomMinSpan*2))
		z.start, z.span = at-(at-v0)*span/(v1-v0), span
	case ctrl:
		if pitched {
			pitch := z.top - float64(ev.Y-zoomRulerH)/z.rowH + 0.5 // 指针处的音高（连续值）
			z.rowH = clampF(z.rowH/wheelFactor(d), zoomMinRowH, zoomMaxRowH)
			z.top = pitch + float64(ev.Y-zoomRulerH)/z.rowH - 0.5
		}
	case shift:
		z.start, z.follow = v0+d/gridW*(v1-v0), false
	case pitched:
		z.top -= d / z.rowH
	}
	return true
}

// wheelFactor 把一次滚轮滚动量（DIP，向下为正）换成缩放比例：向下缩小（比例 > 1 时可见范围变大），
// 向上放大。每 120 DIP（常见鼠标的一格）约 1.35 倍；触控板的小步滚动连续变化。
func wheelFactor(d float64) float64 { return math.Exp(d / 400) }

func clampF(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

func isBlackKey(m int) bool {
	switch m % 12 {
	case 1, 3, 6, 8, 10:
		return true
	}
	return false
}
