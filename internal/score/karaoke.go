package score

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"sync"
)

// K 歌跟唱：播放伴奏时从麦克风录音，逐帧检测演唱的音高与响度（与歌曲时间对应），唱完后对照旋律线评价音准、
// 覆盖与气息。音高用 YIN（de Cheveigné & Kawahara, 2002, "YIN, a fundamental frequency estimator for speech
// and music"）在 16 kHz 上每 10 ms 一帧实时计算，唱完不必再算一遍。
//
// 时间对齐：每个录音缓冲到达时读取伴奏的播放位置，帧时间 = 该位置 − 帧中心之后已录到的时长。播放位置是混音器
// 送出的位置，比实际听到的早一个输出延迟，录音又晚一个输入延迟，人跟唱还会再晚一点；这些合起来的延迟在评价时
// 按「与旋律最吻合的整体偏移」估计后扣除（KaraokeReport.Lag）。

const (
	// KaraokeRate 是音高检测的采样率；Push 收到的录音须是它的整数倍（mic.Rate = 32 kHz，每两个采样取平均）。
	KaraokeRate = 16000
	// karaokeHop 是帧移（10 ms），karaokeWindow 是 YIN 的分析窗（40 ms，能覆盖 70 Hz 以上两个周期）。
	karaokeHop    = 160
	karaokeWindow = 640
	// YIN 搜索的基频范围（Hz）：覆盖男低音到女高音的常用音域。
	karaokeMinF0 = 70
	karaokeMaxF0 = 1100
	// yinThreshold 是 YIN 累计均值归一化差分的阈值（原论文推荐 0.1–0.15）；clarity = 1 − 该差分值。
	yinThreshold = 0.15
	// 有声帧的判定：清晰度不低于 karaokeClarity，且响度高于噪声底（第 10 百分位）karaokeGateDB 以上、不低于 −50 dBFS。
	karaokeClarity = 0.7
	karaokeGateDB  = 12
	karaokeFloorDB = -50
)

// KaraokeFrame 是一帧演唱：T 为歌曲时间（秒，未扣除延迟）；F0 为基频（Hz，0 为没有检测到周期）；
// Clarity 为 0–1 的周期清晰度；DB 为帧 RMS（dBFS）。
type KaraokeFrame struct {
	T, F0, Clarity, DB float64
}

// KaraokeSession 是一次跟唱的录音与逐帧特征。Push 在录音线程调用，其余方法可在任意线程调用（内部加锁）。
type KaraokeSession struct {
	mu     sync.Mutex
	pcm    []float32 // 原始录音（mic.Rate），用于保存
	buf    []float32 // 降采样后尚未处理完的采样（16 kHz）；buf[0] 对应 bufPos
	frames []KaraokeFrame
	// decim 是录音采样率对 KaraokeRate 的倍数；carry 保存上一个缓冲剩下的不足 decim 个的采样。
	decim int
	carry []float32
}

// NewKaraokeSession 创建会话；rate 是 Push 收到的录音采样率，须为 KaraokeRate 的整数倍。
func NewKaraokeSession(rate int) *KaraokeSession {
	return &KaraokeSession{decim: max(rate/KaraokeRate, 1)}
}

// Push 加入一个录音缓冲；songPos 是缓冲到达时伴奏的播放位置（秒）。samples 在返回后可被复用。
func (s *KaraokeSession) Push(samples []float32, songPos float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pcm = append(s.pcm, samples...)
	// 降采样：每 decim 个采样取平均（简单的低通，人声基频与主要谐波远低于 8 kHz）。
	in := append(s.carry, samples...)
	n := len(in) / s.decim
	for i := range n {
		sum := float32(0)
		for _, v := range in[i*s.decim : (i+1)*s.decim] {
			sum += v
		}
		s.buf = append(s.buf, sum/float32(s.decim))
	}
	s.carry = append(s.carry[:0], in[n*s.decim:]...)
	// 逐帧分析：帧 k 覆盖 buf[k·hop, k·hop+window)，中心之后还有 len(buf) − 中心 个采样已经录到。
	end := len(s.buf)
	k := 0
	for ; k*karaokeHop+karaokeWindow <= end; k++ {
		w := s.buf[k*karaokeHop : k*karaokeHop+karaokeWindow]
		f0, clarity := yinPitch(w, KaraokeRate)
		center := k*karaokeHop + karaokeWindow/2
		s.frames = append(s.frames, KaraokeFrame{
			T:  songPos - float64(end-center)/KaraokeRate,
			F0: f0, Clarity: clarity, DB: rmsDB(w[karaokeWindow/2-karaokeHop/2 : karaokeWindow/2+karaokeHop/2]),
		})
	}
	s.buf = append(s.buf[:0], s.buf[k*karaokeHop:]...)
}

