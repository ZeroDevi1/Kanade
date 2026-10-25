package score

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sort"
)

// 导出 MIDI（Standard MIDI File，格式 1）：第 0 轨为速度与拍号，第 1 轨为旋律、第 2 轨为和弦，
// 音符与试听的钢琴音轨相同（MelodyPiano / ChordPad），便于导入 Synthesizer V 等软件编辑旋律、配和声。
//
// 时间换算：速度图让 MIDI 的每一拍落在识别出的拍点上，导入后与原曲音频从 0 秒起对齐。
//   - 第一个强拍对齐到小节线：其前的弱起写成 n/4 拍的弱起小节（n 为四舍五入的拍数），之后换成识别出的拍号；
//     不足 1/4 拍的前奏空白直接并入第一小节（偏移小于 1/4 拍）。
//   - 匀速的歌（Tempo.Stable）只写一个速度，便于在编辑器中按网格编辑；
//     有变速的歌每拍写一个速度，逐拍对齐原曲。
//
// 有多乐器转录时，和弦轨之后每种乐器一轨（按转录的音符数从多到少），音色为该乐器组的代表 GM 音色，鼓在通道 10。
// 轨道名与 Melody / Chords 一样用英文：轨道名元事件没有编码约定，mido 与不少 DAW 按 Latin-1 读取，中文会显示为乱码。
//
// 有歌词时旋律轨在每个音的按下处写歌词事件（FF 05，UTF-8）：字写在字的第一个音上，连音写 "-"
// （Synthesizer V、ACE Studio、VOCALOID 的延音记号），导入后连音 / 转音仍归同一个字。

// midiPPQ 是每四分音符的 tick 数：480 是 DAW 与 Synthesizer V 常用的分辨率。
const midiPPQ = 480

// MIDI 音轨的通道与音色：旋律在通道 1、和弦在通道 2，均为大钢琴（GM 音色 0）。
const (
	midiMelodyChannel = 0
	midiChordChannel  = 1
	// midiDrumChannel 是 GM 规定的打击乐通道（第 10 通道）。
	midiDrumChannel = 9
)

// midiInstrumentVelocity 是转录音符的力度：MuScriptor 不输出力度，取 mf 附近的固定值（约 90）。
const midiInstrumentVelocity = 0.7

// instrumentChannel 返回第 i 种有音高乐器的通道：依次用通道 3–16（跳过鼓通道 10），超过 13 种时循环复用
// （共用通道的乐器在播放时会互相改掉音色，但各轨音符仍分开，导入 DAW 后可逐轨重设）。
func instrumentChannel(i int) int {
	free := []int{2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15}
	return free[i%len(free)]
}

// midiAnchor 是时间（秒）与 tick 的对应点，相邻两点之间线性换算，并各自对应一个速度。
type midiAnchor struct {
	sec  float64
	tick float64
}

// midiTimeMap 把秒换算为 tick，并给出需要写入的速度与拍号事件。
type midiTimeMap struct {
	anchors []midiAnchor
	// sigs 是拍号事件：tick 与每小节拍数（以 denom 分音符为拍）。
	sigs []struct {
		tick  int
		beats int
	}
	// denom 是拍号的分母（4 或 8）；一拍占 beatTicks() 个 tick。
	denom int
}

// beatTicks 返回一拍（denom 分音符）的 tick 数。
func (m *midiTimeMap) beatTicks() int { return midiPPQ * 4 / m.denom }

func (m *midiTimeMap) tick(sec float64) int {
	a := m.anchors
	i := sort.Search(len(a), func(i int) bool { return a[i].sec > sec })
	switch {
	case i == 0:
		return 0
	case i >= len(a):
		i = len(a) - 1 // 末端之后按最后一段的速度外推
	}
	p, q := a[i-1], a[i]
	t := p.tick + (sec-p.sec)/(q.sec-p.sec)*(q.tick-p.tick)
	return max(0, int(math.Round(t)))
}

