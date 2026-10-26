package synth

import (
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"

	"kanade/internal/score/stat"
)

// 乐器音轨的合成：试听多乐器转录的结果时，按 GM 音色（program，主要看音色族 program / 8）选一种轻量的合成方式，
// 目的是听出各乐器的音符、节奏与强弱，不追求还原音色：
//   - 钢琴：沿用钢琴模型（piano.go）；电钢琴（4、5）为 FM 音叉模型；羽管键琴、击弦古钢琴（6、7）为明亮的拨弦。
//   - 吉他、贝斯、竖琴与民族弹拨乐：Karplus–Strong 拨弦，激励按力度变亮、按拨弦位置削弱部分泛音；
//     失真 / 过载吉他再过一级 tanh 失真，电贝斯另加低通。
//   - 风琴：前几次谐波的加法合成，按住时不衰减，叠一组微失谐的拉杆形成合唱感。
//   - 弦乐、合奏、铜管、簧管、笛类与合成音色：减法合成（polyBLEP 锯齿波 / 方波 + 二阶低通 + ADSR，长音加颤音）；
//     合奏与铺底叠几个微失谐的振荡器，铜管的滤波器随起音打开，力度越大越亮。
//   - 有音高打击乐（钟琴、马林巴等）、打击乐族与音效：几个非谐泛音的指数衰减（敲击类）。
//   - 鼓组：按 GM 打击乐键位合成底鼓、军鼓、通鼓、踩镲、吊镲等（正弦扫频与滤波噪声）。
// 乐器音轨合成后过一级轻混响（roomReverb），再按 RMS 调到相近的响度（instrumentRMS），便于在混音中比较。

// instrumentRMS 是乐器音轨调整后的有声部分 RMS（约 −22 dBFS）：与原曲同时播放时能听清而不盖过原曲（经验值）。
const instrumentRMS = 0.08

// RenderTrack 合成音轨 tr（单声道，rate Hz，长 duration 秒），多核并行；没有音符时返回 nil。
// leveled 为 true 时按 RMS 调到 instrumentRMS（峰值不超过 0.9），否则保持合成的原始电平。
func RenderTrack(tr Track, rate int, duration float64, leveled bool) []float32 {
	n := int(duration * float64(rate))
	if n <= 0 || len(tr.Notes) == 0 {
		return nil
	}
	notes := applyPedal(tr.Notes, tr.Pedal)
	workers := max(1, min(runtime.NumCPU(), len(notes)))
	parts := make([][]float32, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]float32, n)
			for i := w; i < len(notes); i += workers {
				// 噪声按音符序号取种子：同一音轨每次合成的结果相同。
				rng := rand.New(rand.NewPCG(uint64(i), 0x6b616e616465))
				renderTrackNote(buf, rate, tr, notes[i], rng)
			}
			parts[w] = buf
		}()
	}
	wg.Wait()
	out := parts[0]
	for _, p := range parts[1:] {
		for i, x := range p {
			out[i] += x
		}
	}
	if leveled {
		wet := roomWet
		if tr.Drum {
			wet = roomWetDrums
		}
		roomReverb(out, rate, wet)
		level(out, instrumentRMS, 0.9)
	}
	return out
}

func renderTrackNote(buf []float32, rate int, tr Track, n PianoNote, rng *rand.Rand) {
	if tr.Drum {
		renderDrum(buf, rate, n, rng)
		return
	}
	p := tr.Program
	switch family := p / 8; {
	case p == 4 || p == 5: // 电钢琴
		renderEPiano(buf, rate, n)
	case p == 6 || p == 7: // 羽管键琴、击弦古钢琴：明亮、短促的拨弦
		renderPluck(buf, rate, n, pluck{decay: 0.994, bright: 1, pickPos: 0.08}, rng)
	case family == 0: // 钢琴
		renderPianoNote(buf, rate, n)
	case family == 3 || family == 4 || p == 46 || family == 13: // 吉他、贝斯、竖琴、民族弹拨乐
		v := pluck{decay: 0.996, bright: 0.6, pickPos: 0.15}
		switch {
		case p == 29 || p == 30: // 过载 / 失真吉他
			v.drive = 6
		case family == 4:
			v.decay, v.bright, v.pickPos = 0.998, 0.35, 0.2 // 贝斯余音更长、更暗
			if p != 32 {
				v.tone = 6 // 电贝斯：拾音器与音箱削去高频
			}
		case p == 46:
			v.bright, v.pickPos = 0.45, 0.5 // 竖琴在弦中部拨，偶次泛音弱、音色圆润
		}
		renderPluck(buf, rate, n, v, rng)
	case family == 2: // 风琴
		renderOrgan(buf, rate, n)
	case family >= 5 && family <= 11:
		renderSustained(buf, rate, n, sustainedVoices[family])
	default: // 有音高打击乐、打击乐族、音效
		renderMallet(buf, rate, n)
	}
}

