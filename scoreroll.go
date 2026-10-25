package main

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 多轨卷帘：原曲、钢琴和弦、钢琴旋律与多乐器转录出的每种乐器各占一行，左侧是音轨的静音 / 独奏 / 音量，
// 右侧是与试听同步滚动的钢琴卷帘（音符块、小节线与播放头），点击或拖动跳转。混音规则与 TrackMix 一致：
// 改动后在后台合成需要发声的音轨（乐器为简化的合成音色，见 score/instrsynth.go），混好后保持播放位置切换过去。

// rollWindow 是卷帘可视范围的一个选项；sec 为 0 表示全曲。
type rollWindow struct {
	title string
	sec   float64
}

var rollWindows = []rollWindow{{"10 秒", 10}, {"20 秒", 20}, {"40 秒", 40}, {"全曲", 0}}

// 卷帘的尺寸（DIP）：每条音轨一行 rollLaneHeight 高，左侧音轨栏 rollHeaderWidth 宽。
const (
	rollLaneHeight  = 64
	rollHeaderWidth = 236
)

// rollState 是卷帘的显示设置，属于 sheetState。
type rollState struct {
	window int     // rollWindows 的序号
	follow bool    // 播放时窗口跟随播放头（播放头在窗口 20% 处）
	start  float64 // 不跟随时窗口的起点（秒）
	// span 是 Ctrl+Shift+滚轮缩放出的可见时长（秒）；0 为按 window 的预设。选择预设时清零。
	span float64
	// laneH 是 Ctrl+滚轮调出的音轨高度（DIP，rollMinLaneH–rollMaxLaneH）；0 为 rollLaneHeight。
	laneH float64

	zoom zoomState // 放大查看（scoreviewer.go）
}

// 卷帘音轨高度的滚轮调节范围（DIP）：下限放得下音轨栏的名称与按钮，上限约为默认的 3 倍。
const (
	rollMinLaneH = 56
	rollMaxLaneH = 200
)

func newRollState() rollState { return rollState{window: 1, follow: true} }

// laneHeight 返回音轨高度。
func (r *rollState) laneHeight() float32 {
	if r.laneH > 0 {
		return float32(r.laneH)
	}
	return rollLaneHeight
}

// visibleSpan 返回可见时长（秒），0 为全曲。
func (r *rollState) visibleSpan() float64 {
	if r.span > 0 {
		return r.span
	}
	return rollWindows[r.window].sec
}

// wheel 处理卷帘上的滚轮：Ctrl+Shift 以指针时刻为中心横向缩放（at 为该时刻，frac 为它在窗口中的比例），
// Ctrl 调整音轨高度；不按 Ctrl 时不处理，交给页面滚动。返回是否处理了。
func (r *rollState) wheel(ev ui.InputEvent, at, frac, duration float64) bool {
	if ev.Kind != ui.InputScroll || ev.Mods&ui.Ctrl == 0 {
		return false
	}
	d := float64(ev.DY)
	if d == 0 {
		d = float64(ev.DX)
	}
	if ev.Mods&ui.Shift != 0 {
		span := r.visibleSpan()
		if span <= 0 {
			span = duration
		}
		span = clampF(span*wheelFactor(d), zoomMinSpan, max(duration, zoomMinSpan))
		r.span, r.start = span, at-frac*span
		return true
	}
	r.laneH = clampF(float64(r.laneHeight())/wheelFactor(d), rollMinLaneH, rollMaxLaneH)
	return true
}

// rollPalette 是各音轨音符的颜色（原曲之外按音轨序号循环取用），色相分散，深浅主题下都看得清。
var rollPalette = []ui.Color{
	ui.Hex("#4cc2ff"), ui.Hex("#ff9f68"), ui.Hex("#6ccb5f"), ui.Hex("#c58af9"), ui.Hex("#f7c948"),
	ui.Hex("#ff6b9a"), ui.Hex("#3dd6c6"), ui.Hex("#9aa7ff"), ui.Hex("#e8a87c"), ui.Hex("#a3d94b"),
}

