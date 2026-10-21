package main

import (
	"fmt"
	"math"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 乐谱视图与播放音源。
const (
	sheetJazz  = iota // 爵士谱（lead sheet）
	sheetStaff        // 五线谱
)

const (
	playOriginal = iota // 原曲
	playChords          // 和弦伴奏
	playBoth            // 原曲 + 和弦伴奏
)

// symbolStyles 是和弦记号写法的选项（标题 → score.Symbol* 键）。
var symbolStyles = []struct{ Key, Title string }{
	{score.SymbolJazz, "爵士记号（CΔ7、C-7）"},
	{score.SymbolStandard, "常规记号（Cmaj7、Cm7）"},
	{score.SymbolRoman, "级数（I、vi、IV、V）"},
}

// 每行（每个系统）的小节数。
const measuresPerRow = 4

// sheetState 是乐谱卡片的状态，属于 scoreState。合成的伴奏按分析结果缓存，切换页面不丢失。
type sheetState struct {
	view     int
	style    string // symbolStyles 的 Title
	follow   bool   // 播放时自动滚动到当前行
	play     int    // playOriginal / playChords / playBoth
	lastRow  int    // 上一帧所在的行，用于只在换行时滚动
	lastView int    // 上一帧的视图；切换视图后立即滚到当前行

	synthFor  *score.Analysis
	synthBusy bool
	chords    *audio.Sound // 和弦伴奏
	both      *audio.Sound // 原曲 + 和弦伴奏
	synthErr  string
}

func newSheetState() sheetState {
	return sheetState{style: symbolStyles[0].Title, follow: true, lastRow: -1}
}

func (s *sheetState) styleKey() string {
	for _, x := range symbolStyles {
		if x.Title == s.style {
			return x.Key
		}
	}
	return score.SymbolJazz
}

// scorePlaySource 是试听卡片中的音源切换：和弦伴奏在后台合成，完成后保持播放位置切换过去。
func (a *app) scorePlaySource(c *ui.Context, res *score.Analysis) {
	t := c.Theme()
	s := &a.score.sheet
	pv := &a.score.preview
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &s.play, "原曲", "和弦伴奏", "原曲 + 和弦").Label("播放音源").Changed() {
			a.applyPlaySource(res)
		}
		switch {
		case s.synthBusy:
			ui.Spinner(c)
			ui.Text(c, "正在合成和弦伴奏…").FontSize(12).TextColor(t.TextMuted)
		case s.synthErr != "":
			ui.Text(c, s.synthErr).FontSize(12).TextColor(t.Danger)
		case s.play != playOriginal:
			ui.Text(c, "按谱面弹奏：低音为根音，高音声部为密集排列，每小节重新弹奏。").FontSize(12).TextColor(t.TextMuted)
		}
	})
	// 切换到别的分析结果（或重新载入原曲）后，合成缓存失效，回到原曲。
	if s.synthFor != nil && s.synthFor != res {
		s.synthFor, s.chords, s.both = nil, nil, nil
		if s.play != playOriginal {
			a.applyPlaySource(res)
		}
	}
	if pv.player == pv.original && s.play != playOriginal && !s.synthBusy && s.chords != nil {
		a.applyPlaySource(res) // 原曲刚被重新载入
	}
}

