package main

import (
	"context"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/settings"
	"kanade/internal/ui/fluent"
)

// settingsState 是设置页的界面状态。
type settingsState struct {
	// pendingRoot 是已保存、重启后生效的新根目录；为空表示没有待生效的修改。
	pendingRoot string
	// uvBusy 为 true 时正在下载 uv；uvVersion 为最近一次 uv --version 的输出（空表示未查询）。
	uvBusy    bool
	uvVersion string
	err       string
	// expanded 记录展开了安装日志的引擎名称。
	expanded map[string]bool
}

var engineStateLabel = map[conductor.EngineState]string{
	conductor.EngineMissing:    "未安装",
	conductor.EngineInstalling: "安装中",
	conductor.EngineReady:      "就绪",
	conductor.EngineRunning:    "运行中",
	conductor.EngineFailed:     "失败",
}

func (a *app) settingsPage(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.settings
	pageHeader(c, "设置", "数据根目录、uv 与推理引擎环境。")
	if a.backendUnavailable(c, tk) {
		return
	}
	l := a.be.layout

	card(c, tk, "数据根目录", func() {
		ui.Text(c, l.Root).Font("monospace").FontSize(12).Selectable()
		ui.Text(c, "存放 uv、Python 运行时、引擎虚拟环境、模型与输出，可能占用数十 GB。").FontSize(12).TextColor(t.TextMuted)
		if paths.SyncedFolderRisk(l.Root) {
			ui.Text(c, "当前目录位于云同步文件夹（OneDrive / iCloud / Dropbox）中：虚拟环境和模型会被上传，"+
				"并可能被替换为在线占位文件导致加载失败。建议改到本地磁盘。").FontSize(12).TextColor(t.Warning)
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "在文件管理器中打开").Clicked() {
				go func() { _ = mygo.Shell.OpenPath(l.Root) }()
			}
			if ui.Button(c, "更改…").Clicked() {
				a.chooseRoot()
			}
			if s.pendingRoot != "" {
				ui.Text(c, "已改为 "+s.pendingRoot+"，重启后生效").FontSize(12).TextColor(t.TextMuted)
				if ui.PrimaryButton(c, "立即重启").Clicked() {
					mygo.App.Relaunch()
				}
			}
		})
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(t.Danger)
		}
	})

	card(c, tk, "uv", func() {
		uv := a.be.cond.UV()
		switch {
		case uv == nil:
			ui.Text(c, "未找到 uv。首次安装引擎时会自动下载 uv "+conductor.UVVersion+" 到数据根目录。").TextColor(t.TextMuted)
		default:
			src := "系统 PATH"
			if uv.Source == conductor.UVFromDownloaded {
				src = "已下载"
			}
			ui.Text(c, src+" · "+uv.Path).Font("monospace").FontSize(12).Selectable()
			if s.uvVersion == "" && !s.uvBusy {
				a.queryUVVersion(uv)
			}
			if s.uvVersion != "" {
				ui.Text(c, s.uvVersion).FontSize(12).TextColor(t.TextMuted)
			}
		}
		if uv == nil {
			label := "下载 uv"
			if s.uvBusy {
				label = "下载中…"
			}
			if ui.PrimaryButton(c, label).Disabled(s.uvBusy).Clicked() {
				a.downloadUV()
			}
		}
	})

	card(c, tk, "推理引擎", func() {
		for _, e := range a.be.cond.Engines() {
			a.engineRow(c, tk, e)
		}
	})
}

// engineRow 显示一个引擎的状态、操作按钮和（展开时的）安装日志。
func (a *app) engineRow(c *ui.Context, tk *fluent.Tokens, e conductor.Engine) {
	t := c.Theme()
	s := &a.settings
	ui.Column(c.Key("engine-" + e.Spec.Name)).Gap(6).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, e.Spec.Title+"（"+e.Spec.Name+"）").FontWeight(600)
				ui.Text(c, e.Spec.Description+" Python "+e.Spec.Python).FontSize(12).TextColor(t.TextMuted)
			})
			ui.Text(c, engineStateLabel[e.State]).FontSize(12).TextColor(t.TextMuted)
			if e.State == conductor.EngineInstalling {
				ui.Spinner(c)
			}
			if (e.State == conductor.EngineMissing || e.State == conductor.EngineFailed) && ui.Button(c, "安装环境").Clicked() {
				a.be.cond.Install(e.Spec.Name)
			}
			if len(e.Log) > 0 {
				label := "查看日志"
				if s.expanded[e.Spec.Name] {
					label = "收起日志"
				}
				if ui.Button(c, label).Clicked() {
					if s.expanded == nil {
						s.expanded = map[string]bool{}
					}
					s.expanded[e.Spec.Name] = !s.expanded[e.Spec.Name]
				}
			}
		})
		if e.Err != "" {
			ui.Text(c, e.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		if s.expanded[e.Spec.Name] {
			ui.Text(c, strings.Join(e.Log, "\n")).Font("monospace").FontSize(11).Padding(8, 10).
				Radius(fluent.ControlRadius).Background(tk.SubtleHover).Selectable()
		}
	})
}

// chooseRoot 弹出目录选择框；选定后写入设置，重启后生效。对话框会阻塞调用方，因此放在 goroutine 中。
func (a *app) chooseRoot() {
	win, userData, cur := a.win, a.be.userData, a.be.settings
	go func() {
		dirs, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent: win, Title: "选择数据根目录", Directory: true, CreateDirectories: true,
		})
		if err != nil || len(dirs) == 0 {
			if err != nil {
				a.post(func() { a.settings.err = err.Error() })
			}
			return
		}
		cur.DataRoot = dirs[0]
		err = settings.Save(userData, cur)
		a.post(func() {
			if err != nil {
				a.settings.err = "保存设置失败：" + err.Error()
				return
			}
			a.be.settings, a.settings.pendingRoot, a.settings.err = cur, cur.DataRoot, ""
		})
	}()
}

func (a *app) downloadUV() {
	a.settings.uvBusy, a.settings.err = true, ""
	go func() {
		_, err := a.be.cond.EnsureUV(context.Background())
		a.post(func() {
			a.settings.uvBusy = false
			if err != nil {
				a.settings.err = "下载 uv 失败：" + err.Error()
			}
		})
	}()
}

// queryUVVersion 在后台查询 uv 版本；结果为空时显示「未知版本」，避免反复查询。
func (a *app) queryUVVersion(uv *conductor.UV) {
	a.settings.uvBusy = true
	go func() {
		v, err := uv.Version(context.Background())
		if err != nil || v == "" {
			v = "未知版本"
		}
		a.post(func() { a.settings.uvBusy, a.settings.uvVersion = false, v })
	}()
}
