package score

import "testing"

func TestMelodyLayout(t *testing.T) {
	// 两小节 4/4，每拍 0.5 秒。
	a := &Analysis{Duration: 4, Downbeats: []float64{0, 2}}
	for i := 0; i < 8; i++ {
		a.Beats = append(a.Beats, float64(i)*0.5)
	}
	a.Melody = &Melody{Notes: []MelodyNote{
		{0.0, 0.5, 60.2},  // 第 1 拍：四分音符 C4（音高四舍五入）
		{0.5, 0.74, 61.9}, // 第 2 拍前半：八分音符 D4
		// 第 2 拍后半空：八分休止
		{1.0, 2.5, 63.0}, // 第 3 拍起 3 拍，跨小节：二分音符连到下一小节的四分音符
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
