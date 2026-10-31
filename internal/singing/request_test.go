package singing

import (
	"os"
	"path/filepath"
	"testing"
)

func validRequest(t *testing.T) Request {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "input.wav")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return Request{MelodyAudio: file, PythonPath: file, SourceDir: dir, ModelDir: dir, RefText: "原词一|原词二", TargetText: "新词一\n新词二"}
}
func TestParams(t *testing.T) {
	r := validRequest(t)
	p, err := r.Params()
	if err != nil {
		t.Fatal(err)
	}
	if p["ref_audio"] != p["melody_audio"] {
		t.Fatal("missing reference must use melody")
	}
	if p["target_text"] != "新词一|新词二" {
		t.Fatal(p)
	}
	if _, ok := p["accompaniment_audio"]; ok {
		t.Fatal("unexpected mix")
	}
	r.AccompanimentAudio = r.MelodyAudio
	if p, err = r.Params(); err != nil || p["accompaniment_audio"] != r.MelodyAudio {
		t.Fatal(p, err)
	}
}
func TestRejectInvalidRequest(t *testing.T) {
	for _, change := range []func(*Request){
		func(r *Request) { r.RefText = "" }, func(r *Request) { r.TargetText = "a||b" },
		func(r *Request) { r.MelodyAudio = "missing" }, func(r *Request) { r.RefAudio = "missing" },
		func(r *Request) { r.PythonPath = r.SourceDir }, func(r *Request) { r.ModelDir = r.MelodyAudio },
		func(r *Request) { r.AccompanimentAudio = "missing" }, func(r *Request) { r.SourceDir = "" },
	} {
		r := validRequest(t)
		change(&r)
		if _, err := r.Params(); err == nil {
			t.Fatalf("accepted invalid request: %+v", r)
		}
	}
}
