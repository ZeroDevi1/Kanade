// Package stat 是乐谱各子包共用的数值小工具（均值、中位数、分位数、相关系数等）。
package stat

import (
	"math"
	"slices"
)

// Mean 是 xs 的算术平均；xs 为空时为 0。
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Abs 是整数的绝对值。
func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Median 是 xs 的中位数（偶数个时取上中位数）；xs 不能为空。
func Median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// Percentile 返回 xs 的 q 分位数（0–1，最近秩）；xs 为空时为 0。
func Percentile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[min(len(s)-1, max(0, int(q*float64(len(s)-1)+0.5)))]
}

// Clamp 把 v 限制在 [lo, hi]。
func Clamp(v, lo, hi float64) float64 { return min(max(v, lo), hi) }

// Pearson 是 xs 与 ys 的相关系数；样本不足或方差为 0 时为 0。
func Pearson(xs, ys []float64) float64 {
	if len(xs) < 3 || len(xs) != len(ys) {
		return 0
	}
	mx, my := Mean(xs), Mean(ys)
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy, sxx, syy = sxy+dx*dy, sxx+dx*dx, syy+dy*dy
	}
	if sxx == 0 || syy == 0 {
		return 0
	}
	return sxy / math.Sqrt(sxx*syy)
}