// level 把有声部分（|x| > 1e-4）的 RMS 调到 rms，峰值不超过 peak。
func level(x []float32, rms, peak float64) {
	var sum float64
	var count int
	var top float32
	for _, v := range x {
		if v > 1e-4 || v < -1e-4 {
			sum += float64(v) * float64(v)
			count++
		}
		top = max(top, v, -v)
	}
	if count == 0 || top == 0 {
		return
	}
	g := rms / math.Sqrt(sum/float64(count))
	g = min(g, peak/float64(top))
	for i := range x {
		x[i] *= float32(g)
	}
}

func MIDIFreq(m int) float64 { return 440 * math.Pow(2, float64(m-69)/12) }

// span 返回音符写入 buf 的采样范围：从起音到松开后 tail 秒（不超过 buf 末尾）。ok 为 false 时音符在 buf 之外。
func span(buf []float32, rate int, n PianoNote, tail float64) (i0, i1 int, ok bool) {
	i0 = int(n.Start * float64(rate))
	i1 = min(len(buf), int((n.End+tail)*float64(rate)))
	return i0, i1, i0 >= 0 && i0 < i1
}

// pluck 是一种拨弦音色。
type pluck struct {
	decay   float64 // 每周期的环路衰减（越接近 1 余音越长）
	bright  float64 // 最大力度时激励的明亮度（0–1）：激励噪声的一阶低通系数，力度越小越暗
	pickPos float64 // 拨弦位置（弦长的比例）：激励减去延迟 pickPos 个周期的自身，削弱该位置为波节的泛音
	drive   float64 // > 0 时过 tanh 失真
	tone    float64 // > 0 时输出过截止为 tone 倍基音的一阶低通
}

// renderPluck 是 Karplus–Strong 拨弦：一段经过塑形的噪声在长为一个周期的延迟线中循环，经两点平均（低通）与 decay 衰减；
// 松开后改用快速衰减（制音）。激励的塑形参照 Jaffe 与 Smith「Extensions of the Karplus-Strong Plucked-String
// Algorithm」（CMJ 7(2), 1983）的力度低通与拨弦位置梳状滤波。
func renderPluck(buf []float32, rate int, n PianoNote, v pluck, rng *rand.Rand) {
	const release = 0.08
	i0, i1, ok := span(buf, rate, n, release)
	if !ok {
		return
	}
	// 两点平均带来半个采样的延迟，周期长度相应减去 0.5。
	period := max(2, int(math.Round(float64(rate)/MIDIFreq(n.MIDI)-0.5)))
	line := make([]float64, period)
	for i := range line {
		line[i] = rng.Float64()*2 - 1
	}
	// 激励塑形：先按力度低通（来回各一遍，避免相位偏向一侧），再减去拨弦位置处的延迟副本，最后去掉直流。
	alpha := stat.Clamp(v.bright*(0.35+0.65*n.Velocity), 0.05, 1)
	for pass := 0; pass < 2; pass++ {
		lp := line[len(line)-1]
		for i := range line {
			lp += alpha * (line[i] - lp)
			line[i] = lp
		}
		slices.Reverse(line)
	}
	if d := int(v.pickPos * float64(period)); d > 0 {
		orig := slices.Clone(line)
		for i := range line {
			line[i] = orig[i] - orig[(i-d+period)%period]
		}
	}
	mean := stat.Mean(line)
	peak := 0.0
	for i := range line {
		line[i] -= mean
		peak = max(peak, math.Abs(line[i]))
	}
	if peak > 0 {
		for i := range line {
			line[i] /= peak
		}
	}
	off := int(n.End * float64(rate))
	damp := math.Exp(-1 / (release * float64(rate) / 6)) // 松开后约 release 秒衰减 −50 dB
	toneA := 0.0
	if v.tone > 0 {
		toneA = 1 - math.Exp(-2*math.Pi*min(v.tone*MIDIFreq(n.MIDI), 9000)/float64(rate))
	}
	amp := n.Velocity * 0.5
	g, lp := 1.0, 0.0
	for i, k := i0, 0; i < i1; i, k = i+1, k+1 {
		j := k % period
		next := line[(j+1)%period]
		x := line[j]
		line[j] = v.decay * 0.5 * (x + next)
		if i >= off {
			g *= damp
		}
		s := x * amp * g
		if toneA > 0 {
			lp += toneA * (s - lp)
			s = lp
		}
		if v.drive > 0 {
			s = math.Tanh(s*v.drive) / math.Tanh(v.drive) * 0.6
		}
		buf[i] += float32(s)
	}
}

