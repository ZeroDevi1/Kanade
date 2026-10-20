// Kanade 是音频生成、人声转换与音乐分析的桌面聚合工具：Go + MyGo Native UI 外壳，
// 经 Conductor 驱动 uv 管理的 Python 推理引擎。
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

	// be 在窗口创建后由 startBackend 赋值；测试中为零值（cond 为 nil），页面显示后端不可用。
	be backend

	current      string // 当前页面，取 navItem.id
	navCollapsed bool

	lab      labState
	settings settingsState
	cantus   cantusState
}

func newApp() *app {
	return &app{current: "cantus", post: func(fn func()) { fn() }, cantus: newCantusState()}
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
		// Conductor 的状态变化来自后台 goroutine：经 Update 回到 UI 线程重建视图（视图读取的是快照）。
		a.be = startBackend(func() { a.post(func() {}) })
		if a.be.err != nil {
			log.Printf("后端启动失败：%v", a.be.err)
		}
	})
	// 退出时关闭 worker，释放显存；App.Exit 不触发此钩子。
	mygo.App.OnQuit(func() {
		if a.be.cond != nil {
			a.be.cond.Shutdown(shutdownGrace)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
