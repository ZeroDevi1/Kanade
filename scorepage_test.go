package main

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/conductor"
	"kanade/internal/paths"
)

// 音乐分析：智能模式只有歌曲与「分离伴奏」；专业模式出现全部参数。不分离时直接提交 score.analyze。
func TestScorePage(t *testing.T) {
	l := paths.New(t.TempDir())
	cond, err := conductor.New(l, engines.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp()
	a.be = backend{layout: l, cond: cond}
	a.current = "score"
	tt := ui.NewTester(a.view, 1200, 2400)
	if !tt.HasText("下载缺少的模型（约 696 MB）") || tt.HasText("和弦词汇") {
		t.Errorf("smart texts %q", tt.Texts())
	}
	if err := tt.Click("专业"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"节拍算法", "调性轮廓", "和弦词汇", "最短和弦（拍）", "和弦平滑", "分离模型"} {
		if !tt.HasText(label) {
			t.Errorf("pro missing %q", label)
		}
	}
	a.score.input, a.score.separate, a.score.vocabTitle = "/music/song.flac", false, "含七和弦、减三、挂四"
	tt.Frame()
	if err := tt.Click("开始分析"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(cond.Jobs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	jobs := cond.Jobs()
	if len(jobs) != 1 || jobs[0].Task != "score.analyze" || jobs[0].Params["harmonic_input"] != nil ||
		jobs[0].Params["options"].(map[string]any)["chord_vocab"] != "extended" {
		t.Fatalf("jobs %+v, err %q", jobs, a.score.err)
	}
	a.score.runs[0].Cancel()
}
