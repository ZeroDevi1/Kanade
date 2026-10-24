package score

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// vocalSample 是 120 BPM 匀速、1 拍弱起的旋律：tick = 480 + (秒 − 0.5) × 960。
func vocalSample() *Analysis {
	a := midiSample(true)
	a.Melody.Notes = []MelodyNote{
		{Start: 0.5, End: 1.2, Pitch: 64, Lyric: "天"},
		{Start: 1, End: 1.5, Pitch: 67.2, Slur: true}, // 与前一个音重叠：前一个音截断到 960
		{Start: 2.5, End: 2.7, Pitch: 60, Lyric: "地"},
		{Start: 3, End: 3.4, Pitch: 62, Slur: true}, // 与前一个音有空隙：前一个音补到 2880
		{Start: 3.5, End: 3.8, Pitch: 65},           // 没有歌词
	}
	return a
}

func TestVocalTrack(t *testing.T) {
	notes, tm, err := vocalSample().vocalTrack()
	if err != nil {
		t.Fatal(err)
	}
	want := []vocalNote{
		{480, 960, 64, "天", false},
		{960, 1440, 67, "", true},
		{2400, 2880, 60, "地", false},
		{2880, 3264, 62, "", true},
		{3360, 3648, 65, "", false},
	}
	if !reflect.DeepEqual(notes, want) {
		t.Errorf("notes\n got %v\nwant %v", notes, want)
	}
	if m := tm.meters(); !reflect.DeepEqual(m, []vocalMeter{{0, 1}, {1, 4}}) {
		t.Errorf("meters %v", m)
	}
}

// 字的第一个音换算后长度为 0 时丢弃，字移到下一个连音上；开头没有前一个音的连音当作无歌词的音。
func TestVocalTrackDroppedNote(t *testing.T) {
	a := midiSample(true)
	a.Melody.Notes = []MelodyNote{
		{Start: 0.5, End: 0.7, Pitch: 60, Slur: true},
		{Start: 4, End: 4.0001, Pitch: 64, Lyric: "落"},
		{Start: 4.0005, End: 4.3, Pitch: 65, Slur: true},
	}
	notes, _, err := a.vocalTrack()
	if err != nil {
		t.Fatal(err)
	}
	want := []vocalNote{{480, 672, 60, "", false}, {3840, 4128, 65, "落", false}}
	if !reflect.DeepEqual(notes, want) {
		t.Errorf("notes\n got %v\nwant %v", notes, want)
	}
}

// 弱起正好是整小节时只有一个拍号，从第 0 小节起。
func TestVocalMetersWholeBarPickup(t *testing.T) {
	a := midiSample(true)
	a.Meter.BeatsPerBar = 3
	a.Downbeats = []float64{1.5, 3} // 第一个强拍前 3 拍（0 秒起每拍 0.5 秒）
	_, tm, err := a.vocalTrack()
	if err != nil {
		t.Fatal(err)
	}
	if m := tm.meters(); !reflect.DeepEqual(m, []vocalMeter{{0, 3}}) {
		t.Errorf("meters %v", m)
	}
}

func TestVocalEmpty(t *testing.T) {
	a := midiSample(true)
	a.Melody = nil
	if _, err := a.SVP("x"); err == nil {
		t.Error("svp exported without melody")
	}
	if _, err := a.USTX("x"); err == nil {
		t.Error("ustx exported without melody")
	}
}

func TestSVP(t *testing.T) {
	data, err := vocalSample().SVP("秋天")
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != 0 {
		t.Fatal("svp must end with a NUL byte")
	}
	var p svpProject
	if err := json.Unmarshal(bytes.TrimSuffix(data, []byte{0}), &p); err != nil {
		t.Fatal(err)
	}
	if p.Version != svpVersion || len(p.Tracks) != 1 {
		t.Fatalf("version %d tracks %d", p.Version, len(p.Tracks))
	}
	if !reflect.DeepEqual(p.Time.Meter, []svpMeter{{0, 1, 4}, {1, 4, 4}}) || !reflect.DeepEqual(p.Time.Tempo, []svpTempo{{0, 120}}) {
		t.Errorf("time %+v", p.Time)
	}
	tr := p.Tracks[0]
	if tr.Name != "秋天" || tr.MainRef.GroupID != tr.MainGroup.UUID || tr.MainGroup.UUID == "" {
		t.Errorf("track %q group %q ref %q", tr.Name, tr.MainGroup.UUID, tr.MainRef.GroupID)
	}
	var lyrics []string
	for _, n := range tr.MainGroup.Notes {
		lyrics = append(lyrics, n.Lyrics)
	}
	if !reflect.DeepEqual(lyrics, []string{"天", "-", "地", "-", "la"}) {
		t.Errorf("lyrics %v", lyrics)
	}
	if n := tr.MainGroup.Notes[0]; n.Onset != 480*705600000/midiPPQ || n.Duration != 705600000 || n.Pitch != 64 {
		t.Errorf("first note %+v", n)
	}
}

func TestUSTX(t *testing.T) {
	data, err := vocalSample().USTX("秋天")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"name: \"秋天\"\n", "ustx_version: \"0.6\"\n",
		"- bar_position: 0\n  beat_per_bar: 1\n", "- bar_position: 1\n  beat_per_bar: 4\n",
		"- position: 0\n  bpm: 120\n",
		"  - position: 480\n    duration: 480\n    tone: 64\n    lyric: \"天\"\n",
		"  - position: 960\n    duration: 480\n    tone: 67\n    lyric: \"+\"\n",
		"  - position: 3360\n    duration: 288\n    tone: 65\n    lyric: \"a\"\n",
		"  duration: 3648\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if n := strings.Count(s, "    pitch:\n"); n != 5 {
		t.Errorf("pitch blocks %d", n)
	}
}