// Frames 返回目前为止的帧（副本）。
func (s *KaraokeSession) Frames() []KaraokeFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

// Recording 返回原始录音（副本，采样率为创建时的 rate）。
func (s *KaraokeSession) Recording() []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pcm)
}

// yinPitch 返回窗口 w 的基频（Hz，没有足够清晰的周期时为 0）与清晰度（0–1）。
// 步骤见 YIN 论文：差分函数 → 累计均值归一化 → 绝对阈值取第一个谷 → 抛物线插值。
func yinPitch(w []float32, rate int) (float64, float64) {
	minLag, maxLag := rate/karaokeMaxF0, rate/karaokeMinF0
	half := len(w) - maxLag - 1 // 差分用到 w[i+maxLag+1]
	if half <= 0 {
		return 0, 0
	}
	d := make([]float64, maxLag+2)
	for lag := 1; lag <= maxLag+1; lag++ {
		sum := 0.0
		for i := range half {
			x := float64(w[i] - w[i+lag])
			sum += x * x
		}
		d[lag] = sum
	}
	// 累计均值归一化：d'(τ) = d(τ) · τ / Σ_{j≤τ} d(j)。
	cum := 0.0
	norm := make([]float64, len(d))
	norm[0] = 1
	for lag := 1; lag < len(d); lag++ {
		cum += d[lag]
		if cum <= 0 {
			norm[lag] = 1
			continue
		}
		norm[lag] = d[lag] * float64(lag) / cum
	}
	best := -1
	for lag := minLag; lag <= maxLag; lag++ {
		if norm[lag] < yinThreshold {
			for lag+1 <= maxLag && norm[lag+1] < norm[lag] {
				lag++ // 走到这个谷的底
			}
			best = lag
			break
		}
	}
	if best < 0 {
		// 没有低于阈值的谷：取全局最小值，只用于给出清晰度（低清晰度帧不计入音准）。
		best = minLag
		for lag := minLag; lag <= maxLag; lag++ {
			if norm[lag] < norm[best] {
				best = lag
			}
		}
		return 0, max(0, 1-norm[best])
	}
	// 抛物线插值求谷底的小数延迟。
	tau := float64(best)
	if best > 1 && best < len(norm)-1 {
		a, b, c := norm[best-1], norm[best], norm[best+1]
		if den := a - 2*b + c; den != 0 {
			tau += 0.5 * (a - c) / den
		}
	}
	return float64(rate) / tau, max(0, 1-norm[best])
}

// rmsDB 是采样的 RMS（dBFS），静音为 −120。
func rmsDB(w []float32) float64 {
	sum := 0.0
	for _, v := range w {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return -120
	}
	return 10 * math.Log10(sum/float64(len(w)))
}

// KaraokeReport 是一次跟唱的评价。百分比为 0–1；音分正值为偏高。
type KaraokeReport struct {
	Score float64 // 总分 0–100：音准 50%、覆盖 20%、气息 30%
	// Lag 是扣除的整体延迟（秒，演唱比伴奏晚为正）；Octave 是演唱相对原调的八度（−1 为低八度唱）。
	Lag    float64
	Octave int
	// Start / End 是评价的歌曲时间范围（秒）。
	Start, End float64
	// 音准：落在音符内的有声帧中，偏差在 ±50 音分内（InTune）与 ±100 音分内（Close）的比例；
	// MeanAbsCents 为平均绝对偏差，Bias 为偏差中位数（整体偏高或偏低）。
	InTune, Close, MeanAbsCents, Bias float64
	// Coverage 是范围内旋律音的时长中唱出了声音的比例。
	Coverage float64
	// 气息：长音（≥ karaokeLongNote）中坚持到 80% 时长的比例（Sustain）；长音中段的音高抖动（音分标准差）与
	// 响度起伏（dB 标准差）；LongNotes 为参与评价的长音数。
	Sustain, Wobble, Steadiness float64
	LongNotes                   int
	Breath                      float64 // 气息分 0–1，由以上三项换算
	Phrases                     []KaraokePhrase
	Voiced                      int // 有声帧数（为 0 时其余指标无意义：没有录到演唱）
}

// KaraokePhrase 是一个乐句的评价：Lyric 为乐句的歌词（没有歌词时为空），Score 为 0–100。
type KaraokePhrase struct {
	Start, End       float64
	Lyric            string
	InTune, Coverage float64
	Score            float64
}

