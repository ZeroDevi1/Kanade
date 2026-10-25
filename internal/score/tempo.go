package score

import (
	"math"
	"slices"
)

// 哼唱的速度与节拍（灵感页）：自由哼唱时没有节拍器，速度、逐拍时刻、拍号与弱起都从音符起点估计；人哼唱会渐快、
// 渐慢、句尾拖长，所以节拍网格跟随局部速度。节拍器模式也跑同一个节拍跟踪，但锚定在点击网格附近、只允许很小的漂移，
// 吸收整体抢拍 / 拖拍。
//
// 设计经 Claude Fable 5.1 咨询：
//  1. 全局速度候选：事件对的加权起音自相关加谐波和（抑制半速），乘以对数正态速度先验（偏好约 95 BPM）；
//     倍速 / 半速歧义不在这里裁决，而是对每个候选跑节拍跟踪后，按「有事件的拍的比例」与「起点落在拍上的比例」选择；
//  2. 节拍跟踪：状态为（帧, 与上一拍的间隔）的动态规划（Ellis 2007 的 DP 节拍跟踪思路），相邻拍长的变化与偏离全局
//     速度都有代价，休止与长音期间按上一拍长等距外推；
//  3. 拍号与强拍：按各拍的重音（时长、音高峰或上跳、前有休止）比较 3 拍与 4 拍的各相位；
//  4. 把时间按相邻拍点线性映射到拍坐标，按平均速度换算成等速的乐谱时间后沿用 quantize。

// humEvent 是一个节奏事件（切分出的音符起点）：t 为录音时间（秒），w 为权重（音节起点重、转音轻）。
type humEvent struct {
	t, w, dur float64
	midi      int
	onset     bool
}

// 全局速度候选。
const (
	tempoPairSigma = 0.04     // 起音间隔与候选拍长比较的高斯宽度（秒）
	tempoPrefBPM   = 95.0     // 速度先验的中心
	tempoPrefOct   = 0.6      // 速度先验的宽度（八度）：70–130 BPM 先验不低于 0.75
	tempoMinBPM    = 40.0     // 扫描范围
	tempoMaxBPM    = 200.0    //
	tempoScanStep  = 1.015    // 扫描的拍长比例步长（1.5%）
	tempoPeaks     = 3        // 取自相关得分的前几个局部极大
	tempoMinEvents = 4        // 事件少于此数或跨度短于 tempoMinSpan 时不估计速度，用默认速度
	tempoMinSpan   = 1.5      //
	tempoBeatWin   = 0.15     // 事件落在拍点 ±该比例拍长内算在拍上
	tempoStrongW   = 0.5      // 计入「有事件的拍」的最小事件权重
	defaultHumBPM  = 90.0     // 无法估计时的速度
	tempoMaxPair   = 3.2      // 只比较间隔不超过该值（秒）的事件对：覆盖 40 BPM 拍长的两倍以内
	tempoDedup     = 1.03     // 候选拍长相差不到 3% 视为同一个
	tempoFrame     = humFrame // 节拍跟踪的时间分辨率
)

// 节拍跟踪。自由模式与节拍器模式的参数分开：后者锚定在点击网格附近。
type trackParams struct {
	lo, hi     float64 // 拍长相对全局候选的范围
	maxChange  float64 // 相邻拍长的最大变化比例
	lambdaD    float64 // 相邻拍长变化的代价系数
	lambdaG    float64 // 拍长偏离全局候选的代价系数
	clickBoost float64 // 节拍器点击作为伪起音的权重（自由模式为 0）
}

var (
	// 自由模式：每拍变化 3% 代价约 0.05、12% 约 0.77（一个完整起音的证据为 1），再大为硬边界；偏离全局 20% 仅 0.13。
	freeTrack = trackParams{lo: 0.75, hi: 1.33, maxChange: 0.12, lambdaD: 60, lambdaG: 4}
	// 节拍器模式：只允许很小的漂移，点击作为伪起音，用户完全跟拍时拍点等于点击网格。
	clickTrack = trackParams{lo: 0.92, hi: 1.08, maxChange: 0.05, lambdaD: 150, lambdaG: 40, clickBoost: 0.5}
)

