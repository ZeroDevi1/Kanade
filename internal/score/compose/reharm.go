// Package compose 是灵感页的创作部分：给哼唱乐句按风格配和声（Harmonize），并把动机发展成乐段（Develop）。
package compose

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"kanade/internal/score"
	"kanade/internal/score/hum"
)

// 灵感页的和声配置：为整理好的旋律（Hum）配几套风格不同的和弦。规则式、确定性：
//  1. 和声时间片：4/4 每小节两片（第 1、3 拍起），2/4、3/4 每小节一片；只在片首换和弦；
//  2. 候选和弦：调内和弦，加副属（V/x）、ii/x、三全音替代（subV）、调式交替借用、减七经过和弦、sus / 6 和弦；
//     调式风格改用 Mixolydian / Dorian 的和弦；
//  3. 旋律—和弦匹配：各旋律音按时值、拍位与上下文（经过音、邻音、转音降权）加权，按音在和弦上是和弦音、可用张力音、
//     避免音还是其他音计代价（Levine《The Jazz Theory Book》的和弦—音阶理论）；
//  4. 进行代价：T / S / D 功能转移矩阵、根音五度下行奖励、副属与替代和弦必须解决、终止式奖励、同和弦重复的代价；
//  5. Viterbi 求最优，再对已选路径加惩罚重解，得到彼此差异足够大的备选。
//
// 设计与权重经 Claude Fable 5.1 咨询，均为经验值；风格之间只换权重（HarmonyStyles）。

// 和弦的类别（决定先验、功能与理由）。
const (
	kindDiatonic = iota // 调内
	kindSecDom          // 副属 V/x
	kindSecII           // x 的 ii（与 V/x 构成 ii–V）
	kindSubV            // 三全音替代
	kindBorrowed        // 调式交替借用
	kindDimPass         // 减七经过
	kindModal           // 调式风格的特征和弦
	kindLocked          // 用户手动指定、不在候选表里的和弦
)

// 和声功能。
const (
	funcT = iota
	funcS
	funcD
)

// 风格。
const (
	StylePop = iota
	StyleJazz
	StyleReharm
	StyleModal
)

// HarmonyStyles 是风格的名称（界面选项），序号即 Style* 常量。
var HarmonyStyles = []string{"流行", "爵士", "重配和声", "调式"}

// chordCand 是一个候选和弦。root 为音级；rel 为相对主音的半音数；target 为副属、替代、经过和弦须解决到的根音
// 相对主音的半音数（其余为 -1）。
type chordCand struct {
	root, rel int
	quality   string
	kind      int
	fn        int
	target    int
	roman     string
	// group 是先验表中的细分：副属的常见 / 少见目标，借用的常见 / 少见和弦，调式的特征和弦。
	group string
}

// harmSlice 是一个和声时间片：start / end 为乐谱时间（拍），notes 为片内的旋律音（音级与权重）。
type harmSlice struct {
	start, end float64
	barStart   bool
	notes      []harmNote
	weight     float64 // 片内音权重之和（空片为 0）
}

type harmNote struct {
	pc int
	w  float64
}

// Harmony 是一套和声配置。
type Harmony struct {
	Style   int
	Chords  []score.Segment // 时间为乐谱时间（秒，相对第一拍）
	Reasons []string        // 与 Chords 一一对应的理由
	// Melody 为旋律与和弦的匹配度、Prog 为进行的流畅度、Total 为综合分（0–100）。
	Melody, Prog, Total float64
}

// 进行与终止的权重（按风格）。
type styleWeights struct {
	changeMid, repeatBar float64
	fifth                float64 // 根音下行五度的奖励（负代价）
	dToS                 float64 // D → S 的转移代价
	startNonT            float64 // 第一个和弦不是主功能的代价
	finalTonic           float64 // 终止片为主和弦
	penultV              float64 // 倒数第二片为属和弦
	penultS              float64 // 倒数第二片为下属功能（IV、ii、iv）
	penultII             float64 // 爵士额外鼓励 ii–V（加在 penultS 上）
	penultBVII           float64
	penultSubV           float64
	halfCadence          float64 // 8 小节时第 4 小节末片不是主和弦
	finalSus             float64
	forbidVI             bool // 调式：禁止 V → I
}

var harmonyWeights = [4]styleWeights{
	StylePop: {changeMid: 1.2, repeatBar: 0.3, fifth: -0.3, dToS: 0.7, startNonT: 1.0, finalTonic: -2, penultV: -1,
		penultS: -0.6, penultBVII: -0.6, penultSubV: math.Inf(1), halfCadence: -0.8, finalSus: 1},
	StyleJazz: {changeMid: 0.2, repeatBar: 0.8, fifth: -0.5, dToS: 1.5, startNonT: 0.6, finalTonic: -2, penultV: -1,
		penultS: -0.6, penultII: -0.3, penultBVII: 0, penultSubV: -0.8, halfCadence: -0.8, finalSus: 1},
	StyleReharm: {changeMid: 0.6, repeatBar: 0.6, fifth: -0.4, dToS: 1.5, startNonT: 0.6, finalTonic: -2, penultV: -1,
		penultS: -0.6, penultBVII: -0.3, penultSubV: -0.8, halfCadence: -0.8, finalSus: 1},
	StyleModal: {changeMid: 1.0, repeatBar: 0.2, fifth: -0.2, dToS: 0.3, startNonT: 0.6, finalTonic: -2, penultV: 3,
		penultS: -1, penultBVII: -1, penultSubV: math.Inf(1), halfCadence: -0.5, finalSus: -0.2, forbidVI: true},
}

