package main

import (
	"cmp"
	"math"
	"slices"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/score/stat"
	"kanade/internal/score/synth"
	"kanade/internal/ui/fluent"
)

// 卷帘的简易演奏调节（放大查看中的乐器音轨）：顶部一栏调整轨的力度与时值倍率、移八度、延音踏板，并可按原曲估计
// 逐音力度；画布底部是力度条，按住拖过音符即按指针高度设置这些音的力度（类似 DAW 的力度编辑器），上沿画出踏板踩下的区间。
// 改动写回分析结果（score.Run 的 SetPerformance / SetVelocities），试听只重新合成这一条音轨，导出 MIDI 时一并写入。

// zoomVelH 是力度条的高度（DIP），zoomPedalH 是其上沿踏板条的高度。
const (
	zoomVelH   = 72
	zoomPedalH = 6
)

// performState 是演奏调节的编辑状态，属于 zoomState。拖动中的改动先记在草稿里，松开时才写回结果：
// 写回会换掉分析结果并重新合成音轨，不宜每帧进行。
type performState struct {
	// perf 是拖动力度 / 时值滑块期间的演奏设置草稿，perfOn 为 false 时没有草稿；perfFor 是它所属的乐器序号。
	perf    score.Performance
	perfOn  bool
	perfFor int
	// vel 是在力度条上拖动期间的逐音力度草稿（与乐器的 Notes 一一对应，未乘整轨倍率），nil 为没有在编辑；
	// velFor 是它所属的乐器序号，velLast 是上一帧指针处的时刻（秒），这一帧从 velLast 扫到指针处的音都改掉。
	vel     []float64
	velFor  int
	velLast float64
	// pressed 是上一帧画布是否按着，用于识别新的按下。
	pressed    bool
	estimating bool // 正在按原曲估计力度（后台）
}

// zoomPerformRow 是放大查看中乐器音轨 tr 的演奏设置栏。鼓只有整轨力度与逐音力度。
func (a *app) zoomPerformRow(c *ui.Context, tk *fluent.Tokens, res *score.Analysis, tr synth.Track) {
	t := c.Theme()
	z := &a.score.sheet.roll.zoom.perform
	pv := &a.score.preview
	k := tr.Instrument
	in := res.Transcription.Instruments[k]
	p := in.Play
	if z.perfOn && z.perfFor == k {
		p = z.perf
	}
	commit := func(p score.Performance) {
		z.perfOn = false
		a.editTranscription(func(r *score.Run) error { return r.SetPerformance(k, p) })
	}
	draft := func(p score.Performance) { z.perf, z.perfOn, z.perfFor = p, true, k }
	// percent 把滑块上的百分数存成倍率：取整到 1%，100% 存为 0（零值即原样）。
	percent := func(v float64) float64 {
		if v = math.Round(v) / 100; v == 1 {
			return 0
		}
		return v
	}
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "演奏").FontSize(13).FontWeight(600)
		vel := p.VelocityScale() * 100
		vs := ui.Slider(c, &vel, 25, 200).Label("整轨力度").Width(110)
		if vs.Changed() {
			p.Velocity = percent(vel)
			draft(p)
		}
		ui.Textf(c, "力度 %.0f%%", p.VelocityScale()*100).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(78)
		dragging := vs.Pressed()
		if !in.Drum {
			length := p.LengthScale() * 100
			ls := ui.Slider(c, &length, 25, 200).Label("整轨时值").Width(110)
			if ls.Changed() {
				p.Length = percent(length)
				draft(p)
			}
			ui.Textf(c, "时值 %.0f%%", p.LengthScale()*100).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(78)
			dragging = dragging || ls.Pressed()
			oct := p.Octave + 2
			if ui.Segmented(c, &oct, "−2", "−1", "0", "+1", "+2").Label("八度").Changed() {
				p.Octave = oct - 2
				commit(p)
			}
			pedals := []string{score.PedalOff, score.PedalBar, score.PedalChord}
			ped := max(slices.Index(pedals, p.Pedal), 0)
			if ui.Segmented(c, &ped, "不踩踏板", "每小节换", "随和弦换").Label("延音踏板").Changed() {
				p.Pedal = pedals[ped]
				commit(p)
			}
		}
		// 滑块松开（或用键盘调整）后写回草稿。
		if z.perfOn && z.perfFor == k && !dragging {
			commit(z.perf)
		}
		ui.Box(c).Grow(1)
		if z.estimating {
			ui.Spinner(c)
		}
		est := ui.Button(c.Key("estimate-velocity"), "按原曲估计力度").Disabled(z.estimating || pv.samples == nil).
			Tooltip("转录不含力度：按原曲中每个音起音处的响度估计强弱（混音中其他乐器会有干扰，只反映大致起伏）")
		if est.Clicked() {
			a.estimateVelocities(k, in)
		}
		hasVel := slices.ContainsFunc(in.Notes, func(n score.InstrumentNote) bool { return n.Velocity > 0 })
		if ui.Button(c.Key("clear-velocity"), "清除逐音力度").Disabled(!hasVel).Clicked() {
			a.editTranscription(func(r *score.Run) error { return r.SetVelocities(k, nil) })
		}
	})
}