func rollColor(i int) ui.Color { return rollPalette[(i-1+len(rollPalette))%len(rollPalette)] }

// rollPaper 是卷帘画布（多轨卷帘的各轨与放大查看）的配色，参考 Cubase 的 Key Editor：浅灰网格、黑键所在行加深、
// 深灰小节线、浅灰拍线、深色播放头与深色标尺。画布不随应用的深浅主题变化，音符的彩色在浅底上都看得清。
var rollPaper = struct {
	white, black     ui.Color // 白键行 / 黑键行（鼓轨按行交替用这两色）
	rowLine, cLine   ui.Color // 行间分隔线 / 每个 C 下沿的八度线
	beat, bar        ui.Color // 拍线 / 小节线
	playhead         ui.Color
	ruler, rulerText ui.Color
	keyWhite         ui.Color
	keyBlack         ui.Color
	keyEdge, keyText ui.Color
	wave             ui.Color // 原曲波形
	noteEdge         ui.Color // 音符块的描边
}{
	white: ui.RGB(232, 232, 232), black: ui.RGB(208, 208, 208),
	rowLine: ui.RGB(219, 219, 219), cLine: ui.RGB(160, 160, 160),
	beat: ui.RGB(192, 192, 192), bar: ui.RGB(118, 118, 118),
	playhead: ui.RGB(24, 24, 24),
	ruler:    ui.RGB(40, 40, 42), rulerText: ui.RGB(205, 205, 205),
	keyWhite: ui.RGB(244, 244, 244), keyBlack: ui.RGB(30, 30, 30),
	keyEdge: ui.RGB(150, 150, 150), keyText: ui.RGB(70, 70, 70),
	wave:     ui.RGB(96, 96, 96),
	noteEdge: ui.RGBA(0, 0, 0, 0.55),
}

// rollNote 在浅色画布上画一个音符块：彩色填充加深色描边；muted 时淡化，active（正在发声）时描边加粗加深。
func rollNote(p *ui.Painter, r ui.Rect, color ui.Color, muted, active bool, radius float32) {
	fill, edge, width := color, rollPaper.noteEdge, float32(1)
	switch {
	case muted:
		fill, edge = color.Alpha(0.35), rollPaper.noteEdge.Alpha(0.2)
	case active:
		edge, width = rollPaper.playhead, 2
	}
	p.Fill(r, fill, radius)
	p.Stroke(r, edge, radius, width)
}

// rollView 返回 pos 时卷帘显示的时间范围 [v0, v1)（秒）。
func (r *rollState) rollView(pos, duration float64) (v0, v1 float64) {
	w := r.visibleSpan()
	if w <= 0 || duration <= w {
		return 0, max(duration, 1)
	}
	if r.follow && pos >= 0 {
		v0 = min(max(pos-w*0.2, 0), duration-w)
	} else {
		v0 = min(max(r.start, 0), duration-w)
	}
	return v0, v0 + w
}

