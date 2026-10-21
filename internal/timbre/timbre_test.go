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
	"kanade/internal/stems"
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
	f.mu.Lock()
	params := f.params[len(f.submitted)-1]
	f.mu.Unlock()
	arts := map[string][]string{
		"stems.chain": {"/o/歌_other.wav", "/o/歌_vocals.wav", "/o/歌_vocals_karaoke.wav", "/o/歌_vocals_other.wav", "/o/歌_vocals_karaoke_noreverb.wav", "/o/歌_vocals_karaoke_noreverb_dry.wav"},
		"timbre.mix":  {"/o/mix.wav", "/o/mix.json"},
	}[task]
	if task == "timbre.infer" {
		arts = []string{"/o/conv-" + filepath.Base(params["input"].(string))}
	}
	return conductor.Job{ID: id, State: conductor.JobDone, Artifacts: arts}, nil
}

func (f *fakeJobs) Cancel(string) {}

func runCover(t *testing.T, r CoverRequest) (*fakeJobs, CoverState) {
	t.Helper()
	jobs := &fakeJobs{}
	c, err := StartCover(jobs, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Snapshot().Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return jobs, c.Snapshot()
}

var testInfer = InferRequest{Model: Model{Name: "xt", Weights: "/xt.pth"}, F0: "rmvpe", IndexRate: 0.65, Protect: 0.4, VolumeEnvelope: 0.25, Normalize: true}

func TestCoverSongConvertsHarmony(t *testing.T) {
	jobs, s := runCover(t, CoverRequest{Song: "/music/歌.flac", StemModels: stems.Recommended, Infer: testInfer,
		HarmonyMode: HarmonyConvert, Mix: MixOptions{HarmonyGainDB: -2}})
	if s.Err != "" || s.Stage != StageDone || s.Outputs["mix"] != "/o/mix.wav" {
		t.Fatalf("state %+v", s)
	}
	if strings.Join(jobs.submitted, ",") != "stems.chain,timbre.infer,timbre.infer,timbre.mix" {
		t.Errorf("order %v", jobs.submitted)
	}
	if m := jobs.params[0]["models"].(map[string]any); m["vocals"] != "BS-Roformer-SW.ckpt" || m["noreverb"] != nil {
		t.Errorf("stem models %v", m)
	}
	if jobs.params[1]["input"] != "/o/歌_vocals_karaoke_noreverb_dry.wav" || jobs.params[2]["input"] != "/o/歌_vocals_other.wav" {
		t.Errorf("infer inputs %v / %v", jobs.params[1]["input"], jobs.params[2]["input"])
	}
	if jobs.params[1]["normalize"] != false {
		t.Error("cover conversion must not peak-normalize")
	}
	tracks := jobs.params[3]["tracks"].([]map[string]any)
	want := [][3]any{
		{"/o/conv-歌_vocals_karaoke_noreverb_dry.wav", RoleVocal, "/o/歌_vocals_karaoke_noreverb_dry.wav"},
		{"/o/歌_other.wav", RoleInst, nil},
		{"/o/conv-歌_vocals_other.wav", RoleHarmony, "/o/歌_vocals_other.wav"},
	}
	if len(tracks) != len(want) {
		t.Fatalf("tracks %v", tracks)
	}
	for i, w := range want {
		if tracks[i]["path"] != w[0] || tracks[i]["role"] != w[1] || tracks[i]["ref"] != w[2] {
			t.Errorf("track %d = %v, want %v", i, tracks[i], w)
		}
	}
	if tracks[2]["gain_db"] != -2.0 {
		t.Errorf("harmony offset %v", tracks[2]["gain_db"])
	}
}

func TestCoverDryVocal(t *testing.T) {
	// 有伴奏：跳过分轨与和声，主唱电平由混音按伴奏推荐（不带参考轨）。
	jobs, s := runCover(t, CoverRequest{Source: SourceDry, Vocal: "/in/干声.wav", Inst: "/in/伴奏.wav", Infer: testInfer})
	if s.Err != "" || s.Stage != StageDone || !s.Skipped[StageStems] || !s.Skipped[StageHarmony] {
		t.Fatalf("state %+v", s)
	}
	if strings.Join(jobs.submitted, ",") != "timbre.infer,timbre.mix" {
		t.Errorf("order %v", jobs.submitted)
	}
	tracks := jobs.params[1]["tracks"].([]map[string]any)
	if len(tracks) != 2 || tracks[0]["ref"] != nil || tracks[1]["path"] != "/in/伴奏.wav" {
		t.Errorf("tracks %v", tracks)
	}
	if !strings.HasPrefix(jobs.params[1]["name"].(string), "干声_cover_xt") {
		t.Errorf("name %v", jobs.params[1]["name"])
	}

	// 没有伴奏：对齐输入干声本身的电平。
	jobs, _ = runCover(t, CoverRequest{Source: SourceDry, Vocal: "/in/干声.wav", Infer: testInfer})
	if tracks := jobs.params[1]["tracks"].([]map[string]any); len(tracks) != 1 || tracks[0]["ref"] != "/in/干声.wav" {
		t.Errorf("tracks %v", tracks)
	}
	if _, err := StartCover(&fakeJobs{}, CoverRequest{Source: SourceDry, Infer: testInfer}, nil); err == nil {
		t.Error("dry mode without vocal accepted")
	}
}

func TestTrainParamsAuto(t *testing.T) {
	ds := t.TempDir()
	p, err := TrainRequest{Name: "m", Dataset: ds, F0: "rmvpe"}.Params()
	if err != nil || p["epochs"] != 0 || p["sample_rate"] != 0 || p["cut_mode"] != nil {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := (TrainRequest{Name: "m", Dataset: ds, F0: "rmvpe", SampleRate: 44100}).Params(); err == nil {
		t.Error("44100 accepted")
	}
	if _, err := (TrainRequest{Name: "m", Dataset: ds, F0: "rmvpe", CutMode: "Fancy"}).Params(); err == nil {
		t.Error("bad cut mode accepted")
	}
}