const (
	// karaokeLongNote 是评价气息的长音最短时长（秒）。
	karaokeLongNote = 0.6
	// karaokePhraseGap 是划分乐句的最短空隙（秒）：旋律音之间停顿到此长度视为换气。
	karaokePhraseGap = 0.35
	// karaokeMaxLag 是延迟搜索的范围（秒）：输出 + 输入延迟加上跟唱的反应，通常在 0.05–0.4 秒。
	karaokeMinLag, karaokeMaxLag = -0.15, 0.6
)

// AnalyzeKaraoke 对照旋律音 notes 评价帧 frames（只评价 [start, end] 内的旋律音）。没有有声帧时 Voiced 为 0。
func AnalyzeKaraoke(notes []MelodyNote, frames []KaraokeFrame, start, end float64) KaraokeReport {
	r := KaraokeReport{Start: start, End: end}
	ns := make([]MelodyNote, 0, len(notes))
	for _, n := range notes {
		if n.End > start && n.Start < end && n.End > n.Start {
			ns = append(ns, n)
		}
	}
	slices.SortFunc(ns, func(a, b MelodyNote) int { return cmp.Compare(a.Start, b.Start) })

	// 1. 有声帧：清晰度足够、响度高于噪声底。
	var dbs []float64
	for _, f := range frames {
		dbs = append(dbs, f.DB)
	}
	gate := max(percentile(dbs, 0.1)+karaokeGateDB, karaokeFloorDB)
	var voiced []KaraokeFrame
	for _, f := range frames {
		if f.F0 > 0 && f.Clarity >= karaokeClarity && f.DB >= gate {
			voiced = append(voiced, f)
		}
	}
	slices.SortStableFunc(voiced, func(a, b KaraokeFrame) int { return cmp.Compare(a.T, b.T) })
	r.Voiced = len(voiced)
	if len(voiced) == 0 || len(ns) == 0 {
		return r
	}

	// 2. 延迟与八度：在搜索范围内逐 10 ms 试偏移，取落在音符内且按八度折叠后偏差 < 100 音分的帧最多的偏移；
	// 八度取这些帧的八度差的众数。
	bestLag, bestHits := 0.0, -1
	for lag := karaokeMinLag; lag <= karaokeMaxLag+1e-9; lag += 0.01 {
		hits := 0
		for _, f := range voiced {
			if n := noteAt(ns, f.T-lag); n != nil {
				if _, c := foldCents(f.F0, n.Pitch); math.Abs(c) < 100 {
					hits++
				}
			}
		}
		if hits > bestHits {
			bestLag, bestHits = lag, hits
		}
	}
	r.Lag = math.Round(bestLag*100) / 100
	octaves := map[int]int{}
	for _, f := range voiced {
		if n := noteAt(ns, f.T-r.Lag); n != nil {
			if o, c := foldCents(f.F0, n.Pitch); math.Abs(c) < 100 {
				octaves[o]++
			}
		}
	}
	for o, k := range octaves {
		if k > octaves[r.Octave] || (k == octaves[r.Octave] && abs(o) < abs(r.Octave)) {
			r.Octave = o
		}
	}

	// 3. 音准：落在音符内的有声帧相对「音符音高 + 八度」的偏差。跨八度的帧（唱错八度或检测错误）按折叠后计算。
	cents := func(f KaraokeFrame, n *MelodyNote) float64 {
		c := 1200*math.Log2(f.F0/midiFreqF(n.Pitch)) - 1200*float64(r.Octave)
		return c - 1200*math.Round(c/1200)
	}
	var all []float64
	for _, f := range voiced {
		if n := noteAt(ns, f.T-r.Lag); n != nil {
			all = append(all, cents(f, n))
		}
	}
	r.InTune, r.Close, r.MeanAbsCents, r.Bias = tuneStats(all)

	// 4. 覆盖：每个音符内有声帧的时长占比，按音符时长加权（帧长 10 ms）。
	voicedAt := func(a, b float64) float64 { // [a, b) 内有声帧的时长
		lo, _ := slices.BinarySearchFunc(voiced, a+r.Lag, func(f KaraokeFrame, t float64) int { return cmp.Compare(f.T, t) })
		k := 0
		for i := lo; i < len(voiced) && voiced[i].T < b+r.Lag; i++ {
			k++
		}
		return float64(k) * karaokeHop / KaraokeRate
	}
	total, sung := 0.0, 0.0
	for _, n := range ns {
		a, b := max(n.Start, start), min(n.End, end)
		total += b - a
		sung += min(voicedAt(a, b), b-a)
	}
	if total > 0 {
		r.Coverage = sung / total
	}

	// 5. 气息：长音是否坚持到 80% 时长，中段（20%–80%）的音高抖动与响度起伏。
	var sustained, wobble, steady []float64
	for i := range ns {
		n := &ns[i]
		d := n.End - n.Start
		if d < karaokeLongNote || n.Start < start || n.End > end {
			continue
		}
		r.LongNotes++
		tail := voicedAt(n.Start+0.6*d, n.Start+0.8*d) / (0.2 * d) // 60%–80% 段仍在发声的比例
		sustained = append(sustained, min(tail, 1))
		var cs, ds []float64
		for _, f := range voiced {
			if t := f.T - r.Lag; t >= n.Start+0.2*d && t < n.Start+0.8*d {
				cs, ds = append(cs, cents(f, n)), append(ds, f.DB)
			}
		}
		if len(cs) >= 10 {
			wobble, steady = append(wobble, stddev(cs)), append(steady, stddev(ds))
		}
	}
	r.Sustain, r.Wobble, r.Steadiness = mean(sustained), mean(wobble), mean(steady)
	if r.LongNotes > 0 {
		// 抖动 15 音分以内、响度起伏 2 dB 以内为满分，60 音分 / 8 dB 为 0（经验值：自然颤音的标准差约 20–40 音分）。
		w := 1 - clamp((r.Wobble-15)/45, 0, 1)
		st := 1 - clamp((r.Steadiness-2)/6, 0, 1)
		if len(wobble) == 0 {
			w, st = 0, 0
		}
		r.Breath = 0.5*r.Sustain + 0.3*w + 0.2*st
	} else {
		r.Breath = r.Coverage // 没有长音时气息不单独评价，按覆盖计
	}

	// 6. 乐句：旋律音间停顿 ≥ karaokePhraseGap 处断开，各自评价音准与覆盖。
	for i := 0; i < len(ns); {
		j := i + 1
		for j < len(ns) && ns[j].Start-ns[j-1].End < karaokePhraseGap {
			j++
		}
		p := KaraokePhrase{Start: ns[i].Start, End: ns[j-1].End}
		var lyric strings.Builder
		var pc []float64
		tot, sg := 0.0, 0.0
		for k := i; k < j; k++ {
			n := &ns[k]
			lyric.WriteString(n.Lyric)
			tot += n.End - n.Start
			sg += min(voicedAt(n.Start, n.End), n.End-n.Start)
			for _, f := range voiced {
				if t := f.T - r.Lag; t >= n.Start && t < n.End {
					pc = append(pc, cents(f, n))
				}
			}
		}
		p.Lyric = lyric.String()
		p.InTune, _, _, _ = tuneStats(pc)
		if tot > 0 {
			p.Coverage = sg / tot
		}
		p.Score = math.Round(100 * (0.7*p.InTune + 0.3*p.Coverage))
		r.Phrases = append(r.Phrases, p)
		i = j
	}

	// 7. 总分：音准按 ±50 音分内的比例与 ±100 音分内的比例各半。
	pitch := 0.5*r.InTune + 0.5*r.Close
	r.Score = math.Round(100 * (0.5*pitch + 0.2*r.Coverage + 0.3*r.Breath))
	return r
}

