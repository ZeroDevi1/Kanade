package score

import (
	"math"
	"slices"
	"strings"
)

// 和声色彩：把每个和弦放到「五度圆盘」上，并计算相邻和弦之间的色差、张力差与新鲜感。
//
// 思路参考华萃康「色彩和声」：和声进行的新鲜感来自和弦在五度圈上方向的变化（色彩）与紧张度的变化。
// 这里的度量是独立实现的，不采用其紧张度分级表：
//
//   - 位置：和弦音级集合的第 5 个离散傅里叶系数（f₅，见 Quinn 2006「General equal-tempered harmony」、
//     Amiot 2016《Music Through Fourier Space》）。每个音按五度圈位置取单位向量，求平均；
//     方向即和弦在五度圈上的整体方向，模长（0–1）是各音在五度圈上的集中程度。
//     纯五度 0.97、挂留和弦 0.91、大小三和弦 0.64、七和弦 0.48、减三 0.33、增三 0：越集中越协和、色彩越鲜明，
//     越靠近圆心张力越大、方向感越弱。
//   - 朝向：整体旋转到主和弦朝正上方；顺时针（右侧）为升号方向，听感更明亮，逆时针（左侧）为降号方向，更暗淡。
//   - 色差 = 相邻两和弦位置的距离（0–2）；张力差 = 两者模长之差的绝对值（0–1）；新鲜感 = 色差 + 张力差。

// qualityIntervals 是和弦性质的音程（相对根音的半音数），与引擎 harmony.QUALITIES 一致。
var qualityIntervals = map[string][]int{
	"maj": {0, 4, 7}, "min": {0, 3, 7}, "7": {0, 4, 7, 10}, "maj7": {0, 4, 7, 11},
	"min7": {0, 3, 7, 10}, "dim": {0, 3, 6}, "sus4": {0, 5, 7},
}

// PitchClasses 返回和弦的音级集合（0 = C）；未知的性质返回 nil。
func PitchClasses(root int, quality string) []int {
	iv, ok := qualityIntervals[quality]
	if !ok {
		return nil
	}
	out := make([]int, len(iv))
	for i, x := range iv {
		out[i] = (root + x) % 12
	}
	return out
}

// fifthsVector 是音级集合在五度圈上的平均单位向量（未旋转）：音级 pc 在五度圈上的位置为 pc×7 mod 12，
// 每格 30°，从正上方顺时针（x = sin，y = cos）。
func fifthsVector(pcs []int) (x, y float64) {
	if len(pcs) == 0 {
		return 0, 0
	}
	for _, pc := range pcs {
		a := float64((pc*7)%12) * math.Pi / 6
		x += math.Sin(a)
		y += math.Cos(a)
	}
	return x / float64(len(pcs)), y / float64(len(pcs))
}

// ChordPoint 是一个和弦在五度圆盘上的位置（已旋转到主和弦朝上）。
type ChordPoint struct {
	Segment
	X, Y float64 // 单位圆内的坐标：Y 朝主和弦，X 正为升号方向（明亮）
	// Strength 是集中程度（模长，0–1）：越大越协和、色彩越鲜明；1 − Strength 即张力。
	Strength float64
}

// ColorStep 是一次换和弦的色彩变化。
type ColorStep struct {
	Time      float64 // 换和弦的时刻（秒）
	From, To  string
	Color     float64 // 色差：两和弦位置的距离（0–2）
	Tension   float64 // 张力差：模长之差的绝对值（0–1）
	Freshness float64 // 新鲜感 = 色差 + 张力差
}

// ColorAnalysis 是一首歌的和声色彩分析。
type ColorAnalysis struct {
	Points []ChordPoint // 按时间顺序，不含无和弦（N）的段
	Steps  []ColorStep  // 相邻和弦之间的变化，len(Steps) == len(Points) - 1
	// BarFreshness 是每小节内新鲜感之和（下标与 Bars() 一致），用于画随时间变化的曲线。
	BarFreshness, BarColor, BarTension []float64
}

// rotation 返回把主和弦方向旋转到正上方的角度（弧度）。主和弦取调号的大三或小三和弦。
func (a *Analysis) rotation() float64 {
	q := "maj"
	if a.Key.Mode == "minor" {
		q = "min"
	}
	x, y := fifthsVector(PitchClasses(a.Key.TonicPC, q))
	return math.Atan2(x, y)
}

