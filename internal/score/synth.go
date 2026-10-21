package score

import "math"

// 和弦伴奏合成：按谱面的音高（ChordVoicing，与五线谱一致）在每个和弦开始处以及其中的每个小节线处弹奏一次，
// 音色为简单的加法合成（类似电钢琴：基音加 3 个泛音，高次泛音衰减更快）。用于单独试听和弦，或叠加在原曲上对照。

const (
	synthAttack  = 0.004 // 起音时间（秒）
	synthRelease = 0.08  // 和弦结束后的释音时间（秒）
	synthMaxNote = 6.0   // 单个音的最长时长（秒）：长时间持续的和弦在此之前已衰减到听不见
	synthDecay   = 1.6   // 基音的衰减时间常数（秒）
	upperGain    = 0.10  // 高音声部每个音的电平
	bassGain     = 0.16  // 低音的电平
)

// harmonics 是泛音的相对电平与衰减倍率（越高的泛音衰减越快）。
var harmonics = []struct{ amp, decay float64 }{{1, 1}, {0.45, 1.8}, {0.22, 2.6}, {0.1, 3.5}}

func midiHz(m int) float64 { return 440 * math.Pow(2, float64(m-69)/12) }

// SynthChords 合成全曲的和弦伴奏，返回单声道采样（rate Hz），长度与 Duration 一致。
func (a *Analysis) SynthChords(rate int) []float32 {
	n := int(a.Duration * float64(rate))
	if n <= 0 {
		return nil
	}
	buf := make([]float64, n)
	var prev []int
	for _, c := range a.Chords {
		v, ok := ChordVoicing(c, prev)
		if !ok {
			continue
		}
		prev = v.MIDIs()
		// 弹奏时刻：和弦开始处，以及和弦持续期间的每个小节线（持续的和弦每小节重新弹一次）。
		strikes := []float64{c.Start}
		for _, d := range a.Downbeats {
			if d > c.Start+0.2 && d < c.End-0.2 {
				strikes = append(strikes, d)
			}
		}
		for i, t0 := range strikes {
			t1 := c.End
			if i+1 < len(strikes) {
				t1 = strikes[i+1]
			}
			note(buf, rate, midiHz(v.Bass.MIDI), bassGain, t0, t1)
			for _, u := range v.Upper {
				note(buf, rate, midiHz(u.MIDI), upperGain, t0, t1)
			}
		}
	}
	// 峰值超过 0.9 时整体衰减，避免削波。
	peak := 0.0
	for _, x := range buf {
		peak = max(peak, math.Abs(x))
	}
	g := 1.0
	if peak > 0.9 {
		g = 0.9 / peak
	}
	out := make([]float32, n)
	for i, x := range buf {
		out[i] = float32(x * g)
	}
	return out
}

// note 把一个音加到 buf：从 t0 开始，t1 时释音。
func note(buf []float64, rate int, hz, gain, t0, t1 float64) {
	dur := min(t1-t0, synthMaxNote)
	start := int(t0 * float64(rate))
	length := int((dur + synthRelease) * float64(rate))
	for k := 0; k < length && start+k < len(buf); k++ {
		if start+k < 0 {
			continue
		}
		t := float64(k) / float64(rate)
		env := min(t/synthAttack, 1)
		if t > dur {
			env *= 1 - (t-dur)/synthRelease
		}
		s := 0.0
		for h, hm := range harmonics {
			f := hz * float64(h+1)
			if f > float64(rate)/2 {
				break
			}
			s += hm.amp * math.Exp(-t*hm.decay/synthDecay) * math.Sin(2*math.Pi*f*t)
		}
		buf[start+k] += gain * env * s
	}
}
