package main

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"unicode/utf8"

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
	lastRow  int    // 上一帧所在的行，用于只在换行时滚动
	lastView int    // 上一帧的视图；切换视图后立即滚到当前行

	// mix 是试听的分轨混音设置，与 tracks 等长（原曲、钢琴和弦、钢琴旋律、各乐器）。
	mix score.TrackMix
	// tracksFor 是 tracks / synthPCM 所对应的分析结果，换了结果时由 useTracks 重建。
	// synthPCM[i] 是第 i 条音轨合成好的单声道采样（synthRate Hz），nil 为还没合成（只在需要发声时合成）。
	tracksFor *score.Analysis
	tracks    []score.Track
	synthPCM  [][]float32
	synthRate int
	synthBusy bool
	// mixVer 在每次请求混音时递增，后台混音完成时只应用最新一次的结果。
	mixVer int
	// mixApplied 是混音结果所基于的原曲播放器：原曲重新载入（播放器变化）后需要重新混音。
	mixApplied *audio.Player
	mixPending bool // 后台混音进行中
	mixErr     string

	roll rollState // 多轨卷帘的显示设置（scoreroll.go）
}

func newSheetState() sheetState {
	return sheetState{style: symbolStyles[0].Title, follow: true, lastRow: -1, roll: newRollState()}
}

func (s *sheetState) styleKey() string {
	for _, x := range symbolStyles {
		if x.Title == s.style {
			return x.Key
		}
	}
	return score.SymbolJazz
}

// useTracks 让音轨缓存对应 res：换了分析结果时重建音轨与混音设置（乐器数可能不同），丢弃已合成的采样。
// 返回是否重建了（此时需要按新结果重新混音）。
func (s *sheetState) useTracks(res *score.Analysis) bool {
	if s.tracksFor == res {
		return false
	}
	s.tracksFor, s.tracks = res, res.Tracks()
	s.synthPCM = make([][]float32, len(s.tracks))
	s.mix.Resize(len(s.tracks))
	return true
}

// trackAvailable 报告各音轨是否可用：原曲总是可用，其余音轨需要有音符（旋律轨需要识别出的旋律线）。
func (s *sheetState) trackAvailable() []bool {
	out := make([]bool, len(s.tracks))
	for i, tr := range s.tracks {
		out[i] = i == score.TrackOriginal || len(tr.Notes) > 0
	}
	return out
}

// syncMix 在每帧构建时调用：换了分析结果，或原曲（重新）载入后还没有为它混过音且混音不是「只有原曲」时，
// 重新应用混音。混音或合成进行中、或上次失败时不重复触发。
func (a *app) syncMix(res *score.Analysis) {
	s := &a.score.sheet
	pv := &a.score.preview
	changed := s.useTracks(res)
	if pv.player != nil && pv.player == pv.original && s.mixApplied != pv.original && !s.mix.OriginalOnly(s.trackAvailable()) &&
		!s.synthBusy && !s.mixPending && s.mixErr == "" {
		changed = true
	}
	if changed {
		a.applyMix(res)
	}
}

// trackButton 是音轨的 M / S 按钮，on 时以 color 填充；返回是否被点击。
func trackButton(c *ui.Context, tk *fluent.Tokens, text, label string, on bool, color ui.Color) bool {
	t := c.Theme()
	b := ui.ButtonBase(c.Key(label)).Size(28, 24).Radius(fluent.ControlRadius).Center().Label(label).
		Border(1, tk.CardStroke)
	switch {
	case on:
		b.Background(color)
	case b.Hovered():
		b.Background(tk.SubtleHover)
	}
	b.Children(func() {
		txt := ui.Text(c, text).FontSize(12).FontWeight(600)
		if on {
			txt.TextColor(t.AccentText)
		}
	})
	return b.Clicked()
}

