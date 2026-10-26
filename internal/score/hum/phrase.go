// Package hum 把灵感页的哼唱整理成乐句：节拍器与自由哼唱的节拍跟踪、音符切分与量化、转音分组、拟声字，
// 以及哼唱旋律的合并精简。
package hum

import (
	"cmp"
	"math"
	"slices"
	"sort"

	"kanade/internal/score"
	"kanade/internal/score/karaoke"
	"kanade/internal/score/stat"
)

// 哼唱转乐句（灵感页）：把节拍器下哼唱的逐帧特征（KaraokeFrame，YIN 每 10 ms 一帧）整理成量化到网格的音符，
// 并按音节分组（同一音节的多个音 = 转音 / 连音，只配一个拟声字）。离线处理，录完后整段计算。
//
// 流水线（算法设计经 Claude Fable 5.1 咨询，阈值为按 10 ms 帧与典型人声颤音推导的经验值，可用真实录音再调）：
//  1. 有声判定（滞回 + 短缺口桥接）、八度错误修正（持续且清晰的八度 / 五度跳进保留）、5 帧中值平滑 → 每帧音分（MIDI×100）；
//  2. 音节起音检测：响度回升、响度谷深、清晰度回升三项证据叠加（辅音造成的谷）；
//  3. 每个有声段独立做只有半音状态的 Viterbi：起音帧换音免费，其余帧换音有代价，颤音因停留代价小于两次换音而不被切开；
//     再合并过短的音（滑音归入目标音）、同音相连与阶梯状滑音；
//  4. 切分前先扣除帧级音准漂移曲线（局部圆均值，相位展开）；代表音高（去头尾后按清晰度加权的中位数）→
//     剩余的整体调音残差（圆均值）→ 取整 → Krumhansl–Kessler 估调 →
//     调性明确时把唱得含糊的调外音吸附到调内；
//  5. 全局节奏偏移（起点相对网格的圆均值）→ 按拍选 1/16 或八分三连音网格 → 边界动态规划量化（距离 + 拍位先验），
//     保证不重叠、不归零。
// 参考：Mauch & Dixon 2014（pYIN）；Mauch et al. 2015（Tony 的音符 HMM）；Krumhansl & Kessler 1982（调性轮廓）。

// 有声判定。
const (
	humClarityEnter = 0.70 // 进入有声段的清晰度
	humClarityStay  = 0.55 // 保持有声的清晰度（滞回，避免起音、辅音处抖动）
	humGateDB       = 12   // 响度门限：噪声底（第 10 百分位）以上
	humFloorDB      = -50
	humEnterFrames  = 3 // 连续满足才进入
	humExitFrames   = 5 // 连续不满足才退出（50 ms）；更短的缺口按线性插值补音高
)

// 音高平滑。
const (
	humOctaveTol = 100 // 与参考音高相差约一个八度（±该音分）时视为八度错误
	humFifthTol  = 60  // 相差约纯五度且清晰度低于 humFifthClarity 时视为五度错误（YIN 偶见 ×3/2）
	humFifthClar = 0.8
	humMedianWin = 5 // 中值滤波窗（帧）：短于颤音半周期（70–100 ms），不削峰
	// 真实跳进的判据：停在远侧至少 80 ms 且平均清晰度够高。♩=120 的十六分音符 125 ms，扣掉换音过渡与下一字的
	// 辅音后只剩约 90 ms，平均清晰度也被辅音帧（约 0.3）拉到 0.75 左右；YIN 的倍频突跳通常只有 1–5 帧，
	// 更长的倍频错误多出现在气声里，清晰度低于该值。
	humLeapFrames  = 8
	humLeapClarity = 0.7
)

// 音节起音。三项证据各自归一化到约 1，单项（例如颤音带来的 ±2 dB 起伏）不足以触发。
const (
	humRiseDBRef     = 6.0 // 80 ms 内响度回升量的参照（dB）
	humDipDBMin      = 3.0 // 响度谷深超过该值才计入（dB）
	humDipDBRef      = 4.0
	humClarityRise   = 0.3 // 清晰度回升量的参照
	humOnsetThresh   = 1.0
	humOnsetGap      = 10 // 相邻起音的最短间隔（帧，100 ms）
	humOnsetAlignWin = 5  // 换音处 ±该帧数内有起音时，边界移到起音（50 ms）
	humOnsetRefine   = 4  // 起音时刻在证据峰值之前最多回溯的帧数
)

// 音符切分。
const (
	humMinMIDI, humMaxMIDI = 36, 84 // 状态范围 C2–C6
	humSigma               = 60.0   // 发射代价的音分标准差
	humEmitMax             = 8.0    // 发射代价上限（防离群帧）
	humSwitch              = 6.0    // 非起音帧的换音代价：换 1 个半音需维持约 90 ms 才成立，大跳几帧即成立
	humSwitchSpan          = 0.3    // 按音程（最多 12 半音）增加的换音代价
	humMinNote             = 6      // 短于该帧数（60 ms）且非音节起点的音并入邻音
	humMergeGap            = 5      // 同音高、无起音、间隔不超过该帧数的相邻音合并
	humGlideStep           = 12     // 阶梯状滑音中每级的最长帧数（120 ms）
)