// applyPlaySource 按 sheet.play 切换播放器音源；需要时先在后台合成。
func (a *app) applyPlaySource(res *score.Analysis) {
	s := &a.score.sheet
	pv := &a.score.preview
	if s.play == playOriginal {
		pv.swap(nil)
		return
	}
	if s.synthFor == res && s.chords != nil {
		if s.play == playChords {
			pv.swap(s.chords)
		} else {
			pv.swap(s.both)
		}
		return
	}
	if s.synthBusy || pv.player == nil {
		return
	}
	s.synthBusy, s.synthErr = true, ""
	orig, rate, ch := pv.samples, pv.rate, pv.ch
	if rate == 0 {
		rate = 44100
	}
	go func() {
		pcm := res.SynthChords(rate)
		var both []float32
		if orig != nil && ch > 0 {
			// 叠加：原曲略降 1.5 dB 给伴奏留出空间；合成音为单声道，两个声道各加一份。
			frames := len(orig) / ch
			both = make([]float32, len(orig))
			for i := 0; i < frames; i++ {
				syn := float32(0)
				if i < len(pcm) {
					syn = pcm[i] * 0.9
				}
				for k := 0; k < ch; k++ {
					v := orig[i*ch+k]*0.84 + syn
					both[i*ch+k] = max(-1, min(1, v))
				}
			}
		}
		a.post(func() {
			s.synthBusy = false
			if len(pcm) == 0 {
				s.synthErr = "没有可合成的和弦"
				return
			}
			s.synthFor, s.chords = res, audio.NewSound(rate, 1, pcm)
			s.both = s.chords
			if both != nil {
				s.both = audio.NewSound(rate, ch, both)
			}
			a.applyPlaySource(res)
		})
	}()
}

// scoreSheetCard 是乐谱：爵士谱或五线谱，随播放高亮并自动跟随，点击跳转。
func (a *app) scoreSheetCard(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	s := &a.score.sheet
	measures := res.Measures()
	card(c, tk, "乐谱", func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Segmented(c, &s.view, "爵士谱", "五线谱").Label("乐谱视图")
			ui.Select(c, &s.style, titles(symbolStyles, func(x struct{ Key, Title string }) string { return x.Title })).
				Label("和弦记号").Width(240)
			ui.Checkbox(c, &s.follow, "播放时跟随")
		})
		if len(measures) == 0 {
			ui.Text(c, "没有检测到小节线，无法排成乐谱。").TextColor(t.TextMuted)
			return
		}
		pos, seek := a.scorePosition()
		cur := -1
		for i, m := range measures {
			if pos >= m.Start && pos < m.End {
				cur = i
			}
		}
		row := cur / measuresPerRow
		if s.view != s.lastView {
			s.lastView, s.lastRow = s.view, -1
		}
		scroll := s.follow && cur >= 0 && row != s.lastRow && a.score.preview.player != nil && a.score.preview.player.Playing()
		if cur >= 0 {
			s.lastRow = row
		}
		if s.view == sheetJazz {
			a.jazzSheet(c, tk, res, measures, pos, cur, scroll, seek)
		} else {
			a.staffSheet(c, tk, res, measures, pos, cur, scroll, seek)
		}
	})
}

// jazzSheet 是爵士谱：每行 4 小节，按拍放置和弦记号，没有新和弦的拍画斜线，与上一小节相同的小节记为 %。
func (a *app) jazzSheet(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, ms []score.Measure, pos float64, cur int,
	scroll bool, seek func(float64)) {
	t := c.Theme()
	style := a.score.sheet.styleKey()
	header := fmt.Sprintf("%s", res.Key.Label)
	if res.Meter.Label != nil {
		header += " · " + *res.Meter.Label
	}
	if res.Tempo.BPM != nil {
		header += fmt.Sprintf(" · ♩ = %.0f", *res.Tempo.BPM)
	}
	ui.Text(c, header).FontSize(13).TextColor(t.TextMuted)
	for r := 0; r < len(ms); r += measuresPerRow {
		rowEl := ui.Row(c.Key(fmt.Sprintf("jazz-row-%d", r))).AlignItems(ui.Stretch).Height(52)
		followRow(rowEl, scroll, cur, r, len(ms))
		rowEl.Children(func() {
			ui.Column(c).Width(44).Shrink(0).Justify(ui.Center).Children(func() {
				ui.Text(c, fmt.Sprint(ms[r].Number)).FontSize(11).Font("monospace").TextColor(t.TextMuted)
				ui.Text(c, score.Clock(ms[r].Start)).FontSize(10).Font("monospace").TextColor(t.TextMuted)
			})
			for i := r; i < min(r+measuresPerRow, len(ms)); i++ {
				a.jazzMeasure(c, tk, res, ms, i, style, pos, i == cur, seek)
			}
			for i := len(ms); i < r+measuresPerRow; i++ {
				ui.Box(c.Key(fmt.Sprintf("jazz-pad-%d", i))).Grow(1).Basis(0)
			}
		})
	}
}

