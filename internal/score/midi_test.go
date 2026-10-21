package score

import (
	"bytes"
	"encoding/binary"
	"testing"
)

type parsedEvent struct {
	tick int
	data []byte
}

// parseMIDI 是测试用的最小 SMF 解析器：返回格式、PPQ 与各音轨的事件（绝对 tick，不含音轨结束）。
func parseMIDI(t *testing.T, data []byte) (format, ppq int, tracks [][]parsedEvent) {
	t.Helper()
	if string(data[:4]) != "MThd" || binary.BigEndian.Uint32(data[4:8]) != 6 {
		t.Fatalf("bad header % x", data[:14])
	}
	format, n, ppq := int(binary.BigEndian.Uint16(data[8:])), int(binary.BigEndian.Uint16(data[10:])), int(binary.BigEndian.Uint16(data[12:]))
	r := bytes.NewReader(data[14:])
	for range n {
		var hdr [4]byte
		var size uint32
		r.Read(hdr[:])
		binary.Read(r, binary.BigEndian, &size)
		if string(hdr[:]) != "MTrk" {
			t.Fatalf("bad track header %q", hdr)
		}
		body := make([]byte, size)
		r.Read(body)
		var evs []parsedEvent
		tick := 0
		for i := 0; i < len(body); {
			d := 0
			for {
				b := body[i]
				i++
				d = d<<7 | int(b&0x7F)
				if b&0x80 == 0 {
					break
				}
			}
			tick += d
			var l int
			switch st := body[i]; {
			case st == 0xFF:
				l = 3 + int(body[i+2])
			case st&0xF0 == 0xC0:
				l = 2
			default:
				l = 3
			}
			if body[i] == 0xFF && body[i+1] == 0x2F {
				break
			}
			evs = append(evs, parsedEvent{tick, body[i : i+l]})
			i += l
		}
		tracks = append(tracks, evs)
	}
	return format, ppq, tracks
}

func notesOn(tr []parsedEvent) map[int][]int {
	out := map[int][]int{} // tick → 音高
	for _, e := range tr {
		if e.data[0]&0xF0 == 0x90 && e.data[2] > 0 {
			out[e.tick] = append(out[e.tick], int(e.data[1]))
		}
	}
	return out
}

func midiSample(stable bool) *Analysis {
	a := &Analysis{Duration: 5}
	a.Beats = []float64{0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5}
	a.Downbeats = []float64{0.5, 2.5, 4.5}
	a.Meter.BeatsPerBar = 4
	bpm := 120.0
	a.Tempo.BPM, a.Tempo.Stable = &bpm, &stable
	a.Chords = []Segment{{Start: 0.5, End: 2.5, Label: "C", Root: ip(0), Quality: "maj"}}
	a.Melody = &Melody{Notes: []MelodyNote{{Start: 0.5, End: 1.2, Pitch: 64}, {Start: 1, End: 1.5, Pitch: 67.2}}}
	return a
}

// 匀速：一个速度；第一个强拍前的 1 拍弱起写成 1/4 小节，强拍落在 tick 480（第 2 小节起点）。
func TestMIDIStable(t *testing.T) {
	data, err := midiSample(true).MIDI()
	if err != nil {
		t.Fatal(err)
	}
	format, ppq, tracks := parseMIDI(t, data)
	if format != 1 || ppq != midiPPQ || len(tracks) != 3 {
		t.Fatalf("format %d ppq %d tracks %d", format, ppq, len(tracks))
	}
	var sigs, tempos []parsedEvent
	for _, e := range tracks[0] {
		switch {
		case e.data[0] == 0xFF && e.data[1] == 0x58:
			sigs = append(sigs, e)
		case e.data[0] == 0xFF && e.data[1] == 0x51:
			tempos = append(tempos, e)
		}
	}
	if len(sigs) != 2 || sigs[0].tick != 0 || sigs[0].data[3] != 1 || sigs[1].tick != 480 || sigs[1].data[3] != 4 {
		t.Errorf("time signatures %v", sigs)
	}
	if len(tempos) != 1 || us(tempos[0]) != 500000 {
		t.Errorf("tempos %v", tempos)
	}
	mel := notesOn(tracks[1])
	if len(mel[480]) != 1 || mel[480][0] != 64 || len(mel[960]) != 1 || mel[960][0] != 67 {
		t.Errorf("melody notes %v", mel)
	}
	// 旋律不重叠：第一个音在第二个音开始（tick 960）时松开。
	for _, e := range tracks[1] {
		if e.data[0]&0xF0 == 0x80 && e.data[1] == 64 && e.tick != 960 {
			t.Errorf("melody note 64 off at %d", e.tick)
		}
	}
	if ch := notesOn(tracks[2]); len(ch[480]) != 4 { // 低音 + 三个和弦音
		t.Errorf("chord notes %v", ch)
	}
}

// 变速：每拍一个速度，拍点准确落在整拍上。
func TestMIDIVariableTempo(t *testing.T) {
	a := midiSample(false)
	a.Beats = []float64{0.5, 1, 1.6, 2.1, 2.5, 3, 3.5, 4, 4.5}
	a.Melody.Notes = []MelodyNote{{Start: 2.1, End: 2.4, Pitch: 60}}
	data, err := a.MIDI()
	if err != nil {
		t.Fatal(err)
	}
	_, _, tracks := parseMIDI(t, data)
	n := 0
	for _, e := range tracks[0] {
		if e.data[0] == 0xFF && e.data[1] == 0x51 {
			n++
		}
	}
	if n < 3 {
		t.Errorf("tempo events %d", n)
	}
	if mel := notesOn(tracks[1]); len(mel[480+3*480]) != 1 { // 2.1 秒是强拍后的第 3 拍
		t.Errorf("melody %v", mel)
	}
}

func us(e parsedEvent) int { return int(e.data[3])<<16 | int(e.data[4])<<8 | int(e.data[5]) }

func TestMIDIEmpty(t *testing.T) {
	if _, err := (&Analysis{Duration: 1}).MIDI(); err == nil {
		t.Error("empty analysis exported")
	}
}

// 歌词：字写在字的第一个音按下处，连音写 "-"。
func TestMIDILyrics(t *testing.T) {
	a := midiSample(true)
	a.Melody.Notes[0].Lyric = "天"
	a.Melody.Notes[1].Slur = true
	data, err := a.MIDI()
	if err != nil {
		t.Fatal(err)
	}
	_, _, tracks := parseMIDI(t, data)
	got := map[int]string{}
	for _, e := range tracks[1] {
		if e.data[0] == 0xFF && e.data[1] == 0x05 {
			got[e.tick] = string(e.data[3:])
		}
	}
	if len(got) != 2 || got[480] != "天" || got[960] != "-" {
		t.Errorf("lyric events %v", got)
	}
}
