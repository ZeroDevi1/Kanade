// Package fluent 提供 Windows 11 Fluent 风格的设计 token，并据此调整 MyGo 的默认主题。
//
// 颜色取自 WinUI 3 的主题资源（Microsoft.UI.Xaml 的 Common_themeresources_any.xaml），
// 名称与 WinUI 资源键对应，便于对照官方设计稿；macOS 上沿用同一套 token，保证两端外观一致。
package fluent

import "github.com/egoist/mygo/ui"

// Tokens 是一套明或暗模式下的 Fluent 颜色与尺寸。
type Tokens struct {
	// SolidBackground 是不显示 Mica 时窗口的底色（SolidBackgroundFillColorBase）。
	SolidBackground ui.Color
	// Layer 是内容区叠在 Mica 上的半透明层（LayerFillColorDefault）。
	Layer ui.Color
	// Card 是卡片底色（CardBackgroundFillColorDefault），CardStroke 是卡片与内容层描边。
	Card       ui.Color
	CardStroke ui.Color
	// SubtleHover / SubtlePressed 是导航项、透明按钮悬停与按下时的填充
	// （SubtleFillColorSecondary / Tertiary）。
	SubtleHover   ui.Color
	SubtlePressed ui.Color
	// TextPrimary / TextSecondary 对应 TextFillColorPrimary / Secondary。
	TextPrimary   ui.Color
	TextSecondary ui.Color
}

// 圆角取 Fluent 规范：控件 4，卡片和内容层 8。
const (
	ControlRadius = 4
	OverlayRadius = 8
)

var (
	light = Tokens{
		SolidBackground: ui.Hex("#F3F3F3"),
		Layer:           ui.RGBA(255, 255, 255, 0.5),
		Card:            ui.RGBA(255, 255, 255, 0.7),
		CardStroke:      ui.RGBA(0, 0, 0, 0.0578),
		SubtleHover:     ui.RGBA(0, 0, 0, 0.0373),
		SubtlePressed:   ui.RGBA(0, 0, 0, 0.0241),
		TextPrimary:     ui.RGBA(0, 0, 0, 0.8956),
		TextSecondary:   ui.RGBA(0, 0, 0, 0.6063),
	}
	dark = Tokens{
		SolidBackground: ui.Hex("#202020"),
		Layer:           ui.RGBA(58, 58, 58, 0.3),
		Card:            ui.RGBA(255, 255, 255, 0.0512),
		CardStroke:      ui.RGBA(0, 0, 0, 0.1),
		SubtleHover:     ui.RGBA(255, 255, 255, 0.0605),
		SubtlePressed:   ui.RGBA(255, 255, 255, 0.0419),
		TextPrimary:     ui.RGBA(255, 255, 255, 1),
		TextSecondary:   ui.RGBA(255, 255, 255, 0.786),
	}
)

// For 返回与主题明暗一致的 token。
func For(t *ui.Theme) *Tokens {
	if t.Dark {
		return &dark
	}
	return &light
}

// Apply 把当前帧的主题改成 Fluent 风格并返回对应 token。
//
// MyGo 的 Theme 按帧生效，每次构建视图都要在创建任何控件之前调用。
// 强调色不覆盖：默认主题已跟随系统强调色（Windows 个性化设置 / macOS 外观）。
func Apply(c *ui.Context) *Tokens {
	t := *c.Theme()
	tk := For(&t)
	t.Radius = ControlRadius
	t.Text = tk.TextPrimary
	t.TextMuted = tk.TextSecondary
	if !c.Vibrancy() {
		t.Background = tk.SolidBackground
	}
	c.SetTheme(&t)
	if c.Vibrancy() {
		// 根节点不画背景，Mica 才能透出来。
		c.Root().Background(ui.Transparent)
	}
	return tk
}