// 其他常量。
const (
	harmAnchorTone   = 0.3  // 片内最重的音是和弦音时的奖励（乘以该音权重）
	harmAnchorRoot   = 0.2  // 并且是根音或三音时的额外奖励
	harmRunExtra     = 1.0  // 同一和弦持续第 3 个小节起每小节的额外代价
	harmMustResolve  = 3.0  // 副属、替代、经过和弦没有解决的代价
	harmAltPenalty   = 0.8  // 求备选时对已选（片, 和弦）的初始惩罚
	harmAltStep      = 0.4  // 差异不够时惩罚的增量
	harmAltTries     = 4    //
	harmMinDist      = 0.35 // 备选与已有方案的最小差异
	harmAltDrop      = 15.0 // 备选的综合分比最优低超过该值时不显示
	harmSlowBPM      = 72   // 流行风格在慢速时允许小节中换和弦（否则一拍一个音的慢歌会整小节一个和弦）
	harmLeadingMax   = 0.25 // 旋律中导音的权重超过该比例时 Mixolydian 与旋律冲突，退回调内和弦
	harmHalfDominant = 0.5  // 半终止片用属和弦的额外奖励（与 penultS 同量级）
)

// toneCost 是各性质的和弦上各音程（相对根音的半音数）的代价：[0] 用于爵士与重配和声，[1] 用于流行与调式。
// 和弦音 0；可用张力音 0.2–0.8；避免音 1.0–2.0；其余 2.5–3.0。
var toneCost = func() map[string][2][12]float64 {
	type entry struct {
		chord        []int
		tension      map[int][2]float64
		avoid        map[int]float64
		other        float64
		otherByIndex map[int]float64
	}
	both := func(v float64) [2]float64 { return [2]float64{v, v} }
	table := map[string]entry{
		"maj":     {chord: []int{0, 4, 7}, tension: map[int][2]float64{2: {0.3, 0.5}, 9: {0.4, 0.6}}, avoid: map[int]float64{5: 2}, other: 3},
		"maj7":    {chord: []int{0, 4, 7, 11}, tension: map[int][2]float64{2: both(0.3), 9: both(0.4), 6: {0.8, 1.5}}, avoid: map[int]float64{5: 2}, other: 3},
		"6":       {chord: []int{0, 4, 7, 9}, tension: map[int][2]float64{2: both(0.3), 11: both(0.5)}, avoid: map[int]float64{5: 2}, other: 3},
		"min":     {chord: []int{0, 3, 7}, tension: map[int][2]float64{2: both(0.3), 5: both(0.3), 10: both(0.3), 9: {0.6, 1}}, avoid: map[int]float64{8: 1.5, 11: 1.5}, other: 3},
		"min7":    {chord: []int{0, 3, 7, 10}, tension: map[int][2]float64{2: both(0.3), 5: both(0.3), 9: {0.6, 1}}, avoid: map[int]float64{8: 1.5, 11: 1.5}, other: 3},
		"minmaj7": {chord: []int{0, 3, 7, 11}, tension: map[int][2]float64{2: both(0.3), 5: both(0.3), 9: both(0.5)}, avoid: map[int]float64{10: 2}, other: 3},
		"min6":    {chord: []int{0, 3, 7, 9}, tension: map[int][2]float64{2: both(0.3), 5: both(0.3), 11: both(0.6)}, avoid: map[int]float64{10: 1}, other: 3},
		"7": {chord: []int{0, 4, 7, 10}, tension: map[int][2]float64{2: both(0.3), 9: both(0.4), 1: {0.6, 1.5}, 3: {0.7, 2},
			8: {0.6, 1.5}, 6: {0.8, 1.5}}, avoid: map[int]float64{5: 2}, other: 3},
		"sus4": {chord: []int{0, 5, 7}, tension: map[int][2]float64{10: both(0.2), 2: both(0.3), 9: both(0.5)}, avoid: map[int]float64{4: 2}, other: 3},
		"sus2": {chord: []int{0, 2, 7}, tension: map[int][2]float64{4: both(0.5), 9: both(0.5), 10: both(0.8), 11: both(1)}, avoid: map[int]float64{5: 1}, other: 3},
		"dim":  {chord: []int{0, 3, 6}, tension: map[int][2]float64{2: both(0.5), 5: both(0.5), 8: both(0.5), 9: both(0.5), 11: both(0.5)}, other: 2.5},
		"dim7": {chord: []int{0, 3, 6, 9}, tension: map[int][2]float64{2: both(0.5), 5: both(0.5), 8: both(0.5), 11: both(0.5)}, other: 2.5},
		"hdim7": {chord: []int{0, 3, 6, 10}, tension: map[int][2]float64{5: both(0.3), 8: both(0.5), 2: both(0.8)},
			avoid: map[int]float64{1: 2}, other: 3},
		"aug": {chord: []int{0, 4, 8}, tension: map[int][2]float64{2: both(0.5), 6: both(0.8), 10: both(0.5)}, other: 2.5},
	}
	out := map[string][2][12]float64{}
	for q, e := range table {
		var c [2][12]float64
		for col := range 2 {
			for iv := range 12 {
				switch {
				case slices.Contains(e.chord, iv):
					c[col][iv] = 0
				case e.tension[iv] != [2]float64{}:
					c[col][iv] = e.tension[iv][col]
				case e.avoid[iv] != 0:
					c[col][iv] = e.avoid[iv]
				default:
					c[col][iv] = e.other
				}
			}
		}
		out[q] = c
	}
	return out
}()

// toneColumn 返回风格使用的代价列：爵士与重配和声对张力音更宽容。
func toneColumn(style int) int {
	if style == StyleJazz || style == StyleReharm {
		return 0
	}
	return 1
}

