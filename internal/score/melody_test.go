package score

import "testing"

func TestMelodyLayout(t *testing.T) {
	// 两小节 4/4，每拍 0.5 秒。
	a := &Analysis{Duration: 4, Downbeats: []float64{0, 2}}
	for i := 0; i < 8; i++ {
		a.Beats = append(a.Beats, float64(i)*0.5)
	}
	a.Melody = &Melody{Notes: []MelodyNote{
		{Start: 0.0, End: 0.5, Pitch: 60.2},  // 第 1 拍：四分音符 C4（音高四舍五入）
		{Start: 0.5, End: 0.74, Pitch: 61.9}, // 第 2 拍前半：八分音符 D4
		// 第 2 拍后半空：八分休止
		{Start: 1.0, End: 2.5, Pitch: 63.0}, // 第 3 拍起 3 拍，跨小节：二分音符连到下一小节的四分音符
	}}
	ms := a.Measures()
	lay := a.MelodyLayout(ms, 0, true)
	if len(lay) != 2 || lay[0].Slots != 8 {
		t.Fatalf("layout %+v", lay)
	}
	got := lay[0].Items
	want := []struct {
		slot, l int
		rest    bool
		name    string
		tie     bool
	}{{0, 2, false, "C4", false}, {2, 1, false, "D4", false}, {3, 1, true, "", false}, {4, 4, false, "E♭4", true}}
	if len(got) != len(want) {
		t.Fatalf("items %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Slot != w.slot || g.Len != w.l || g.Rest != w.rest || (!w.rest && g.Note.String() != w.name) || g.TieNext != w.tie {
			t.Errorf("item %d = %+v (%s), want %+v", i, g, g.Note, w)
		}
	}
	// 第二小节：连过来的 E♭4 占 1 拍（2 格），其后休止 3 拍（附点二分休止）。
	if it := lay[1].Items; len(it) != 2 || it[0].Len != 2 || it[0].Rest || !it[1].Rest || it[1].Len != 6 {
		t.Errorf("measure 2 %+v", it)
	}
	if a.MelodyTranspose() != 12 {
		t.Errorf("low melody should be written an octave up")
	}
}

func TestBeamGroups(t *testing.T) {
	note := func(slot, l int) MelodyItem { return MelodyItem{Slot: slot, Len: l} }
	rest := func(slot, l int) MelodyItem { return MelodyItem{Slot: slot, Len: l, Rest: true} }
	// 4/4：前半小节 4 个八分连成一组；后半小节八分、八分休止、八分、八分 → 休止打断，后两个成组；
	mm := MelodyMeasure{Slots: 8, Items: []MelodyItem{
		note(0, 1), note(1, 1), note(2, 1), note(3, 1),
		note(4, 1), rest(5, 1), note(6, 1), note(7, 1),
	}}
	got := mm.BeamGroups()
	if len(got) != 2 || len(got[0]) != 4 || got[1][0] != 6 || got[1][1] != 7 {
		t.Errorf("4/4 groups %v", got)
	}
	// 跨半小节不连：第 2 拍后半与第 3 拍前半各自独立（单个八分保留符尾）。
	mm = MelodyMeasure{Slots: 8, Items: []MelodyItem{note(0, 3), note(3, 1), note(4, 1), note(5, 3)}}
	if got := mm.BeamGroups(); len(got) != 0 {
		t.Errorf("cross half-bar %v", got)
	}
	// 3/4：按单拍分组。
	mm = MelodyMeasure{Slots: 6, Items: []MelodyItem{note(0, 1), note(1, 1), note(2, 1), note(3, 1), note(4, 2)}}
	if got := mm.BeamGroups(); len(got) != 2 || len(got[0]) != 2 || got[1][0] != 2 {
		t.Errorf("3/4 groups %v", got)
	}
}

// 歌词：字只写在音的第一段（跨小节的后续段不重复），连音标记保留；量化到同一格时被丢掉的音若带字，字移到留下的音上。
func TestMelodyLayoutLyrics(t *testing.T) {
	a := &Analysis{Duration: 4, Downbeats: []float64{0, 2}}
	for i := 0; i < 8; i++ {
		a.Beats = append(a.Beats, float64(i)*0.5)
	}
	a.Melody = &Melody{Notes: []MelodyNote{
		{Start: 0.0, End: 0.25, Pitch: 72, Lyric: "秋"},
		{Start: 0.25, End: 0.5, Pitch: 74, Slur: true}, // 连音：秋的第二个音
		{Start: 0.5, End: 0.55, Pitch: 76, Slur: true}, // 与下一个带字的音量化到同一格，被丢掉
		{Start: 0.52, End: 1.0, Pitch: 77, Lyric: "天"},
		{Start: 1.5, End: 2.5, Pitch: 79, Lyric: "满"}, // 跨小节
	}}
	lay := a.MelodyLayout(a.Measures(), 0, true)
	var lyr, names []string
	var slur []bool
	for _, mm := range lay {
		for _, it := range mm.Items {
			if !it.Rest {
				lyr, slur, names = append(lyr, it.Lyric), append(slur, it.Slur), append(names, it.Note.String())
			}
		}
	}
	want := []string{"秋", "", "天", "满", ""}
	if len(lyr) != len(want) {
		t.Fatalf("lyrics %q", lyr)
	}
	for i := range want {
		if lyr[i] != want[i] {
			t.Errorf("lyrics %q, want %q", lyr, want)
			break
		}
	}
	if names[2] != "F5" {
		t.Errorf("kept note %s, want the lyric note F5", names[2])
	}
	if !slur[1] || slur[2] || slur[4] {
		t.Errorf("slur flags %v", slur)
	}
}

// 快唱的几个带字的音量化到同一格时，字合并到留下的音上，不丢字；英文单词之间加空格。
func TestMelodyLayoutKeepsCollidingLyrics(t *testing.T) {
	a := &Analysis{Beats: []float64{0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4}, Downbeats: []float64{0, 2, 4}}
	a.Meter.BeatsPerBar = 4
	a.Melody = &Melody{Notes: []MelodyNote{{Start: 0, End: 0.1, Pitch: 60, Lyric: "to"}, {Start: 0.1, End: 0.25, Pitch: 62, Lyric: "know"},
		{Start: 0.5, End: 0.6, Pitch: 64, Lyric: "ま"}, {Start: 0.6, End: 1, Pitch: 65, Lyric: "狂"}}}
	var got []string
	for _, mm := range a.MelodyLayout(a.Measures(), 0, false) {
		for _, it := range mm.Items {
			if it.Lyric != "" {
				got = append(got, it.Lyric)
			}
		}
	}
	if len(got) != 2 || got[0] != "to know" || got[1] != "ま狂" {
		t.Errorf("lyrics %q", got)
	}
	if JoinLyric("I", "want") != "I want" || JoinLyric("い", "ま") != "いま" || JoinLyric("so", "い") != "soい" {
		t.Error("JoinLyric spacing")
	}
}