const (
	trackEventSigma = 0.035 // 起音证据的高斯宽度（秒）：哼唱起音抖动约 30–50 ms
	trackClickSigma = 0.05  // 点击伪起音的宽度（拍长的比例）
	trackMinCover   = 0.3   // 节拍器模式下有事件的拍比例低于此值视为哼唱与节拍器脱节，退回等距网格
)

// 拍号与强拍。
const (
	meterDurW     = 0.5  // 时长重音（上限 2 拍）
	meterPitchW   = 0.3  // 音高峰或上跳 ≥ 3 半音
	meterRestW    = 0.3  // 前有 ≥ 1/4 拍休止
	meterThree    = 0.15 // 3 拍须比 4 拍领先该比例的最大重音才选
	meterTie      = 0.05 // 相位平局阈
	meterMinBeats = 6
)

// humEvents 把切分出的音符变成节奏事件：音节起点权重 1，转音 0.3，再按时长加权（很短的转音几乎不影响拍点）。
func humEvents(notes []rawNote, frames []KaraokeFrame, midi []int) []humEvent {
	ev := make([]humEvent, len(notes))
	for i, n := range notes {
		dur := float64(n.b-n.a) * humFrame
		w := 0.3
		if n.onset {
			w = 1
		}
		w *= 0.6 + 0.4*min(dur/0.4, 1)
		ev[i] = humEvent{t: frames[n.a].T, w: w, dur: dur, midi: midi[i], onset: n.onset}
	}
	return ev
}

// tempoCand 是一个全局拍长候选（秒）与它的自相关得分（含速度先验）。
type tempoCand struct{ p, score float64 }

