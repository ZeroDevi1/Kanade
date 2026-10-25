package score

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// 灵感页的动机发展（M3）：把整理好的动机（Hum）按经典句式发展成 8 或 16 小节的乐段。规则式、确定性：
//  1. 在调内级数空间里做变换（模进、倒影、裂解、句尾改写、节奏变奏），最后才换回 MIDI，变换不会产生调外音；
//  2. 句式模板以 2 小节为一格（并列、问答、AA'BA、ABAC），每格是动机单元加一串变换，段末放半终止或全终止；
//     B / C 段从音区、反向轮廓、节奏密度、和声区域四种对比手段中取两种；
//  3. 枚举模板的整数参数（模进级数、倒影轴、对比手段…），按统一与对比的平衡、音域与跨度、高潮位置、终止、
//     段间跳进与重复打分，选出彼此差异足够大的三套；
//  4. 发展后的乐段整段交给 HarmonizeWith，按模板给出终止点与段落的和声区域。
//
// 设计与权重经 Claude Fable 5.1 咨询（参考 Schoenberg《Fundamentals of Musical Composition》的动机发展手法），
// 均为经验值。用户可以锁定某些段落，其余段落重新生成（DevelopOptions.Locked）。

// degNote 是发展阶段的音：deg 为相对主音的调内级数（0 = 主音，7 = 高八度的主音，可为负），alt 为相对音阶音的
// 半音变化（保留动机里的变化音；平移类变换把它清零）。onset / dur 以拍为单位；head 为音节的第一个音（false 为转音）。
type degNote struct {
	deg, alt   int
	onset, dur float64
	head       bool
}

// 段落的终止（DevSlot.Cadence）。
const (
	DevCadNone = iota
	DevCadHalf
	DevCadFull
)

// DevSlot 是乐段中的一格（2 小节）：Label 为段落标记（A、A'、B…），Bar 为起始小节（0 起），Spec 为生成方式，
// Desc 为变换的说明，Cadence 为段末的终止，Region 为和声区域倾向（Region* 常量，与 Spec 中的相同）。
type DevSlot struct {
	Label   string
	Bar     int
	Spec    SlotSpec
	Desc    string
	Cadence int
	Region  int
	notes   []degNote // 展开后的音（onset 相对本格起点）
}

// Period 是一套发展好的乐段。Hum 是展开后的乐段（可直接配和声、合成与导出），Score 为 0–100 的评分。
type Period struct {
	Form  string // 句式名称（DevForms 之一的 Title）
	Bars  int
	Slots []DevSlot
	Hum   Hum
	Score float64
	// Notes 是评分的说明（统一、对比、音域、高潮）。
	Notes string
	ctx   *devContext // 生成时的素材，供 EditSlot 重算
}

// Cadences 返回乐段的终止点（供 HarmonizeWith）：各格末的最后一个起音所在时刻。
func (p Period) Cadences() []CadencePoint {
	beat := p.Hum.Settings.Beat()
	bpb := float64(p.Hum.Settings.BeatsPerBar)
	var out []CadencePoint
	for _, sl := range p.Slots {
		if sl.Cadence == DevCadNone || len(sl.notes) == 0 {
			continue
		}
		last := sl.notes[len(sl.notes)-1]
		kind := CadHalf
		if sl.Cadence == DevCadFull {
			kind = CadFull
		}
		out = append(out, CadencePoint{At: (float64(sl.Bar)*bpb + last.onset) * beat, Kind: kind})
	}
	return out
}

// Regions 返回各格的和声区域倾向（供 HarmonizeWith）。
func (p Period) Regions() []RegionBias {
	beat := p.Hum.Settings.Beat()
	bpb := float64(p.Hum.Settings.BeatsPerBar)
	var out []RegionBias
	for _, sl := range p.Slots {
		if sl.Region != RegionTonic {
			out = append(out, RegionBias{Start: float64(sl.Bar) * bpb * beat, End: float64(sl.Bar+devSlotBars) * bpb * beat,
				Region: sl.Region, Weight: devRegionWeight})
		}
	}
	return out
}

// Harmonize 为乐段配 style 风格的和声（按句式的终止点与段落区域），锁定的和弦见 locks。
func (p Period) Harmonize(style, n int, locks []ChordLock) []Harmony {
	return HarmonizeWith(p.Hum, style, n, HarmonyOptions{Locks: locks, Cadences: p.Cadences(), Regions: p.Regions()})
}

// DevForm 是一种句式。
type DevForm struct {
	Key, Title string
	Bars       int
}

// DevForms 是可选的句式。
var DevForms = []DevForm{
	{"parallel", "并列 A A'", 8},
	{"qa", "问答 a b", 8},
	{"aaba8", "AA'BA（8 小节）", 8},
	{"aaba", "AA'BA", 16},
	{"abac", "ABAC", 16},
}

