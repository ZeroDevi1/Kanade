package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/mic"
	"kanade/internal/paths"
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

// 播放快捷键：空格播放 / 暂停，← / → 后退 / 前进，Home 回到开头，L 循环，M 静音（作用于界面上的播放器）。
func TestPlaybackShortcuts(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt := ui.NewTester(a.view, 1200, 1800)
	tt.Frame()
	p := s.player
	if p == nil || shortcutTarget() != p {
		t.Fatalf("target %p, player %p", shortcutTarget(), p)
	}
	tt.Key(0, ui.KeySpace)
	tt.Frame()
	if !p.Playing() {
		t.Fatal("space should play")
	}
	tt.Key(0, ui.KeySpace)
	tt.Frame()
	if p.Playing() {
		t.Fatal("space should pause")
	}
	p.Seek(time.Second)
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	if got := p.Position(); got < 4*time.Second {
		t.Errorf("→ moved to %v", got) // 1 秒 + 5 秒，超过时长时停在末尾
	}
	before := p.Position()
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	// 播放器的位置在 Seek 之后可能有几毫秒的漂移（混音器提前解码），按 50 ms 的误差比较。
	if want := max(before-transportSkip, 0); (p.Position() - want).Abs() > 50*time.Millisecond {
		t.Errorf("← moved from %v to %v, want %v", before, p.Position(), want)
	}
	tt.Key(0, ui.KeyHome)
	tt.Frame()
	if p.Position() > 50*time.Millisecond {
		t.Errorf("Home: %v", p.Position())
	}
	tt.Key(0, ui.KeyL)
	tt.Key(0, ui.KeyM)
	tt.Frame()
	if !p.Loop() || p.Volume() != 0 {
		t.Errorf("L/M: loop %v volume %v", p.Loop(), p.Volume())
	}
	tt.Key(0, ui.KeyM)
	tt.Frame()
	if p.Volume() != 1 {
		t.Errorf("unmute: volume %v", p.Volume())
	}
}

// 编辑：拖动改音高、双击空白处添加音符、一分为二，撤销 / 重做（按钮与 Ctrl+Z / Ctrl+Y）。
func TestInspireEditing(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt := ui.NewTester(a.view, 1200, 2400)
	tt.Frame()
	r, ok := tt.Find("乐句卷帘")
	if !ok {
		t.Fatal("roll not found")
	}
	// 纵轴范围：音高 60–67 上下各留 3 个半音并扩到一个八度半，为 55–73。
	lo, hi := 55.0, 73.0
	rowH := (r.H - rollChordH) / float32(hi-lo+1)
	length := s.set().Length()
	xAt := func(sec float64) float32 { return r.X + rollKeysW + float32(sec/length)*(r.W-rollKeysW) }
	yAt := func(m float64) float32 { return r.Y + rollChordH + float32(hi-m)*rowH }
	// 拖动第 5 个音（2–3 秒，F4）向上两个半音。
	x, y := xAt(2.5), yAt(65)
	tt.Press(x, y)
	tt.Frame()
	tt.Move(x, y-2*rowH)
	tt.Frame()
	tt.Release(x, y-2*rowH)
	tt.Frame()
	if n := s.hum.Notes[4]; n.Pitch != 67 || n.Start != 2 || len(s.undo) != 1 {
		t.Fatalf("drag: %+v undo %d", n, len(s.undo))
	}
	// 撤销与重做。
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if s.hum.Notes[4].Pitch != 65 || len(s.redo) != 1 {
		t.Fatalf("undo: %+v", s.hum.Notes[4])
	}
	tt.Key(ui.Cmd, ui.KeyY)
	tt.Frame()
	if s.hum.Notes[4].Pitch != 67 {
		t.Fatalf("redo: %+v", s.hum.Notes[4])
	}
	// 删掉最后一个音，再在空出来的位置添加音符。
	s.sel = len(s.hum.Notes) - 1
	tt.Key(0, ui.KeyDelete)
	tt.Frame()
	if len(s.hum.Notes) != 5 {
		t.Fatalf("delete: %d notes", len(s.hum.Notes))
	}
	// 3.05 秒向下取整到十六分音符网格（120 BPM 时 0.125 秒）为 3 秒，时值一拍。
	if i := s.addNote(3.05, 64); i != 5 || s.hum.Notes[5].Start != 3 || s.hum.Notes[5].End != 3.5 {
		t.Fatalf("add: %d %+v", i, s.hum.Notes)
	}
	if s.addNote(3.1, 62) >= 0 {
		t.Error("added a note inside another one")
	}
	// 一分为二：0.5–1 秒的音拆成两个八分音符，后一个是新的字。
	if !s.splitNote(2) || s.hum.Notes[2].End != 0.75 || s.hum.Notes[3].Start != 0.75 || s.hum.Notes[3].Slur {
		t.Fatalf("split: %+v", s.hum.Notes)
	}
	// 第 1 个音是八分音符：拆成两个十六分音符后，十六分音符不能再拆。
	if !s.splitNote(0) || s.splitNote(0) {
		t.Error("split an eighth into sixteenths, then refuse to split a sixteenth")
	}
}

