package score

import (
	"math"
	"slices"
)

// 哼唱旋律的合并精简（灵感页）：哼唱时音高不稳，整理出的乐句常有起音滑入、字尾滑出、字内半音抖动、邻音与经过音、
// 被切出来的十六分音符碎片，以及音与音之间被切开的短空隙。SimplifyMelody 在不改变旋律骨架（主要音高、字的节奏位置）
// 的前提下去掉这些，只合并、不新增，合并后的起止仍在网格上、互不重叠。
//
// 规则与常量经 Claude Fable 5.1 咨询；另加两处：网格用十六分音符的 1/3（整理结果里可能有八分三连音），以及「补连奏」
// （哼唱的音常被切成一个十六分音符加一段空隙，短空隙并入前一个字）。级别：0 原样，1 适中（去装饰与抖动、补短空隙），
// 2 精简（基本每字一音、最小时值提到八分音符）。

// SimplifyLevels 是精简程度的名称（界面选项），序号即级别。
var SimplifyLevels = []string{"原样", "适中", "精简"}

// simplifyParams 是一个级别的阈值，时值以「格」（十六分音符）计。
type simplifyParams struct {
	slideMax, slideInterval       int // 滑入 / 滑出音的最长时值与和目标音的最大半音差
	jitterNoteMax, jitterInterval int // 抖动中每个音的最长时值、两音的最大半音差
	nbrMax, passMax               int // 邻音、经过音的最长时值
	fragMax, fragInterval         int // 字间碎片的最长时值、与所并邻音的最大半音差
	gapMax                        int // 补连奏的最长空隙；-1 为按一拍
}

var simplifyTable = [3]simplifyParams{
	{},
	{slideMax: 1, slideInterval: 2, jitterNoteMax: 2, jitterInterval: 1, nbrMax: 1, passMax: 1, fragMax: 1, fragInterval: 1, gapMax: 2},
	// 精简级别的经过音也只合并一格长的：一个八分音符以上的级进转音由 oneNotePerSyllable 判断是否保留（与 Fable 的表
	// 不同：表中为 2 格，会把应保留的级进转音先拆掉）。
	{slideMax: 2, slideInterval: 2, jitterNoteMax: 2, jitterInterval: 2, nbrMax: 2, passMax: 1, fragMax: 2, fragInterval: 1, gapMax: -1},
}

const (
	simplifyUnitsPerCell = 3  // 一格（十六分音符）分成 3 个单位，八分三连音也落在整数单位上
	jitterMinRun         = 3  // 构成抖动的最少音数
	keepMaxNotes         = 3  // 精简级别下保留转音的条件：音数上限、每音至少一个八分音符、整字至少一拍
	fallMax              = 12 // 字尾掉音（一格长的下滑）的最大落差（半音）
)

// spNote 是精简中的音：起止以单位计。
type spNote struct {
	s, e, p int
	lyric   string
}

func (n spNote) dur() int { return n.e - n.s }

