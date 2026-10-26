package compose

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"kanade/internal/score"
	"kanade/internal/score/hum"
)

// motifHum 由「音名:拍数」序列构造 tonic / mode 下的动机（melodyHum 的通用版本，bars 小节，4/4、100 BPM）。
func motifHum(t *testing.T, spec string, bars int) hum.Hum {
	t.Helper()
	h := melodyHum(t, spec, bars)
	h.Settings.BPM = 100
	beat := h.Settings.Beat()
	ratio := 120.0 / 100
	for i := range h.Notes {
		h.Notes[i].Start *= ratio
		h.Notes[i].End *= ratio
	}
	_ = beat
	return h
}

func degs(u []degNote) []int {
	var out []int
	for _, n := range u {
		out = append(out, n.deg)
	}
	return out
}

// 变换的具体例子（C 大调，动机 C D E G | E D C(2 拍)）。
func TestDevelopTransforms(t *testing.T) {
	h := motifHum(t, "C D E G E D C:2", 2)
	k := newKeyModel(h)
	u0, u1 := devUnits(h, k)
	if u1 != nil || !reflect.DeepEqual(degs(u0), []int{0, 1, 2, 4, 2, 1, 0}) {
		t.Fatalf("units %v %v", degs(u0), u1)
	}
	midi := func(u []degNote) []int {
		var out []int
		for _, n := range u {
			out = append(out, k.toMIDI(n.deg, n.alt))
		}
		return out
	}
	if got := midi(seq(u0, 1)); !reflect.DeepEqual(got, []int{62, 64, 65, 69, 65, 64, 62}) {
		t.Errorf("seq +1 → %v", got)
	}
	if got := midi(inv(u0, 0)); !reflect.DeepEqual(got, []int{60, 59, 57, 53, 57, 59, 60}) {
		t.Errorf("inv → %v", got)
	}
	if got := midi(frag(u0, false, 1, 3, 8, -1)); !reflect.DeepEqual(got, []int{60, 59, 57}) {
		t.Errorf("frag → %v", got)
	}
	half := cadence(u0, DevCadHalf, 8, true, 0.5)
	if last := half[len(half)-1]; k.toMIDI(last.deg, last.alt) != 62 || last.onset+last.dur > 7+1e-9 {
		t.Errorf("half cadence %+v", half)
	}
	full := cadence(u0, DevCadFull, 8, true, 0.5)
	if last := full[len(full)-1]; k.toMIDI(last.deg, last.alt) != 60 || last.dur < 1 {
		t.Errorf("full cadence %+v", full)
	}
	if got := vary(u0, -1, 0.5); len(got) != len(u0) {
		t.Errorf("vary -1 changed a motif without repeated notes: %v", degs(got))
	}
	// 小调导音保留为变化音；调外音吸附到音阶。
	km := keyModel{base: 57, scale: devScale("minor")} // A 小调
	if d, a := km.toDeg(68); d != 6 || a != 1 || km.toMIDI(d, a) != 68 {
		t.Errorf("leading tone %d %d", d, a)
	}
}

// 属性：小节数与格数、终止音、调内音、不重叠、确定性、移调不变、方案间的差异。
func TestDevelopProperties(t *testing.T) {
	motifs := []struct {
		spec string
		bars int
	}{
		{"C D E G E D C:2", 2},
		{"E:2 G A:2 G E:3", 2},
		{"G E C:2", 1},
		{"C:2 E G A:2 G:2 F:2 E D C:4", 4},
	}
	for _, m := range motifs {
		h := motifHum(t, m.spec, m.bars)
		for _, bars := range []int{8, 16} {
			ps := Develop(h, DevelopOptions{Bars: bars}, 3)
			if len(ps) == 0 {
				t.Fatalf("%q %d bars: no period", m.spec, bars)
			}
			again := Develop(h, DevelopOptions{Bars: bars}, 3)
			if !reflect.DeepEqual(ps, again) {
				t.Errorf("%q: not deterministic", m.spec)
			}
			for i, p := range ps {
				checkPeriod(t, h, p, bars)
				for _, q := range ps[:i] {
					if d := periodDist(p, q, 8); d < devMinDist*0.7-1e-9 {
						t.Errorf("%q: periods too similar (%.2f)", m.spec, d)
					}
				}
			}
			// 移调：动机升 3 个半音（主音随之），乐段整体升 3 个半音、评分相同。
			up := h
			up.TonicPC = 3
			up.Notes = append([]score.MelodyNote(nil), h.Notes...)
			for i := range up.Notes {
				up.Notes[i].Pitch += 3
			}
			moved := Develop(up, DevelopOptions{Bars: bars}, 3)
			if len(moved) != len(ps) || moved[0].Score != ps[0].Score {
				t.Fatalf("transpose changed the result")
			}
			for i, n := range moved[0].Hum.Notes {
				if n.Pitch != ps[0].Hum.Notes[i].Pitch+3 {
					t.Fatalf("transpose: note %d %v vs %v", i, n.Pitch, ps[0].Hum.Notes[i].Pitch)
				}
			}
		}
	}
}

