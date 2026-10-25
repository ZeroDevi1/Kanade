package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
)

// 灵感页：录音需要真实的麦克风，这里直接放入整理好的乐句，检查概况、拟声字、编辑与试听。
func TestInspirePage(t *testing.T) {
	a := newApp()
	tt := ui.NewTester(a.view, 1200, 1400)
	if err := tt.Click("灵感"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("● 录音") || !tt.HasText("还没有录音。") {
		t.Fatalf("empty page: %q", tt.Texts())
	}
	s := &a.inspire
	s.set = score.HumSettings{BPM: 120, BeatsPerBar: 4, Bars: 2, CountIn: 1}
	s.hum = &score.Hum{Settings: s.set, TonicPC: 0, Mode: "major", KeyClear: true, Tuning: 30, Shift: -0.03, Voiced: 100,
		Notes: []score.MelodyNote{{Start: 0, End: 0.25, Pitch: 60}, {Start: 0.25, End: 0.5, Pitch: 62, Slur: true},
			{Start: 0.5, End: 1, Pitch: 64}, {Start: 2, End: 3, Pitch: 67}}}
	s.changed()
	tt.Frame()
	for _, want := range []string{"C 大调", "4 个音、3 个字（1 个转音）", "整体偏高 30 音分（已校正）", "整体抢拍 30 ms（已校正）",
		"啦", "导出 MIDI…", "播放进度"} {
		if !tt.HasText(want) {
			t.Errorf("missing %q: %q", want, tt.Texts())
		}
	}
	if s.player == nil {
		t.Fatal("no preview player")
	}
	// 选中第 3 个音并升半音；拟声字随之重填，试听重新合成。
	s.sel = 2
	tt.Frame()
	built := s.builtVersion
	if err := tt.Click("升半音"); err != nil || s.hum.Notes[2].Pitch != 65 {
		t.Fatalf("raise: %v %+v", err, s.hum.Notes[2])
	}
	tt.Frame()
	if s.builtVersion == built {
		t.Error("preview not rebuilt after edit")
	}
	// 把第 2 个音（转音）拆成新的字。
	s.sel = 1
	tt.Frame()
	if err := tt.Click("拆成新的字"); err != nil || s.hum.Notes[1].Slur || s.hum.Notes[1].Lyric == "" {
		t.Fatalf("split: %v %+v", err, s.hum.Notes[1])
	}
}