// DevelopOptions 是动机发展的选项。
type DevelopOptions struct {
	Bars int    // 8 或 16
	Form string // DevForms 的 Key；空为该长度的全部句式
	// Locked 是要保留的格：键为格序号，值为该格的音（来自之前生成的方案，Period.Slots 的同一格）。
	// 锁定的格不再变换，但仍参与整段的评分（连接、高潮、重复）。
	Locked map[int]DevSlot
	// Filler 是拟声字组（FillerSets 的序号）。
	Filler int
}

// 常量（Fable 5.1 的建议值）。
const (
	devSlotBars     = 2    // 一格的小节数
	devSeqRange     = 3    // 模进级数的枚举范围 ±3
	devMaxVarEdits  = 3    // 节奏变奏最多改动的处数
	devBreathBeats  = 1.0  // 终止后到下一段的换气（拍）
	devUnitySim     = 0.6  // A 类段与动机的相似度下限
	devContrastLo   = 0.25 // B / C 段与动机的相似度区间
	devContrastHi   = 0.6
	devWUnity       = 60
	devWContrast    = 40
	devWRange       = 4  // 超出舒适音域每半音
	devMaxSpan      = 14 // 舒适的整体跨度（半音，约一个九度）
	devWSpan        = 3
	devWClimax      = 25
	devWClimaxDup   = 8
	devJoinMax      = 7 // 段间跳进超过它（半音）开始扣分
	devJoinHard     = 12
	devWJoin        = 3
	devWSame        = 15 // 相邻两格完全相同（模板没要求时）
	devMinDist      = 0.3
	devRegionWeight = 0.6 // 段落和声区域的先验（与和声求解器的 penultS 同量级）
	devComfortBelow = 4   // 舒适音域：动机最低音以下、最高音以上的半音数
	devComfortAbove = 5
	devComfortMin   = 48 // 舒适音域的绝对范围（C3–C6）
	devComfortMax   = 84
)

// devScale 返回调式的音阶（相对主音的半音数）。
func devScale(mode string) [7]int {
	if mode == "minor" {
		return [7]int{0, 2, 3, 5, 7, 8, 10}
	}
	return [7]int{0, 2, 4, 5, 7, 9, 11}
}

// keyModel 在 MIDI 与调内级数之间换算：base 为级数 0 所在的 MIDI 音高。
type keyModel struct {
	base  int
	scale [7]int
}

func newKeyModel(h Hum) keyModel {
	lo := 127
	for _, n := range h.Notes {
		lo = min(lo, int(math.Round(n.Pitch)))
	}
	if lo == 127 {
		lo = 60
	}
	return keyModel{base: lo - ((lo-h.TonicPC)%12+12)%12, scale: devScale(h.Mode)}
}

// toDeg 把 MIDI 音高换成级数与变化音：调外音吸附到最近的音阶音（距离相等取下方），alt 记差值。
func (k keyModel) toDeg(m int) (int, int) {
	rel := m - k.base
	oct := floorDiv(rel, 12)
	pc := rel - 12*oct
	best, bestD := 0, 99
	for i, s := range k.scale {
		if d := abs(pc - s); d < bestD {
			best, bestD = i, d
		}
	}
	// 主音高八度一侧也比较（例如大调的 11 半音离高八度主音 1 个半音）。
	if d := 12 - pc; d < bestD {
		return 7*(oct+1) + 0, pc - 12
	}
	return 7*oct + best, pc - k.scale[best]
}

