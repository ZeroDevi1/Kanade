package synth

import (
	"math"
	"testing"

	"kanade/internal/score"
)

// 演奏设置：力度倍率乘在逐音力度（未设置时为默认力度）上，移八度与时值倍率只作用于有音高的乐器。
func TestInstrumentNotes(t *testing.T) {
	in := score.Instrument{Name: "acoustic_piano", Notes: []score.InstrumentNote{
		{Start: 1, End: 2, Pitch: 60}, {Start: 2, End: 2.5, Pitch: 120, Velocity: 0.4}},
		Play: score.Performance{Velocity: 1.5, Length: 0.5, Octave: 1}}
	got := InstrumentNotes(in)
	want := []PianoNote{{MIDI: 72, Start: 1, End: 1.5, Velocity: 1}, {MIDI: 120, Start: 2, End: 2.25, Velocity: 0.6000000000000001}}
	for i := range want {
		if math.Abs(got[i].Velocity-want[i].Velocity) > 1e-9 || got[i].MIDI != want[i].MIDI || got[i].End != want[i].End {
			t.Errorf("note %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	drum := score.Instrument{Drum: true, Notes: []score.InstrumentNote{{Start: 0, End: 0.05, Pitch: 36}},
		Play: score.Performance{Octave: 2, Length: 2}}
	if n := InstrumentNotes(drum)[0]; n.MIDI != 36 || n.End != 0.05 || n.Velocity != score.DefaultNoteVelocity {
		t.Errorf("drum %+v", n)
	}
}

// 踏板：每小节换踏板时在小节线后再踩下；松开落在踩下区间内的音延长到抬起处，但不越过同音高的下一次按下。
func TestPedal(t *testing.T) {
	a := &score.Analysis{Duration: 4, Downbeats: []float64{0, 2}}
	spans := PedalSpans(a, score.PedalBar)
	if len(spans) != 2 || spans[0] != (Span{pedalRepress, 2}) || spans[1] != (Span{2 + pedalRepress, 4}) {
		t.Fatalf("spans %+v", spans)
	}
	a.Chords = []score.Segment{{Start: 0, End: 1, Root: ip(0)}, {Start: 1, End: 3}, {Start: 3, End: 4, Root: ip(7)}}
	if got := PedalSpans(a, score.PedalChord); len(got) != 2 || got[1].Start != 3+pedalRepress {
		t.Errorf("chord spans %+v", got)
	}
	if PedalSpans(a, score.PedalOff) != nil {
		t.Error("pedal off")
	}
	notes := []PianoNote{
		{MIDI: 60, Start: 0, End: 0.5}, // 延长到 1.0（被 1.0 处的同音截住之前是 2.0）
		{MIDI: 64, Start: 0.2, End: 0.6},
		{MIDI: 60, Start: 1, End: 1.2},
		{MIDI: 67, Start: 1.9, End: 2.01}, // 松开落在换踏板的空隙里：不延长
	}
	got := applyPedal(notes, spans)
	ends := []float64{1, 2, 2, 2.01}
	for i, e := range ends {
		if got[i].End != e {
			t.Errorf("note %d end %v, want %v", i, got[i].End, e)
		}
	}
	if notes[0].End != 0.5 {
		t.Error("input modified")
	}
}

// 按原曲估计力度：同一音高三个响度不同的音，估计出的力度按响度排序，中间的约为默认力度。
func TestEstimateVelocities(t *testing.T) {
	const rate = 16000
	notes := []PianoNote{{MIDI: 69, Start: 0.2, End: 0.5}, {MIDI: 69, Start: 0.8, End: 1.1}, {MIDI: 69, Start: 1.4, End: 1.7}}
	amps := []float64{0.05, 0.4, 0.15}
	pcm := make([]float32, 2*rate*2) // 2 秒立体声
	for k, n := range notes {
		for i := int(n.Start * rate); i < int(n.End*rate); i++ {
			v := float32(amps[k] * math.Sin(2*math.Pi*440*float64(i)/rate))
			pcm[2*i], pcm[2*i+1] = v, v
		}
	}
	v := EstimateVelocities(pcm, 2, rate, notes, false)
	if len(v) != 3 || !(v[0] < v[2] && v[2] < v[1]) || math.Abs(v[2]-score.DefaultNoteVelocity) > 0.01 {
		t.Errorf("velocities %v", v)
	}
	drums := EstimateVelocities(pcm, 2, rate, notes, true)
	if !(drums[0] < drums[2] && drums[2] < drums[1]) {
		t.Errorf("drum velocities %v", drums)
	}
	if EstimateVelocities(nil, 2, rate, notes, false) != nil {
		t.Error("no audio")
	}
}

// 改乐器后同名的乐器各自成轨，第二条标题带序号；Source 指回乐器中的音符序号。
func TestTracksDuplicateInstruments(t *testing.T) {
	a := &score.Analysis{Duration: 2, Downbeats: []float64{0}}
	a.Transcription = &score.Transcription{Instruments: []score.Instrument{
		{Name: "acoustic_piano", Notes: []score.InstrumentNote{{Start: 1, End: 1.5, Pitch: 60}, {Start: 0.5, End: 1, Pitch: 62}}},
		{Name: "acoustic_piano", Notes: []score.InstrumentNote{{Start: 0, End: 1, Pitch: 48}}, Play: score.Performance{Pedal: score.PedalBar}},
	}}
	tracks := Tracks(a)
	p1, p2 := tracks[TrackInstruments], tracks[TrackInstruments+1]
	if p1.Title != "原声钢琴" || p2.Title != "原声钢琴 2" || p2.Instrument != 1 || p1.Pedal != nil || len(p2.Pedal) != 1 {
		t.Fatalf("tracks %+v / %+v", p1, p2)
	}
	if p1.Notes[0].MIDI != 62 || p1.Source[0] != 1 || p1.Source[1] != 0 {
		t.Errorf("sorted notes %+v source %v", p1.Notes, p1.Source)
	}
	if SameSound(p1, p2) || !SameSound(p1, Tracks(a)[TrackInstruments]) {
		t.Error("SameSound")
	}
}

// MIDI 导出写入逐音力度与踏板（CC64）。
func TestMIDIPerformance(t *testing.T) {
	a := midiSample(true)
	a.Transcription = &score.Transcription{Instruments: []score.Instrument{
		{Name: "acoustic_piano", Program: 0, Notes: []score.InstrumentNote{{Start: 1, End: 1.5, Pitch: 60, Velocity: 0.5}},
			Play: score.Performance{Pedal: score.PedalBar}},
	}}
	data, err := MIDI(a)
	if err != nil {
		t.Fatal(err)
	}
	_, _, tracks := parseMIDI(t, data)
	var vel, down, up int
	for _, e := range tracks[3] {
		switch {
		case e.data[0] == 0x92 && e.data[1] == 60:
			vel = int(e.data[2])
		case e.data[0] == 0xB2 && e.data[1] == 64 && e.data[2] == 127:
			down++
		case e.data[0] == 0xB2 && e.data[1] == 64 && e.data[2] == 0:
			up++
		}
	}
	if vel != 64 || down == 0 || down != up {
		t.Errorf("velocity %d pedal down %d up %d", vel, down, up)
	}
}