// renderEPiano 是电钢琴（Rhodes 一类的音叉琴）的二算子 FM 模型：载波与调制同频（1:1），调制指数随力度增大、
// 起音后快速回落，得到「叮」的起音与圆润的余音；另叠一个 14 倍频的弱泛音（音叉的金属声），衰减更快。
func renderEPiano(buf []float32, rate int, n PianoNote) {
	const tail = 0.3
	i0, i1, ok := span(buf, rate, n, tail)
	if !ok {
		return
	}
	f := MIDIFreq(n.MIDI)
	dur := n.End - n.Start
	index := 0.4 + 2.2*n.Velocity*n.Velocity
	t60 := stat.Clamp(6*math.Exp(-0.03*float64(n.MIDI-48)), 1, 8) // 低音余音更长
	amp := n.Velocity * 0.3
	for i := i0; i < i1; i++ {
		t := float64(i-i0) / float64(rate)
		env := math.Exp(-6.9*t/t60) * min(t/0.002, 1)
		if t > dur {
			env *= math.Exp(-(t - dur) / 0.06) // 松键后的制音
		}
		mod := index * math.Exp(-t/0.25) * math.Sin(2*math.Pi*f*t)
		s := math.Sin(2*math.Pi*f*t + mod)
		if f*14 < float64(rate)/2 {
			s += 0.12 * n.Velocity * math.Exp(-t/0.05) * math.Sin(2*math.Pi*f*14*t)
		}
		buf[i] += float32(s * env * amp)
	}
}

// renderOrgan 是加法合成的风琴：基音与 2、3、4 次谐波（拉杆 8'、4'、2 2/3'、2'），起音与释音各 10 ms；
// 同样的拉杆再叠一组高 4 音分的，两组之间缓慢的拍音近似旋转扬声器的合唱感。
func renderOrgan(buf []float32, rate int, n PianoNote) {
	const edge = 0.01
	i0, i1, ok := span(buf, rate, n, edge)
	if !ok {
		return
	}
	f := MIDIFreq(n.MIDI)
	detune := math.Pow(2, 4.0/1200)
	weights := [...]float64{1, 0.6, 0.35, 0.25}
	dur := n.End - n.Start
	amp := n.Velocity * 0.12
	for i := i0; i < i1; i++ {
		t := float64(i-i0) / float64(rate)
		env := adsr(t, dur, edge, 0, 1, edge)
		var s float64
		for h, w := range weights {
			fh := f * float64(h+1)
			if fh*detune >= float64(rate)/2 {
				break
			}
			s += w * (math.Sin(2*math.Pi*fh*t) + 0.7*math.Sin(2*math.Pi*fh*detune*t))
		}
		buf[i] += float32(s * env * amp)
	}
}

// sustainedVoice 是减法合成的一种音色。
type sustainedVoice struct {
	square          bool    // 方波（簧管、合成主奏）；否则锯齿波
	attack, release float64 // 秒
	cutoff          float64 // 二阶低通截止频率相对基音的倍数（最大力度、起音完成后的值）
	// filterEnv > 0 时截止频率在起音时从 cutoff × filterEnv 开始，filterTime 秒内升到 cutoff（铜管「张开」的音头）。
	filterEnv, filterTime float64
	vibrato               float64 // 颤音深度（音分），起音 0.25 秒后渐入
	breath                float64 // 叠加的气声噪声比例（笛类）
	sine                  bool    // 正弦（笛类）：不用锯齿 / 方波
	// voices 是叠加的振荡器数（合奏、铺底），彼此相差 spread 音分，相位随机；0 视为 1。
	voices int
	spread float64
	level  float64
}