// harmCandidates 返回调 tonic / mode 下的候选和弦（按罗马数字与性质固定排序，保证结果可复现）。
// modal 为 true 时返回调式风格的和弦（大调 → Mixolydian，小调 → Dorian）。
func harmCandidates(tonic int, mode string, modal bool) []chordCand {
	var out []chordCand
	seen := map[[2]any]bool{}
	add := func(rel int, quality string, kind, fn, target int, roman, group string) {
		key := [2]any{(rel%12 + 12) % 12, quality}
		if seen[key] {
			return
		}
		seen[key] = true
		r := (rel%12 + 12) % 12
		out = append(out, chordCand{root: (tonic + r) % 12, rel: r, quality: quality, kind: kind, fn: fn, target: target,
			roman: roman, group: group})
	}
	minor := mode == "minor"
	if modal {
		if minor { // Dorian
			for _, d := range []struct {
				rel int
				qs  []string
				fn  int
				g   string
			}{{0, []string{"min", "min7"}, funcT, ""}, {2, []string{"min", "min7"}, funcS, ""}, {3, []string{"maj", "maj7"}, funcT, ""},
				{5, []string{"maj", "7"}, funcS, "feature"}, {7, []string{"min", "min7"}, funcD, ""}, {9, []string{"dim", "hdim7"}, funcD, ""},
				{10, []string{"maj"}, funcD, ""}} {
				for _, q := range d.qs {
					add(d.rel, q, kindModal, d.fn, -1, "", d.g)
				}
			}
		} else { // Mixolydian
			for _, d := range []struct {
				rel int
				qs  []string
				fn  int
				g   string
			}{{0, []string{"maj", "7", "sus4"}, funcT, ""}, {2, []string{"min", "min7"}, funcS, ""}, {4, []string{"dim"}, funcT, ""},
				{5, []string{"maj", "maj7"}, funcS, ""}, {7, []string{"min", "min7"}, funcD, ""}, {9, []string{"min", "min7"}, funcT, ""},
				{10, []string{"maj", "maj7"}, funcD, "feature"}} {
				for _, q := range d.qs {
					add(d.rel, q, kindModal, d.fn, -1, "", d.g)
				}
			}
		}
		add(0, "sus2", kindModal, funcT, -1, "", "")
		return out
	}
	type deg struct {
		rel int
		qs  []string
		fn  int
	}
	var diatonic []deg
	var secTargets []int // 副属的目标（相对主音）
	if minor {
		diatonic = []deg{{0, []string{"min", "min7", "min6", "minmaj7"}, funcT}, {2, []string{"dim", "hdim7"}, funcS},
			{3, []string{"maj", "maj7"}, funcT}, {5, []string{"min", "min7"}, funcS}, {7, []string{"maj", "7", "min", "min7"}, funcD},
			{8, []string{"maj", "maj7"}, funcS}, {10, []string{"maj", "7"}, funcD}, {11, []string{"dim7"}, funcD}}
		secTargets = []int{5, 7, 8, 10, 3}
	} else {
		diatonic = []deg{{0, []string{"maj", "maj7", "6", "sus2", "sus4"}, funcT}, {2, []string{"min", "min7"}, funcS},
			{4, []string{"min", "min7"}, funcT}, {5, []string{"maj", "maj7", "6", "sus2"}, funcS}, {7, []string{"maj", "7", "sus4"}, funcD},
			{9, []string{"min", "min7"}, funcT}, {11, []string{"dim", "hdim7"}, funcD}}
		secTargets = []int{2, 4, 5, 7, 9}
	}
	for _, d := range diatonic {
		for _, q := range d.qs {
			add(d.rel, q, kindDiatonic, d.fn, -1, "", "")
		}
	}
	// 目标是大三类（大调的 IV、V，小调的 V、♭VI、♭VII、♭III）还是小三类，决定 ii/x 用 min7 还是 hdim7。
	majorTarget := func(rel int) bool {
		if minor {
			return rel != 5
		}
		return rel == 5 || rel == 7
	}
	for _, x := range secTargets {
		group := "common"
		if !minor && (x == 2 || x == 4) {
			group = "rare"
		}
		add(x+7, "7", kindSecDom, funcD, x, "", group)
		add(x+7, "maj", kindSecDom, funcD, x, "", group)
		iiQ := "hdim7"
		if majorTarget(x) {
			iiQ = "min7"
		}
		add(x+2, iiQ, kindSecII, funcD, (x+7)%12, "", "")
		add(x+1, "7", kindSubV, funcD, x, "", "")
	}
	if minor {
		add(5, "maj", kindBorrowed, funcS, -1, "", "common")
		add(2, "min7", kindBorrowed, funcS, -1, "", "rare")
		add(0, "maj", kindBorrowed, funcT, -1, "", "picardy")
	} else {
		add(5, "min", kindBorrowed, funcS, -1, "", "common")
		add(5, "min7", kindBorrowed, funcS, -1, "", "common")
		add(8, "maj", kindBorrowed, funcS, -1, "", "common")
		add(8, "maj7", kindBorrowed, funcS, -1, "", "common")
		add(10, "maj", kindBorrowed, funcD, -1, "", "common")
		add(10, "7", kindBorrowed, funcD, -1, "", "common")
		add(3, "maj", kindBorrowed, funcT, -1, "", "rare")
		add(2, "hdim7", kindBorrowed, funcS, -1, "", "rare")
		add(1, "maj7", kindBorrowed, funcS, -1, "", "rare")
		for _, p := range [][2]int{{1, 2}, {3, 4}, {6, 7}, {8, 9}} {
			add(p[0], "dim7", kindDimPass, funcD, p[1], "", "")
		}
	}
	return out
}

