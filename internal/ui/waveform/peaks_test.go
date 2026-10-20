package waveform

import "testing"

func TestComputeStereoAndPartialBucket(t *testing.T) {
	// 5 帧立体声，每桶 2 帧：最后一个桶只有 1 帧。
	samples := []float32{
		0.1, -0.2, 0.3, 0.0,
		-0.5, 0.4, 0.2, 0.2,
		0.9, -0.9,
	}
	p := Compute(samples, 2, 48000, 2)
	if p.Frames != 5 || len(p.Min) != 3 {
		t.Fatalf("frames %d buckets %d", p.Frames, len(p.Min))
	}
	want := [][2]float32{{-0.2, 0.3}, {-0.5, 0.4}, {-0.9, 0.9}}
	for i, w := range want {
		if p.Min[i] != w[0] || p.Max[i] != w[1] {
			t.Errorf("bucket %d = [%v, %v], want %v", i, p.Min[i], p.Max[i], w)
		}
	}
	if lo, hi := p.Range(1, 4); lo != -0.5 || hi != 0.4 {
		t.Errorf("Range(1,4) = %v, %v", lo, hi)
	}
	if lo, hi := p.Range(10, 20); lo != 0 || hi != 0 {
		t.Errorf("out-of-range Range = %v, %v", lo, hi)
	}
}