// sustainedVoices 按 GM 音色族给出音色（族 5–11）。取值为经验值：起音越慢越像弓弦 / 铺底，截止越低越暗。
var sustainedVoices = map[int]sustainedVoice{
	5:  {attack: 0.08, release: 0.25, cutoff: 6, vibrato: 12, voices: 2, spread: 4, level: 0.2},                     // 弦乐
	6:  {attack: 0.15, release: 0.4, cutoff: 5, vibrato: 8, voices: 3, spread: 9, level: 0.17},                      // 合奏、合唱
	7:  {attack: 0.04, release: 0.12, cutoff: 8, filterEnv: 0.3, filterTime: 0.08, vibrato: 6, level: 0.22},         // 铜管
	8:  {square: true, attack: 0.03, release: 0.1, cutoff: 5, vibrato: 10, level: 0.2},                              // 簧管
	9:  {sine: true, attack: 0.05, release: 0.12, vibrato: 10, breath: 0.08, level: 0.35},                           // 笛类
	10: {square: true, attack: 0.01, release: 0.08, cutoff: 10, voices: 2, spread: 6, level: 0.16},                  // 合成主奏
	11: {attack: 0.3, release: 0.6, cutoff: 3, filterEnv: 0.5, filterTime: 0.6, voices: 3, spread: 12, level: 0.17}, // 合成铺底
}

func renderSustained(buf []float32, rate int, n PianoNote, v sustainedVoice) {
	i0, i1, ok := span(buf, rate, n, v.release)
	if !ok {
		return
	}
	f := MIDIFreq(n.MIDI)
	dur := n.End - n.Start
	sr := float64(rate)
	voices := max(v.voices, 1)
	noise := rand.New(rand.NewPCG(uint64(n.MIDI), uint64(i0)))
	phase, ratio := make([]float64, voices), make([]float64, voices)
	for k := range voices {
		phase[k] = noise.Float64()
		ratio[k] = 1
		if voices > 1 {
			ratio[k] = math.Pow(2, v.spread*(float64(k)/float64(voices-1)-0.5)/1200)
		}
	}
	// 力度越大越亮：截止频率在 0.55–1 倍之间随力度变化。
	bright := 0.55 + 0.45*n.Velocity
	lowpass := func(cut float64) float64 { return 1 - math.Exp(-2*math.Pi*min(cut*f, 9000)/sr) }
	steady := lowpass(v.cutoff * bright)
	var lp1, lp2 float64
	amp := n.Velocity * v.level / math.Sqrt(float64(voices))
	for i := i0; i < i1; i++ {
		t := float64(i-i0) / sr
		mul := 1.0
		if v.vibrato > 0 && t > 0.25 {
			depth := v.vibrato * min((t-0.25)/0.3, 1)
			mul = math.Pow(2, depth/1200*math.Sin(2*math.Pi*5.5*t))
		}
		var s float64
		for k := range voices {
			dt := f * ratio[k] * mul / sr
			switch {
			case v.sine:
				s += math.Sin(2 * math.Pi * phase[k])
			case v.square:
				s += 0.8 * squareWave(phase[k], dt)
			default:
				s += 2*phase[k] - 1 - polyBLEP(phase[k], dt)
			}
			if phase[k] += dt; phase[k] >= 1 {
				phase[k]--
			}
		}
		if v.breath > 0 {
			s += v.breath * (noise.Float64()*2 - 1)
		}
		if !v.sine {
			// 两个一阶低通串联（−12 dB/倍频程），比一阶更接近模拟合成器的滤波；截止不超过 9 kHz。
			alpha := steady
			if v.filterEnv > 0 && t < v.filterTime {
				alpha = lowpass(v.cutoff * bright * (v.filterEnv + (1-v.filterEnv)*t/v.filterTime))
			}
			lp1 += alpha * (s - lp1)
			lp2 += alpha * (lp1 - lp2)
			s = lp2
		}
		buf[i] += float32(s * adsr(t, dur, v.attack, 0.1, 0.85, v.release) * amp)
	}
}