// seconds 是 tick 的逆换算：tick 对应的时间（秒），超出末端时按最后一段的速度外推。
func (m *midiTimeMap) seconds(tick int) float64 {
	a := m.anchors
	t := float64(tick)
	i := sort.Search(len(a), func(i int) bool { return a[i].tick > t })
	switch {
	case i == 0:
		return a[0].sec
	case i >= len(a):
		i = len(a) - 1
	}
	p, q := a[i-1], a[i]
	return p.sec + (t-p.tick)/(q.tick-p.tick)*(q.sec-p.sec)
}

// newMidiTimeMap 由拍点与小节线建立速度图；end 是需要覆盖到的最晚时间（秒）。
func (a *Analysis) newMidiTimeMap(end float64) *midiTimeMap {
	bpb := a.Meter.BeatsPerBar
	if bpb <= 0 {
		bpb = 4
	}
	m := &midiTimeMap{denom: 4}
	if a.Meter.Denominator == 8 {
		m.denom = 8
	}
	tpb := m.beatTicks()
	beats := a.Beats
	if len(beats) < 2 {
		// 没有拍点：按识别出的 BPM（没有时 120）匀速。
		bpm := 120.0
		if a.Tempo.BPM != nil && *a.Tempo.BPM > 0 {
			bpm = *a.Tempo.BPM
		}
		m.anchors = []midiAnchor{{0, 0}, {max(end, 1), max(end, 1) * bpm / 60 * midiPPQ}}
		m.sigs = append(m.sigs, struct{ tick, beats int }{0, bpb})
		return m
	}
	// 第一个强拍在拍点序列中的位置（没有小节线时取第一拍）。
	k0 := 0
	if len(a.Downbeats) > 0 {
		k0 = nearestIndex(beats, a.Downbeats[0])
	}
	intervals := make([]float64, len(beats)-1)
	for i := range intervals {
		intervals[i] = beats[i+1] - beats[i]
	}
	beatLen := median(intervals)
	fd := beats[k0]

	// 弱起：n 拍的弱起小节，从 0 秒到第一个强拍。
	n := int(math.Round(fd / beatLen))
	if fd < beatLen/4 {
		n = 0
	} else {
		n = max(n, 1)
	}
	t0 := float64(n * tpb)
	if n > 0 {
		m.anchors = append(m.anchors, midiAnchor{0, 0})
	}
	if n%bpb != 0 {
		m.sigs = append(m.sigs, struct{ tick, beats int }{0, n}, struct{ tick, beats int }{int(t0), bpb})
	} else {
		// 弱起正好是整小节（或没有弱起）：拍号写在 0 处，强拍自然落在小节线上。
		// 不能只写在第一个强拍处，否则之前的小节按 MIDI 默认的 4/4 计，非 4/4 拍的歌小节线会错位。
		m.sigs = append(m.sigs, struct{ tick, beats int }{0, bpb})
	}
	m.anchors = append(m.anchors, midiAnchor{fd, t0})

	stable := a.Tempo.Stable != nil && *a.Tempo.Stable
	if !stable {
		for k := k0 + 1; k < len(beats); k++ {
			m.anchors = append(m.anchors, midiAnchor{beats[k], t0 + float64((k-k0)*tpb)})
		}
	}
	// 末端：按平均拍长（匀速）或最后一拍的拍长（变速）外推到 end 之后。
	last := m.anchors[len(m.anchors)-1]
	step := beatLen
	if !stable && len(intervals) > 0 {
		step = intervals[len(intervals)-1]
	}
	tail := max(end, last.sec) + step
	m.anchors = append(m.anchors, midiAnchor{tail, last.tick + (tail-last.sec)/step*float64(tpb)})
	return m
}

// midiTempo 是从 tick 起的一段速度：每四分音符 us 微秒。
type midiTempo struct {
	tick int
	us   uint32
}

// tempos 返回各段的速度，相邻相同的合并。歌声合成工程导出（vocal.go）也用这份速度表，与 MIDI 保持一致。
func (m *midiTimeMap) tempos() []midiTempo {
	var out []midiTempo
	prev := uint32(0)
	for i := 0; i+1 < len(m.anchors); i++ {
		p, q := m.anchors[i], m.anchors[i+1]
		if q.tick <= p.tick {
			continue
		}
		us := uint32(math.Round((q.sec - p.sec) / ((q.tick - p.tick) / midiPPQ) * 1e6))
		us = min(max(us, 1), 1<<24-1) // 速度事件是 3 字节
		if us == prev {
			continue
		}
		prev = us
		out = append(out, midiTempo{int(math.Round(p.tick)), us})
	}
	return out
}

