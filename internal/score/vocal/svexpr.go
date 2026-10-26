package vocal

import (
	"math"
	"slices"

	"kanade/internal/score"
	"kanade/internal/score/stat"
	"kanade/internal/score/synth"
)

// 把主唱的表情特征（Expression）换算成 Synthesizer V 的参数曲线。各曲线只在音符范围内写点
// （音符之外 SV 不发声），音符之间的空隙长于 svGapTicks 时在两端补默认值的点，免得 SV 跨过空隙连成长斜线。
//
// 单位与范围（SV 脚本 API 的参数定义）：pitchDelta 音分（±1200），loudness dB（−48–12），tension / breathiness
// −1–1，voicing 0–1（默认 1）。
//
//   - pitchDelta：帧基频相对所在音符音高的音分偏差（八度误差折回 ±600 内，超过 ±500 的视为跟踪错误丢弃），
//     5 帧中值滤波去掉毛刺，每 20 ms 一点。配合音符上关闭 SV 自身的颤音与滑音（svPitchAttributes），
//     「音符音高 + pitchDelta」即原唱的音高，转音、颤音与滑音都来自原唱。
//   - loudness：乐句级的响度起伏：帧 RMS 平滑 250 ms 后相对整首人声中位数的差，只取一半（SV 本身已按音高与
//     音素生成动态，原样叠加会把起伏放大），限制在 −6–+3 dB。最初按 90 百分位为 0 dB、下限 −24 dB 时，
//     渲染出的人声比原唱整体低约 16 dB、起伏从 9 dB 放大到 16 dB（2026-10-24《我不想念》、Weina 实测），
//     乐句头尾被压得发闷，与张力、Firm 模式一起听起来像憋着唱。
//   - breathiness：高频噪声比与非周期性（1 − 周期性）的稳健 z 分数（按整首有声帧的中位数与分位距归一化），
//     加权后经 tanh 压到 ±1。
//   - tension：频谱倾斜（1.5–5 kHz 对 80 Hz–1.5 kHz）的稳健 z 分数经 tanh 压到 ±1：声音越亮越「紧」。
//   - voicing：平滑后的周期性低于 0.6 时按比例降低（最多降 0.6），对应耳语、气声段落。
// 每条曲线再乘以预设中的强度；强度为 0 的曲线不写。这些换算是声学上的经验近似，不是 SV 参数的精确反演。

const (
	svPitchStride = 2  // pitchDelta 每 2 帧（20 ms）一点
	svCurveStride = 5  // 其余曲线每 5 帧（50 ms）一点
	svGapTicks    = 48 // 音符间空隙长于此（synth.PPQ tick，约 1/10 拍）时在两端补默认值
	// 响度曲线（见文件头）：平滑窗 25 帧（250 ms，跟乐句而不跟辅音与换气）、起伏取一半、范围 −6–+3 dB。
	svLoudSmooth = 25
	svLoudScale  = 0.5
	svLoudMin    = -6.0
	svLoudMax    = 3.0
	// svVoicedPeriodicity 是统计「有声帧」与采用基频的周期性下限（经验值：清音与噪声帧多在 0.3 以下）。
	svVoicedPeriodicity = 0.45
)

// svCurves 是各参数曲线（键为 SV 的参数名）。
type svCurves map[string]svpCurve

// expressionCurves 按预设 p 的强度生成参数曲线。notes 的音高是移调前的（与原唱基频比较）。
func expressionCurves(notes []vocalNote, tm *synth.TimeMap, e *score.Expression, p SVPreset) svCurves {
	n := len(e.F0)
	if n == 0 || len(notes) == 0 {
		return nil
	}
	// 每帧所在的音符（-1 为不在音符内）与 tick。
	tick := make([]int, n)
	owner := make([]int, n)
	k := 0
	for i := range n {
		tick[i] = tm.Tick(float64(i) * e.Hop)
		for k < len(notes) && notes[k].off <= tick[i] {
			k++
		}
		owner[i] = -1
		if k < len(notes) && notes[k].on <= tick[i] {
			owner[i] = k
		}
	}
	// 有声帧：用于整首的统计（响度参考、z 分数）。
	var voiced []int
	for i := range n {
		if owner[i] >= 0 && e.Periodicity[i] >= svVoicedPeriodicity && e.Loudness[i] > -60 {
			voiced = append(voiced, i)
		}
	}
	if len(voiced) < 10 {
		return nil
	}
	pick := func(xs []float64) []float64 {
		out := make([]float64, len(voiced))
		for j, i := range voiced {
			out[j] = xs[i]
		}
		return out
	}

	out := svCurves{}
	emit := func(name string, stride int, def float64, value func(i int) (float64, bool)) {
		out[name] = curveOverNotes(notes, tick, owner, stride, def, value)
	}
	if s := p.PitchStrength; s > 0 {
		cents := pitchCents(notes, owner, e)
		emit("pitchDelta", svPitchStride, 0, func(i int) (float64, bool) {
			return cents[i] * s, !math.IsNaN(cents[i])
		})
	}
	if s := p.DynamicsStrength; s > 0 {
		loud := smooth(e.Loudness, svLoudSmooth)
		ref := stat.Percentile(pick(loud), 0.5)
		emit("loudness", svCurveStride, 0, func(i int) (float64, bool) {
			return stat.Clamp((loud[i]-ref)*svLoudScale*s, svLoudMin, svLoudMax), true
		})
	}
	if s := p.BreathStrength; s > 0 {
		noise, per := smooth(e.Noise, 15), smooth(e.Periodicity, 15)
		zn, zp := robustZ(noise, pick(noise)), robustZ(per, pick(per))
		emit("breathiness", svCurveStride, 0, func(i int) (float64, bool) {
			return math.Tanh((0.6*zn(i)-0.4*zp(i))/1.2) * s, true // 周期性越低越「气」，所以减去
		})
	}
	if s := p.TensionStrength; s > 0 {
		tilt := smooth(e.Tilt, 15)
		zt := robustZ(tilt, pick(tilt))
		emit("tension", svCurveStride, 0, func(i int) (float64, bool) { return math.Tanh(zt(i)/1.2) * s, true })
	}
	if s := p.VoicingStrength; s > 0 {
		per := smooth(e.Periodicity, 21)
		emit("voicing", svCurveStride, 1, func(i int) (float64, bool) {
			return 1 - s*0.6*stat.Clamp((0.6-per[i])/0.35, 0, 1), true
		})
	}
	return out
}