// SimplifyMelody 按级别 level 精简乐句的音符（返回新切片，不改 h）。拟声字需由调用方重填（FillLyrics）。
func SimplifyMelody(h Hum, level int) []MelodyNote {
	if level <= 0 || len(h.Notes) == 0 || h.Settings.BPM <= 0 {
		return slices.Clone(h.Notes)
	}
	level = min(level, len(simplifyTable)-1)
	sp := simplifyTable[level]
	set := h.Settings
	unit := set.Grid() / simplifyUnitsPerCell
	cell := simplifyUnitsPerCell
	eighth := 2 * cell
	beatU := 2 * set.Sub() * cell
	barU := beatU * max(set.BeatsPerBar, 1)
	strength := func(p int) int {
		switch {
		case p%barU == 0:
			return 3
		case p%beatU == 0:
			return 2
		case p%eighth == 0:
			return 1
		}
		return 0
	}
	// 换成单位并按字分组（字首为非连音的音）。
	var syls [][]spNote
	for _, n := range h.Notes {
		x := spNote{int(math.Round(n.Start / unit)), int(math.Round(n.End / unit)), int(math.Round(n.Pitch)), n.Lyric}
		if x.e <= x.s {
			continue
		}
		if !n.Slur || len(syls) == 0 {
			syls = append(syls, []spNote{x})
		} else {
			syls[len(syls)-1] = append(syls[len(syls)-1], x)
		}
	}
	gapMax := sp.gapMax * cell
	if sp.gapMax < 0 {
		gapMax = beatU
	}
	// A. 字内规则（滑入、滑出、抖动、邻音、经过音）、B. 字间碎片、补连奏（短空隙并入前一个字），以及精简级别的
	// C. 每字一音、D. 对齐八分音符，反复施加到不再变化：补连奏后字与字紧接、对齐后时值改变，都可能出现新的可合并之处；
	// 保证精简两次与一次相同。每轮至少合并或移动一处，次数有上限。
	for range len(h.Notes) + 1 {
		before := cloneSyllables(syls)
		for i := range syls {
			for syllableRules(&syls[i], sp, cell, strength, neighborsOf(syls, i)) {
			}
		}
		syls = mergeFragments(syls, sp, cell, level, strength)
		for i := 0; i+1 < len(syls); i++ {
			last := &syls[i][len(syls[i])-1]
			if gap := syls[i+1][0].s - last.e; gap > 0 && gap <= gapMax {
				last.e = syls[i+1][0].s
			}
		}
		if level >= 2 {
			for i := range syls {
				syls[i] = oneNotePerSyllable(syls[i], cell, beatU, strength)
			}
			syls = snapToEighth(syls, eighth, strength)
		}
		if equalSyllables(before, syls) {
			break
		}
	}
	// 换回音符：字首非连音并保留歌词，其余为连音。
	var out []MelodyNote
	for _, sy := range syls {
		for j, n := range sy {
			m := MelodyNote{Start: float64(n.s) * unit, End: float64(n.e) * unit, Pitch: float64(n.p), Slur: j > 0}
			if j == 0 {
				m.Lyric = n.lyric
			}
			out = append(out, m)
		}
	}
	return out
}

func cloneSyllables(syls [][]spNote) [][]spNote {
	out := make([][]spNote, len(syls))
	for i, sy := range syls {
		out[i] = slices.Clone(sy)
	}
	return out
}

func equalSyllables(a, b [][]spNote) bool {
	return slices.EqualFunc(a, b, func(x, y []spNote) bool { return slices.Equal(x, y) })
}

// neighborsOf 返回第 i 个字前后紧邻的音（裁决抖动的音高用；没有时为 nil）。
func neighborsOf(syls [][]spNote, i int) [2]*spNote {
	var nb [2]*spNote
	if i > 0 {
		nb[0] = &syls[i-1][len(syls[i-1])-1]
	}
	if i+1 < len(syls) {
		nb[1] = &syls[i+1][0]
	}
	return nb
}

// syllableRules 对一个字施加一次字内规则（按优先级，施加了一条就返回 true）。
func syllableRules(sy *[]spNote, sp simplifyParams, cell int, strength func(int) int, nb [2]*spNote) bool {
	s := *sy
	merge := func(i, j, pitch int) { // 把 s[i..j] 合成一个音
		s[i].e, s[i].p = s[j].e, pitch
		*sy = slices.Delete(s, i+1, j+1)
	}
	// 同音相连。
	for i := 0; i+1 < len(s); i++ {
		if s[i].p == s[i+1].p && s[i].e == s[i+1].s {
			merge(i, i+1, s[i].p)
			return true
		}
	}
	if len(s) < 2 {
		return false
	}
	// 滑入：字首短音与紧随的长音只差一两个半音：音高取后者，起点不动。
	if a, b := s[0], s[1]; a.dur() <= sp.slideMax*cell && abs(a.p-b.p) <= sp.slideInterval && b.dur() >= 2*a.dur() {
		lyric := a.lyric
		merge(0, 1, b.p)
		(*sy)[0].lyric = lyric
		return true
	}
	// 滑出：字尾短音并入前一个音（前一个音至少是它的两倍长，与滑入对称；等长的是级进转音，不是滑音）。
	// 字尾只有一格的下滑（收音时掉下去，常见于哼唱）不论落差多大都算滑出。
	if z, y := s[len(s)-1], s[len(s)-2]; z.dur() <= sp.slideMax*cell && y.dur() >= 2*z.dur() &&
		(abs(z.p-y.p) <= sp.slideInterval || (z.p < y.p && y.p-z.p <= fallMax && z.dur() <= cell)) {
		merge(len(s)-2, len(s)-1, y.p)
		return true
	}
	// 抖动：连续 ≥ jitterMinRun 个短音只在两个相差不大的音高间交替：合成一个音，音高取时长加权众数。
	for i := 0; i+jitterMinRun <= len(s); i++ {
		j := i
		for j+1 < len(s) && s[j+1].dur() <= sp.jitterNoteMax*cell && s[j].dur() <= sp.jitterNoteMax*cell &&
			abs(s[j+1].p-s[j].p) <= sp.jitterInterval && s[j+1].p != s[j].p && (j == i || s[j+1].p == s[j-1].p) {
			j++
		}
		if j-i+1 >= jitterMinRun {
			merge(i, j, modePitch(s[i:j+1], strength, nb))
			return true
		}
	}
	// 邻音：a b a（b 短、与 a 相差不超过两个半音）。
	for i := 0; i+2 < len(s); i++ {
		if a, b, c := s[i], s[i+1], s[i+2]; a.p == c.p && abs(b.p-a.p) <= 2 && b.dur() <= sp.nbrMax*cell {
			merge(i, i+2, a.p)
			return true
		}
	}
	// 经过音：a b c 级进、b 短且不在拍首：并入较长的一侧（相等并入前音）。
	for i := 0; i+2 < len(s); i++ {
		a, b, c := s[i], s[i+1], s[i+2]
		between := (a.p < b.p && b.p < c.p) || (a.p > b.p && b.p > c.p)
		if between && abs(a.p-b.p) <= 2 && abs(b.p-c.p) <= 2 && b.dur() <= sp.passMax*cell && strength(b.s) <= 1 {
			if c.dur() > a.dur() {
				s[i+2].s = b.s
				*sy = slices.Delete(s, i+1, i+2)
			} else {
				merge(i, i+1, a.p)
			}
			return true
		}
	}
	return false
}