// applyMix 按当前混音设置切换播放器音源：只有原曲时直接播放原文件；有发声的音轨还没合成时先在后台合成；
// 然后在后台混合并保持播放位置切换。连续改动时只应用最后一次的结果。
func (a *app) applyMix(res *score.Analysis) {
	s := &a.score.sheet
	pv := &a.score.preview
	s.useTracks(res)
	available := s.trackAvailable()
	s.mixErr = ""
	if s.mix.OriginalOnly(available) {
		s.mixVer++ // 作废进行中的混音
		s.mixPending = false
		pv.swap(nil)
		return
	}
	if pv.player == nil {
		return
	}
	rate := pv.rate
	if rate == 0 {
		rate = 44100
	}
	if s.synthRate != rate {
		s.synthPCM, s.synthRate = make([][]float32, len(s.tracks)), rate // 采样率变了，已合成的作废
	}
	var missing []int
	for _, i := range s.mix.Synthesized(available) {
		if s.synthPCM[i] == nil {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		if s.synthBusy {
			return // 合成完成后会再次调用
		}
		s.synthBusy = true
		tracks := s.tracks
		go func() {
			pcm := make([][]float32, len(missing))
			for k, i := range missing {
				pcm[k] = res.TrackPCM(tracks, i, rate)
			}
			a.post(func() {
				s.synthBusy = false
				// 期间换了分析结果或采样率：丢弃（新的请求会自己合成）。
				if s.tracksFor == res && s.synthRate == rate {
					for k, i := range missing {
						s.synthPCM[i] = pcm[k]
						if s.synthPCM[i] == nil {
							s.synthPCM[i] = []float32{} // 合成结果为空（音符都在音频之外）：记为已合成，避免反复请求
						}
					}
				}
				a.applyMix(res)
			})
		}()
		return
	}
	if pv.samples == nil || pv.ch == 0 {
		s.mixErr = "该音频的声道数不支持混音，只能播放原曲"
		return
	}
	s.mixVer++
	s.mixPending = true
	// 混音设置复制一份交给后台：界面线程之后的改动不影响这次混音。
	mix := score.TrackMix{Mute: slices.Clone(s.mix.Mute), Solo: slices.Clone(s.mix.Solo), Gain: slices.Clone(s.mix.Gain)}
	ver, orig, ch, original, synth := s.mixVer, pv.samples, pv.ch, pv.original, slices.Clone(s.synthPCM)
	go func() {
		out := score.MixTracks(orig, ch, synth, mix, available)
		a.post(func() {
			// 期间又改了设置或换了文件时丢弃这次结果（更新的请求会自己完成）。
			if ver != s.mixVer {
				return
			}
			s.mixPending = false
			if pv.original == original {
				pv.swap(audio.NewSound(rate, ch, out))
				s.mixApplied = original
			}
		})
	}()
}

// scoreSheetCard 是乐谱：爵士谱或五线谱，随播放高亮并自动跟随，点击跳转。
// 乐谱放在自己的滚动容器里，长乐谱不会把页面无限拉长：与卷帘并排时填满卡片（与卷帘等高），
// 单列时高度为 sheetViewportMinH。播放跟随（followRow）滚动的是这个容器。
func (a *app) scoreSheetCard(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) ui.Element {
	t := c.Theme()
	s := &a.score.sheet
	measures := res.Measures()
	return card(c, tk, "乐谱", func() {
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
		// Basis(0) + Grow(1)：高度取卡片剩余的空间（并排时卡片被卷帘撑高），不随乐谱内容增长。
		ui.Scroll(c.Key("sheet-scroll")).Basis(0).Grow(1).MinHeight(sheetViewportMinH).Children(func() {
			ui.Column(c).AlignItems(ui.Stretch).Padding(0, 12, 0, 0).Children(func() { // 右侧留出滚动条的位置
				if s.view == sheetJazz {
					a.jazzSheet(c, tk, res, measures, pos, cur, scroll, seek)
				} else {
					a.staffSheet(c, tk, res, measures, pos, cur, scroll, seek)
				}
			})
		})
	})
}

// sheetViewportMinH 是乐谱滚动区的最小高度（DIP）：单列布局时的高度，约 5 行爵士谱或 2 行五线谱。
const sheetViewportMinH = 520

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
		for j := 0; j < len(m.Beats); j++ {
			bt := m.Beats[j]
			// 记号较长（转位、七和弦等）且下一拍延续同一和弦时，占两拍的宽度，避免被截断成「…」。
			span, end := 1, bt.End
			if bt.Starts && j+1 < len(m.Beats) && !m.Beats[j+1].Starts && m.Beats[j+1].Chord.Label == bt.Chord.Label &&
				utf8.RuneCountInString(score.Symbol(bt.Chord, style, res.Key.TonicPC)) > longSymbol {
				span, end = 2, m.Beats[j+1].End
			}
			on := pos >= bt.Time && pos < end
			b := ui.ButtonBase(c.Key(fmt.Sprintf("jazz-b-%d-%d", i, j))).Grow(float32(span)).Basis(0).MinWidth(0).
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
					size := float32(17)
					if span == 1 && utf8.RuneCountInString(sym) > longSymbol {
						size = 13 // 只占一拍的长记号缩小字号
					}
					txt.FontSize(size).FontWeight(600)
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
			j += span - 1
		}
	})
}

