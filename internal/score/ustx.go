package score

import (
	"fmt"
	"strconv"
	"strings"
)

// 导出 OpenUtau 工程（.ustx，UTF-8 YAML）：一条人声音轨、一个声部（voice part），音符与歌词来自旋律线，
// 速度与拍号同 MIDI 导出。格式依据 OpenUtau 源码（OpenUtau.Core/Ustx、Format/USTx.cs）：
//   - 分辨率固定 480 tick 每四分音符（与 midiPPQ 相同）；字段名为下划线风格；拍号按小节序号（bar_position）记录。
//   - 版本写 0.6：自该版本起用 time_signatures / tempos 列表，此后的版本都能读取（读取时自动升级）；
//     表情（expressions）不写，OpenUtau 读取时会补上默认表情。
//   - 音符必须带 pitch（至少一个点，snap_first 时读取第一个点）与 vibrato，否则加载出错；取 OpenUtau 的默认值
//     （NotePresets：滑音从 -40 tick 起、长 80 tick，颤音周期 175 ms、深度 25 音分，颤音长度为 0 即不开启）。
//   - 歌手与音素器不写：打开后由用户选择歌手，音素器为默认。
// 连音写 "+"（OpenUtau 的延音记号，只接在首尾相接的前一个音上），没有歌词的音写 OpenUtau 的默认歌词 "a"。

// ustxVersion 是写入的工程格式版本，见文件头说明。
const ustxVersion = "0.6"

// USTX 返回旋律线的 OpenUtau 工程；name 为工程与声部名。没有旋律线时返回错误。
func (a *Analysis) USTX(name string) ([]byte, error) {
	notes, tm, err := a.vocalTrack()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	q := strconv.Quote // Go 的转义序列（\" \\ \n \xXX \uXXXX）都是 YAML 双引号字符串的合法转义
	fmt.Fprintf(&b, "name: %s\ncomment: \"\"\noutput_dir: Vocal\ncache_dir: UCache\nustx_version: %s\n", q(name), q(ustxVersion))
	b.WriteString("time_signatures:\n")
	for _, m := range tm.meters() {
		fmt.Fprintf(&b, "- bar_position: %d\n  beat_per_bar: %d\n  beat_unit: %d\n", m.bar, m.beats, m.denom)
	}
	b.WriteString("tempos:\n")
	for _, t := range tm.tempos() {
		fmt.Fprintf(&b, "- position: %d\n  bpm: %s\n", t.tick, strconv.FormatFloat(t.bpm(), 'f', -1, 64))
	}
	b.WriteString("tracks:\n- track_name: Vocal\n  track_color: Blue\n  mute: false\n  solo: false\n  volume: 0\n  pan: 0\n")
	fmt.Fprintf(&b, "voice_parts:\n- name: %s\n  comment: \"\"\n  track_no: 0\n  position: 0\n  duration: %d\n  notes:\n",
		q(name), notes[len(notes)-1].off)
	for _, n := range notes {
		lyric := n.lyric
		switch {
		case n.slur:
			lyric = "+"
		case lyric == "":
			lyric = "a"
		}
		fmt.Fprintf(&b, "  - position: %d\n    duration: %d\n    tone: %d\n    lyric: %s\n", n.on, n.off-n.on, n.pitch, q(lyric))
		b.WriteString("    pitch:\n      data:\n      - {x: -40, y: 0, shape: io}\n      - {x: 40, y: 0, shape: io}\n      snap_first: true\n")
		b.WriteString("    vibrato: {length: 0, period: 175, depth: 25, in: 10, out: 10, shift: 0, drift: 0, vol_link: 0}\n")
	}
	b.WriteString("wave_parts: []\n")
	return []byte(b.String()), nil
}
