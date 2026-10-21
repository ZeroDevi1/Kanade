package main

import (
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// page 按当前导航项构建内容区。
func (a *app) page(c *ui.Context, tk *fluent.Tokens) {
	ui.Column(c).Padding(28, 36).Gap(16).MaxWidth(1000).AlignItems(ui.Stretch).Children(func() {
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

// card 是 Fluent 卡片：半透明底色、细描边、圆角 8，标题使用 Subtitle 层级。
func card(c *ui.Context, tk *fluent.Tokens, title string, body func()) {
	ui.Column(c).Padding(16).Gap(12).AlignItems(ui.Stretch).Radius(fluent.OverlayRadius).
		Background(tk.Card).Border(1, tk.CardStroke).Children(func() {
		ui.Text(c, title).FontSize(16).FontWeight(600)
		body()
	})
}
