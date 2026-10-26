package synth

import (
	"cmp"
	"math"
	"slices"
	"sort"

	"kanade/internal/score"
	"kanade/internal/score/stat"
)

// 乐器音轨的演奏（多轨卷帘的简易调节，score.Performance）：力度倍率、时值倍率、移八度与延音踏板作用于试听合成与
// MIDI 导出；卷帘显示的是演奏后的音（踏板延长的部分不画进音符块，另画成踏板条）。
//
// 踏板按「换踏板」的习惯：在小节线或和弦变化处抬起，pedalRepress 秒后再踩下，新和弦按下的音已在键上，仍被踏板留住，
// 上一和弦的余音在抬起时被制音。合成时把松开落在踩下区间内的音延长到抬起处；MIDI 导出写 CC64，音符保持原长。

// pedalRepress 是换踏板时抬起到再次踩下的间隔（秒）：钢琴演奏中「切分踏板」常见的几十毫秒（经验值）。
const pedalRepress = 0.03

// Span 是一段时间区间（秒），Start < End。
type Span struct{ Start, End float64 }

// InstrumentNotes 返回乐器 in 按演奏设置演奏的音（秒、MIDI 音高、力度 0–1），顺序与 in.Notes 相同；
// 移八度后超出 0–127 的音保留原音高。
func InstrumentNotes(in score.Instrument) []PianoNote {
	p := in.Play
	out := make([]PianoNote, len(in.Notes))
	for i, n := range in.Notes {
		v := n.Velocity
		if v == 0 {
			v = score.DefaultNoteVelocity
		}
		m := PianoNote{MIDI: n.Pitch, Start: n.Start, End: n.End, Velocity: stat.Clamp(v*p.VelocityScale(), 0.05, 1)}
		if !in.Drum {
			if k := n.Pitch + 12*p.Octave; k >= 0 && k <= 127 {
				m.MIDI = k
			}
			m.End = n.Start + (n.End-n.Start)*p.LengthScale()
		}
		out[i] = m
	}
	return out
}

// PedalSpans 返回踏板方式 mode（score.PedalBar / PedalChord）在 a 上踩下的区间，按时间排序；PedalOff、鼓或没有
// 小节线 / 和弦时为 nil。每小节换踏板时区间从小节线之后到下一条小节线（最后一小节到曲末）；随和弦换踏板时
// 从和弦开始之后到和弦结束，无和弦（N）的段不踩。
func PedalSpans(a *score.Analysis, mode string) []Span {
	var bounds []Span
	switch mode {
	case score.PedalBar:
		for i, d := range a.Downbeats {
			end := a.Duration
			if i+1 < len(a.Downbeats) {
				end = a.Downbeats[i+1]
			}
			bounds = append(bounds, Span{d, end})
		}
	case score.PedalChord:
		for _, c := range a.Chords {
			if c.Root != nil {
				bounds = append(bounds, Span{c.Start, c.End})
			}
		}
	}
	var out []Span
	for _, b := range bounds {
		if b.End-b.Start > 2*pedalRepress {
			out = append(out, Span{b.Start + pedalRepress, b.End})
		}
	}
	slices.SortFunc(out, func(x, y Span) int { return cmp.Compare(x.Start, y.Start) })
	return out
}

// applyPedal 返回按踏板延长后的音：松开时刻落在某个踩下区间内的音延长到该区间的抬起处，但不越过同一音高的下一次
// 按下（重复按键会先制音）。notes 按开始时间排序，不被修改。
func applyPedal(notes []PianoNote, pedal []Span) []PianoNote {
	if len(pedal) == 0 {
		return notes
	}
	out := slices.Clone(notes)
	next := map[int]float64{} // 音高 → 已经扫过的、该音高最早的下一次按下
	for i := len(out) - 1; i >= 0; i-- {
		n := &out[i]
		k := sort.Search(len(pedal), func(j int) bool { return pedal[j].End > n.End })
		if k < len(pedal) && pedal[k].Start <= n.End {
			n.End = pedal[k].End
			if t, ok := next[n.MIDI]; ok {
				n.End = max(min(n.End, t), notes[i].End)
			}
		}
		next[n.MIDI] = n.Start
	}
	return out
}

