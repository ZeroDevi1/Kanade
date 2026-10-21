package main

import (
	"github.com/egoist/mygo/ui"
)

// 参数模式：智能模式只给最关键的几个选项，其余由预设与引擎自动决定；专业模式给出全部可调参数。
const (
	modeSmart = iota
	modePro
)

// modeSwitch 切换参数模式，hint 是智能模式下的一句说明（专业模式不显示）。
func modeSwitch(c *ui.Context, key string, mode *int, hint string) {
	ui.Row(c.Key(key)).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Segmented(c, mode, "智能", "专业").Label("参数模式")
		text := hint
		if *mode == modePro {
			text = "显示全部可调参数。"
		}
		ui.Text(c, text).FontSize(12).TextColor(c.Theme().TextMuted).Grow(1)
	})
}

// labeledSlider 是带标签与数值的滑块。format 用于显示当前值，例如 "%+.0f dB"。
func labeledSlider(c *ui.Context, label string, v *float64, lo, hi float64, format string) ui.Element {
	var s ui.Element
	ui.Row(c.Key("slider-" + label)).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).Width(96)
		s = ui.Slider(c, v, lo, hi).Label(label).Width(240)
		ui.Textf(c, format, *v).FontSize(12).Font("monospace").Width(72)
	})
	return s
}

// titles 取出列表中每项的标题，用于下拉框。
func titles[T any](list []T, title func(T) string) []string {
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = title(x)
	}
	return out
}
