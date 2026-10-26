package score

import (
	"strings"
	"testing"
)

func TestMeasuresAndEvents(t *testing.T) {
	a := &Analysis{Duration: 6.5, Beats: []float64{0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5, 5, 5.5, 6}, Downbeats: []float64{0.5, 2.5, 4.5}}
	a.Chords = []Segment{{0.5, 1.5, "C", ip(0), "maj", nil}, {1.5, 2.5, "Am", ip(9), "min", nil}, {2.5, 3.5, "C", ip(0), "maj", nil},
		{3.5, 4.5, "Am", ip(9), "min", nil}, {4.5, 5.5, "C", ip(0), "maj", nil}, {5.5, 6.5, "Am", ip(9), "min", nil}}
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

// 转位和弦与 BTC 的扩展性质：记号加「/低音」，低音谱表写低音而不是根音。
func TestSlashAndExtendedSymbols(t *testing.T) {
	slash := Segment{Label: "Db/F", Root: ip(1), Quality: "maj", Bass: ip(5)}
	if got := Symbol(slash, SymbolJazz, 1); got != "D♭/F" {
		t.Errorf("jazz slash %q", got)
	}
	if got := Symbol(slash, SymbolRoman, 1); got != "I/F" {
		t.Errorf("roman slash %q", got)
	}
	for _, c := range []struct {
		seg       Segment
		jazz, std string
	}{
		{Segment{Label: "Bm7b5", Root: ip(11), Quality: "hdim7"}, "Bø7", "Bm7♭5"},
		{Segment{Label: "Gbm6", Root: ip(6), Quality: "min6"}, "G♭-6", "G♭m6"},
		{Segment{Label: "Caug", Root: ip(0), Quality: "aug"}, "C+", "Caug"},
	} {
		if j, s := Symbol(c.seg, SymbolJazz, 0), Symbol(c.seg, SymbolStandard, 0); j != c.jazz || s != c.std {
			t.Errorf("%s: jazz %q standard %q", c.seg.Label, j, s)
		}
	}
	v, ok := ChordVoicing(slash, nil)
	if !ok || v.Bass.MIDI%12 != 5 || len(v.Upper) != 3 {
		t.Fatalf("voicing %+v", v)
	}
	if v, ok := ChordVoicing(Segment{Label: "Ebm7", Root: ip(3), Quality: "min7"}, nil); !ok || len(v.Upper) != 4 {
		t.Errorf("min7 voicing %+v", v)
	}
}
