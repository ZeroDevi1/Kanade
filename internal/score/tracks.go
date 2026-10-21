package score

// 试听的分轨混音：原曲、钢琴和弦、钢琴旋律三条音轨，各有静音、独奏与音量，规则与常见的混音台一致。

// 音轨序号。
const (
	TrackOriginal = iota
	TrackChords
	TrackMelody
	TrackCount
)

// TrackMix 是各音轨的混音设置。Gain 为线性增益（0–2，1 为原电平）。
type TrackMix struct {
	Mute, Solo [TrackCount]bool
	Gain       [TrackCount]float64
}

// DefaultTrackMix 是初始设置：只听原曲，钢琴两轨静音（不需要合成）。
func DefaultTrackMix() TrackMix {
	return TrackMix{Mute: [TrackCount]bool{false, true, true}, Gain: [TrackCount]float64{1, 1, 1}}
}

// Audible 报告各音轨是否发声：有任一音轨独奏时只有独奏的音轨发声，否则未静音的音轨发声。
// available 为 false 的音轨（例如没有识别旋律时的旋律轨）总是不发声。
func (m TrackMix) Audible(available [TrackCount]bool) [TrackCount]bool {
	solo := false
	for i, s := range m.Solo {
		solo = solo || s && available[i]
	}
	var out [TrackCount]bool
	for i := range out {
		if !available[i] {
			continue
		}
		if solo {
			out[i] = m.Solo[i]
		} else {
			out[i] = !m.Mute[i]
		}
	}
	return out
}

// OriginalOnly 报告混音结果是否就是原曲本身（只有原曲发声且电平为 1），此时直接播放原文件即可。
func (m TrackMix) OriginalOnly(available [TrackCount]bool) bool {
	a := m.Audible(available)
	return a[TrackOriginal] && !a[TrackChords] && !a[TrackMelody] && m.Gain[TrackOriginal] == 1
}

// NeedsPiano 报告是否有钢琴音轨发声（需要先合成）。
func (m TrackMix) NeedsPiano(available [TrackCount]bool) bool {
	a := m.Audible(available)
	return a[TrackChords] || a[TrackMelody]
}

// MixTracks 按 m 把三条音轨混成交错的 ch 声道采样（与原曲相同的帧数）。orig 为原曲的交错采样，
// chords / melody 为单声道钢琴（可为 nil，较短时其余部分为静音）。结果限制在 [−1, 1]。
func MixTracks(orig []float32, ch int, chords, melody []float32, m TrackMix, available [TrackCount]bool) []float32 {
	a := m.Audible(available)
	frames := len(orig) / ch
	out := make([]float32, frames*ch)
	g := func(i int) float32 {
		if !a[i] {
			return 0
		}
		return float32(m.Gain[i])
	}
	g0, g1, g2 := g(TrackOriginal), g(TrackChords), g(TrackMelody)
	for i := 0; i < frames; i++ {
		syn := float32(0)
		if g1 != 0 && i < len(chords) {
			syn += chords[i] * g1
		}
		if g2 != 0 && i < len(melody) {
			syn += melody[i] * g2
		}
		for k := 0; k < ch; k++ {
			v := orig[i*ch+k]*g0 + syn
			out[i*ch+k] = max(-1, min(1, v))
		}
	}
	return out
}
