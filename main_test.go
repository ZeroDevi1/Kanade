package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 视图在无窗口的 Tester 中运行，按用户的方式点击。
func TestNavigation(t *testing.T) {
	a := newApp()
	tt := ui.NewTester(a.view, 1200, 780)
	if err := tt.Click("分轨"); err != nil {
		t.Fatal(err)
	}
	if a.current != "stems" || !tt.HasText("Stems · MDX / Demucs / BS-RoFormer 源分离。") {
		t.Errorf("page %q after clicking 分轨, texts %q", a.current, tt.Texts())
	}
}

func TestSimulatedJob(t *testing.T) {
	a := newApp()
	// 后台 goroutine 的更新改为同步执行会与视图并发，这里改用通道串行化到测试 goroutine。
	updates := make(chan func(), 256)
	a.post = func(fn func()) { updates <- fn }
	tt := ui.NewTester(a.view, 1200, 780)
	if err := tt.Click("开始模拟任务"); err != nil {
		t.Fatal(err)
	}
	for a.lab.jobCancel != nil || a.lab.jobStatus == "运行中" {
		(<-updates)()
	}
	tt.Frame()
	if a.lab.jobProgress != 1 || !tt.HasText("已完成 100%") {
		t.Errorf("progress %v, texts %q", a.lab.jobProgress, tt.Texts())
	}
}
