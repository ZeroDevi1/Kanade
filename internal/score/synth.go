package score

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

// Accompaniment 返回钢琴要弹的音：和弦铺底，withMelody 为 true 且有旋律线时再加上旋律（按识别出的音高与时间）。
func (a *Analysis) Accompaniment(withMelody bool) []PianoNote {
	var out []PianoNote
	var prev []int
	for _, c := range a.Chords {
		v, ok := ChordVoicing(c, prev)
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
	if withMelody && a.Melody != nil {
		for _, n := range a.Melody.Notes {
			out = append(out, PianoNote{MIDI: int(n.Pitch + 0.5), Start: n.Start, End: n.End, Velocity: melodyVelocity})
		}
	}
	return out
}

// SynthPiano 合成全曲的钢琴（和弦铺底，可选加旋律），返回单声道采样（rate Hz），长度与 Duration 一致。
func (a *Analysis) SynthPiano(rate int, withMelody bool) []float32 {
	return RenderPiano(a.Accompaniment(withMelody), rate, a.Duration)
}
