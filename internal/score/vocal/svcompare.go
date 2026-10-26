package vocal

import (
	"math"
	"strings"

	"kanade/internal/score"
	"kanade/internal/score/karaoke"
	"kanade/internal/score/stat"
	"kanade/internal/score/synth"
)

// 渲染对比与引导式音高（docs/roadmap.md 第 3 节的第 2、3 步）。
//
// 渲染对比：用户把 Synthesizer V 渲染出的人声拖回来，用与原唱相同的特征提取（score.expression）后逐帧对比：
// 基频误差（按导出时的移调换算回原调）、响度的中位数 / 起伏 / 相关、频谱倾斜与高频噪声的差。没有这一步，
// 参数映射的改动只能凭感觉判断（roadmap 中两版表情都「不如不调」，但只有一版做过客观对比）。
//
// 引导式音高：不再把原唱 F0 整条覆盖到手动音高的音符上，而是保留 SV 的 AI 音高（音符不改手动模式、不关颤音），
// 只在原唱与 SV 基准渲染差得多的地方（转音、明显的滑音、原唱特有的音高走向）写 pitchDelta = 原唱 − 基准，
// 其余处为 0，进出差异段时按 svGuideRamp 渐变。基准是同一工程不写表情曲线时 SV 的渲染：SV 的最终音高是
// 「音符自身的音高曲线 + pitchDelta」（SV 1 手册 Pitch Curves Basics），所以补上差值即接近原唱，
// 而 SV 自己的起音、过渡与颤音在差异小的地方原样保留。

const (
	// svGuideMinCents：原唱与基准的偏差不到此值（音分）的帧不补（在演唱的自然误差内，补了只会叠加抖动）。
	svGuideMinCents = 40
	// svGuideMinRun：偏差段至少连续这么多帧（10 ms 一帧）才补，免得逐帧跟踪噪声变成音高毛刺。
	svGuideMinRun = 6
	// svGuideRamp：差异段两端的渐变帧数（各 80 ms），段内权重 1，段外 0。
	svGuideRamp = 8
)

// RenderComparison 是一次渲染与原唱的对比。音分正值表示渲染偏高；dB 正值表示渲染更响 / 更亮 / 噪声更多。
type RenderComparison struct {
	Frames int // 两边都是有声帧、参与音高对比的帧数（为 0 时其余字段无意义）
	// 音高：偏差绝对值的中位数，±50 音分内的比例。
	PitchMedianAbs, PitchWithin50 float64
	// 响度：两边有声帧响度的中位数差、各自的 10–90 分位距（起伏），以及逐帧（250 ms 平滑后）的相关系数。
	LoudnessDiff, LoudnessRangeOrig, LoudnessRangeRender, LoudnessCorr float64
	// 频谱倾斜（张力的近似）与高频噪声（气声的近似）的中位数差。
	TiltDiff, NoiseDiff float64
	// VoicedOrig / VoicedRender 是音符内有声帧的比例（渲染明显低于原唱时往往是气声过多或发声被压低）。
	VoicedOrig, VoicedRender float64
	Phrases                  []PhraseComparison
}

// PhraseComparison 是一个乐句的对比（字段含义同 RenderComparison）。
type PhraseComparison struct {
	Start, End                             float64 // 秒
	Lyric                                  string
	PitchMedianAbs, LoudnessDiff, TiltDiff float64
	NoiseDiff                              float64
}

