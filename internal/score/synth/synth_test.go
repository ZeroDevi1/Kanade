package synth

import (
	"math"
	"reflect"
	"testing"

	"kanade/internal/score"
)

func TestPianoTracks(t *testing.T) {
	a := &score.Analysis{Duration: 2, Downbeats: []float64{0, 1}}
	a.Chords = []score.Segment{{Start: 0, End: 2, Label: "C", Root: ip(0), Quality: "maj"}}
	tracks := Tracks(a)
	pcm, mel := TrackPCM(a, tracks, TrackChords, 8000), TrackPCM(a, tracks, TrackMelody, 8000)
	if mel != nil {
		t.Error("melody track without melody")
	}
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
	if peak == 0 || peak > 1 || energy(8000, 8800) <= energy(7200, 8000) {
		t.Errorf("peak %v, energy before %v after %v", peak, energy(7200, 8000), energy(8000, 8800))
	}
}

func TestPianoModel(t *testing.T) {
	const rate = 16000
	// 非谐性：高音的第 10 泛音比整数倍更高。
	if inharmonicity(84) <= inharmonicity(48) {
		t.Error("inharmonicity should grow with pitch")
	}
	highEnergy := func(pcm []float32) float64 {
		// 一阶差分的能量：差分放大高频，用来比较音色明暗。
		e := 0.0
		for i := 1; i < len(pcm); i++ {
			d := float64(pcm[i] - pcm[i-1])
			e += d * d
		}
		return e
	}
	soft := RenderPiano([]PianoNote{{MIDI: 60, Start: 0, End: 1, Velocity: 0.3}}, rate, 1.5)
	loud := RenderPiano([]PianoNote{{MIDI: 60, Start: 0, End: 1, Velocity: 0.8}}, rate, 1.5)
	rms := func(pcm []float32) float64 {
		e := 0.0
		for _, x := range pcm {
			e += float64(x) * float64(x)
		}
		return e
	}
	// 力度越大越响、越亮：高频能量占比更高。
	if rms(loud) <= rms(soft) || highEnergy(loud)/rms(loud) <= highEnergy(soft)/rms(soft) {
		t.Errorf("velocity: soft %.3g/%.3g loud %.3g/%.3g", rms(soft), highEnergy(soft), rms(loud), highEnergy(loud))
	}
	// 松键后迅速衰减：1.2 秒后的能量远小于按住期间。
	if rms(soft[int(1.2*rate):]) > rms(soft[:rate])*0.01 {
		t.Error("damper did not stop the note")
	}
}

func TestTrackMix(t *testing.T) {
	all := []bool{true, true, true, true}
	m := NewTrackMix(4)
	if !m.OriginalOnly(all) || len(m.Synthesized(all)) != 0 {
		t.Error("default should be original only")
	}
	// 独奏旋律：只有旋律发声，原曲与和弦即使未静音也不发声。
	m.Mute = make([]bool, 4)
	m.Solo[TrackMelody] = true
	if a := m.Audible(all); !reflect.DeepEqual(a, []bool{false, false, true, false}) {
		t.Errorf("solo melody %v", a)
	}
	// 没有旋律轨时独奏它无效，回到未静音的音轨。
	if a := m.Audible([]bool{true, true, false, true}); !reflect.DeepEqual(a, []bool{true, true, false, true}) {
		t.Errorf("unavailable solo %v", a)
	}
	// 混音：原曲 ×0.5 + 和弦 + 乐器 ×2（静音旋律）。
	m = TrackMix{Mute: []bool{false, false, true, false}, Solo: make([]bool, 4), Gain: []float64{0.5, 1, 1, 2}}
	synth := [][]float32{nil, {0.1, 0.3}, {0.9, 0.9}, {0.05}}
	out := MixTracks([]float32{0.4, 0.4, 0.2, 0.2}, 2, synth, m, all)
	want := []float32{0.4, 0.4, 0.4, 0.4}
	for i := range want {
		if d := out[i] - want[i]; d > 1e-6 || d < -1e-6 {
			t.Fatalf("mix %v, want %v", out, want)
		}
	}
	if s := m.Synthesized(all); !reflect.DeepEqual(s, []int{TrackChords, 3}) {
		t.Errorf("synthesized %v", s)
	}
	// 换成乐器数不同的结果：保留已有设置，新增的音轨静音。
	m.Resize(6)
	if len(m.Gain) != 6 || m.Gain[3] != 2 || !m.Mute[5] || m.Solo[5] {
		t.Errorf("resize %+v", m)
	}
}

// 乐器音轨：每种音色族与鼓都发出声音，并调到相近的响度；空音轨为 nil。
func TestRenderTrack(t *testing.T) {
	const rate = 16000
	for _, tr := range []Track{
		{Program: 0}, {Program: 4}, {Program: 6}, {Program: 25}, {Program: 30}, {Program: 32}, {Program: 33}, {Program: 46}, {Program: 19}, {Program: 40}, {Program: 48}, {Program: 56},
		{Program: 65}, {Program: 73}, {Program: 81}, {Program: 89}, {Program: 11}, {Program: 117},
		{Drum: true, Notes: []PianoNote{{MIDI: 36, Start: 0.1, End: 0.15, Velocity: 0.7}, {MIDI: 38, Start: 0.5, End: 0.55, Velocity: 0.7},
			{MIDI: 42, Start: 0.7, End: 0.75, Velocity: 0.7}, {MIDI: 49, Start: 0.9, End: 0.95, Velocity: 0.7}, {MIDI: 45, Start: 1.2, End: 1.25, Velocity: 0.7}}},
	} {
		if tr.Notes == nil {
			tr.Notes = []PianoNote{{MIDI: 48, Start: 0.1, End: 0.6, Velocity: 0.7}, {MIDI: 60, Start: 0.6, End: 1.2, Velocity: 0.7},
				{MIDI: 76, Start: 1.2, End: 1.6, Velocity: 0.7}}
		}
		out := RenderTrack(tr, rate, 2, true)
		if len(out) != 2*rate {
			t.Fatalf("program %d drum %v: %d samples", tr.Program, tr.Drum, len(out))
		}
		var sum float64
		var n int
		var peak float32
		for _, v := range out {
			if math.IsNaN(float64(v)) {
				t.Fatalf("program %d drum %v: NaN", tr.Program, tr.Drum)
			}
			if v > 1e-4 || v < -1e-4 {
				sum += float64(v) * float64(v)
				n++
			}
			peak = max(peak, v, -v)
		}
		rms := math.Sqrt(sum / float64(max(n, 1)))
		if peak > 0.9001 || rms < instrumentRMS/4 || rms > instrumentRMS*1.01 {
			t.Errorf("program %d drum %v: rms %.3f peak %.3f", tr.Program, tr.Drum, rms, peak)
		}
	}
	if out := RenderTrack(Track{}, rate, 2, true); out != nil {
		t.Error("empty track rendered")
	}
}