// ColorAnalysis 计算和声色彩。没有和弦信息（旧版结果缺少 root / quality）时返回 nil。
func (a *Analysis) ColorAnalysis() *ColorAnalysis {
	rot := a.rotation()
	sin, cos := math.Sin(-rot), math.Cos(-rot)
	ca := &ColorAnalysis{}
	for _, c := range a.Chords {
		if c.Root == nil {
			continue
		}
		pcs := PitchClasses(*c.Root, c.Quality)
		if pcs == nil {
			continue
		}
		x, y := fifthsVector(pcs)
		// 旋转 −rot（坐标系为从正上方顺时针，旋转公式与常规的逆时针相反）。
		rx, ry := x*cos+y*sin, -x*sin+y*cos
		ca.Points = append(ca.Points, ChordPoint{Segment: c, X: rx, Y: ry, Strength: math.Hypot(x, y)})
	}
	if len(ca.Points) == 0 {
		return nil
	}
	for i := 1; i < len(ca.Points); i++ {
		p, q := ca.Points[i-1], ca.Points[i]
		col := math.Hypot(q.X-p.X, q.Y-p.Y)
		ten := math.Abs(q.Strength - p.Strength)
		ca.Steps = append(ca.Steps, ColorStep{Time: q.Start, From: p.Label, To: q.Label, Color: col, Tension: ten, Freshness: col + ten})
	}
	bars := a.Bars()
	ca.BarFreshness = make([]float64, len(bars))
	ca.BarColor = make([]float64, len(bars))
	ca.BarTension = make([]float64, len(bars))
	for _, s := range ca.Steps {
		i, _ := slices.BinarySearchFunc(bars, s.Time, func(b Bar, t float64) int {
			switch {
			case b.End <= t:
				return -1
			case b.Start > t:
				return 1
			}
			return 0
		})
		if i < len(bars) {
			ca.BarFreshness[i] += s.Freshness
			ca.BarColor[i] += s.Color
			ca.BarTension[i] += s.Tension
		}
	}
	return ca
}

// ChordSpot 是五度圆盘上一个和弦名的汇总位置：同名和弦位置相同，按总时长加权显示大小。
type ChordSpot struct {
	Label    string
	X, Y     float64
	Strength float64
	Seconds  float64 // 该和弦在全曲中的总时长
}

// Spots 按位置汇总和弦，时长降序。位置相同的不同和弦（例如大三和弦与高大二度的小三和弦，f₅ 相同）
// 合并为一个点，名称以「 / 」连接，按各自时长降序排列。
func (ca *ColorAnalysis) Spots() []ChordSpot {
	type spot struct {
		ChordSpot
		names map[string]float64
	}
	var out []*spot
	for _, p := range ca.Points {
		var hit *spot
		for _, s := range out {
			if math.Hypot(s.X-p.X, s.Y-p.Y) < 1e-6 {
				hit = s
				break
			}
		}
		if hit == nil {
			hit = &spot{ChordSpot{X: p.X, Y: p.Y, Strength: p.Strength}, map[string]float64{}}
			out = append(out, hit)
		}
		d := p.End - p.Start
		hit.Seconds += d
		hit.names[p.Label] += d
	}
	res := make([]ChordSpot, len(out))
	for i, s := range out {
		names := make([]string, 0, len(s.names))
		for n := range s.names {
			names = append(names, n)
		}
		slices.SortStableFunc(names, func(a, b string) int {
			if s.names[a] != s.names[b] {
				if s.names[a] > s.names[b] {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		s.Label = strings.Join(names, " / ")
		res[i] = s.ChordSpot
	}
	out2 := res
	slices.SortStableFunc(out2, func(a, b ChordSpot) int {
		switch {
		case a.Seconds > b.Seconds:
			return -1
		case a.Seconds < b.Seconds:
			return 1
		}
		return 0
	})
	return out2
}

// Smooth 返回 v 的居中滑动平均（窗口 w 个元素，两端按实际覆盖的元素数平均），用于看段落级的趋势。
func Smooth(v []float64, w int) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		lo, hi := max(i-w/2, 0), min(i+(w+1)/2, len(v))
		sum := 0.0
		for _, x := range v[lo:hi] {
			sum += x
		}
		out[i] = sum / float64(hi-lo)
	}
	return out
}

// Peaks 返回新鲜感最高的 n 次换和弦（按时间排序），用于提示全曲的色彩高点。
func (ca *ColorAnalysis) Peaks(n int) []ColorStep {
	s := slices.Clone(ca.Steps)
	slices.SortStableFunc(s, func(a, b ColorStep) int {
		switch {
		case a.Freshness > b.Freshness:
			return -1
		case a.Freshness < b.Freshness:
			return 1
		}
		return 0
	})
	s = s[:min(n, len(s))]
	slices.SortFunc(s, func(a, b ColorStep) int {
		switch {
		case a.Time < b.Time:
			return -1
		case a.Time > b.Time:
			return 1
		}
		return 0
	})
	return s
}

// PitchName 返回音级名；flats 为 true 时用降号拼写，与引擎一致。
func PitchName(pc int, flats bool) string {
	sharp := []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
	flat := []string{"C", "Db", "D", "Eb", "E", "F", "Gb", "G", "Ab", "A", "Bb", "B"}
	if flats {
		return flat[((pc%12)+12)%12]
	}
	return sharp[((pc%12)+12)%12]
}

// RingLabels 返回五度圆盘外圈 12 个音名及其旋转后的位置（单位圆上），与 ColorAnalysis 的朝向一致。
func (a *Analysis) RingLabels() []ChordSpot {
	rot := a.rotation()
	out := make([]ChordSpot, 12)
	for k := 0; k < 12; k++ {
		pc := (k * 7) % 12
		ang := float64(k)*math.Pi/6 - rot
		out[k] = ChordSpot{Label: PitchName(pc, a.Key.Flats), X: math.Sin(ang), Y: math.Cos(ang)}
	}
	return out
}