// CompareRender 对比渲染 render 与原唱 orig；transpose 是导出时的移调（半音，渲染按移调后的音高演唱）。
// 两者的帧时间都从歌曲 0 秒起（导出的工程与原曲对齐）。没有旋律线或特征时返回零值。
func CompareRender(a *score.Analysis, orig, render *score.Expression, transpose int) RenderComparison {
	var c RenderComparison
	notes, tm, err := vocalTrack(a)
	if err != nil || orig == nil || render == nil || len(orig.F0) == 0 || len(render.F0) == 0 {
		return c
	}
	n := min(len(orig.F0), len(render.F0))
	owner := frameOwners(notes, tm, orig.Hop, n)
	voicedO := func(i int) bool {
		return orig.F0[i] > 0 && orig.Periodicity[i] >= svVoicedPeriodicity && orig.Loudness[i] > -60
	}
	voicedR := func(i int) bool {
		return render.F0[i] > 0 && render.Periodicity[i] >= svVoicedPeriodicity && render.Loudness[i] > -60
	}
	shift := math.Pow(2, float64(transpose)/12)
	// cents 返回帧 i 渲染相对原唱的音高偏差（八度误差折回 ±600）。
	cents := func(i int) float64 {
		d := 1200 * math.Log2(render.F0[i]/(orig.F0[i]*shift))
		return d - 1200*math.Round(d/1200)
	}
	loudO, loudR := smooth(orig.Loudness[:n], svLoudSmooth), smooth(render.Loudness[:n], svLoudSmooth)

	// 整首：音符内的帧。
	var pitch, lo, lr, to, tr, no, nr, cx, cy []float64
	inNote, vo, vr := 0, 0, 0
	for i := range n {
		if owner[i] < 0 {
			continue
		}
		inNote++
		bo, br := voicedO(i), voicedR(i)
		if bo {
			vo++
			lo, to, no = append(lo, orig.Loudness[i]), append(to, orig.Tilt[i]), append(no, orig.Noise[i])
		}
		if br {
			vr++
			lr, tr, nr = append(lr, render.Loudness[i]), append(tr, render.Tilt[i]), append(nr, render.Noise[i])
		}
		if bo && br {
			pitch = append(pitch, cents(i))
			cx, cy = append(cx, loudO[i]), append(cy, loudR[i])
		}
	}
	c.Frames = len(pitch)
	if c.Frames == 0 {
		return c
	}
	abs := make([]float64, len(pitch))
	within := 0
	for i, p := range pitch {
		abs[i] = math.Abs(p)
		if abs[i] <= 50 {
			within++
		}
	}
	c.PitchMedianAbs, c.PitchWithin50 = stat.Percentile(abs, 0.5), float64(within)/float64(len(pitch))
	c.LoudnessDiff = stat.Percentile(lr, 0.5) - stat.Percentile(lo, 0.5)
	c.LoudnessRangeOrig = stat.Percentile(lo, 0.9) - stat.Percentile(lo, 0.1)
	c.LoudnessRangeRender = stat.Percentile(lr, 0.9) - stat.Percentile(lr, 0.1)
	c.LoudnessCorr = stat.Pearson(cx, cy)
	c.TiltDiff = stat.Percentile(tr, 0.5) - stat.Percentile(to, 0.5)
	c.NoiseDiff = stat.Percentile(nr, 0.5) - stat.Percentile(no, 0.5)
	c.VoicedOrig, c.VoicedRender = float64(vo)/float64(inNote), float64(vr)/float64(inNote)

	// 乐句：音符间空隙 ≥ karaoke.KaraokePhraseGap 处断开（与 K 歌相同的划分）。
	for _, ph := range vocalPhrases(notes, tm) {
		var pc, dl, dt, dn []float64
		for i := range n {
			k := owner[i]
			if k < ph.first || k > ph.last || !voicedO(i) || !voicedR(i) {
				continue
			}
			pc = append(pc, math.Abs(cents(i)))
			dl, dt, dn = append(dl, render.Loudness[i]-orig.Loudness[i]), append(dt, render.Tilt[i]-orig.Tilt[i]),
				append(dn, render.Noise[i]-orig.Noise[i])
		}
		if len(pc) == 0 {
			continue
		}
		c.Phrases = append(c.Phrases, PhraseComparison{Start: ph.start, End: ph.end, Lyric: ph.lyric,
			PitchMedianAbs: stat.Percentile(pc, 0.5), LoudnessDiff: stat.Percentile(dl, 0.5), TiltDiff: stat.Percentile(dt, 0.5),
			NoiseDiff: stat.Percentile(dn, 0.5)})
	}
	return c
}

// vocalPhrase 是工程音符中的一个乐句：音符序号 [first, last]，时间（秒）与歌词。
type vocalPhrase struct {
	first, last int
	start, end  float64
	lyric       string
}