// tempoCandidates 返回全局拍长候选：自相关得分的前几个局部极大及其两倍、一半。
func tempoCandidates(ev []humEvent) []tempoCand {
	r := func(p float64) float64 {
		sum := 0.0
		for i := range ev {
			for j := i + 1; j < len(ev) && ev[j].t-ev[i].t <= tempoMaxPair; j++ {
				d := ev[j].t - ev[i].t - p
				sum += ev[i].w * ev[j].w * math.Exp(-d*d/(2*tempoPairSigma*tempoPairSigma))
			}
		}
		return sum
	}
	pref := 60 / tempoPrefBPM
	score := func(p float64) float64 {
		prior := math.Exp(-0.5 * math.Pow(math.Log2(p/pref)/tempoPrefOct, 2))
		return (r(p) + 0.5*r(2*p)) * prior
	}
	var ps, scores []float64
	for p := 60 / tempoMaxBPM; p <= 60/tempoMinBPM; p *= tempoScanStep {
		ps, scores = append(ps, p), append(scores, score(p))
	}
	type peak struct{ p, s float64 }
	var peaks []peak
	for i := range ps {
		if (i == 0 || scores[i] >= scores[i-1]) && (i == len(ps)-1 || scores[i] > scores[i+1]) && scores[i] > 0 {
			peaks = append(peaks, peak{ps[i], scores[i]})
		}
	}
	slices.SortFunc(peaks, func(a, b peak) int { return cmpDesc(a.s, b.s) })
	var out []tempoCand
	add := func(p float64) {
		if p < 60/tempoMaxBPM || p > 60/tempoMinBPM {
			return
		}
		for _, q := range out {
			if max(p, q.p)/min(p, q.p) < tempoDedup {
				return
			}
		}
		out = append(out, tempoCand{p, score(p)})
	}
	for _, pk := range peaks[:min(tempoPeaks, len(peaks))] {
		add(pk.p)
		add(2 * pk.p)
		add(pk.p / 2)
		add(3 * pk.p) // 复合拍：从八分音符的峰跳到附点四分音符
		add(pk.p / 3)
	}
	return out
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// trackBeats 用动态规划在拍长 p0 附近跟踪逐拍时刻（录音时间，秒），覆盖第一个事件到最后一个音的终点 end。
// clicks 为节拍器点击时刻（自由模式为 nil）。
func trackBeats(ev []humEvent, end, p0 float64, tp trackParams, clicks []float64) []float64 {
	if len(ev) == 0 {
		return nil
	}
	first := ev[0].t
	nf := int(math.Ceil((end+1.5*p0)/tempoFrame)) + 1
	// 起音证据 O(n)：各事件的高斯，加上点击的伪起音。
	o := make([]float64, nf)
	addGauss := func(t, w, sigma float64) {
		c := int(math.Round(t / tempoFrame))
		r := int(4*sigma/tempoFrame) + 1
		for n := max(0, c-r); n < min(nf, c+r+1); n++ {
			d := float64(n)*tempoFrame - t
			o[n] += w * math.Exp(-d*d/(2*sigma*sigma))
		}
	}
	for _, e := range ev {
		addGauss(e.t, e.w, trackEventSigma)
	}
	for _, c := range clicks {
		addGauss(c, tp.clickBoost, trackClickSigma*p0)
	}
	p0f := p0 / tempoFrame
	dmin := max(1, int(math.Floor(p0f*tp.lo)))
	dmax := max(dmin, int(math.Ceil(p0f*tp.hi)))
	nd := dmax - dmin + 1
	maxStep := math.Log(1 + tp.maxChange)
	// cost[n][k]：第 n 帧放一拍、与上一拍间隔 dmin+k 帧时的最小代价；back 为上一拍的间隔序号（-1 为第一拍）。
	cost := make([][]float64, nf)
	back := make([][]int16, nf)
	initLo := max(0, int(math.Floor((first-1.2*p0)/tempoFrame)))
	initHi := min(nf-1, int(math.Ceil((first+0.2*p0)/tempoFrame)))
	for n := range nf {
		cost[n] = make([]float64, nd)
		back[n] = make([]int16, nd)
		for k := range nd {
			d := float64(dmin + k)
			g := tp.lambdaG * math.Pow(math.Log(d/p0f), 2)
			best, arg := math.Inf(1), int16(-1)
			if n >= initLo && n <= initHi {
				best = g
			}
			if prev := n - (dmin + k); prev >= 0 {
				for pk := range nd {
					step := math.Log(d / float64(dmin+pk))
					if math.Abs(step) > maxStep || math.IsInf(cost[prev][pk], 1) {
						continue
					}
					if c := cost[prev][pk] + tp.lambdaD*step*step + g; c < best {
						best, arg = c, int16(pk)
					}
				}
			}
			cost[n][k], back[n][k] = best-o[n], arg
		}
	}
	// 终点：最后一拍在最后一个音的终点附近。
	endLo := max(0, int(math.Floor((end-0.5*p0)/tempoFrame)))
	endHi := min(nf-1, int(math.Ceil((end+1.2*p0)/tempoFrame)))
	bn, bk := -1, 0
	for n := endLo; n <= endHi; n++ {
		for k := range nd {
			if bn < 0 || cost[n][k] < cost[bn][bk] {
				bn, bk = n, k
			}
		}
	}
	if bn < 0 || math.IsInf(cost[bn][bk], 1) {
		return nil
	}
	var beats []float64
	for n, k := bn, bk; ; {
		beats = append(beats, float64(n)*tempoFrame)
		pk := back[n][k]
		if pk < 0 {
			break
		}
		n, k = n-(dmin+k), int(pk)
	}
	slices.Reverse(beats)
	return beats
}

// beatFit 返回节拍序列的两个指标：有（足够重的）事件落在拍上的拍的比例，以及音节起点权重中落在拍上（半拍算一半）的比例。
func beatFit(ev []humEvent, beats []float64) (coverage, onbeat float64) {
	if len(beats) < 2 {
		return 0, 0
	}
	hit := 0
	for k, b := range beats {
		l := beatLen(beats, k)
		for _, e := range ev {
			if e.w >= tempoStrongW && math.Abs(e.t-b) <= tempoBeatWin*l {
				hit++
				break
			}
		}
	}
	var on, total float64
	for _, e := range ev {
		if !e.onset {
			continue
		}
		total += e.w
		u := beatCoord(beats, e.t)
		frac := u - math.Round(u)
		switch {
		case math.Abs(frac) <= tempoBeatWin:
			on += e.w
		case math.Abs(math.Abs(frac)-0.5) <= tempoBeatWin, math.Abs(math.Abs(frac)-1.0/3) <= subThirdTol:
			on += 0.5 * e.w // 半拍（简单拍）或三分之一拍（复合拍）上的起音算一半
		}
	}
	if total > 0 {
		onbeat = on / total
	}
	return float64(hit) / float64(len(beats)), onbeat
}

// beatLen 返回第 k 拍的拍长（与下一拍的间隔；最后一拍取前一个间隔）。
func beatLen(beats []float64, k int) float64 {
	if k+1 < len(beats) {
		return beats[k+1] - beats[k]
	}
	return beats[k] - beats[k-1]
}

// beatCoord 把时刻 t 映射到拍坐标（拍序号 + 拍内比例），相邻拍点之间线性插值，范围外按两端的拍长外推。
func beatCoord(beats []float64, t float64) float64 {
	n := len(beats)
	switch {
	case n == 0:
		return 0
	case n == 1:
		return t - beats[0]
	}
	k := 0
	switch {
	case t < beats[0]:
		k = 0
	case t >= beats[n-1]:
		k = n - 2
	default:
		k, _ = slices.BinarySearch(beats, t)
		if k == n || beats[k] > t {
			k--
		}
		k = min(k, n-2)
	}
	return float64(k) + (t-beats[k])/(beats[k+1]-beats[k])
}

// beatTime 是 beatCoord 的反映射。
func beatTime(beats []float64, u float64) float64 {
	n := len(beats)
	switch {
	case n == 0:
		return u
	case n == 1:
		return beats[0] + u
	}
	k := min(max(int(math.Floor(u)), 0), n-2)
	return beats[k] + (u-float64(k))*(beats[k+1]-beats[k])
}

// estimateMeter 返回每小节拍数（3 或 4）与第一个强拍的拍序号相位 p（0 ≤ p < M）。
func estimateMeter(ev []humEvent, beats []float64, choices []int) (int, int) {
	k1 := int(math.Round(beatCoord(beats, ev[0].t)))
	m0 := choices[0]
	fallback := func() (int, int) { return m0, ((k1 % m0) + m0) % m0 }
	if len(ev) < meterMinBeats || len(beats) < 8 {
		return fallback()
	}
	// 各拍的重音：落在拍点附近的事件中最重的一个。
	acc := make([]float64, len(beats))
	for i, e := range ev {
		u := beatCoord(beats, e.t)
		b := int(math.Round(u))
		if b < 0 || b >= len(beats) || math.Abs(u-float64(b)) > tempoBeatWin {
			continue
		}
		l := beatLen(beats, b)
		a := 1 + meterDurW*min(e.dur/l, 2)
		if i > 0 && (e.midi-ev[i-1].midi >= 3 || (i+1 < len(ev) && e.midi > ev[i-1].midi && e.midi > ev[i+1].midi)) {
			a += meterPitchW
		}
		if i == 0 || e.t-(ev[i-1].t+ev[i-1].dur) >= 0.25*l {
			a += meterRestW // 第一个音前是录音开头的静音，也算前有休止
		}
		acc[b] = max(acc[b], e.w*a)
	}
	// 只比较第一个到最后一个起音之间的拍：两端外推出的空拍不代表弱拍，会拉低所在相位的平均重音。
	kLast := int(math.Round(beatCoord(beats, ev[len(ev)-1].t)))
	lo, hi := max(k1, 0), min(kLast, len(beats)-1)
	if hi-lo+1 < meterMinBeats {
		return fallback()
	}
	acc = acc[lo : hi+1]
	maxA := slices.Max(acc)
	if maxA-slices.Min(acc) < meterTie*maxA || maxA == 0 {
		return fallback()
	}
	score := func(m, p int) float64 {
		var on, off float64
		var non, noff int
		for i, a := range acc {
			if b := lo + i; ((b-p)%m+m)%m == 0 {
				on, non = on+a, non+1
			} else {
				off, noff = off+a, noff+1
			}
		}
		if non == 0 || noff == 0 {
			return math.Inf(-1)
		}
		return on/float64(non) - off/float64(noff)
	}
	best := func(m int) (int, float64) {
		bp, bs := 0, math.Inf(-1)
		for p := range m {
			if s := score(m, p); s > bs {
				bp, bs = p, s
			}
		}
		// 平局时优先让第一个音所在拍是强拍，其次选弱起拍数最少的相位。
		pickup := func(p int) int { return ((k1-p)%m + m) % m }
		for p := range m {
			if s := score(m, p); s >= bs-meterTie*maxA && pickup(p) < pickup(bp) {
				bp = p
			}
		}
		return bp, score(m, bp)
	}
	// 第一个选择是默认的；其余的须领先 meterThree 的最大重音才选。
	bm := m0
	bp, bs := best(m0)
	for _, m := range choices[1:] {
		if p, sc := best(m); sc > bs+meterThree*maxA {
			bm, bp, bs = m, p, sc
		}
	}
	return bm, bp
}

// 复合拍（6/8）的判定（Fable 5.1 的建议值）：拍长在附点四分音符的常见范围内，拍内的起音足够多，且多数落在三分之一、
// 三分之二拍处、很少落在半拍处；跟踪锁在八分音符上（拍很短、几乎全在拍上）时改按三拍一组的重音判断。
const (
	compoundMinP  = 0.50 // 附点四分音符的拍长范围（秒，48–120 BPM）
	compoundMaxP  = 1.25
	subOnTol      = 0.12 // 落在拍上的容差（拍）
	subThirdTol   = 0.08 // 落在三分点 / 半拍 / 四分点的容差（拍）
	subHalfTol    = 0.08
	subQuarterTol = 0.06
	subMinOff     = 2.0  // 拍内起音的最小总权重
	subMinOffFrac = 0.25 // 拍内起音占全部起音权重的最小比例
	subThirdRatio = 0.60 // 三分点起音占拍内起音的最小比例
	subHalfMax    = 0.25 // 半拍起音占拍内起音的最大比例
	pulseMinP     = 0.18 // 跟踪锁在八分音符上时的拍长范围（秒）
	pulseMaxP     = 0.36
)

// compoundFeel 报告拍内的起音是否呈三分（复合拍）：见上面的常量。p0 为拍长（秒）。
func compoundFeel(ev []humEvent, beats []float64, p0 float64) bool {
	if p0 < compoundMinP || p0 > compoundMaxP {
		return false
	}
	var m3, m2, m4, on float64
	for _, e := range ev {
		if !e.onset || e.w < tempoStrongW {
			continue
		}
		on += e.w
		u := beatCoord(beats, e.t)
		f := u - math.Floor(u)
		switch {
		case min(f, 1-f) <= subOnTol:
		case math.Abs(f-1.0/3) <= subThirdTol || math.Abs(f-2.0/3) <= subThirdTol:
			m3 += e.w
		case math.Abs(f-0.5) <= subHalfTol:
			m2 += e.w
		case math.Abs(f-0.25) <= subQuarterTol || math.Abs(f-0.75) <= subQuarterTol:
			m4 += e.w
		}
	}
	off := m3 + m2 + m4
	return off >= subMinOff && off >= subMinOffFrac*on && m3 >= subThirdRatio*off && m2 <= subHalfMax*off
}

// groupPulses 把锁在八分音符上的拍点每三个取一个（相位按重音选），返回附点四分音符的拍点。
func groupPulses(ev []humEvent, beats []float64) []float64 {
	_, p := estimateMeter(ev, beats, []int{3})
	var out []float64
	for k := ((p % 3) + 3) % 3; k < len(beats); k += 3 {
		out = append(out, beats[k])
	}
	return out
}

// rhythm 估计（或在节拍器模式下校准）逐拍时刻，返回拍点（录音时间）、乐谱原点的拍坐标与更新后的节拍设置。
// 自由模式下 s.BPM 非 0 时作为速度提示（跳过候选搜索），s.BeatsPerBar 非 0 时固定拍号。
// ok 为 false 表示节拍器模式下哼唱与节拍器脱节，应退回等距网格。
func rhythm(ev []humEvent, end float64, s HumSettings) (beats []float64, origin float64, out HumSettings, ok bool) {
	out = s
	if !s.Free {
		beat := s.Beat()
		var clicks []float64
		for k := 0; k <= s.Bars*s.BeatsPerBar; k++ {
			clicks = append(clicks, s.Start()+float64(k)*beat)
		}
		beats = trackBeats(ev, end, beat, clickTrack, clicks)
		if cov, _ := beatFit(ev, beats); len(beats) < 2 || cov < trackMinCover {
			return nil, 0, s, false
		}
		// 原点：离预备拍结束最近的拍。
		best := 0
		for k, b := range beats {
			if math.Abs(b-s.Start()) < math.Abs(beats[best]-s.Start()) {
				best = k
			}
		}
		return beats, float64(best), s, true
	}
	// 自由模式：选拍长。
	var p0 float64
	switch {
	case s.BPM > 0:
		p0 = 60 / s.BPM
	case len(ev) < tempoMinEvents || ev[len(ev)-1].t-ev[0].t < tempoMinSpan:
		p0 = 60 / defaultHumBPM
	default:
		cands := tempoCandidates(ev)
		top := 0.0
		for _, c := range cands {
			top = max(top, c.score)
		}
		bestTotal := math.Inf(-1)
		for _, c := range cands {
			b := trackBeats(ev, end, c.p, freeTrack, nil)
			cov, on := beatFit(ev, b)
			// 倍速候选会有一半的拍空着（coverage 低），半速候选的起点多落在半拍上（onbeat 低）。
			if t := c.score / top * (0.3 + 0.7*cov) * (0.3 + 0.7*on); t > bestTotal {
				bestTotal, p0, beats = t, c.p, b
			}
		}
		if p0 == 0 {
			p0 = 60 / defaultHumBPM
		}
	}
	if beats == nil {
		beats = trackBeats(ev, end, p0, freeTrack, nil)
	}
	if len(beats) < 2 {
		// 跟踪失败（只有一两个音）：从第一个音起按 p0 等距。
		beats = nil
		for t := ev[0].t; t <= end+p0; t += p0 {
			beats = append(beats, t)
		}
		if len(beats) < 2 {
			beats = append(beats, ev[0].t+p0)
		}
	}
	// 复合拍：用户指定了 6/8，或自动判断时拍内起音呈三分；跟踪锁在八分音符上时先三个一组合成附点四分音符的拍。
	compound := s.Compound && s.BeatsPerBar > 0
	pulse := p0 >= pulseMinP && p0 <= pulseMaxP && len(beats) >= 6
	switch {
	case compound && pulse:
		beats = groupPulses(ev, beats)
	case s.BeatsPerBar == 0 && compoundFeel(ev, beats, p0):
		compound = true
	case s.BeatsPerBar == 0 && pulse:
		if m, _ := estimateMeter(ev, beats, []int{2, 3}); m == 3 {
			beats, compound = groupPulses(ev, beats), true
		}
	}
	if len(beats) < 2 {
		beats = append(beats, beats[len(beats)-1]+3*p0)
	}
	m, p := s.BeatsPerBar, 0
	k1 := int(math.Round(beatCoord(beats, ev[0].t)))
	switch {
	case compound:
		m, p = estimateMeter(ev, beats, []int{2})
	case m == 0:
		m, p = estimateMeter(ev, beats, []int{4, 3})
	default:
		p = ((k1 % m) + m) % m
	}
	// 原点：不晚于第一个音所在拍的最近小节线（可能为负，表示弱起前补的空拍）。
	o := p + m*int(math.Floor(float64(k1-p)/float64(m))) + s.BarShift
	if o > k1 {
		o -= m // 小节线不能移到第一个音之后
	}
	avg := (beats[len(beats)-1] - beats[0]) / float64(len(beats)-1)
	out.BPM = math.Round(60 / avg)
	out.BeatsPerBar, out.Compound = m, compound
	out.CountIn = 0
	// 小节数：最后一个音的终点之后不足半拍的余音不再占一个小节（量化时截到小节末）。
	out.Bars = max(1, int(math.Ceil((beatCoord(beats, end)-float64(o)-0.5)/float64(m))))
	return beats, float64(o), out, true
}
