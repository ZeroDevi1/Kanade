package main

import (
	"math"
	"slices"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 灵感页的乐句卷帘与编辑：点击选中音符、拖动改位置与音高、双击空白处添加音符，编辑栏做升降半音、移动、伸缩、
// 一分为二、连唱与删除；每次编辑前保存音符快照，可撤销 / 重做（Ctrl+Z / Ctrl+Y，见 inspireShortcuts）。

// inspireUndoMax 是保留的撤销步数（经验值：一段动机的编辑通常几十步以内）。
const inspireUndoMax = 100

// 卷帘的尺寸（DIP）：左侧音名栏宽 rollKeysW，顶部和弦名一行高 rollChordH，整体高 rollHeight。
const (
	rollKeysW  = 40
	rollChordH = 18
	rollHeight = 280
)

// rollDrag 是卷帘里的一次拖动：按下音符时开始，松开时结束。纵轴范围在拖动中固定（否则音符移出音域时网格随之
// 伸缩，造成抖动）；before 是按下时的音符快照，拖动真的改动了音符（moved）时在松开时存入撤销栈。
type rollDrag struct {
	active bool
	note   int
	x0, y0 float32
	orig   score.MelodyNote
	lo, hi float64
	before []score.MelodyNote
	moved  bool
}

// edit 对乐句做一次编辑：先保存快照供撤销，再执行 fn 并重新填拟声字、配和声、合成试听。
func (s *inspireState) edit(fn func()) {
	before := slices.Clone(s.hum.Notes)
	fn()
	s.pushUndo(before)
	s.changed()
}

// pushUndo 把编辑前的快照存入撤销栈（超出 inspireUndoMax 时丢掉最早的），并清空重做栈。
func (s *inspireState) pushUndo(before []score.MelodyNote) {
	s.undo = append(s.undo, before)
	if len(s.undo) > inspireUndoMax {
		s.undo = s.undo[1:]
	}
	s.redo = nil
}

// undoEdit / redoEdit 撤销与重做一步编辑；没有可撤销 / 重做的步骤时不做任何事。
func (s *inspireState) undoEdit() { s.swapHistory(&s.undo, &s.redo) }
func (s *inspireState) redoEdit() { s.swapHistory(&s.redo, &s.undo) }

// swapHistory 从 from 取出最近的快照换上，当前的音符存入 to。
func (s *inspireState) swapHistory(from, to *[][]score.MelodyNote) {
	if s.hum == nil || len(*from) == 0 || s.drag.active {
		return
	}
	last := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	*to = append(*to, slices.Clone(s.hum.Notes))
	s.hum.Notes, s.sel = last, -1
	s.changed()
}

// inspireShortcuts 注册灵感页的编辑快捷键：Ctrl+Z 撤销，Ctrl+Y 或 Ctrl+Shift+Z 重做，Delete 删除选中的音符。
// 须在 view 的顶层调用。
func (a *app) inspireShortcuts(c *ui.Context) {
	s := &a.inspire
	c.OnShortcut(ui.Cmd, ui.KeyZ, s.undoEdit)
	c.OnShortcut(ui.Cmd, ui.KeyY, s.redoEdit)
	c.OnShortcut(ui.Cmd|ui.Shift, ui.KeyZ, s.redoEdit)
	c.OnShortcut(0, ui.KeyDelete, func() {
		if s.hum != nil && s.sel >= 0 && s.sel < len(s.hum.Notes) {
			s.edit(func() { s.deleteNote(s.sel) })
		}
	})
}

// deleteNote 删除第 i 个音；删掉的是音节的第一个音时，后面紧接的连音成为新的字。
func (s *inspireState) deleteNote(i int) {
	h := s.hum
	h.Notes = slices.Delete(h.Notes, i, i+1)
	if i < len(h.Notes) && h.Notes[i].Slur && (i == 0 || h.Notes[i-1].End < h.Notes[i].Start-1e-9) {
		h.Notes[i].Slur = false
	}
	s.sel = -1
}

// neighbors 返回第 i 个音可占用的范围：前一音的终点与后一音的起点（没有时为乐句的起止）。
func (s *inspireState) neighbors(i int) (float64, float64) {
	h := s.hum
	prevEnd, nextStart := 0.0, h.Settings.Length()
	if i > 0 {
		prevEnd = h.Notes[i-1].End
	}
	if i+1 < len(h.Notes) {
		nextStart = h.Notes[i+1].Start
	}
	return prevEnd, nextStart
}

// addNote 在乐谱时间 sec（向下取整到十六分音符）添加音高为 pitch 的音：时值一拍，碰到下一音时截短；
// 该处已有音或放不下一个十六分音符时不添加。返回新音的序号（-1 为没有添加）。
func (s *inspireState) addNote(sec float64, pitch int) int {
	h := s.hum
	g := h.Settings.Grid()
	start := math.Floor(sec/g+1e-9) * g
	if start < 0 || start >= h.Settings.Length()-1e-9 {
		return -1
	}
	i := 0
	for i < len(h.Notes) && h.Notes[i].Start < start-1e-9 {
		i++
	}
	if i > 0 && h.Notes[i-1].End > start+1e-9 {
		return -1 // 落在前一个音里
	}
	end := min(start+h.Settings.Beat(), h.Settings.Length())
	if i < len(h.Notes) {
		end = min(end, h.Notes[i].Start)
	}
	if end-start < g-1e-9 {
		return -1
	}
	h.Notes = slices.Insert(h.Notes, i, score.MelodyNote{Start: start, End: end, Pitch: float64(pitch)})
	return i
}

// splitNote 把第 i 个音在靠近中点的十六分音符处一分为二，后一半成为新的字；短于两个十六分音符时不拆。
func (s *inspireState) splitNote(i int) bool {
	h := s.hum
	n := h.Notes[i]
	g := h.Settings.Grid()
	if n.End-n.Start < 2*g-1e-9 {
		return false
	}
	mid := n.Start + math.Max(1, math.Round((n.End-n.Start)/g/2))*g
	h.Notes[i].End = mid
	h.Notes = slices.Insert(h.Notes, i+1, score.MelodyNote{Start: mid, End: n.End, Pitch: n.Pitch})
	return true
}

// inspireRoll 绘制卷帘：小节、拍与十六分网格，所选和声的和弦名，量化后的音符（标出拟声字，转音与前一音相连），
// 录到的音高曲线，以及播放头。录音中横轴为录音时间（节拍器模式扣除预备拍）；整理后为乐谱时间，可以编辑：
// 点击音符选中、拖动改位置（按十六分音符）与音高，双击空白处添加音符，点击空白处取消选择并跳转。
func (a *app) inspireRoll(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	recording := s.recordingNow()
	if recording {
		s.frames = s.sess.Frames()
	}
	// 横轴：录音中为录音时间（toX 把帧时间换成横轴时间），整理后为乐谱时间。
	var (
		notes  []score.MelodyNote
		chords []score.Segment
		length float64
		beat   float64
		bpb    int
		sub    int
		toX    func(float64) float64
		tuning float64
	)
	switch {
	case recording && s.clickPlayer == nil:
		elapsed := time.Since(s.recStart).Seconds()
		length, toX = max(8, math.Ceil(elapsed/4+0.5)*4), func(t float64) float64 { return t }
	case recording:
		set := s.recSet
		length, beat, bpb, sub = set.Length(), set.Beat(), set.BeatsPerBar, set.Sub()
		toX = func(t float64) float64 { return t - set.Start() }
	default:
		h := *s.hum
		set := h.Settings
		notes, tuning = h.Notes, h.Tuning
		length, beat, bpb, sub = set.Length(), set.Beat(), set.BeatsPerBar, set.Sub()
		toX = h.ScoreTime
		if s.pick < len(s.harmonies) {
			chords = s.harmonies[s.pick].Chords
		}
	}
	// 纵轴：音符（没有时取录到的音高）的音域上下各留 3 个半音，至少一个八度半；拖动中沿用按下时的范围。
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, n := range notes {
		lo, hi = min(lo, n.Pitch), max(hi, n.Pitch)
	}
	for _, f := range s.frames {
		if f.F0 > 0 && f.Clarity > 0.7 {
			m := 69 + 12*math.Log2(f.F0/440)
			lo, hi = min(lo, m), max(hi, m)
		}
	}
	if math.IsInf(lo, 1) {
		lo, hi = 60, 72
	}
	lo, hi = math.Floor(lo)-3, math.Ceil(hi)+3
	if hi-lo < 18 {
		lo -= math.Floor((18 - (hi - lo)) / 2)
		hi = lo + 18
	}
	if s.drag.active {
		lo, hi = s.drag.lo, s.drag.hi
	}
	box := ui.Box(c.Key("inspire-roll")).Height(rollHeight).Radius(fluent.ControlRadius).Background(rollPaper.white).
		Border(1, tk.CardStroke).Clip().Cursor(ui.CursorPointer).Label("乐句卷帘")
	if !recording && s.hum != nil {
		a.rollInput(box, length, lo, hi)
	}
	notes = nil // 编辑可能在本帧改了音符：绘制时读取最新的
	if !recording && s.hum != nil {
		notes = s.hum.Notes
	}
	rv := rollView{lo: lo, hi: hi, length: length, beat: beat, bpb: bpb, sub: sub, notes: notes, chords: chords,
		frames: s.frames, toX: toX, tuning: tuning, sel: s.sel, accent: t.Accent}
	if s.hum != nil {
		rv.tonic = s.hum.TonicPC
	}
	player, clickPlayer, recStart, scoreTime := s.player, s.clickPlayer, s.recStart, s.scoreTime
	rv.pos = func() (float64, bool) {
		switch {
		case clickPlayer != nil:
			return toX(clickPlayer.Position().Seconds()), true
		case recording:
			return time.Since(recStart).Seconds(), true
		case player != nil:
			return scoreTime(player.Position().Seconds()), player.Playing()
		}
		return -1, false
	}
	box.Draw(func(p *ui.Painter, r ui.Rect) { paintRoll(p, r, rv) })
}

// rollView 是卷帘要画的内容：纵轴音高范围 [lo, hi]，横轴 [0, length]（秒）；beat 为 0 时不画节拍网格（只画秒线）。
// frames 经 toX 换到横轴时间、扣除 tuning（音分）后画成淡色的点；sel 为选中的音（-1 为没有）；pos 在绘制时
// 返回播放头的位置（-1 为不画）与是否在播放（播放中逐帧重画）。
type rollView struct {
	lo, hi, length, beat float64
	bpb                  int
	sub                  int // 一拍里的八分音符数（复合拍 3，否则 2；0 视为 2）
	notes                []score.MelodyNote
	chords               []score.Segment
	tonic                int
	frames               []score.KaraokeFrame
	toX                  func(float64) float64
	tuning               float64
	sel                  int
	accent               ui.Color
	pos                  func() (float64, bool)
}

// paintRoll 画卷帘：行与音名、节拍网格、顶部的和弦名、录到的音高、音符（拟声字、转音连线、选中描边）与播放头。
func paintRoll(p *ui.Painter, r ui.Rect, v rollView) {
	lo, hi, length := v.lo, v.hi, v.length
	xOf := func(sec float64) float32 { return r.X + rollKeysW + float32(sec/length)*(r.W-rollKeysW) }
	top := r.Y + rollChordH
	rowH := (r.H - rollChordH) / float32(hi-lo+1)
	yOf := func(m float64) float32 { return top + float32(hi-m)*rowH }
	// 行：黑键行深色，每个 C 标出音名。
	for m := int(lo); m <= int(hi); m++ {
		y := yOf(float64(m)) - rowH/2
		if slices.Contains([]int{1, 3, 6, 8, 10}, m%12) {
			p.Fill(ui.Rect{X: r.X + rollKeysW, Y: y, W: r.W - rollKeysW, H: rowH}, rollPaper.black, 0)
		}
		if m%12 == 0 {
			p.Line(r.X, y+rowH, r.X+r.W, y+rowH, 1, rollPaper.cLine)
			p.Text(r.X+4, y+(rowH-10)/2, midiNoteName(m), 10, rollPaper.keyText)
		}
	}
	// 网格：有节拍时画十六分、拍与小节线；自由录音中只画秒线。
	if v.beat > 0 {
		per := 2 * max(v.sub, 2) // 每拍的十六分音符数
		g := v.beat / float64(per)
		for k := 0; float64(k)*g <= length+1e-9; k++ {
			color := rollPaper.rowLine
			switch {
			case k%(per*v.bpb) == 0:
				color = rollPaper.bar
			case k%per == 0:
				color = rollPaper.beat
			}
			x := xOf(float64(k) * g)
			p.Line(x, top, x, r.Y+r.H, 1, color)
		}
	} else {
		for sec := 0.0; sec <= length; sec++ {
			p.Line(xOf(sec), top, xOf(sec), r.Y+r.H, 1, rollPaper.beat)
		}
	}
	// 和弦名（爵士写法）写在顶部一行。
	p.Fill(ui.Rect{X: r.X + rollKeysW, Y: r.Y, W: r.W - rollKeysW, H: rollChordH}, rollPaper.ruler, 0)
	for _, ch := range v.chords {
		p.Text(xOf(ch.Start)+3, r.Y+3, score.Symbol(ch, score.SymbolJazz, v.tonic), 11, rollPaper.rulerText)
	}
	// 录到的音高（扣除整体偏移），淡色细点。
	for _, f := range v.frames {
		if f.F0 <= 0 || f.Clarity < 0.7 {
			continue
		}
		sec := v.toX(f.T)
		if sec < 0 || sec > length {
			continue
		}
		m := 69 + 12*math.Log2(f.F0/440) - v.tuning/100
		p.Fill(ui.Rect{X: xOf(sec) - 1, Y: yOf(m) - 1, W: 2, H: 2}, ui.RGBA(60, 60, 60, 0.45), 1)
	}
	// 音符：转音与前一音之间画连线；选中的音加深描边；拟声字写在音符上方。
	for i, n := range v.notes {
		x0, x1 := xOf(n.Start), xOf(n.End)
		y := yOf(n.Pitch) - rowH/2
		if n.Slur && i > 0 {
			pn := v.notes[i-1]
			p.Line(xOf(pn.End)-2, yOf(pn.Pitch), x0+2, yOf(n.Pitch), 2, v.accent)
		}
		rc := ui.Rect{X: x0 + 1, Y: y + 1, W: max(x1-x0-2, 2), H: max(rowH-2, 3)}
		p.Fill(rc, v.accent, 3)
		if i == v.sel {
			p.Stroke(rc, ui.RGB(20, 20, 20), 3, 2)
		}
		if n.Lyric != "" {
			p.Text(x0+2, y-14, n.Lyric, 12, ui.RGB(30, 30, 30))
		}
	}
	if v.pos == nil {
		return
	}
	pos, playing := v.pos()
	if pos >= 0 && pos <= length {
		x := xOf(pos)
		p.Line(x, r.Y, x, r.Y+r.H, 2, rollPaper.playhead)
	}
	if playing {
		p.AnimationFrame()
	}
}

// rollInput 处理卷帘上的指针操作（尺寸取上一帧的）：按下音符开始拖动，拖动中按十六分音符与半音移动（不越过相邻的音），
// 松开时把拖动作为一步编辑；没有拖动的单击选中音符或跳转；双击空白处添加音符。
func (a *app) rollInput(box ui.Element, length, lo, hi float64) {
	s := &a.inspire
	h := s.hum
	b := box.Bounds()
	if b.W <= rollKeysW || b.H <= rollChordH {
		return
	}
	rowH := (b.H - rollChordH) / float32(hi-lo+1)
	x, y, _ := box.PointerPosition()
	secAt := func(x float32) float64 { return float64((x-rollKeysW)/(b.W-rollKeysW)) * length }
	pitchAt := func(y float32) int { return int(math.Floor(hi - float64((y-rollChordH)/rowH) + 0.5)) }
	hit := func(x, y float32) int {
		sec, m := secAt(x), pitchAt(y)
		return slices.IndexFunc(h.Notes, func(n score.MelodyNote) bool {
			return sec >= n.Start && sec < n.End && int(n.Pitch) == m
		})
	}
	g := h.Settings.Grid()
	d := &s.drag
	switch pressed := box.Pressed(); {
	case pressed && !d.active:
		if i := hit(x, y); i >= 0 {
			*d = rollDrag{active: true, note: i, x0: x, y0: y, orig: h.Notes[i], lo: lo, hi: hi, before: slices.Clone(h.Notes)}
			s.sel = i
		}
	case pressed && d.active && d.note < len(h.Notes):
		steps := math.Round(secAt(x)/g) - math.Round(secAt(d.x0)/g)
		dp := pitchAt(y) - pitchAt(d.y0)
		prevEnd, nextStart := s.neighbors(d.note)
		dur := d.orig.End - d.orig.Start
		start := min(max(d.orig.Start+steps*g, prevEnd), nextStart-dur)
		pitch := min(max(int(d.orig.Pitch)+dp, 24), 96)
		if start < prevEnd-1e-9 {
			start = d.orig.Start // 两侧都放不下（不会发生：原位置总是放得下）
		}
		n := &h.Notes[d.note]
		if math.Abs(n.Start-start) > 1e-9 || int(n.Pitch) != pitch {
			n.Start, n.End, n.Pitch = start, start+dur, float64(pitch)
			d.moved = true
		}
	case !pressed && d.active:
		if d.moved {
			s.pushUndo(d.before)
			s.changed()
		}
		*d = rollDrag{moved: d.moved} // 保留 moved：本帧的 Clicked 属于这次拖动，不再当作单击
	}
	if box.DoubleClicked() {
		if hit(x, y) < 0 {
			before := slices.Clone(h.Notes)
			if i := s.addNote(max(secAt(x), 0), pitchAt(y)); i >= 0 {
				s.sel = i
				s.pushUndo(before)
				s.changed()
			}
		}
		return
	}
	if box.Clicked() {
		if s.drag.moved {
			s.drag.moved = false
			return
		}
		s.sel = hit(x, y)
		if s.sel < 0 {
			s.seekScore(max(secAt(x), 0))
		}
	}
}

// inspireEditBar 是编辑栏：撤销 / 重做；选中音符时升降半音、左右移动与伸缩（十六分音符）、一分为二、连唱、删除。
func (a *app) inspireEditBar(c *ui.Context) {
	t := c.Theme()
	s := &a.inspire
	h := s.hum
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.Button(c, "撤销").Tooltip("Ctrl+Z").Disabled(len(s.undo) == 0).Clicked() {
			s.undoEdit()
		}
		if ui.Button(c, "重做").Tooltip("Ctrl+Y").Disabled(len(s.redo) == 0).Clicked() {
			s.redoEdit()
		}
		if s.sel < 0 || s.sel >= len(h.Notes) {
			ui.Text(c, "点击音符选中，拖动改位置与音高；双击空白处添加音符。").FontSize(12).TextColor(t.TextMuted)
			return
		}
		i := s.sel
		n := &h.Notes[i]
		g := s.set().Grid()
		prevEnd, nextStart := s.neighbors(i)
		ui.Textf(c, "第 %d 个音 · %s", i+1, midiNoteName(int(n.Pitch))).FontSize(12).Width(110)
		edit := func(label string, ok bool, fn func()) {
			if ui.Button(c, label).Disabled(!ok).Clicked() {
				s.edit(fn)
			}
		}
		edit("升半音", n.Pitch < 96, func() { n.Pitch++ })
		edit("降半音", n.Pitch > 24, func() { n.Pitch-- })
		edit("左移", n.Start-g >= prevEnd-1e-9, func() { n.Start -= g; n.End -= g })
		edit("右移", n.End+g <= nextStart+1e-9, func() { n.Start += g; n.End += g })
		edit("缩短", n.End-n.Start > g+1e-9, func() { n.End -= g })
		edit("延长", n.End+g <= nextStart+1e-9, func() { n.End += g })
		edit("一分为二", n.End-n.Start >= 2*g-1e-9, func() { s.splitNote(i) })
		slurLabel := "与前一音连唱"
		if n.Slur {
			slurLabel = "拆成新的字"
		}
		edit(slurLabel, i > 0, func() { n.Slur = !n.Slur })
		edit("删除", true, func() { s.deleteNote(i) })
	})
}