// vocalPhrases 把工程音符按 karaoke.KaraokePhraseGap 的停顿分成乐句。
func vocalPhrases(notes []vocalNote, tm *synth.TimeMap) []vocalPhrase {
	var out []vocalPhrase
	for i := 0; i < len(notes); {
		j := i
		var b strings.Builder
		b.WriteString(notes[i].lyric)
		for j+1 < len(notes) && tm.Seconds(notes[j+1].on)-tm.Seconds(notes[j].off) < karaoke.KaraokePhraseGap {
			j++
			b.WriteString(notes[j].lyric)
		}
		out = append(out, vocalPhrase{first: i, last: j, start: tm.Seconds(notes[i].on), end: tm.Seconds(notes[j].off), lyric: b.String()})
		i = j + 1
	}
	return out
}

// frameOwners 返回每帧（帧移 hop 秒）所在的音符序号，不在音符内为 -1。
func frameOwners(notes []vocalNote, tm *synth.TimeMap, hop float64, n int) []int {
	owner := make([]int, n)
	k := 0
	for i := range n {
		t := tm.Tick(float64(i) * hop)
		for k < len(notes) && notes[k].off <= t {
			k++
		}
		owner[i] = -1
		if k < len(notes) && notes[k].on <= t {
			owner[i] = k
		}
	}
	return owner
}

// guidedPitch 返回引导式音高的逐帧 pitchDelta（音分，NaN 为不写点）：原唱相对音符的偏差减去基准渲染相对
// 移调后音符的偏差，只保留持续偏差段并在两端渐变，再乘以 strength。base 与 e 的帧移须相同。
func guidedPitch(notes []vocalNote, owner []int, e, base *score.Expression, transpose int, strength float64) []float64 {
	n := min(len(e.F0), len(base.F0))
	shifted := make([]vocalNote, len(notes))
	for i, v := range notes {
		v.pitch += transpose
		shifted[i] = v
	}
	orig := pitchCents(notes, owner[:n], &score.Expression{F0: e.F0[:n], Periodicity: e.Periodicity[:n]})
	ref := pitchCents(shifted, owner[:n], &score.Expression{F0: base.F0[:n], Periodicity: base.Periodicity[:n]})
	diff := make([]float64, n)
	mask := make([]bool, n)
	for i := range n {
		diff[i] = math.NaN()
		if owner[i] >= 0 && !math.IsNaN(orig[i]) && !math.IsNaN(ref[i]) {
			diff[i] = orig[i] - ref[i]
			mask[i] = math.Abs(diff[i]) >= svGuideMinCents
		}
	}
	// 只保留足够长的偏差段（同一音符内连续）。
	for i := 0; i < n; {
		if !mask[i] {
			i++
			continue
		}
		j := i
		for j < n && mask[j] && owner[j] == owner[i] {
			j++
		}
		if j-i < svGuideMinRun {
			for k := i; k < j; k++ {
				mask[k] = false
			}
		}
		i = j
	}
	// 权重：段内 1，两端 svGuideRamp 帧线性渐变（离最近的段越远越小）。
	weight := make([]float64, n)
	dist := svGuideRamp + 1
	for i := range n { // 向右扫：到左边最近的段的距离
		if mask[i] {
			dist = 0
		} else {
			dist++
		}
		weight[i] = max(0, 1-float64(dist)/float64(svGuideRamp+1))
	}
	dist = svGuideRamp + 1
	for i := n - 1; i >= 0; i-- { // 向左扫：到右边最近的段的距离
		if mask[i] {
			dist = 0
		} else {
			dist++
		}
		weight[i] = max(weight[i], 1-float64(dist)/float64(svGuideRamp+1))
	}
	out := make([]float64, n)
	last := 0.0 // 渐变区里偏差未知（无声帧）时沿用最近的偏差
	for i := range n {
		out[i] = math.NaN()
		if owner[i] < 0 {
			continue
		}
		if !math.IsNaN(diff[i]) {
			last = diff[i]
		}
		out[i] = weight[i] * last * strength
		if weight[i] == 0 {
			out[i] = 0
		}
	}
	return out
}