// estimateVelocities 在后台按原曲估计第 k 种乐器 in 的逐音力度，完成后写回当前记录。估计用转录的原音高
// （不含移八度），因为测的是原曲里这个音的能量。
func (a *app) estimateVelocities(k int, in score.Instrument) {
	s := &a.score
	pv := &s.preview
	z := &s.sheet.roll.zoom.perform
	if len(s.runs) == 0 || pv.samples == nil {
		return
	}
	run := s.runs[s.shown]
	notes := make([]synth.PianoNote, len(in.Notes))
	for i, n := range in.Notes {
		notes[i] = synth.PianoNote{MIDI: n.Pitch, Start: n.Start, End: n.End}
	}
	pcm, ch, rate := pv.samples, pv.ch, pv.rate
	z.estimating = true
	go func() {
		v := synth.EstimateVelocities(pcm, ch, rate, notes, in.Drum)
		a.post(func() {
			z.estimating = false
			if v == nil {
				s.err = "没有可用的原曲采样，无法估计力度"
				return
			}
			// 写回估计时的那条记录（期间可能切换了显示的记录）；音符数变了时 SetVelocities 报错。
			if err := run.SetVelocities(k, v); err != nil {
				s.err = err.Error()
			}
		})
	}()
}

// baseVelocities 返回乐器 in 的逐音力度（未乘整轨倍率），未设置的音为默认力度。
func baseVelocities(in score.Instrument) []float64 {
	out := make([]float64, len(in.Notes))
	for i, n := range in.Notes {
		out[i] = cmp.Or(n.Velocity, score.DefaultNoteVelocity)
	}
	return out
}

// velocityDrag 处理力度条上的按住拖动（每帧调用）：pressed 为画布是否按着，x、y 为指针相对画布的位置，
// lane 为力度条相对画布的区域，timeAt 把横坐标换成时刻，tol 为命中音符的时间容差（秒）。
// 新的按下落在力度条上时开始编辑；按着时把从上一帧到这一帧扫过的音的力度设为指针高度对应的值；松开时写回。
// 返回是否正在编辑（此时画布不平移、不跳转）。
func (a *app) velocityDrag(res *score.Analysis, tr synth.Track, pressed bool, x, y float32, lane ui.Rect,
	timeAt func(float32) float64, tol float64) bool {
	z := &a.score.sheet.roll.zoom.perform
	k := tr.Instrument
	if z.vel != nil && (z.velFor != k || res.Transcription == nil || k >= len(res.Transcription.Instruments)) {
		z.vel = nil // 换了音轨或结果：放弃草稿
	}
	defer func() { z.pressed = pressed }()
	if k < 0 || res.Transcription == nil || k >= len(res.Transcription.Instruments) {
		return false
	}
	in := res.Transcription.Instruments[k]
	if pressed && !z.pressed && x >= lane.X && y >= lane.Y && y < lane.Y+lane.H {
		z.vel, z.velFor, z.velLast = baseVelocities(in), k, timeAt(x)
	}
	if z.vel == nil {
		return false
	}
	if !pressed {
		v := z.vel
		z.vel = nil
		a.editTranscription(func(r *score.Run) error { return r.SetVelocities(k, v) })
		return true
	}
	now := timeAt(x)
	lo, hi := min(z.velLast, now)-tol, max(z.velLast, now)+tol
	val := stat.Clamp(1-float64((y-lane.Y-zoomPedalH)/(lane.H-zoomPedalH-2)), 0.05, 1)
	for j, n := range tr.Notes {
		if n.Start >= lo && n.Start <= hi && j < len(tr.Source) && tr.Source[j] < len(z.vel) {
			// 力度条显示的是乘过整轨倍率的力度，存回时除掉倍率。
			z.vel[tr.Source[j]] = math.Round(stat.Clamp(val/in.Play.VelocityScale(), 0.05, 1)*100) / 100
		}
	}
	z.velLast = now
	return true
}

// drawVelocityLane 在 lane 中画 [v0, v1) 内各音的力度条与上沿的踏板区间。拖动编辑中显示草稿的力度。
func (a *app) drawVelocityLane(p *ui.Painter, lane ui.Rect, res *score.Analysis, tr synth.Track, v0, v1 float64,
	color ui.Color, xOf func(float64) float32) {
	z := &a.score.sheet.roll.zoom.perform
	p.Fill(lane, rollPaper.black, 0)
	p.Line(lane.X, lane.Y, lane.X+lane.W, lane.Y, 1, rollPaper.bar)
	body := ui.Rect{X: lane.X, Y: lane.Y + zoomPedalH, W: lane.W, H: lane.H - zoomPedalH - 2}
	p.Line(lane.X, body.Y+body.H/2, lane.X+lane.W, body.Y+body.H/2, 1, rollPaper.rowLine)
	for _, s := range tr.Pedal {
		if s.End > v0 && s.Start < v1 {
			x0, x1 := xOf(max(s.Start, v0)), xOf(min(s.End, v1))
			p.Fill(ui.Rect{X: x0, Y: lane.Y + 1, W: max(x1-x0-1, 1), H: zoomPedalH - 2}, rollPaper.bar.Alpha(0.7), 1)
		}
	}
	scale := 1.0
	var draft []float64
	if z.vel != nil && z.velFor == tr.Instrument && res.Transcription != nil && tr.Instrument < len(res.Transcription.Instruments) {
		draft, scale = z.vel, res.Transcription.Instruments[tr.Instrument].Play.VelocityScale()
	}
	for j, n := range tr.Notes {
		if n.Start < v0 || n.Start >= v1 {
			continue
		}
		v := n.Velocity
		if draft != nil && j < len(tr.Source) && tr.Source[j] < len(draft) {
			v = stat.Clamp(draft[tr.Source[j]]*scale, 0.05, 1)
		}
		h := float32(v) * body.H
		x := xOf(n.Start)
		p.Fill(ui.Rect{X: x - 1, Y: body.Y + body.H - h, W: 3, H: h}, color, 1)
		p.Fill(ui.Rect{X: x - 3, Y: body.Y + body.H - h - 1, W: 7, H: 3}, rollPaper.noteEdge, 1)
	}
}
