package score

import (
	"cmp"
	"slices"
)

// 试听的分轨混音：原曲、钢琴和弦、钢琴旋律，以及多乐器转录出的每种乐器（合成音色，见 instrsynth.go），
// 各有静音、独奏与音量，规则与常见的混音台一致。多轨卷帘（界面）与混音共用 Tracks 给出的音轨顺序。

// 固定音轨的序号；乐器音轨从 TrackInstruments 起，按 Transcription.Instruments 的顺序排列。
const (
	TrackOriginal = iota
	TrackChords
	TrackMelody
	TrackInstruments
)

// Track 是一条音轨：标题与要合成的音符（秒、MIDI 音高）。原曲轨没有音符。
type Track struct {
	Title string
	// Notes 按开始时间排序。鼓轨的音高是 GM 打击乐键位。
	Notes []PianoNote
	// Program 是合成用的 GM 音色（0–127）；Drum 为 true 时是鼓组，Program 无意义。钢琴两轨为 0（大钢琴）。
	Program int
	Drum    bool
}

// Tracks 返回全部音轨：原曲、钢琴和弦、钢琴旋律（没有旋律线时音符为空），然后是每种转录出音符的乐器。
func (a *Analysis) Tracks() []Track {
	out := []Track{
		{Title: "原曲"},
		{Title: "钢琴 · 和弦", Notes: a.ChordPad()},
		{Title: "钢琴 · 旋律", Notes: a.MelodyPiano()},
	}
	if a.Transcription != nil {
		for _, in := range a.Transcription.Instruments {
			notes := make([]PianoNote, len(in.Notes))
			for i, n := range in.Notes {
				notes[i] = PianoNote{MIDI: n.Pitch, Start: n.Start, End: n.End, Velocity: midiInstrumentVelocity}
			}
			out = append(out, Track{Title: in.Title(), Notes: notes, Program: in.Program, Drum: in.Drum})
		}
	}
	for i := range out {
		slices.SortStableFunc(out[i].Notes, func(x, y PianoNote) int { return cmp.Compare(x.Start, y.Start) })
	}
	return out
}

// TrackMix 是各音轨的混音设置，三个切片等长（音轨数）。Gain 为线性增益（0–2，1 为原电平）。
type TrackMix struct {
	Mute, Solo []bool
	Gain       []float64
}

// NewTrackMix 返回 n 条音轨的初始设置：只听原曲，其余音轨静音（不需要合成）。
func NewTrackMix(n int) TrackMix {
	var m TrackMix
	m.Resize(n)
	return m
}

// Resize 把设置调整为 n 条音轨（换了分析结果、乐器数不同时）：保留已有音轨的设置，新增的音轨静音、增益为 1。
func (m *TrackMix) Resize(n int) {
	for len(m.Mute) < n {
		m.Mute = append(m.Mute, len(m.Mute) != TrackOriginal)
		m.Solo = append(m.Solo, false)
		m.Gain = append(m.Gain, 1)
	}
	m.Mute, m.Solo, m.Gain = m.Mute[:n], m.Solo[:n], m.Gain[:n]
}

// Audible 报告各音轨是否发声：有任一音轨独奏时只有独奏的音轨发声，否则未静音的音轨发声。
// available 为 false 的音轨（例如没有识别旋律时的旋律轨）总是不发声。available 与 m 等长。
func (m TrackMix) Audible(available []bool) []bool {
	solo := false
	for i, s := range m.Solo {
		solo = solo || s && available[i]
	}
	out := make([]bool, len(m.Mute))
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
func (m TrackMix) OriginalOnly(available []bool) bool {
	a := m.Audible(available)
	for i := TrackOriginal + 1; i < len(a); i++ {
		if a[i] {
			return false
		}
	}
	return a[TrackOriginal] && m.Gain[TrackOriginal] == 1
}

// Synthesized 返回需要合成的发声音轨（原曲以外）的序号。
func (m TrackMix) Synthesized(available []bool) []int {
	var out []int
	for i, on := range m.Audible(available) {
		if on && i != TrackOriginal {
			out = append(out, i)
		}
	}
	return out
}

// MixTracks 按 m 把各音轨混成交错的 ch 声道采样（与原曲相同的帧数）。orig 为原曲的交错采样；
// synth[i] 为第 i 条合成音轨的单声道采样（原曲位置忽略；可为 nil 或较短，其余部分为静音）。结果限制在 [−1, 1]。
func MixTracks(orig []float32, ch int, synth [][]float32, m TrackMix, available []bool) []float32 {
	a := m.Audible(available)
	frames := len(orig) / ch
	g0 := float32(0)
	if a[TrackOriginal] {
		g0 = float32(m.Gain[TrackOriginal])
	}
	// 先把发声的合成音轨叠成一条单声道，再与原曲逐声道相加。
	syn := make([]float32, frames)
	for i, pcm := range synth {
		if i == TrackOriginal || i >= len(a) || !a[i] || m.Gain[i] == 0 {
			continue
		}
		g := float32(m.Gain[i])
		for j, x := range pcm[:min(len(pcm), frames)] {
			syn[j] += x * g
		}
	}
	out := make([]float32, frames*ch)
	for i := 0; i < frames; i++ {
		for k := 0; k < ch; k++ {
			v := orig[i*ch+k]*g0 + syn[i]
			out[i*ch+k] = max(-1, min(1, v))
		}
	}
	return out
}