// candPrior 返回候选在风格下的一元先验代价（+Inf 为不启用）。
func candPrior(c chordCand, style int, mode string, final bool) float64 {
	inf := math.Inf(1)
	pick := func(v [4]float64) float64 { return v[style] }
	triad := c.quality == "maj" || c.quality == "min" || c.quality == "dim" || c.quality == "aug"
	switch c.kind {
	case kindModal:
		if style != StyleModal {
			return inf
		}
		p := 0.0
		switch c.quality {
		case "maj", "min":
			p = 0.1
		case "dim":
			p = 0.8
		case "maj7":
			p = 1.5
		case "7":
			p = 0.4
		case "hdim7":
			p = 0.8
		}
		if c.group == "feature" {
			p -= 0.3
		}
		return p
	case kindDiatonic:
		if style == StyleModal {
			return inf
		}
		deg := 0.0
		switch {
		case c.rel == 4 && mode == "major":
			deg = pick([4]float64{0.4, 0.2, 0.2, 0.3})
		case c.rel == 2 && mode == "major":
			deg = pick([4]float64{0.2, 0, 0, 0})
		case c.quality == "dim" || c.quality == "hdim7" || c.quality == "dim7":
			deg = pick([4]float64{1, 0.4, 0.4, 0.8})
		}
		var q float64
		switch {
		case triad:
			q = pick([4]float64{0, 0.8, 0.3, 0.1})
			if final && style == StyleJazz {
				q = 0.3
			}
		case c.quality == "maj7":
			q = pick([4]float64{0.6, 0, 0, 1.5})
		case c.quality == "min7" || c.quality == "hdim7" || c.quality == "dim7":
			q = pick([4]float64{0.3, 0, 0, 0})
		case c.quality == "7":
			q = pick([4]float64{0.2, 0, 0, 0})
		case c.quality == "6" || c.quality == "min6":
			q = pick([4]float64{0.4, 0.1, 0.2, 0.5})
		case c.quality == "sus2" || c.quality == "sus4":
			q = pick([4]float64{0.3, 0.4, 0.3, 0})
			if style == StyleJazz && c.rel == 7 {
				q = 0.1
			}
		case c.quality == "minmaj7":
			q = pick([4]float64{1, 0.3, 0.3, 0})
		}
		if style == StyleReharm {
			deg += 0.3 // 重配和声：调内和弦让位给副属、借用与替代
		}
		return deg + q
	case kindSecDom:
		if c.group == "rare" {
			return pick([4]float64{inf, 0.4, 0, inf})
		}
		return pick([4]float64{0.8, 0.3, 0, inf})
	case kindSecII:
		return pick([4]float64{inf, 0.3, 0.2, inf})
	case kindSubV:
		return pick([4]float64{inf, 0.6, 0.3, inf})
	case kindBorrowed:
		switch c.group {
		case "picardy":
			if !final {
				return inf
			}
			return pick([4]float64{0.5, 0.5, 0.3, inf})
		case "rare":
			return pick([4]float64{inf, 1, 0.4, inf})
		}
		return pick([4]float64{0.7, 0.6, 0.2, inf})
	case kindDimPass:
		return pick([4]float64{inf, 0.6, 0.3, inf})
	}
	return inf
}

// harmSlices 把旋律按和声节奏切片并给每个音加权（时值 × 拍位 × 上下文）。4/4 每小节两片，复合拍每拍（附点四分
// 音符）一片，其余每小节一片。
func harmSlices(h hum.Hum) []harmSlice {
	s := h.Settings
	beat := s.Beat()
	bpb := s.BeatsPerBar
	per := 1
	switch {
	case s.Compound:
		per = bpb
	case bpb == 4:
		per = 2
	}
	sliceBeats := float64(bpb) / float64(per)
	var out []harmSlice
	for b := range s.Bars {
		for k := range per {
			st := float64(b*bpb) + float64(k)*sliceBeats
			out = append(out, harmSlice{start: st, end: st + sliceBeats, barStart: k == 0})
		}
	}
	notes := h.Notes
	pcOf := func(n score.MelodyNote) int { return ((int(math.Round(n.Pitch)) % 12) + 12) % 12 }
	for i, n := range notes {
		ns, ne := n.Start/beat, n.End/beat
		// 上下文：经过音（前后级进同向）、邻音（级进离开又返回）、转音。
		ctx := 1.0
		inBar := math.Mod(ns, float64(bpb))
		if i > 0 && i+1 < len(notes) && ne-ns <= 1+1e-9 && inBar > 1e-6 {
			d1 := notes[i].Pitch - notes[i-1].Pitch
			d2 := notes[i+1].Pitch - notes[i].Pitch
			switch {
			case math.Abs(d1) <= 2 && math.Abs(d2) <= 2 && d1*d2 > 0:
				ctx = 0.3
			case math.Abs(d1) <= 2 && math.Abs(d2) <= 2 && d1*d2 < 0 && notes[i+1].Pitch == notes[i-1].Pitch:
				ctx = 0.35
			}
		}
		if n.Slur {
			ctx *= 0.7
		}
		for j := range out {
			sl := &out[j]
			lo, hi := max(ns, sl.start), min(ne, sl.end)
			if hi <= lo+1e-9 {
				continue
			}
			pos := 0.8 // 跨过片首而延续的音
			if ns >= sl.start-1e-9 {
				frac := ns - math.Floor(ns+1e-9)
				switch {
				case math.Abs(inBar) < 1e-6:
					pos = 1
				case math.Abs(ns-sl.start) < 1e-6:
					pos = 0.8
				case frac < 1e-6:
					pos = 0.6
				case !s.Compound && math.Abs(frac-0.5) < 1e-6,
					s.Compound && math.Abs(frac*3-math.Round(frac*3)) < 1e-6: // 拍内的八分音符
					pos = 0.35
				default:
					pos = 0.25
				}
			}
			w := min(hi-lo, 2) * pos * ctx
			sl.notes = append(sl.notes, harmNote{pcOf(n), w})
			sl.weight += w
		}
	}
	return out
}

// sliceCost 是旋律在候选和弦上的匹配代价（不含先验）。
func sliceCost(sl harmSlice, c chordCand, col int) float64 {
	if len(sl.notes) == 0 {
		return 0
	}
	tc := toneCost[c.quality][col]
	cost, wmax, top := 0.0, 0.0, -1
	for i, n := range sl.notes {
		iv := ((n.pc-c.root)%12 + 12) % 12
		cost += n.w * tc[iv]
		if n.w > wmax {
			wmax, top = n.w, i
		}
	}
	// 片内最重的音是和弦音时，和弦被旋律「认出」。
	iv := ((sl.notes[top].pc-c.root)%12 + 12) % 12
	if tc[iv] == 0 {
		cost -= harmAnchorTone * wmax
		if iv == 0 || iv == 3 || iv == 4 {
			cost -= harmAnchorRoot * wmax
		}
	}
	return cost
}