// 按原曲估计力度：MuScriptor 只给出音符，不给力度。对每个音测原曲中起音后一小段的能量——有音高的乐器用 Goertzel
// 测基音与二次谐波处的能量，鼓用宽带能量——减去起音前同一段的能量（扣除仍在响的前一个音与其他乐器的底噪），
// 换成分贝后按整轨的分布映射到力度：中位数为 DefaultNoteVelocity，第 10 与第 90 百分位约为 ±0.25。
// 混音中别的乐器也会落在同一频率上，估计只反映大致的强弱起伏。

// 估计用的时间窗（秒）：起音后跳过 velocityOnsetSkip（起音瞬态与转录的时间误差），测 velocityWindow；
// 有音高的音不超过音符本身的长度（但至少 velocityMinWindow，低音需要几个周期）。
const (
	velocityOnsetSkip = 0.01
	velocityWindow    = 0.08
	velocityMinWindow = 0.03
	// velocityMinSpread 是映射时第 10–90 百分位跨度的下限（dB）：起伏很小时不放大成大的力度差。
	velocityMinSpread = 6.0
)

// EstimateVelocities 按原曲 pcm（交错 ch 声道，rate Hz）估计 notes 中每个音的力度（0.2–1，与 notes 一一对应）。
// drum 为 true 时按宽带能量估计。原曲为空或没有音符时返回 nil。
func EstimateVelocities(pcm []float32, ch, rate int, notes []PianoNote, drum bool) []float64 {
	if len(pcm) == 0 || ch <= 0 || rate <= 0 || len(notes) == 0 {
		return nil
	}
	frames := len(pcm) / ch
	mono := func(i int) float64 {
		var s float32
		for k := range ch {
			s += pcm[i*ch+k]
		}
		return float64(s) / float64(ch)
	}
	// energy 返回 [t0, t0+w) 秒的能量（窗内平均）：freqs 非空时为这些频率处（Hann 窗 Goertzel）的能量之和，否则为宽带能量。
	energy := func(t0, w float64, freqs []float64) float64 {
		i0, i1 := max(int(t0*float64(rate)), 0), min(int((t0+w)*float64(rate)), frames)
		n := i1 - i0
		if n < 8 {
			return 0
		}
		if len(freqs) == 0 {
			var e float64
			for i := i0; i < i1; i++ {
				x := mono(i)
				e += x * x
			}
			return e / float64(n)
		}
		var e float64
		for _, f := range freqs {
			if f >= float64(rate)/2 {
				continue
			}
			coef := 2 * math.Cos(2*math.Pi*f/float64(rate))
			var s1, s2 float64
			for i := i0; i < i1; i++ {
				hann := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i-i0)/float64(n-1))
				s := mono(i)*hann + coef*s1 - s2
				s2, s1 = s1, s
			}
			e += (s1*s1 + s2*s2 - coef*s1*s2) / float64(n*n)
		}
		return e
	}
	db := make([]float64, len(notes))
	for i, n := range notes {
		w := velocityWindow
		var freqs []float64
		if !drum {
			w = stat.Clamp(n.End-n.Start-velocityOnsetSkip, velocityMinWindow, velocityWindow)
			f := MIDIFreq(n.MIDI)
			freqs = []float64{f, 2 * f}
		}
		on := energy(n.Start+velocityOnsetSkip, w, freqs)
		pre := energy(n.Start-w, w, freqs)
		db[i] = 10 * math.Log10(max(on-pre, 0.1*on)+1e-12)
	}
	med := stat.Percentile(db, 0.5)
	half := max(stat.Percentile(db, 0.9)-stat.Percentile(db, 0.1), velocityMinSpread) / 2
	out := make([]float64, len(notes))
	for i, d := range db {
		out[i] = math.Round(stat.Clamp(score.DefaultNoteVelocity+0.25*(d-med)/half, 0.2, 1)*100) / 100
	}
	return out
}
