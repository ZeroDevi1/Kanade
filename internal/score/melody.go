package score

import (
	"math"
	"slices"
	"sort"
)

// 旋律线：把 SOME 识别出的音符（连续时间、浮点音高）量化到拍点网格上，排成五线谱可以写出的音符与休止符。

// MelodyNote 是引擎输出的一个旋律音。
type MelodyNote struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Pitch float64 `json:"pitch"` // MIDI 音高，可含微分音
}

// Melody 是引擎输出的旋律线。
type Melody struct {
	Notes  []MelodyNote `json:"notes"`
	Source string       `json:"source"`
}

// MelodySubdiv 是量化网格：每拍分为 2 格（八分音符）。更细的网格（十六分）在人声识别误差下会写出大量碎音符。
const MelodySubdiv = 2

// beatPos 把时间换算为全曲的拍位置（拍点之间线性插值，首拍之前与末拍之后按平均拍长外推）。
func (a *Analysis) beatPos(t float64) float64 {
	b := a.Beats
	if len(b) < 2 {
		return t * 2 // 没有拍点时按 120 BPM
	}
	i := sort.SearchFloat64s(b, t)
	switch {
	case i == 0:
		return (t - b[0]) / (b[1] - b[0])
	case i >= len(b):
		n := len(b) - 1
		return float64(n) + (t-b[n])/(b[n]-b[n-1])
	}
	return float64(i-1) + (t-b[i-1])/(b[i]-b[i-1])
}

// MelodyItem 是小节内的一个音符或休止符（以网格格数计）。
type MelodyItem struct {
	Slot, Len int  // 起始格与长度（格）
	Rest      bool // 休止符
	Note      Note // 音高与拼写（Rest 为 false 时有效）
	TieNext   bool // 与下一个同音符用连音线相连（时值拆分或跨小节）
	// Start、End 是对应的实际时间（秒），用于播放高亮。
	Start, End float64
}

// MelodyMeasure 是一个小节的旋律排版。
type MelodyMeasure struct {
	Slots int // 小节总格数 = 拍数 × MelodySubdiv
	Items []MelodyItem
}

// noteLens 是可以直接写出的时值（格，八分音符为 1）：全、附点二分、二分、附点四分、四分、八分。
var noteLens = []int{8, 6, 4, 3, 2, 1}

// MelodyLayout 把旋律排进 ms 的各小节。transpose 是记谱时整体移动的半音数（男声常写高八度，配合「高音谱号 8」），
// flats 决定变化音用降号还是升号拼写。没有旋律时返回 nil。
func (a *Analysis) MelodyLayout(ms []Measure, transpose int, flats bool) []MelodyMeasure {
	if a.Melody == nil || len(a.Melody.Notes) == 0 || len(ms) == 0 {
		return nil
	}
	// 1. 量化：起止时间换算到网格（四舍五入），单音化（后一个音开始时截断前一个），去掉长度为 0 的音。
	type qn struct {
		s, e  int
		pitch int
	}
	var qs []qn
	for _, n := range a.Melody.Notes {
		s := int(math.Round(a.beatPos(n.Start) * MelodySubdiv))
		e := int(math.Round(a.beatPos(n.End) * MelodySubdiv))
		if e <= s {
			e = s + 1
		}
		qs = append(qs, qn{s, e, int(math.Round(n.Pitch)) + transpose})
	}
	slices.SortStableFunc(qs, func(x, y qn) int { return x.s - y.s })
	var mono []qn
	for _, q := range qs {
		if n := len(mono); n > 0 && mono[n-1].e > q.s {
			if mono[n-1].s == q.s {
				continue // 同时开始的两个音只留第一个
			}
			mono[n-1].e = q.s
		}
		mono = append(mono, q)
	}

	// 2. 小节的网格范围：小节第一拍在全曲中的拍序号。
	starts := make([]int, len(ms))
	for i, m := range ms {
		starts[i] = int(math.Round(a.beatPos(m.Beats[0].Time) * MelodySubdiv))
	}
	gridTime := func(g int) float64 { // 网格位置对应的时间（用于高亮）
		bi := float64(g) / MelodySubdiv
		k := int(bi)
		if len(a.Beats) < 2 {
			return bi / 2
		}
		k = max(0, min(k, len(a.Beats)-2))
		return a.Beats[k] + (bi-float64(k))*(a.Beats[k+1]-a.Beats[k])
	}

	out := make([]MelodyMeasure, len(ms))
	j := 0
	for i, m := range ms {
		lo := starts[i]
		hi := lo + len(m.Beats)*MelodySubdiv
		mm := MelodyMeasure{Slots: hi - lo}
		cursor := lo
		emit := func(s, e int, rest bool, pitch int, tieOut bool) {
			// 拆成可以写出的时值，拆分处用连音线相连。
			for s < e {
				l := e - s
				for _, c := range noteLens {
					if c <= l && (c != 8 || s == lo) {
						l = c
						break
					}
				}
				it := MelodyItem{Slot: s - lo, Len: l, Rest: rest, Start: gridTime(s), End: gridTime(s + l)}
				if !rest {
					it.Note = spellPitch(pitch, flats)
					it.TieNext = s+l < e || tieOut
				}
				mm.Items = append(mm.Items, it)
				s += l
			}
		}
		for ; j < len(mono) && mono[j].s < hi; j++ {
			q := mono[j]
			if q.e <= lo {
				continue
			}
			s, e := max(q.s, lo), min(q.e, hi)
			if s > cursor {
				emit(cursor, s, true, 0, false)
			}
			emit(s, e, false, q.pitch, q.e > hi)
			cursor = e
			if q.e > hi {
				break // 跨小节的音在下一小节继续
			}
		}
		if cursor < hi {
			emit(cursor, hi, true, 0, false)
		}
		out[i] = mm
	}
	return out
}

// spellPitch 拼写一个旋律音：按调号偏好用降号或升号（不考虑和声功能）。
func spellPitch(midi int, flats bool) Note {
	pc := ((midi % 12) + 12) % 12
	sharpLetter := []int{0, 0, 1, 1, 2, 3, 3, 4, 4, 5, 5, 6} // C C# D D# E F F# G G# A A# B
	flatLetter := []int{0, 1, 1, 2, 2, 3, 4, 4, 5, 5, 6, 6}  // C Db D Eb E F Gb G Ab A Bb B
	letter := sharpLetter[pc]
	if flats {
		letter = flatLetter[pc]
	}
	return spell(midi, letter)
}

// MelodyTranspose 返回记谱时的移调：旋律音高中位数低于 E4（多为男声）时写高八度（+12），配合「高音谱号 8」记号。
func (a *Analysis) MelodyTranspose() int {
	if a.Melody == nil || len(a.Melody.Notes) == 0 {
		return 0
	}
	ps := make([]float64, len(a.Melody.Notes))
	for i, n := range a.Melody.Notes {
		ps[i] = n.Pitch
	}
	slices.Sort(ps)
	if ps[len(ps)/2] < 64 {
		return 12
	}
	return 0
}
