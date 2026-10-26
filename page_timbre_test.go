package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/timbre"
)

func newTimbreTestApp(t *testing.T) (*app, *conductor.Conductor, *ui.Tester) {
	t.Helper()
	l := paths.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "voice.pth")
	os.WriteFile(src, []byte("x"), 0o644)
	if _, err := timbre.Import(l, "炫狗", src, ""); err != nil {
		t.Fatal(err)
	}
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be = backend{layout: l, cond: cond}
	a.current = "timbre"
	return a, cond, ui.NewTester(a.view, 1200, 2400)
}

// 翻唱：智能模式只有人声音量与 MP3；专业模式出现分轨模型、和声与混响参数；纯干声跳过分轨，直接提交转换。
func TestTimbreCoverModes(t *testing.T) {
	a, cond, tt := newTimbreTestApp(t)
	if err := tt.Click("翻唱"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("人声音量") || tt.HasText("湿声电平") {
		t.Errorf("smart cover texts %q", tt.Texts())
	}
	if err := tt.Click("专业"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("湿声电平") || !tt.HasText("检索比例") || !tt.HasText("分轨模型") {
		t.Errorf("pro cover texts %q", tt.Texts())
	}
	if err := tt.Click("纯干声"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("伴奏（可选）") || tt.HasText("分轨模型") {
		t.Errorf("dry cover texts %q", tt.Texts())
	}
	a.timbre.vocal = "/in/dry.wav"
	tt.Frame()
	if err := tt.Click("开始翻唱"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cond.Jobs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 || jobs[0].Task != "timbre.infer" || jobs[0].Params["input"] != "/in/dry.wav" || jobs[0].Params["normalize"] != false {
		t.Fatalf("jobs %+v, err %q", jobs, a.timbre.err)
	}
	a.timbre.cover.Cancel()
}

// 训练：智能模式不显示数值参数并提交 0（自动）；专业模式的数值框带可见标签。
func TestTimbreTrainModes(t *testing.T) {
	a, cond, tt := newTimbreTestApp(t)
	if err := tt.Click("训练"); err != nil {
		t.Fatal(err)
	}
	if tt.HasText("总轮数") {
		t.Error("smart training shows epochs")
	}
	a.timbre.trainName, a.timbre.trainDataset = "新模型", t.TempDir()
	tt.Frame()
	if err := tt.Click("开始训练"); err != nil {
		t.Fatal(err)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 || jobs[0].Params["epochs"] != 0 || jobs[0].Params["batch_size"] != 0 {
		t.Fatalf("jobs %+v, err %q", jobs, a.timbre.err)
	}
	if err := tt.Click("专业"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"总轮数", "batch", "每 N 轮保存", "采样率", "切片方式", "数据缓存到显存", "训练精度"} {
		if !tt.HasText(label) {
			t.Errorf("pro training missing visible label %q", label)
		}
	}
	// 专业模式默认与智能模式相同：缓存自动、fp16；选「开」后提交 true。
	if r := a.timbre.trainRequest(); r.CacheGPU != "auto" || r.Precision != "fp16" {
		t.Errorf("pro defaults %+v", r)
	}
	a.timbre.trainCacheGPU = trainCacheTitles[1]
	if p, _ := a.timbre.trainRequest().Params(); p["cache_gpu"] != true {
		t.Errorf("cache on: %v", p["cache_gpu"])
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	if got := uniquePath(dir, "a.wav"); got != filepath.Join(dir, "a.wav") {
		t.Errorf("free name %s", got)
	}
	os.WriteFile(filepath.Join(dir, "a.wav"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "a (2).wav"), nil, 0o644)
	if got := uniquePath(dir, "a.wav"); got != filepath.Join(dir, "a (3).wav") {
		t.Errorf("taken name %s", got)
	}
	if safeFileName(` a/b:c? `) != "a_b_c_" {
		t.Errorf("safe name %q", safeFileName(` a/b:c? `))
	}
	src := filepath.Join(dir, "src.wav")
	os.WriteFile(src, []byte("data"), 0o644)
	dst := filepath.Join(dir, "a.wav")
	if err := copyFileAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "data" {
		t.Errorf("copied %q", b)
	}
}