// scoreRollCard 是多轨卷帘卡片。
func (a *app) scoreRollCard(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) ui.Element {
	t := c.Theme()
	s := &a.score.sheet
	r := &s.roll
	pv := &a.score.preview
	available := s.trackAvailable()
	audible := s.mix.Audible(available)
	changed := false
	rollTick(c, pv)
	el := card(c, tk, "多轨卷帘", func() {
		// 顶栏：播放、可视范围、跟随与合成状态。
		ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
			if p := pv.player; p != nil && !pv.loading {
				label := "播放"
				if p.Playing() {
					label = "暂停"
				}
				if ui.Button(c.Key("roll-play"), label).Width(72).Clicked() {
					if p.Playing() {
						p.Pause()
					} else {
						p.Play()
					}
				}
				ui.Textf(c, "%s / %s", clock(p.Position()), clock(p.Duration())).Font("monospace").FontSize(12).TextColor(t.TextMuted)
			}
			if ui.Segmented(c, &r.window, titles(rollWindows, func(w rollWindow) string { return w.title })...).
				Label("可视范围").Changed() {
				r.span = 0 // 选择预设时放弃滚轮缩放出的时长
			}
			if r.span > 0 {
				ui.Textf(c, "%.0f 秒", r.span).FontSize(12).Font("monospace").TextColor(t.TextMuted).
					Tooltip("Ctrl+Shift+滚轮横向缩放，Ctrl+滚轮调整音轨高度；选择上面的范围恢复预设")
			}
			ui.Checkbox(c, &r.follow, "播放时跟随")
			switch {
			case s.synthBusy:
				ui.Spinner(c)
				ui.Text(c, "正在合成音轨…").FontSize(12).TextColor(t.TextMuted)
			case s.mixPending:
				ui.Spinner(c)
				ui.Text(c, "正在混音…").FontSize(12).TextColor(t.TextMuted)
			case s.mixErr != "":
				ui.Text(c, s.mixErr).FontSize(12).TextColor(t.Danger)
			}
		})
		pos, seek := a.scorePosition()
		v0, v1 := r.rollView(pos, res.Duration)
		// 不跟随且只显示一段时：用滑块移动窗口。
		if w := r.visibleSpan(); !r.follow && w > 0 && res.Duration > w {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "位置").FontSize(12).TextColor(t.TextMuted)
				r.start = v0
				ui.Slider(c, &r.start, 0, res.Duration-w).Label("卷帘位置").Grow(1)
			})
		}
		ui.Column(c).Gap(2).AlignItems(ui.Stretch).Children(func() {
			for i, tr := range s.tracks {
				ui.Row(c.Key(fmt.Sprintf("roll-%d", i))).Gap(8).Height(r.laneHeight()).AlignItems(ui.Stretch).Children(func() {
					var inst *score.Instrument
					if k := i - score.TrackInstruments; k >= 0 && res.Transcription != nil && k < len(res.Transcription.Instruments) {
						inst = &res.Transcription.Instruments[k]
					}
					if a.rollHeader(c, tk, i, tr, inst, available[i], audible[i]) {
						changed = true
					}
					a.rollLane(c, tk, res, i, tr, audible[i], v0, v1, seek)
				})
			}
		})
		switch {
		case res.Transcription == nil:
			ui.Text(c, "分析时勾选「多乐器转录」后，这里会为原曲中的每种乐器各列一条音轨。").FontSize(12).TextColor(t.TextMuted)
		case len(s.tracks) == score.TrackInstruments:
			ui.Text(c, "多乐器转录没有转录出音符。").FontSize(12).TextColor(t.TextMuted)
		default:
			ui.Text(c, "乐器音轨用简化的合成音色播放转录出的音符，用于听出各乐器的旋律与节奏；导出 MIDI 后可在 DAW 中换成真实音色。"+
				"转录由 MuScriptor 完成，其权重为 CC BY-NC 4.0 许可，仅限非商业用途。").FontSize(12).TextColor(t.TextMuted)
		}
	})
	a.scoreZoomDialog(c, tk, res)
	if changed {
		a.applyMix(res)
	}
	return el
}

