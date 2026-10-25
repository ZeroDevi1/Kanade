package score

import (
	"math"
	"testing"
)

// vowel 是元音的两个共振峰（Hz）。
type vowel struct{ f1, f2 float64 }

var (
	vowelU = vowel{300, 800}
	vowelA = vowel{700, 1200}
)

// synthVoice 合成 32 kHz 的哼唱：基频 f0(t)、20 个谐波，谐波幅度按共振峰包络 env(t, 频率) 取值；每个采样按包络的
// 总能量归一，整体响度不变（字的交替只体现在谱形状上）。am 为额外的幅度调制（dB），可为 nil。
func synthVoice(dur float64, f0 func(float64) float64, env func(t, hz float64) float64, am func(float64) float64) []float32 {
	const rate = 32000
	out := make([]float32, int(dur*rate))
	phase := make([]float64, 21)
	for i := range out {
		t := float64(i) / rate
		f := f0(t)
		var amps [21]float64
		norm := 0.0
		for h := 1; h <= 20; h++ {
			if f*float64(h) > 15000 {
				break
			}
			amps[h] = env(t, f*float64(h))
			norm += amps[h] * amps[h]
		}
		g := 0.1 / math.Sqrt(norm)
		if am != nil {
			g *= math.Pow(10, am(t)/20)
		}
		v := 0.0
		for h := 1; h <= 20; h++ {
			phase[h] += 2 * math.Pi * f * float64(h) / rate
			v += amps[h] * math.Sin(phase[h])
		}
		out[i] = float32(v * g)
	}
	return out
}

// formants 返回元音的谱包络：两个共振峰的高斯加 1/f 倾斜。
func formants(v vowel) func(hz float64) float64 {
	return func(hz float64) float64 {
		g := func(c, bw float64) float64 { return math.Exp(-0.5 * math.Pow((hz-c)/bw, 2)) }
		return (g(v.f1, 120) + 0.6*g(v.f2, 180) + 0.02) * 300 / hz
	}
}

// switchVowels 在 at 各时刻用 20 ms 交替两个元音（u、a、u…）的包络。
func switchVowels(at ...float64) func(t, hz float64) float64 {
	return func(t, hz float64) float64 {
		k, frac := 0, 1.0
		for _, s := range at {
			switch {
			case t >= s+0.02:
				k++
			case t >= s:
				frac = (t - s) / 0.02
				a := []vowel{vowelU, vowelA}
				return (1-frac)*formants(a[k%2])(hz) + frac*formants(a[(k+1)%2])(hz)
			}
		}
		return formants([]vowel{vowelU, vowelA}[k%2])(hz)
	}
}

// voiceFrames 用 KaraokeSession（与录音时相同的 YIN）从 PCM 求帧；开头留 0.3 秒静音。
func voiceFrames(pcm []float32) ([]KaraokeFrame, []float32) {
	const rate = 32000
	full := append(make([]float32, int(0.3*rate)), pcm...)
	full = append(full, make([]float32, int(0.3*rate))...)
	sess := NewKaraokeSession(rate)
	for i := 0; i < len(full); i += 1024 {
		end := min(i+1024, len(full))
		sess.Push(full[i:end], float64(end)/rate)
	}
	return sess.Frames(), full
}

// syllables 返回乐句的字数（不是连音的音符数）。
func syllables(h Hum) int {
	n := 0
	for _, x := range h.Notes {
		if !x.Slur {
			n++
		}
	}
	return n
}

var fluxSet = HumSettings{BPM: 100, BeatsPerBar: 4, Bars: 1, Free: true}