// modePitch 返回一组音按时长加权的众数音高；平局取首次出现在更强拍位的，再平局取与段外相邻音（先后再前）相同的，
// 最后取第一个音。
func modePitch(run []spNote, strength func(int) int, nb [2]*spNote) int {
	w := map[int]int{}
	first := map[int]int{} // 音高首次出现的位置
	for _, n := range run {
		w[n.p] += n.dur()
		if _, ok := first[n.p]; !ok {
			first[n.p] = n.s
		}
	}
	best := run[0].p
	better := func(p int) bool {
		switch {
		case w[p] != w[best]:
			return w[p] > w[best]
		case strength(first[p]) != strength(first[best]):
			return strength(first[p]) > strength(first[best])
		}
		for _, x := range []*spNote{nb[1], nb[0]} {
			if x != nil && (x.p == p) != (x.p == best) {
				return x.p == p
			}
		}
		return false
	}
	for _, n := range run {
		if better(n.p) {
			best = n.p
		}
	}
	return best
}

// mergeFragments 把字间的碎片（单音、很短、与前字尾或后字首紧接且只差一个半音以内）并入邻字。
func mergeFragments(syls [][]spNote, sp simplifyParams, cell, level int, strength func(int) int) [][]spNote {
	for i := 0; i < len(syls); i++ {
		if len(syls[i]) != 1 || syls[i][0].dur() > sp.fragMax*cell {
			continue
		}
		f := syls[i][0]
		// 只并入不比它短的音：碎片不去喂大另一个碎音（例如字尾的掉音）。
		if i > 0 {
			p := &syls[i-1][len(syls[i-1])-1]
			if p.e == f.s && abs(f.p-p.p) <= sp.fragInterval && p.dur() >= f.dur() {
				p.e = f.e // 前字延长，字首位置都不变
				syls = slices.Delete(syls, i, i+1)
				i--
				continue
			}
		}
		if i+1 < len(syls) {
			n := &syls[i+1][0]
			if f.e == n.s && abs(f.p-n.p) <= sp.fragInterval && n.dur() >= f.dur() {
				switch {
				case strength(f.s) >= strength(n.s):
					n.s = f.s // 碎片在更强的拍位：后字提前到这里（修正无意的切分）
					n.lyric = f.lyric
				case level < 2:
					continue // 弱位的碎片在适中级别下保留
				}
				syls = slices.Delete(syls, i, i+1) // 精简级别：弱位碎片删去成休止
				i--
			}
		}
	}
	return syls
}