// rollHeader 是一条音轨左侧的栏：色块、名称与音符概况、静音 / 独奏与音量；inst 非 nil 时（转录出的乐器音轨）另有
// 「改为其他乐器」的菜单。返回混音设置是否被改动。
func (a *app) rollHeader(c *ui.Context, tk *fluent.Tokens, i int, tr score.Track, inst *score.Instrument, available, audible bool) (changed bool) {
	t := c.Theme()
	s := &a.score.sheet
	ui.Row(c).Width(rollHeaderWidth).Shrink(0).Gap(8).AlignItems(ui.Center).Padding(0, 8, 0, 0).Children(func() {
		swatch := t.TextMuted
		if i != score.TrackOriginal {
			swatch = rollColor(i)
		}
		if !audible {
			swatch = swatch.Alpha(0.35)
		}
		ui.Box(c).Width(4).Height(s.roll.laneHeight() - 12).Radius(2).Background(swatch)
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				name := ui.Text(c, tr.Title).FontSize(13).SingleLine().Shrink(1).MinWidth(0)
				if !audible {
					name.TextColor(t.TextMuted)
				}
				ui.Text(c, rollSummary(tr)).FontSize(11).Font("monospace").TextColor(t.TextMuted).SingleLine().Grow(1)
				if inst != nil {
					a.relabelButton(c, tk, *inst)
				}
				zoom := ui.ButtonBase(c.Key("zoom-"+tr.Title)).Size(22, 20).Radius(fluent.ControlRadius).Center().
					Label("放大查看" + tr.Title).Tooltip("放大查看（也可双击卷帘）")
				if zoom.Hovered() {
					zoom.Background(tk.SubtleHover)
				}
				zoom.Children(func() { ui.Icon(c, iconExpand).Size(12, 12) })
				if zoom.Clicked() {
					a.openZoom(i)
				}
			})
			if !available {
				ui.Text(c, "未识别旋律（分析时勾选「识别旋律线」）").FontSize(11).TextColor(t.TextMuted).SingleLine().
					Tooltip("未识别旋律（分析时勾选「识别旋律线」）")
				return
			}
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				if trackButton(c, tk, "M", "静音"+tr.Title, s.mix.Mute[i], t.Warning) {
					s.mix.Mute[i], changed = !s.mix.Mute[i], true
				}
				if trackButton(c, tk, "S", "独奏"+tr.Title, s.mix.Solo[i], t.Accent) {
					s.mix.Solo[i], changed = !s.mix.Solo[i], true
				}
				if ui.Slider(c, &s.mix.Gain[i], 0, 2).Label(tr.Title + "音量").Grow(1).MinWidth(0).Changed() {
					changed = true
				}
			})
		})
	})
	return changed
}

// rollSummary 是音轨的音符概况：音符数与音域（鼓与原曲只有前者或为空）。
func rollSummary(tr score.Track) string {
	if len(tr.Notes) == 0 {
		return ""
	}
	if tr.Drum {
		return fmt.Sprintf("%d 击", len(tr.Notes))
	}
	lo, hi := tr.Notes[0].MIDI, tr.Notes[0].MIDI
	for _, n := range tr.Notes {
		lo, hi = min(lo, n.MIDI), max(hi, n.MIDI)
	}
	return fmt.Sprintf("%d 音 · %s–%s", len(tr.Notes), midiNoteName(lo), midiNoteName(hi))
}

