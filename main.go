// Kanade 是音频生成、人声转换与音乐分析的桌面聚合工具：Go + MyGo Native UI 外壳，
// 经 Conductor 驱动 uv 管理的 Python 推理引擎。
package main

import (
	"log"
	"os"
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
	// saveDir 是上次另存结果的目录，作为下次保存对话框的默认目录；空为数据根目录下的 outputs。
	saveDir string

	lab      labState
	settings settingsState
	cantus   cantusState
	stems    stemsState
	timbre   timbreState
}

func newApp() *app {
	return &app{current: "cantus", post: func(fn func()) { fn() }, cantus: newCantusState(), timbre: newTimbreState()}
}

func main() {
	// 单实例：重复启动时把参数交给已运行的实例并退出，避免两个实例各自启动 worker 争用 GPU，
	// 也保证启动时清理暂存区不会删掉另一个实例的结果。
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	a := newApp()
	mygo.App.OnSecondInstance(func([]string, string) {
		a.post(func() {
			if a.win != nil {
				a.win.Show()
				a.win.Focus()
			}
		})
	})
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
			// 红绿灯按钮框高 16，放在 48 高标题栏的垂直中线上，与导航切换按钮居中对齐。
			// +1：实测按钮框的上沿比给定的 Y 高 1 pt（macOS 26，Y=16 时 AX 框位于 y=15）。
			opts.TrafficLightPosition = &mygo.Point{X: 18, Y: (titleBarHeight-16)/2 + 1}
		}
		a.win = mygo.NewWindow(opts)
		a.post = a.win.Update
		// Conductor 的状态变化来自后台 goroutine：经 Update 回到 UI 线程重建视图（视图读取的是快照）。
		a.be = startBackend(func() { a.post(func() {}) })
		if a.be.err != nil {
			log.Printf("后端启动失败：%v", a.be.err)
		}
	})
	// 退出时关闭 worker，释放显存，并删除未保存的暂存结果；App.Exit 不触发此钩子。
	mygo.App.OnQuit(func() {
		if a.be.cond != nil {
			a.be.cond.Shutdown(shutdownGrace)
		}
		if a.be.layout.Scratch != "" {
			_ = os.RemoveAll(a.be.layout.Scratch)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