// 代表音高、调音与调性。
const (
	humTrimHead     = 3   // 去掉音符开头的帧（30 ms，YIN 窗跨过起音瞬态）
	humTrimHeadFrac = 0.2 // 短于 15 帧的音去掉开头的比例
	humTrimTail     = 1
	humTuneMinR     = 0.4 // 调音偏移的圆均值集中度低于此值时不校正
	// 音准漂移曲线的时间窗（秒）：约覆盖前后各 2–4 个音，单个唱偏的音只把偏移拉动一小部分；
	// 太窄会把唱偏的音当成漂移吸收掉，太宽跟不上越唱越低。
	humDriftSigma  = 1.0
	humDriftAvg    = 19  // 残差的滑动平均窗（帧，190 ms ≈ 5–6 Hz 颤音的一个周期）
	humDriftStable = 3   // 稳定帧：平滑后每帧变化不到该音分（漂移通常 < 0.3 音分 / 帧；一个半音的换音在平均窗内约 5 音分 / 帧）
	humKeyMinCorr  = 0.6 // 调性明确：最佳调的相关系数
	humKeyMargin   = 0.1 // 且领先第二名
	humSnapDev     = 35  // 取整残差至少该音分、且指向的相邻半音在调内时才吸附
)

// 量化。
const (
	humTripletBias = 0.15 // 选三连音网格的额外代价（以 1/16 网格为单位），偏向直拍
	humTripletLone = 0.15 // 拍内只有一个边界时三连音的额外代价：唱准的四分三连音（离三分点不到约 10 ms）仍保留
	humShiftMinDur = 0.1  // 参与估计节奏偏移的音符最短时长（秒）
	humShiftMinR   = 0.3  // 节奏偏移的圆均值集中度低于此值时不校正
	humLegatoGap   = 0.5  // 与下一音的间隙小于半个网格时连奏
	// 拍位先验（以 1/16 网格为单位的额外代价）：八分音符位置比十六分音符位置多 0.2 格的容差，离八分音符位置不到
	// 0.6 格（离十六分音符位置超过 0.4 格）的边界取八分音符位置；唱准的十六分音符（距离约 0）不受影响。
	// 合成哼唱（边界抖动 σ = 30–45 ms，♩=120）上调出的值，见 phrase_test.go。
	humMetricHalf    = 0.1
	humMetricTriplet = 0.3
	humMetricQuarter = 0.3
)

// humFrame 是录音的帧移（秒），与 KaraokeSession 一致。
const humFrame = 0.01

// Hum 是整理好的乐句。Notes 的时间是相对第一小节第一拍的秒数（已量化），Pitch 为整数 MIDI，Slur 标出同一音节的
// 后续音；Lyric 由 FillerLyrics 填写。
type Hum struct {
	Settings HumSettings
	Notes    []score.MelodyNote
	// TonicPC / Mode 是估计的调（mode 为 major / minor）；KeyClear 表示调性明确（调外音吸附只在此时进行）。
	TonicPC  int
	Mode     string
	KeyClear bool
	// Tuning 是整体音高偏移（音分，正值为偏高；有漂移时是逐帧偏移在有声帧上的平均）；Shift 是整体节奏偏移（秒，
	// 负值为抢拍）。都已在结果中扣除。
	Tuning, Shift float64
	// Drift 是逐帧的音高偏移（音分，与整理时的 frames 一一对应，已含 Tuning 的残差校正），跟随越唱越高 / 越低；
	// 旧记录没有时为 nil，按 Tuning 处理（见 TuningAt）。
	Drift  []float64 `json:",omitempty"`
	Voiced int       // 有声帧数；为 0 时没有录到哼唱
	// Beats 是跟踪出的逐拍时刻（录音时间，秒），Origin 是乐谱第一拍在其中的拍坐标（可为负：弱起前补的空拍）。
	// 乐谱按 Settings.BPM 等速；RecTime / ScoreTime 在乐谱时间与录音时间之间换算。Beats 为 nil 时为等距网格。
	Beats  []float64
	Origin float64
}

// TuningAt 返回第 i 帧（整理时的 frames 下标）已扣除的音高偏移（音分）。
func (h Hum) TuningAt(i int) float64 {
	if i >= 0 && i < len(h.Drift) {
		return h.Drift[i]
	}
	return h.Tuning
}

// DriftSpan 返回从开头到结尾音准漂移了多少（音分，正值为越唱越高）；没有逐帧偏移时为 0。
func (h Hum) DriftSpan() float64 {
	if len(h.Drift) == 0 {
		return 0
	}
	return h.Drift[len(h.Drift)-1] - h.Drift[0]
}

// RecTime 把乐谱时间（秒，相对第一拍）换算成录音时间。
func (h Hum) RecTime(tau float64) float64 {
	if h.Beats == nil {
		return tau + h.Settings.Start() + h.Shift
	}
	return beatTime(h.Beats, h.Origin+tau/h.Settings.Beat())
}

// ScoreTime 是 RecTime 的反映射。
func (h Hum) ScoreTime(t float64) float64 {
	if h.Beats == nil {
		return t - h.Settings.Start() - h.Shift
	}
	return (beatCoord(h.Beats, t) - h.Origin) * h.Settings.Beat()
}

// rawNote 是切分阶段的音符：帧区间 [a, b)、半音 m、是否为音节起点。
type rawNote struct {
	a, b  int
	m     int
	onset bool
	cents float64 // 代表音高（音分）
}

// TranscribeHum 把录音帧整理成乐句。frames 的时间轴是节拍器的播放位置（含预备拍）。
func TranscribeHum(frames []karaoke.KaraokeFrame, s HumSettings) Hum {
	return TranscribeHumAudio(frames, nil, 0, s)
}