// toMIDI 把级数换回 MIDI 音高。
func (k keyModel) toMIDI(deg, alt int) int {
	oct := floorDiv(deg, 7)
	return k.base + 12*oct + k.scale[deg-7*oct] + alt
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// devUnits 把动机切成 2 小节的单元：u0 为前 2 小节，u1 为第 3–4 小节（动机不足 3 小节时为 nil）。
// 1 小节的动机由调用方扩成 2 小节；长于 4 小节的只用前 4 小节。
func devUnits(h Hum, k keyModel) (u0, u1 []degNote) {
	beat := h.Settings.Beat()
	bpb := float64(h.Settings.BeatsPerBar)
	take := func(fromBar int) []degNote {
		var out []degNote
		lo, hi := float64(fromBar)*bpb, float64(fromBar+devSlotBars)*bpb
		for _, n := range h.Notes {
			on := n.Start / beat
			if on < lo-1e-9 || on >= hi-1e-9 {
				continue
			}
			d, a := k.toDeg(int(math.Round(n.Pitch)))
			out = append(out, degNote{deg: d, alt: a, onset: on - lo, dur: min(n.End/beat, hi) - on, head: !n.Slur || len(out) == 0})
		}
		return out
	}
	u0 = take(0)
	if h.Settings.Bars >= 3 {
		u1 = take(2)
	}
	return u0, u1
}

// —— 变换（都返回新切片，不改输入）——

// seq 是调内模进：级数平移 k（k ≠ 0 时清除变化音）。
func seq(u []degNote, k int) []degNote {
	out := slices.Clone(u)
	for i := range out {
		out[i].deg += k
		if k != 0 {
			out[i].alt = 0
		}
	}
	return out
}

// inv 是调内倒影：轴 axis 为 0 首音、1 离首音最近的主音、2 离首音最近的属音；deg' = 2a − deg。
func inv(u []degNote, axis int) []degNote {
	if len(u) == 0 {
		return nil
	}
	first := u[0].deg
	a := first
	switch axis {
	case 1:
		a = 7 * int(math.Round(float64(first)/7))
	case 2:
		a = 4 + 7*int(math.Round(float64(first-4)/7))
	}
	out := slices.Clone(u)
	for i := range out {
		out[i].deg, out[i].alt = 2*a-out[i].deg, 0
	}
	return out
}

// frag 是裂解：取单元开头（tail = false）或末尾 beats 拍内的音，连续排 reps 次（不超过 length 拍），第 r 次再模进
// r·k 级；之后留空，由句尾改写把末音延长。
func frag(u []degNote, tail bool, beats float64, reps int, length float64, k int) []degNote {
	var cell []degNote
	end := 0.0
	for _, n := range u {
		end = max(end, n.onset+n.dur)
	}
	lo := 0.0
	if tail {
		lo = math.Max(0, math.Ceil(end/beats)*beats-beats)
	}
	for _, n := range u {
		if n.onset >= lo-1e-9 && n.onset < lo+beats-1e-9 {
			c := n
			c.onset -= lo
			c.dur = min(c.dur, beats-c.onset)
			c.alt = 0
			cell = append(cell, c)
		}
	}
	if len(cell) == 0 {
		return slices.Clone(u)
	}
	cell[0].head = true
	var out []degNote
	for r := 0; r < reps && float64(r)*beats < length-1e-9; r++ {
		for _, c := range cell {
			c.onset += float64(r) * beats
			c.deg += r * k
			if c.onset < length-1e-9 {
				c.dur = min(c.dur, length-c.onset)
				out = append(out, c)
			}
		}
	}
	return out
}

// vary 是保持轮廓的节奏变奏：density 为 +1 时在与下一音相差两级、不短于一拍的音后半加经过音；-1 时合并相邻的同音。
// 改动处按时值从长到短、同长按先后取前 devMaxVarEdits 处。
func vary(u []degNote, density int, unit float64) []degNote {
	out := slices.Clone(u)
	switch density {
	case 1:
		var cand []int
		for i := 0; i+1 < len(out); i++ {
			if out[i].dur >= 1-1e-9 && abs(out[i+1].deg-out[i].deg) == 2 {
				cand = append(cand, i)
			}
		}
		slices.SortStableFunc(cand, func(a, b int) int { return cmp.Compare(out[b].dur, out[a].dur) })
		cand = cand[:min(len(cand), devMaxVarEdits)]
		slices.Sort(cand)
		for j := len(cand) - 1; j >= 0; j-- { // 从后往前插入，前面的下标不变
			i := cand[j]
			first := splitPoint(out[i].dur, unit)
			mid := degNote{deg: (out[i].deg + out[i+1].deg) / 2, onset: out[i].onset + first, dur: out[i].dur - first}
			out[i].dur = first
			out = slices.Insert(out, i+1, mid)
		}
	case -1:
		for i := 0; i+1 < len(out); {
			if out[i].deg == out[i+1].deg && out[i].alt == out[i+1].alt &&
				math.Abs(out[i].onset+out[i].dur-out[i+1].onset) < 1e-9 && !out[i+1].head {
				out[i].dur += out[i+1].dur
				out = slices.Delete(out, i+1, i+2)
				continue
			}
			i++
		}
	}
	return out
}

// cadence 改写句尾：最后一个音节落到终止音（全终止为主音，半终止为 2、5、7 级中离倒数第二个音最近的），
// 距离超过两级且倒数第二个音不短于一拍时在它后半插入中间的级进音；末音延长到格末，并在需要换气时留出 devBreathBeats 拍。
func cadence(u []degNote, kind int, length float64, breath bool, unit float64) []degNote {
	out := slices.Clone(u)
	if len(out) == 0 || kind == DevCadNone {
		return out
	}
	// 末音节只留第一个音（转音删掉），作为终止音。
	last := len(out) - 1
	for last > 0 && !out[last].head {
		last--
	}
	out = out[:last+1]
	prev := out[last].deg
	if last > 0 {
		prev = out[last-1].deg
	}
	targets := []int{0}
	if kind == DevCadHalf {
		targets = []int{1, 4, 6}
	}
	best, bestD := 0, 99
	for oct := floorDiv(prev, 7) - 1; oct <= floorDiv(prev, 7)+1; oct++ {
		for _, t := range targets {
			if d := abs(7*oct + t - prev); d < bestD || (d == bestD && 7*oct+t < best) {
				best, bestD = 7*oct+t, d
			}
		}
	}
	out[last].deg, out[last].alt = best, 0
	if last > 0 && bestD > 2 && out[last-1].dur >= 1-1e-9 {
		first := splitPoint(out[last-1].dur, unit)
		mid := degNote{deg: (out[last-1].deg + best) / 2, onset: out[last-1].onset + first, dur: out[last-1].dur - first}
		out[last-1].dur = first
		out = slices.Insert(out, last, mid)
		last++
	}
	end := length
	if breath && length-out[last].onset >= 1+devBreathBeats-1e-9 {
		end = length - devBreathBeats
	}
	out[last].dur = max(end-out[last].onset, math.Min(1, length-out[last].onset))
	return out
}

// splitPoint 返回把时值 dur（拍）分成两半时前一半的时值：取最接近一半的八分音符网格点（unit 为八分音符的拍数，
// 简单拍 1/2、复合拍 1/3），两半都至少一个网格。
func splitPoint(dur, unit float64) float64 {
	return min(max(math.Round(dur/2/unit)*unit, unit), dur-unit)
}

// —— 句式模板 ——

// devParams 是一套模板参数（全部为整数，便于确定性枚举与排序）。
type devParams struct {
	k1, k2 int // 模进级数
	axis   int // 倒影轴
	b      int // 对比手段组合（0–5，见 contrastPairs）
	d      int // 节奏变奏的密度
	k3     int // 裂解的模进方向
	c      int // C 段的模进级数（2 或 3）
	k0     int // 1 小节动机扩成 2 小节时的模进级数
}

// SlotSpec 描述一格怎么从动机生成，用户可以逐项修改（EditSlot）。变换按 素材 → 裂解 → 倒影 → 模进 → 节奏变奏 →
// 句尾改写 的顺序施加。零值为「动机原样」。
type SlotSpec struct {
	Source    int // 0 为动机的前 2 小节；1 为动机的第 3–4 小节（动机不足 3 小节时同 0）
	Frag      int // 裂解：0 不裂解，1 取开头，2 取末尾
	FragBeats int // 裂解取的拍数
	FragReps  int // 裂解重复的次数
	FragK     int // 裂解每次重复再模进的级数
	Inv       int // 倒影：0 不倒影，1 以首音、2 以主音、3 以属音为轴
	Seq       int // 调内模进的级数（正为上行）
	Var       int // 节奏变奏：-1 合并同音，0 不变，1 加经过音
	Cadence   int // 段末的终止（DevCad*）
	Region    int // 和声区域倾向（Region*，只影响配和声）
}

// SpecInvAxes 是倒影轴的名称，序号即 SlotSpec.Inv。
var SpecInvAxes = []string{"不倒影", "以首音为轴", "以主音为轴", "以属音为轴"}

// devContext 是一套方案生成时的素材与限制，供逐格修改（EditSlot）时重算：base 为 2 小节的动机单元，
// u1 为动机的第 3–4 小节（没有时为 nil），length 为一格的拍数，top 为舒适音域上限的级数。
type devContext struct {
	base, u1 []degNote
	k        keyModel
	length   float64
	top      int
	comfort  [2]int
	bars     int
	sub      int // 一拍里的八分音符数（HumSettings.Sub）
}

// unit 返回八分音符的拍数。
func (c *devContext) unit() float64 { return 1 / float64(c.sub) }

// applySpec 按 spec 生成一格的音，并返回说明。
func applySpec(sp SlotSpec, ctx *devContext) ([]degNote, string) {
	src := ctx.base
	var desc []string
	if sp.Source == 1 && len(ctx.u1) > 0 {
		src = ctx.u1
		desc = append(desc, "动机第 3–4 小节")
	}
	out := slices.Clone(src)
	if sp.Frag > 0 {
		beats, reps := max(sp.FragBeats, 1), max(sp.FragReps, 1)
		out = frag(out, sp.Frag == 2, float64(beats), reps, ctx.length, sp.FragK)
		part := map[int]string{1: "开头", 2: "末尾"}[sp.Frag]
		d := fmt.Sprintf("裂解%s %d 拍 ×%d", part, beats, reps)
		if sp.FragK != 0 {
			d += "（每次" + stepDesc(sp.FragK) + "）"
		}
		desc = append(desc, d)
	}
	if sp.Inv > 0 {
		out = inv(out, sp.Inv-1)
		desc = append(desc, SpecInvAxes[sp.Inv]+"倒影")
	}
	if sp.Seq != 0 {
		out = seq(out, sp.Seq)
		desc = append(desc, stepDesc(sp.Seq))
	}
	if sp.Var != 0 {
		if v := vary(out, sp.Var, ctx.unit()); len(v) != len(out) {
			out = v
			desc = append(desc, map[int]string{-1: "合并同音", 1: "加经过音"}[sp.Var])
		}
	}
	switch sp.Region {
	case RegionSub:
		desc = append(desc, "和声转向下属")
	case RegionRelative:
		desc = append(desc, "和声转向关系调")
	}
	if sp.Cadence != DevCadNone {
		out = cadence(out, sp.Cadence, ctx.length, true, ctx.unit())
		desc = append(desc, map[int]string{DevCadHalf: "半终止", DevCadFull: "全终止"}[sp.Cadence])
	}
	if len(desc) == 0 {
		return out, "动机原样"
	}
	return out, strings.Join(desc, "，")
}

// stepDesc 是模进的说明。
func stepDesc(k int) string {
	switch {
	case k > 0:
		return fmt.Sprintf("上行模进 %d 级", k)
	case k < 0:
		return fmt.Sprintf("下行模进 %d 级", -k)
	}
	return "原样重复"
}

// contrastPairs 是 B / C 段的对比手段组合（四选二）：0 音区、1 反向轮廓、2 节奏密度、3 和声区域。
var contrastPairs = [6][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}

// contrastSpec 返回 B 段第一格的生成方式：按组合 b 取两种对比手段。换音区时头顶还有至少 3 级的空间则上移 3 级，否则下移。
func contrastSpec(b int, ctx *devContext) SlotSpec {
	var sp SlotSpec
	register := false
	for _, m := range contrastPairs[b] {
		switch m {
		case 0:
			register = true
		case 1:
			sp.Inv = 2
		case 2:
			sp.Frag, sp.FragBeats, sp.FragReps, sp.FragK = 1, 1, 4, 1
		case 3:
			sp.Region = RegionSub
		}
	}
	if register {
		notes, _ := applySpec(sp, ctx)
		hi := -99
		for _, n := range notes {
			hi = max(hi, n.deg)
		}
		sp.Seq = -3
		if hi+3 <= ctx.top {
			sp.Seq = 3
		}
	}
	return sp
}

// slotPlan 是一格的生成结果。
type slotPlan struct {
	label string
	spec  SlotSpec
	notes []degNote
	desc  string
}

// buildForm 按句式与参数生成各格（每格 2 小节）。
func buildForm(form string, p devParams, ctx *devContext) []slotPlan {
	plan := func(label string, sp SlotSpec) slotPlan {
		notes, desc := applySpec(sp, ctx)
		return slotPlan{label, sp, notes, desc}
	}
	A := plan("A", SlotSpec{})
	var specs []slotPlan
	switch form {
	case "parallel":
		specs = []slotPlan{A,
			plan("A'", SlotSpec{Source: 1, Seq: p.k1, Var: p.d, Cadence: DevCadHalf}),
			A,
			plan("A''", SlotSpec{Source: 1, Seq: p.k2, Cadence: DevCadFull})}
	case "qa":
		specs = []slotPlan{plan("a", SlotSpec{}),
			plan("a'", SlotSpec{Source: 1, Seq: p.k1, Cadence: DevCadHalf}),
			plan("b", SlotSpec{Inv: p.axis + 1}),
			plan("b'", SlotSpec{Source: 1, Frag: 2, FragBeats: 2, FragReps: 2, FragK: p.k3, Cadence: DevCadFull})}
	case "aaba8":
		specs = []slotPlan{A,
			plan("A'", SlotSpec{Source: 1, Seq: p.k1, Cadence: DevCadHalf}),
			plan("B", contrastSpec(p.b, ctx)),
			plan("A", SlotSpec{Cadence: DevCadFull})}
	case "aaba", "abac":
		b := contrastSpec(p.b, ctx)
		b2 := b
		b2.Seq--
		b2.Cadence = DevCadHalf
		specs = []slotPlan{A, plan("A", SlotSpec{Source: 1, Seq: p.k1, Cadence: DevCadHalf})}
		if form == "aaba" {
			specs = append(specs, plan("A'", SlotSpec{}), plan("A'", SlotSpec{Source: 1, Seq: p.k2, Cadence: DevCadFull}),
				plan("B", b), plan("B", b2),
				plan("A", SlotSpec{}), plan("A", SlotSpec{Source: 1, Seq: p.k1, Cadence: DevCadFull}))
		} else {
			b2.Cadence = DevCadFull
			specs = append(specs, plan("B", b), plan("B", b2), specs[0], specs[1],
				plan("C", SlotSpec{Seq: p.c}),
				plan("C", SlotSpec{Frag: 1, FragBeats: 1, FragReps: 2, FragK: -1, Cadence: DevCadFull}))
		}
	}
	return specs
}

// formParams 返回句式需要枚举的参数集合（其余参数为 0）。
func formParams(form string, oneBar bool) []devParams {
	var out []devParams
	ks := []int{}
	for k := -devSeqRange; k <= devSeqRange; k++ {
		ks = append(ks, k)
	}
	k0s := []int{0}
	if oneBar {
		k0s = []int{1, -1, -2}
	}
	for _, k0 := range k0s {
		switch form {
		case "parallel":
			for _, k1 := range ks {
				for _, k2 := range ks {
					for _, d := range []int{-1, 0, 1} {
						out = append(out, devParams{k1: k1, k2: k2, d: d, k0: k0})
					}
				}
			}
		case "qa":
			for _, k1 := range ks {
				for axis := range 3 {
					for _, k3 := range []int{-1, 1} {
						out = append(out, devParams{k1: k1, axis: axis, k3: k3, k0: k0})
					}
				}
			}
		case "aaba8":
			for _, k1 := range ks {
				for b := range len(contrastPairs) {
					out = append(out, devParams{k1: k1, b: b, k0: k0})
				}
			}
		case "aaba":
			for _, k1 := range ks {
				for _, k2 := range ks {
					for b := range len(contrastPairs) {
						out = append(out, devParams{k1: k1, k2: k2, b: b, k0: k0})
					}
				}
			}
		case "abac":
			for _, k1 := range ks {
				for b := range len(contrastPairs) {
					for _, c := range []int{2, 3} {
						out = append(out, devParams{k1: k1, b: b, c: c, k0: k0})
					}
				}
			}
		}
	}
	return out
}

// —— 评分 ——

// contourSim 是两段旋律的轮廓相似度：相邻级数差（截到 ±3）序列的编辑距离换算成 0–1。
func contourSim(x, y []degNote) float64 {
	diff := func(u []degNote) []int {
		var d []int
		for i := 1; i < len(u); i++ {
			d = append(d, min(max(u[i].deg-u[i-1].deg, -3), 3))
		}
		return d
	}
	a, b := diff(x), diff(y)
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	return 1 - float64(editDistance(a, b))/float64(max(len(a), len(b)))
}

func editDistance(a, b []int) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// rhythmSim 是节奏相似度：两段在八分音符网格上的起音位图的 Jaccard 系数。
func rhythmSim(x, y []degNote, length float64, sub int) float64 {
	bits := func(u []degNote) map[int]bool {
		m := map[int]bool{}
		for _, n := range u {
			if n.onset < length-1e-9 {
				m[int(math.Round(n.onset*float64(sub)))] = true
			}
		}
		return m
	}
	a, b := bits(x), bits(y)
	inter, union := 0, len(b)
	for k := range a {
		if b[k] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

// slotSim 是一格与动机的相似度（轮廓 0.6、节奏 0.4）。
func slotSim(x, u []degNote, length float64, sub int) float64 {
	return 0.6*contourSim(x, u) + 0.4*rhythmSim(x, u, length, sub)
}

// scorePeriod 给一套方案打分；硬约束不满足（终止音、段间大跳、末音太短）时 ok 为 false。
func scorePeriod(plans []slotPlan, u0 []degNote, k keyModel, length float64, bars int, comfort [2]int, sub int) (float64, string, bool) {
	total := 100.0
	var unity, contr float64
	// 硬约束：终止音与末音时值。
	for i, pl := range plans {
		if len(pl.notes) == 0 {
			return 0, "", false
		}
		last := pl.notes[len(pl.notes)-1]
		deg := ((last.deg % 7) + 7) % 7
		switch pl.spec.Cadence {
		case DevCadFull:
			if deg != 0 || last.dur < 1-1e-9 {
				return 0, "", false
			}
		case DevCadHalf:
			if deg != 1 && deg != 4 && deg != 6 {
				return 0, "", false
			}
		}
		if i == len(plans)-1 && pl.spec.Cadence != DevCadFull {
			return 0, "", false
		}
	}
	// 统一与对比。
	for _, pl := range plans {
		sim := slotSim(pl.notes, u0, length, sub)
		switch pl.label[0] {
		case 'A', 'a':
			if sim < devUnitySim {
				unity += (devUnitySim - sim) * devWUnity
			}
		default:
			switch {
			case sim < devContrastLo:
				contr += (devContrastLo - sim) * devWContrast
			case sim > devContrastHi:
				contr += (sim - devContrastHi) * devWContrast
			}
		}
	}
	total -= unity + contr
	// 音域、跨度、高潮。
	lo, hi := 999, -999
	hiBar, hiSlots := 0.0, map[int]bool{}
	over := 0
	for i, pl := range plans {
		for _, n := range pl.notes {
			m := k.toMIDI(n.deg, n.alt)
			lo = min(lo, m)
			over += max(0, m-comfort[1]) + max(0, comfort[0]-m)
			if m > hi {
				hi, hiBar = m, float64(i*devSlotBars)+n.onset/(length/devSlotBars)+1
				clear(hiSlots)
			}
			if m == hi {
				hiSlots[i] = true
			}
		}
	}
	rangePen := float64(over)*devWRange + float64(max(0, hi-lo-devMaxSpan))*devWSpan
	climaxBar := 5.5
	if bars == 16 {
		climaxBar = 11
	}
	climax := math.Abs(hiBar-climaxBar) / float64(bars) * devWClimax
	if len(hiSlots) > 2 {
		climax += devWClimaxDup
	}
	total -= rangePen + climax
	// 段间连接与重复。
	for i := 1; i < len(plans); i++ {
		a, b := plans[i-1].notes, plans[i].notes
		jump := abs(k.toMIDI(b[0].deg, b[0].alt) - k.toMIDI(a[len(a)-1].deg, a[len(a)-1].alt))
		if jump > devJoinHard {
			return 0, "", false
		}
		if jump > devJoinMax {
			total -= float64(jump-devJoinMax) * devWJoin
		}
		if plans[i].label != plans[i-1].label && slices.Equal(a, b) {
			total -= devWSame
		}
	}
	notes := fmt.Sprintf("统一 −%.0f · 对比 −%.0f · 音域 −%.0f · 高潮 −%.0f", unity, contr, rangePen, climax)
	return max(total, 0), notes, true
}

// periodDist 是两套方案的差异：句式不同为 1，否则为 1 − 各格相似度的平均。
func periodDist(a, b Period, length float64) float64 {
	if a.Form != b.Form || len(a.Slots) != len(b.Slots) {
		return 1
	}
	sum := 0.0
	for i := range a.Slots {
		sum += slotSim(a.Slots[i].notes, b.Slots[i].notes, length, a.ctx.sub)
	}
	return 1 - sum/float64(len(a.Slots))
}

// Develop 把动机发展成乐段，返回最多 n 套彼此差异足够大的方案（按评分从高到低）。动机没有音符时为 nil。
func Develop(h Hum, o DevelopOptions, n int) []Period {
	if len(h.Notes) == 0 || h.Settings.BeatsPerBar <= 0 {
		return nil
	}
	bars := o.Bars
	if bars != 16 {
		bars = 8
	}
	k := newKeyModel(h)
	bpb := float64(h.Settings.BeatsPerBar)
	length := devSlotBars * bpb
	u0, u1 := devUnits(h, k)
	if len(u0) == 0 {
		return nil
	}
	oneBar := h.Settings.Bars == 1
	// 舒适音域：动机音域向下 4、向上 5 个半音，限制在 C3–C6。
	mlo, mhi := 127, 0
	for _, nt := range h.Notes {
		m := int(math.Round(nt.Pitch))
		mlo, mhi = min(mlo, m), max(mhi, m)
	}
	comfort := [2]int{max(mlo-devComfortBelow, devComfortMin), min(mhi+devComfortAbove, devComfortMax)}
	top, _ := k.toDeg(comfort[1])
	if len(u1) == 0 {
		u1 = nil
	}
	type cand struct {
		p     Period
		form  int
		param devParams
	}
	var cands []cand
	for fi, f := range DevForms {
		if f.Bars != bars || (o.Form != "" && o.Form != f.Key) {
			continue
		}
		for _, p := range formParams(f.Key, oneBar) {
			base := u0
			if oneBar {
				// 1 小节的动机：后一小节为模进，组成 2 小节的单元。
				base = append(slices.Clone(u0), shift(seq(u0, p.k0), bpb)...)
			}
			ctx := &devContext{base: base, u1: u1, k: k, length: length, top: top, comfort: comfort, bars: bars, sub: h.Settings.Sub()}
			plans := buildForm(f.Key, p, ctx)
			for i, l := range o.Locked {
				if i < len(plans) {
					plans[i] = slotPlan{l.Label, l.Spec, l.notes, strings.TrimSuffix(l.Desc, "（已锁定）") + "（已锁定）"}
				}
			}
			score, notes, ok := scorePeriod(plans, base, k, length, bars, comfort, ctx.sub)
			if !ok {
				continue
			}
			cands = append(cands, cand{Period{Form: f.Title, Bars: bars, Score: score, Notes: notes, Slots: toSlots(plans), ctx: ctx}, fi, p})
		}
	}
	// 按评分降序，同分按句式与参数的先后（枚举顺序），保证确定性。
	slices.SortStableFunc(cands, func(a, b cand) int { return cmp.Compare(b.p.Score, a.p.Score) })
	var out []Period
	for _, minDist := range []float64{devMinDist, devMinDist * 0.7} {
		for _, c := range cands {
			if len(out) >= n {
				break
			}
			ok := true
			for _, q := range out {
				if periodDist(c.p, q, length) < minDist {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, c.p)
			}
		}
	}
	for i := range out {
		out[i].Hum = expandPeriod(h, out[i], k, o.Filler)
	}
	return out
}

// shift 把单元整体后移 beats 拍。
func shift(u []degNote, beats float64) []degNote {
	out := slices.Clone(u)
	for i := range out {
		out[i].onset += beats
	}
	return out
}

// toSlots 把各格的生成结果整理成 DevSlot。
func toSlots(plans []slotPlan) []DevSlot {
	out := make([]DevSlot, len(plans))
	for i, pl := range plans {
		out[i] = DevSlot{Label: pl.label, Bar: i * devSlotBars, Spec: pl.spec, Desc: pl.desc, Cadence: pl.spec.Cadence,
			Region: pl.spec.Region, notes: pl.notes}
	}
	return out
}

// expandPeriod 把方案展开成乐句：级数换回 MIDI、拍换成秒，去掉越界与重叠的部分，填拟声字。
func expandPeriod(h Hum, p Period, k keyModel, filler int) Hum {
	set := h.Settings
	set.Bars, set.CountIn, set.Free, set.BarShift = p.Bars, 0, false, 0
	beat := set.Beat()
	bpb := float64(set.BeatsPerBar)
	length := devSlotBars * bpb
	minDur := set.Grid() / beat // 最短时值（拍）：十六分音符
	var notes []MelodyNote
	for _, sl := range p.Slots {
		off := float64(sl.Bar) * bpb
		for _, n := range sl.notes {
			if n.onset >= length-1e-9 || n.dur < minDur-1e-9 {
				continue
			}
			end := min(n.onset+n.dur, length)
			notes = append(notes, MelodyNote{Start: (off + n.onset) * beat, End: (off + end) * beat,
				Pitch: float64(k.toMIDI(n.deg, n.alt)), Slur: !n.head})
		}
	}
	slices.SortStableFunc(notes, func(a, b MelodyNote) int { return cmp.Compare(a.Start, b.Start) })
	for i := 0; i+1 < len(notes); i++ {
		notes[i].End = min(notes[i].End, notes[i+1].Start)
	}
	notes = slices.DeleteFunc(notes, func(n MelodyNote) bool { return n.End-n.Start < minDur*beat-1e-9 })
	for i := range notes {
		// 与前一音之间有休止时，连音标记失效（成为新的字）。
		if notes[i].Slur && (i == 0 || notes[i-1].End < notes[i].Start-1e-9) {
			notes[i].Slur = false
		}
	}
	// 同一个字里相邻的同音合并（模进、倒影后换回 MIDI 时，两个音可能落到同一个音高上）。
	for i := 1; i < len(notes); {
		if notes[i].Slur && notes[i].Pitch == notes[i-1].Pitch && math.Abs(notes[i].Start-notes[i-1].End) < 1e-9 {
			notes[i-1].End = notes[i].End
			notes = slices.Delete(notes, i, i+1)
			continue
		}
		i++
	}
	out := Hum{Settings: set, Notes: notes, TonicPC: h.TonicPC, Mode: h.Mode, KeyClear: h.KeyClear, Voiced: h.Voiced}
	FillLyrics(out.Notes, filler, beat)
	return out
}

// EditSlot 按 sp 重新生成方案 p 的第 i 格（其余格不变），重新评分并展开成乐句；h 为动机，filler 为拟声字组。
// 修改后不满足终止或连接的硬约束时仍返回结果，评分为 0 并在 Notes 中说明。p 不是 Develop 生成的（没有素材）时原样返回。
func EditSlot(h Hum, p Period, i int, sp SlotSpec, filler int) Period {
	ctx := p.ctx
	if ctx == nil || i < 0 || i >= len(p.Slots) {
		return p
	}
	notes, desc := applySpec(sp, ctx)
	p.Slots = slices.Clone(p.Slots)
	old := p.Slots[i]
	p.Slots[i] = DevSlot{Label: old.Label, Bar: old.Bar, Spec: sp, Desc: desc, Cadence: sp.Cadence, Region: sp.Region, notes: notes}
	plans := make([]slotPlan, len(p.Slots))
	for j, sl := range p.Slots {
		plans[j] = slotPlan{sl.Label, sl.Spec, sl.notes, sl.Desc}
	}
	score, why, ok := scorePeriod(plans, ctx.base, ctx.k, ctx.length, ctx.bars, ctx.comfort, ctx.sub)
	if !ok {
		score, why = 0, "不合规范：段末终止音不对、末尾不是全终止或段间跳进超过八度"
	}
	p.Score, p.Notes = score, why
	p.Hum = expandPeriod(h, p, ctx.k, filler)
	return p
}
