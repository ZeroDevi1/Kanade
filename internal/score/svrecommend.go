package score

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// 按分析结果给歌姬打分推荐，并为所选歌姬建议演唱模式。打分是可解释的经验规则，每一项都给出理由：
//   - 语言（歌词按普通话写）：普通话母语 +3；能跨语种唱普通话 +1.5；不支持 −4。
//   - 音域：在 0、±12、±24 半音的移调中，取「落在歌姬推荐音域（两端各放宽 2 个半音）内的旋律音占比 × 4
//     − 每移一个八度 0.8」最高的；推荐音域未知时不加分，理由注明。
//   - 风格：歌曲的唤醒度 / 效价（情绪识别）与歌姬演唱模式的匹配度（modeMood），最高 +1.5。
//   - 曲风：商店标注的曲风与歌曲情绪大致相符时 +0.5（如激昂对 Rock）。
//   - 已安装 +0.5（SV2 中无法确认已安装的歌姬可能需要另行安装）。

// SingerPick 是一个歌姬的推荐结果。
type SingerPick struct {
	Index     int     // 在传入列表中的序号
	Score     float64 // 越高越推荐
	Transpose int     // 建议的移调（半音，12 的倍数）
	Reasons   []string
}

// moodTraits 是演唱模式名称（小写）对应的倾向：energetic 激烈、gentle 柔和、sad 暗、happy 明亮、neutral 中性。
var moodTraits = map[string]string{
	"power": "energetic", "powerful": "energetic", "belt": "energetic", "chest": "energetic", "passionate": "energetic",
	"rock": "energetic", "solid": "energetic", "firm": "energetic", "resonant": "energetic", "vivid": "energetic",
	"opera": "energetic", "theatrical": "energetic", "emotional": "energetic", "open": "energetic", "hoarse": "energetic",
	"soft": "gentle", "airy": "gentle", "whisper": "gentle", "gentle": "gentle", "tender": "gentle", "delicate": "gentle",
	"light": "gentle", "relaxed": "gentle", "languid": "gentle", "mellow": "gentle", "warm": "gentle", "lyric": "gentle",
	"dark": "sad", "sorrow": "sad", "cold": "sad", "cool": "sad", "mature": "sad",
	"bright": "happy", "lively": "happy", "smile": "happy", "sweet": "happy", "cute": "happy", "young": "happy",
	"clear": "neutral", "lucid": "neutral", "classic": "neutral", "unadorned": "neutral", "focused": "neutral",
	"singing": "neutral", "closed": "neutral", "elegant": "neutral",
}

// songMood 返回歌曲在各倾向上的程度（0–1）：由情绪识别的唤醒度与效价换算；没有情绪识别时都取中值。
func (a *Analysis) songMood() map[string]float64 {
	arousal, valence := 0.5, 0.5
	if e := a.Emotion; e != nil {
		arousal, valence = e.Arousal, e.Valence
	}
	return map[string]float64{"energetic": arousal, "gentle": 1 - arousal, "sad": 1 - valence, "happy": valence, "neutral": 0.5}
}

// modeFit 返回演唱模式与歌曲的匹配度（0–1）；名称不在 moodTraits 中（如 PLUS 声库里以其他角色命名的模式）时为 −1。
func modeFit(mode string, mood map[string]float64) float64 {
	trait, ok := moodTraits[strings.ToLower(mode)]
	if !ok {
		return -1
	}
	return mood[trait]
}