// TranscribeHumAudio 同 TranscribeHum，并用原始录音 pcm（rate Hz，样本序号 ≈ 帧时间 × rate）的频谱通量补充音节起音
// 的证据（连续哼「噜噜」这类字之间没有响度谷的唱法）。pcm 为 nil 时与 TranscribeHum 相同。
func TranscribeHumAudio(frames []karaoke.KaraokeFrame, pcm []float32, rate int, s HumSettings) Hum {
	h := Hum{Settings: s, Mode: "major"}
	if s.Free {
		// 没有录到音符时提前返回：自由模式的设置里速度与拍号可能还是「自动」（0），先给出默认值，结果仍是合法的节拍。
		h.Settings.BPM = cmp.Or(s.BPM, defaultHumBPM)
		h.Settings.BeatsPerBar = cmp.Or(s.BeatsPerBar, 4)
		h.Settings.Bars, h.Settings.CountIn = 1, 0
	}
	if len(frames) == 0 {
		return h
	}
	cents, voiced := humPitch(frames)
	for _, v := range voiced {
		if v {
			h.Voiced++
		}
	}
	if h.Voiced == 0 {
		return h
	}
	// 先扣除音准漂移再切分：Viterbi 的状态是绝对半音，偏了近半个半音的长音会随颤音在两个状态间来回切。
	drift := humDrift(cents, voiced, frames)
	var dsum float64
	for i, v := range voiced {
		if v {
			cents[i] -= drift[i]
			dsum += drift[i]
		}
	}
	h.Tuning = dsum / float64(h.Voiced)
	onset := humOnsets(frames, voiced, spectralFlux(pcm, rate, frames))
	notes := humSegment(cents, voiced, onset)
	if len(notes) == 0 {
		return h
	}
	for i := range notes {
		notes[i].cents = humNoteCents(cents, frames, notes[i])
	}
	midi := h.tune(notes)
	// 逐帧偏移 = 漂移曲线 + tune 校正的残差（h.Tuning 里减去漂移平均的部分）；保留一位小数，记录里不占太多空间。
	resid := h.Tuning - dsum/float64(h.Voiced)
	h.Drift = drift
	for i := range h.Drift {
		h.Drift[i] = math.Round((h.Drift[i]+resid)*10) / 10
	}
	starts, ends := make([]float64, len(notes)), make([]float64, len(notes))
	for i, n := range notes {
		starts[i], ends[i] = frames[n.a].T, frames[n.b-1].T+humFrame
	}
	// 节拍跟踪：把录音时间按逐拍时刻映射成等速的乐谱时间（相对第一拍）后量化；节拍器模式下哼唱与节拍器脱节时
	// 退回等距网格（扣除预备拍，量化时再估计整体偏移）。
	ev := humEvents(notes, frames, midi)
	beats, origin, set, ok := rhythm(ev, ends[len(ends)-1], s)
	h.Settings = set
	if ok {
		h.Beats, h.Origin = beats, origin
		beat := set.Beat()
		for i := range starts {
			starts[i] = (beatCoord(beats, starts[i]) - origin) * beat
			ends[i] = (beatCoord(beats, ends[i]) - origin) * beat
		}
	} else {
		for i := range starts {
			starts[i] -= s.Start()
			ends[i] -= s.Start()
		}
	}
	h.Notes = h.quantize(starts, ends, midi, notes, !ok)
	return h
}

// humPitch 返回每帧的音分（MIDI×100；无声帧为 0）与有声标记：滞回门限、缺口插值、八度修正、中值平滑。
func humPitch(frames []karaoke.KaraokeFrame) ([]float64, []bool) {
	n := len(frames)
	dbs := make([]float64, n)
	for i, f := range frames {
		dbs[i] = f.DB
	}
	slices.Sort(dbs)
	gate := max(dbs[n/10]+humGateDB, humFloorDB)
	ok := func(f karaoke.KaraokeFrame, stay bool) bool {
		cl := humClarityEnter
		if stay {
			cl = humClarityStay
		}
		return f.F0 > 0 && f.DB >= gate && f.Clarity >= cl
	}
	// 滞回：连续 humEnterFrames 帧满足才进入，连续 humExitFrames 帧不满足才退出；段内更短的缺口算有声。
	voiced := make([]bool, n)
	in, run, fail := false, 0, 0
	for i, f := range frames {
		if !in {
			if ok(f, false) {
				run++
			} else {
				run = 0
			}
			if run == humEnterFrames {
				in, fail = true, 0
				for j := i - humEnterFrames + 1; j <= i; j++ {
					voiced[j] = true
				}
			}
			continue
		}
		if ok(f, true) {
			for j := i - fail; j <= i; j++ {
				voiced[j] = true // 桥接缺口
			}
			fail = 0
			continue
		}
		fail++
		if fail >= humExitFrames {
			in, run = false, 0
		}
	}
	// 音分：有周期的有声帧直接换算，缺口帧在两侧之间线性插值。
	cents := make([]float64, n)
	for i, f := range frames {
		if voiced[i] && f.F0 > 0 {
			cents[i] = 6900 + 1200*math.Log2(f.F0/440)
		}
	}
	for _, seg := range humSegments(voiced) {
		humInterpolate(cents[seg[0]:seg[1]])
		humFixOctaves(cents[seg[0]:seg[1]], frames[seg[0]:seg[1]])
		copy(cents[seg[0]:seg[1]], medianFilter(cents[seg[0]:seg[1]], humMedianWin))
	}
	return cents, voiced
}

