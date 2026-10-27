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

	// 下载镜像与 Hugging Face 令牌的编辑值；mirrorLoaded 为 false 时从已保存设置初始化。
	mirrorLoaded bool
	mirrorCN     bool
	ghMirror     string
	hfToken      string
	mirrorSaved  bool
	// doctor 是各引擎环境检查任务的 ID。
	doctor map[string]string
	s3     s3State
}

var engineStateLabel = map[conductor.EngineState]string{
	conductor.EngineMissing:    "未安装",
	conductor.EngineInstalling: "安装中",
	conductor.EngineReady:      "就绪",
	conductor.EngineRunning:    "运行中",
	conductor.EngineFailed:     "失败",
}

func (a *app) settingsPage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "设置", "数据根目录、uv、推理引擎环境与云端同步。")
	if a.backendUnavailable(c, tk) {
		return
	}
	cardRow(c, cardMinWidth, func() ui.Element { return a.rootCard(c, tk) }, func() ui.Element { return a.uvCard(c, tk) })
	cardRow(c, cardMinWidth, func() ui.Element {
		return card(c, tk, "推理引擎", func() {
			for _, e := range a.be.cond.Engines() {
				a.engineRow(c, tk, e)
			}
		})
	}, func() ui.Element { return a.mirrorCard(c, tk) })
	cardRow(c, cardMinWidth, func() ui.Element { return a.s3Card(c, tk) })
	a.doctorCard(c, tk)
}

// rootCard 显示数据根目录，可在文件管理器中打开或更改（重启后生效）。
func (a *app) rootCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.settings
	l := a.be.layout
	return card(c, tk, "数据根目录", func() {
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
}

// uvCard 显示所用的 uv（系统 PATH 或已下载）与版本；找不到时提供下载。
func (a *app) uvCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.settings
	return card(c, tk, "uv", func() {
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
}

// mirrorCard 设置模型与运行时的下载镜像和 Hugging Face 访问令牌，保存后从下一个任务起生效。
func (a *app) mirrorCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.settings
	if !s.mirrorLoaded {
		s.mirrorLoaded = true
		s.mirrorCN, s.ghMirror, s.hfToken = a.be.settings.Mirror == settings.MirrorCN, a.be.settings.GitHubMirror, a.be.settings.HFToken
	}
	return card(c, tk, "下载", func() {
		ui.Checkbox(c, &s.mirrorCN, "Hugging Face 使用国内镜像（"+settings.HFMirror+"）")
		ui.TextInput(c, &s.ghMirror).Label("GitHub 镜像前缀").Placeholder("留空直连，例如 https://ghfast.top/").Width(360)
		ui.Text(c, "GitHub 镜像用于分轨模型、uv 与 Python 运行时的下载；CosyVoice 模型可在语音生成页选择 ModelScope。").
			FontSize(12).TextColor(t.TextMuted)
		ui.TextInput(c, &s.hfToken).Label("Hugging Face 访问令牌").Placeholder("hf_…（可选）").Password().Width(360)
		ui.Text(c, "下载需要登录并接受许可的模型（音乐分析的多乐器转录 MuScriptor）时使用，Read 权限即可；以明文保存在本机设置文件中。").
			FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "保存").Clicked() {
				gh := strings.TrimSpace(s.ghMirror)
				if gh != "" && !strings.HasPrefix(gh, "https://") {
					s.err, s.mirrorSaved = "GitHub 镜像前缀须以 https:// 开头", false
					return
				}
				tok := strings.TrimSpace(s.hfToken)
				if strings.ContainsAny(tok, " \t\r\n") {
					s.err, s.mirrorSaved = "Hugging Face 令牌不能包含空白字符", false
					return
				}
				next := a.be.settings
				next.Mirror, next.GitHubMirror, next.HFToken = "", gh, tok
				if s.mirrorCN {
					next.Mirror = settings.MirrorCN
				}
				if err := settings.Save(a.be.userData, next); err != nil {
					s.err, s.mirrorSaved = "保存设置失败："+err.Error(), false
					return
				}
				a.be.settings, s.err, s.mirrorSaved = next, "", true
				a.be.cond.SetEnv(next.Env())
			}
			if s.mirrorSaved {
				ui.Text(c, "已保存，从下一个任务起生效").FontSize(12).TextColor(t.Success)
			}
		})
	})
}

// doctorCard 对已安装的引擎运行环境检查，显示 Python、torch、CUDA 版本与实际使用的设备。
func (a *app) doctorCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.settings
	card(c, tk, "环境检查", func() {
		engines := a.be.cond.Engines()
		if ui.Button(c, "检查已安装的引擎").AlignSelf(ui.Start).Clicked() {
			if s.doctor == nil {
				s.doctor = map[string]string{}
			}
			for _, e := range engines {
				if e.State == conductor.EngineReady || e.State == conductor.EngineRunning {
					id, err := a.be.cond.Submit(e.Spec.Name, e.Spec.Name+".doctor", map[string]any{})
					s.doctor[e.Spec.Name], s.err = id, errText(err)
				}
			}
		}
		for _, e := range engines {
			ui.Row(c.Key("doctor-" + e.Spec.Name)).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, e.Spec.Title).Width(180).SingleLine()
				id := s.doctor[e.Spec.Name]
				switch j, _ := a.be.cond.Job(id); {
				case e.State == conductor.EngineMissing && e.Outdated:
					ui.Text(c, "需要更新环境").FontSize(12).TextColor(t.TextMuted)
				case e.State == conductor.EngineMissing || e.State == conductor.EngineFailed:
					ui.Text(c, "未安装").FontSize(12).TextColor(t.TextMuted)
				case id == "":
					ui.Text(c, "—").FontSize(12).TextColor(t.TextMuted)
				case j.State == conductor.JobDone:
					ui.Text(c, j.Message).FontSize(12).Font("monospace").Selectable()
				case j.State == conductor.JobFailed:
					ui.Text(c, j.Err).FontSize(12).TextColor(t.Danger).Selectable()
				default:
					ui.Spinner(c)
				}
			})
		}
		ui.Text(c, "Windows 上应显示 CUDA 与显卡型号；显示 CPU 时请检查 NVIDIA 驱动。CosyVoice 不使用 Apple MPS。").
			FontSize(12).TextColor(t.TextMuted)
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
			label, action := engineStateLabel[e.State], "安装环境"
			if e.State == conductor.EngineMissing && e.Outdated {
				label, action = "需要更新", "更新环境"
			}
			ui.Text(c, label).FontSize(12).TextColor(t.TextMuted)
			if e.State == conductor.EngineInstalling {
				ui.Spinner(c)
			}
			if (e.State == conductor.EngineMissing || e.State == conductor.EngineFailed) && ui.Button(c, action).Clicked() {
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
