package score

import (
	"math"
	"math/cmplx"
)

// 频谱通量：连续哼「噜噜」「呜啦」这类字之间没有响度谷的唱法，字的交替只表现为谱形状的变化（共振峰跳变、辅音处
// 高频先掉后升）。按 SuperFlux（Böck & Widmer 2013, "Maximum Filter Vibrato Suppression for Onset Detection"）计算：
// 1/4 八度频带的对数能量，与 fluxLag 帧之前相邻 ±1 带的最大值比较（音高在 20 ms 内移动几百音分也不产生正差分，
// 颤音、滑音因此被抑制），扣除各带共同的上升量（整体变响由响度证据负责），正差分求和，再扣除 ±0.5 秒的局部中位数
// 基线。设计与常量经 Claude Fable 5.1 咨询（其建议减去中位数；合成测试发现多数频带衰减时会把不变的频带翻成假的
// 正差分，改为只扣共同的上升量），为按人声频谱推导的经验值。

const (
	fluxN         = 1024   // FFT 长度（32 kHz 下 32 ms）
	fluxLag       = 2      // 与几帧之前比较（20 ms）
	fluxLoHz      = 250.0  // 频带范围：避开基频区与几乎只有气声的高频
	fluxHiHz      = 5000.0 //
	fluxPerOct    = 4      // 每八度的频带数
	fluxFloorDB   = 60     // 每个频带相对其最大值的动态范围下限（静音带不产生大跳）
	fluxBaseWin   = 50     // 局部基线的半窗（帧，±0.5 秒）
	humFluxMin    = 10.0   // 低于此的通量（dB·带）不计入起音证据
	humFluxRef    = 15.0   // 通量证据的参照：通量 25 时这一项为 1，可单独触发起音
	humFluxCap    = 1.3    // 通量证据的上限，免得一个爆破音压住邻近的真实起音
	fluxBandWidth = 1      // 最大值滤波的频带邻域（±1 带）
	fluxFallDB    = -3.0   // 帧响度比 fluxLag 帧之前低超过它（dB，整体在衰减）时不计通量
)

// spectralFlux 返回每帧的通量证据（dB·带，已扣除局部基线，≥ 0）。pcm 为原始录音（rate Hz），第 i 帧对应样本
// round(frames[i].T·rate)；窗口超出录音的帧为 0。没有录音时返回 nil。
func spectralFlux(pcm []float32, rate int, frames []KaraokeFrame) []float64 {
	if len(pcm) < fluxN || len(frames) == 0 || rate <= 0 {
		return nil
	}
	// 频带：中心 250 Hz 起每 1/4 八度一个，到 5 kHz；三角滤波，边缘在相邻频带的中心。
	var centers []float64
	for c := fluxLoHz; c <= fluxHiHz*1.0001; c *= math.Pow(2, 1.0/fluxPerOct) {
		centers = append(centers, c)
	}
	B := len(centers)
	binHz := float64(rate) / fluxN
	type tap struct {
		bin int
		w   float64
	}
	filters := make([][]tap, B)
	for b, c := range centers {
		lo, hi := c/math.Pow(2, 1.0/fluxPerOct), c*math.Pow(2, 1.0/fluxPerOct)
		for k := int(lo / binHz); k <= int(hi/binHz)+1 && k < fluxN/2; k++ {
			f := float64(k) * binHz
			w := 0.0
			switch {
			case f >= lo && f <= c:
				w = (f - lo) / (c - lo)
			case f > c && f <= hi:
				w = (hi - f) / (hi - c)
			}
			if w > 0 {
				filters[b] = append(filters[b], tap{k, w})
			}
		}
	}
	window := make([]float64, fluxN)
	for i := range window {
		window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/fluxN)
	}
	// 每帧各频带的对数能量（dB）。
	L := make([][]float64, len(frames))
	valid := make([]bool, len(frames))
	buf := make([]complex128, fluxN)
	for i, f := range frames {
		c := int(math.Round(f.T * float64(rate)))
		if c-fluxN/2 < 0 || c+fluxN/2 > len(pcm) {
			continue
		}
		for j := range buf {
			buf[j] = complex(float64(pcm[c-fluxN/2+j])*window[j], 0)
		}
		fft(buf)
		L[i] = make([]float64, B)
		for b, taps := range filters {
			e := 1e-12
			for _, tp := range taps {
				a := cmplx.Abs(buf[tp.bin])
				e += tp.w * a * a
			}
			L[i][b] = 10 * math.Log10(e)
		}
		valid[i] = true
	}
	// 每个频带限制在其最大值以下 fluxFloorDB 之内。
	for b := range B {
		top := math.Inf(-1)
		for i := range L {
			if valid[i] {
				top = max(top, L[i][b])
			}
		}
		for i := range L {
			if valid[i] {
				L[i][b] = max(L[i][b], top-fluxFloorDB)
			}
		}
	}
	// SuperFlux 差分、去掉各带共同的变化、正差分求和。
	raw := make([]float64, len(frames))
	d := make([]float64, B)
	for i := fluxLag; i < len(frames); i++ {
		if !valid[i] || !valid[i-fluxLag] {
			continue
		}
		for b := range B {
			ref := math.Inf(-1)
			for bb := max(0, b-fluxBandWidth); bb <= min(B-1, b+fluxBandWidth); bb++ {
				ref = max(ref, L[i-fluxLag][bb])
			}
			d[b] = L[i][b] - ref
		}
		// 整体响度在下降，或 FFT 窗的后半段里声音在结束（截断产生宽带的能量）：不计。
		if frames[i].DB-frames[i-fluxLag].DB < fluxFallDB || frames[min(i+fluxLag, len(frames)-1)].DB-frames[i].DB < fluxFallDB {
			continue
		}
		// 扣除各带共同的上升量（第 25 百分位，不小于 0）：整体变响由响度证据负责；只扣上升，多数频带衰减时
		// 不变的频带不会被翻成正差分。
		common := max(0, percentile(d, 0.25))
		for _, v := range d {
			raw[i] += max(0, v-common)
		}
	}
	// 扣除局部中位数基线（气声多的嗓音基线更高）。
	out := make([]float64, len(frames))
	for i := range raw {
		base := median(raw[max(0, i-fluxBaseWin):min(len(raw), i+fluxBaseWin+1)])
		out[i] = max(0, raw[i]-base)
	}
	return out
}

// fluxEvidence 把通量换算成起音证据的一项（0–humFluxCap）。
func fluxEvidence(fx []float64, i int) float64 {
	if fx == nil {
		return 0
	}
	return min(humFluxCap, max(0, fx[i]-humFluxMin)/humFluxRef)
}

// fft 是原地的基 2 快速傅里叶变换（len(x) 须为 2 的幂）。
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := range size / 2 {
				a, b := x[start+k], x[start+k+size/2]*wk
				x[start+k], x[start+k+size/2] = a+b, a-b
				wk *= w
			}
		}
	}
}