// 和弦锁定：锁定选中的和弦，替换成别的和弦，其余重新配；解除全部锁定。
func TestInspireHarmonyLock(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt := ui.NewTester(a.view, 1200, 2400)
	s.chordSel = 0
	tt.Frame()
	if err := tt.Click("锁定此和弦"); err != nil || len(s.locks) != 1 {
		t.Fatalf("lock: %v %+v", err, s.locks)
	}
	first := s.harmonies[0].Chords[0]
	if !strings.HasSuffix(s.harmonies[0].Reasons[0], "（已锁定）") {
		t.Errorf("reason %q", s.harmonies[0].Reasons[0])
	}
	// 替换为 A-7。
	s.chordSel = 0
	s.replaceRoot, s.replaceQuality = "A", "-7"
	tt.Frame()
	if err := tt.Click("替换并锁定"); err != nil {
		t.Fatal(err)
	}
	if c := s.harmonies[0].Chords[0]; *c.Root != 9 || c.Quality != "min7" || len(s.locks) != 1 {
		t.Fatalf("replace: %+v (was %+v), locks %+v", c, first, s.locks)
	}
	if err := tt.Click("解除全部锁定（1）"); err != nil || len(s.locks) != 0 {
		t.Fatalf("unlock all: %v", err)
	}
}

// 动机发展：生成乐段，锁定一格后重新生成其余格；换拟声字组不作废方案；改动动机后方案作废。
func TestInspireDevelop(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt := ui.NewTester(a.view, 1200, 3000)
	if err := tt.Click("生成乐段"); err != nil || len(s.dev.periods) == 0 || len(s.dev.harmony) == 0 {
		t.Fatalf("develop: %v %d", err, len(s.dev.periods))
	}
	tt.Frame()
	if s.dev.player == nil || !hasPrefix(tt.Texts(), "方案 1 · ") {
		t.Fatalf("develop card: %q", tt.Texts())
	}
	p := *s.dev.period()
	s.dev.locked = map[int]bool{1: true}
	tt.Frame()
	if err := tt.Click("重新生成其余段落（锁定 1 格）"); err != nil {
		t.Fatal(err)
	}
	for _, q := range s.dev.periods {
		if q.Form != p.Form || q.Slots[1].Label != p.Slots[1].Label || !strings.HasSuffix(q.Slots[1].Desc, "（已锁定）") {
			t.Fatalf("locked slot not kept: %+v", q.Slots[1])
		}
	}
	// 换拟声字组：方案保留，拟声字重填。
	if err := tt.Click("呜（抒情）"); err != nil || len(s.dev.periods) == 0 {
		t.Fatalf("filler: %v", err)
	}
	if n := s.dev.period().Hum.Notes[0]; n.Lyric == "啦" || n.Lyric == "" {
		t.Errorf("period lyrics not refilled: %q", n.Lyric)
	}
	// 改动动机：方案作废。
	s.edit(func() { s.hum.Notes[0].Pitch++ })
	if len(s.dev.periods) != 0 {
		t.Error("periods kept after editing the motif")
	}
}