// longSymbol 是一拍宽的格子稳妥能完整显示的和弦记号字符数：17 号字、四小节一行、窗口约 1100 DIP 宽时，
// 「D♭Δ7」这样的 4 个字符已会被截断。超过时占两拍或缩小字号。
const longSymbol = 3

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

// staffSheet 是五线谱（大谱表，每行 4 小节）。有旋律线时：高音谱表写旋律（量化到八分音符），低音谱表写和弦
// （根音加和弦音，整体降一个八度）；没有旋律时：高音谱表写和弦的密集排列，低音谱表写根音。和弦记号写在最上方。
func (a *app) staffSheet(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, ms []score.Measure, pos float64, cur int,
	scroll bool, seek func(float64)) {
	style := a.score.sheet.styleKey()
	sig := score.KeySignature(res.Key.TonicPC, res.Key.Mode)
	if res.Key.Flats && sig == 6 {
		sig = -6 // F♯ / G♭ 大调按拼写选择升号或降号
	}
	transpose := res.MelodyTranspose()
	melody := res.MelodyLayout(ms, transpose, res.Key.Flats || sig < 0)
	// 声部进行跨小节连续：先按顺序算出每个和弦事件的配置。
	events := make([][]staffEvent, len(ms))
	var prev []int
	for i, m := range ms {
		for _, e := range m.Events() {
			v, ok := score.ChordVoicing(e.Chord, prev)
			if ok {
				prev = v.MIDIs()
			}
			events[i] = append(events[i], staffEvent{e, v, ok})
		}
	}
	if melody != nil {
		t := c.Theme()
		ui.Text(c, "旋律由主唱识别并量化到八分音符，仅供参考；低音谱表为和弦。").FontSize(12).TextColor(t.TextMuted)
		if ly := res.Melody.Lyrics; ly != nil {
			msg := fmt.Sprintf("歌词 %s：%d / %d 个字对上了音符，音符下的横线表示连音。", filepath.Base(ly.Source), ly.Matched, len(ly.Words))
			if ly.Method == "game" {
				msg = "连音由 GAME 切分。" + msg
			}
			color := t.TextMuted
			if ly.Note != "" {
				msg, color = msg+"对齐时"+ly.Note+"，字的位置可能不准。", t.Warning
			}
			ui.Text(c, msg).FontSize(12).TextColor(color)
		}
	}
	for r := 0; r < len(ms); r += measuresPerRow {
		rowEl := ui.Row(c.Key(fmt.Sprintf("staff-row-%d", r))).Height(systemHeight).AlignItems(ui.Stretch)
		followRow(rowEl, scroll, cur, r, len(ms))
		rowEl.Children(func() {
			staffHeaderBox(c, sig, ms[r].Number, melody != nil && transpose == 12)
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
				faint := theme.TextMuted.Alpha(0.6)
				mw := rc.W / measuresPerRow
				drawStaffLines(p, rc.X, rc.Y, mw*float32(n), faint)
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
						color := theme.Text
						if pos >= e.Start && pos < e.End {
							color = theme.Accent
						}
						treble, bass := e.staffNotes(melody != nil)
						if melody == nil {
							drawChordNotes(p, rc, x, treble, true, e.Beats, color, faint)
						}
						drawChordNotes(p, rc, x, bass, false, e.Beats, color, faint)
					}
					if melody != nil {
						drawMelody(p, rc, x0, mw, melody[i], pos, theme, faint, i+1 < len(ms) && k+1 < n)
					}
				}
			})
			// 和弦记号与临时记号是文字，放在绘图区上方的绝对定位层（按比例定位，随宽度伸缩）。
			area.Children(func() {
				keyAcc := score.KeyAccidentals(sig)
				for k := 0; k < n; k++ {
					i := r + k
					m := ms[i]
					shown := map[[2]int]int{} // 本小节已出现的临时记号：(音级序号, 是否高音谱表) → 变化
					accidental := func(key string, nt score.Note, treble bool, frac float32) {
						letter := ((nt.Step % 7) + 7) % 7
						want := keyAcc[letter]
						if prev, ok := shown[[2]int{nt.Step, b2i(treble)}]; ok {
							want = prev
						}
						if nt.Accidental == want {
							return
						}
						shown[[2]int{nt.Step, b2i(treble)}] = nt.Accidental
						ui.Text(c.Key(key), score.AccidentalSign(nt.Accidental)).Absolute().LeftPercent(frac*100).
							Top(stepY(nt.Step, treble)-11).Margin(0, 0, 0, -15).FontSize(15).NoWrap()
					}
					for _, e := range events[i] {
						if !e.ok {
							continue
						}
						frac := (float32(k) + (float32(e.FirstBeat)+0.4)/float32(len(m.Beats))) / measuresPerRow
						sym := score.Symbol(e.Chord, style, res.Key.TonicPC)
						on := pos >= e.Start && pos < e.End
						ui.Box(c.Key(fmt.Sprintf("sym-%d-%d", i, e.FirstBeat))).Absolute().LeftPercent(frac * 100).Top(2).
							Children(func() {
								txt := ui.Text(c, sym).FontSize(14).FontWeight(600).NoWrap()
								if on {
									txt.TextColor(theme.Accent)
								}
							})
						treble, bass := e.staffNotes(melody != nil)
						if melody == nil {
							for _, nt := range treble {
								accidental(fmt.Sprintf("acc-t-%d-%d-%d", i, e.FirstBeat, nt.MIDI), nt, true, frac)
							}
						}
						for _, nt := range bass {
							accidental(fmt.Sprintf("acc-b-%d-%d-%d", i, e.FirstBeat, nt.MIDI), nt, false, frac)
						}
					}
					if melody != nil {
						mm := melody[i]
						for _, it := range mm.Items {
							if it.Rest {
								continue
							}
							frac := (float32(k) + melodyX(it, mm)) / measuresPerRow
							accidental(fmt.Sprintf("acc-m-%d-%d", i, it.Slot), it.Note, true, frac)
							if it.Lyric != "" {
								// 字居中在符头下方（按一个汉字宽度左移半个字）。
								txt := ui.Text(c.Key(fmt.Sprintf("lyric-%d-%d", i, it.Slot)), it.Lyric).Absolute().LeftPercent(frac*100).
									Top(lyricTop).Margin(0, 0, 0, -6.5).FontSize(13).NoWrap()
								if pos >= it.Start && pos < it.End {
									txt.TextColor(theme.Accent)
								}
							}
						}
					}
				}
			})
		})
	}
}

