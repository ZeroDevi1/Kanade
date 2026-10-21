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
	// Lyric 是该音唱的字（一个字的第一个音）；Slur 为 true 表示同一个字的后续音（连音）。两者都为空的音没有歌词。
	// 只有分析时给了 LRC 歌词才有。
	Lyric string `json:"lyric,omitempty"`
	Slur  bool   `json:"slur,omitempty"`
}

// Melody 是引擎输出的旋律线。
type Melody struct {
	Notes  []MelodyNote `json:"notes"`
	Source string       `json:"source"`
	// Lyrics 是歌词对齐的概况；分析时没有歌词则为 nil。
	Lyrics *MelodyLyrics `json:"lyrics,omitempty"`
}

// MelodyLyrics 是引擎的逐字对齐结果。
type MelodyLyrics struct {
	Source string `json:"source"` // LRC 文件路径
	Words  []struct {
		Text  string  `json:"text"`
		Line  int     `json:"line"`  // 所在歌词行（从 0 起）
		Start float64 `json:"start"` // 对齐出的起止时间（秒）
		End   float64 `json:"end"`
	} `json:"words"`
	Matched int    `json:"matched"` // 分到音符的字数；其余的字没有对应的识别音符
	Note    string `json:"note"`    // 非空时说明对齐放宽了哪些约束（结果可信度较低）
	Method  string `json:"method"`  // 字与音符的对应方法（SlurModels 的 Key）；旧记录为空，即 align
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
	// Lyric 是写在音符下的字（只在一个音的第一段上）；Slur 为 true 表示该音是前一个字的连音。
	Lyric string
	Slur  bool
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
	//    快速的转音在八分音符网格上常量化到同一格：同时开始的音只留一个，通常是第一个；但后一个音带字而前一个
	//    只是连音时留后一个，避免丢字。
	type qn struct {
		s, e  int
		pitch int
		lyric string
		slur  bool
	}
	var qs []qn
	for _, n := range a.Melody.Notes {
		s := int(math.Round(a.beatPos(n.Start) * MelodySubdiv))
		e := int(math.Round(a.beatPos(n.End) * MelodySubdiv))
		if e <= s {
			e = s + 1
		}
		qs = append(qs, qn{s, e, int(math.Round(n.Pitch)) + transpose, n.Lyric, n.Slur})
	}
	slices.SortStableFunc(qs, func(x, y qn) int { return x.s - y.s })
	var mono []qn
	for _, q := range qs {
		if n := len(mono); n > 0 && mono[n-1].e > q.s {
			if mono[n-1].s == q.s {
				if mono[n-1].lyric == "" && q.lyric != "" {
					mono[n-1] = q
				}
				continue
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
		emit := func(s, e int, rest bool, q qn, tieOut bool) {
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
					it.Note = spellPitch(q.pitch, flats)
					it.TieNext = s+l < e || tieOut
					// 字只写在音的第一段：跨小节或拆分时值后的后续段是连线延续，不再标字。
					if s == q.s {
						it.Lyric, it.Slur = q.lyric, q.slur
					}
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
				emit(cursor, s, true, qn{}, false)
			}
			emit(s, e, false, q, q.e > hi)
			cursor = e
			if q.e > hi {
				break // 跨小节的音在下一小节继续
			}
		}
		if cursor < hi {
			emit(cursor, hi, true, qn{}, false)
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

// BeamGroups 返回小节内需要用符杠相连的八分音符组（Items 的下标，每组至少 2 个）。
// 规则：相邻的八分音符（Len == 1、非休止）连成一组，休止符或更长的音符打断；4 拍的小节按半小节（2 拍）分组，
// 其余拍数按单拍分组，组不跨越分组边界。只有一个八分音符的组不返回（保留符尾）。
func (mm MelodyMeasure) BeamGroups() [][]int {
	span := MelodySubdiv // 分组单位（格）
	if mm.Slots == 4*MelodySubdiv {
		span = 2 * MelodySubdiv
	}
	var out [][]int
	var cur []int
	flush := func() {
		if len(cur) >= 2 {
			out = append(out, cur)
		}
		cur = nil
	}
	for i, it := range mm.Items {
		if it.Rest || it.Len != 1 {
			flush()
			continue
		}
		if len(cur) > 0 && mm.Items[cur[0]].Slot/span != it.Slot/span {
			flush()
		}
		cur = append(cur, i)
	}
	flush()
	return out
}
