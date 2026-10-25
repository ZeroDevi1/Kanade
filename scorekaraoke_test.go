package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/score"
)

// K 歌：有旋律线时显示卡片（没有分离伴奏时播放原曲）；评价结果显示总分、各项与需要多练的乐句。
// 录音需要真实的麦克风，这里用合成的帧代替演唱，直接评价。
func TestScoreKaraokeCard(t *testing.T) {
	dir := t.TempDir()
	wav, js := filepath.Join(dir, "audio.wav"), filepath.Join(dir, "analysis.json")
	writeTestWav(t, wav, 8000, 48000)
	os.WriteFile(js, []byte(`{"duration": 6, "beats": [0,0.5,1,1.5], "downbeats": [0],
		"key": {"label": "C 大调", "tonic_pc": 0, "mode": "major"},
		"chords": [{"start":0,"end":6,"label":"C","root":0,"quality":"maj"}],
		"melody": {"notes": [{"start":1,"end":1.5,"pitch":64,"lyric":"我"},{"start":1.5,"end":2.5,"pitch":67,"lyric":"不"},
			{"start":3,"end":3.5,"pitch":69,"lyric":"想"},{"start":3.5,"end":4.5,"pitch":67,"lyric":"念"}], "source": "lead_vocal"}}`), 0o644)
	l := paths.New(t.TempDir())
	cond, _ := conductor.New(l, engines.FS, nil)
	a := newApp()
	a.be, a.current = backend{layout: l, cond: cond}, "score"
	updates := make(chan func(), 64)
	a.post = func(fn func()) { updates <- fn }
	run, _ := score.Start(artifactJobs{[]string{wav, js}}, score.Request{Input: "/x/歌.flac", Options: score.DefaultOptions()}, nil)
	for run.Snapshot().Running() {
		time.Sleep(5 * time.Millisecond)
	}
	a.score.runs = []*score.Run{run}
	tt := ui.NewTester(a.view, 1200, 4000)
	// 等原曲解码完（没有伴奏时 K 歌直接用试听的原文件播放器）。
	for a.score.karaoke.player() == nil {
		(<-updates)()
		tt.Frame()
	}
	if !tt.HasText("K 歌跟唱") || !tt.HasText("从头开始跟唱") || !tt.HasText("没有分离出的伴奏（分析时未分离），播放原曲。") {
		t.Fatalf("karaoke card: %q", tt.Texts())
	}
	if !tt.HasText("我不") || !tt.HasText("想念") {
		t.Errorf("lyric lines: %q", tt.Texts())
	}

	// 合成的演唱：第一句唱准、晚 0.15 秒；第二句整体偏高 80 音分。
	res := run.Snapshot().Result
	var frames []score.KaraokeFrame
	for ti := 0.0; ti < 5; ti += 0.01 {
		for _, n := range res.Melody.Notes {
			if ti >= n.Start && ti < n.End {
				cents := 0.0
				if n.Start >= 3 {
					cents = 80
				}
				f0 := 440 * math.Pow(2, (n.Pitch-69)/12+cents/1200)
				frames = append(frames, score.KaraokeFrame{T: ti + 0.15, F0: f0, Clarity: 0.95, DB: -20})
			}
		}
		frames = append(frames, score.KaraokeFrame{T: ti + 0.15, F0: 0, Clarity: 0.1, DB: -70})
	}
	r := score.AnalyzeKaraoke(res.Melody.Notes, frames, 0, 5)
	a.score.karaoke.report = &r
	tt.Frame()
	for _, want := range []string{"总分", "音准", "唱满", "气息", "需要多练的乐句", "已扣除 150 ms 的整体延迟（耳机与声卡的延迟加上跟唱的反应）。"} {
		if !tt.HasText(want) {
			t.Errorf("report missing %q: %q", want, tt.Texts())
		}
	}
	// 只列出没唱好的乐句：偏高的第二句在列，唱准的第一句不在列。
	if len(r.Phrases) != 2 || r.Phrases[1].Score >= karaokeGoodPhrase || r.Phrases[0].Score < karaokeGoodPhrase {
		t.Fatalf("phrases %+v", r.Phrases)
	}
	if !tt.HasText("想念") || tt.HasText("我不") {
		t.Errorf("practice list: %q", tt.Texts())
	}
	if err := tt.Click("清除结果"); err != nil || a.score.karaoke.report != nil {
		t.Errorf("clear: %v", err)
	}
}

func TestKaraokeLines(t *testing.T) {
	notes := []score.MelodyNote{{Start: 1, End: 1.5, Lyric: "我"}, {Start: 1.5, End: 2, Lyric: "不"}, {Start: 2.1, End: 2.4, Slur: true},
		{Start: 3, End: 3.5, Lyric: "想"}, {Start: 4, End: 4.5}}
	for _, tc := range []struct {
		pos       float64
		cur, next string
	}{{0, "我不", "想"}, {2.5, "想", "♪"}, {3.9, "♪", ""}, {5, "（结束）", ""}} {
		if cur, next := karaokeLines(notes, tc.pos); cur != tc.cur || next != tc.next {
			t.Errorf("pos %v: %q / %q", tc.pos, cur, next)
		}
	}
}