// tempoEvents 返回各段速度的 MIDI 事件。
func (m *midiTimeMap) tempoEvents() []midiEvent {
	var out []midiEvent
	for _, t := range m.tempos() {
		out = append(out, midiEvent{tick: t.tick, data: []byte{0xFF, 0x51, 3, byte(t.us >> 16), byte(t.us >> 8), byte(t.us)}})
	}
	return out
}

// midiEvent 是音轨中的一个事件；data 为不含时间差的原始字节。order 决定同一 tick 内的先后（音符关闭在打开之前）。
type midiEvent struct {
	tick  int
	order int
	data  []byte
}

// MIDI 返回分析结果的 MIDI 文件。没有和弦、旋律与转录音符时返回错误。
func (a *Analysis) MIDI() ([]byte, error) {
	melody, chords := a.MelodyPiano(), a.ChordPad()
	var parts []Instrument
	if a.Transcription != nil {
		parts = slices.DeleteFunc(slices.Clone(a.Transcription.Instruments), func(in Instrument) bool { return len(in.Notes) == 0 })
	}
	if len(melody) == 0 && len(chords) == 0 && len(parts) == 0 {
		return nil, errors.New("没有可导出的和弦、旋律或转录音符")
	}
	end := a.Duration
	for _, n := range append(slices.Clone(melody), chords...) {
		end = max(end, n.End)
	}
	for _, in := range parts {
		for _, n := range in.Notes {
			end = max(end, n.End)
		}
	}
	tm := a.newMidiTimeMap(end)

	conductor := []midiEvent{{data: append([]byte{0xFF, 0x03, byte(len("Tempo"))}, "Tempo"...)}}
	for _, s := range tm.sigs {
		// 拍号：分子、分母（2 的幂，2 = 四分音符）、每拍的 MIDI 时钟数、每四分音符的三十二分音符数。
		conductor = append(conductor, midiEvent{tick: s.tick, order: 1, data: tm.timeSignature(s.beats)})
	}
	conductor = append(conductor, tm.tempoEvents()...)

	// 旋律：同一时刻只保留一个音（后一个音开始时截断前一个），导入歌声合成软件时不会出现重叠音符。
	slices.SortStableFunc(melody, func(x, y PianoNote) int { return cmp.Compare(x.Start, y.Start) })
	for i := 0; i+1 < len(melody); i++ {
		melody[i].End = min(melody[i].End, melody[i+1].Start)
	}
	melodyTrack := noteTrack("Melody", midiMelodyChannel, 0, melody, tm)
	if a.Melody != nil {
		for _, n := range a.Melody.Notes {
			text := n.Lyric
			if text == "" && n.Slur {
				text = "-"
			}
			if text != "" {
				melodyTrack = append(melodyTrack, lyricEvent(tm.tick(n.Start), text))
			}
		}
	}
	tracks := [][]midiEvent{
		conductor,
		melodyTrack,
		noteTrack("Chords", midiChordChannel, 0, chords, tm),
	}
	pitched := 0
	for _, in := range parts {
		ch, prog := midiDrumChannel, 0
		if !in.Drum {
			ch, prog = instrumentChannel(pitched), min(max(in.Program, 0), 127)
			pitched++
		}
		notes := make([]PianoNote, len(in.Notes))
		for i, n := range in.Notes {
			notes[i] = PianoNote{MIDI: n.Pitch, Start: n.Start, End: n.End, Velocity: midiInstrumentVelocity}
		}
		tracks = append(tracks, noteTrack(in.TrackName(), ch, prog, notes, tm))
	}

	var buf bytes.Buffer
	buf.WriteString("MThd")
	binary.Write(&buf, binary.BigEndian, uint32(6))
	binary.Write(&buf, binary.BigEndian, [3]uint16{1, uint16(len(tracks)), midiPPQ})
	for _, tr := range tracks {
		writeTrack(&buf, tr)
	}
	return buf.Bytes(), nil
}

