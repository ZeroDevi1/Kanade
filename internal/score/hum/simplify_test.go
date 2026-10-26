package hum

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"kanade/internal/score"
)

// sn 是测试用的音：音高、起止格（十六分音符，4/4、120 BPM 时 0.125 秒一格）、是否连音。
type sn struct {
	p, s, e int
	slur    bool
}

var simpSet = HumSettings{BPM: 120, BeatsPerBar: 4, Bars: 2}

func simpHum(set HumSettings, notes ...sn) Hum {
	g := set.Grid()
	h := Hum{Settings: set, Mode: "major"}
	for _, n := range notes {
		h.Notes = append(h.Notes, score.MelodyNote{Start: float64(n.s) * g, End: float64(n.e) * g, Pitch: float64(n.p), Slur: n.slur})
	}
	return h
}

func simpCells(set HumSettings, notes []score.MelodyNote) []sn {
	g := set.Grid()
	var out []sn
	for _, n := range notes {
		out = append(out, sn{int(n.Pitch), int(math.Round(n.Start / g)), int(math.Round(n.End / g)), n.Slur})
	}
	return out
}

// Fable 5.1 建议的用例。
func TestSimplifyMelody(t *testing.T) {
	cases := []struct {
		name  string
		level int
		in    []sn
		want  []sn
	}{
		{"滑入", 1, []sn{{65, 0, 1, false}, {67, 1, 7, true}}, []sn{{67, 0, 7, false}}},
		{"原样不动", 0, []sn{{65, 0, 1, false}, {67, 1, 7, true}}, []sn{{65, 0, 1, false}, {67, 1, 7, true}}},
		{"滑出", 1, []sn{{67, 0, 6, false}, {65, 6, 7, true}}, []sn{{67, 0, 7, false}}},
		{"差三个半音的上滑字尾不是滑出", 1, []sn{{64, 0, 6, false}, {67, 6, 8, true}}, []sn{{64, 0, 6, false}, {67, 6, 8, true}}},
		{"一格长的字尾掉音并入", 1, []sn{{67, 0, 6, false}, {61, 6, 7, true}}, []sn{{67, 0, 7, false}}},
		{"碎片不并入更短的掉音", 2, []sn{{66, 0, 3, false}, {68, 3, 4, true}, {66, 4, 5, true}, {60, 5, 6, true}, {61, 6, 8, false}},
			[]sn{{66, 0, 6, false}, {61, 6, 8, false}}},
		{"抖动取时长众数", 1, []sn{{67, 0, 1, false}, {68, 1, 2, true}, {67, 2, 3, true}, {68, 3, 4, true}, {67, 4, 6, true}},
			[]sn{{67, 0, 6, false}}},
		{"邻音", 1, []sn{{67, 0, 3, false}, {66, 3, 4, true}, {67, 4, 8, true}}, []sn{{67, 0, 8, false}}},
		{"两格的邻音适中保留", 1, []sn{{67, 0, 3, false}, {66, 3, 5, true}, {67, 5, 8, true}},
			[]sn{{67, 0, 3, false}, {66, 3, 5, true}, {67, 5, 8, true}}},
		{"两格的邻音精简合并", 2, []sn{{67, 0, 3, false}, {66, 3, 5, true}, {67, 5, 8, true}}, []sn{{67, 0, 8, false}}},
		{"弱位经过音并入", 1, []sn{{64, 0, 2, false}, {65, 2, 3, true}, {67, 3, 8, true}}, []sn{{64, 0, 2, false}, {67, 2, 8, true}}},
		{"拍首的经过音保留", 1, []sn{{64, 0, 4, false}, {65, 4, 5, true}, {67, 5, 10, true}},
			[]sn{{64, 0, 4, false}, {65, 4, 5, true}, {67, 5, 10, true}}},
		{"碎片并入前字", 1, []sn{{67, 0, 4, false}, {67, 4, 5, false}, {69, 5, 12, false}}, []sn{{67, 0, 5, false}, {69, 5, 12, false}}},
		{"拍上的碎片让后字提前", 1, []sn{{60, 0, 2, false}, {67, 4, 5, false}, {67, 5, 8, false}}, []sn{{60, 0, 4, false}, {67, 4, 8, false}}},
		{"短空隙补成连奏", 1, []sn{{60, 0, 2, false}, {62, 4, 6, false}, {64, 12, 14, false}},
			[]sn{{60, 0, 4, false}, {62, 4, 6, false}, {64, 12, 14, false}}},
		{"级进转音精简保留", 2, []sn{{64, 0, 2, false}, {65, 2, 4, true}, {67, 4, 8, true}},
			[]sn{{64, 0, 2, false}, {65, 2, 4, true}, {67, 4, 8, true}}},
		{"跳进转音精简为一音（平局取拍首）", 2, []sn{{67, 0, 4, false}, {64, 4, 8, true}}, []sn{{67, 0, 8, false}}},
		{"精简：先滑入再判定", 2, []sn{{64, 0, 1, false}, {65, 1, 2, true}, {67, 2, 8, true}}, []sn{{67, 0, 8, false}}},
		{"精简：十六分位的字首移到更强的拍位", 2, []sn{{60, 0, 2, false}, {64, 3, 8, false}}, []sn{{60, 0, 4, false}, {64, 4, 8, false}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := simpCells(simpSet, SimplifyMelody(simpHum(simpSet, c.in...), c.level))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// 复合拍：6/8 下整字一拍（6 格）的级进转音在精简级别保留；拍首（第 6 格）的经过音在适中级别保留。
func TestSimplifyCompound(t *testing.T) {
	set := HumSettings{BPM: 60, BeatsPerBar: 2, Bars: 2, Compound: true}
	in := simpHum(set, sn{64, 0, 2, false}, sn{65, 2, 4, true}, sn{67, 4, 6, true})
	if got := simpCells(set, SimplifyMelody(in, 2)); len(got) != 3 {
		t.Errorf("6/8 stepwise melisma: %v", got)
	}
	in = simpHum(set, sn{64, 0, 6, false}, sn{65, 6, 7, true}, sn{67, 7, 12, true})
	if got := simpCells(set, SimplifyMelody(in, 1)); len(got) != 3 {
		t.Errorf("6/8 passing tone on the beat: %v", got)
	}
}

// 真实哼唱（用户的录音整理结果，3/4、135 BPM、7 小节，起止以拍计）：精简后音数、字数逐级减少；转音减少。
func TestSimplifyRealHum(t *testing.T) {
	type rn struct {
		s, e float64
		p    int
		slur bool
	}
	raw := []rn{{2, 2.5, 48, false}, {3, 3.25, 51, false}, {3.25, 3.5, 48, true}, {4, 4.5, 49, false}, {5, 5.25, 54, false},
		{5.25, 5.5, 56, true}, {5.5, 5.75, 54, true}, {5.75, 6, 48, true}, {6, 6.5, 49, false}, {7.5, 7.75, 47, false},
		{7.75, 8.25, 48, true}, {9, 9.25, 51, false}, {9.25, 9.75, 49, true}, {9.75, 10, 48, false}, {11.25, 11.5, 48, false},
		{12, 12.25, 51, false}, {12.25, 12.5, 49, true}, {13, 13 + 1.0/3, 49, false}, {14, 14 + 1.0/3, 54, false},
		{14 + 1.0/3, 14 + 2.0/3, 56, true}, {15, 15 + 1.0/3, 49, false}, {16 + 2.0/3, 17, 49, false}, {17, 17.25, 48, false},
		{18, 18.25, 52, false}, {18.25, 18.75, 49, true}, {19, 19 + 1.0/3, 49, false}, {19 + 1.0/3, 19 + 2.0/3, 49, false}}
	set := HumSettings{BPM: 135, BeatsPerBar: 3, Bars: 7}
	h := Hum{Settings: set, Mode: "major", TonicPC: 1}
	for _, n := range raw {
		h.Notes = append(h.Notes, score.MelodyNote{Start: n.s * set.Beat(), End: n.e * set.Beat(), Pitch: float64(n.p), Slur: n.slur})
	}
	count := func(ns []score.MelodyNote) (notes, syl, slurs int) {
		for _, n := range ns {
			if n.Slur {
				slurs++
			} else {
				syl++
			}
		}
		return len(ns), syl, slurs
	}
	n0, s0, m0 := count(h.Notes)
	n1, s1, m1 := count(SimplifyMelody(h, 1))
	n2, s2, m2 := count(SimplifyMelody(h, 2))
	t.Logf("原样 %d 音 %d 字 %d 转音；适中 %d 音 %d 字 %d 转音；精简 %d 音 %d 字 %d 转音", n0, s0, m0, n1, s1, m1, n2, s2, m2)
	if !(n1 < n0 && n2 <= n1 && m1 < m0 && m2 <= m1 && s2 <= s1 && s1 <= s0) {
		t.Errorf("not simplified enough")
	}
	checkSimplified(t, set, SimplifyMelody(h, 1), false)
	checkSimplified(t, set, SimplifyMelody(h, 2), true)
}

// checkSimplified 检查性质：有序、不重叠、长度为正、字首不是连音；精简级别下起止都在八分音符网格上。
func checkSimplified(t *testing.T, set HumSettings, ns []score.MelodyNote, eighths bool) {
	t.Helper()
	e := 2 * set.Grid()
	for i, n := range ns {
		if n.End <= n.Start || (i > 0 && n.Start < ns[i-1].End-1e-9) || (i == 0 && n.Slur) {
			t.Fatalf("note %d %+v in %+v", i, n, ns)
		}
		if i > 0 && n.Slur && ns[i-1].End < n.Start-1e-9 {
			t.Fatalf("slur after a rest at %d", i)
		}
		if eighths && (math.Abs(n.Start/e-math.Round(n.Start/e)) > 1e-6 || math.Abs(n.End/e-math.Round(n.End/e)) > 1e-6) {
			t.Fatalf("level 2 note off the eighth grid: %+v", n)
		}
	}
}

// 随机乐句：性质成立，且幂等（精简两次与一次相同）。
func TestSimplifyProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for trial := range 300 {
		var in []sn
		for c := 0; c < 32; {
			d := 1 + rng.IntN(4)
			if c+d > 32 {
				break
			}
			in = append(in, sn{60 + rng.IntN(5), c, c + d, len(in) > 0 && in[len(in)-1].e == c && rng.IntN(2) == 0})
			c += d
			if rng.IntN(4) == 0 {
				c += 1 + rng.IntN(3)
			}
		}
		h := simpHum(simpSet, in...)
		for level := 1; level <= 2; level++ {
			once := SimplifyMelody(h, level)
			checkSimplified(t, simpSet, once, level == 2)
			again := SimplifyMelody(Hum{Settings: simpSet, Notes: once}, level)
			if !reflect.DeepEqual(simpCells(simpSet, once), simpCells(simpSet, again)) {
				t.Fatalf("trial %d level %d not idempotent:\n%v\n%v\n%v", trial, level, in, simpCells(simpSet, once), simpCells(simpSet, again))
			}
		}
	}
}