// polyBLEP 是带限阶跃的修正项：从锯齿 / 方波的跳变处减去，抑制混叠。t 为相位（0–1），dt 为每采样的相位增量。
func polyBLEP(t, dt float64) float64 {
	switch {
	case t < dt:
		t /= dt
		return t + t - t*t - 1
	case t > 1-dt:
		t = (t - 1) / dt
		return t*t + t + t + 1
	}
	return 0
}

func squareWave(t, dt float64) float64 {
	s := 1.0
	if t >= 0.5 {
		s = -1
	}
	return s + polyBLEP(t, dt) - polyBLEP(math.Mod(t+0.5, 1), dt)
}

// adsr 是 t 秒时的包络：attack 秒升到 1，decay 秒降到 sustain，按住到 dur 后 release 秒内线性释放到 0。
func adsr(t, dur, attack, decay, sustain, release float64) float64 {
	held := func(t float64) float64 {
		switch {
		case attack > 0 && t < attack:
			return t / attack
		case decay > 0 && t < attack+decay:
			return 1 - (1-sustain)*(t-attack)/decay
		}
		return sustain
	}
	if t < dur {
		return held(t)
	}
	if release <= 0 {
		return 0
	}
	return held(dur) * max(0, 1-(t-dur)/release)
}

// renderMallet 是敲击类有音高乐器：基音与两个非谐泛音（2.76、5.4 倍，近似木琴 / 钟琴的板振动）指数衰减，
// 松开后再加快衰减。
func renderMallet(buf []float32, rate int, n PianoNote) {
	const tail = 0.6
	i0, i1, ok := span(buf, rate, n, tail)
	if !ok {
		return
	}
	f := MIDIFreq(n.MIDI)
	partials := [...]struct{ ratio, gain, t60 float64 }{{1, 1, 1.2}, {2.76, 0.35, 0.4}, {5.4, 0.15, 0.15}}
	dur := n.End - n.Start
	amp := n.Velocity * 0.3
	for i := i0; i < i1; i++ {
		t := float64(i-i0) / float64(rate)
		var s float64
		for _, p := range partials {
			if p.ratio*f >= float64(rate)/2 {
				continue
			}
			s += p.gain * math.Exp(-6.9*t/p.t60) * math.Sin(2*math.Pi*f*p.ratio*t)
		}
		if t > dur {
			s *= math.Exp(-6.9 * (t - dur) / 0.15)
		}
		buf[i] += float32(s * amp)
	}
}