func checkPeriod(t *testing.T, h hum.Hum, p Period, bars int) {
	t.Helper()
	ph := p.Hum
	beat := ph.Settings.Beat()
	if p.Bars != bars || ph.Settings.Bars != bars || len(p.Slots) != bars/devSlotBars {
		t.Fatalf("bars %d slots %d", p.Bars, len(p.Slots))
	}
	if p.Score < 0 || p.Score > 100 {
		t.Errorf("score %v", p.Score)
	}
	scale := devScale(h.Mode)
	for i, n := range ph.Notes {
		if n.Start < -1e-9 || n.End > ph.Settings.Length()+1e-9 || n.End-n.Start < ph.Settings.Grid()-1e-9 {
			t.Fatalf("note %d out of range %+v", i, n)
		}
		if i > 0 && n.Start < ph.Notes[i-1].End-1e-9 {
			t.Fatalf("overlap at %d", i)
		}
		pc := ((int(n.Pitch)-h.TonicPC)%12 + 12) % 12
		inScale := false
		for _, s := range scale {
			inScale = inScale || s == pc
		}
		if !inScale {
			t.Errorf("%s: off-key note %v", p.Form, n.Pitch)
		}
		if !n.Slur && n.Lyric == "" {
			t.Errorf("syllable without lyric at %d", i)
		}
	}
	last := ph.Notes[len(ph.Notes)-1]
	if (int(last.Pitch)-h.TonicPC)%12 != 0 || (last.End-last.Start)/beat < 1-1e-9 {
		t.Errorf("%s: final note %+v", p.Form, last)
	}
	// 和声：按句式的终止点配，最后一个和弦是主和弦，半终止格末的和弦不是主和弦。
	hs := p.Harmonize(StylePop, 1, nil)
	if len(hs) == 0 {
		t.Fatalf("%s: no harmony", p.Form)
	}
	chords := hs[0].Chords
	if *chords[len(chords)-1].Root != h.TonicPC {
		t.Errorf("%s: final chord %v", p.Form, labels(hs[0]))
	}
	for _, cp := range p.Cadences() {
		if cp.Kind != CadHalf {
			continue
		}
		for _, c := range chords {
			if cp.At >= c.Start-1e-9 && cp.At < c.End-1e-9 && *c.Root == h.TonicPC {
				t.Errorf("%s: half cadence on tonic at %.2f s: %v", p.Form, cp.At, labels(hs[0]))
			}
		}
	}
	_ = math.Abs
}

// 锁定：被锁定的格保持原样，其余格重新生成。
func TestDevelopLocked(t *testing.T) {
	h := motifHum(t, "C D E G E D C:2", 2)
	ps := Develop(h, DevelopOptions{Bars: 16, Form: "aaba"}, 3)
	if len(ps) == 0 {
		t.Fatal("no period")
	}
	locked := map[int]DevSlot{4: ps[len(ps)-1].Slots[4], 5: ps[len(ps)-1].Slots[5]}
	re := Develop(h, DevelopOptions{Bars: 16, Form: "aaba", Locked: locked}, 3)
	if len(re) == 0 {
		t.Fatal("no period after locking")
	}
	for _, p := range re {
		for i, l := range locked {
			if !reflect.DeepEqual(p.Slots[i].notes, l.notes) || p.Slots[i].Desc != l.Desc+"（已锁定）" {
				t.Fatalf("locked slot %d changed: %q", i, p.Slots[i].Desc)
			}
		}
	}
	// 再次锁定同一格不会把「（已锁定）」叠加。
	again := Develop(h, DevelopOptions{Bars: 16, Form: "aaba", Locked: map[int]DevSlot{4: re[0].Slots[4]}}, 1)
	if again[0].Slots[4].Desc != re[0].Slots[4].Desc {
		t.Errorf("desc %q", again[0].Slots[4].Desc)
	}
}

// 逐格修改：改第 2 格的模进与倒影，其余格不变；说明随之更新；把末格的全终止去掉时评分为 0 并说明原因。
func TestEditSlot(t *testing.T) {
	h := motifHum(t, "C D E G E D C:2", 2)
	p := Develop(h, DevelopOptions{Bars: 8, Form: "parallel"}, 1)[0]
	sp := p.Slots[1].Spec
	sp.Seq, sp.Inv = 2, 2
	q := EditSlot(h, p, 1, sp, 0)
	if q.Slots[1].Spec != sp || !strings.Contains(q.Slots[1].Desc, "以主音为轴倒影") || !strings.Contains(q.Slots[1].Desc, "上行模进 2 级") {
		t.Fatalf("edited slot %+v", q.Slots[1])
	}
	for _, i := range []int{0, 2, 3} {
		if !reflect.DeepEqual(q.Slots[i].notes, p.Slots[i].notes) {
			t.Errorf("slot %d changed", i)
		}
	}
	if reflect.DeepEqual(q.Slots[1].notes, p.Slots[1].notes) || len(q.Hum.Notes) == 0 {
		t.Error("slot 1 not regenerated")
	}
	if !reflect.DeepEqual(p.Slots[1].Spec, Develop(h, DevelopOptions{Bars: 8, Form: "parallel"}, 1)[0].Slots[1].Spec) {
		t.Error("EditSlot modified its input")
	}
	last := q.Slots[3].Spec
	last.Cadence = DevCadNone
	bad := EditSlot(h, q, 3, last, 0)
	if bad.Score != 0 || !strings.Contains(bad.Notes, "不合规范") {
		t.Errorf("no final cadence: score %v %q", bad.Score, bad.Notes)
	}
}