// staffEvent 是五线谱上一个和弦事件及其配置。
type staffEvent struct {
	score.Event
	v  score.Voicing
	ok bool
}

// staffNotes 把和弦的音分到两行谱表。withMelody 为 true 时高音谱表留给旋律，和弦音整体降一个八度写在低音谱表
// （与根音同音的不重复）；否则以中央 C 为界分开。
func (e staffEvent) staffNotes(withMelody bool) (treble, bass []score.Note) {
	if withMelody {
		bass = append(bass, e.v.Bass)
		for _, u := range e.v.Upper {
			low := u
			low.MIDI -= 12
			low.Step -= 7
			if low.MIDI != e.v.Bass.MIDI {
				bass = append(bass, low)
			}
		}
		slices.SortFunc(bass, func(a, b score.Note) int { return a.Step - b.Step })
		return nil, bass
	}
	for _, nt := range append([]score.Note{e.v.Bass}, e.v.Upper...) {
		if nt.MIDI >= middleC {
			treble = append(treble, nt)
		} else {
			bass = append(bass, nt)
		}
	}
	return treble, bass
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// melodyX 是旋律音在小节内的横向位置（0–1）：按网格位置均分，音符画在所占第一格的中部。
func melodyX(it score.MelodyItem, mm score.MelodyMeasure) float32 {
	return (float32(it.Slot) + 0.45) / float32(max(mm.Slots, 1))
}

// drawStaffLines 画大谱表的十条线。
func drawStaffLines(p *ui.Painter, x, y, w float32, color ui.Color) {
	for _, top := range []float64{trebleTop, bassTop} {
		for k := 0; k < 5; k++ {
			yy := y + float32(top+float64(k)*staffGap)
			p.Line(x, yy, x+w, yy, 1, color)
		}
	}
}

// lyricTop 是歌词文字的顶端（相对系统顶端）：两行谱表之间，避开高音谱表的下行符干与低音谱表上方的加线。
const lyricTop = float32(trebleBottom + 1.9*staffGap)

// 符干与符杠的尺寸。
const (
	stemLen     = float32(3.5 * staffGap) // 标准符干长度（三个半线距）
	minStemLen  = float32(2.5 * staffGap) // 成组时的最短符干
	beamWidth   = float32(4)              // 符杠粗细
	maxBeamRise = float32(staffGap)       // 符杠首尾的最大高差
)

// stemSpec 是一个音的符干：方向、符干末端的纵坐标（绝对坐标），以及是否在符杠组内（组内不画符尾）。
type stemSpec struct {
	up     bool
	endY   float32
	beamed bool
}

// drawMelody 画一个小节的旋律：音符（符头、符干、符尾或符杠、附点、加线）、休止符与连音线。
// tieOut 为 false 时（行末）跨小节的连音线只画到小节线。
func drawMelody(p *ui.Painter, rc ui.Rect, x0, mw float32, mm score.MelodyMeasure, pos float64, theme *ui.Theme,
	faint ui.Color, tieOut bool) {
	xOf := func(it score.MelodyItem) float32 { return x0 + mw*melodyX(it, mm) }
	yOf := func(it score.MelodyItem) float32 { return rc.Y + stepY(it.Note.Step, true) }
	playing := func(it score.MelodyItem) bool { return pos >= it.Start && pos < it.End }

	// 1. 符干：成组的八分音符按符杠统一方向与末端，其余各自按音高决定方向。
	stems := make([]stemSpec, len(mm.Items))
	for k, it := range mm.Items {
		up := it.Note.Step < 34 // 第三线（B4）以下符干朝上
		end := yOf(it) + stemLen
		if up {
			end = yOf(it) - stemLen
		}
		stems[k] = stemSpec{up: up, endY: end}
	}
	for _, g := range mm.BeamGroups() {
		sum := 0
		for _, k := range g {
			sum += mm.Items[k].Note.Step
		}
		up := float32(sum)/float32(len(g)) < 34
		first, last := mm.Items[g[0]], mm.Items[g[len(g)-1]]
		sx := func(it score.MelodyItem) float32 {
			if up {
				return xOf(it) + headW/2 - 0.6
			}
			return xOf(it) - headW/2 + 0.6
		}
		dir := float32(1) // 符干从符头延伸的方向：朝下为 +1
		if up {
			dir = -1
		}
		// 符杠斜率跟随首尾音的高低，高差限制在 maxBeamRise 以内。
		xf, xl := sx(first), sx(last)
		yf, yl := yOf(first)+dir*stemLen, yOf(last)+dir*stemLen
		rise := max(-maxBeamRise, min(maxBeamRise, yl-yf))
		slope := float32(0)
		if xl > xf {
			slope = rise / (xl - xf)
		}
		beamY := func(x float32) float32 { return yf + slope*(x-xf) }
		// 整体平移符杠，保证每根符干不短于 minStemLen。
		shift := float32(0)
		for _, k := range g {
			it := mm.Items[k]
			// 符干末端至少要到 need；v > 0 表示符杠离符头太近（朝上时太低、朝下时太高），需要再移开 v。
			need := yOf(it) + dir*minStemLen
			if v := (beamY(sx(it)) - need) * -dir; v > 0 {
				shift = max(shift, v)
			}
		}
		for _, k := range g {
			it := mm.Items[k]
			stems[k] = stemSpec{up: up, endY: beamY(sx(it)) + dir*shift, beamed: true}
		}
		color := theme.Text
		for _, k := range g {
			if playing(mm.Items[k]) {
				color = theme.Accent
			}
		}
		// 符杠：符干末端连成的平行四边形，粗细朝符头一侧。
		y1, y2 := beamY(xf)+dir*shift, beamY(xl)+dir*shift
		t := -dir * beamWidth
		beam := (&ui.Path{}).MoveTo(xf-0.6, y1).LineTo(xl+0.6, y2).LineTo(xl+0.6, y2+t).LineTo(xf-0.6, y1+t).Close()
		p.FillPath(beam, color)
	}

	// 2. 歌词延长线：一个字的连音音符下方画一条横线，从字后延伸到它最后一个连音音符；字在前一小节时从小节起点画起。
	for idx := 0; idx < len(mm.Items); idx++ {
		it := mm.Items[idx]
		if it.Rest || it.Lyric == "" && idx > 0 {
			continue
		}
		last := -1
		for j := idx + 1; j < len(mm.Items) && !mm.Items[j].Rest && mm.Items[j].Lyric == ""; j++ {
			if mm.Items[j].Slur {
				last = j
			}
		}
		if it.Slur {
			last = max(last, idx)
		}
		if last < 0 {
			continue
		}
		x1, x2 := xOf(it)+7, xOf(mm.Items[last])+headW/2
		if it.Lyric == "" {
			x1 = x0 + 2
		}
		if x2 > x1 {
			y := rc.Y + lyricTop + 15
			p.Line(x1, y, x2, y, 1, theme.Text.Alpha(0.6))
		}
		idx = last
	}

	// 3. 音符、休止符与连音线。
	for idx, it := range mm.Items {
		x := xOf(it)
		color := theme.Text
		if playing(it) {
			color = theme.Accent
		}
		if it.Rest {
			drawRest(p, x, rc.Y, it.Len, color)
			continue
		}
		drawMelodyNote(p, rc, x, it, stems[idx], color, faint)
		if it.TieNext {
			// 连音线画到下一个音符；它在下一小节时画到下一小节第一个音符的位置，行末则画到小节线。
			x1 := x0 + mw
			if idx+1 < len(mm.Items) {
				x1 = xOf(mm.Items[idx+1])
			} else if tieOut {
				x1 = x0 + mw + mw*0.45/float32(max(mm.Slots, 1))
			}
			y := yOf(it)
			dy := float32(6) // 连音线在符干的另一侧
			if !stems[idx].up {
				dy = -6
			}
			path := (&ui.Path{}).MoveTo(x+3, y+dy*0.8).QuadTo((x+x1)/2, y+dy*2.2, x1-3, y+dy*0.8)
			p.StrokePath(path, 1.2, color)
		}
	}
}

// drawMelodyNote 画一个旋律音：加线、符头（二分及以上空心）、附点、符干，以及不在符杠组内的八分音符的符尾。
func drawMelodyNote(p *ui.Painter, rc ui.Rect, x float32, it score.MelodyItem, st stemSpec, color, faint ui.Color) {
	step := it.Note.Step
	y := rc.Y + stepY(step, true)
	for s := trebleBase - 2; s >= step; s -= 2 {
		yy := rc.Y + stepY(s, true)
		p.Line(x-headW*0.9, yy, x+headW*0.9, yy, 1, faint)
	}
	for s := trebleBase + 10; s <= step; s += 2 {
		yy := rc.Y + stepY(s, true)
		p.Line(x-headW*0.9, yy, x+headW*0.9, yy, 1, faint)
	}
	head := ui.Rect{X: x - headW/2, Y: y - headH/2, W: headW, H: headH}
	if it.Len >= 4 {
		p.Stroke(head, color, headH/2, 1.6)
	} else {
		p.Fill(head, color, headH/2)
	}
	if it.Len == 3 || it.Len == 6 {
		p.Fill(ui.Rect{X: x + headW/2 + 3, Y: y - 1.5, W: 3, H: 3}, color, 1.5)
	}
	if it.Len >= 8 {
		return
	}
	sx := x - headW/2 + 0.6
	if st.up {
		sx = x + headW/2 - 0.6
	}
	p.Line(sx, y, sx, st.endY, 1.2, color)
	if it.Len == 1 && !st.beamed {
		// 符尾：从符干末端向符头方向弯出。
		d := float32(1)
		if !st.up {
			d = -1
		}
		e := st.endY
		path := (&ui.Path{}).MoveTo(sx, e).CubeTo(sx+2, e+8*d, sx+9, e+10*d, sx+6, e+20*d)
		p.StrokePath(path, 1.4, color)
	}
}

// drawRest 画休止符（高音谱表）：全休止挂在第四线下，二分休止坐在第三线上，四分休止为折线，八分休止为圆点加斜线；
// 附点时值在右侧加点。字体中通常没有休止符字形，因此用路径绘制。
func drawRest(p *ui.Painter, x, y0 float32, l int, color ui.Color) {
	line := func(k int) float32 { return y0 + float32(trebleTop+float64(k)*staffGap) } // 第 k 线（从上往下 0–4）
	switch {
	case l >= 8:
		p.Fill(ui.Rect{X: x - 5, Y: line(1), W: 10, H: 4}, color, 0)
	case l >= 4:
		p.Fill(ui.Rect{X: x - 5, Y: line(2) - 4, W: 10, H: 4}, color, 0)
	case l >= 2:
		yt := line(1)
		path := (&ui.Path{}).MoveTo(x-2, yt).LineTo(x+3, yt+7).LineTo(x-2, yt+13).LineTo(x+3, yt+19).
			QuadTo(x-5, yt+17, x-1, yt+25)
		p.StrokePath(path, 2, color)
	default:
		yt := line(1) + 4
		p.Fill(ui.Rect{X: x - 3, Y: yt - 2, W: 4, H: 4}, color, 2)
		p.Line(x+3, yt, x-1, yt+16, 1.5, color)
		p.Line(x-3, yt+1, x+3, yt, 1.2, color)
	}
	if l == 3 || l == 6 {
		p.Fill(ui.Rect{X: x + 8, Y: line(2) - 6, W: 3, H: 3}, color, 1.5)
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

// staffHeaderBox 是每个系统开头的谱号与调号（以及小节号）。octaveUp 为 true 时高音谱号下方标「8」：旋律按高八度记谱。
func staffHeaderBox(c *ui.Context, sig, number int, octaveUp bool) {
	t := c.Theme()
	box := ui.Box(c).Width(staffHeader).Shrink(0)
	box.Draw(func(p *ui.Painter, rc ui.Rect) {
		faint := t.TextMuted.Alpha(0.6)
		drawStaffLines(p, rc.X, rc.Y, rc.W, faint)
		// 系统左端的连线与花括号的位置用一条粗线代替。
		p.Line(rc.X+1, rc.Y+trebleTop, rc.X+1, rc.Y+bassBottom, 2, faint)
	})
	box.Children(func() {
		ui.Text(c, fmt.Sprint(number)).Absolute().Left(4).Top(trebleTop - 30).FontSize(10).Font("monospace").TextColor(t.TextMuted)
		// 谱号字形（U+1D11E / U+1D122）的字号按谱表高度标定：高音谱号约跨 7 个线距，低音谱号约跨 3 个。
		ui.Text(c, "𝄞").Absolute().Left(6).Top(trebleTop - 30).FontSize(58).NoWrap()
		if octaveUp {
			ui.Text(c, "8").Absolute().Left(17).Top(trebleBottom + 10).FontSize(10).NoWrap()
		}
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
