package main

import (
	"context"
	"encoding/binary"
	"fmt"
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

// 音乐分析：智能模式只有歌曲与「分离伴奏」；专业模式出现全部参数。不分离时直接提交 score.analyze。
func TestScorePage(t *testing.T) {
	l := paths.New(t.TempDir())
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be = backend{layout: l, cond: cond}
	a.current = "score"
	tt := ui.NewTester(a.view, 1200, 2400)
	if !tt.HasText("下载缺少的模型（约 1131 MB）") || tt.HasText("和弦词汇") {
		t.Errorf("smart texts %q", tt.Texts())
	}
	if err := tt.Click("专业"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"节拍算法", "调性轮廓", "和弦词汇", "最短和弦（拍）", "和弦平滑", "分离模型"} {
		if !tt.HasText(label) {
			t.Errorf("pro missing %q", label)
		}
	}
	// 默认识别旋律：先提交分轨（干声 → 去和声）。
	a.score.input, a.score.separate, a.score.vocabTitle = "/music/song.flac", false, "含七和弦、减三、挂四"
	tt.Frame()
	if err := tt.Click("开始分析"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cond.Jobs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if jobs := cond.Jobs(); len(jobs) != 1 || jobs[0].Task != "stems.chain" || fmt.Sprint(jobs[0].Params["steps"]) != "[vocals karaoke]" {
		t.Fatalf("melody separation jobs %+v", jobs)
	}
	a.score.runs[0].Cancel()
	for a.score.runs[0].Snapshot().Running() {
		time.Sleep(5 * time.Millisecond)
	}
	cond.Cancel(cond.Jobs()[0].ID)

	// 不分离、不识别旋律：直接提交分析。
	a.score.melody = false
	tt.Frame()
	if err := tt.Click("开始分析"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(cond.Jobs()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	jobs := cond.Jobs()[1:]
	if len(jobs) != 1 || jobs[0].Task != "score.analyze" || jobs[0].Params["harmonic_input"] != nil ||
		jobs[0].Params["options"].(map[string]any)["chord_vocab"] != "extended" {
		t.Fatalf("jobs %+v, err %q", jobs, a.score.err)
	}
	a.score.runs[0].Cancel()
}

// fileJobs 让分析流水线直接返回给定的 analysis.json。
type fileJobs struct{ path string }

func (f fileJobs) Submit(string, string, map[string]any) (string, error) { return "j", nil }
func (f fileJobs) Wait(context.Context, string) (conductor.Job, error) {
	return conductor.Job{State: conductor.JobDone, Artifacts: []string{f.path}}, nil
}
func (fileJobs) Cancel(string) {}

// 乐谱：爵士谱按拍放置和弦记号与反复记号，级数记法相对调号；五线谱渲染谱号与和弦记号；和弦伴奏在后台合成。
func TestScoreSheet(t *testing.T) {
	js := filepath.Join(t.TempDir(), "analysis.json")
	os.WriteFile(js, []byte(`{"duration": 8, "beats": [0,0.5,1,1.5,2,2.5,3,3.5,4,4.5,5,5.5,6,6.5,7,7.5],
		"downbeats": [0,2,4,6], "key": {"label": "C 大调", "tonic_pc": 0, "mode": "major"},
		"chords": [{"start":0,"end":1,"label":"C","root":0,"quality":"maj"},{"start":1,"end":2,"label":"Am7","root":9,"quality":"min7"},
		{"start":2,"end":3,"label":"C","root":0,"quality":"maj"},{"start":3,"end":4,"label":"Am7","root":9,"quality":"min7"},
		{"start":4,"end":8,"label":"Fmaj7","root":5,"quality":"maj7"}]}`), 0o644)
	l := paths.New(t.TempDir())
	cond, _ := conductor.New(l, engines.FS, nil)
	a := newApp()
	a.be, a.current = backend{layout: l, cond: cond}, "score"
	run, err := score.Start(fileJobs{js}, score.Request{Input: "/x/歌.flac", Options: score.DefaultOptions()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for run.Snapshot().Running() {
		time.Sleep(5 * time.Millisecond)
	}
	a.score.runs = []*score.Run{run}
	tt := ui.NewTester(a.view, 1200, 6000)
	for _, want := range []string{"C", "A-7", "%", "FΔ7", "/"} {
		if !tt.HasText(want) {
			t.Errorf("jazz sheet missing %q", want)
		}
	}
	a.score.sheet.style = symbolStyles[2].Title
	tt.Frame()
	if !tt.HasText("vi7") || !tt.HasText("IVΔ7") {
		t.Errorf("roman texts %q", tt.Texts())
	}
	if err := tt.Click("五线谱"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("𝄞") || !tt.HasText("𝄢") {
		t.Errorf("staff texts %q", tt.Texts())
	}
}

// writeTestWav 写一段静音的 16 bit 立体声 WAV。
func writeTestWav(t *testing.T, path string, rate, frames int) {
	t.Helper()
	data := make([]byte, 44+frames*4)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+frames*4))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 2)
	binary.LittleEndian.PutUint32(data[24:], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(rate*4))
	binary.LittleEndian.PutUint16(data[32:], 4)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(frames*4))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// artifactJobs 让分析流水线直接返回给定的产物。
type artifactJobs struct{ arts []string }

func (f artifactJobs) Submit(string, string, map[string]any) (string, error) { return "j", nil }
func (f artifactJobs) Wait(context.Context, string) (conductor.Job, error) {
	return conductor.Job{State: conductor.JobDone, Artifacts: f.arts}, nil
}
func (artifactJobs) Cancel(string) {}

// 分轨混音：独奏旋律时合成钢琴并切到混音结果；取消独奏后回到只播放原曲（原文件的播放器）。
func TestScoreTrackMixer(t *testing.T) {
	dir := t.TempDir()
	wav, js := filepath.Join(dir, "audio.wav"), filepath.Join(dir, "analysis.json")
	writeTestWav(t, wav, 8000, 16000)
	os.WriteFile(js, []byte(`{"duration": 2, "beats": [0,0.5,1,1.5], "downbeats": [0],
		"key": {"label": "C 大调", "tonic_pc": 0, "mode": "major"},
		"chords": [{"start":0,"end":2,"label":"C","root":0,"quality":"maj"}],
		"melody": {"notes": [{"start":0,"end":0.5,"pitch":64},{"start":0.5,"end":1,"pitch":67}], "source": "lead_vocal"}}`), 0o644)
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
	pv := &a.score.preview
	drain := func(until func() bool) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for !until() {
			select {
			case fn := <-updates:
				fn()
				tt.Frame()
			case <-deadline:
				t.Fatal("timed out")
			}
		}
	}
	drain(func() bool { return pv.player != nil })
	if !tt.HasText("钢琴 · 旋律") || tt.HasText("未识别旋律（分析时勾选「识别旋律线」）") {
		t.Fatalf("texts %q", tt.Texts())
	}
	a.score.sheet.mix.Solo = [score.TrackCount]bool{false, false, true}
	a.applyMix(run.Snapshot().Result)
	drain(func() bool { return pv.player != pv.original && a.score.sheet.melodyPCM != nil })
	a.score.sheet.mix.Solo = [score.TrackCount]bool{}
	a.applyMix(run.Snapshot().Result)
	if pv.player != pv.original {
		t.Error("original-only mix should play the original player")
	}
}
