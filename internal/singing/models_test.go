package singing

import (
	"os"
	"path/filepath"
	"testing"
)

func modelFixture(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "fixture")
	if e := os.WriteFile(p, []byte("fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	return d, p
}
func TestSoulXParams(t *testing.T) {
	d, f := modelFixture(t)
	r := SoulXRequest{PythonPath: f, SourceDir: d, ModelDir: d, PromptAudio: f, PromptMetadata: f, TargetMetadata: f}
	p, e := r.Params()
	if e != nil || p["control"] != "score" {
		t.Fatal(p, e)
	}
	if _, ok := p["target_text"]; ok {
		t.Fatal("SVS requires metadata not text")
	}
	r.Control = "svc"
	if _, e = r.Params(); e == nil {
		t.Fatal("accepted SVC")
	}
	r.Control = "melody"
	r.TargetMetadata = "missing"
	if _, e = r.Params(); e == nil {
		t.Fatal("accepted missing metadata")
	}
	if _, e = r.DoctorParams(); e != nil {
		t.Fatal(e)
	}
}
func TestVevo2Params(t *testing.T) {
	d, f := modelFixture(t)
	r := Vevo2Request{PythonPath: f, SourceDir: d, ModelDir: d, WhisperPath: f, SourceAudio: f, OriginalText: " original ", TargetText: "new"}
	if _, e := r.DoctorParams(); e == nil {
		t.Fatal("license acceptance required")
	}
	r.LicenseAccepted = true
	p, e := r.Params()
	if e != nil || p["original_text"] != "original" {
		t.Fatal(p, e)
	}
	if _, ok := p["whisper_stats_path"]; ok {
		t.Fatal("should use snapshot default")
	}
	r.WhisperStatsPath = "missing"
	if _, e = r.Params(); e == nil {
		t.Fatal("missing override accepted")
	}
	r.WhisperStatsPath = ""
	r.OriginalText = ""
	if _, e = r.Params(); e == nil {
		t.Fatal("must not invent original lyrics")
	}
	if _, e = r.DoctorParams(); e != nil {
		t.Fatal("doctor should not require lyrics", e)
	}
}