// pitchCents 返回每帧相对所在音符的音分偏差（不在音符内或不可信的帧为 NaN），已在每个音符内做 5 帧中值滤波，
// 音符内不可信的帧沿用前一个可信值（音符开头不可信时沿用之后第一个可信值）。
func pitchCents(notes []vocalNote, owner []int, e *score.Expression) []float64 {
	n := len(e.F0)
	raw := make([]float64, n)
	for i := range n {
		raw[i] = math.NaN()
		k := owner[i]
		if k < 0 || e.F0[i] <= 0 || e.Periodicity[i] < svVoicedPeriodicity {
			continue
		}
		c := 1200 * math.Log2(e.F0[i]/synth.MIDIFreq(notes[k].pitch))
		c -= 1200 * math.Round(c/1200) // 八度误差折回 ±600
		if math.Abs(c) <= 500 {
			raw[i] = c
		}
	}
	out := make([]float64, n)
	for i := 0; i < n; {
		k := owner[i]
		j := i
		for j < n && owner[j] == k {
			j++
		}
		// [i, j) 属于同一个音符（或同一段空隙）。
		seg := out[i:j]
		for x := range seg {
			seg[x] = math.NaN()
		}
		if k >= 0 {
			for x := i; x < j; x++ {
				var win []float64
				for y := max(i, x-2); y < min(j, x+3); y++ {
					if !math.IsNaN(raw[y]) {
						win = append(win, raw[y])
					}
				}
				if !math.IsNaN(raw[x]) && len(win) > 0 {
					slices.Sort(win)
					out[x] = win[len(win)/2]
				}
			}
			holdGaps(seg)
		}
		i = j
	}
	return out
}

// holdGaps 把 seg 中的 NaN 换成前一个有效值（开头的换成第一个有效值）；全为 NaN 时不变。
func holdGaps(seg []float64) {
	first := slices.IndexFunc(seg, func(v float64) bool { return !math.IsNaN(v) })
	if first < 0 {
		return
	}
	prev := seg[first]
	for x := range seg {
		if math.IsNaN(seg[x]) {
			seg[x] = prev
		} else {
			prev = seg[x]
		}
	}
}

// curveOverNotes 在音符内每 stride 帧写一点（value 返回 false 的帧跳过），每个音符的第一帧与最后一帧都写；
// 相邻音符之间的空隙长于 svGapTicks 时，在前一个音符结束处与后一个音符开始前写默认值 def。
func curveOverNotes(notes []vocalNote, tick, owner []int, stride int, def float64, value func(i int) (float64, bool)) svpCurve {
	c := svpCurve{Mode: "cubic", Points: []float64{}}
	last := math.MinInt
	add := func(t int, v float64) {
		if t <= last {
			return // SV 要求点的位置严格递增
		}
		c.Points = append(c.Points, float64(int64(t)*svpBlicksPerTick), math.Round(v*1000)/1000)
		last = t
	}
	n := len(owner)
	for i := 0; i < n; {
		k := owner[i]
		j := i
		for j < n && owner[j] == k {
			j++
		}
		if k >= 0 {
			if k > 0 && notes[k].on-notes[k-1].off > svGapTicks {
				add(notes[k].on-svGapTicks/2, def)
			}
			for x := i; x < j; x++ {
				if (x-i)%stride != 0 && x != j-1 {
					continue
				}
				if v, ok := value(x); ok {
					add(tick[x], v)
				}
			}
			if k+1 >= len(notes) || notes[k+1].on-notes[k].off > svGapTicks {
				add(notes[k].off+svGapTicks/4, def)
			}
		}
		i = j
	}
	return c
}

// smooth 是窗长 w 帧（奇数）的居中滑动平均。
func smooth(xs []float64, w int) []float64 {
	out := make([]float64, len(xs))
	h := w / 2
	sum, cnt := 0.0, 0
	for i := 0; i < len(xs)+h; i++ {
		if i < len(xs) {
			sum += xs[i]
			cnt++
		}
		if i-w >= 0 {
			sum -= xs[i-w]
			cnt--
		}
		if c := i - h; c >= 0 && c < len(xs) {
			out[c] = sum / float64(cnt)
		}
	}
	return out
}

// robustZ 返回按 ref（有声帧上的取值）的中位数与 10–90 分位距归一化的 z 分数函数（分位距 / 2.56 近似标准差）。
func robustZ(xs, ref []float64) func(i int) float64 {
	med := stat.Percentile(ref, 0.5)
	scale := (stat.Percentile(ref, 0.9) - stat.Percentile(ref, 0.1)) / 2.56
	if scale <= 1e-9 {
		return func(int) float64 { return 0 }
	}
	return func(i int) float64 { return (xs[i] - med) / scale }
}
