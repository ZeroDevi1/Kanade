package compose

import (
	"math"
	"strings"
	"testing"

	"kanade/internal/score"
	"kanade/internal/score/hum"
)

// melodyHum 由「音名:拍数」序列构造 C 大调 4/4、120 BPM 的乐句（音名为 C4 起的唱名字母，- 表示延续前一音）。
func melodyHum(t *testing.T, spec string, bars int) hum.Hum {
	t.Helper()
	pcs := map[byte]int{'C': 60, 'D': 62, 'E': 64, 'F': 65, 'G': 67, 'A': 69, 'B': 71}
	s := hum.HumSettings{BPM: 120, BeatsPerBar: 4, Bars: bars}
	beat := s.Beat()
	var notes []score.MelodyNote
	at := 0.0
	for _, tok := range strings.Fields(spec) {
		name, dur, _ := strings.Cut(tok, ":")
		d := 1.0
		if dur != "" {
			var err error
			if d, err = parseFloat(dur); err != nil {
				t.Fatal(err)
			}
		}
		m := pcs[name[0]]
		if len(name) > 1 && name[1] == '\'' {
			m += 12
		}
		notes = append(notes, score.MelodyNote{Start: at * beat, End: (at + d) * beat, Pitch: float64(m)})
		at += d
	}
	return hum.Hum{Settings: s, Notes: notes, TonicPC: 0, Mode: "major", KeyClear: true}
}

func parseFloat(s string) (float64, error) {
	var v float64
	var frac, div float64 = 0, 1
	seenDot := false
	for _, r := range s {
		switch {
		case r == '.':
			seenDot = true
		case r >= '0' && r <= '9' && !seenDot:
			v = v*10 + float64(r-'0')
		case r >= '0' && r <= '9':
			div *= 10
			frac += float64(r-'0') / div
		}
	}
	return v + frac, nil
}

// chordAt 返回乐谱时间 sec 处的和弦记号（标准写法）。
func chordAt(h Harmony, sec float64) string {
	for _, c := range h.Chords {
		if sec >= c.Start-1e-9 && sec < c.End-1e-9 {
			return c.Label
		}
	}
	return ""
}

// 《欢乐颂》前 8 小节（流行风格）：各小节首拍的和弦落在允许集合里，第 4 小节半终止、第 8 小节终止在 C。
func TestHarmonizeOdeToJoy(t *testing.T) {
	h := melodyHum(t, "E E F G G F E D C C D E E:1.5 D:0.5 D:2 E E F G G F E D C C D E D:1.5 C:0.5 C:2", 8)
	res := Harmonize(h, StylePop, 3)
	if len(res) == 0 {
		t.Fatal("no harmony")
	}
	best := res[0]
	allowed := []([]string){
		{"C"}, {"G", "G7", "C"}, {"C", "F", "Am"}, {"G", "G7", "Dm", "Dm7", "Gsus4", "C"},
		{"C"}, {"G", "G7", "Em", "C"}, {"C", "F", "Am"}, {"G", "G7", "C", "Gsus4"},
	}
	for bar, ok := range allowed {
		got := chordAt(best, float64(bar*4)*h.Settings.Beat())
		if !contains(ok, got) {
			t.Errorf("bar %d downbeat %q, allowed %v; chords %v", bar+1, got, ok, labels(best))
		}
	}
	// 第 4 小节第 3 拍（长音 D）为半终止的属和弦。
	if got := chordAt(best, 14*h.Settings.Beat()); !contains([]string{"G", "G7", "Gsus4"}, got) {
		t.Errorf("half cadence %q; chords %v", got, labels(best))
	}
	if last := best.Chords[len(best.Chords)-1]; last.Label != "C" {
		t.Errorf("final chord %q, chords %v", last.Label, labels(best))
	}
	if best.Melody < 80 {
		t.Errorf("melody fit %.0f", best.Melody)
	}
	for _, r := range res[1:] {
		if r.Total < res[0].Total-harmAltDrop {
			t.Errorf("alternative too far behind: %.0f vs %.0f", r.Total, res[0].Total)
		}
	}
}

// 《小星星》（4 小节，C 大调）：结尾为 G → C 或 G7 → C。
func TestHarmonizeTwinkle(t *testing.T) {
	h := melodyHum(t, "C C G G A A G:2 F F E E D D C:2", 4)
	for _, style := range []int{StylePop, StyleJazz, StyleReharm} {
		res := Harmonize(h, style, 1)
		cs := res[0].Chords
		last, prev := cs[len(cs)-1], cs[len(cs)-2]
		if *last.Root != 0 || (*prev.Root != 7 && *prev.Root != 1 && *prev.Root != 5) {
			t.Errorf("style %d cadence %v", style, labels(res[0]))
		}
	}
}