// jazzMeasure 是爵士谱的一个小节：左侧小节线，末小节加终止线。
func (a *app) jazzMeasure(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, ms []score.Measure, i int, style string,
	pos float64, current bool, seek func(float64)) {
	t := c.Theme()
	m := ms[i]
	right := float32(0)
	if i == len(ms)-1 {
		right = 4 // 终止线
	} else if (i+1)%measuresPerRow == 0 {
		right = 1
	}
	box := ui.Row(c.Key(fmt.Sprintf("jazz-m-%d", i))).Grow(1).Basis(0).MinWidth(0).AlignItems(ui.Stretch).
		BorderWidth(0, right, 0, 1).BorderColor(t.TextMuted.Alpha(0.6))
	if current {
		box.Background(tk.SubtleHover)
	}
	box.Children(func() {
		if m.Simile {
			b := ui.ButtonBase(c.Key(fmt.Sprintf("jazz-sim-%d", i))).Grow(1).AlignItems(ui.Center).Justify(ui.Center).
				Label(fmt.Sprintf("第 %d 小节，同上一小节", m.Number))
			b.Children(func() { ui.Text(c, "%").FontSize(22).TextColor(t.TextMuted) })
			if b.Clicked() {
				seek(m.Start)
			}
			return
		}
		for j, bt := range m.Beats {
			on := pos >= bt.Time && pos < bt.End
			b := ui.ButtonBase(c.Key(fmt.Sprintf("jazz-b-%d-%d", i, j))).Grow(1).Basis(0).MinWidth(0).
				AlignItems(ui.Center).Justify(ui.Center)
			if on {
				b.Background(t.Accent.Alpha(0.18))
			} else if b.Hovered() {
				b.Background(tk.SubtlePressed)
			}
			label, sym := "/", ""
			switch {
			case bt.Starts:
				sym = score.Symbol(bt.Chord, style, res.Key.TonicPC)
				label = sym
			case bt.Chord.Label == "" && (j == 0 || m.Beats[j-1].Chord.Label != ""):
				label = "N.C."
			}
			b.Label(fmt.Sprintf("第 %d 小节第 %d 拍 %s", m.Number, j+1, bt.Chord.Label))
			b.Children(func() {
				txt := ui.Text(c, label).SingleLine()
				if sym != "" {
					txt.FontSize(17).FontWeight(600)
					if on {
						txt.TextColor(t.Accent)
					}
				} else {
					txt.FontSize(15).TextColor(t.TextMuted.Alpha(0.7))
				}
			})
			if b.Clicked() {
				seek(bt.Time)
			}
		}
	})
}

// 五线谱的版面（像素）：线距、两谱表的位置与系统高度。
const (
	staffGap     = 8.0                        // 线距
	trebleTop    = 46.0                       // 高音谱表第一线
	trebleBottom = trebleTop + 4*staffGap     // 高音谱表第五线（E4）
	bassTop      = trebleBottom + 6*staffGap  // 低音谱表第一线（A3）
	bassBottom   = bassTop + 4*staffGap       // 低音谱表第五线（G2）
	systemHeight = bassBottom + 5*staffGap    // 留出下加线空间
	staffHeader  = 92.0                       // 谱号与调号区宽度
	trebleBase   = 30                         // E4 的音级序号（第五线）
	bassBase     = 18                         // G2 的音级序号
	middleC      = 60                         // 低于中央 C 的音写在低音谱表
	headW, headH = float32(9.5), float32(7.0) // 符头
)

// 调号升降号在高音谱表上的音级位置（低音谱表低两个八度，即 −14），按升号 / 降号的书写顺序。
var (
	sharpSteps = []int{38, 35, 39, 36, 33, 37, 34} // F5 C5 G5 D5 A4 E5 B4
	flatSteps  = []int{34, 37, 33, 36, 32, 35, 31} // B4 E5 A4 D5 G4 C5 F4
)