// 灵感记录：整理后自动保存（录音帧、音符、各项选择与录音），重新进入页面可从列表打开，删除需要确认。
func TestInspireHistory(t *testing.T) {
	l := paths.New(t.TempDir())
	a := newApp()
	a.be, a.current = backend{layout: l}, "inspire"
	s := &a.inspire
	s.recSet = score.HumSettings{BPM: 120, BeatsPerBar: 4, Bars: 2, CountIn: 1}
	s.frames = []score.KaraokeFrame{{T: 2, F0: 262, Clarity: 0.9, DB: -20}}
	s.recording = make([]float32, mic.Rate)
	s.recording[100] = 0.5
	s.hum = testInspireHum()
	s.style = score.StyleJazz
	s.changed()
	tt := ui.NewTester(a.view, 1200, 2400)
	tt.Frame()
	dir := s.history.dir
	if dir == "" || s.dirty {
		t.Fatalf("not saved: %q dirty %v err %q", dir, s.dirty, s.err)
	}
	// 编辑后写回同一条记录。
	s.edit(func() { s.hum.Notes[0].Pitch = 72 })
	tt.Frame()
	if s.history.dir != dir || len(s.history.items) != 1 {
		t.Fatalf("items %+v", s.history.items)
	}
	// 新的应用实例：列表里有这条记录，选择即打开。
	b := newApp()
	b.be, b.current = backend{layout: l}, "inspire"
	tb := ui.NewTester(b.view, 1200, 2400)
	title := a.inspire.history.items[0].title
	if !tb.HasText("灵感记录") {
		t.Fatalf("history row: %q", tb.Texts())
	}
	b.inspire.history.sel = ""
	b.openInspireRecord(b.inspire.history.items[0].dir)
	bs := &b.inspire
	if bs.hum == nil || bs.hum.Notes[0].Pitch != 72 || bs.style != score.StyleJazz || len(bs.frames) != 1 ||
		len(bs.recording) != mic.Rate || bs.recording[100] < 0.49 || bs.history.dir != dir {
		t.Fatalf("opened %+v", bs.hum)
	}
	tb.Frame()
	if bs.history.sel != title {
		t.Errorf("selected %q, want %q", bs.history.sel, title)
	}
	// 删除：第一次点只进入确认状态。
	if err := tb.Click("删除记录"); err != nil || !bs.history.confirmDelete {
		t.Fatalf("delete: %v", err)
	}
	if err := tb.Click("确认删除？"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || bs.hum != nil || len(bs.history.items) != 0 {
		t.Fatalf("after delete: %v %+v", err, bs.history.items)
	}
}

// 6/8：节拍器模式选 6/8 得到复合拍设置；自由模式的拍号调整选 6/8 时整理按复合拍；概况显示 ♩. 与 6/8。
func TestInspireCompoundMeter(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.meter = 3
	if set := s.metronomeSettings(); !set.Compound || set.BeatsPerBar != 2 || set.MeterLabel() != "6/8" {
		t.Fatalf("metronome 6/8: %+v", set)
	}
	s.recSet, s.meterPick = score.HumSettings{Free: true}, 4
	if set := s.transcribeSettings(); !set.Compound || set.BeatsPerBar != 2 {
		t.Fatalf("free 6/8: %+v", set)
	}
	h := testInspireHum()
	h.Settings = score.HumSettings{BPM: 60, BeatsPerBar: 2, Bars: 2, Compound: true}
	s.hum = h
	s.changed()
	tt := ui.NewTester(a.view, 1200, 2400)
	if !tt.HasText("♩. = 60 · 6/8 · 2 小节") {
		t.Fatalf("info: %q", tt.Texts())
	}
}

// 乐谱视图与逐格编辑：乐句与乐段都能切换到五线谱、爵士谱；改一格的模进后该格自动锁定、说明更新。
func TestInspireSheetAndSlotEdit(t *testing.T) {
	a := newApp()
	a.current = "inspire"
	s := &a.inspire
	s.hum = testInspireHum()
	s.changed()
	tt := ui.NewTester(a.view, 1200, 4000)
	for _, v := range []string{"五线谱", "爵士谱"} {
		s.view = slices.Index(inspireViews, v)
		tt.Frame()
		if _, ok := tt.Find("乐句卷帘"); ok {
			t.Errorf("%s: roll still shown", v)
		}
	}
	if err := tt.Click("生成乐段"); err != nil || len(s.dev.periods) == 0 {
		t.Fatalf("develop: %v", err)
	}
	s.dev.view = inspireViewStaff
	s.dev.selected = 1
	tt.Frame()
	for _, label := range []string{"素材", "裂解", "倒影", "模进级数", "节奏", "终止", "和声区域"} {
		if _, ok := tt.Find(label); !ok {
			t.Errorf("slot editor has no %q control", label)
		}
	}
	// 直接走编辑的路径（下拉框的交互由 mygo 负责）：把第 2 格改为上行模进 2 级。
	p := *s.dev.period()
	sp := p.Slots[1].Spec
	sp.Seq = 2
	s.dev.periods[s.dev.pick] = score.EditSlot(*s.hum, p, 1, sp, s.filler)
	s.dev.locked = map[int]bool{1: true}
	s.devHarmonize()
	tt.Frame()
	if q := s.dev.period(); q.Slots[1].Spec.Seq != 2 || !strings.Contains(q.Slots[1].Desc, "上行模进 2 级") || len(s.dev.harmony) == 0 {
		t.Fatalf("edited slot %+v", q.Slots[1])
	}
}
