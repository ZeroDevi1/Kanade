// Package synth 把分析结果合成成可试听的音频（钢琴与乐器音色、和弦铺底、多轨混音），并导出 MIDI。
// 速度与拍号的秒 ↔ tick 换算（TimeMap）也供 vocal 包的歌声合成工程导出使用。
package synth

import "kanade/internal/score"

// 和弦伴奏：用钢琴模型（piano.go）以弱力度的柱式和弦铺底。音高与五线谱一致（ChordVoicing：低音为根音，
// 高音声部为密集排列、相邻和弦按最小移动选择转位）；在每个和弦开始处以及其中的每个小节线处弹奏一次，
// 按住到下一次弹奏（相当于连音踏板），便于听出和弦的色彩而不盖过原曲。

// 铺底的力度：高音声部 pp–p，低音略重以托住和声。
const (
	padUpperVelocity = 0.30
	padBassVelocity  = 0.38
	padLegato        = 0.04 // 与下一次弹奏重叠的时长（秒），避免和弦之间出现空隙
)

// 旋律的力度：比铺底明显，作为主奏。
const melodyVelocity = 0.55

// ChordPad 返回和弦铺底要弹的音。
func ChordPad(a *score.Analysis) []PianoNote {
	var out []PianoNote
	var prev []int
	for _, c := range a.Chords {
		v, ok := score.ChordVoicing(c, prev)
		if !ok {
			continue
		}
		prev = v.MIDIs()
		strikes := []float64{c.Start}
		for _, d := range a.Downbeats {
			if d > c.Start+0.2 && d < c.End-0.2 {
				strikes = append(strikes, d)
			}
		}
		for i, t0 := range strikes {
			t1 := c.End
			if i+1 < len(strikes) {
				t1 = strikes[i+1]
			}
			t1 += padLegato
			out = append(out, PianoNote{MIDI: v.Bass.MIDI, Start: t0, End: t1, Velocity: padBassVelocity})
			for _, u := range v.Upper {
				out = append(out, PianoNote{MIDI: u.MIDI, Start: t0, End: t1, Velocity: padUpperVelocity})
			}
		}
	}
	return out
}

// MelodyPiano 返回旋律要弹的音（按识别出的音高与时间）；没有旋律线时为 nil。
func MelodyPiano(a *score.Analysis) []PianoNote {
	if a.Melody == nil {
		return nil
	}
	out := make([]PianoNote, 0, len(a.Melody.Notes))
	for _, n := range a.Melody.Notes {
		out = append(out, PianoNote{MIDI: int(n.Pitch + 0.5), Start: n.Start, End: n.End, Velocity: melodyVelocity})
	}
	return out
}

// TrackPCM 合成 tracks（a.Tracks() 的结果）中的第 i 条音轨（单声道，rate Hz，长度与 Duration 一致），
// 供分轨静音 / 独奏混音。钢琴两轨不做电平处理，和弦铺底与旋律保持一起合成时的相对电平；
// 乐器音轨按 RMS 调到相近的响度。原曲轨与没有音符的音轨返回 nil。
func TrackPCM(a *score.Analysis, tracks []Track, i, rate int) []float32 {
	if i == TrackOriginal {
		return nil
	}
	return RenderTrack(tracks[i], rate, a.Duration, i >= TrackInstruments)
}