// noteTrack 把音符换成一条音轨的事件，音色为 GM 音色 program（0–127）。同一音高的音在下一次按下前松开
// （MIDI 不允许同音重叠）；换算后短于 1 tick 的音丢弃。
func noteTrack(name string, ch, program int, notes []PianoNote, tm *midiTimeMap) []midiEvent {
	ev := []midiEvent{
		{data: append([]byte{0xFF, 0x03, byte(len(name))}, name...)},
		{order: 1, data: []byte{byte(0xC0 | ch), byte(program)}},
	}
	byPitch := map[int][]PianoNote{}
	for _, n := range notes {
		if n.MIDI >= 0 && n.MIDI <= 127 {
			byPitch[n.MIDI] = append(byPitch[n.MIDI], n)
		}
	}
	for pitch, ns := range byPitch {
		slices.SortFunc(ns, func(x, y PianoNote) int { return cmp.Compare(x.Start, y.Start) })
		for i, n := range ns {
			on, off := tm.tick(n.Start), tm.tick(n.End)
			if i+1 < len(ns) {
				off = min(off, tm.tick(ns[i+1].Start))
			}
			if off <= on {
				continue
			}
			vel := byte(min(max(int(math.Round(n.Velocity*127)), 1), 127))
			ev = append(ev,
				midiEvent{tick: on, order: 3, data: []byte{byte(0x90 | ch), byte(pitch), vel}},
				midiEvent{tick: off, order: 2, data: []byte{byte(0x80 | ch), byte(pitch), 0}})
		}
	}
	return ev
}

// lyricEvent 是 tick 处的歌词元事件。order 与音符松开相同、早于按下，读取方把它归给同一 tick 按下的音。
func lyricEvent(tick int, text string) midiEvent {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0x05})
	writeVLQ(&b, uint32(len(text)))
	b.WriteString(text)
	return midiEvent{tick: tick, order: 2, data: b.Bytes()}
}

// writeTrack 写一个 MTrk 块：事件按 tick 与 order 排序，时间差用变长数量编码，末尾为音轨结束事件。
func writeTrack(buf *bytes.Buffer, ev []midiEvent) {
	slices.SortStableFunc(ev, func(x, y midiEvent) int {
		return cmp.Or(cmp.Compare(x.tick, y.tick), cmp.Compare(x.order, y.order), cmp.Compare(x.data[1], y.data[1]))
	})
	var body bytes.Buffer
	last := 0
	for _, e := range ev {
		writeVLQ(&body, uint32(e.tick-last))
		body.Write(e.data)
		last = e.tick
	}
	body.Write([]byte{0, 0xFF, 0x2F, 0})
	buf.WriteString("MTrk")
	binary.Write(buf, binary.BigEndian, uint32(body.Len()))
	buf.Write(body.Bytes())
}

// writeVLQ 写 MIDI 的变长数量：每字节 7 位，高位为 1 表示后面还有字节。
func writeVLQ(buf *bytes.Buffer, v uint32) {
	var b [5]byte
	i := len(b) - 1
	b[i] = byte(v & 0x7F)
	for v >>= 7; v > 0; v >>= 7 {
		i--
		b[i] = byte(v&0x7F) | 0x80
	}
	buf.Write(b[i:])
}

// nearestIndex 返回 xs（升序）中最接近 x 的下标。
func nearestIndex(xs []float64, x float64) int {
	i := sort.SearchFloat64s(xs, x)
	if i == len(xs) || i > 0 && x-xs[i-1] < xs[i]-x {
		i--
	}
	return i
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// timeSignature 返回 beats/denom 拍号的 MIDI 事件：分母写成 2 的幂；节拍器每「击」的 MIDI 时钟数（每四分音符 24 个）
// 在复合拍（八分音符为拍、拍数为 3 的倍数）时取附点四分音符（36），其余取一拍。
func (m *midiTimeMap) timeSignature(beats int) []byte {
	pow, clocks := byte(2), byte(24)
	if m.denom == 8 {
		pow, clocks = 3, 12
		if beats%3 == 0 {
			clocks = 36
		}
	}
	return []byte{0xFF, 0x58, 4, byte(beats), pow, clocks, 8}
}
