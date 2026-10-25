package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// 尺寸取 WinUI NavigationView 的默认值：标题栏 48、展开导航 280、紧凑导航 48。
const (
	titleBarHeight = 48
	navWidth       = 280
	navCompact     = 48
)

// navItem 是导航栏的一项；id 同时是页面标识。
type navItem struct {
	id, label string
	icon      *ui.SVG
}

var (
	navMain = []navItem{
		{"home", "主页", iconHome},
		{"cantus", "语音生成", iconCantus},
		{"timbre", "音色转换", iconTimbre},
		{"stems", "分轨", iconStems},
		{"score", "音乐分析", iconScore},
		{"inspire", "灵感", iconInspire},
	}
	navFooter = []navItem{
		{"tasks", "任务", iconTasks},
		{"lab", "M0 验证", iconLab},
		{"settings", "设置", iconSettings},
	}
)

// view 是窗口根视图：Fluent 主题、标题栏、导航栏与内容层。
func (a *app) view(c *ui.Context) {
	tk := fluent.Apply(c)
	ui.Column(c).Fill().Bind(&a.root).AlignItems(ui.Stretch).Children(func() {
		a.titleBar(c)
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			a.nav(c, tk)
			// 内容层：叠在 Mica 上的半透明层，左上角圆角、上左两边描边，与 WinUI NavigationView 一致。
			ui.Column(c).Grow(1).MinWidth(0).AlignItems(ui.Stretch).
				Background(tk.Layer).Radius(fluent.OverlayRadius, 0, 0, 0).
				BorderWidth(1, 0, 0, 1).BorderColor(tk.CardStroke).Clip().
				Children(func() {
					ui.Scroll(c).Grow(1).Children(func() { a.page(c, tk) })
					if a.current == "score" {
						a.scoreDock(c, tk)
					}
				})
		})
	})
}

// titleBar 绘制隐藏系统标题栏后的自绘标题栏，避开系统窗口按钮并可拖动窗口。
func (a *app) titleBar(c *ui.Context) {
	t := c.Theme()
	bar := c.TitleBar()
	ui.Row(c).Height(titleBarHeight).Shrink(0).AlignItems(ui.Center).Gap(12).
		Padding(0, bar.Right+8, 0, bar.Left+4).DragWindow().Children(func() {
		toggle := ui.ButtonBase(c.Key("nav-toggle")).Size(40, 36).Radius(fluent.ControlRadius).Center().
			Label("切换导航栏").Tooltip("切换导航栏")
		tk := fluent.For(t)
		if toggle.Pressed() {
			toggle.Background(tk.SubtlePressed)
		} else if toggle.Hovered() {
			toggle.Background(tk.SubtleHover)
		}
		toggle.Children(func() { ui.Icon(c, iconMenu).Size(16, 16) })
		if toggle.Clicked() {
			a.navCollapsed = !a.navCollapsed
		}
		ui.Text(c, "Kanade").FontSize(12)
	})
}

// nav 绘制 Fluent 风格导航栏：主项在上、页脚项在下，宽度在展开与紧凑之间动画切换。
func (a *app) nav(c *ui.Context, tk *fluent.Tokens) {
	target := float32(navWidth)
	if a.navCollapsed {
		target = navCompact
	}
	col := ui.Column(c).Shrink(0).Padding(0, 4, 4, 4).Gap(4).AlignItems(ui.Stretch).Clip()
	col.Width(col.Animate("nav-width", target, 200*time.Millisecond))
	col.Children(func() {
		for _, it := range navMain {
			a.navButton(c, tk, it)
		}
		ui.Box(c).Grow(1)
		for _, it := range navFooter {
			// 「M0 验证」是开发期的框架验证页，正式构建（mygo build）中不显示。
			if it.id == "lab" && !mygo.IsDev() {
				continue
			}
			a.navButton(c, tk, it)
		}
	})
}

// navButton 是一项导航：选中时显示左侧强调色指示条与浅色填充，紧凑模式只显示图标并以提示框给出名称。
func (a *app) navButton(c *ui.Context, tk *fluent.Tokens, it navItem) {
	t := c.Theme()
	selected := a.current == it.id
	b := ui.ButtonBase(c.Key("nav-"+it.id)).Height(36).Radius(fluent.ControlRadius).
		Label(it.label).AlignItems(ui.Center).Padding(0, 12)
	switch {
	case b.Pressed():
		b.Background(tk.SubtlePressed)
	case selected || b.Hovered():
		b.Background(tk.SubtleHover)
	}
	if a.navCollapsed {
		label := it.label
		if n := a.activeJobs(); it.id == "tasks" && n > 0 {
			label = fmt.Sprintf("%s（%d 个进行中）", label, n)
		}
		b.Tooltip(label)
	}
	b.Children(func() {
		ui.Row(c).Grow(1).AlignItems(ui.Center).Gap(16).Children(func() {
			ui.Icon(c, it.icon).Size(16, 16).Shrink(0)
			ui.Text(c, it.label).FontSize(14).SingleLine().Grow(1)
			// 任务项显示进行中的任务数；紧凑导航时文字被裁掉，数量放在提示框里。
			if n := a.activeJobs(); it.id == "tasks" && n > 0 && !a.navCollapsed {
				ui.Badge(c, fmt.Sprint(n))
			}
		})
	})
	if selected {
		// 指示条位于按钮左缘、垂直居中，3×16，与 WinUI 选中态一致。
		b.DrawOver(func(p *ui.Painter, r ui.Rect) {
			p.Fill(ui.Rect{X: r.X, Y: r.Y + (r.H-16)/2, W: 3, H: 16}, t.Accent, 1.5)
		})
	}
	if b.Clicked() {
		a.current = it.id
	}
}

// activeJobs 返回尚未结束的任务数；后端未启动时为 0。
func (a *app) activeJobs() int {
	if a.be.cond == nil {
		return 0
	}
	n := 0
	for _, j := range a.be.cond.Jobs() {
		if !j.State.Finished() {
			n++
		}
	}
	return n
}
