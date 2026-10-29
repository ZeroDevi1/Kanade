package timbre

import (
	"math"
	"testing"
)

func TestTrainBWEParams(t *testing.T) {
	base := TrainRequest{Name: "bwe_test", Dataset: t.TempDir(), F0: "rmvpe"}
	for _, mode := range []string{"", "off", "auto", "manual"} {
		r := base
		r.BWEMode, r.BWECutoffHz = mode, 8000
		p, err := r.Params()
		if err != nil {
			t.Fatal(err)
		}
		want := mode
		if want == "" {
			want = "off"
		}
		if p["bwe_mode"] != want {
			t.Fatalf("mode %q: %v", mode, p)
		}
		if mode == "manual" && p["bwe_cutoff_hz"] != float64(8000) {
			t.Fatal(p)
		}
		if mode != "manual" && p["bwe_cutoff_hz"] != nil {
			t.Fatal(p)
		}
	}
	for _, cutoff := range []float64{0, 1999, 15000, math.NaN(), math.Inf(1)} {
		r := base
		r.BWEMode, r.BWECutoffHz = "manual", cutoff
		if _, err := r.Params(); err == nil {
			t.Errorf("accepted cutoff %v", cutoff)
		}
	}
	for _, cutoff := range []float64{2000, 14999} {
		r := base
		r.BWEMode, r.BWECutoffHz = "manual", cutoff
		if _, err := r.Params(); err != nil {
			t.Error(err)
		}
	}
	// The engine validates resume fingerprints; retain both flags for a verified reuse.
	r := base
	r.BWEMode, r.SkipPreprocess, r.SkipExtract = "auto", true, true
	if p, err := r.Params(); err != nil || p["skip_preprocess"] != true || p["skip_extract"] != true {
		t.Fatalf("resume flags: %v %v", p, err)
	}
	for _, r := range []TrainRequest{
		{Name: base.Name, Dataset: base.Dataset, F0: base.F0, BWEMode: "invalid"},
	} {
		if _, err := r.Params(); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestBWEPreviewParams(t *testing.T) {
	r := BWEPreviewRequest{Dataset: t.TempDir(), List: "selected.list", BWEMode: "auto"}
	p, err := r.Params()
	if err != nil {
		t.Fatal(err)
	}
	if p["preview_count"] != 3 || p["dataset_dir"] != r.Dataset || p["list"] != r.List {
		t.Fatal(p)
	}
	for _, key := range []string{"name", "epochs", "skip_preprocess"} {
		if _, ok := p[key]; ok {
			t.Errorf("preview contains training field %s", key)
		}
	}
	r.BWEMode, r.BWECutoffHz, r.PreviewCount = "manual", 7000, 5
	if p, err := r.Params(); err != nil || p["bwe_cutoff_hz"] != float64(7000) || p["preview_count"] != 5 {
		t.Fatalf("%v %v", p, err)
	}
	for _, count := range []int{-1, 6} {
		r.PreviewCount = count
		if _, err := r.Params(); err == nil {
			t.Errorf("accepted count %d", count)
		}
	}
	r.PreviewCount = 3
	for _, mode := range []string{"", "off", "invalid"} {
		r.BWEMode = mode
		if _, err := r.Params(); err == nil {
			t.Errorf("accepted preview mode %q", mode)
		}
	}
	r.BWEMode, r.Dataset = "auto", ""
	if _, err := r.Params(); err == nil {
		t.Error("accepted missing dataset")
	}
}