// RecommendSingers 给 singers 逐个打分，按分数从高到低返回。
func (a *Analysis) RecommendSingers(singers []SVSinger) []SingerPick {
	var pitches []float64
	if a.Melody != nil {
		for _, n := range a.Melody.Notes {
			pitches = append(pitches, n.Pitch)
		}
	}
	mood := a.songMood()
	topMood := ""
	if e := a.Emotion; e != nil && len(e.Moods) > 0 {
		topMood = e.Moods[0].Label
	}
	out := make([]SingerPick, len(singers))
	for i, s := range singers {
		p := SingerPick{Index: i}
		// 语言。
		switch {
		case len(s.Languages) > 0 && s.Languages[0] == "mandarin":
			p.Score += 3
			p.Reasons = append(p.Reasons, "普通话母语")
		case slices.Contains(s.Languages, "mandarin"):
			p.Score += 1.5
			p.Reasons = append(p.Reasons, "可跨语种唱普通话")
		default:
			p.Score -= 4
			p.Reasons = append(p.Reasons, "不支持普通话")
		}
		// 音域。
		if lo, hi, ok := s.Range(); ok && len(pitches) > 0 {
			best, bestShare := math.Inf(-1), 0.0
			for _, t := range []int{0, -12, 12, -24, 24} {
				in := 0
				for _, m := range pitches {
					if v := m + float64(t); v >= float64(lo-2) && v <= float64(hi+2) {
						in++
					}
				}
				share := float64(in) / float64(len(pitches))
				if v := 4*share - 0.8*math.Abs(float64(t))/12; v > best {
					best, bestShare, p.Transpose = v, share, t
				}
			}
			p.Score += best
			shift := "原调"
			if p.Transpose != 0 {
				shift = fmt.Sprintf("移调 %+d", p.Transpose)
			}
			p.Reasons = append(p.Reasons, fmt.Sprintf("音域 %s，%s %.0f%% 的音在内", strings.ReplaceAll(s.PitchRange, " ", ""), shift, bestShare*100))
		} else {
			p.Reasons = append(p.Reasons, "音域未知")
		}
		// 风格：最匹配的演唱模式。
		bestMode, bestFit := "", -1.0
		for _, m := range s.Modes {
			if f := modeFit(m, mood); f > bestFit {
				bestMode, bestFit = m, f
			}
		}
		if bestFit >= 0 {
			p.Score += 1.5 * bestFit
			if bestFit >= 0.6 {
				p.Reasons = append(p.Reasons, "有适合这首歌的「"+bestMode+"」模式")
			}
		}
		// 曲风。
		if genreFits(s.Genres, topMood) {
			p.Score += 0.5
			p.Reasons = append(p.Reasons, "曲风 "+strings.Join(s.Genres, "/"))
		}
		if s.Installed() {
			p.Score += 0.5
		} else {
			p.Reasons = append(p.Reasons, "未确认已安装")
		}
		out[i] = p
	}
	slices.SortStableFunc(out, func(x, y SingerPick) int {
		switch {
		case x.Score > y.Score:
			return -1
		case x.Score < y.Score:
			return 1
		}
		return 0
	})
	return out
}

// genreFits 报告商店曲风与歌曲最突出的情绪是否大致相符。
func genreFits(genres []string, mood string) bool {
	want := map[string][]string{
		"激昂": {"Rock", "Metal", "Electronic"}, "愤怒": {"Rock", "Metal"}, "宏大": {"Rock", "Classical", "Orchestral"},
		"欢快": {"Pop", "Dance", "Electronic"}, "悲伤": {"Ballad", "Pop", "Jazz"}, "怀旧": {"Ballad", "Jazz", "Pop"},
		"阴郁": {"Ballad", "Rock"}, "梦幻": {"Pop", "Electronic", "Ballad"},
	}[mood]
	for _, g := range genres {
		if slices.Contains(want, g) {
			return true
		}
	}
	return false
}

// SuggestModes 按歌曲情绪为歌姬建议演唱模式（VocalModes 的格式），以中性为主、倾向为辅：
//   - 声库有中性模式（Clear、Lucid 等）时取第一个，强度 50，作为基底；
//   - 再取与歌曲最匹配的非中性模式，只在匹配度明显（≥ 0.6，即情绪明显偏向一侧）时加上，强度 20–45。
//
// 最初按匹配度给到 30–100 时，唤醒度 0.63 的歌给 Weina 选了 Firm=74（偏紧的发声），渲染听起来像憋着唱
// （2026-10-24 实测），因此倾向模式只轻度叠加。没有可用的模式时返回空串（沿用声库默认）。
func (a *Analysis) SuggestModes(s SVSinger) string {
	mood := a.songMood()
	var parts []string
	for _, m := range s.Modes {
		if moodTraits[strings.ToLower(m)] == "neutral" {
			parts = append(parts, m+"=50")
			break
		}
	}
	best, bestFit := "", -1.0
	for _, m := range s.Modes {
		if moodTraits[strings.ToLower(m)] == "neutral" {
			continue
		}
		if f := modeFit(m, mood); f > bestFit {
			best, bestFit = m, f
		}
	}
	if bestFit >= 0.6 {
		parts = append(parts, fmt.Sprintf("%s=%.0f", best, 20+50*(bestFit-0.5)))
	}
	return strings.Join(parts, ", ")
}