// humDrift 返回每帧的音准偏移曲线（音分）：以该帧为中心、高斯窗（σ = humDriftSigma 秒）内稳定有声帧的音分残差
// （mod 100）按清晰度加权的圆均值，跟随越唱越低 / 越高的漂移；窗内集中度不足 humTuneMinR 时沿用前一帧的值（开头
// 为 0）。相邻帧的值按差展开到 ±50 音分以内（漂移是连续的），所以漂移超过半个半音后仍保持音程，整句与开头的
// 音准对齐——这比与绝对音高对齐更符合哼唱者心里的旋律。
// 残差取自约一个颤音周期（humDriftAvg 帧）的滑动平均：±30 音分的颤音直接放到 100 音分的圆上，集中度只剩约 0.3。
// 稳定帧指平滑后每帧变化不到 humDriftStable 音分、且平均窗内全部有声的帧，排除滑音、换音与段的边缘。
// 无声帧的值是前后的延续，只供连续性使用。
func humDrift(cents []float64, voiced []bool, frames []karaoke.KaraokeFrame) []float64 {
	n := len(cents)
	r := humDriftAvg / 2
	avg := make([]float64, n) // 平均窗内全部有声时的滑动平均，否则为 0
	for i := r; i+r < n; i++ {
		sum := 0.0
		for j := i - r; j <= i+r; j++ {
			if !voiced[j] {
				sum = 0
				break
			}
			sum += cents[j]
		}
		avg[i] = sum / float64(2*r+1)
	}
	type cs struct{ x, y float64 }
	unit := make([]cs, n) // 每帧残差的单位向量 × 权重；不参与的帧为零
	for i := 1; i+1 < n; i++ {
		if avg[i-1] == 0 || avg[i] == 0 || avg[i+1] == 0 ||
			math.Abs(avg[i]-avg[i-1]) > humDriftStable || math.Abs(avg[i+1]-avg[i]) > humDriftStable {
			continue
		}
		th := 2 * math.Pi * math.Mod(avg[i], 100) / 100
		w := max(frames[i].Clarity, 0.05)
		unit[i] = cs{w * math.Cos(th), w * math.Sin(th)}
	}
	sigma := humDriftSigma / humFrame
	half := int(3 * sigma)
	kern := make([]float64, half+1)
	for d := range kern {
		kern[d] = math.Exp(-float64(d*d) / (2 * sigma * sigma))
	}
	out := make([]float64, n)
	prev := 0.0
	for i := range n {
		var sx, sy, sw float64
		for j := max(0, i-half); j <= min(n-1, i+half); j++ {
			if u := unit[j]; u != (cs{}) {
				k := kern[stat.Abs(j-i)]
				sx, sy, sw = sx+k*u.x, sy+k*u.y, sw+k*math.Hypot(u.x, u.y)
			}
		}
		if sw > 0 && math.Hypot(sx, sy)/sw >= humTuneMinR {
			v := 100 / (2 * math.Pi) * math.Atan2(sy, sx)
			prev = v + 100*math.Round((prev-v)/100)
		}
		out[i] = prev
	}
	return out
}

