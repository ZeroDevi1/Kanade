package main

import (
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
)

// page 按当前导航项构建内容区。M0 中除验证页外均为占位页。
func (a *app) page(c *ui.Context, tk *fluent.Tokens) {
	ui.Column(c).Padding(28, 36).Gap(16).MaxWidth(1000).AlignItems(ui.Stretch).Children(func() {
		switch a.current {
		case "lab":
			a.labPage(c, tk)
		case "cantus":
			placeholder(c, tk, "语音生成", "Cantus · CosyVoice 零样本克隆与指令合成。")
		case "timbre":
			placeholder(c, tk, "音色转换", "Timbre · RVC 歌曲与纯人声音色转换。")
		case "stems":
			placeholder(c, tk, "分轨", "Stems · MDX / Demucs / BS-RoFormer 源分离。")
		case "score":
			placeholder(c, tk, "音乐分析", "Score · 调号、拍号、BPM、和弦与情绪识别。")
		case "settings":
			placeholder(c, tk, "设置", "数据根目录、uv 与推理引擎、模型镜像源。")
		default:
			placeholder(c, tk, "主页", "最近项目与任务队列。")
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

func placeholder(c *ui.Context, tk *fluent.Tokens, title, subtitle string) {
	pageHeader(c, title, subtitle)
	card(c, tk, "尚未实现", func() {
		ui.Text(c, "该模块在后续里程碑中接入 Conductor 推理引擎。").TextColor(c.Theme().TextMuted)
	})
}