// rollLane 是一条音轨的卷帘：小节线与拍线、音符块（正在发声的高亮）、播放头；原曲轨画这一段的波形。
// 按住时按指针位置跳转（拖动即擦洗）。
func (a *app) rollLane(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, i int, tr score.Track, audible bool,
	v0, v1 float64, seek func(float64)) {
	t := c.Theme()
	pv := &a.score.preview
	r := &a.score.sheet.roll
	box := ui.Box(c.Key("lane")).Grow(1).MinWidth(0).Radius(4).Background(rollPaper.white).Border(1, tk.CardStroke).
		Cursor(ui.CursorPointer).Label(tr.Title + "卷帘").Clip()
	if box.Pressed() {
		if x, _, _ := box.PointerPosition(); box.Bounds().W > 0 {
			seek(v0 + float64(x/box.Bounds().W)*(v1-v0))
		}
	}
	if box.DoubleClicked() {
		a.openZoom(i)
	}
	w := box.Bounds().W // 上一帧的宽度；回调在构建之外执行，不能再访问 box
	box.HandleInput(func(ev ui.InputEvent) bool {
		if w <= 0 {
			return false
		}
		frac := float64(ev.X / w)
		return r.wheel(ev, v0+frac*(v1-v0), frac, res.Duration)
	})
	rows, rowOf := rollRows(tr)
	color := t.TextMuted
	if i != score.TrackOriginal {
		color = rollColor(i)
	}
	box.Draw(func(p *ui.Painter, rect ui.Rect) {
		// 位置在绘制时读取：播放中每帧重画（AnimationFrame），不必重建视图。
		pos := -1.0
		playing := false
		if pl := pv.player; pl != nil && !pv.loading {
			pos, playing = pl.Position().Seconds(), pl.Playing()
		}
		v0, v1 := r.rollView(pos, res.Duration)
		span := v1 - v0
		xOf := func(sec float64) float32 { return rect.X + float32((sec-v0)/span)*rect.W }

		// 拍线与小节线。
		for _, b := range res.Beats {
			if b >= v0 && b < v1 {
				p.Line(xOf(b), rect.Y, xOf(b), rect.Y+rect.H, 1, rollPaper.beat)
			}
		}
		for _, d := range res.Downbeats {
			if d >= v0 && d < v1 {
				p.Line(xOf(d), rect.Y, xOf(d), rect.Y+rect.H, 1, rollPaper.bar)
			}
		}

		if i == score.TrackOriginal {
			rollWaveform(p, rect, pv, v0, v1, rollPaper.wave)
		} else if rows > 0 {
			rowH := (rect.H - 6) / float32(rows)
			noteH := max(3, min(rowH, 8))
			// 音符按开始时间排序：开始晚于窗口终点的都不用看。
			end := sort.Search(len(tr.Notes), func(k int) bool { return tr.Notes[k].Start >= v1 })
			for _, n := range tr.Notes[:end] {
				if n.End < v0 {
					continue
				}
				y := rect.Y + 3 + float32(rows-1-rowOf(n.MIDI))*rowH + (rowH-noteH)/2
				x0, x1 := xOf(max(n.Start, v0)), xOf(min(n.End, v1))
				rollNote(p, ui.Rect{X: x0, Y: y, W: max(x1-x0, 2), H: noteH}, color, !audible,
					audible && pos >= n.Start && pos < n.End, 1.5)
			}
		}

		if pos >= v0 && pos < v1 {
			x := xOf(pos)
			p.Fill(ui.Rect{X: x - 1, Y: rect.Y, W: 2, H: rect.H}, rollPaper.playhead, 1)
		}
		if playing {
			p.AnimationFrame()
		}
	})
}

// rollRows 返回音轨卷帘的行数与音高到行（从下往上，0 起）的映射：有音高的音轨每个半音一行，覆盖音域；
// 鼓按键位归成 5 行（底鼓、军鼓 / 拍手、通鼓、踩镲、镲片），便于看出节奏型。没有音符时行数为 0。
func rollRows(tr score.Track) (int, func(int) int) {
	if len(tr.Notes) == 0 {
		return 0, nil
	}
	if tr.Drum {
		return 5, drumRow
	}
	lo, hi := tr.Notes[0].MIDI, tr.Notes[0].MIDI
	for _, n := range tr.Notes {
		lo, hi = min(lo, n.MIDI), max(hi, n.MIDI)
	}
	return hi - lo + 1, func(m int) int { return m - lo }
}

// drumRow 把 GM 打击乐键位归到 5 行之一（与 instrsynth.go 的鼓组分类一致）。
func drumRow(k int) int {
	switch {
	case k == 35 || k == 36:
		return 0 // 底鼓
	case k == 37 || k == 38 || k == 39 || k == 40:
		return 1 // 军鼓、边击、拍手
	case k == 41 || k == 43 || k == 45 || k == 47 || k == 48 || k == 50:
		return 2 // 通鼓
	case k == 42 || k == 44 || k == 46:
		return 3 // 踩镲
	default:
		return 4 // 吊镲、叮叮镲与其他
	}
}

