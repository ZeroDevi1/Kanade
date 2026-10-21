package score

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kanade/internal/conductor"
	"kanade/internal/stems"
)

// fileJobs 像真实引擎一样把产物写到暂存目录 dir 下的任务子目录。
type fileJobs struct {
	dir       string
	mu        sync.Mutex
	submitted []string
	params    []map[string]any
}

func (f *fileJobs) Submit(_, task string, params map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted, f.params = append(f.submitted, task), append(f.params, params)
	return task + "#" + string(rune('0'+len(f.submitted))), nil
}

func (f *fileJobs) Wait(_ context.Context, id string) (conductor.Job, error) {
	out := filepath.Join(f.dir, strings.ReplaceAll(id, "#", "-"))
	os.MkdirAll(out, 0o755)
	write := func(name, body string) string {
		p := filepath.Join(out, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	var arts []string
	switch strings.Split(id, "#")[0] {
	case "stems.chain":
		arts = []string{write("歌_other.wav", "other"), write("歌_vocals_karaoke.wav", "lead")}
	case "score.analyze":
		data, _ := json.Marshal(sample())
		arts = []string{write("audio.wav", "audio"), write("analysis.json", string(data)), write("chords.lab", "0\t1\tC\n")}
	}
	return conductor.Job{ID: id, State: conductor.JobDone, Artifacts: arts}, nil
}

func (*fileJobs) Cancel(string) {}

func wait(t *testing.T, run *Run) RunState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for run.Snapshot().Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return run.Snapshot()
}

// 分析结果保存为记录、重启后读回；同一首歌再次分析时复用记录里的分轨，不再提交分离任务；删除记录移除目录。
func TestHistorySaveLoadReuse(t *testing.T) {
	root, scratch := t.TempDir(), t.TempDir()
	input := filepath.Join(t.TempDir(), "歌.flac")
	os.WriteFile(input, []byte("flac"), 0o644)
	req := Request{Input: input, Separate: true, Melody: true, StemModels: stems.Recommended, Options: DefaultOptions(), History: root}

	jobs := &fileJobs{dir: scratch}
	run, _ := Start(jobs, req, nil)
	st := wait(t, run)
	if st.Err != "" || !st.Saved || st.SaveErr != "" {
		t.Fatalf("first run %+v", st)
	}
	if !strings.HasPrefix(st.Audio, root) || !strings.HasPrefix(st.Files["analysis"], root) || !fileExists(st.Files["chords"]) {
		t.Fatalf("files not moved into history: %+v", st)
	}

	runs := LoadHistory(root)
	if len(runs) != 1 {
		t.Fatalf("loaded %d records", len(runs))
	}
	got := runs[0].Snapshot()
	if got.Running() || got.Result == nil || got.Result.Key.Label != "C 大调" || got.Audio != st.Audio || runs[0].Request.Input != input {
		t.Fatalf("loaded %+v", got)
	}

	jobs2 := &fileJobs{dir: scratch}
	run2, _ := Start(jobs2, req, nil)
	st2 := wait(t, run2)
	if st2.Err != "" || !st2.Reused || strings.Join(jobs2.submitted, ",") != "score.analyze" {
		t.Fatalf("second run %+v submitted %v", st2, jobs2.submitted)
	}
	if h := jobs2.params[0]["harmonic_input"].(string); !strings.HasPrefix(h, root) || !strings.HasSuffix(h, "_other.wav") {
		t.Errorf("reused harmonic %q", h)
	}

	// 改了分轨模型就不能复用。
	other := req
	other.StemModels = nil
	if s := findStems(root, other); s.harmonic != "" {
		t.Errorf("reused stems across models: %+v", s)
	}

	if err := runs[0].Delete(); err != nil {
		t.Fatal(err)
	}
	if len(LoadHistory(root)) != 1 { // 第二次分析的记录仍在
		t.Errorf("records after delete: %d", len(LoadHistory(root)))
	}
}

// 没有设置 History 时行为与之前一致：结果留在暂存区，不能删除。
func TestRunWithoutHistory(t *testing.T) {
	input := filepath.Join(t.TempDir(), "歌.flac")
	os.WriteFile(input, []byte("flac"), 0o644)
	run, _ := Start(&fileJobs{dir: t.TempDir()}, Request{Input: input, Options: DefaultOptions()}, nil)
	st := wait(t, run)
	if st.Err != "" || st.Saved || run.Delete() == nil {
		t.Fatalf("state %+v", st)
	}
}
