package score

import (
	"math"
	"runtime"
	"sync"
)

// 钢琴的模态加法合成：每个音由若干泛音（模态）组成，每个泛音是一个按指数衰减的正弦。参照 Bank、Zambon、Fontana
// 「A Modal-Based Real-Time Piano Synthesizer」（IEEE TASLP 18(4), 2010）的模态思路，保留对音色影响最大的几项物理特征：
//
//   - 弦的非谐性（Fletcher 1964）：fₙ = n·f₀·√(1 + B·n²)，B 随音高升高；高音的泛音偏高，产生钢琴特有的明亮感。
//   - 与频率相关的损耗：σₙ = σ₁·(1 + 0.08·(n−1)) + b₃·fₙ²，高次泛音衰减更快；基音衰减时间随音高降低而变长。
//   - 同音多弦（Weinreich 1977）：中高音区每个音 3 根弦、低音 2 根，各弦相差零点几音分，泛音之间产生拍音；
//     每个泛音分为快衰减的「瞬时声」与慢衰减的「余音」两部分，即钢琴的两段式衰减。
//   - 击弦：击弦点约在弦长 1/7 处，使第 7 泛音附近被削弱；击弦锤越轻越软，激励的高频越少——力度决定音色明暗，
//     弱奏（pp–mp）时只剩少数低次泛音，声音柔和，适合铺底。
//   - 制音器：松键后以约 0.12 秒的时间常数快速衰减。
//
// 未模拟音板与琴体的共鸣（原文用 FFT 卷积或并联二阶滤波器实现），以及弦的纵向振动等非线性效应。

// PianoNote 是一个要演奏的音。Velocity 为 0–1（约 0.3 为 pp、0.5 为 mf、0.8 为 f）。
type PianoNote struct {
	MIDI       int
	Start, End float64 // 按下与松开的时刻（秒）
	Velocity   float64
}

const (
	pianoMaxPartials = 40
	pianoMaxFreq     = 9000.0 // 高于此频率的泛音弱到可以忽略，不再计算
	pianoStrikePoint = 1.0 / 7
	pianoDamperTau   = 0.12 // 松键后的衰减时间常数（秒）
	pianoTail        = 0.6  // 松键后继续计算的时长（秒）
	pianoAttack      = 0.002
	pianoPromptShare = 0.65 // 瞬时声在泛音能量中的占比，其余为余音
	pianoMinPartial  = 1e-3 // 相对激励低于此值的泛音跳过
)

// inharmonicity 返回音高 m 的非谐系数 B：A0 约 1e−4，中央 C 约 4.6e−4，C8 约 2.7e−3（钢琴实测值的量级拟合）。
func inharmonicity(m int) float64 { return math.Pow(10, -4+0.017*float64(m-21)) }

// fundamentalT60 返回基音的 60 dB 衰减时间（秒）：低音约 25 秒，中央 C 约 4 秒，高音更短。
func fundamentalT60(m int) float64 { return 25 * math.Exp(-0.045*float64(m-21)) }

// stringCount 是每个音的弦数：最低音区 1 根（缠弦），低音区 2 根，其余 3 根。
func stringCount(m int) int {
	switch {
	case m < 30:
		return 1
	case m < 46:
		return 2
	}
	return 3
}