// rollWaveform 在 rect 中画原曲 [v0, v1) 秒的波形（每 2 DIP 一列）。
func rollWaveform(p *ui.Painter, rect ui.Rect, pv *audioPreview, v0, v1 float64, color ui.Color) {
	pk := pv.peaks
	if pk == nil || pk.Frames == 0 || pk.SampleRate == 0 {
		return
	}
	mid, half := rect.Y+rect.H/2, rect.H/2-3
	cols := int(rect.W / 2)
	for k := range cols {
		f0 := int((v0 + (v1-v0)*float64(k)/float64(cols)) * float64(pk.SampleRate))
		f1 := int((v0 + (v1-v0)*float64(k+1)/float64(cols)) * float64(pk.SampleRate))
		if f0 >= pk.Frames {
			break
		}
		lo, hi := pk.Range(max(f0, 0), min(max(f1, f0+1), pk.Frames))
		top, bot := mid-min(max(hi, -1), 1)*half, mid-min(max(lo, -1), 1)*half
		if bot-top < 1 {
			top, bot = mid-0.5, mid+0.5
		}
		p.Fill(ui.Rect{X: rect.X + float32(k)*2, Y: top, W: 1.4, H: bot - top}, color, 0)
	}
}

// rollTick 让播放中的卡片每 100 ms 重建一次，更新顶栏的时间（卷帘本身在绘制时逐帧更新）。
func rollTick(c *ui.Context, pv *audioPreview) {
	if p := pv.player; p != nil && p.Playing() {
		c.After(100 * time.Millisecond)
	}
}

// relabelButton 是乐器音轨的「改为…」菜单：常用乐器组直接列出，其余在「其他乐器」子菜单中；改成已有的乐器时合并。
// 鼓只能与鼓互改，所以鼓轨没有这个菜单。
func (a *app) relabelButton(c *ui.Context, tk *fluent.Tokens, inst score.Instrument) {
	if inst.Drum {
		return
	}
	b := ui.ButtonBase(c.Key("relabel-"+inst.Name)).Size(22, 20).Radius(fluent.ControlRadius).Center().
		Label("把" + inst.Title() + "改为其他乐器").Tooltip("识别错了乐器？改为其他乐器（改成已有的乐器时合并）")
	if b.Hovered() {
		b.Background(tk.SubtleHover)
	}
	b.Children(func() { ui.Icon(c, iconSwap).Size(12, 12) })
	item := func(m *ui.Menu, g score.InstrumentGroup) {
		if g.Key == "drums" {
			return
		}
		if m.Item(g.Title).Checked(g.Key == inst.Name).Disabled(g.Key == inst.Name).Chosen() {
			a.relabelInstrument(inst.Name, g.Key)
		}
	}
	b.Menu(func(m *ui.Menu) {
		var rest []score.InstrumentGroup
		for _, g := range score.InstrumentGroups {
			if slices.Contains(instrCommon, g.Key) {
				item(m, g)
			} else {
				rest = append(rest, g)
			}
		}
		m.Separator()
		m.Submenu("其他乐器", func(m *ui.Menu) {
			for _, g := range rest {
				item(m, g)
			}
		})
	})
}

// relabelInstrument 把当前记录转录中的乐器 from 改为 to，并按新的音轨重新开始混音（各音轨的静音、独奏与音量恢复默认）。
func (a *app) relabelInstrument(from, to string) {
	s := &a.score
	if len(s.runs) == 0 {
		return
	}
	if err := s.runs[s.shown].RelabelInstrument(from, to); err != nil {
		s.err = err.Error()
		return
	}
	// 菜单的选择在本帧构建中途回调，本帧其余部分仍按旧音轨构建：混音设置保持旧音轨数，下一帧 useTracks 按新音轨调整。
	s.err = ""
	s.sheet.mix = score.NewTrackMix(len(s.sheet.tracks))
}