// oneNotePerSyllable 在精简级别下把一个字压成一个音，除非它是明确的级进转音（音数不多、相邻都在两个半音以内、
// 方向最多改变一次、每个音至少一个八分音符、整字至少一拍）。音高取时长加权众数，平局取拍位最强的。
func oneNotePerSyllable(sy []spNote, cell, beatU int, strength func(int) int) []spNote {
	if len(sy) < 2 {
		return sy
	}
	keep := len(sy) <= keepMaxNotes && sy[len(sy)-1].e-sy[0].s >= beatU
	turns, dir := 0, 0
	for i := range sy {
		if sy[i].dur() < 2*cell {
			keep = false
		}
		if i == 0 {
			continue
		}
		d := sy[i].p - sy[i-1].p
		if abs(d) > 2 {
			keep = false
		}
		if s := sign(d); s != 0 {
			if dir != 0 && s != dir {
				turns++
			}
			dir = s
		}
	}
	if keep && turns <= 1 {
		return sy
	}
	return []spNote{{s: sy[0].s, e: sy[len(sy)-1].e, p: modePitch(sy, strength, [2]*spNote{}), lyric: sy[0].lyric}}
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// snapToEighth 把字首、字尾与字内的边界移到八分音符网格上：字首在两侧的八分音符位置中取拍位更强的（相等取后者），
// 放不下（与前字重叠或本字不足一个八分音符）时并入前字；字尾与后字相接时随后字的字首，否则向上取整；
// 字内不足一个八分音符的音并入较长的邻音。
func snapToEighth(syls [][]spNote, eighth int, strength func(int) int) [][]spNote {
	var out [][]spNote
	for i, sy := range syls {
		end := sy[len(sy)-1].e
		prevEnd := math.MinInt
		if len(out) > 0 {
			prevEnd = out[len(out)-1][len(out[len(out)-1])-1].e
		}
		s := sy[0].s
		if s%eighth != 0 {
			lo := s - ((s%eighth)+eighth)%eighth
			hi := lo + eighth
			var cands []int
			for _, c := range []int{lo, hi} {
				if c >= prevEnd && end-c >= eighth {
					cands = append(cands, c)
				}
			}
			switch len(cands) {
			case 0:
				if len(out) > 0 { // 并入前字
					last := &out[len(out)-1][len(out[len(out)-1])-1]
					last.e = max(last.e, end)
				}
				continue
			case 1:
				s = cands[0]
			default:
				s = cands[1]
				if strength(cands[0]) > strength(cands[1]) {
					s = cands[0]
				}
			}
			sy[0].s = s
		}
		// 字尾：与后字相接时留给后字决定（后字吸附后再对齐），否则向上取整。
		if e := sy[len(sy)-1].e; e%eighth != 0 {
			e += eighth - e%eighth
			if i+1 < len(syls) {
				e = min(e, syls[i+1][0].s+eighth) // 不越过后字可能的新起点（后面再按实际起点截短）
			}
			sy[len(sy)-1].e = e
		}
		// 字内边界：就近取八分音符（相等取后），不足一个八分音符的音并入较长的邻音。
		for j := 1; j < len(sy); j++ {
			b := sy[j].s
			r := ((b % eighth) + eighth) % eighth
			if r != 0 {
				if r*2 >= eighth {
					b += eighth - r
				} else {
					b -= r
				}
				sy[j-1].e, sy[j].s = b, b
			}
		}
		for j := 0; j < len(sy) && len(sy) > 1; {
			if sy[j].dur() >= eighth {
				j++
				continue
			}
			switch {
			case j == 0:
				sy[1].s = sy[0].s
				sy = slices.Delete(sy, 0, 1)
			case j == len(sy)-1 || sy[j-1].dur() >= sy[j+1].dur():
				sy[j-1].e = sy[j].e
				sy = slices.Delete(sy, j, j+1)
			default:
				sy[j+1].s = sy[j].s
				sy = slices.Delete(sy, j, j+1)
			}
		}
		// 前字的字尾不能越过本字的新起点。
		if len(out) > 0 {
			last := &out[len(out)-1][len(out[len(out)-1])-1]
			last.e = min(last.e, sy[0].s)
		}
		out = append(out, sy)
	}
	// 截短后不足一个八分音符的字尾音（极少）与零长音去掉。
	for i := range out {
		out[i] = slices.DeleteFunc(out[i], func(n spNote) bool { return n.dur() <= 0 })
	}
	return slices.DeleteFunc(out, func(sy []spNote) bool { return len(sy) == 0 })
}
