// Package waveform 计算音频峰值并在 MyGo Native UI 中绘制波形。
package waveform

import "math"

// Peaks 是按固定帧数分桶的峰值摘要，绘制时不再访问原始采样。
//
// 不变量：len(Min) == len(Max) == ceil(Frames / FramesPerBucket)，且 Min[i] <= Max[i]。
// 创建后只读，可在 goroutine 间共享。
type Peaks struct {
	Min, Max []float32
	// FramesPerBucket 是每个桶覆盖的帧数（一帧 = 所有声道的一个采样点）。
	FramesPerBucket int
	// Frames 是音频总帧数，SampleRate 为每秒帧数。
	Frames     int
	SampleRate int
}

// DefaultFramesPerBucket 在 48 kHz 下约 2.7 ms 一个桶：3 分钟音频约 6.6 万个桶，
// 1 MB 左右内存，足够在 4K 宽度上放大约 15 倍仍保持细节；调小会提高放大精度但增加内存。
const DefaultFramesPerBucket = 128

// Compute 从交错采样计算峰值，多声道取各声道的最小 / 最大值合并。
//
// samples 按 channels 交错存放；channels < 1 或 framesPerBucket < 1 时返回空 Peaks。
// 只读取 samples，不持有它。
func Compute(samples []float32, channels, sampleRate, framesPerBucket int) Peaks {
	if channels < 1 || framesPerBucket < 1 {
		return Peaks{}
	}
	frames := len(samples) / channels
	n := (frames + framesPerBucket - 1) / framesPerBucket
	p := Peaks{
		Min:             make([]float32, n),
		Max:             make([]float32, n),
		FramesPerBucket: framesPerBucket,
		Frames:          frames,
		SampleRate:      sampleRate,
	}
	for b := range n {
		lo := b * framesPerBucket * channels
		hi := min(lo+framesPerBucket*channels, frames*channels)
		mn, mx := float32(math.Inf(1)), float32(math.Inf(-1))
		for _, s := range samples[lo:hi] {
			mn = min(mn, s)
			mx = max(mx, s)
		}
		p.Min[b], p.Max[b] = mn, mx
	}
	return p
}

// Range 返回帧区间 [from, to) 内的最小与最大值；区间为空或越界部分被裁掉，全空时返回 0, 0。
func (p *Peaks) Range(from, to int) (lo, hi float32) {
	if p.FramesPerBucket < 1 {
		return 0, 0
	}
	b0 := max(from/p.FramesPerBucket, 0)
	b1 := min((to+p.FramesPerBucket-1)/p.FramesPerBucket, len(p.Min))
	if b0 >= b1 {
		return 0, 0
	}
	lo, hi = p.Min[b0], p.Max[b0]
	for b := b0 + 1; b < b1; b++ {
		lo = min(lo, p.Min[b])
		hi = max(hi, p.Max[b])
	}
	return lo, hi
}