// renderDrum 合成鼓组的一击。GM 打击乐键位（通道 10）：35–36 底鼓，37 边击，38 / 40 军鼓，39 拍手，
// 41–50 中的 41/43/45/47/48/50 通鼓，42 / 44 闭合 / 踏板踩镲，46 开镲，49 / 52 / 55 / 57 吊镲，51 / 53 / 59 叮叮镲；
// 其余键位写成短促的噪声敲击。鼓不看音符时长（引擎给的是固定短音），按各自的衰减时间发声。
func renderDrum(buf []float32, rate int, n PianoNote, rng *rand.Rand) {
	type hit struct {
		f0, f1, sweep float64 // 正弦从 f0 扫到 f1 用 sweep 秒；f0 为 0 时没有正弦部分
		toneT60       float64
		noise         float64 // 噪声比例
		noiseT60      float64
		highpass      bool // 噪声过高通（镲类）
		level         float64
	}
	var h hit
	switch k := n.MIDI; {
	case k == 35 || k == 36:
		h = hit{f0: 150, f1: 48, sweep: 0.07, toneT60: 0.35, noise: 0.15, noiseT60: 0.02, level: 1.1}
	case k == 38 || k == 40:
		h = hit{f0: 220, f1: 180, sweep: 0.02, toneT60: 0.12, noise: 0.8, noiseT60: 0.2, level: 0.7}
	case k == 37:
		h = hit{f0: 900, f1: 800, sweep: 0.01, toneT60: 0.04, noise: 0.4, noiseT60: 0.04, level: 0.5}
	case k == 39:
		h = hit{noise: 1, noiseT60: 0.12, level: 0.6}
	case k == 41 || k == 43 || k == 45 || k == 47 || k == 48 || k == 50:
		f := 70 + float64(k-41)*14 // 低通鼓约 70 Hz，高通鼓约 200 Hz
		h = hit{f0: f * 1.6, f1: f, sweep: 0.05, toneT60: 0.4, noise: 0.1, noiseT60: 0.05, level: 0.8}
	case k == 42 || k == 44:
		h = hit{noise: 1, noiseT60: 0.06, highpass: true, level: 0.35}
	case k == 46:
		h = hit{noise: 1, noiseT60: 0.45, highpass: true, level: 0.32}
	case k == 49 || k == 52 || k == 55 || k == 57:
		h = hit{noise: 1, noiseT60: 1.6, highpass: true, level: 0.3}
	case k == 51 || k == 53 || k == 59:
		h = hit{f0: 2600, f1: 2600, toneT60: 0.8, noise: 0.6, noiseT60: 0.9, highpass: true, level: 0.22}
	default:
		h = hit{noise: 1, noiseT60: 0.05, highpass: true, level: 0.3}
	}
	length := max(h.toneT60, h.noiseT60)
	i0 := int(n.Start * float64(rate))
	i1 := min(len(buf), i0+int(length*float64(rate)))
	amp := n.Velocity * h.level
	phase, prevNoise, hp := 0.0, 0.0, 0.0
	a := 0.85 // 一阶高通系数（截止约 1 kHz @ 44.1 kHz）
	for i := max(i0, 0); i < i1; i++ {
		t := float64(i-i0) / float64(rate)
		var s float64
		if h.f0 > 0 {
			f := h.f1
			if t < h.sweep {
				f = h.f0 + (h.f1-h.f0)*t/h.sweep
			}
			phase += f / float64(rate)
			s += (1 - h.noise) * math.Exp(-6.9*t/h.toneT60) * math.Sin(2*math.Pi*phase)
		}
		if h.noise > 0 {
			x := rng.Float64()*2 - 1
			if h.highpass {
				hp = a * (hp + x - prevNoise)
				prevNoise = x
				x = hp
			}
			s += h.noise * math.Exp(-6.9*t/h.noiseT60) * x
		}
		buf[i] += float32(s * amp)
	}
}

// 乐器音轨的混响比例（湿声相对干声）：干巴巴的合成音加一点房间感更接近录音里的乐器；鼓少一些，免得节奏发糊（经验值）。
const (
	roomWet      = 0.18
	roomWetDrums = 0.08
)

// roomReverb 给单声道 x（rate Hz）原地加上小房间混响：Schroeder–Moorer 结构（Freeverb 的单声道版），4 个带阻尼的
// 并联梳状滤波器再串联 2 个全通滤波器，延迟按 44.1 kHz 下 Freeverb 的取值换算，混响时间约 1 秒。wet 为湿声比例。
func roomReverb(x []float32, rate int, wet float64) {
	if wet <= 0 || len(x) == 0 {
		return
	}
	scale := float64(rate) / 44100
	combs := [...]int{1116, 1188, 1277, 1356}
	allpasses := [...]int{556, 441}
	const feedback, damp = 0.78, 0.3
	type comb struct {
		buf    []float64
		i      int
		filter float64
	}
	cs := make([]comb, len(combs))
	for k, d := range combs {
		cs[k].buf = make([]float64, max(1, int(float64(d)*scale)))
	}
	aps := make([][]float64, len(allpasses))
	apIdx := make([]int, len(allpasses))
	for k, d := range allpasses {
		aps[k] = make([]float64, max(1, int(float64(d)*scale)))
	}
	for i, v := range x {
		in := float64(v) * 0.25
		var out float64
		for k := range cs {
			c := &cs[k]
			y := c.buf[c.i]
			c.filter = y*(1-damp) + c.filter*damp
			c.buf[c.i] = in + c.filter*feedback
			c.i = (c.i + 1) % len(c.buf)
			out += y
		}
		for k, b := range aps {
			y := b[apIdx[k]]
			b[apIdx[k]] = out + y*0.5
			out = y - out
			apIdx[k] = (apIdx[k] + 1) % len(b)
		}
		x[i] = v + float32(out*wet)
	}
}