// stepY 返回音级在系统中的纵坐标：高音或低音谱表。
func stepY(step int, treble bool) float32 {
	if treble {
		return float32(trebleBottom - float64(step-trebleBase)*staffGap/2)
	}
	return float32(bassBottom - float64(step-bassBase)*staffGap/2)
}

// staffSheet 是五线谱：大谱表，每行 4 小节；高音谱表为和弦的密集排列，低音谱表为根音，按占的拍数决定时值。
func (a *app) staffSheet(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, ms []score.Measure, pos float64, cur int,
	scroll bool, seek func(float64)) {
	style := a.score.sheet.styleKey()
	sig := score.KeySignature(res.Key.TonicPC, res.Key.Mode)
	if res.Key.Flats && sig == 6 {
		sig = -6 // F♯ / G♭ 大调按拼写选择升号或降号
	}
	// 声部进行跨小节连续：先按顺序算出每个事件的配置。
	type ev struct {
		score.Event
		v  score.Voicing
		ok bool
	}
	events := make([][]ev, len(ms))
	var prev []int
	for i, m := range ms {
		for _, e := range m.Events() {
			v, ok := score.ChordVoicing(e.Chord, prev)
			if ok {
				prev = v.MIDIs()
			}
			events[i] = append(events[i], ev{e, v, ok})
		}
	}
	for r := 0; r < len(ms); r += measuresPerRow {
		rowEl := ui.Row(c.Key(fmt.Sprintf("staff-row-%d", r))).Height(systemHeight).AlignItems(ui.Stretch)
		followRow(rowEl, scroll, cur, r, len(ms))
		rowEl.Children(func() {
			staffHeaderBox(c, sig, ms[r].Number)
			n := min(measuresPerRow, len(ms)-r)
			area := ui.Box(c.Key(fmt.Sprintf("staff-area-%d", r))).Grow(1).MinWidth(0).Cursor(ui.CursorPointer)
			// 点击跳到所点位置所在的拍。
			if area.Clicked() {
				if x, _, ok := area.PointerPosition(); ok {
					w := area.Bounds().W
					mi := min(int(x/w*measuresPerRow), n-1)
					m := ms[r+mi]
					fx := float64(x/w*measuresPerRow) - float64(mi)
					bi := min(int(fx*float64(len(m.Beats))), len(m.Beats)-1)
					seek(m.Beats[max(bi, 0)].Time)
				}
			}
			theme := c.Theme()
			area.Draw(func(p *ui.Painter, rc ui.Rect) {
				ink := theme.Text
				faint := theme.TextMuted.Alpha(0.6)
				mw := rc.W / measuresPerRow
				for _, top := range []float64{trebleTop, bassTop} {
					for k := 0; k < 5; k++ {
						y := rc.Y + float32(top+float64(k)*staffGap)
						p.Line(rc.X, y, rc.X+mw*float32(n), y, 1, faint)
					}
				}
				for k := 0; k <= n; k++ {
					x := rc.X + mw*float32(k)
					w := float32(1)
					if r+k == len(ms) {
						w = 3 // 终止线
					}
					p.Line(x, rc.Y+trebleTop, x, rc.Y+bassBottom, w, faint)
				}
				for k := 0; k < n; k++ {
					i := r + k
					m := ms[i]
					x0 := rc.X + mw*float32(k)
					if i == cur {
						p.Fill(ui.Rect{X: x0 + 1, Y: rc.Y + trebleTop - 18, W: mw - 2, H: bassBottom - trebleTop + 36}, theme.Accent.Alpha(0.07), 4)
						// 播放线：按拍插值。
						if fx := measureFraction(m, pos); fx >= 0 {
							x := x0 + mw*float32(fx)
							p.Line(x, rc.Y+trebleTop-14, x, rc.Y+bassBottom+14, 1.5, theme.Danger.Alpha(0.7))
						}
					}
					for _, e := range events[i] {
						if !e.ok {
							continue
						}
						x := x0 + mw*(float32(e.FirstBeat)+0.4)/float32(len(m.Beats))
						color := ink
						if pos >= e.Start && pos < e.End {
							color = theme.Accent
						}
						var treble, bass []score.Note
						for _, nt := range append([]score.Note{e.v.Bass}, e.v.Upper...) {
							if nt.MIDI >= middleC {
								treble = append(treble, nt)
							} else {
								bass = append(bass, nt)
							}
						}
						drawChordNotes(p, rc, x, treble, true, e.Beats, color, faint)
						drawChordNotes(p, rc, x, bass, false, e.Beats, color, faint)
					}
				}
			})
			// 和弦记号与临时记号是文字，放在绘图区上方的绝对定位层（按比例定位，随宽度伸缩）。
			area.Children(func() {
				for k := 0; k < n; k++ {
					i := r + k
					m := ms[i]
					shown := map[int]int{} // 本小节已出现的临时记号：音级序号 → 变化
					keyAcc := score.KeyAccidentals(sig)
					for _, e := range events[i] {
						if !e.ok {
							continue
						}
						frac := (float32(k) + (float32(e.FirstBeat)+0.4)/float32(len(m.Beats))) / measuresPerRow
						sym := score.Symbol(e.Chord, style, res.Key.TonicPC)
						on := pos >= e.Start && pos < e.End
						ui.Box(c.Key(fmt.Sprintf("sym-%d-%d", i, e.FirstBeat))).Absolute().LeftPercent(frac * 100).Top(4).
							Children(func() {
								txt := ui.Text(c, sym).FontSize(14).FontWeight(600).NoWrap()
								if on {
									txt.TextColor(theme.Accent)
								}
							})
						for _, nt := range append([]score.Note{e.v.Bass}, e.v.Upper...) {
							letter := ((nt.Step % 7) + 7) % 7
							want := keyAcc[letter]
							if prev, ok := shown[nt.Step]; ok {
								want = prev
							}
							if nt.Accidental == want {
								continue
							}
							shown[nt.Step] = nt.Accidental
							y := stepY(nt.Step, nt.MIDI >= middleC)
							ui.Text(c.Key(fmt.Sprintf("acc-%d-%d-%d", i, e.FirstBeat, nt.MIDI)), score.AccidentalSign(nt.Accidental)).
								Absolute().LeftPercent(frac*100).Top(y-11).Margin(0, 0, 0, -15).FontSize(15).NoWrap()
						}
					}
				}
			})
		})
	}
}

