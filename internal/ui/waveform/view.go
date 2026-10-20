package waveform

import "github.com/egoist/mygo/ui"

// Playback 是波形读取播放状态、发起跳转的接口，实现须可在 UI 线程调用。
type Playback interface {
	// Progress 返回已播放比例 [0, 1] 以及是否正在播放；播放中波形每帧重绘播放头。
	Progress() (fraction float64, playing bool)
	// Seek 跳到比例 fraction（已裁剪到 [0, 1]）。
	Seek(fraction float64)
}

// columnStep 是每根波形柱的间距（DIP）：2 时每根柱宽约 1.4，留出缝隙接近 DAW 的条形外观；
// 改成 1 则为连续包络，路径顶点数翻倍。
const columnStep = 2

// Show 构建可点击、可拖动跳转的波形元素，高度由调用方设置。
//
// h 须存放在应用状态中，用于在输入处理时读取元素的已提交尺寸；peaks 为 nil 时只画中线。
// pb 可为 nil，此时不显示播放头也不响应跳转。
func Show(c *ui.Context, h *ui.Handle, peaks *Peaks, pb Playback) ui.Element {
	t := c.Theme()
	box := ui.Box(c).Bind(h).Radius(4).Cursor(ui.CursorPointer)

	// 按住时持续跳转，实现拖动擦洗（scrub）。
	if pb != nil && box.Pressed() {
		if x, _, _ := box.PointerPosition(); h.Bounds(c).W > 0 {
			pb.Seek(clamp01(float64(x / h.Bounds(c).W)))
		}
	}

	played, rest := t.Accent, t.TextMuted.Alpha(0.45)
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		mid := r.Y + r.H/2
		if peaks == nil || peaks.Frames == 0 {
			p.Line(r.X, mid, r.X+r.W, mid, 1, rest)
			return
		}
		env := envelope(peaks, r)

		frac, playing := 0.0, false
		if pb != nil {
			frac, playing = pb.Progress()
		}
		split := r.X + r.W*float32(frac)

		// 同一条包络画两次，分别裁剪到已播放 / 未播放区域，避免逐柱判断颜色。
		p.Clip(ui.Rect{X: r.X, Y: r.Y, W: split - r.X, H: r.H}, 0, func() { p.FillPath(env, played) })
		p.Clip(ui.Rect{X: split, Y: r.Y, W: r.X + r.W - split, H: r.H}, 0, func() { p.FillPath(env, rest) })
		if pb != nil {
			p.Fill(ui.Rect{X: split - 1, Y: r.Y, W: 2, H: r.H}, t.Text, 1)
		}
		if playing {
			p.AnimationFrame()
		}
	})
	return box
}

// envelope 把峰值折算成 r 内的柱状路径：每列一个矩形，上沿取最大值、下沿取最小值。
// 最小高度 1 DIP，静音段仍可见一条细线。
func envelope(peaks *Peaks, r ui.Rect) *ui.Path {
	var path ui.Path
	mid, half := r.Y+r.H/2, r.H/2
	cols := int(r.W / columnStep)
	if cols < 1 {
		return &path
	}
	framesPerCol := float64(peaks.Frames) / float64(cols)
	barW := float32(columnStep) * 0.7
	for i := range cols {
		lo, hi := peaks.Range(int(float64(i)*framesPerCol), int(float64(i+1)*framesPerCol))
		top, bot := mid-clamp1(hi)*half, mid-clamp1(lo)*half
		if bot-top < 1 {
			top, bot = mid-0.5, mid+0.5
		}
		x := r.X + float32(i)*columnStep
		path.MoveTo(x, top).LineTo(x+barW, top).LineTo(x+barW, bot).LineTo(x, bot).Close()
	}
	return &path
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

func clamp1(v float32) float32 { return min(max(v, -1), 1) }