// 乐句与乐段的显示方式。
const (
	inspireViewRoll  = iota // 卷帘（乐句卷帘可编辑）
	inspireViewStaff        // 五线谱（大谱表：旋律与和弦）
	inspireViewJazz         // 爵士谱（按拍写和弦记号）
)

// inspireViews 是显示方式的选项名，序号即 inspireView* 常量。
var inspireViews = []string{"卷帘", "五线谱", "爵士谱"}

// playerPos 返回播放器的位置（秒）；没有播放器时为 -1。
func playerPos(p *audio.Player) float64 {
	if p == nil {
		return -1
	}
	return p.Position().Seconds()
}

// inspireSheet 用音乐分析页的乐谱绘制显示 res（灵感页的乐句或乐段）：五线谱或爵士谱，高亮 pos（乐谱时间，秒）
// 所在的小节，点击跳转（seek）。播放中每 100 ms 由播放控制条重建一次，高亮随之移动。
func (a *app) inspireSheet(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, view int, pos float64, seek func(float64)) {
	ms := res.Measures()
	if len(ms) == 0 {
		ui.Text(c, "没有小节，无法排成乐谱。").FontSize(12).TextColor(c.Theme().TextMuted)
		return
	}
	cur := -1
	for i, m := range ms {
		if pos >= m.Start && pos < m.End {
			cur = i
		}
	}
	ui.Column(c.Key("inspire-sheet")).AlignItems(ui.Stretch).Children(func() {
		if view == inspireViewJazz {
			a.jazzSheet(c, tk, res, ms, pos, cur, false, seek, score.SymbolJazz)
		} else {
			a.staffSheet(c, tk, res, ms, pos, cur, false, seek, score.SymbolJazz)
		}
	})
}