// humSegments 返回连续有声帧的区间 [a, b)。
func humSegments(voiced []bool) [][2]int {
	var out [][2]int
	for i := 0; i < len(voiced); {
		if !voiced[i] {
			i++
			continue
		}
		j := i
		for j < len(voiced) && voiced[j] {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

// humInterpolate 把段内为 0 的帧按两侧的值线性插值（段首尾的缺口取最近的值）。
func humInterpolate(c []float64) {
	last := -1
	for i := range c {
		if c[i] == 0 {
			continue
		}
		if last < 0 {
			for j := range i {
				c[j] = c[i]
			}
		} else {
			for j := last + 1; j < i; j++ {
				c[j] = c[last] + (c[i]-c[last])*float64(j-last)/float64(i-last)
			}
		}
		last = i
	}
	if last >= 0 {
		for j := last + 1; j < len(c); j++ {
			c[j] = c[last]
		}
	}
}

// humFixOctaves 修正段内 YIN 的八度错误（×2、×1/2）与低清晰度时的五度错误（×3/2、×2/3）：参考音高为之前
// 5 个已接受帧的中位数，段首没有足够的帧时用整段中位数。偏离参考约一个八度（或五度）的一串帧若持续至少
// humLeapFrames 帧且平均清晰度不低于 humLeapClarity，是真实的八度 / 五度跳进，原样保留（参考随之移过去）；
// YIN 的倍频错误多是几十毫秒的突跳，或出现在清晰度低的气声、换声处。
func humFixOctaves(c []float64, frames []karaoke.KaraokeFrame) {
	if len(c) == 0 {
		return
	}
	segMed := stat.Median(c)
	var acc []float64
	// keepUntil 之前的帧属于已判定为真实跳进的一串，原样接受。
	keepUntil := 0
	for i := range c {
		ref := segMed
		if len(acc) >= 5 {
			ref = stat.Median(acc[len(acc)-5:])
		}
		// fold 返回偏离参考所对应的修正量（0 表示不偏离）。
		fold := func(k int) float64 {
			d := c[k] - ref
			switch {
			case math.Abs(d-1200) < humOctaveTol:
				return -1200
			case math.Abs(d+1200) < humOctaveTol:
				return 1200
			case frames[k].Clarity < humFifthClar && math.Abs(d-702) < humFifthTol:
				return -702
			case frames[k].Clarity < humFifthClar && math.Abs(d+702) < humFifthTol:
				return 702
			}
			return 0
		}
		if fix := fold(i); fix != 0 && i >= keepUntil {
			// 往后数停在远侧（同方向偏离超过修正量的一半）的帧：跳进后的音带颤音、或跳的是大七度 / 小九度时，
			// 不会每帧都贴近整八度；五度也不再要求低清晰度（跳进后的音清晰度回升也属于这一串）。
			j, cl := i, 0.0
			for j < len(c) && (ref-c[j])*math.Copysign(1, fix) > math.Abs(fix)/2 {
				cl += frames[j].Clarity
				j++
			}
			if j-i >= humLeapFrames && cl/float64(j-i) >= humLeapClarity {
				keepUntil = j
			} else {
				c[i] += fix
			}
		}
		acc = append(acc, c[i])
	}
}

// humOnsets 标出音节起音帧：响度回升、谷深、清晰度回升三项证据（有录音时再加频谱通量 fx，见 spectralFlux）之和
// 达到阈值且为 ±5 帧内的局部最大，相邻起音至少隔 humOnsetGap 帧；每个有声段的首帧总是起音。fx 可为 nil。
func humOnsets(frames []karaoke.KaraokeFrame, voiced []bool, fx []float64) []bool {
	n := len(frames)
	dbs := make([]float64, n)
	for i := range n {
		sum, k := 0.0, 0
		for j := max(0, i-1); j <= min(n-1, i+1); j++ {
			sum += frames[j].DB
			k++
		}
		dbs[i] = sum / float64(k)
	}
	o := make([]float64, n)
	for i := 1; i < n; i++ {
		lo := slices.Min(dbs[max(0, i-8):i])
		rise := dbs[i] - lo
		trough := slices.Min(dbs[max(0, i-8) : i+1])
		left := slices.Max(dbs[max(0, i-15) : i+1])
		right := slices.Max(dbs[i:min(n, i+16)])
		dip := min(left, right) - trough
		clRise := frames[i].Clarity - slices.MinFunc(frames[max(0, i-5):i], func(x, y karaoke.KaraokeFrame) int {
			return cmp.Compare(x.Clarity, y.Clarity)
		}).Clarity
		o[i] = max(0, rise)/humRiseDBRef + max(0, dip-humDipDBMin)/humDipDBRef + max(0, clRise)/humClarityRise +
			fluxEvidence(fx, i)
	}
	onset := make([]bool, n)
	last := -humOnsetGap
	for i := range n {
		if voiced[i] && (i == 0 || !voiced[i-1]) {
			onset[i], last = true, i
			continue
		}
		if !voiced[i] || o[i] < humOnsetThresh || i-last < humOnsetGap {
			continue
		}
		if o[i] < slices.Max(o[max(0, i-5):min(n, i+6)]) {
			continue
		}
		// 证据函数的峰值因平滑与回看窗口比实际起音晚一两帧：起音时刻取此前 humOnsetRefine 帧内（不早于上一个起音）
		// 原始响度（与通量）回升最快的帧（响度按 10 ms 子窗计算，比 40 ms 窗的音高更准）。只有通量证据时响度平坦，
		// 通量的回升决定时刻。
		rise := func(k int) float64 {
			r := (frames[k].DB - frames[k-1].DB) / humRiseDBRef
			if fx != nil {
				r += (fx[k] - fx[k-1]) / humFluxRef
			}
			return r
		}
		at := i
		for k := max(last+1, i-humOnsetRefine, 1); k <= i; k++ {
			if voiced[k] && rise(k) > rise(at) {
				at = k
			}
		}
		onset[at], last = true, at
	}
	return onset
}

// humSegment 在每个有声段内做半音状态的 Viterbi，切出原始音符并做合并后处理。
func humSegment(cents []float64, voiced, onset []bool) []rawNote {
	var out []rawNote
	for _, seg := range humSegments(voiced) {
		notes := humViterbi(cents, onset, seg[0], seg[1])
		notes = humAlignOnsets(notes, onset)
		notes = humMergeShort(notes)
		notes = humMergeSame(notes)
		notes = humMergeGlides(notes)
		out = append(out, notes...)
	}
	// 段与段之间：同音高、间隔很短且后者不是音节起点的合并（段首总是起点，所以实际只合并被缺口切开的同一音）。
	return humMergeSame(out)
}

// humViterbi 返回帧区间 [a, b) 的最优半音序列切成的音符；起音帧处同音高也切开（同音反复的新音节）。
func humViterbi(cents []float64, onset []bool, a, b int) []rawNote {
	const states = humMaxMIDI - humMinMIDI + 1
	emit := func(i, s int) float64 {
		d := cents[i] - 100*float64(humMinMIDI+s)
		return min(d*d/(2*humSigma*humSigma), humEmitMax)
	}
	T := b - a
	cost := make([]float64, states)
	backs := make([][]uint8, T) // backs[t][s]：第 t 帧处于状态 s 时前一帧的最优状态
	for s := range states {
		cost[s] = emit(a, s)
	}
	next := make([]float64, states)
	for t := 1; t < T; t++ {
		i := a + t
		backs[t] = make([]uint8, states)
		for s := range states {
			best, arg := math.Inf(1), 0
			for p := range states {
				c := cost[p]
				if p != s && !onset[i] {
					c += humSwitch + humSwitchSpan*float64(min(stat.Abs(p-s), 12))/12
				}
				if c < best {
					best, arg = c, p
				}
			}
			next[s], backs[t][s] = best+emit(i, s), uint8(arg)
		}
		cost, next = next, cost
	}
	path := make([]int, T)
	path[T-1] = int(argmin(cost))
	for t := T - 1; t > 0; t-- {
		path[t-1] = int(backs[t][path[t]])
	}
	var out []rawNote
	for t := 0; t < T; {
		u := t + 1
		for u < T && path[u] == path[t] && !onset[a+u] {
			u++
		}
		out = append(out, rawNote{a: a + t, b: a + u, m: humMinMIDI + path[t], onset: onset[a+t]})
		t = u
	}
	return out
}

func argmin(v []float64) int {
	best := 0
	for i, x := range v {
		if x < v[best] {
			best = i
		}
	}
	return best
}

// humAlignOnsets 处理换音边界：±humOnsetAlignWin 帧内有起音时把边界移到起音帧，后一个音成为新音节；
// 没有起音的换音是同一音节内的转音。
func humAlignOnsets(notes []rawNote, onset []bool) []rawNote {
	for j := 1; j < len(notes); j++ {
		if notes[j].onset {
			continue
		}
		at := notes[j].a
		for d := 1; d <= humOnsetAlignWin; d++ {
			for _, k := range []int{at - d, at + d} {
				if k > notes[j-1].a && k < notes[j].b && k >= 0 && k < len(onset) && onset[k] {
					notes[j-1].b, notes[j].a, notes[j].onset = k, k, true
					goto next
				}
			}
		}
	next:
	}
	return notes
}

// humMergeShort 合并过短的音：非音节起点的短音若音高朝下一音靠近（滑入），并入下一音，否则并入音高更近的相邻音；
// 音节起点的短音后面紧跟同一音节的音时，把后者并进来（起音处的滑音归入目标音）。
func humMergeShort(notes []rawNote) []rawNote {
	for changed := true; changed; {
		changed = false
		for j := 0; j < len(notes); j++ {
			n := notes[j]
			if n.b-n.a >= humMinNote || len(notes) == 1 {
				continue
			}
			hasNext := j+1 < len(notes) && notes[j+1].a == n.b
			hasPrev := j > 0 && notes[j-1].b == n.a
			switch {
			case n.onset && hasNext && !notes[j+1].onset:
				notes[j+1].a, notes[j+1].onset = n.a, true
			case !n.onset && hasNext && !notes[j+1].onset && (!hasPrev ||
				stat.Abs(notes[j+1].m-n.m) <= stat.Abs(notes[j-1].m-n.m)):
				notes[j+1].a = n.a
			case !n.onset && hasPrev:
				notes[j-1].b = n.b
			case !n.onset && hasNext:
				// 后一音是新音节：短音只能并入前一音，没有前一音时保留
				continue
			default:
				continue
			}
			notes = slices.Delete(notes, j, j+1)
			changed = true
			break
		}
	}
	return notes
}

// humMergeSame 合并同音高、后者不是音节起点、间隔不超过 humMergeGap 帧的相邻音。
func humMergeSame(notes []rawNote) []rawNote {
	for j := 1; j < len(notes); {
		p, n := notes[j-1], notes[j]
		if n.m == p.m && !n.onset && n.a-p.b <= humMergeGap {
			notes[j-1].b = n.b
			notes = slices.Delete(notes, j, j+1)
			continue
		}
		j++
	}
	return notes
}

// humMergeGlides 把同一音节内连续 ≥3 个单向阶梯、每级都很短的音视为滑音，只保留首尾两音（中间并入末音）。
func humMergeGlides(notes []rawNote) []rawNote {
	for j := 0; j+2 < len(notes); j++ {
		k := j + 1
		dir := notes[k].m - notes[j].m
		for k < len(notes) && !notes[k].onset && notes[k].a == notes[k-1].b &&
			(notes[k].m-notes[k-1].m)*dir > 0 && notes[k-1].b-notes[k-1].a <= humGlideStep {
			k++
		}
		// notes[j+1 .. k-1] 在阶梯上；至少 3 个音（含首音）且中间至少一级时合并中间各级到末音。
		if k-j >= 3 && dir != 0 {
			last := k - 1
			notes[last].a = notes[j+1].a
			notes = slices.Delete(notes, j+1, last)
		}
	}
	return notes
}

// humNoteCents 返回音符的代表音高：去掉开头（瞬态、滑入）与结尾后，按清晰度加权的音分中位数。
func humNoteCents(cents []float64, frames []karaoke.KaraokeFrame, n rawNote) float64 {
	head := humTrimHead
	if n.b-n.a < 15 {
		head = int(float64(n.b-n.a) * humTrimHeadFrac)
	}
	a, b := n.a+head, n.b-humTrimTail
	if b-a < 1 {
		a, b = n.a, n.b
	}
	type wv struct{ v, w float64 }
	xs := make([]wv, 0, b-a)
	total := 0.0
	for i := a; i < b; i++ {
		w := max(frames[i].Clarity, 0.05)
		xs = append(xs, wv{cents[i], w})
		total += w
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].v < xs[j].v })
	acc := 0.0
	for _, x := range xs {
		acc += x.w
		if acc >= total/2 {
			return x.v
		}
	}
	return xs[len(xs)-1].v
}

// tune 估计整体音高偏移与调，返回每个音的整数 MIDI 音高（调性明确时吸附唱得含糊的调外音）。notes 的音高已扣除
// 帧级漂移曲线（humDrift），这里只校正剩下的整体残差并累加到 h.Tuning。
func (h *Hum) tune(notes []rawNote) []int {
	// 整体偏移：各音残差（mod 100）按时长加权的圆均值。
	var sx, sy, sw float64
	for _, n := range notes {
		th := 2 * math.Pi * math.Mod(n.cents, 100) / 100
		w := float64(n.b - n.a)
		sx, sy, sw = sx+w*math.Cos(th), sy+w*math.Sin(th), sw+w
	}
	var resid float64
	if math.Hypot(sx, sy)/sw >= humTuneMinR {
		resid = 100 / (2 * math.Pi) * math.Atan2(sy, sx)
	}
	midi := make([]int, len(notes))
	dev := make([]float64, len(notes))
	var hist [12]float64
	for i, n := range notes {
		c := n.cents - resid
		midi[i] = int(math.Round(c / 100))
		dev[i] = c - 100*float64(midi[i])
		hist[((midi[i]%12)+12)%12] += float64(n.b - n.a)
	}
	// Krumhansl–Kessler 估调：时长加权的音级分布与 24 条轮廓的皮尔逊相关。
	r1, r2 := math.Inf(-1), math.Inf(-1)
	for _, mode := range []string{"major", "minor"} {
		prof := kkMajor
		if mode == "minor" {
			prof = kkMinor
		}
		for tonic := range 12 {
			var rot [12]float64
			for k := range 12 {
				rot[(tonic+k)%12] = prof[k]
			}
			r := stat.Pearson(hist[:], rot[:])
			switch {
			case r > r1:
				r1, r2 = r, r1
				h.TonicPC, h.Mode = tonic, mode
			case r > r2:
				r2 = r
			}
		}
	}
	h.Tuning += resid
	h.KeyClear = r1 >= humKeyMinCorr && r1-r2 >= humKeyMargin
	if !h.KeyClear {
		return midi
	}
	inKey := func(m int) bool {
		steps := []int{0, 2, 4, 5, 7, 9, 11}
		if h.Mode == "minor" {
			steps = []int{0, 2, 3, 5, 7, 8, 10, 11} // 自然小调加和声小调的导音
		}
		return slices.Contains(steps, ((m-h.TonicPC)%12+12)%12)
	}
	for i, m := range midi {
		if inKey(m) || math.Abs(dev[i]) < humSnapDev {
			continue
		}
		to := m + 1
		if dev[i] < 0 {
			to = m - 1
		}
		if inKey(to) {
			midi[i] = to
		}
	}
	return midi
}

// Krumhansl–Kessler 调性轮廓（Krumhansl & Kessler 1982），从主音起的 12 个音级。
var (
	kkMajor = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	kkMinor = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
)

// quantize 扣除整体节奏偏移后，把音符边界量化到网格：每拍按边界离网格的距离选 1/16 或八分三连音（复合拍只用
// 十六分音符网格：拍本身已三分），
// 再对边界序列做动态规划（每个边界在网格上取下取整、上取整或再后一格），保证每个音时长不为零、相邻不重叠；
// 与下一音的间隙不到半个网格时连奏。超出动机范围的音丢弃或截断。
// estimateShift 为 false 时（节拍跟踪已经对齐了拍点）不估计整体偏移。
func (h *Hum) quantize(starts, ends []float64, midi []int, notes []rawNote, estimateShift bool) []score.MelodyNote {
	s := h.Settings
	beat := s.Beat()
	g := s.Grid()
	// 整体节奏偏移：时长足够的音符起点相对 1/16 网格的圆均值。
	var sx, sy, sw float64
	if !estimateShift {
		sw = math.Inf(-1) // 跳过下面的估计
	}
	for i := range starts {
		if d := ends[i] - starts[i]; d >= humShiftMinDur && estimateShift {
			th := 2 * math.Pi * starts[i] / g
			sx, sy, sw = sx+d*math.Cos(th), sy+d*math.Sin(th), sw+d
		}
	}
	if sw > 0 && math.Hypot(sx, sy)/sw >= humShiftMinR {
		h.Shift = g / (2 * math.Pi) * math.Atan2(sy, sx)
	}
	length := s.Length()
	// 边界序列 x[2i] = 起点、x[2i+1] = 终点（扣除偏移，限制在动机范围内）。
	var keep []int
	for i := range starts {
		if ends[i]-h.Shift > g/2 && starts[i]-h.Shift < length-g/2 {
			keep = append(keep, i)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	x := make([]float64, 0, 2*len(keep))
	for _, i := range keep {
		x = append(x, min(max(starts[i]-h.Shift, 0), length), min(max(ends[i]-h.Shift, 0), length))
	}
	// 拍位先验：落在越弱的拍位上代价越高（拍点 0；半拍与复合拍的八分音符位置 humMetricHalf；三连音位置
	// humMetricTriplet；其余十六分音符位置 humMetricQuarter）。人哼唱时起止前后偏几十毫秒，只按距离取最近格会把
	// 八分音符节奏切碎成附点十六分音符或孤立的三连音；先验让离两格差不多远的边界落到更强的拍位。思路同
	// Cemgil et al. 2000（节奏量化的贝叶斯先验）、Desain & Honing 1989 的「简单节奏优先」。
	metric := func(q float64) float64 {
		f := math.Mod(q/beat+1e-9, 1)
		on := func(v float64) bool { return math.Abs(f-v) < 1e-6 }
		switch {
		case on(0):
			return 0
		case s.Compound && (on(1.0/3) || on(2.0/3)), !s.Compound && on(0.5):
			return humMetricHalf
		case on(1.0/3) || on(2.0/3):
			return humMetricTriplet
		}
		return humMetricQuarter
	}
	// snapCost 是边界 v 落到间距 st 的网格上两侧格点的最小代价（距离以 g 为单位，加拍位先验）。
	snapCost := func(v, st float64) float64 {
		lo := math.Floor(v/st+1e-9) * st
		return min(math.Abs(v-lo)/g+metric(lo), math.Abs(lo+st-v)/g+metric(lo+st))
	}
	// 每拍选网格：该拍内各边界落到网格的代价之和（snapCost），三连音多付 humTripletBias。
	beats := int(math.Ceil(length/beat - 1e-9))
	step := make([]float64, beats+1)
	for b := range step {
		step[b] = g
	}
	for b := range beats {
		// 连奏处前一音的终点与后一音的起点是同一个边界，只计一次；拍内（离拍点超过半格）的边界只有一个时，
		// 三连音再多付 humTripletLone——一拍里孤立的三分点多半是唱得偏后的半拍。
		c4, c3, any, inner, last := 0.0, humTripletBias, false, 0, math.Inf(-1)
		b0 := float64(b) * beat
		for _, v := range x {
			if v < b0 || v >= b0+beat || v-last < g/2 {
				continue
			}
			any, last = true, v
			c4 += snapCost(v, g)
			c3 += snapCost(v, beat/3)
			if v-b0 > g/2 && b0+beat-v > g/2 {
				inner++
			}
		}
		if inner == 1 {
			c3 += humTripletLone
		}
		if any && c3 < c4 && !s.Compound {
			step[b] = beat / 3
		}
	}
	cands := func(v float64) []float64 {
		b := min(int(v/beat), beats)
		st := step[b]
		lo := math.Floor(v/st+1e-9) * st
		out := []float64{lo, lo + st, lo + 2*st}
		for i := range out {
			out[i] = min(out[i], length)
		}
		return slices.Compact(out)
	}
	// 动态规划：dp[k][c] 为前 k 个边界、第 k 个取候选 c 时的最小总距离；约束见函数注释。
	C := make([][]float64, len(x))
	for k, v := range x {
		C[k] = cands(v)
	}
	dp := make([][]float64, len(x))
	from := make([][]int, len(x))
	for k := range x {
		dp[k] = make([]float64, len(C[k]))
		from[k] = make([]int, len(C[k]))
		for c, q := range C[k] {
			dp[k][c] = math.Inf(1)
			d := math.Abs(x[k]-q)/g + metric(q)
			if k == 0 {
				dp[k][c] = d
				continue
			}
			for pc, pq := range C[k-1] {
				// 终点（k 为奇数）须严格晚于起点；起点须不早于前一音的终点。
				okay := (k%2 == 1 && q > pq+1e-9) || (k%2 == 0 && q >= pq-1e-9)
				if okay && dp[k-1][pc]+d < dp[k][c] {
					dp[k][c], from[k][c] = dp[k-1][pc]+d, pc
				}
			}
		}
	}
	last := len(x) - 1
	c := argmin(dp[last])
	if math.IsInf(dp[last][c], 1) {
		return nil // 动机范围放不下这么多音（只在极端密集时发生）
	}
	q := make([]float64, len(x))
	for k := last; k >= 0; k-- {
		q[k] = C[k][c]
		c = from[k][c]
	}
	out := make([]score.MelodyNote, len(keep))
	for j, i := range keep {
		out[j] = score.MelodyNote{Start: q[2*j], End: q[2*j+1], Pitch: float64(midi[i]), Slur: !notes[i].onset}
		if j > 0 && !notes[i].onset && notes[keep[j-1]].b != notes[i].a {
			out[j].Slur = false // 中间有休止的不算同一音节
		}
	}
	for j := 0; j+1 < len(out); j++ {
		if x[2*j+2]-x[2*j+1] < humLegatoGap*g {
			out[j].End = out[j+1].Start
		}
	}
	if len(out) > 0 {
		out[0].Slur = false
	}
	return out
}

// gridDist 返回 v 到间距 step 的网格的最近距离。
func gridDist(v, step float64) float64 {
	r := math.Mod(v, step)
	return min(r, step-r)
}

// medianFilter 返回 v 的中值滤波（奇数窗 w，边缘取窗口内可用的帧）。
func medianFilter(v []float64, w int) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		out[i] = stat.Median(v[max(0, i-w/2):min(len(v), i+w/2+1)])
	}
	return out
}

// FillerSets 是拟声字的几组选择：每组为长音、短音、句尾的字。
var FillerSets = []struct {
	Title             string
	Long, Short, Tail string
}{
	{"啦（明亮）", "啦", "哒", "啦"},
	{"噜（轻快）", "噜", "嘟", "噜"},
	{"呜（抒情）", "呜", "嗯", "哦"},
}

// FillLyrics 按 FillerSets[set] 给每个音节的第一个音填拟声字（连音不填）：音节后有一拍以上的休止或是最后一个
// 音节时用句尾字，音节总时长不短于一拍时用长音字，否则用短音字。beat 为一拍的时长（秒）。
func FillLyrics(notes []score.MelodyNote, set int, beat float64) {
	fs := FillerSets[min(max(set, 0), len(FillerSets)-1)]
	for i := 0; i < len(notes); {
		j := i + 1
		for j < len(notes) && notes[j].Slur {
			notes[j].Lyric = ""
			j++
		}
		end := notes[j-1].End
		switch {
		case j == len(notes) || notes[j].Start-end >= beat-1e-9:
			notes[i].Lyric = fs.Tail
		case end-notes[i].Start >= beat-1e-9:
			notes[i].Lyric = fs.Long
		default:
			notes[i].Lyric = fs.Short
		}
		i = j
	}
}

// Analysis 把乐句包装成分析结果（节拍、小节、调与旋律线），以便复用乐谱、卷帘、合成与 MIDI / 歌声合成工程导出。
func (h Hum) Analysis() *score.Analysis {
	s := h.Settings
	a := &score.Analysis{Duration: s.Length()}
	bpm := s.BPM
	a.Tempo.BPM = &bpm
	// 复合拍以八分音符为拍（拍号分母 8），速度按四分音符写（MIDI 与歌声合成工程的速度都以四分音符计）。
	per, step := s.BeatsPerBar, s.Beat()
	if s.Compound {
		per, step = 3*s.BeatsPerBar, s.Beat()/3
		bpm = s.BPM * 1.5
		a.Meter.Denominator = 8
	}
	a.Meter.BeatsPerBar = per
	label := s.MeterLabel()
	a.Meter.Label, a.Meter.Confidence = &label, 1
	for b := range s.Bars*per + 1 {
		t := float64(b) * step
		a.Beats = append(a.Beats, t)
		if b%per == 0 && b < s.Bars*per { // 终点那一拍只标出最后一小节的结束，不是新的小节
			a.Downbeats = append(a.Downbeats, t)
		}
	}
	a.Key.TonicPC, a.Key.Mode = h.TonicPC, h.Mode
	a.Key.Flats = score.KeySignature(h.TonicPC, h.Mode) < 0
	a.Key.Label = KeyLabel(h.TonicPC, h.Mode, a.Key.Flats)
	a.Melody = &score.Melody{Notes: slices.Clone(h.Notes), Source: "hum"}
	return a
}

// KeyLabel 返回调名，例如「C 大调」「F# 小调」（与分析引擎的写法一致）。
func KeyLabel(tonic int, mode string, flats bool) string {
	name := map[string]string{"major": "大调", "minor": "小调"}[mode]
	return score.PitchName(tonic, flats) + " " + name
}