// 同一音高上「呜—啊—呜」的共振峰跳变（响度不变）：只有频谱通量能分出三个字，没有录音时只有一个字。
func TestFluxFormantSwitch(t *testing.T) {
	pcm := synthVoice(1.2, func(float64) float64 { return 262 }, switchVowels(0.4, 0.8), nil)
	frames, full := voiceFrames(pcm)
	if n := syllables(TranscribeHum(frames, fluxSet)); n != 1 {
		t.Fatalf("without audio: %d syllables, want 1 (the test relies on flux alone)", n)
	}
	h := TranscribeHumAudio(frames, full, 32000, fluxSet)
	if n := syllables(h); n != 3 {
		t.Fatalf("with flux: %d syllables %+v", n, h.Notes)
	}
}

// 侧音式的凹陷：0.4 秒起 60 ms 内 1 kHz 以上的谐波衰减 12 dB、整体响度只降约 1.5 dB，然后恢复。
func TestFluxLateralDip(t *testing.T) {
	env := func(t, hz float64) float64 {
		a := formants(vowelA)(hz)
		if t >= 0.4 && t < 0.46 && hz > 1000 {
			a *= math.Pow(10, -12.0/20)
		}
		return a
	}
	pcm := synthVoice(1.0, func(float64) float64 { return 262 }, env, func(t float64) float64 {
		if t >= 0.4 && t < 0.46 {
			return -1.5
		}
		return 0
	})
	frames, full := voiceFrames(pcm)
	if n := syllables(TranscribeHumAudio(frames, full, 32000, fluxSet)); n != 2 {
		t.Fatalf("with flux: %d syllables, want 2", n)
	}
}

// 纯滑音与颤音（含幅度起伏）不应被通量切开。
func TestFluxNoFalseOnsets(t *testing.T) {
	glide := func(t float64) float64 {
		switch {
		case t < 0.3:
			return 440
		case t < 0.6:
			return 440 * math.Pow(2, 2.0/12*(t-0.3)/0.3)
		case t < 1.0:
			return 440 * math.Pow(2, 2.0/12-1.0*(t-0.6)/0.4)
		}
		return 220
	}
	vib := func(t float64) float64 { return 330 * math.Pow(2, 60.0/1200*math.Sin(2*math.Pi*5.5*t)) }
	cases := []struct {
		name string
		f0   func(float64) float64
		am   func(float64) float64
		dur  float64
	}{
		{"滑音", glide, nil, 1.4},
		{"颤音", vib, func(t float64) float64 { return 2 * math.Sin(2*math.Pi*5.5*t) }, 1.5},
	}
	for _, c := range cases {
		pcm := synthVoice(c.dur, c.f0, func(_, hz float64) float64 { return formants(vowelA)(hz) }, c.am)
		frames, full := voiceFrames(pcm)
		without := syllables(TranscribeHum(frames, fluxSet))
		with := syllables(TranscribeHumAudio(frames, full, 32000, fluxSet))
		if with != without {
			t.Errorf("%s: %d syllables with flux, %d without", c.name, with, without)
		}
		// 只看声音中段：从静音起音与结束处本来就是有声段的边界。
		fx := spectralFlux(full, 32000, frames)
		peak := 0.0
		for i, f := range frames {
			if f.T > 0.38 && f.T < 0.3+c.dur-0.05 {
				peak = max(peak, fx[i])
			}
		}
		// 通量证据远不足以单独触发起音（起音阈值为 1）。
		if ev := min(humFluxCap, max(0, peak-humFluxMin)/humFluxRef); ev >= 0.25 {
			t.Errorf("%s: flux peak %.1f inside the note (evidence %.2f)", c.name, peak, ev)
		}
	}
}

func TestFFT(t *testing.T) {
	x := make([]complex128, 16)
	for i := range x {
		x[i] = complex(math.Cos(2*math.Pi*3*float64(i)/16), 0)
	}
	fft(x)
	for k, v := range x {
		want := 0.0
		if k == 3 || k == 13 {
			want = 8
		}
		if math.Abs(real(v)-want) > 1e-9 || math.Abs(imag(v)) > 1e-9 {
			t.Fatalf("bin %d = %v, want %v", k, v, want)
		}
	}
}
