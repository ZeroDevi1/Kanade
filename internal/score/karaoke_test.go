package score

import (
	"math"
	"testing"
)

// 合成的「演唱」：按旋律音唱正弦波（含 5 Hz、±30 音分的颤音），整体晚 lag 秒、低一个八度，
// 最后一个长音只唱到一半（气息不足）。
func singAlong(notes []MelodyNote, lag float64, cut bool) *KaraokeSession {
	const rate = 32000
	s := NewKaraokeSession(rate)
	end := notes[len(notes)-1].End + 0.5
	buf := make([]float32, 1024)
	phase := 0.0
	for i := 0; float64(i)/rate < end; i += len(buf) {
		for k := range buf {
			t := float64(i+k)/rate - lag
			buf[k] = 0.0005 * float32(math.Sin(float64(i+k)))
			for j, n := range notes {
				stop := n.End
				if cut && j == len(notes)-1 {
					stop = n.Start + 0.5*(n.End-n.Start)
				}
				if t >= n.Start && t < stop {
					f := midiFreqF(n.Pitch-12) * math.Pow(2, 30*math.Sin(2*math.Pi*5*t)/1200)
					phase += 2 * math.Pi * f / rate
					buf[k] = 0.3 * float32(math.Sin(phase))
				}
			}
		}
		s.Push(buf, float64(i+len(buf))/rate)
	}
	return s
}

func TestYinPitch(t *testing.T) {
	for _, f := range []float64{82.4, 220, 440, 880} {
		w := make([]float32, karaokeWindow)
		for i := range w {
			w[i] = float32(math.Sin(2 * math.Pi * f * float64(i) / KaraokeRate))
		}
		got, clarity := yinPitch(w, KaraokeRate)
		if math.Abs(1200*math.Log2(got/f)) > 5 || clarity < 0.9 {
			t.Errorf("%v Hz: got %v clarity %v", f, got, clarity)
		}
	}
	noise := make([]float32, karaokeWindow)
	for i := range noise {
		noise[i] = float32(math.Sin(float64(i*i) * 0.37))
	}
	if f, c := yinPitch(noise, KaraokeRate); f != 0 && c > karaokeClarity {
		t.Errorf("noise: %v Hz clarity %v", f, c)
	}
}

func TestAnalyzeKaraoke(t *testing.T) {
	notes := []MelodyNote{
		{Start: 1.0, End: 1.4, Pitch: 64, Lyric: "我"}, {Start: 1.4, End: 1.8, Pitch: 67, Lyric: "不"},
		{Start: 1.8, End: 3.0, Pitch: 69, Lyric: "想"},
		{Start: 3.6, End: 4.0, Pitch: 67, Lyric: "念"}, {Start: 4.0, End: 5.2, Pitch: 64, Lyric: "你"},
	}
	full := AnalyzeKaraoke(notes, singAlong(notes, 0.2, false).Frames(), 0, 6)
	if full.Octave != -1 || math.Abs(full.Lag-0.2) > 0.02 {
		t.Fatalf("lag %v octave %d", full.Lag, full.Octave)
	}
	if full.InTune < 0.9 || full.Coverage < 0.9 || full.Sustain < 0.95 || full.Score < 85 {
		t.Errorf("full: %+v", full)
	}
	if len(full.Phrases) != 2 || full.Phrases[0].Lyric != "我不想" || full.Phrases[1].Lyric != "念你" {
		t.Errorf("phrases %+v", full.Phrases)
	}
	cut := AnalyzeKaraoke(notes, singAlong(notes, 0.2, true).Frames(), 0, 6)
	if cut.Sustain > 0.6 || cut.Breath >= full.Breath || cut.Score >= full.Score {
		t.Errorf("cut sustain %v breath %v/%v score %v/%v", cut.Sustain, cut.Breath, full.Breath, cut.Score, full.Score)
	}
	// 没有录到演唱（只有底噪）：Voiced 为 0。
	quiet := NewKaraokeSession(32000)
	quiet.Push(make([]float32, 32000), 1)
	if r := AnalyzeKaraoke(notes, quiet.Frames(), 0, 6); r.Voiced != 0 {
		t.Errorf("quiet voiced %d", r.Voiced)
	}
}
