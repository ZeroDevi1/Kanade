package main

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/singing"
)

func newSingingTestApp(t *testing.T) (*app, *conductor.Conductor) {
	t.Helper()
	// Keep tasks queued: no Python installation, downloads or GPU work in UI tests.
	fs := fstest.MapFS{"yingmusic/engine.json": &fstest.MapFile{Data: []byte(`{"name":"yingmusic","module":"kanade_yingmusic","tasks":["yingmusic.edit","yingmusic.doctor"]}`)}}
	c, err := conductor.New(paths.New(t.TempDir()), fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be.cond = c
	a.current = "singing"
	return a, c
}

func TestSingingNavigationAndReturn(t *testing.T) {
	a := newApp()
	tt := ui.NewTester(a.view, 1200, 780)
	for i := 0; i < 2; i++ {
		if err := tt.Click("歌声改词"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if a.current != "singing" || !tt.HasText("推理后端未启动。") {
			t.Fatalf("page %q, texts %q", a.current, tt.Texts())
		}
		if err := tt.Click("主页"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
	}
}

func TestSingingRequiresReferenceAndTargetLyrics(t *testing.T) {
	a, c := newSingingTestApp(t)
	a.submitSinging(false)
	if len(c.Jobs()) != 0 || a.singing.err == "" {
		t.Fatalf("invalid request submitted: %+v, error %q", c.Jobs(), a.singing.err)
	}
	if a.singing.request.RefText != "" {
		t.Fatal("reference lyrics must never be invented")
	}
}

func TestSingingDoctorSingleFlightCancelRetry(t *testing.T) {
	a, c := newSingingTestApp(t)
	a.singing.request.PythonPath = "/isolated/python"
	a.singing.request.SourceDir = "/upstream"
	a.singing.request.ModelDir = "/snapshot"
	a.submitSinging(true)
	a.submitSinging(true)
	a.submitSinging(false)
	jobs := c.Jobs()
	if len(jobs) != 1 || jobs[0].Task != "yingmusic.doctor" {
		t.Fatalf("duplicate tasks %+v", jobs)
	}
	if jobs[0].Params["python_path"] != "/isolated/python" {
		t.Fatalf("wrong runtime params %+v", jobs[0].Params)
	}
	c.Cancel(a.singing.doctorJob)
	if a.singingActive() {
		t.Fatal("queued cancellation did not release submission guard")
	}
	a.submitSinging(true)
	if len(c.Jobs()) != 2 {
		t.Fatalf("retry tasks %+v", c.Jobs())
	}
}

func TestSingingFieldsSurviveNavigation(t *testing.T) {
	a, _ := newSingingTestApp(t)
	a.singing.request.RefText = "参考原词|第二句"
	a.singing.request.TargetText = "目标新词|第二句"
	tt := ui.NewTester(a.view, 1200, 2400)
	for _, label := range []string{"音色参考原歌词（必填）", "目标新歌词（必填）", "检查本地环境", "生成改词歌声"} {
		if !tt.HasText(label) {
			t.Errorf("missing label %q: %q", label, tt.Texts())
		}
	}
	if err := tt.Click("主页"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("歌声改词"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.singing.request.RefText != "参考原词|第二句" || a.singing.request.TargetText != "目标新词|第二句" {
		t.Fatal("navigation lost lyrics")
	}
}

func TestSingingDoctorResultBelongsToSubmittedRuntime(t *testing.T) {
	r := singing.Request{PythonPath: "/python", SourceDir: "/src", ModelDir: "/model"}
	j := conductor.Job{State: conductor.JobDone, Params: map[string]any{
		"python_path": r.PythonPath, "source_dir": r.SourceDir, "model_dir": r.ModelDir,
	}}
	if !singingRuntimeMatches(j, r) {
		t.Fatal("matching check not recognized")
	}
	for _, change := range []func(*singing.Request){
		func(r *singing.Request) { r.PythonPath += "-new" },
		func(r *singing.Request) { r.SourceDir += "-new" },
		func(r *singing.Request) { r.ModelDir += "-new" },
	} {
		updated := r
		change(&updated)
		if singingRuntimeMatches(j, updated) {
			t.Fatal("stale check reused after runtime change")
		}
	}
	r.TargetText = "different lyrics"
	if !singingRuntimeMatches(j, r) {
		t.Fatal("lyrics should not invalidate runtime check")
	}
}

func TestSingingOptionalInputsCanBeRemoved(t *testing.T) {
	a, _ := newSingingTestApp(t)
	a.singing.request.RefAudio = "/optional-reference.wav"
	a.singing.request.RefText = "旧参考原词"
	a.singing.request.AccompanimentAudio = "/accompaniment.wav"
	tt := ui.NewTester(a.view, 1200, 2400)
	if err := tt.Click("改用旋律干声音色"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.singing.request.RefAudio != "" || a.singing.request.RefText != "" {
		t.Fatal("old reference/transcription retained")
	}
	if err := tt.Click("移除伴奏"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.singing.request.AccompanimentAudio != "" {
		t.Fatal("accompaniment retained")
	}
	tt.Frame()
}

func TestSingingEditSingleFlightAndRetry(t *testing.T) {
	a, c := newSingingTestApp(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "input.wav")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a.singing.request = singing.Request{PythonPath: file, SourceDir: dir, ModelDir: dir, MelodyAudio: file, RefText: "原词", TargetText: "新词"}
	a.submitSinging(false)
	a.submitSinging(false)
	a.submitSinging(true)
	if len(c.Jobs()) != 1 || c.Jobs()[0].Task != singing.Task {
		t.Fatalf("jobs %+v; err %s", c.Jobs(), a.singing.err)
	}
	if c.Jobs()[0].Params["ref_text"] != "原词" {
		t.Fatal("wrong reference transcription")
	}
	c.Cancel(a.singing.job)
	a.submitSinging(false)
	if len(c.Jobs()) != 2 {
		t.Fatalf("retry %+v", c.Jobs())
	}
}
