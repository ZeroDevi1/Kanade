package main

import (
	"fmt"
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
}

func newRollState() rollState { return rollState{window: 1, follow: true} }

// rollPalette 是各音轨音符的颜色（原曲之外按音轨序号循环取用），色相分散，深浅主题下都看得清。
var rollPalette = []ui.Color{
	ui.Hex("#4cc2ff"), ui.Hex("#ff9f68"), ui.Hex("#6ccb5f"), ui.Hex("#c58af9"), ui.Hex("#f7c948"),
	ui.Hex("#ff6b9a"), ui.Hex("#3dd6c6"), ui.Hex("#9aa7ff"), ui.Hex("#e8a87c"), ui.Hex("#a3d94b"),
}

func rollColor(i int) ui.Color { return rollPalette[(i-1+len(rollPalette))%len(rollPalette)] }

// rollView 返回 pos 时卷帘显示的时间范围 [v0, v1)（秒）。
func (r *rollState) rollView(pos, duration float64) (v0, v1 float64) {
	w := rollWindows[r.window].sec
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
func (a *app) scoreRollCard(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	s := &a.score.sheet
	r := &s.roll
	pv := &a.score.preview
	available := s.trackAvailable()
	audible := s.mix.Audible(available)
	changed := false
	rollTick(c, pv)
	card(c, tk, "多轨卷帘", func() {
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
			ui.Segmented(c, &r.window, titles(rollWindows, func(w rollWindow) string { return w.title })...).Label("可视范围")
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
		if w := rollWindows[r.window].sec; !r.follow && w > 0 && res.Duration > w {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "位置").FontSize(12).TextColor(t.TextMuted)
				r.start = v0
				ui.Slider(c, &r.start, 0, res.Duration-w).Label("卷帘位置").Grow(1)
			})
		}
		ui.Column(c).Gap(2).AlignItems(ui.Stretch).Children(func() {
			for i, tr := range s.tracks {
				ui.Row(c.Key(fmt.Sprintf("roll-%d", i))).Gap(8).Height(rollLaneHeight).AlignItems(ui.Stretch).Children(func() {
					if a.rollHeader(c, tk, i, tr, available[i], audible[i]) {
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
	if changed {
		a.applyMix(res)
	}
}

// rollHeader 是一条音轨左侧的栏：色块、名称与音符概况、静音 / 独奏与音量。返回混音设置是否被改动。
func (a *app) rollHeader(c *ui.Context, tk *fluent.Tokens, i int, tr score.Track, available, audible bool) (changed bool) {
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
		ui.Box(c).Width(4).Height(rollLaneHeight - 12).Radius(2).Background(swatch)
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				name := ui.Text(c, tr.Title).FontSize(13).SingleLine().Shrink(1).MinWidth(0)
				if !audible {
					name.TextColor(t.TextMuted)
				}
				ui.Text(c, rollSummary(tr)).FontSize(11).Font("monospace").TextColor(t.TextMuted).SingleLine()
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
	box := ui.Box(c.Key("lane")).Grow(1).MinWidth(0).Radius(4).Background(tk.Card).Border(1, tk.CardStroke).
		Cursor(ui.CursorPointer).Label(tr.Title + "卷帘").Clip()
	if box.Pressed() {
		if x, _, _ := box.PointerPosition(); box.Bounds().W > 0 {
			seek(v0 + float64(x/box.Bounds().W)*(v1-v0))
		}
	}
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
				p.Line(xOf(b), rect.Y, xOf(b), rect.Y+rect.H, 1, t.TextMuted.Alpha(0.08))
			}
		}
		for _, d := range res.Downbeats {
			if d >= v0 && d < v1 {
				p.Line(xOf(d), rect.Y, xOf(d), rect.Y+rect.H, 1, t.TextMuted.Alpha(0.25))
			}
		}

		if i == score.TrackOriginal {
			rollWaveform(p, rect, pv, v0, v1, color.Alpha(0.6))
		} else if rows > 0 {
			rowH := (rect.H - 6) / float32(rows)
			noteH := max(3, min(rowH, 8))
			alpha := float32(0.8)
			if !audible {
				alpha = 0.4
			}
			// 音符按开始时间排序：开始晚于窗口终点的都不用看。
			end := sort.Search(len(tr.Notes), func(k int) bool { return tr.Notes[k].Start >= v1 })
			for _, n := range tr.Notes[:end] {
				if n.End < v0 {
					continue
				}
				y := rect.Y + 3 + float32(rows-1-rowOf(n.MIDI))*rowH + (rowH-noteH)/2
				x0, x1 := xOf(max(n.Start, v0)), xOf(min(n.End, v1))
				fill := color.Alpha(alpha)
				if pos >= n.Start && pos < n.End && audible {
					fill = color // 正在发声：不透明并加一圈描边
					p.Stroke(ui.Rect{X: x0 - 1, Y: y - 1, W: max(x1-x0, 2) + 2, H: noteH + 2}, t.Text.Alpha(0.6), 2, 1)
				}
				p.Fill(ui.Rect{X: x0, Y: y, W: max(x1-x0, 2), H: noteH}, fill, 1.5)
			}
		}

		if pos >= v0 && pos < v1 {
			x := xOf(pos)
			p.Fill(ui.Rect{X: x - 1, Y: rect.Y, W: 2, H: rect.H}, t.Text, 1)
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
