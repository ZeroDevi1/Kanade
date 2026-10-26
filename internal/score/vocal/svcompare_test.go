package vocal

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

// 引导式音高：原唱与基准渲染只在「地」（2.5–2.7 秒；工程里这个音补到 3.0 秒接上连音，之后的无声帧沿用最后的偏差）
// 上差 100 音分，pitchDelta 只在这里补上，其余为 0；音符保持 AI 音高（不写 instantMode、不关颤音）。
func TestGuidedPitch(t *testing.T) {
	a := vocalSample()
	orig := syntheticExpression(a, 30, false)
	base := syntheticExpression(a, 30, false)
	for i := range base.F0 {
		if ti := float64(i) * base.Hop; ti >= 2.5 && ti < 2.7 {
			base.F0[i] *= math.Pow(2, -100.0/1200)
		}
	}
	p := DefaultSVPreset()
	p.AutoTranspose, p.PitchStrength = false, 1
	data, err := SVPExport(a, "秋天", SVExport{Preset: p, Expr: orig, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	var proj svpProject
	json.Unmarshal(bytes.TrimSuffix(data, []byte{0}), &proj)
	for _, n := range proj.Tracks[0].MainGroup.Notes {
		if n.InstantMode != nil || len(n.Attributes) != 0 {
			t.Fatalf("guided pitch changed note mode: %+v", n)
		}
	}
	pos, vals := curvePoints(t, proj.Tracks[0].MainGroup.Parameters["pitchDelta"])
	peak := 0.0
	for k, v := range vals {
		sec := pos[k] / float64(svpBlicksPerTick) / 960 // vocalSample 为 120 BPM：每秒 960 tick
		switch {
		case sec >= 2.55 && sec < 2.65:
			peak = max(peak, v)
		case sec < 2.4 || sec > 3.1:
			if v != 0 {
				t.Fatalf("pitchDelta %v at %.2f s outside the differing note", v, sec)
			}
		}
	}
	if math.Abs(peak-100) > 1 {
		t.Errorf("peak %v, want 100", peak)
	}
}

// 换气：乐句前停顿 ≥ 0.3 秒处插入 br 音符，紧接乐句第一个音、不与上一个音重叠；停顿短的地方不插。
func TestSVPBreaths(t *testing.T) {
	a := vocalSample()
	data, err := SVPExport(a, "秋天", SVExport{Preset: SVPreset{}, Breaths: true})
	if err != nil {
		t.Fatal(err)
	}
	var proj svpProject
	json.Unmarshal(bytes.TrimSuffix(data, []byte{0}), &proj)
	var br []svpNote
	notes := proj.Tracks[0].MainGroup.Notes
	for i, n := range notes {
		if n.Phonemes == "br" {
			br = append(br, n)
			if i+1 >= len(notes) || n.Onset+n.Duration != notes[i+1].Onset {
				t.Errorf("breath %d not right before a note", i)
			}
			if i > 0 && notes[i-1].Onset+notes[i-1].Duration > n.Onset {
				t.Errorf("breath %d overlaps the previous note", i)
			}
		}
	}
	// 0.5 秒处的第一句（停顿 0.5 秒）与 2.5 秒处的第二句（停顿 1 秒）前各一个，0.45 秒长；3.5 秒处的音前只停 0.1 秒。
	b := int64(svpBlicksPerTick)
	if len(br) != 2 || br[0].Onset != 48*b || br[0].Duration != 432*b || br[1].Onset != 1968*b || br[1].Pitch != 60 {
		t.Errorf("breaths %+v", br)
	}
}

// 渲染对比：渲染整体高 20 音分、响 6 dB 时，对比给出相应的偏差；乐句按停顿划分。
func TestCompareRender(t *testing.T) {
	a := vocalSample()
	orig := syntheticExpression(a, 0, false)
	render := syntheticExpression(a, 20, false)
	for i := range render.Loudness {
		render.Loudness[i] += 6
	}
	c := CompareRender(a, orig, render, 0)
	if c.Frames == 0 || math.Abs(c.PitchMedianAbs-20) > 0.5 || c.PitchWithin50 != 1 || math.Abs(c.LoudnessDiff-6) > 0.01 {
		t.Fatalf("comparison %+v", c)
	}
	if c.LoudnessCorr < 0.99 || c.TiltDiff != 0 || c.VoicedRender != c.VoicedOrig {
		t.Errorf("comparison %+v", c)
	}
	// 移调 +12 渲染的高八度与原唱相比，换算回原调后仍是 20 音分。
	up := syntheticExpression(a, 20, true)
	if c := CompareRender(a, orig, up, 12); math.Abs(c.PitchMedianAbs-20) > 0.5 {
		t.Errorf("transposed %v", c.PitchMedianAbs)
	}
	// 「地」与 3.5 秒处的音只隔 0.1 秒，同属第二句。
	if len(c.Phrases) != 2 || c.Phrases[0].Lyric != "天" || c.Phrases[1].Lyric != "地" || c.Phrases[1].End < 3.79 {
		t.Errorf("phrases %+v", c.Phrases)
	}
}
