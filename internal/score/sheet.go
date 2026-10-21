package score

import (
	"fmt"
	"strings"
)

// 乐谱：把和弦序列按小节与拍排成谱面（爵士谱的和弦记号、五线谱的音高与时值），以及合成和弦伴奏用的音高。

// BeatSlot 是小节中的一拍。
type BeatSlot struct {
	Time, End float64
	// Chord 是这一拍上的和弦（Label 为空表示无和弦），Starts 为 true 表示和弦从这一拍开始。
	Chord  Segment
	Starts bool
}

// Measure 是一个小节。
type Measure struct {
	Number     int // 从 1 开始
	Start, End float64
	Beats      []BeatSlot
	// Simile 为 true 表示与上一小节的和弦完全相同（爵士谱中记为反复记号 %）。
	Simile bool
}

// Measures 按小节线与拍点排出全曲的小节。第一个小节线之前的弱起并入第一小节；没有小节线时返回 nil。
func (a *Analysis) Measures() []Measure {
	bars := a.Bars()
	out := make([]Measure, 0, len(bars))
	var prev Segment
	for i, b := range bars {
		m := Measure{Number: i + 1, Start: b.Start, End: b.End}
		var times []float64
		for _, t := range a.Beats {
			if t >= b.Start-1e-3 && t < b.End-1e-3 {
				times = append(times, t)
			}
		}
		if len(times) == 0 || times[0] > b.Start+1e-3 {
			times = append([]float64{b.Start}, times...)
		}
		for j, t := range times {
			end := b.End
			if j+1 < len(times) {
				end = times[j+1]
			}
			// 取拍点稍后的位置：和弦边界与拍点几乎重合，正好落在边界上时归到新和弦。
			seg := Segment{}
			if k := a.ChordAt(t + 0.05); k >= 0 && a.Chords[k].Label != "N" {
				seg = a.Chords[k]
			}
			starts := seg.Label != "" && (seg.Label != prev.Label || j == 0 && seg.Start >= b.Start-1e-3)
			m.Beats = append(m.Beats, BeatSlot{Time: t, End: end, Chord: seg, Starts: starts})
			prev = seg
		}
		out = append(out, m)
	}
	for i := 1; i < len(out); i++ {
		out[i].Simile = sameChords(out[i-1], out[i])
	}
	return out
}

// sameChords 报告两个小节逐拍的和弦是否相同（拍数也相同）。
func sameChords(a, b Measure) bool {
	if len(a.Beats) != len(b.Beats) {
		return false
	}
	for i := range a.Beats {
		if a.Beats[i].Chord.Label != b.Beats[i].Chord.Label {
			return false
		}
	}
	return true
}

// Events 是小节内按和弦切分的事件：同一和弦连续的拍合并为一个，Beats 为占的拍数。
type Event struct {
	Chord      Segment
	Start, End float64
	FirstBeat  int // 小节内的拍号，从 0 开始
	Beats      int
}

// Events 返回小节内的和弦事件（无和弦的拍也是一个事件，Chord.Label 为空）。
func (m Measure) Events() []Event {
	var out []Event
	for i, b := range m.Beats {
		if n := len(out); n > 0 && out[n-1].Chord.Label == b.Chord.Label && !b.Starts {
			out[n-1].End, out[n-1].Beats = b.End, out[n-1].Beats+1
			continue
		}
		out = append(out, Event{Chord: b.Chord, Start: b.Time, End: b.End, FirstBeat: i, Beats: 1})
	}
	return out
}

// 和弦记号的写法。
const (
	SymbolJazz     = "jazz"     // CΔ7、C-7、C°
	SymbolStandard = "standard" // Cmaj7、Cm7、Cdim
	SymbolRoman    = "roman"    // 相对调号的级数：I、vi7、IVΔ7
)

var jazzSuffix = map[string]string{"maj": "", "min": "-", "7": "7", "maj7": "Δ7", "min7": "-7", "dim": "°", "sus4": "sus4",
	"aug": "+", "6": "6", "min6": "-6", "minmaj7": "-Δ7", "dim7": "°7", "hdim7": "ø7", "sus2": "sus2"}
var standardSuffix = map[string]string{"maj": "", "min": "m", "7": "7", "maj7": "maj7", "min7": "m7", "dim": "dim", "sus4": "sus4",
	"aug": "aug", "6": "6", "min6": "m6", "minmaj7": "m(maj7)", "dim7": "dim7", "hdim7": "m7♭5", "sus2": "sus2"}

// romanSuffix 是级数写法的后缀：小三度家族的和弦用小写级数表示，后缀不再重复「m」。
var romanSuffix = map[string]string{"maj": "", "min": "", "7": "7", "maj7": "Δ7", "min7": "7", "dim": "°", "sus4": "sus4",
	"aug": "+", "6": "6", "min6": "6", "minmaj7": "Δ7", "dim7": "°7", "hdim7": "ø7", "sus2": "sus2"}