// transCost 是从和弦 a 换到和弦 b 的进行代价（不含小节中换和弦的代价与终止奖励）。
func transCost(a, b chordCand, style int) float64 {
	w := harmonyWeights[style]
	fn := func(c chordCand) int {
		if c.kind == kindBorrowed && c.rel == 10 && style == StyleJazz {
			return funcS
		}
		return c.fn
	}
	matrix := [3][3]float64{{0.6, 0.2, 0.5}, {0.5, 0.5, 0}, {0, w.dToS, 0.8}}
	cost := matrix[fn(a)][fn(b)]
	// 根音进行。
	switch iv := ((b.root-a.root)%12 + 12) % 12; iv {
	case 5:
		cost += w.fifth
	case 7:
		cost -= 0.1
	case 2:
		cost += 0.1
	case 6:
		if a.kind != kindSubV && b.kind != kindSubV {
			cost += 0.5
		}
	}
	// 必须解决：副属、ii/x、替代、经过和弦。
	switch a.kind {
	case kindSecDom:
		switch {
		case b.rel == a.target:
		case (b.kind == kindSubV || b.kind == kindSecII) && b.target == a.target:
			cost += 0.4 // 延迟解决，形成链
		case a.target == 9 && b.rel == 5:
			cost += 0.8 // V/vi → IV 的伪解决
		default:
			cost += harmMustResolve
		}
	case kindSecII:
		switch {
		case b.kind == kindSecDom && b.rel == a.target:
		case b.rel == (a.target+5)%12:
			cost += 1
		default:
			cost += harmMustResolve
		}
	case kindSubV, kindDimPass:
		if b.rel != a.target {
			cost += harmMustResolve
		}
	}
	if a.kind == kindSubV && b.rel == a.target {
		cost -= 0.5 // 根音下行半音解决
	}
	if a.kind == kindDimPass && b.rel == a.target {
		cost -= 0.3
	}
	if a.kind == kindBorrowed && b.kind == kindBorrowed && style != StyleModal {
		cost += 0.3
	}
	if a.kind == kindBorrowed && a.rel == 10 && b.rel == 0 {
		cost -= 0.2 // ♭VII → I
	}
	if a.kind == kindBorrowed && a.rel == 10 && b.rel == 5 {
		cost += 0.3
	}
	if w.forbidVI && a.rel == 7 && (a.quality == "maj" || a.quality == "7") && b.rel == 0 {
		cost += math.Inf(1)
	}
	return cost
}

// isTonic 报告候选是否可作为终止的主和弦。
func isTonic(c chordCand, mode string, style int) bool {
	if c.rel != 0 {
		return false
	}
	if style == StyleModal {
		return c.quality != "sus2" && c.quality != "sus4"
	}
	if mode == "minor" {
		return slices.Contains([]string{"min", "min7", "min6", "minmaj7"}, c.quality) || c.group == "picardy"
	}
	return slices.Contains([]string{"maj", "maj7", "6"}, c.quality)
}

// cadenceBonus 返回把和弦 c 放在终止角色为 role 的片上的代价（负值为奖励）；scale 为终止权重（2 小节的动机减半）。
func cadenceBonus(c chordCand, role int, mode string, style int, scale float64) float64 {
	w := harmonyWeights[style]
	cost := 0.0
	switch role {
	case roleFinal:
		switch {
		case isTonic(c, mode, style):
			cost += w.finalTonic * scale
		case c.rel == 0 && (c.quality == "sus2" || c.quality == "sus4"):
			cost += w.finalSus
		case scale < 1 && c.rel == 7 && c.kind == kindDiatonic:
			cost += w.finalTonic * scale // 2 小节的开放片段：落在 V 上也可以
		}
	case rolePenult:
		switch {
		case c.rel == 7 && (c.quality == "maj" || c.quality == "7" || c.quality == "sus4"):
			cost += w.penultV * scale
		case c.rel == 5 || (c.rel == 2 && c.fn == funcS):
			cost += w.penultS * scale
			if c.rel == 2 {
				cost += w.penultII * scale
			}
		case c.rel == 10:
			cost += w.penultBVII * scale
		case c.rel == 1 && c.quality == "7":
			cost += w.penultSubV * scale
		}
	case roleHalf:
		if c.rel != 0 { // 根音是主音的和弦（含 Isus2、Isus4）都不构成半终止
			cost += w.halfCadence
		}
		if c.rel == 7 && (c.quality == "maj" || c.quality == "7" || c.quality == "sus4") {
			cost -= harmHalfDominant // 半终止落在属和弦上最典型
		}
	}
	return cost
}

// 和声时间片在终止式中的角色（cadenceBonus）。
const (
	roleNone   = iota
	roleFinal  // 全终止的主和弦片
	rolePenult // 全终止前一片（属、下属等）
	roleHalf   // 半终止片
)

// ChordLock 把乐谱时间 [Start, End)（秒）内起始的和声时间片固定为根音 Root（音级）、性质 Quality 的和弦。
type ChordLock struct {
	Start, End float64
	Root       int
	Quality    string
}

// 终止式的种类（CadencePoint.Kind）。
const (
	CadHalf = iota + 1 // 半终止：落在属和弦等非主和弦上
	CadFull            // 全终止：落在主和弦
)

// CadencePoint 是一个终止点：乐谱时间 At（秒）所在的和声时间片按 Kind 计终止奖励。
type CadencePoint struct {
	At   float64
	Kind int
}

// 段落的和声区域（RegionBias.Region）。
const (
	RegionTonic    = iota // 不加倾向
	RegionSub             // 下属方向：大调 IV、ii，小调 iv、♭VI
	RegionRelative        // 关系调：大调 vi、iii，小调 ♭III、♭VII
)

// RegionBias 让片首落在 [Start, End)（秒）内的时间片倾向某个和声区域：区域内的根音代价减 Weight，主和弦加 Weight。
type RegionBias struct {
	Start, End float64
	Region     int
	Weight     float64
}