// noteAt 返回时刻 t 所在的音符（ns 按开始时间排序、互不重叠）；不在音符内时为 nil。
func noteAt(ns []MelodyNote, t float64) *MelodyNote {
	i, _ := slices.BinarySearchFunc(ns, t, func(n MelodyNote, t float64) int { return cmp.Compare(n.Start, t) })
	// i 是第一个 Start ≥ t 的音符；t 落在它之前那个音符内，或正好落在它的起点上。
	if i < len(ns) && ns[i].Start == t {
		return &ns[i]
	}
	if i > 0 && t < ns[i-1].End {
		return &ns[i-1]
	}
	return nil
}

// foldCents 返回 f0 相对 MIDI 音高 pitch 的八度差（四舍五入）与折叠到 ±600 音分内的偏差。
func foldCents(f0, pitch float64) (int, float64) {
	c := 1200 * math.Log2(f0/midiFreqF(pitch))
	o := int(math.Round(c / 1200))
	return o, c - 1200*float64(o)
}

// midiFreqF 是可含微分音的 MIDI 音高对应的频率（Hz）。
func midiFreqF(m float64) float64 { return 440 * math.Pow(2, (m-69)/12) }

// tuneStats 返回音分偏差中 ±50、±100 以内的比例、平均绝对偏差与中位数；空切片时都为 0。
func tuneStats(cs []float64) (inTune, near, meanAbs, median float64) {
	if len(cs) == 0 {
		return 0, 0, 0, 0
	}
	a, b, sum := 0, 0, 0.0
	for _, c := range cs {
		ac := math.Abs(c)
		if ac <= 50 {
			a++
		}
		if ac <= 100 {
			b++
		}
		sum += ac
	}
	n := float64(len(cs))
	return float64(a) / n, float64(b) / n, sum / n, percentile(cs, 0.5)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m, s := mean(xs), 0.0
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