// minorFamily 是三音为小三度的性质，级数写成小写。
var minorFamily = map[string]bool{"min": true, "min7": true, "dim": true, "min6": true, "minmaj7": true, "dim7": true, "hdim7": true}

// romanDegree 是相对主音的半音数对应的级数（大调音阶为基准，变化音加升降号）。
var romanDegree = []string{"I", "♭II", "II", "♭III", "III", "IV", "♯IV", "V", "♭VI", "VI", "♭VII", "VII"}

// Symbol 返回和弦记号。style 为 Symbol* 之一；级数写法需要调号主音 tonic（音级）。转位和弦在后面加「/低音」。
func Symbol(c Segment, style string, tonic int) string {
	if c.Root == nil || c.Label == "" {
		return c.Label
	}
	root := rootName(c.Label)
	var s string
	switch style {
	case SymbolStandard:
		s = root + standardSuffix[c.Quality]
	case SymbolRoman:
		deg := romanDegree[((*c.Root-tonic)%12+12)%12]
		if minorFamily[c.Quality] {
			deg = strings.ToLower(deg)
		}
		s = deg + romanSuffix[c.Quality]
	default:
		s = root + jazzSuffix[c.Quality]
	}
	if bass := bassName(c.Label); c.Bass != nil && bass != "" {
		s += "/" + rootName(bass)
	}
	return s
}

// bassName 返回转位和弦名「/」之后的低音名（如 C/E 的 E）；不是转位时返回空串。
func bassName(label string) string {
	if i := strings.LastIndexByte(label, '/'); i >= 0 {
		return label[i+1:]
	}
	return ""
}

// rootName 取和弦名开头的根音（字母加可选的 # / b），并把 # / b 换成 ♯ / ♭。
func rootName(label string) string {
	if label == "" {
		return ""
	}
	n := 1
	if len(label) > 1 && (label[1] == '#' || label[1] == 'b') {
		n = 2
	}
	return strings.NewReplacer("#", "♯", "b", "♭").Replace(label[:n])
}

// ---------- 五线谱 ----------

// 和弦性质中各音相对根音的音级（三度叠置的字母距离：三音 2、五音 4、七音 6、挂四 3），与 qualityIntervals 一一对应。
var qualityDegrees = map[string][]int{
	"maj": {0, 2, 4}, "min": {0, 2, 4}, "7": {0, 2, 4, 6}, "maj7": {0, 2, 4, 6},
	"min7": {0, 2, 4, 6}, "dim": {0, 2, 4}, "sus4": {0, 3, 4},
	"aug": {0, 2, 4}, "6": {0, 2, 4, 5}, "min6": {0, 2, 4, 5}, "minmaj7": {0, 2, 4, 6},
	"dim7": {0, 2, 4, 6}, "hdim7": {0, 2, 4, 6}, "sus2": {0, 1, 4},
}

// 字母 C D E F G A B 的自然音级。
var letterPC = []int{0, 2, 4, 5, 7, 9, 11}

// Note 是五线谱上的一个音。
type Note struct {
	MIDI int
	// Step 是自 C0 起的自然音级序号（字母 + 7×八度），决定在谱表上的上下位置。
	Step int
	// Accidental 是相对该字母自然音的变化：-2 重降 … 2 重升。
	Accidental int
}

// Voicing 是一个和弦在五线谱与合成伴奏中的音：低音谱表的根音与高音谱表的密集排列。
type Voicing struct {
	Bass  Note
	Upper []Note
}

// 音域：低音放在 E2–D♯3，高音声部的最低音在 F3–E4 之间（大谱表中部，少加线）。
const (
	bassLow  = 40 // E2
	upperLow = 53 // F3
)

// rootLetter 返回和弦名根音的字母序号（C=0 … B=6）。
func rootLetter(label string) int {
	if label == "" {
		return 0
	}
	return strings.IndexByte("CDEFGAB", label[0])
}

// spell 按根音字母与音级距离拼写一个音（例如 Eb 大三和弦的三音写作 G 而不是 F##）。
func spell(midi, letter int) Note {
	letter = ((letter % 7) + 7) % 7
	pc := ((midi % 12) + 12) % 12
	acc := pc - letterPC[letter]
	if acc > 6 {
		acc -= 12
	}
	if acc < -6 {
		acc += 12
	}
	natural := midi - acc
	return Note{MIDI: midi, Step: letter + 7*(natural/12-1), Accidental: acc}
}

