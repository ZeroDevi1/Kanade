package score

import (
	"strings"
	"testing"
)

func TestMeasuresAndEvents(t *testing.T) {
	a := &Analysis{Duration: 6.5, Beats: []float64{0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5, 5, 5.5, 6}, Downbeats: []float64{0.5, 2.5, 4.5}}
	a.Chords = []Segment{{0.5, 1.5, "C", ip(0), "maj"}, {1.5, 2.5, "Am", ip(9), "min"}, {2.5, 3.5, "C", ip(0), "maj"},
		{3.5, 4.5, "Am", ip(9), "min"}, {4.5, 5.5, "C", ip(0), "maj"}, {5.5, 6.5, "Am", ip(9), "min"}}
	ms := a.Measures()
	if len(ms) != 3 {
		t.Fatalf("measures %+v", ms)
	}
	// 第一小节含弱起前的 0–0.5 秒（没有拍点，补一个起点拍）：无和弦、C、C、Am、Am。
	var labels []string
	for _, b := range ms[0].Beats {
		mark := ""
		if b.Starts {
			mark = "*"
		}
		labels = append(labels, b.Chord.Label+mark)
	}
	if strings.Join(labels, ",") != ",C*,C,Am*,Am" {
		t.Errorf("beats %v", labels)
	}
	// 第一小节含弱起（5 拍），与第二小节拍数不同，不算反复；第三小节与第二小节相同。
	if ms[1].Simile || !ms[2].Simile {
		t.Errorf("simile %v %v", ms[1].Simile, ms[2].Simile)
	}
	ev := ms[1].Events()
	if len(ev) != 2 || ev[0].Beats != 2 || ev[1].Chord.Label != "Am" || ev[1].FirstBeat != 2 {
		t.Errorf("events %+v", ev)
	}
}

func TestSymbols(t *testing.T) {
	c := Segment{Label: "Ebmaj7", Root: ip(3), Quality: "maj7"}
	cases := map[string]string{SymbolJazz: "E♭Δ7", SymbolStandard: "E♭maj7", SymbolRoman: "IVΔ7"}
	for style, want := range cases {
		if got := Symbol(c, style, 10); got != want { // 调号 B♭：E♭ 是 IV
			t.Errorf("%s: %q, want %q", style, got, want)
		}
	}
	if got := Symbol(Segment{Label: "Gm7", Root: ip(7), Quality: "min7"}, SymbolRoman, 10); got != "vi7" {
		t.Errorf("roman minor %q", got)
	}
}

func TestVoicingSpellingAndLeading(t *testing.T) {
	// E♭ 大三和弦：三音拼作 G（不是 F𝄪），五音 B♭。
	v, ok := ChordVoicing(Segment{Label: "Eb", Root: ip(3), Quality: "maj"}, nil)
	if !ok || v.Bass.String() != "E♭3" {
		t.Fatalf("bass %v %v", v.Bass, ok)
	}
	var names []string
	for _, n := range v.Upper {
		names = append(names, strings.TrimRight(n.String(), "0123456789"))
	}
	if strings.Join(names, " ") != "B♭ E♭ G" && strings.Join(names, " ") != "E♭ G B♭" && strings.Join(names, " ") != "G B♭ E♭" {
		t.Errorf("upper %v", names)
	}
	// 声部进行：C → F 应保持共同音 C，总移动最小（C-E-G → C-F-A）。
	c, _ := ChordVoicing(Segment{Label: "C", Root: ip(0), Quality: "maj"}, []int{60, 64, 67})
	f, _ := ChordVoicing(Segment{Label: "F", Root: ip(5), Quality: "maj"}, c.MIDIs())
	if got := f.MIDIs(); got[0] != 60 || got[1] != 65 || got[2] != 69 {
		t.Errorf("F voicing %v after %v", got, c.MIDIs())
	}
}

func TestKeySignature(t *testing.T) {
	if KeySignature(1, "major") != -5 || KeySignature(7, "major") != 1 || KeySignature(9, "minor") != 0 {
		t.Errorf("sig Db %d G %d Am %d", KeySignature(1, "major"), KeySignature(7, "major"), KeySignature(9, "minor"))
	}
	acc := KeyAccidentals(-2) // B♭ 大调：B、E 降
	if acc[6] != -1 || acc[2] != -1 || acc[5] != 0 {
		t.Errorf("acc %v", acc)
	}
}

func TestSynthChords(t *testing.T) {
	a := &Analysis{Duration: 2, Downbeats: []float64{0, 1}}
	a.Chords = []Segment{{0, 2, "C", ip(0), "maj"}}
	pcm := a.SynthChords(8000)
	if len(pcm) != 16000 {
		t.Fatalf("len %d", len(pcm))
	}
	// 有声、不削波，且在第二小节线（1 秒）处重新弹奏：重新起音后的能量高于起音前。
	energy := func(from, to int) (e float64) {
		for _, x := range pcm[from:to] {
			e += float64(x) * float64(x)
		}
		return e
	}
	peak := float32(0)
	for _, x := range pcm {
		peak = max(peak, x, -x)
	}
	if peak == 0 || peak > 0.91 || energy(8000, 8800) <= energy(7200, 8000) {
		t.Errorf("peak %v, energy before %v after %v", peak, energy(7200, 8000), energy(8000, 8800))
	}
}
