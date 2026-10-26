package hum

import (
	"bytes"
	"math"
	"testing"

	"kanade/internal/score"
	"kanade/internal/score/synth"
	"kanade/internal/score/vocal"
)

// 6/8 节奏型（以附点四分音符为一拍，每小节 2 拍）：♩♪ ♩♪、♪♪♪ ♩♪、♩. ♪♪♪、♩♪ ♩.，按小节轮流。
func sixEightNotes(bars int) []tempoNote {
	patterns := [][]float64{{2. / 3, 1. / 3, 2. / 3, 1. / 3}, {1. / 3, 1. / 3, 1. / 3, 2. / 3, 1. / 3}, {1, 1. / 3, 1. / 3, 1. / 3}, {2. / 3, 1. / 3, 1}}
	var out []tempoNote
	b := 0.0
	for bar := range bars {
		for i, d := range patterns[bar%len(patterns)] {
			m := 62 + []int{0, 2, 4, 2, 5}[i%5]
			if i == 0 {
				m += 3 // 小节首音上跳，给拍号判断重音
			}
			out = append(out, tempoNote{b, d, m})
			b += d
		}
	}
	return out
}

// 自由哼唱：6/8 被识别为复合拍（附点四分音符 80 BPM、每小节 2 拍）；3/4 的八分音符节奏仍是简单拍。
func TestRhythmCompound(t *testing.T) {
	ev, _, end := synthEvents(sixEightNotes(6), func(int) float64 { return 0.75 }, 11)
	_, _, set, ok := rhythm(ev, end, HumSettings{Free: true})
	if !ok || !set.Compound || set.BeatsPerBar != 2 || set.BPM < 76 || set.BPM > 84 {
		t.Fatalf("6/8: %+v", set)
	}
	ev, _, end = synthEvents(rhythmPattern([]float64{1, 0.5, 0.5, 1}, 6, 0), func(int) float64 { return 0.6 }, 12)
	if _, _, set, _ := rhythm(ev, end, HumSettings{Free: true}); set.Compound {
		t.Errorf("3/4 with eighths taken as compound: %+v", set)
	}
	// 用户指定 6/8：即使跟踪锁在八分音符上，也合成附点四分音符的拍。
	ev, _, end = synthEvents(sixEightNotes(4), func(int) float64 { return 0.75 }, 13)
	_, _, set, _ = rhythm(ev, end, HumSettings{Free: true, BPM: 240, BeatsPerBar: 2, Compound: true})
	if !set.Compound || set.BPM < 76 || set.BPM > 84 {
		t.Errorf("forced 6/8 with an eighth-note tempo hint: %+v", set)
	}
}

// 节拍器模式的 6/8：音符量化到八分 / 十六分音符网格，不出现三连音。
func TestTranscribeHumCompound(t *testing.T) {
	set := HumSettings{BPM: 60, BeatsPerBar: 2, Bars: 2, CountIn: 1, Compound: true}
	notes := []synthNote{{0, 2. / 3, 60, 0}, {2. / 3, 1. / 3, 62, 1}, {1, 1, 64, 2}, {2, 1. / 3, 65, 3}, {7. / 3, 1. / 3, 64, 4},
		{8. / 3, 1. / 3, 62, 5}, {3, 1, 60, 6}}
	h := TranscribeHum(synthFrames(set, notes, synthOpts{}), set)
	checkNotes(t, h, []wantNote{{0, 2. / 3, 60, false}, {2. / 3, 1, 62, false}, {1, 2, 64, false}, {2, 7. / 3, 65, false},
		{7. / 3, 8. / 3, 64, false}, {8. / 3, 3, 62, false}, {3, 4, 60, false}})
}

// 6/8 的导出与节拍器：Analysis 以八分音符为拍（拍号 6/8），MIDI 写 6/8 拍号；节拍器每小节 6 声，强拍、次强拍分开。
func TestCompoundExport(t *testing.T) {
	set := HumSettings{BPM: 60, BeatsPerBar: 2, Bars: 2, Compound: true}
	h := Hum{Settings: set, Mode: "major", Notes: []score.MelodyNote{{Start: 0, End: 1, Pitch: 60}, {Start: 1, End: 4, Pitch: 64}}}
	a := h.Analysis()
	if *a.Meter.Label != "6/8" || a.Meter.Denominator != 8 || a.Meter.BeatsPerBar != 6 || len(a.Beats) != 13 ||
		math.Abs(a.Beats[1]-1.0/3) > 1e-9 || len(a.Downbeats) != 2 || len(a.Measures()) != 2 || *a.Tempo.BPM != 90 {
		t.Fatalf("analysis meter %+v beats %v", a.Meter, a.Beats)
	}
	b, err := synth.MIDI(a)
	if err != nil || !bytes.Contains(b, []byte{0xFF, 0x58, 4, 6, 3, 36, 8}) {
		t.Fatalf("MIDI time signature: %v", err)
	}
	if u, err := vocal.USTX(a, "x"); err != nil || !bytes.Contains(u, []byte("beat_per_bar: 6\n  beat_unit: 8")) {
		t.Errorf("USTX meter: %v", err)
	}
	var levels []int
	for _, c := range set.Clicks(1) {
		levels = append(levels, c.Level)
	}
	if len(levels) != 6 || levels[0] != 2 || levels[3] != 1 || levels[1] != 0 {
		t.Errorf("clicks %v", levels)
	}
	if set.Grid() != 1.0/6 || set.MeterLabel() != "6/8" {
		t.Errorf("grid %v label %q", set.Grid(), set.MeterLabel())
	}
}