// HarmonyOptions 是 Harmonize 的附加约束；零值为不加约束。
type HarmonyOptions struct {
	// Locks 是用户锁定或替换的和弦：被覆盖的时间片只能用该和弦，其余部分照常求解并与之衔接。
	Locks []ChordLock
	// Cadences 非空时取代自动判定的终止位置（最后一个起音所在的片为全终止、8 小节时第 4 小节末为半终止）：
	// 各全终止片与其前一片计终止奖励，最后一个全终止之后只能保持。用于动机发展后按句式给出的终止。
	Cadences []CadencePoint
	// Regions 是段落的和声区域倾向（例如 B 段倾向下属方向）。
	Regions []RegionBias
}

// Harmonize 为乐句配 style 风格的和声，返回最多 n 套彼此差异足够大的方案：第一套为模型代价最低的路径，之后为加惩罚
// 重解得到的备选（综合分比第一套低超过 harmAltDrop 的不返回）。乐句没有音符时为 nil。
func Harmonize(h hum.Hum, style, n int) []Harmony {
	return HarmonizeWith(h, style, n, HarmonyOptions{})
}

// HarmonizeWith 同 Harmonize，并满足 opt 的约束。锁定的和弦不在候选表里时按根音相对主音的位置归入主 / 下属 / 属功能
// 加入候选（没有解决要求），理由标为手动指定。
func HarmonizeWith(h hum.Hum, style, n int, opt HarmonyOptions) []Harmony {
	if len(h.Notes) == 0 || h.Settings.Bars <= 0 {
		return nil
	}
	sls := harmSlices(h)
	mode := h.Mode
	// 调式风格：旋律里导音分量过重时 Mixolydian 与旋律冲突，退回调内和弦（仍禁止 V → I）。
	modal := style == StyleModal
	if modal && mode == "major" {
		lead, total := 0.0, 0.0
		for _, sl := range sls {
			for _, nt := range sl.notes {
				total += nt.w
				if ((nt.pc-h.TonicPC)%12+12)%12 == 11 {
					lead += nt.w
				}
			}
		}
		if total > 0 && lead/total > harmLeadingMax {
			modal = false
		}
	}
	cands := harmCandidates(h.TonicPC, mode, modal)
	// 锁定：forced[s] 为第 s 片固定的候选序号（-1 为不固定）。
	beat := h.Settings.Beat()
	forced := make([]int, len(sls))
	for s := range forced {
		forced[s] = -1
	}
	for _, l := range opt.Locks {
		if _, ok := score.QualityIntervals[l.Quality]; !ok {
			continue
		}
		root := ((l.Root % 12) + 12) % 12
		k := slices.IndexFunc(cands, func(c chordCand) bool { return c.root == root && c.quality == l.Quality })
		if k < 0 {
			rel := ((root-h.TonicPC)%12 + 12) % 12
			fn := funcD
			switch rel {
			case 0, 3, 4, 8, 9:
				fn = funcT
			case 2, 5:
				fn = funcS
			}
			cands = append(cands, chordCand{root: root, rel: rel, quality: l.Quality, kind: kindLocked, fn: fn, target: -1})
			k = len(cands) - 1
		}
		for s, sl := range sls {
			if t := sl.start * beat; t >= l.Start-1e-9 && t < l.End-1e-9 {
				forced[s] = k
			}
		}
	}
	priorStyle := style
	if style == StyleModal && !modal {
		priorStyle = StylePop // 退回调内和弦时按流行的先验，进行仍按调式的权重
	}
	w := harmonyWeights[style]
	if style == StylePop && h.Settings.BPM < harmSlowBPM {
		w.changeMid = harmonyWeights[StyleJazz].changeMid
	}
	// 终止角色：给出了终止点时按终止点；否则最后一个起音所在的片为全终止，8 小节时第 4 小节末片为半终止。
	// final 是最后一个全终止片，之后的空片只能保持（锁定的除外）。
	role := make([]int, len(sls))
	sliceAt := func(sec float64) int {
		k := 0
		for i, sl := range sls {
			if sec/beat >= sl.start-1e-9 {
				k = i
			}
		}
		return k
	}
	final := 0
	if len(opt.Cadences) > 0 {
		for _, cp := range opt.Cadences {
			if k := sliceAt(cp.At); cp.Kind == CadHalf {
				role[k] = roleHalf
			} else {
				final = max(final, k)
			}
		}
		for _, cp := range opt.Cadences {
			if k := sliceAt(cp.At); cp.Kind == CadFull {
				role[k] = roleFinal
				if k > 0 && role[k-1] == roleNone {
					role[k-1] = rolePenult
				}
			}
		}
	} else {
		final = sliceAt(h.Notes[len(h.Notes)-1].Start)
		if h.Settings.Bars == 8 {
			for i, sl := range sls {
				if sl.end <= float64(4*h.Settings.BeatsPerBar)+1e-9 {
					role[i] = roleHalf
				}
			}
			for i := range role {
				if role[i] == roleHalf && i+1 < len(role) && role[i+1] == roleHalf {
					role[i] = roleNone // 只有第 4 小节末的那一片
				}
			}
		}
		role[final] = roleFinal
		if final > 0 {
			role[final-1] = rolePenult
		}
	}
	// 段落区域倾向：regionCost[s] 返回候选在第 s 片上的额外代价。
	regionCost := func(s int, c chordCand) float64 {
		cost := 0.0
		for _, rb := range opt.Regions {
			if t := sls[s].start * beat; t < rb.Start-1e-9 || t >= rb.End-1e-9 {
				continue
			}
			var roots []int
			switch {
			case rb.Region == RegionSub && mode == "minor":
				roots = []int{5, 8}
			case rb.Region == RegionSub:
				roots = []int{5, 2}
			case rb.Region == RegionRelative && mode == "minor":
				roots = []int{3, 10}
			case rb.Region == RegionRelative:
				roots = []int{9, 4}
			}
			switch {
			case slices.Contains(roots, c.rel):
				cost -= rb.Weight
			case c.rel == 0:
				cost += rb.Weight
			}
		}
		return cost
	}
	scale := 1.0
	if h.Settings.Bars <= 2 {
		scale = 0.5
	}
	col := toneColumn(style)
	// 一元代价表 E[s][k] = 先验 + 旋律匹配 + 终止。
	E := make([][]float64, len(sls))
	match := make([][]float64, len(sls))
	for s, sl := range sls {
		E[s] = make([]float64, len(cands))
		match[s] = make([]float64, len(cands))
		for k, c := range cands {
			match[s][k] = sliceCost(sl, c, col)
			switch {
			case forced[s] >= 0 && k != forced[s]:
				E[s][k] = math.Inf(1)
				continue
			case forced[s] == k:
				E[s][k] = 0 // 锁定的和弦不计先验（可能是本风格不启用的和弦）
			default:
				E[s][k] = candPrior(c, priorStyle, mode, s == final)
			}
			E[s][k] += match[s][k] + cadenceBonus(c, role[s], mode, style, scale) + regionCost(s, c)
			if s == 0 && c.fn != funcT {
				E[s][k] += w.startNonT
			}
		}
	}
	T := make([][]float64, len(cands))
	for a := range cands {
		T[a] = make([]float64, len(cands))
		for b := range cands {
			T[a][b] = transCost(cands[a], cands[b], style)
		}
	}
	var results []Harmony
	var paths [][]int
	penalty := map[[2]int]float64{}
	for len(results) < n {
		var path []int
		accepted := false
		lambda := harmAltPenalty
		for try := 0; try <= harmAltTries; try++ {
			path = harmViterbi(sls, E, T, w, final, forced, role, penalty)
			if path == nil {
				break
			}
			if len(paths) == 0 {
				accepted = true
				break
			}
			ok := true
			for _, p := range paths {
				if harmDist(sls, cands, p, path) < harmMinDist {
					ok = false
				}
			}
			if ok {
				accepted = true
				break
			}
			for s, k := range path {
				penalty[[2]int{s, k}] += lambda
			}
			lambda += harmAltStep
		}
		if !accepted {
			break
		}
		r := harmResult(h, sls, cands, path, T, style, forced)
		if len(results) > 0 && r.Total < results[0].Total-harmAltDrop {
			break
		}
		results, paths = append(results, r), append(paths, path)
		for s, k := range path {
			penalty[[2]int{s, k}] += harmAltPenalty
		}
	}
	// 方案的先后即求解的先后：第一个是模型代价最低的路径；综合分只用于显示与剔除差得太多的备选。
	return results
}

