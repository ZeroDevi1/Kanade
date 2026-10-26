package main

import (
	"fmt"
	"math"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 五度圆盘的绘制参数：单位圆占绘图区的比例（半径），外圈音名所在的半径，以及最多标注名称的和弦数。
const (
	diskRadius     = 0.40
	diskLabelR     = 0.47
	diskMaxSpots   = 24
	diskMaxNames   = 10
	diskTrailSteps = 6 // 播放时连线显示最近几次换和弦的轨迹
	// curveWindow 是新鲜感曲线的滑动平均窗口（小节）：4 小节约为一个乐句，看得出段落之间的起伏。
	curveWindow = 4
)

// scoreColorCard 是「和声色彩」：五度圆盘上的和弦分布、色彩高点与按小节的新鲜感曲线，与试听同步。
func (a *app) scoreColorCard(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	ca := res.ColorAnalysis()
	if ca == nil {
		return
	}
	pos, seek := a.scorePosition()
	card(c, tk, "和声色彩", func() {
		ui.Text(c, "每个和弦按其音在五度圈上的合成方向与集中程度放在圆盘上：越靠外越协和、色彩越鲜明，越靠中心张力越大；"+
			"主和弦朝上，右侧为升号方向（明亮），左侧为降号方向（暗淡）。新鲜感 = 色差（与前一和弦的距离）+ 张力差。"+
			"思路参考华萃康「色彩和声」，度量为五度圈傅里叶分量。").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(20).AlignItems(ui.Start).Children(func() {
			a.fifthsDisk(c, res, ca, pos)
			ui.Column(c).Grow(1).Gap(8).AlignItems(ui.Stretch).Children(func() {
				colorSummary(c, ca)
				ui.Text(c, "色彩高点（点击跳转）").FontSize(12).FontWeight(600)
				for i, p := range ca.Peaks(6) {
					b := ui.ButtonBase(c.Key(fmt.Sprintf("peak-%d", i))).Padding(4, 8).Radius(fluent.ControlRadius).
						AlignItems(ui.Stretch).Label(fmt.Sprintf("%s %s 到 %s", score.Clock(p.Time), p.From, p.To))
					if b.Hovered() {
						b.Background(tk.SubtleHover)
					}
					b.Children(func() {
						ui.Row(c).Grow(1).Gap(10).AlignItems(ui.Center).Children(func() {
							ui.Text(c, score.Clock(p.Time)).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(40)
							ui.Text(c, p.From+" → "+p.To).FontSize(13).Grow(1).SingleLine()
							ui.Textf(c, "%.2f", p.Freshness).FontSize(12).Font("monospace").TextColor(t.TextMuted)
						})
					})
					if b.Clicked() {
						seek(p.Time)
					}
				}
			})
		})
		ui.Text(c, "新鲜感随时间的变化").FontSize(12).FontWeight(600)
		colorCurve(c, res, ca, pos)
	})
}

// colorSummary 显示整体统计：平均新鲜感、色差与张力差各自的占比。
func colorSummary(c *ui.Context, ca *score.ColorAnalysis) {
	t := c.Theme()
	if len(ca.Steps) == 0 {
		ui.Text(c, "和弦没有变化。").FontSize(12).TextColor(t.TextMuted)
		return
	}
	var fr, col, ten float64
	for _, s := range ca.Steps {
		fr, col, ten = fr+s.Freshness, col+s.Color, ten+s.Tension
	}
	n := float64(len(ca.Steps))
	ui.Textf(c, "%d 次换和弦 · 平均新鲜感 %.2f", len(ca.Steps), fr/n).FontSize(13)
	share := 0.0
	if fr > 0 {
		share = col / fr
	}
	ui.Textf(c, "新鲜感中色差占 %.0f%%、张力差占 %.0f%%", share*100, (1-share)*100).FontSize(12).TextColor(t.TextMuted)
	if share > 0.9 {
		ui.Text(c, "和弦以三和弦为主，变化主要来自色彩；在专业模式选择含七和弦的词汇可看到更多张力变化。").FontSize(12).TextColor(t.TextMuted)
	}
}

// diskPos 把单位圆坐标映射到绘图区的比例坐标。
func diskPos(x, y float64, r float64) (float32, float32) {
	return float32(0.5 + x*r), float32(0.5 + y*r)
}

// fifthsDisk 绘制五度圆盘：外圈 12 个音名、强度 0.5 的参考圆、按总时长定大小的和弦点、
// 当前和弦高亮及最近几次换和弦的轨迹。
func (a *app) fifthsDisk(c *ui.Context, res *score.Analysis, ca *score.ColorAnalysis, pos float64) {
	t := c.Theme()
	spots := ca.Spots()
	if len(spots) > diskMaxSpots {
		spots = spots[:diskMaxSpots]
	}
	maxSec := spots[0].Seconds
	cur := -1
	for i, p := range ca.Points {
		if pos >= p.Start && pos < p.End {
			cur = i
		}
	}
	ui.Column(c).Gap(2).Shrink(0).Width(320).Children(func() {
		a.fifthsDiskPlot(c, res, ca, spots, maxSec, cur)
		ui.Row(c).Children(func() {
			ui.Text(c, "← 暗淡（降号方向）").FontSize(11).TextColor(t.TextMuted)
			ui.Box(c).Grow(1)
			ui.Text(c, "明亮（升号方向）→").FontSize(11).TextColor(t.TextMuted)
		})
	})
}

// fifthsDiskPlot 是五度圆盘的绘图区。
func (a *app) fifthsDiskPlot(c *ui.Context, res *score.Analysis, ca *score.ColorAnalysis, spots []score.ChordSpot, maxSec float64, cur int) {
	t := c.Theme()
	chart.Plot(c, chart.Axes{}, func(area ui.Element) {
		area.Draw(func(p *ui.Painter, r ui.Rect) {
			cx, cy := r.X+r.W/2, r.Y+r.H/2
			for _, k := range []float32{1, 0.5} {
				rad := r.W * diskRadius * k
				p.Stroke(ui.Rect{X: cx - rad, Y: cy - rad, W: rad * 2, H: rad * 2}, t.Border, rad, 1)
			}
			p.Line(cx, r.Y+r.H*(0.5-diskRadius), cx, r.Y+r.H*(0.5+diskRadius), 1, t.Border.Alpha(0.5))
			p.Line(r.X+r.W*(0.5-diskRadius), cy, r.X+r.W*(0.5+diskRadius), cy, 1, t.Border.Alpha(0.5))
			// 播放轨迹：当前和弦之前的几次换和弦依次连线，越近越不透明。
			if cur > 0 {
				from := max(cur-diskTrailSteps, 0)
				for i := from; i < cur; i++ {
					p0, p1 := ca.Points[i], ca.Points[i+1]
					x0, y0 := diskPos(p0.X, p0.Y, diskRadius)
					x1, y1 := diskPos(p1.X, p1.Y, diskRadius)
					alpha := float32(i-from+1) / float32(cur-from+1)
					p.Line(chart.X(r, x0), chart.Y(r, y0), chart.X(r, x1), chart.Y(r, y1), 2, t.Accent.Alpha(alpha))
				}
			}
		})
		for _, l := range res.RingLabels() {
			x, y := diskPos(l.X, l.Y, diskLabelR)
			chart.Anchor(c, x, y, ui.Center, ui.Center, func() {
				ui.Text(c, l.Label).FontSize(11).TextColor(t.TextMuted).NoWrap()
			})
		}
		for i, s := range spots {
			size := float32(8 + 20*math.Sqrt(s.Seconds/maxSec))
			x, y := diskPos(s.X, s.Y, diskRadius)
			dot := chart.Point(c, x, y, size, size).Radius(size / 2).Background(t.Accent.Alpha(0.45)).
				Tooltip(fmt.Sprintf("%s · 集中度 %.2f · 共 %s", s.Label, s.Strength, score.Clock(s.Seconds)))
			if cur >= 0 && math.Hypot(ca.Points[cur].X-s.X, ca.Points[cur].Y-s.Y) < 1e-6 {
				dot.Background(t.Accent).Border(2, t.AccentText)
			}
			if i < diskMaxNames {
				chart.Anchor(c, x, y, ui.Start, ui.End, func() {
					ui.Text(c, s.Label).FontSize(11).NoWrap().Margin(0, 0, 2, 0).Padding(0, 0, 0, size/2)
				})
			}
		}
	}).Width(320).Height(320).Shrink(0).Label("五度圆盘")
}

// colorCurve 按小节画新鲜感（及其中的色差、张力差）曲线，竖线标出播放位置。
func colorCurve(c *ui.Context, res *score.Analysis, ca *score.ColorAnalysis, pos float64) {
	t := c.Theme()
	n := len(ca.BarFreshness)
	if n < 2 {
		ui.Text(c, "没有小节线，无法按小节绘制。").FontSize(12).TextColor(t.TextMuted)
		return
	}
	hi := 0.0
	for _, v := range ca.BarFreshness {
		hi = max(hi, v)
	}
	y := chart.NewLinear(0, max(hi, 0.5)).Nice(3)
	// 主线是 curveWindow 小节的滑动平均（段落级趋势），逐小节的原始新鲜感淡色画在后面。
	sf, sc, st := score.Smooth(ca.BarFreshness, curveWindow), score.Smooth(ca.BarColor, curveWindow), score.Smooth(ca.BarTension, curveWindow)
	xs := make([]float32, n)
	raw, fr, col, ten := make([]float32, n), make([]float32, n), make([]float32, n), make([]float32, n)
	for i := range n {
		xs[i] = float32(i) / float32(n-1)
		raw[i], fr[i], col[i], ten[i] = y.At(ca.BarFreshness[i]), y.At(sf[i]), y.At(sc[i]), y.At(st[i])
	}
	bars := res.Bars()
	chart.Plot(c, chart.Axes{Left: y.Ticks(3)}, func(area ui.Element) {
		area.Draw(func(p *ui.Painter, r ui.Rect) {
			for _, tick := range y.Ticks(3) {
				p.Line(r.X, chart.Y(r, tick.At), r.X+r.W, chart.Y(r, tick.At), 1, t.Border.Alpha(0.5))
			}
			p.StrokePath(chart.Line(r, xs, raw, chart.CurveLinear), 1, t.Accent.Alpha(0.25))
			p.StrokePath(chart.Line(r, xs, ten, chart.CurveMonotone), 1.5, t.Warning.Alpha(0.8))
			p.StrokePath(chart.Line(r, xs, col, chart.CurveMonotone), 1.5, t.Success.Alpha(0.8))
			p.StrokePath(chart.Line(r, xs, fr, chart.CurveMonotone), 2, t.Accent)
		})
		if pos >= 0 {
			for i, b := range bars {
				if pos >= b.Start && pos < b.End {
					chart.RuleX(c, float32(i)/float32(n-1), 1).Background(t.Danger.Alpha(0.7))
				}
			}
		}
	}).Height(150).Gap(6).Label("每小节的新鲜感")
	ui.Row(c).Gap(16).Children(func() {
		legend := func(label string, color ui.Color) {
			ui.Row(c.Key("legend-" + label)).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Box(c).Size(12, 3).Background(color)
				ui.Text(c, label).FontSize(12).TextColor(t.TextMuted)
			})
		}
		legend(fmt.Sprintf("新鲜感（%d 小节平均）", curveWindow), t.Accent)
		legend("逐小节", t.Accent.Alpha(0.25))
		legend("色差", t.Success)
		legend("张力差", t.Warning)
		ui.Textf(c, "横轴：第 1–%d 小节", n).FontSize(12).TextColor(t.TextMuted)
	})
}