// 属性：强拍上的长旋律音不是避免音或其他音；副属等和弦都解决；调式风格没有 V → I；结果可复现、移调不变；
// 备选之间差异足够大；3/4 每小节至多一个和弦。
func TestHarmonizeProperties(t *testing.T) {
	melodies := []struct {
		spec  string
		bars  int
		meter int
	}{
		{"E E F G G F E D C C D E E:1.5 D:0.5 D:2", 4, 4},
		{"C:2 E G A:2 G:2 F:2 E D C:4", 4, 4},
		{"G:3 E:2 F:1 G:3 C:3 D:2 E:1 F:3", 4, 3},
		{"A:2 C' B A G:2 E:2 F:2 A:2 G:4", 4, 4},
	}
	for _, m := range melodies {
		base := melodyHum(t, m.spec, m.bars)
		base.Settings.BeatsPerBar = m.meter
		for style := range len(HarmonyStyles) {
			res := Harmonize(base, style, 3)
			if len(res) == 0 {
				t.Fatalf("%q style %d: no result", m.spec, style)
			}
			again := Harmonize(base, style, 3)
			if labels(again[0]) != labels(res[0]) {
				t.Errorf("not deterministic: %v vs %v", labels(res[0]), labels(again[0]))
			}
			for _, a := range res {
				checkHarmony(t, base, a, style, m.meter)
			}
			// 移调：整体升 3 个半音，根音全部 +3，性质不变。
			up := base
			up.TonicPC = 3
			up.Notes = append([]score.MelodyNote(nil), base.Notes...)
			for i := range up.Notes {
				up.Notes[i].Pitch += 3
			}
			moved := Harmonize(up, style, 3)
			if len(moved[0].Chords) != len(res[0].Chords) {
				t.Errorf("transpose changed length: %v vs %v", labels(res[0]), labels(moved[0]))
				continue
			}
			for i, c := range res[0].Chords {
				d := moved[0].Chords[i]
				if (*c.Root+3)%12 != *d.Root || c.Quality != d.Quality {
					t.Errorf("transpose: %v vs %v", labels(res[0]), labels(moved[0]))
					break
				}
			}
		}
	}
}

func checkHarmony(t *testing.T, h hum.Hum, r Harmony, style, meter int) {
	t.Helper()
	beat := h.Settings.Beat()
	if len(r.Reasons) != len(r.Chords) || r.Total < 0 || r.Total > 100 {
		t.Errorf("result %+v", r)
	}
	perBar := map[int]int{}
	for i, c := range r.Chords {
		if _, ok := score.QualityIntervals[c.Quality]; !ok {
			t.Errorf("quality %q", c.Quality)
		}
		perBar[int(math.Round(c.Start/beat))/meter]++
		if i > 0 && style == StyleModal {
			p := r.Chords[i-1]
			if (*p.Root-h.TonicPC+12)%12 == 7 && (p.Quality == "maj" || p.Quality == "7") && *c.Root == h.TonicPC {
				t.Errorf("modal V → I in %v", labels(r))
			}
		}
	}
	if meter == 3 {
		for bar, n := range perBar {
			if n > 1 {
				t.Errorf("3/4 bar %d has %d chords: %v", bar, n, labels(r))
			}
		}
	}
	// 强拍上时值 ≥ 1 拍的旋律音在所选和弦上不是避免音或其他音。
	for _, n := range h.Notes {
		sb := n.Start / beat
		if math.Mod(sb, float64(meter)) > 1e-6 || (n.End-n.Start)/beat < 1-1e-9 {
			continue
		}
		for _, c := range r.Chords {
			if n.Start >= c.Start-1e-9 && n.Start < c.End-1e-9 {
				iv := ((int(n.Pitch)-*c.Root)%12 + 12) % 12
				if toneCost[c.Quality][toneColumn(style)][iv] >= 2 {
					t.Errorf("style %d: downbeat %v on %s (%s)", style, n.Pitch, c.Label, labels(r))
				}
			}
		}
	}
}

func labels(r Harmony) string {
	var out []string
	for _, c := range r.Chords {
		out = append(out, c.Label)
	}
	return strings.Join(out, " ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// 锁定和弦：被锁定的时段只用指定的和弦（不在候选表里的也可以），其余部分重新求解；理由标出已锁定。
func TestHarmonizeLocks(t *testing.T) {
	h := melodyHum(t, "E E F G G F E D C C D E E:1.5 D:0.5 D:2", 4)
	bar := float64(h.Settings.BeatsPerBar) * h.Settings.Beat()
	for _, l := range []ChordLock{{Start: bar, End: 2 * bar, Root: 9, Quality: "min7"}, {Start: bar, End: 1.5 * bar, Root: 6, Quality: "7"}} {
		for style := range HarmonyStyles {
			res := HarmonizeWith(h, style, 3, HarmonyOptions{Locks: []ChordLock{l}})
			if len(res) == 0 {
				t.Fatalf("style %d lock %+v: no result", style, l)
			}
			for _, r := range res {
				for i, c := range r.Chords {
					inLock := c.Start < l.End-1e-9 && c.End > l.Start+1e-9
					if inLock && (*c.Root != l.Root || c.Quality != l.Quality) {
						t.Errorf("style %d: %s inside lock %+v (%s)", style, c.Label, l, labels(r))
					}
					if inLock && !strings.HasSuffix(r.Reasons[i], "（已锁定）") {
						t.Errorf("reason %q", r.Reasons[i])
					}
				}
			}
		}
	}
}