// followRow 在播放换行时把当前行与下一行都滚进视野：当前行不贴在可视区底边，能看到接下来的内容；
// 往回跳转时向上滚到当前行。只在换行的那一帧滚动，否则会妨碍用户自己滚动。
func followRow(row ui.Element, scroll bool, cur, first, total int) {
	if !scroll {
		return
	}
	curRow, thisRow := cur/measuresPerRow, first/measuresPerRow
	lastRow := (total - 1) / measuresPerRow
	if thisRow == min(curRow+1, lastRow) {
		row.ScrollIntoView()
	}
	if thisRow == curRow {
		row.ScrollIntoView()
	}
}

// measureFraction 返回 pos 在小节内的位置（0–1，按拍均分插值）；不在小节内时为 −1。
func measureFraction(m score.Measure, pos float64) float64 {
	for j, b := range m.Beats {
		if pos >= b.Time && pos < b.End {
			return (float64(j) + (pos-b.Time)/math.Max(b.End-b.Time, 1e-6)) / float64(len(m.Beats))
		}
	}
	return -1
}

// drawChordNotes 画一组同时发声的音（同一谱表）：加线、符头、符干与附点。beats 决定时值：
// 4 拍及以上为全音符（空心无符干），3 拍附点二分，2 拍二分（空心有符干），1 拍四分（实心有符干）。
func drawChordNotes(p *ui.Painter, rc ui.Rect, x float32, notes []score.Note, treble bool, beats int, color, faint ui.Color) {
	if len(notes) == 0 {
		return
	}
	base := trebleBase
	if !treble {
		base = bassBase
	}
	topStep := base + 8 // 第一线的音级
	lo, hi := notes[0].Step, notes[0].Step
	for _, n := range notes {
		lo, hi = min(lo, n.Step), max(hi, n.Step)
	}
	// 加线：低于第五线或高于第一线的偶数音级（线上）。
	for s := base - 2; s >= lo; s -= 2 {
		y := rc.Y + stepY(s, treble)
		p.Line(x-headW*0.9, y, x+headW*0.9, y, 1, faint)
	}
	for s := topStep + 2; s <= hi; s += 2 {
		y := rc.Y + stepY(s, treble)
		p.Line(x-headW*0.9, y, x+headW*0.9, y, 1, faint)
	}
	hollow := beats >= 2
	prevStep := -100
	shift := false
	for _, n := range notes {
		// 相差二度的相邻音错开到符干另一侧，避免符头重叠。
		shift = n.Step-prevStep == 1 && !shift
		prevStep = n.Step
		hx := x
		if shift {
			hx += headW
		}
		y := rc.Y + stepY(n.Step, treble)
		r := ui.Rect{X: hx - headW/2, Y: y - headH/2, W: headW, H: headH}
		if hollow {
			p.Stroke(r, color, headH/2, 1.6)
		} else {
			p.Fill(r, color, headH/2)
		}
		if beats == 3 {
			p.Fill(ui.Rect{X: hx + headW/2 + 3, Y: y - 1.5, W: 3, H: 3}, color, 1.5)
		}
	}
	// 符干：高音谱表朝上（符头右侧），低音谱表朝下（符头左侧），不伸进两谱表之间。
	if beats < 4 {
		if treble {
			sx := x + headW/2 - 0.6
			p.Line(sx, rc.Y+stepY(lo, treble), sx, rc.Y+stepY(hi, treble)-3.5*staffGap, 1.2, color)
		} else {
			sx := x - headW/2 + 0.6
			p.Line(sx, rc.Y+stepY(hi, treble), sx, rc.Y+stepY(lo, treble)+3.5*staffGap, 1.2, color)
		}
	}
}

