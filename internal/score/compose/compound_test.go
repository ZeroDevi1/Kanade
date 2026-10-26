package compose

import (
	"math"
	"math/rand/v2"
	"testing"

	"kanade/internal/score"
	"kanade/internal/score/hum"
	"kanade/internal/score/karaoke"
)

// 6/8 复合拍的乐句：和声只在拍点换和弦，动机发展保持复合拍网格。
// 6/8 的和声每拍（附点四分音符）一片；动机发展的音在十六分音符网格上。
func TestCompoundHarmonyAndDevelop(t *testing.T) {
	set := hum.HumSettings{BPM: 60, BeatsPerBar: 2, Bars: 2, Compound: true}
	third := 1.0 / 3
	h := hum.Hum{Settings: set, Mode: "major", KeyClear: true, Notes: []score.MelodyNote{
		{Start: 0, End: 2 * third, Pitch: 64}, {Start: 2 * third, End: 1, Pitch: 62}, {Start: 1, End: 2, Pitch: 60},
		{Start: 2, End: 2 + 2*third, Pitch: 67}, {Start: 2 + 2*third, End: 3, Pitch: 65}, {Start: 3, End: 4, Pitch: 64}}}
	if sl := harmSlices(h); len(sl) != 4 || sl[1].start != 1 {
		t.Fatalf("slices %+v", sl)
	}
	for _, r := range Harmonize(h, StyleJazz, 3) {
		for _, c := range r.Chords {
			if f := c.Start - math.Round(c.Start); math.Abs(f) > 1e-9 {
				t.Errorf("chord inside a beat: %+v", c)
			}
		}
	}
	for _, p := range Develop(h, DevelopOptions{Bars: 8}, 3) {
		g := p.Hum.Settings.Grid()
		for _, n := range p.Hum.Notes {
			if math.Abs(n.Start/g-math.Round(n.Start/g)) > 1e-6 || math.Abs(n.End/g-math.Round(n.End/g)) > 1e-6 {
				t.Fatalf("%s: off-grid note %+v", p.Form, n)
			}
		}
		if !p.Hum.Settings.Compound {
			t.Fatal("period lost the compound meter")
		}
	}
}

// 任意帧（噪声、提前停止、只有预备拍）整理出的乐句，各风格配和声都不应崩溃。帧序列与 hum 包的
// TestTranscribeHumRobust 相同（10 ms 一帧）。
func TestHarmonizeNoisyHum(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 200 {
		n := rng.IntN(600)
		frames := make([]karaoke.KaraokeFrame, n)
		for i := range frames {
			f := karaoke.KaraokeFrame{T: float64(i) * 0.01, DB: -70 + 60*rng.Float64(), Clarity: rng.Float64()}
			if rng.IntN(3) > 0 {
				f.F0 = 60 + 1000*rng.Float64()
			}
			frames[i] = f
		}
		for _, set := range []hum.HumSettings{{BPM: 120, BeatsPerBar: 4, Bars: 2, CountIn: 1}, {Free: true}} {
			h := hum.TranscribeHum(frames, set)
			for style := range HarmonyStyles {
				Harmonize(h, style, 3)
			}
		}
	}
}
