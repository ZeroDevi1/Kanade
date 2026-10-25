package conductor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"kanade/engines"
	"kanade/internal/paths"
)

func TestLoadSpecsAndSourceHash(t *testing.T) {
	src := fstest.MapFS{
		"sdk/src/kanade_engine/__init__.py":   {Data: []byte("v1")},
		"sdk/src/kanade_engine/__pycache__/x": {Data: []byte("ignored")},
		"echo/engine.json":                    {Data: []byte(`{"name":"echo","python":"3.12","module":"echo","tasks":["t"]}`)},
	}
	specs, err := LoadSpecs(src)
	if err != nil || len(specs) != 1 || specs[0].Name != "echo" {
		t.Fatalf("specs %+v, err %v", specs, err)
	}
	h1, _ := sourceHash(src, "echo")
	src["sdk/src/kanade_engine/__pycache__/x"] = &fstest.MapFile{Data: []byte("changed")}
	if h2, _ := sourceHash(src, "echo"); h2 != h1 {
		t.Error("__pycache__ changed the source hash")
	}
	src["sdk/src/kanade_engine/__init__.py"] = &fstest.MapFile{Data: []byte("v2")}
	if h3, _ := sourceHash(src, "echo"); h3 == h1 {
		t.Error("shared sdk change did not change the source hash")
	}
}

// testRoot 返回集成测试的数据根目录。它跨次运行保留，避免每次重新下载 Python 运行时；
// 可用 KANADE_TEST_ROOT 指定。
func testRoot(t *testing.T) string {
	if r := os.Getenv("KANADE_TEST_ROOT"); r != "" {
		return r
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache dir")
	}
	return filepath.Join(dir, "kanade-conductor-test")
}

// 端到端：用真实 uv 安装 dev-mock，执行一个任务、取消一个任务。需要 PATH 中的 uv 与网络（首次运行）。
func TestDevMockEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not in PATH")
	}
	l := paths.New(testRoot(t))
	c, err := New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Start()
	defer c.Shutdown(10 * time.Second)

	done, err := c.Submit("dev-mock", "mock.tts", map[string]any{"text": "测试", "steps": 5})
	if err != nil {
		t.Fatal(err)
	}
	slow, _ := c.Submit("dev-mock", "mock.tts", map[string]any{"steps": 500})

	j := waitJob(t, c, done, 5*time.Minute)
	if j.State != JobDone || len(j.Artifacts) != 1 {
		t.Fatalf("job %+v", j)
	}
	if st, err := os.Stat(j.Artifacts[0]); err != nil || st.Size() < 1000 {
		t.Fatalf("artifact %s: %v", j.Artifacts[0], err)
	}

	// 等第二个任务开始运行后取消，worker 应在检查点停止并回报 canceled。
	deadline := time.Now().Add(time.Minute)
	for {
		if j, _ := c.Job(slow); j.State == JobRunning && j.Progress > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow job never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.Cancel(slow)
	if j := waitJob(t, c, slow, 30*time.Second); j.State != JobCanceled {
		t.Fatalf("slow job %+v", j)
	}

	// 环境不变时复用 worker；SetEnv 改变环境后，下一个任务换用新启动的 worker。
	quick := func() *Worker {
		t.Helper()
		id, _ := c.Submit("dev-mock", "mock.tts", map[string]any{"steps": 1})
		if j := waitJob(t, c, id, time.Minute); j.State != JobDone {
			t.Fatalf("job %+v", j)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.worker
	}
	w1 := quick()
	if w2 := quick(); w2 != w1 {
		t.Error("worker restarted without an env change")
	}
	c.SetEnv([]string{"KANADE_HF_TOKEN=hf_test"})
	w3 := quick()
	if w3 == w1 {
		t.Error("worker kept after env change")
	}
	// FreshTasks 中的任务每次都交给没有运行过任务的 worker（释放上一个任务留在进程里的显存）。
	c.mu.Lock()
	c.engines["dev-mock"].spec.FreshTasks = []string{"mock.tts"}
	c.mu.Unlock()
	w4 := quick()
	if w4 == w3 || quick() == w4 {
		t.Error("fresh task reused a worker that had already run a task")
	}
}

func waitJob(t *testing.T, c *Conductor, id string, timeout time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j, ok := c.Job(id)
		if !ok {
			t.Fatalf("job %s missing", id)
		}
		if j.State.Finished() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s: %s", id, j.State, j.Message)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