// staffHeaderBox 是每个系统开头的谱号与调号（以及小节号）。
func staffHeaderBox(c *ui.Context, sig, number int) {
	t := c.Theme()
	box := ui.Box(c).Width(staffHeader).Shrink(0)
	box.Draw(func(p *ui.Painter, rc ui.Rect) {
		faint := t.TextMuted.Alpha(0.6)
		for _, top := range []float64{trebleTop, bassTop} {
			for k := 0; k < 5; k++ {
				y := rc.Y + float32(top+float64(k)*staffGap)
				p.Line(rc.X, y, rc.X+rc.W, y, 1, faint)
			}
		}
		// 系统左端的连线与花括号的位置用一条粗线代替。
		p.Line(rc.X+1, rc.Y+trebleTop, rc.X+1, rc.Y+bassBottom, 2, faint)
	})
	box.Children(func() {
		ui.Text(c, fmt.Sprint(number)).Absolute().Left(4).Top(trebleTop - 30).FontSize(10).Font("monospace").TextColor(t.TextMuted)
		// 谱号字形（U+1D11E / U+1D122）的字号按谱表高度标定：高音谱号约跨 7 个线距，低音谱号约跨 3 个。
		ui.Text(c, "𝄞").Absolute().Left(6).Top(trebleTop - 30).FontSize(58).NoWrap()
		ui.Text(c, "𝄢").Absolute().Left(8).Top(bassTop - 15).FontSize(42).NoWrap()
		steps, sign := sharpSteps, "♯"
		if sig < 0 {
			steps, sign = flatSteps, "♭"
		}
		for i := 0; i < min(abs(sig), 7); i++ {
			x := float32(38 + i*7)
			ui.Text(c, sign).Absolute().Left(x).Top(stepY(steps[i], true) - 11).FontSize(15).NoWrap()
			ui.Text(c, sign).Absolute().Left(x).Top(stepY(steps[i]-14, false) - 11).FontSize(15).NoWrap()
		}
	})
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
