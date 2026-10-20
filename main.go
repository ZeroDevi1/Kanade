// Kanade 是音频生成、人声转换与音乐分析的桌面聚合工具。当前为 M0 技术验证原型：
// 验证 Fluent 风格外壳（隐藏标题栏、Mica、导航栏）、波形绘制与后台任务进度刷新。
package main

import (
	"log"
	"runtime"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// app 是窗口显示的全部状态。视图在 UI 线程读取它；后台 goroutine 只能经由 post 修改。
type app struct {
	// post 在 UI 线程执行 fn 并重绘，生产环境为 Window.Update，测试中同步执行。
	post func(fn func())
	// win 在窗口创建后赋值，用于运行时切换材质；测试中为 nil。
	win *mygo.Window

	current      string // 当前页面，取 navItem.id
	navCollapsed bool

	lab labState
}

func newApp() *app {
	return &app{current: "lab", post: func(fn func()) { fn() }}
}

func main() {
	a := newApp()
	mygo.App.WhenReady(func() {
		opts := mygo.WindowOptions{
			Title:     "Kanade",
			Width:     1200,
			Height:    780,
			MinWidth:  720,
			MinHeight: 480,
			StateKey:  "main",
			// 隐藏系统标题栏，保留系统窗口按钮，由视图在其下方自绘标题栏。
			TitleBarStyle:  mygo.TitleBarHidden,
			TitleBarHeight: titleBarHeight,
			// Windows 11 22H2+ 为 Mica；macOS 映射为 under-window 材质；更早的 Windows 与 Linux 无材质，
			// 视图通过 ui.Context.Vibrancy 判断并改画纯色背景。Windows 上只有创建时带材质的窗口才能在运行时切换。
			Vibrancy:        mygo.VibrancyMica,
			BackgroundColor: "light-dark(#F3F3F3, #202020)",
			Content:         ui.View(a.view),
		}
		if runtime.GOOS == "darwin" {
			// 红绿灯按钮高 12，放在 48 高标题栏的垂直中线上。
			opts.TrafficLightPosition = &mygo.Point{X: 18, Y: (titleBarHeight - 12) / 2}
		}
		a.win = mygo.NewWindow(opts)
		a.post = a.win.Update
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
