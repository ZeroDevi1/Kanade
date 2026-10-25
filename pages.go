package main

import (
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// 内容列的最大宽度：宽度够时各页的卡片两两并排（输入与结果、两张设置卡片等，见 cardRow），最大化的窗口里
// 不再只占中间一窄条；超宽屏上限 1760（约 2560 宽的窗口），免得并排的表单仍被拉得过长。音乐分析页的卷帘、
// 乐谱、波形与和声色彩随宽度展开，不限宽度（只留页边距）。
const pageMaxWidth = 1760

// cardMinWidth 是并排卡片的最小宽度：表单卡片在约 560 宽时文件名与按钮仍放得下一行；窄于两张的宽度时各占一行
// （1200 宽的默认窗口展开导航时为单列，与原来的布局相同）。
const cardMinWidth = 560

// page 按当前导航项构建内容区。限宽的内容列在宽窗口中水平居中。
func (a *app) page(c *ui.Context, tk *fluent.Tokens) {
	maxW := float32(pageMaxWidth)
	if a.current == "score" {
		maxW = 0
	}
	ui.Row(c).Justify(ui.Center).Children(func() {
		a.pageColumn(c, tk, maxW)
	})
}

// pageColumn 是内容列；maxW 为 0 时不限宽度。
func (a *app) pageColumn(c *ui.Context, tk *fluent.Tokens, maxW float32) {
	col := ui.Column(c).Grow(1).MinWidth(0)
	if maxW > 0 {
		col.MaxWidth(maxW)
	}
	col.Padding(28, 36).Gap(16).AlignItems(ui.Stretch).Children(func() {
		switch a.current {
		case "lab":
			a.labPage(c, tk)
		case "cantus":
			a.cantusPage(c, tk)
		case "timbre":
			a.timbrePage(c, tk)
		case "stems":
			a.stemsPage(c, tk)
		case "score":
			a.scorePage(c, tk)
		case "tasks":
			a.tasksPage(c, tk)
		case "settings":
			a.settingsPage(c, tk)
		default:
			a.homePage(c, tk)
		}
	})
}

// pageHeader 是页面标题（Fluent Title 样式 28/SemiBold）与说明。
func pageHeader(c *ui.Context, title, subtitle string) {
	ui.Column(c).Gap(4).Margin(0, 0, 8, 0).Children(func() {
		ui.Text(c, title).FontSize(28).FontWeight(600)
		ui.Text(c, subtitle).TextColor(c.Theme().TextMuted)
	})
}

// cardRow 把几张卡片放在一行：每张至少 minW 宽、平分宽度并等高；放不下时换行，各占一整行。
// cards 各自构建一张卡片并返回它（card 的返回值）；nil 跳过（例如还没有结果时的概览卡片），剩下的卡片占满整行。
func cardRow(c *ui.Context, minW float32, cards ...func() ui.Element) {
	ui.Row(c).Wrap().Gap(16).AlignItems(ui.Stretch).Children(func() {
		for i, build := range cards {
			if build == nil {
				continue
			}
			ui.Column(c.Key(i)).Basis(minW).Grow(1).MinWidth(0).AlignItems(ui.Stretch).Children(func() {
				build().Grow(1) // 填满被同行较高卡片撑高的格子，使并排的卡片等高
			})
		}
	})
}

// card 是 Fluent 卡片：半透明底色、细描边、圆角 8，标题使用 Subtitle 层级。返回卡片元素，供 cardRow 调整。
func card(c *ui.Context, tk *fluent.Tokens, title string, body func()) ui.Element {
	return ui.Column(c).Padding(16).Gap(12).AlignItems(ui.Stretch).Radius(fluent.OverlayRadius).
		Background(tk.Card).Border(1, tk.CardStroke).Children(func() {
		ui.Text(c, title).FontSize(16).FontWeight(600)
		body()
	})
}
