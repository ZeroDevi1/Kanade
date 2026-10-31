package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/egoist/mygo/ui"
	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/singing"
)

func newSingingModelsTestApp(t *testing.T) (*app, *conductor.Conductor) {
	t.Helper()
	fs := fstest.MapFS{}
	for _, name := range []string{"yingmusic", "soulx", "vevo2"} {
		fs[name+"/engine.json"] = &fstest.MapFile{Data: []byte(fmt.Sprintf(`{"name":%q,"module":"test","tasks":[%q,%q]}`, name, name+".edit", name+".doctor"))}
	}
	cond, err := conductor.New(paths.New(t.TempDir()), fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be.cond = cond
	a.current = "singing"
	dir := t.TempDir()
	file := filepath.Join(dir, "fixture")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a.singing.soulx.request = singing.SoulXRequest{PythonPath: file, SourceDir: dir, ModelDir: dir, PromptAudio: file, PromptMetadata: file, TargetMetadata: file, Control: "score"}
	a.singing.vevo2.request = singing.Vevo2Request{PythonPath: file, SourceDir: dir, ModelDir: dir, WhisperPath: file, SourceAudio: file, OriginalText: "原词", TargetText: "新词"}
	return a, cond
}
func TestSingingModelsNavigationAndInputs(t *testing.T) {
	a, _ := newSingingModelsTestApp(t)
	tt := ui.NewTester(a.view, 1200, 2200)
	for _, mode := range []struct{ label, key, expected string }{{"SoulX SVS", "soulx", "目标 metadata JSON（包含新歌词音素）"}, {"Vevo2 AR+FM", "vevo2", "已阅读并确认遵守 Vevo2 权重许可"}, {"YingMusic", "", "音色参考原歌词（必填）"}} {
		if err := tt.Click(mode.label); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if a.singing.selectedBackend != mode.key || !tt.HasText(mode.expected) {
			t.Fatalf("mode %s texts %q", mode.key, tt.Texts())
		}
		if err := tt.Click("主页"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if err := tt.Click("歌声改词"); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if a.singing.selectedBackend != mode.key {
			t.Fatal("model selection lost on return")
		}
	}
	if a.singing.vevo2.request.OriginalText != "原词" || a.singing.vevo2.request.TargetText != "新词" {
		t.Fatal("model switching lost inputs")
	}
}
func TestSoulXMetadataRequired(t *testing.T) {
	a, c := newSingingModelsTestApp(t)
	a.singing.soulx.request.TargetMetadata = ""
	a.submitSingingModel("soulx", false)
	if len(c.Jobs()) != 0 || a.singing.soulx.jobs.err == "" {
		t.Fatal("missing metadata accepted")
	}
}
func TestVevo2LicenseExplicitForDoctorAndEdit(t *testing.T) {
	a, c := newSingingModelsTestApp(t)
	for _, doctor := range []bool{true, false} {
		a.submitSingingModel("vevo2", doctor)
		if len(c.Jobs()) != 0 || a.singing.vevo2.jobs.err == "" {
			t.Fatal("license not enforced")
		}
	}
}
func TestSingingModelsSingleFlightCancelRetry(t *testing.T) {
	for _, engine := range []string{"soulx", "vevo2"} {
		t.Run(engine, func(t *testing.T) {
			a, c := newSingingModelsTestApp(t)
			a.singing.vevo2.request.LicenseAccepted = true
			a.submitSingingModel(engine, false)
			a.submitSingingModel(engine, false)
			a.submitSingingModel(engine, true)
			a.submitSinging(false)
			if len(c.Jobs()) != 1 || c.Jobs()[0].Task != engine+".edit" {
				t.Fatalf("jobs %+v", c.Jobs())
			}
			other := "vevo2"
			if engine == other {
				other = "soulx"
			}
			a.submitSingingModel(other, false)
			if len(c.Jobs()) != 1 {
				t.Fatal("cross-model duplicate")
			}
			c.Cancel(c.Jobs()[0].ID)
			if a.singingAnyActive() {
				t.Fatal("cancel did not release guard")
			}
			a.submitSingingModel(engine, false)
			if len(c.Jobs()) != 2 {
				t.Fatal("cannot retry")
			}
		})
	}
}
func TestSingingModelsDoctorSnapshotInvalidated(t *testing.T) {
	a, c := newSingingModelsTestApp(t)
	a.submitSingingModel("soulx", true)
	if a.singing.soulx.jobs.doctorSnapshot != soulxRuntimeKey(a.singing.soulx.request) {
		t.Fatal("snapshot not stored")
	}
	a.singing.soulx.request.ModelDir += "new"
	if a.singing.soulx.jobs.doctorSnapshot == soulxRuntimeKey(a.singing.soulx.request) {
		t.Fatal("stale snapshot")
	}
	c.Cancel(c.Jobs()[0].ID)
	a.singing.vevo2.request.LicenseAccepted = true
	a.submitSingingModel("vevo2", true)
	if a.singing.vevo2.jobs.doctorSnapshot != vevo2RuntimeKey(a.singing.vevo2.request) {
		t.Fatal("snapshot not stored")
	}
	a.singing.vevo2.request.WhisperStatsPath = "changed"
	if a.singing.vevo2.jobs.doctorSnapshot == vevo2RuntimeKey(a.singing.vevo2.request) {
		t.Fatal("stats did not invalidate")
	}
}
func TestSingingModelsCancelButtonAndLockedSelector(t *testing.T) {
	a, c := newSingingModelsTestApp(t)
	a.singing.selectedBackend = "soulx"
	a.submitSingingModel("soulx", false)
	tt := ui.NewTester(a.view, 1200, 2400)
	_ = tt.Click("Vevo2 AR+FM")
	tt.Frame()
	if a.singing.selectedBackend != "soulx" {
		t.Fatal("changed model during active task")
	}
	if err := tt.Click("取消"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.singingAnyActive() || c.Jobs()[0].State != conductor.JobCanceled {
		t.Fatal("cancel button failed")
	}
	if err := tt.Click("Vevo2 AR+FM"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.singing.selectedBackend != "vevo2" {
		t.Fatal("selector remains locked")
	}
}
