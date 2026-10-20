package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/cantus"
	"kanade/internal/conductor"
	"kanade/internal/paths"
)

// 界面输入经 cantus.Request 组装后提交给 Conductor（不启动调度，任务停在队列中）。
func TestCantusSubmitsZeroShot(t *testing.T) {
	l := paths.New(t.TempDir())
	m := cantus.Models[0]
	os.MkdirAll(m.Dir(l), 0o755)
	os.WriteFile(filepath.Join(m.Dir(l), ".kanade-complete"), []byte("{}"), 0o644)
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}

	a := newApp()
	a.be = backend{layout: l, cond: cond}
	a.cantus.promptWav, a.cantus.promptText, a.cantus.text = "/tmp/p.wav", "参考", "目标文本"
	tt := ui.NewTester(a.view, 1200, 1400)
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %+v, err %q", jobs, a.cantus.err)
	}
	j := jobs[0]
	if j.Task != "cosyvoice.zero_shot" || j.Params["prompt_wav"] != "/tmp/p.wav" || j.Params["text"] != "目标文本" || j.Params["model_dir"] != m.Dir(l) {
		t.Errorf("job %+v", j)
	}

	// 任务进行中「生成」按钮禁用；取消排队任务后，缺少参考文本时不提交，并在界面显示原因。
	cond.Cancel(j.ID)
	a.cantus.promptText = ""
	tt.Frame()
	if err := tt.Click("生成"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(cond.Jobs()) != 1 || !tt.HasText("零样本克隆需要填写参考音频对应的文本") {
		t.Errorf("jobs %d, texts %q", len(cond.Jobs()), tt.Texts())
	}
}