// ChordVoicing 返回和弦的谱面音高。prevUpper 是前一个和弦高音声部的 MIDI 音高（可为 nil）：
// 在各转位中选总移动最小的一个，使相邻和弦衔接平滑（声部进行）；没有前一个时选最低音最接近 upperLow + 6 的转位。
// 无和弦或未知性质时返回 false。
func ChordVoicing(c Segment, prevUpper []int) (Voicing, bool) {
	if c.Root == nil {
		return Voicing{}, false
	}
	ivs, degs := qualityIntervals[c.Quality], qualityDegrees[c.Quality]
	if ivs == nil {
		return Voicing{}, false
	}
	letter := rootLetter(c.Label)
	root := *c.Root
	// 转位和弦的低音谱表写低音（按其音名拼写），否则写根音。
	bassPC, bassLetter := root, letter
	if b := bassName(c.Label); c.Bass != nil && b != "" {
		bassPC, bassLetter = *c.Bass, rootLetter(b)
	}
	bass := bassLow + ((bassPC-bassLow)%12+12)%12
	v := Voicing{Bass: spell(bass, bassLetter)}

	best, bestCost := []int(nil), 1<<30
	for inv := range ivs {
		// 第 inv 转位：从第 inv 个和弦音起依次向上排列。
		first := upperLow + ((root+ivs[inv]-upperLow)%12+12)%12
		notes := []int{first}
		for k := 1; k < len(ivs); k++ {
			pc := root + ivs[(inv+k)%len(ivs)]
			n := notes[k-1] + ((pc-notes[k-1])%12+12)%12
			if n == notes[k-1] {
				n += 12
			}
			notes = append(notes, n)
		}
		cost := 0
		if len(prevUpper) > 0 {
			for i, n := range notes {
				d := n - prevUpper[min(i, len(prevUpper)-1)]
				cost += max(d, -d)
			}
		} else {
			d := notes[0] - (upperLow + 6)
			cost = max(d, -d)
		}
		if cost < bestCost {
			best, bestCost = notes, cost
		}
	}
	for _, n := range best {
		// 由音高反推它是第几个和弦音，以便按该和弦音的字母距离拼写。
		pc := ((n-root)%12 + 12) % 12
		deg := 0
		for k, iv := range ivs {
			if iv%12 == pc {
				deg = degs[k]
			}
		}
		v.Upper = append(v.Upper, spell(n, letter+deg))
	}
	return v, true
}

// MIDIs 返回高音声部的 MIDI 音高，用作下一个和弦的 prevUpper。
func (v Voicing) MIDIs() []int {
	out := make([]int, len(v.Upper))
	for i, n := range v.Upper {
		out[i] = n.MIDI
	}
	return out
}

// KeySignature 返回调号的升降号个数：正数为升号、负数为降号（小调按关系大调）。
func KeySignature(tonic int, mode string) int {
	major := tonic
	if mode == "minor" {
		major = (tonic + 3) % 12
	}
	// 五度圈位置：C=0、G=1、D=2 … F=-1、Bb=-2 …，取绝对值较小的一侧（F♯ / G♭ 取 6 个升号或降号，按 Flats 决定）。
	k := (major * 7) % 12
	if k > 6 {
		k -= 12
	}
	return k
}

// KeyAccidentals 返回调号对每个字母（C=0 … B=6）的变化：+1 升、-1 降、0 无。
func KeyAccidentals(sig int) [7]int {
	var out [7]int
	sharps := []int{3, 0, 4, 1, 5, 2, 6} // F C G D A E B
	flats := []int{6, 2, 5, 1, 4, 0, 3}  // B E A D G C F
	for i := 0; i < sig && i < 7; i++ {
		out[sharps[i]] = 1
	}
	for i := 0; i < -sig && i < 7; i++ {
		out[flats[i]] = -1
	}
	return out
}

// AccidentalSign 返回变化音记号（与调号相同时不需要记号，由调用方判断）。
func AccidentalSign(acc int) string {
	switch acc {
	case -2:
		return "𝄫"
	case -1:
		return "♭"
	case 1:
		return "♯"
	case 2:
		return "𝄪"
	}
	return "♮"
}

// DurationName 是拍数对应的音符（四四拍以四分音符为一拍）：用于测试与无障碍描述。
func DurationName(beats int) string {
	switch {
	case beats >= 4:
		return "全音符"
	case beats == 3:
		return "附点二分音符"
	case beats == 2:
		return "二分音符"
	}
	return "四分音符"
}

// String 返回音名（如 E♭4），用于提示与测试。
func (n Note) String() string {
	acc := map[int]string{-2: "𝄫", -1: "♭", 0: "", 1: "♯", 2: "𝄪"}[n.Accidental]
	return fmt.Sprintf("%c%s%d", "CDEFGAB"[((n.Step%7)+7)%7], acc, n.Step/7)
}