// renderPianoNote 把一个音加到 buf（单声道，rate Hz）。
func renderPianoNote(buf []float32, rate int, n PianoNote) {
	sr := float64(rate)
	f0 := 440 * math.Pow(2, float64(n.MIDI-69)/12)
	B := inharmonicity(n.MIDI)
	v := math.Max(0.05, math.Min(1, n.Velocity))
	// 击弦锤的激励截止频率：力度越大锤毡越「硬」，高频越多。
	fc := f0 * (1.5 + 14*v*v)
	sigma1 := 6.91 / fundamentalT60(n.MIDI)
	const b3 = 1.2e-7 // 使 5 kHz 处额外损耗约 3 /s

	held := math.Max(n.End-n.Start, 0.02)
	start := int(n.Start * sr)
	length := int((held + pianoTail) * sr)
	if start >= len(buf) {
		return
	}
	length = min(length, len(buf)-start)
	strings := stringCount(n.MIDI)
	gain := 0.18 * math.Pow(v, 1.5) / float64(strings)
	releaseAt := int(held * sr)
	damper := math.Exp(-1 / (pianoDamperTau * sr))
	attack := int(pianoAttack * sr)

	for k := 1; k <= pianoMaxPartials; k++ {
		fk := float64(k) * f0 * math.Sqrt(1+B*float64(k*k))
		if fk > pianoMaxFreq || fk > sr*0.45 {
			break
		}
		// 激励：击弦点梳状 × 锤的低通 × 1/n 的弦振幅分布。
		amp := math.Abs(math.Sin(math.Pi*float64(k)*pianoStrikePoint)) / math.Pow(float64(k), 0.9) /
			(1 + math.Pow(fk/fc, 4))
		if amp < pianoMinPartial {
			continue
		}
		sigma := sigma1*(1+0.08*float64(k-1)) + b3*fk*fk
		dAfter := math.Exp(-sigma / sr)        // 余音
		dPrompt := math.Exp(-3.5 * sigma / sr) // 瞬时声衰减快约 3.5 倍
		for s := 0; s < strings; s++ {
			// 各弦相差约 ±0.4 音分。
			cents := 0.0
			if strings > 1 {
				cents = (float64(s)/float64(strings-1) - 0.5) * 0.8
			}
			w := 2 * math.Pi * fk * math.Pow(2, cents/1200) / sr
			cr, ci := math.Cos(w), math.Sin(w)
			// 相位用复数旋转递推（每个采样两次乘加），初相按弦错开，避免各弦同相叠加成尖峰。
			pr, pi := math.Cos(float64(s)*1.3), math.Sin(float64(s)*1.3)
			ap, aa := amp*pianoPromptShare, amp*(1-pianoPromptShare)
			rel := 1.0
			for i := 0; i < length; i++ {
				env := ap + aa
				if i < attack {
					env *= 0.5 - 0.5*math.Cos(math.Pi*float64(i)/float64(attack))
				}
				if i >= releaseAt {
					rel *= damper
					env *= rel
				}
				buf[start+i] += float32(gain * env * pi)
				pr, pi = pr*cr-pi*ci, pr*ci+pi*cr
				ap *= dPrompt
				aa *= dAfter
				if ap+aa < 1e-5 || rel < 1e-4 {
					break
				}
			}
		}
	}
}

// RenderPiano 合成 notes（单声道，长度 duration 秒），多核并行，峰值超过 0.9 时整体衰减。
func RenderPiano(notes []PianoNote, rate int, duration float64) []float32 {
	out := renderRaw(notes, rate, duration)
	peak := float32(0)
	for _, x := range out {
		peak = max(peak, x, -x)
	}
	if peak > 0.9 {
		g := 0.9 / peak
		for i := range out {
			out[i] *= g
		}
	}
	return out
}

// renderRaw 合成 notes，不做峰值处理（多条音轨分开合成后再混合时保持相对电平）。
func renderRaw(notes []PianoNote, rate int, duration float64) []float32 {
	n := int(duration * float64(rate))
	if n <= 0 {
		return nil
	}
	workers := max(1, min(runtime.NumCPU(), len(notes)))
	parts := make([][]float32, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]float32, n)
			for i := w; i < len(notes); i += workers {
				renderPianoNote(buf, rate, notes[i])
			}
			parts[w] = buf
		}(w)
	}
	wg.Wait()
	out := parts[0]
	for _, p := range parts[1:] {
		for i, x := range p {
			out[i] += x
		}
	}
	return out
}
