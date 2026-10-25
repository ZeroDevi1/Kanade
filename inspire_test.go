package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/mic"
	"kanade/internal/score"
)

// testHum 是一段整理好的 C 大调乐句（节拍器模式，120 BPM，4/4，2 小节）：第 1 个字唱两个音（转音）。
func testInspireHum() *score.Hum {
	set := score.HumSettings{BPM: 120, BeatsPerBar: 4, Bars: 2, CountIn: 1}
	return &score.Hum{Settings: set, TonicPC: 0, Mode: "major", KeyClear: true, Tuning: 30, Voiced: 100,
		Notes: []score.MelodyNote{{Start: 0, End: 0.25, Pitch: 64}, {Start: 0.25, End: 0.5, Pitch: 62, Slur: true},
			{Start: 0.5, End: 1, Pitch: 60}, {Start: 1, End: 2, Pitch: 67}, {Start: 2, End: 3, Pitch: 65},
			{Start: 3, End: 4, Pitch: 60}}}
}

// 灵感页：录音需要真实的麦克风，这里直接放入整理好的乐句，检查概况、拟声字、编辑、和声与试听。
func TestInspirePage(t *testing.T) {
	a := newApp()
	tt := ui.NewTester(a.view, 1200, 1800)
	if err := tt.Click("灵感"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("● 录音") || !tt.HasText("还没有录音。") || !tt.HasText("自由哼唱") {
		t.Fatalf("empty page: %q", tt.Texts())
	}
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt.Frame()
	for _, want := range []string{"C 大调", "♩ = 120 · 4/4 · 2 小节", "6 个音、5 个字（1 个转音）", "整体偏高 30 音分（已校正）",
		"啦", "导出 MIDI…", "播放进度", "和声", "方案 1 · "} {
		if !hasPrefix(tt.Texts(), want) {
			t.Errorf("missing %q: %q", want, tt.Texts())
		}
	}
	if s.player == nil || len(s.harmonies) == 0 {
		t.Fatal("no preview player or harmony")
	}
	// 点和弦看理由。
	first := score.Symbol(s.harmonies[0].Chords[0], score.SymbolJazz, 0)
	if err := tt.Click(first); err != nil || s.chordSel != 0 || !tt.HasText(s.harmonies[0].Reasons[0]) {
		t.Fatalf("chord reason: %v %q", err, tt.Texts())
	}
	// 换风格：重新配和声。
	if err := tt.Click("爵士"); err != nil || s.style != score.StyleJazz || len(s.harmonies) == 0 {
		t.Fatalf("style: %v", err)
	}
	// 选中第 3 个音并升半音；拟声字随之重填，和声与试听重新生成。
	s.sel = 2
	tt.Frame()
	built := s.builtVersion
	if err := tt.Click("升半音"); err != nil || s.hum.Notes[2].Pitch != 61 {
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
	// 含原始哼唱的试听以录音时间为轴：乐谱时间与播放器位置可往返换算。
	s.recording = make([]float32, 5*mic.Rate)
	s.source = inspireBoth
	s.playerVersion++
	tt.Frame()
	if !s.voiceTime {
		t.Fatal("voice timeline")
	}
	s.seekScore(1.5)
	if got := s.scoreTime(s.player.Position().Seconds()); got < 1.49 || got > 1.51 {
		t.Errorf("score time after seek %.3f", got)
	}
}

// 录音中点「停止」：停止录音并整理（曾因同一帧还读取已释放的节拍器而崩溃）。真实录音需要麦克风，这里放入进行中的会话。
func TestInspireStopDuringRecording(t *testing.T) {
	for _, free := range []bool{false, true} {
		a := newApp()
		a.current = "inspire"
		s := &a.inspire
		s.free = free
		if free {
			s.recSet = score.HumSettings{Free: true}
		} else {
			s.recSet = s.metronomeSettings()
			s.clickPlayer = audio.NewSound(inspireRate, 1, score.MetronomePCM(s.recSet, inspireRate)).NewPlayer()
			s.clickPlayer.Play()
		}
		s.capture, s.sess, s.recStart = &mic.Capture{}, score.NewKaraokeSession(mic.Rate), time.Now()
		tt := ui.NewTester(a.view, 1200, 1400)
		if !tt.HasText("停止") {
			t.Fatalf("recording view: %q", tt.Texts())
		}
		if err := tt.Click("停止"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if s.recordingNow() || s.hum == nil || !tt.HasText("没有录到哼唱：请检查录音设备是否选对、麦克风音量是否足够。") {
			t.Fatalf("free=%v after stop: %q", free, tt.Texts())
		}
	}
}

// 自由模式的节奏调整：速度减半后用同一段录音重新整理，速度提示随之写入。
func TestInspireRhythmFix(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.recSet = score.HumSettings{Free: true}
	h := testInspireHum()
	h.Settings = score.HumSettings{Free: true, BPM: 120, BeatsPerBar: 4, Bars: 2}
	s.hum = h
	s.changed()
	tt := ui.NewTester(a.view, 1200, 1800)
	if !tt.HasText("♩ = 120 · 4/4 · 2 小节（自动识别）") {
		t.Fatalf("free info: %q", tt.Texts())
	}
	if err := tt.Click("速度减半"); err != nil || s.tempoHint != 60 || s.transcribeSettings().BPM != 60 {
		t.Fatalf("halve: %v hint %v", err, s.tempoHint)
	}
	if !strings.Contains(strings.Join(tt.Texts(), "|"), "没有录到哼唱") {
		t.Errorf("re-transcribed from the (empty) recording expected: %q", tt.Texts())
	}
}

// hasPrefix 报告 texts 中是否有以 prefix 开头的文本（方案按钮带有评分，事先不知道）。
func hasPrefix(texts []string, prefix string) bool {
	for _, x := range texts {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}
