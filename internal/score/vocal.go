package score

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
)

// 歌声合成工程导出（Synthesizer V 的 .svp 见 svp.go，OpenUtau 的 .ustx 见 ustx.go）共用的旋律音与时间轴。
// 时间轴与 MIDI 导出相同（midiTimeMap：弱起小节、匀速一个速度 / 变速逐拍），导入后与原曲音频从 0 秒起对齐；
// 两种工程都按小节序号记拍号，按 tick 记速度与音符（OpenUtau 为 480 分辨率，与 midiPPQ 相同）。

// vocalNote 是导出到工程的一个旋律音：on / off 为 midiPPQ 分辨率的 tick，pitch 为 MIDI 音高（0–127）。
// lyric 是字（只在字的第一个音上）；slur 为 true 表示同一个字的后续音（连音），此时 lyric 为空且紧接在前一个音之后。
type vocalNote struct {
	on, off, pitch int
	lyric          string
	slur           bool
}

// vocalMeter 是从第 bar 小节（从 0 起）开始的拍号，以四分音符为拍、每小节 beats 拍。
type vocalMeter struct {
	bar, beats int
}

// vocalTrack 返回旋律线的工程音符（按时间排序、互不重叠）与时间轴。没有旋律线时返回错误。
func (a *Analysis) vocalTrack() ([]vocalNote, *midiTimeMap, error) {
	if a.Melody == nil || len(a.Melody.Notes) == 0 {
		return nil, nil, errors.New("没有旋律线：分析时需要识别旋律")
	}
	ns := slices.Clone(a.Melody.Notes)
	slices.SortStableFunc(ns, func(x, y MelodyNote) int { return cmp.Compare(x.Start, y.Start) })
	end := a.Duration
	for _, n := range ns {
		end = max(end, n.End)
	}
	tm := a.newMidiTimeMap(end)

	isSlur := func(n MelodyNote) bool { return n.Slur && n.Lyric == "" }
	out := make([]vocalNote, 0, len(ns))
	for i, n := range ns {
		v := vocalNote{on: tm.tick(n.Start), off: tm.tick(n.End), pitch: min(max(int(n.Pitch+0.5), 0), 127),
			lyric: n.Lyric, slur: isSlur(n)}
		if i+1 < len(ns) {
			// 同一时刻只保留一个音（后一个音开始时截断）；下一个音是连音时把中间的空隙补上，
			// 因为两种软件都只把紧接在前一个音之后的延音记号接到前一个字上。
			if next := tm.tick(ns[i+1].Start); v.off > next || isSlur(ns[i+1]) {
				v.off = next
			}
		}
		if v.off <= v.on {
			// 换算后长度为 0 的音丢弃；它是字的第一个音时，字移到下一个连音上，免得连音失去所属的字。
			if v.lyric != "" && i+1 < len(ns) && isSlur(ns[i+1]) {
				ns[i+1].Lyric, ns[i+1].Slur = v.lyric, false
			}
			continue
		}
		if v.slur && (len(out) == 0 || out[len(out)-1].off != v.on) {
			v.slur = false // 前面没有紧接的音，连音无处可接，当作没有歌词的音
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, nil, errors.New("旋律音都短于一个 tick，没有可导出的音符")
	}
	return out, tm, nil
}

// meters 把拍号事件换算成按小节序号定位的拍号（第一个拍号总在 tick 0），弱起小节之后的拍号从第 1 小节开始。
func (m *midiTimeMap) meters() []vocalMeter {
	var out []vocalMeter
	bar, tick, beats := 0, 0, m.sigs[0].beats
	for _, s := range m.sigs {
		bar += (s.tick - tick) / (beats * midiPPQ)
		tick, beats = s.tick, s.beats
		if len(out) > 0 && out[len(out)-1].bar == bar {
			out[len(out)-1].beats = beats
			continue
		}
		out = append(out, vocalMeter{bar, beats})
	}
	return out
}

// bpm 返回速度段的每分钟拍数（四分音符）。
func (t midiTempo) bpm() float64 { return 60e6 / float64(t.us) }

// newUUID 返回随机的 UUID（版本 4）字符串，用作 Synthesizer V 工程中音符组的标识。
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
