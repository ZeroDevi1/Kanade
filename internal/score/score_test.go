package score

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kanade/internal/conductor"
	"kanade/internal/stems"
)

func sample() *Analysis {
	a := &Analysis{Duration: 10, Beats: []float64{0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5}, Downbeats: []float64{0.5, 2.5, 4.5}}
	a.Chords = []Segment{{Start: 0, End: 2.5, Label: "C"}, {Start: 2.5, End: 3.5, Label: "G"}, {Start: 3.5, End: 4.5, Label: "Am"}, {Start: 4.5, End: 10, Label: "F"}}
	a.Key.Label = "C 大调"
	return a
}

func TestBarsAndPosition(t *testing.T) {
	a := sample()
	bars := a.Bars()
	if len(bars) != 3 || strings.Join(bars[0].Chords, ",") != "C" || strings.Join(bars[1].Chords, ",") != "G,Am" || bars[2].End != 10 {
		t.Fatalf("bars %+v", bars)
	}
	if bar, beat := a.BarAt(3.1); bar != 2 || beat != 2 {
		t.Errorf("BarAt(3.1) = %d·%d", bar, beat)
	}
	if bar, _ := a.BarAt(0.2); bar != 0 {
		t.Errorf("pickup bar %d", bar)
	}
	if i := a.ChordAt(3.6); a.Chords[i].Label != "Am" {
		t.Errorf("ChordAt %d", i)
	}
	chart := a.ChordChart("歌")
	if !strings.Contains(chart, "| C") || !strings.Contains(chart, "G Am") {
		t.Errorf("chart %q", chart)
	}
}

func TestOptionsValidation(t *testing.T) {
	r := Request{Input: "/a.flac", Options: DefaultOptions()}
	p, err := r.Params("/inst.wav")
	if err != nil || p["harmonic_input"] != "/inst.wav" || p["options"].(map[string]any)["beat_method"] != "beat_this" {
		t.Fatalf("%v %v", p, err)
	}
	r.Options.TempoMin, r.Options.TempoMax = 80, 120 // 上限不足下限 2 倍，无法折叠
	if _, err := r.Params(""); err == nil {
		t.Error("narrow tempo range accepted")
	}
	r.Options.TempoMin, r.Options.TempoMax = 0, 0
	r.Options.ChordSelfProb = 1.2
	if _, err := r.Params(""); err == nil {
		t.Error("self prob accepted")
	}
}

type fakeJobs struct {
	mu        sync.Mutex
	submitted []string
	params    []map[string]any
	dir       string
}

func (f *fakeJobs) Submit(engine, task string, params map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted, f.params = append(f.submitted, task), append(f.params, params)
	return fmt.Sprintf("%s#%d", task, len(f.submitted)), nil
}

func (f *fakeJobs) Wait(_ context.Context, id string) (conductor.Job, error) {
	var arts []string
	switch strings.Split(id, "#")[0] {
	case "stems.chain":
		arts = []string{"/o/歌_other.wav"}
	case "score.analyze":
		js := filepath.Join(f.dir, "analysis.json")
		data, _ := json.Marshal(sample())
		os.WriteFile(js, data, 0o644)
		arts = []string{filepath.Join(f.dir, "audio.wav"), js, filepath.Join(f.dir, "chords.lab")}
	}
	return conductor.Job{ID: id, State: conductor.JobDone, Artifacts: arts}, nil
}

func (f *fakeJobs) Cancel(string) {}

func TestRunSeparatesThenAnalyzes(t *testing.T) {
	jobs := &fakeJobs{dir: t.TempDir()}
	run, err := Start(jobs, Request{Input: "/music/歌.flac", Separate: true, StemModels: stems.Recommended, Options: DefaultOptions()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for run.Snapshot().Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := run.Snapshot()
	if s.Err != "" || s.Result == nil || s.Result.Key.Label != "C 大调" || filepath.Base(s.Audio) != "audio.wav" || s.Files["chords"] == "" {
		t.Fatalf("state %+v", s)
	}
	if strings.Join(jobs.submitted, ",") != "stems.chain,score.analyze" {
		t.Errorf("order %v", jobs.submitted)
	}
	if jobs.params[0]["steps"].([]string)[0] != "other" || jobs.params[0]["models"].(map[string]any)["other"] != "model_bs_roformer_ep_317_sdr_12.9755.ckpt" {
		t.Errorf("stems params %v", jobs.params[0])
	}
	if jobs.params[1]["harmonic_input"] != "/o/歌_other.wav" {
		t.Errorf("analyze params %v", jobs.params[1])
	}
}
