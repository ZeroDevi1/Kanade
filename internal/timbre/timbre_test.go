package timbre

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kanade/internal/conductor"
	"kanade/internal/paths"
)

func touch(t *testing.T, p string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListModelsPicksLatestWeights(t *testing.T) {
	l := paths.New(t.TempDir())
	dir := filepath.Join(logsDir(l), "xingtong")
	for _, f := range []string{"xingtong_9e_900s.pth", "xingtong_12e_1200s.pth", "xingtong_12e_1100s.pth", "G_1200.pth", "D_1200.pth", "xingtong.index"} {
		touch(t, filepath.Join(dir, f))
	}
	touch(t, filepath.Join(logsDir(l), "empty", "G_1.pth")) // 只有检查点、没有导出权重：不是可用模型

	src := t.TempDir()
	touch(t, filepath.Join(src, "voice.pth"))
	if _, err := Import(l, "导入 模型", filepath.Join(src, "voice.pth"), ""); err == nil {
		t.Error("name with space accepted")
	}
	if _, err := Import(l, "xingtong", filepath.Join(src, "voice.pth"), ""); err == nil {
		t.Error("duplicate of trained model accepted")
	}
	imp, err := Import(l, "炫神", filepath.Join(src, "voice.pth"), "")
	if err != nil {
		t.Fatal(err)
	}

	models := ListModels(l)
	if len(models) != 2 {
		t.Fatalf("models %+v", models)
	}
	var trained Model
	for _, m := range models {
		if m.Trained {
			trained = m
		}
	}
	if filepath.Base(trained.Weights) != "xingtong_12e_1200s.pth" || trained.Epoch != 12 || filepath.Base(trained.Index) != "xingtong.index" {
		t.Errorf("trained %+v", trained)
	}
	if err := DeleteImported(l, trained); err == nil {
		t.Error("trained model deletable")
	}
	if err := DeleteImported(l, imp); err != nil || len(ListModels(l)) != 1 {
		t.Errorf("delete imported: %v", err)
	}
}

func TestInferParamsValidation(t *testing.T) {
	m := Model{Name: "m", Weights: "/m.pth"}
	ok := InferRequest{Model: m, Input: "/in.wav", F0: "rmvpe", IndexRate: 0.8, Protect: 0.33, VolumeEnvelope: 0.25}
	if p, err := ok.Params(); err != nil || p["stereo_mode"] != "mono" {
		t.Fatalf("%v %v", p, err)
	}
	bad := []func(r *InferRequest){
		func(r *InferRequest) { r.Pitch = 30 },
		func(r *InferRequest) { r.F0 = "pm" },
		func(r *InferRequest) { r.IndexRate = 1.5 },
		func(r *InferRequest) { r.Model = Model{} },
	}
	for i, f := range bad {
		r := ok
		f(&r)
		if _, err := r.Params(); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

// fakeJobs 按任务名返回预设产物，记录提交顺序与参数。
type fakeJobs struct {
	mu        sync.Mutex
	submitted []string
	params    []map[string]any
}

func (f *fakeJobs) Submit(engine, task string, params map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, task)
	f.params = append(f.params, params)
	return fmt.Sprintf("%s#%d", task, len(f.submitted)), nil
}

func (f *fakeJobs) Wait(_ context.Context, id string) (conductor.Job, error) {
	task := strings.Split(id, "#")[0]
	arts := map[string][]string{
		"stems.chain":  {"/o/歌_other.wav", "/o/歌_vocals.wav", "/o/歌_vocals_karaoke.wav", "/o/歌_vocals_other.wav", "/o/歌_vocals_karaoke_noreverb.wav", "/o/歌_vocals_karaoke_noreverb_dry.wav"},
		"timbre.infer": {"/o/converted.wav"},
		"timbre.mix":   {"/o/mix.wav"},
	}[task]
	return conductor.Job{ID: id, State: conductor.JobDone, Artifacts: arts}, nil
}

func (f *fakeJobs) Cancel(string) {}

func TestCoverPipeline(t *testing.T) {
	jobs := &fakeJobs{}
	r := CoverRequest{Song: "/music/歌.flac", Backing: true, BackingGainDB: -6,
		Infer: InferRequest{Model: Model{Name: "xt", Weights: "/xt.pth"}, F0: "rmvpe", IndexRate: 0.65, Protect: 0.4, VolumeEnvelope: 0.25}}
	c, err := StartCover(jobs, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Snapshot().Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := c.Snapshot()
	if s.Err != "" || s.Stage != StageDone || s.Outputs["mix"] != "/o/mix.wav" {
		t.Fatalf("state %+v", s)
	}
	if strings.Join(jobs.submitted, ",") != "stems.chain,timbre.infer,timbre.mix" {
		t.Errorf("order %v", jobs.submitted)
	}
	if jobs.params[1]["input"] != "/o/歌_vocals_karaoke_noreverb_dry.wav" {
		t.Errorf("infer input %v", jobs.params[1]["input"])
	}
	tracks := jobs.params[2]["tracks"].([]map[string]any)
	if len(tracks) != 3 || tracks[1]["path"] != "/o/歌_other.wav" || tracks[2]["path"] != "/o/歌_vocals_other.wav" || tracks[2]["gain_db"] != -6.0 {
		t.Errorf("tracks %v", tracks)
	}
}