// harmViterbi 在（片, 和弦, 连续小节数）上求最小代价路径；forced 为锁定的片（终止之后也允许换到锁定的和弦），
// role 为各片的终止角色（终止相关的片在小节中换和弦不计 changeMid：终止处和声节奏加快），
// penalty 为求备选时对（片, 和弦）的额外代价。
func harmViterbi(sl []harmSlice, E, T [][]float64, w styleWeights, final int, forced, role []int, penalty map[[2]int]float64) []int {
	K := len(E[0])
	const runs = 3
	inf := math.Inf(1)
	V := make([][][runs]float64, len(sl))
	B := make([][][runs][2]int, len(sl))
	for s := range sl {
		V[s] = make([][runs]float64, K)
		B[s] = make([][runs][2]int, K)
		for k := range K {
			for r := range runs {
				V[s][k][r] = inf
			}
		}
	}
	for k := range K {
		V[0][k][0] = E[0][k] + penalty[[2]int{0, k}]
	}
	for s := 1; s < len(sl); s++ {
		bar := sl[s].barStart
		for b := range K {
			e := E[s][b] + penalty[[2]int{s, b}]
			if math.IsInf(e, 1) {
				continue
			}
			for a := range K {
				for r := range runs {
					prev := V[s-1][a][r]
					if math.IsInf(prev, 1) {
						continue
					}
					var c float64
					nr := 0
					switch {
					case a == b && bar:
						nr = min(r+1, runs-1)
						c = w.repeatBar
						if r+1 >= runs-1 {
							c += harmRunExtra
						}
					case a == b:
						nr = r
					case s > final && forced[s] < 0:
						continue // 终止之后只保持
					default:
						c = T[a][b]
						if !bar && role[s] == roleNone {
							c += w.changeMid
						}
					}
					if v := prev + c + e; v < V[s][b][nr] {
						V[s][b][nr] = v
						B[s][b][nr] = [2]int{a, r}
					}
				}
			}
		}
	}
	last := len(sl) - 1
	bk, br, best := -1, 0, inf
	for k := range K {
		for r := range runs {
			if V[last][k][r] < best {
				bk, br, best = k, r, V[last][k][r]
			}
		}
	}
	if bk < 0 {
		return nil
	}
	path := make([]int, len(sl))
	for s := last; s >= 0; s-- {
		path[s] = bk
		if s > 0 {
			p := B[s][bk][br]
			bk, br = p[0], p[1]
		}
	}
	return path
}

// harmDist 是两条和弦路径的差异（0–1）：按片内旋律权重加权；同根同性质 0，同根异性质 0.3，同功能且共有至少两个音 0.5，其余 1。
func harmDist(sl []harmSlice, cands []chordCand, p, q []int) float64 {
	var sum, total float64
	for s := range sl {
		w := sl[s].weight
		if w == 0 {
			w = 0.5
		}
		a, b := cands[p[s]], cands[q[s]]
		d := 1.0
		switch {
		case a.root == b.root && a.quality == b.quality:
			d = 0
		case a.root == b.root:
			d = 0.3
		case a.fn == b.fn:
			common := 0
			for _, x := range score.PitchClasses(a.root, a.quality) {
				if slices.Contains(score.PitchClasses(b.root, b.quality), x) {
					common++
				}
			}
			if common >= 2 {
				d = 0.5
			}
		}
		sum, total = sum+w*d, total+w
	}
	if total == 0 {
		return 0
	}
	return sum / total
}

// harmResult 把路径合并成和弦段，写理由并评分。
func harmResult(h hum.Hum, sl []harmSlice, cands []chordCand, path []int, T [][]float64, style int, forced []int) Harmony {
	beat := h.Settings.Beat()
	flats := score.KeySignature(h.TonicPC, h.Mode) < 0
	r := Harmony{Style: style}
	// 匹配度只看旋律音在和弦上的音程代价（不含「被旋律认出」的奖励），进行分只计正的转移代价（奖励不抵扣），
	// 两者才有区分度。
	col := toneColumn(style)
	var ms, mw, tsum float64
	for s := range sl {
		c := cands[path[s]]
		for _, n := range sl[s].notes {
			ms += n.w * toneCost[c.quality][col][((n.pc-c.root)%12+12)%12]
		}
		mw += sl[s].weight
		if s > 0 && path[s] != path[s-1] {
			tsum += max(T[path[s-1]][path[s]], 0)
		}
	}
	type seg struct{ k, s0, s1 int }
	var segs []seg
	for s, k := range path {
		if len(segs) > 0 && segs[len(segs)-1].k == k {
			segs[len(segs)-1].s1 = s
			continue
		}
		segs = append(segs, seg{k, s, s})
	}
	for i, g := range segs {
		c := cands[g.k]
		root := c.root
		r.Chords = append(r.Chords, score.Segment{Start: sl[g.s0].start * beat, End: sl[g.s1].end * beat,
			Label: score.PitchName(c.root, chordFlats(c, flats)) + score.StandardSuffix[c.quality], Root: &root, Quality: c.quality})
		var next *chordCand
		if i+1 < len(segs) {
			next = &cands[segs[i+1].k]
		}
		reason := harmReason(c, next, sl[g.s0], h, flats)
		if slices.ContainsFunc(forced[g.s0:g.s1+1], func(k int) bool { return k >= 0 }) {
			reason += "（已锁定）"
		}
		r.Reasons = append(r.Reasons, reason)
	}
	if mw > 0 {
		r.Melody = 100 * min(max(1-(ms/mw)/2, 0), 1)
	} else {
		r.Melody = 100
	}
	r.Prog = 100 * min(max(1-tsum/(1.5*float64(len(sl))), 0), 1)
	r.Total = math.Round(0.7*r.Melody + 0.3*r.Prog)
	return r
}

// chordFlats 返回和弦根音是否用降号拼写：降级数（♭II、♭III、♭VI、♭VII）的根音用降号（C 大调的 B♭7 而不是 A♯7），
// 其余按调号。
func chordFlats(c chordCand, keyFlats bool) bool {
	switch c.rel {
	case 1, 3, 8, 10:
		return true
	}
	return keyFlats
}

// romanOf 返回候选的罗马数字（大调音阶为基准；小三家族小写）。
func romanOf(rel int, quality string) string {
	deg := score.RomanDegree[rel]
	if score.MinorFamily[quality] {
		deg = strings.ToLower(deg)
	}
	return deg + score.RomanSuffix[quality]
}

// harmReason 写一个和弦的理由。
func harmReason(c chordCand, next *chordCand, sl harmSlice, h hum.Hum, flats bool) string {
	name := romanOf(c.rel, c.quality)
	targetName := func(rel int) string {
		q := "maj"
		if h.Mode == "minor" && (rel == 5 || rel == 0) || h.Mode == "major" && (rel == 2 || rel == 4 || rel == 9) {
			q = "min"
		}
		return romanOf(rel, q)
	}
	fnName := []string{"主", "下属", "属"}[c.fn]
	// 片内最重的旋律音在和弦上的角色。
	role := ""
	if len(sl.notes) > 0 {
		top := sl.notes[0]
		for _, n := range sl.notes {
			if n.w > top.w {
				top = n
			}
		}
		iv := ((top.pc-c.root)%12 + 12) % 12
		tc := toneCost[c.quality][0][iv]
		switch {
		case tc == 0:
			role = fmt.Sprintf("，旋律 %s 是和弦音", score.PitchName(top.pc, flats))
		case tc < 1:
			role = fmt.Sprintf("，旋律 %s 是张力音", score.PitchName(top.pc, flats))
		}
	}
	switch c.kind {
	case kindSecDom:
		return fmt.Sprintf("V7/%s：副属和弦，解决到 %s", targetName(c.target), targetName(c.target)) + role
	case kindSecII:
		return fmt.Sprintf("%s：与随后的属和弦构成 ii–V", name) + role
	case kindSubV:
		return fmt.Sprintf("♭II7/%s：属和弦的三全音替代，根音下行半音到 %s", targetName(c.target), targetName(c.target)) + role
	case kindBorrowed:
		from := "小调"
		if h.Mode == "minor" {
			from = "大调"
		}
		return fmt.Sprintf("%s：借自同主音%s（调式交替）", name, from) + role
	case kindDimPass:
		return fmt.Sprintf("%s：减七经过和弦，半音连接到 %s", name, targetName(c.target)) + role
	case kindLocked:
		return fmt.Sprintf("%s：手动指定的和弦", name) + role
	case kindModal:
		mode := "Mixolydian"
		if h.Mode == "minor" {
			mode = "Dorian"
		}
		if c.group == "feature" {
			return fmt.Sprintf("%s：%s 特征和弦，不用导音", name, mode) + role
		}
		return fmt.Sprintf("%s：%s 调式和弦", name, mode) + role
	}
	if c.quality == "sus4" || c.quality == "sus2" {
		return fmt.Sprintf("%s：挂留和弦，避开三音与旋律的冲突", name) + role
	}
	s := fmt.Sprintf("%s：调内%s功能和弦", name, fnName) + role
	if next != nil {
		switch {
		case c.rel == 7 && next.rel == 0 && (c.quality == "7" || c.quality == "maj"):
			s += "；V → I 正格终止"
		case c.rel == 5 && next.rel == 0:
			s += "；IV → I 变格终止"
		}
	}
	return s
}
