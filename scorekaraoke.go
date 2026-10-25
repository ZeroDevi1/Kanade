package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/mic"
	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// K 歌跟唱卡片：播放分离出的伴奏（没有时播放原曲），从麦克风录下演唱，实时显示目标旋律与唱出的音高；
// 停止后对照旋律线给出音准、覆盖与气息的评价（score.AnalyzeKaraoke），并列出最需要练习的乐句。
//
// 生命周期：sing 开始录音与播放；每帧检查伴奏是否播完，播完或点「停止」时 finish 停止录音并评价。
// 换记录、离开页面不会自动停止（与试听一致），切换记录时 sync 发现记录变了会先结束当前演唱。

// karaokeView 是实时视图显示的时间范围（秒）：当前位置之前 karaokeBehind、之后 karaokeAhead。
const (
	karaokeBehind = 3.0
	karaokeAhead  = 5.0
)

// karaokeGoodPhrase 是乐句「唱得好」的分数线（经验值：约 80% 的帧在 ±50 音分内且唱满）：达到的不列入多练。
const karaokeGoodPhrase = 85

// 伴奏音源（karaokeState.source）。
const (
	karaokeAccomp = iota
	karaokeOriginal
)

// karaokeState 是 K 歌卡片的状态，属于 scoreState。只在 UI 线程修改（录音回调只写 sess）。
type karaokeState struct {
	run *score.Run // 所属记录；与当前显示的记录不同时重置
	// accomp 是该记录分离出的伴奏路径（打开记录时查找一次；空为没有）；source 是所选音源。
	accomp string
	source int
	// accompAudio 解码分离出的伴奏（不显示波形）；播放原曲时直接用试听的原文件播放器（audioPreview.original），
	// 不再解码一遍。src 是当前所用的一个，由 sync 设置。
	accompAudio audioPreview
	src         *audioPreview
	// 进行中的演唱：capture 非 nil 时正在录音。start 是开始时的歌曲位置（秒）。
	capture *mic.Capture
	sess    *score.KaraokeSession
	start   float64
	// 上一次的评价与录音（16 位 WAV 字节在保存时才生成）。
	report    *score.KaraokeReport
	recording []float32
	err       string
}

// sync 在显示的记录变化时结束演唱并重置状态，查找伴奏；选了伴奏时开始解码它。
func (k *karaokeState) sync(a *app, run *score.Run) {
	if k.run != run {
		k.stop()
		k.accompAudio.reset()
		*k = karaokeState{run: run, accomp: run.AccompanimentStem()}
		if k.accomp == "" {
			k.source = karaokeOriginal
		}
	}
	k.src = &a.score.preview
	if k.source == karaokeAccomp && k.accomp != "" {
		k.accompAudio.load(a, k.accomp)
		k.src = &k.accompAudio
	}
}

// player 返回所选音源的播放器（原曲为试听的原文件播放器，不受分轨混音切换的影响）；解码中或未就绪时为 nil。
func (k *karaokeState) player() *audio.Player {
	if k.src == nil || k.src.loading {
		return nil
	}
	if k.src.original != nil {
		return k.src.original
	}
	return k.src.player
}

// singing 报告是否正在录音。
func (k *karaokeState) singing() bool { return k.capture != nil }

// stop 停止录音与播放（不评价）。
func (k *karaokeState) stop() {
	if k.capture != nil {
		k.capture.Stop()
		k.capture = nil
	}
	if p := k.player(); p != nil {
		p.Pause()
	}
}

// sing 从歌曲时间 from（秒）开始播放伴奏并录音。
func (a *app) sing(from float64) {
	k := &a.score.karaoke
	p := k.player()
	if p == nil {
		return
	}
	if pv := a.score.preview.player; pv != nil && pv != p {
		pv.Pause() // 试听（含分轨混音）与伴奏同时出声会干扰录音
	}
	sess := score.NewKaraokeSession(mic.Rate)
	p.Seek(time.Duration(from * float64(time.Second)))
	// 录音回调在录音线程上：读取播放位置（audio.Player 内部加锁）后交给会话。
	capture, err := mic.Start(func(s []float32) { sess.Push(s, p.Position().Seconds()) })
	if err != nil {
		k.err = err.Error()
		return
	}
	k.capture, k.sess, k.start, k.report, k.recording, k.err = capture, sess, from, nil, nil, ""
	p.Play()
}

// finish 停止演唱并评价 [start, 当前位置] 内的旋律音。
func (a *app) finish(res *score.Analysis) {
	k := &a.score.karaoke
	if !k.singing() {
		return
	}
	end := k.player().Position().Seconds()
	k.stop()
	frames := k.sess.Frames()
	k.recording = k.sess.Recording()
	r := score.AnalyzeKaraoke(res.Melody.Notes, frames, k.start, end)
	k.report, k.sess = &r, nil
	if r.Voiced == 0 {
		k.err = "没有录到演唱：请检查麦克风是否选对、音量是否足够（系统设置中的输入设备）。"
	}
}

// karaokeCardFunc 返回构建 K 歌卡片的函数（供 cardRow）；没有旋律线时为 nil，不显示卡片。
func karaokeCardFunc(a *app, c *ui.Context, tk *fluent.Tokens, run *score.Run, res *score.Analysis) func() ui.Element {
	if res.Melody == nil || len(res.Melody.Notes) == 0 {
		a.score.karaoke.stop()
		return nil
	}
	return func() ui.Element { return a.scoreKaraokeCard(c, tk, run, res) }
}

// scoreKaraokeCard 是 K 歌卡片（res 须有旋律线）。
func (a *app) scoreKaraokeCard(c *ui.Context, tk *fluent.Tokens, run *score.Run, res *score.Analysis) ui.Element {
	k := &a.score.karaoke
	k.sync(a, run)
	t := c.Theme()
	return card(c, tk, "K 歌跟唱", func() {
		ui.Text(c, "播放伴奏时跟着唱，实时显示目标旋律与你唱出的音高；停止后按旋律线评价音准、是否唱满与长音的气息。"+
			"建议戴耳机（外放的伴奏会被录进去，干扰音高检测）。").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
			if k.accomp != "" {
				if ui.Segmented(c, &k.source, "伴奏", "原曲").Label("音源").Disabled(k.singing()).Changed() {
					k.sync(a, run)
				}
			} else {
				ui.Text(c, "没有分离出的伴奏（分析时未分离），播放原曲。").FontSize(12).TextColor(t.TextMuted)
			}
			ui.Box(c).Grow(1)
			ready := k.player() != nil
			if k.src.loading {
				ui.Spinner(c)
			}
			if k.singing() {
				if ui.PrimaryButton(c, "停止并评分").Clicked() {
					a.finish(res)
				}
			} else {
				pos, _ := a.scorePosition()
				if pos > 1 && ui.Button(c, "从试听位置开始").Disabled(!ready).
					Tooltip(fmt.Sprintf("从 %s 开始", score.Clock(pos))).Clicked() {
					a.sing(pos)
				}
				if ui.PrimaryButton(c, "从头开始跟唱").Disabled(!ready).Clicked() {
					a.sing(0)
				}
			}
		})
		if k.src.err != "" {
			ui.Text(c, k.src.err).FontSize(12).TextColor(t.Danger)
		}
		if k.err != "" {
			ui.Text(c, k.err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		if k.singing() {
			// 伴奏播完时自动结束；播放中每 33 ms 重建一次（歌词与实时统计）。
			if p := k.player(); p == nil || !p.Playing() {
				a.finish(res)
			} else {
				c.After(33 * time.Millisecond)
			}
		}
		if k.singing() || k.report == nil {
			a.karaokeLive(c, tk, res)
		}
		if k.report != nil {
			a.karaokeReport(c, tk, res)
		}
	})
}

// karaokeLive 是实时视图：目标旋律音（灰色条）与唱出的音高（按偏差着色的点），以及当前与下一句歌词。
func (a *app) karaokeLive(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	k := &a.score.karaoke
	notes := res.Melody.Notes
	pos := k.start
	if p := k.player(); p != nil && k.singing() {
		pos = p.Position().Seconds()
	}
	var frames []score.KaraokeFrame
	if k.sess != nil {
		frames = k.sess.Frames()
	}
	// 纵轴：整首旋律的音域上下各留 2 个半音。
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, n := range notes {
		lo, hi = min(lo, n.Pitch), max(hi, n.Pitch)
	}
	lo, hi = math.Floor(lo)-2, math.Ceil(hi)+2
	// 实时显示的八度：最近 4 秒内的帧相对目标音的八度差的众数（低八度唱时把点移回原调的位置显示）。
	octave := liveOctave(notes, frames, pos)
	ui.Box(c.Key("karaoke-live")).Height(180).Radius(fluent.ControlRadius).Background(rollPaper.white).
		Border(1, tk.CardStroke).Clip().Draw(func(p *ui.Painter, r ui.Rect) {
		v0, v1 := pos-karaokeBehind, pos+karaokeAhead
		xOf := func(sec float64) float32 { return r.X + float32((sec-v0)/(v1-v0))*r.W }
		yOf := func(m float64) float32 { return r.Y + r.H - float32((m-lo)/(hi-lo))*r.H }
		for m := math.Ceil(lo); m <= hi; m++ {
			if int(m)%12 == 0 { // C 音所在的线
				p.Line(r.X, yOf(m), r.X+r.W, yOf(m), 1, rollPaper.bar)
			}
		}
		for _, n := range notes {
			if n.End < v0 || n.Start > v1 {
				continue
			}
			y := yOf(n.Pitch)
			box := ui.Rect{X: xOf(n.Start), Y: y - 5, W: max(xOf(n.End)-xOf(n.Start)-1, 2), H: 10}
			color := rollPaper.keyEdge
			if pos >= n.Start && pos < n.End {
				color = t.Accent.Alpha(0.6)
			}
			p.Fill(box, color, 3)
		}
		for _, f := range frames {
			if f.T < v0 || f.T > v1 || f.F0 <= 0 || f.Clarity < 0.7 || f.DB < -50 {
				continue
			}
			m := 69 + 12*math.Log2(f.F0/440) - 12*float64(octave)
			if m < lo || m > hi {
				continue
			}
			color := t.Warning
			if n := liveNote(notes, f.T); n != nil && math.Abs(m-n.Pitch) <= 0.5 {
				color = t.Success
			}
			p.Fill(ui.Rect{X: xOf(f.T) - 2, Y: yOf(m) - 2, W: 4, H: 4}, color, 2)
		}
		x := xOf(pos)
		p.Fill(ui.Rect{X: x - 1, Y: r.Y, W: 2, H: r.H}, rollPaper.playhead, 1)
	})
	cur, next := karaokeLines(notes, pos)
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, cur).FontSize(22).FontWeight(600)
		ui.Text(c, next).FontSize(14).TextColor(t.TextMuted)
	})
	if octave != 0 && k.singing() {
		ui.Textf(c, "按%s八度显示你的音高。", map[bool]string{true: "低", false: "高"}[octave < 0]).FontSize(12).TextColor(t.TextMuted)
	}
}

// liveOctave 返回 pos 之前 4 秒内有声帧相对所在旋律音的八度差的众数；帧太少时为 0。
func liveOctave(notes []score.MelodyNote, frames []score.KaraokeFrame, pos float64) int {
	counts := map[int]int{}
	for _, f := range frames {
		if f.T < pos-4 || f.F0 <= 0 || f.Clarity < 0.7 || f.DB < -50 {
			continue
		}
		if n := liveNote(notes, f.T); n != nil {
			counts[int(math.Round((69+12*math.Log2(f.F0/440)-n.Pitch)/12))]++
		}
	}
	best, bestN := 0, 20 // 至少 20 帧（0.2 秒）才换八度
	for o, n := range counts {
		if n > bestN {
			best, bestN = o, n
		}
	}
	return best
}

// liveNote 返回时刻 t 所在（前后放宽 0.15 秒，容纳实时显示时未扣除的延迟）的旋律音。
func liveNote(notes []score.MelodyNote, t float64) *score.MelodyNote {
	for i := range notes {
		if t >= notes[i].Start && t < notes[i].End+0.15 {
			return &notes[i]
		}
	}
	return nil
}

// karaokeLines 返回 pos 时正在唱（或即将唱）的一句与下一句歌词；乐句按停顿 ≥ 0.35 秒划分，没有歌词时显示音名。
func karaokeLines(notes []score.MelodyNote, pos float64) (string, string) {
	var lines []struct {
		end  float64
		text string
	}
	var b strings.Builder
	for i, n := range notes {
		if n.Lyric != "" {
			b.WriteString(n.Lyric)
		}
		if i+1 == len(notes) || notes[i+1].Start-n.End >= 0.35 {
			lines = append(lines, struct {
				end  float64
				text string
			}{n.End, b.String()})
			b.Reset()
		}
	}
	i := slices.IndexFunc(lines, func(l struct {
		end  float64
		text string
	}) bool {
		return l.end > pos
	})
	if i < 0 {
		return "（结束）", ""
	}
	text := func(j int) string {
		if j >= len(lines) {
			return ""
		}
		if lines[j].text == "" {
			return "♪"
		}
		return lines[j].text
	}
	return text(i), text(i + 1)
}

// karaokeReport 显示评价：总分、音准 / 覆盖 / 气息，延迟与八度说明，最需要练习的乐句，以及保存录音。
func (a *app) karaokeReport(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	k := &a.score.karaoke
	r := k.report
	if r.Voiced == 0 {
		return
	}
	ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
		tile := func(title, value, note string) {
			ui.Column(c.Key("ktile-" + title)).Grow(1).Basis(0).Padding(12).Gap(4).Radius(fluent.ControlRadius).
				Background(tk.SubtleHover).Children(func() {
				ui.Text(c, title).FontSize(12).TextColor(t.TextMuted)
				ui.Text(c, value).FontSize(24).FontWeight(600)
				ui.Text(c, note).FontSize(12).TextColor(t.TextMuted)
			})
		}
		tile("总分", fmt.Sprintf("%.0f", r.Score), fmt.Sprintf("%s – %s", score.Clock(r.Start), score.Clock(r.End)))
		bias := "整体音高准"
		switch {
		case r.Bias > 15:
			bias = fmt.Sprintf("整体偏高 %.0f 音分", r.Bias)
		case r.Bias < -15:
			bias = fmt.Sprintf("整体偏低 %.0f 音分", -r.Bias)
		}
		tile("音准", fmt.Sprintf("%.0f%%", r.InTune*100), fmt.Sprintf("±50 音分内；平均偏差 %.0f 音分，%s", r.MeanAbsCents, bias))
		tile("唱满", fmt.Sprintf("%.0f%%", r.Coverage*100), "旋律音的时长中唱出声音的比例")
		breath := "没有 0.6 秒以上的长音"
		if r.LongNotes > 0 {
			breath = fmt.Sprintf("%d 个长音：坚持到尾 %.0f%%，音高抖动 %.0f 音分，响度起伏 %.1f dB",
				r.LongNotes, r.Sustain*100, r.Wobble, r.Steadiness)
		}
		tile("气息", fmt.Sprintf("%.0f", r.Breath*100), breath)
	})
	notes := []string{fmt.Sprintf("已扣除 %.0f ms 的整体延迟（耳机与声卡的延迟加上跟唱的反应）", r.Lag*1000)}
	if r.Octave != 0 {
		notes = append(notes, fmt.Sprintf("你按%s %d 个八度演唱，音准按同一八度计算", map[bool]string{true: "低", false: "高"}[r.Octave < 0], abs(r.Octave)))
	}
	ui.Text(c, strings.Join(notes, "；")+"。").FontSize(12).TextColor(t.TextMuted)

	// 最需要练习的乐句：唱过的、低于 karaokeGoodPhrase 分的乐句中最低的 5 句，可从该句开头重唱。
	ps := slices.Clone(r.Phrases)
	ps = slices.DeleteFunc(ps, func(p score.KaraokePhrase) bool {
		return p.End < r.Start || p.Start > r.End || p.Score >= karaokeGoodPhrase
	})
	slices.SortStableFunc(ps, func(x, y score.KaraokePhrase) int { return int(x.Score - y.Score) })
	if len(ps) > 0 {
		ui.Text(c, "需要多练的乐句").FontSize(14).FontWeight(600)
	}
	for _, p := range ps[:min(5, len(ps))] {
		ui.Row(c.Key(fmt.Sprintf("kphrase-%.2f", p.Start))).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, score.Clock(p.Start)).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(56)
			lyric := p.Lyric
			if lyric == "" {
				lyric = "♪"
			}
			ui.Text(c, lyric).Grow(1).SingleLine()
			ui.Textf(c, "音准 %.0f%% · 唱满 %.0f%%", p.InTune*100, p.Coverage*100).FontSize(12).TextColor(t.TextMuted)
			ui.Textf(c, "%.0f 分", p.Score).FontSize(12).Font("monospace").Width(48)
			if ui.Button(c, "重唱这句").Clicked() {
				a.sing(max(0, p.Start-2)) // 提前 2 秒开始，留出进入的时间
			}
		})
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		if len(k.recording) > 0 && ui.Button(c, "保存录音…").Clicked() {
			a.saveKaraokeRecording()
		}
		if ui.Button(c, "清除结果").Clicked() {
			k.report, k.recording, k.err = nil, nil, ""
		}
	})
}

// saveKaraokeRecording 把上一次演唱的录音写成 16 位单声道 WAV 并弹出另存对话框。
func (a *app) saveKaraokeRecording() {
	k := &a.score.karaoke
	path := filepath.Join(a.be.layout.Scratch, "karaoke.wav")
	if err := os.WriteFile(path, wav16(k.recording, mic.Rate), 0o644); err != nil {
		k.err = err.Error()
		return
	}
	name := "跟唱录音"
	if k.run != nil {
		in := k.run.Request.Input
		name = strings.TrimSuffix(filepath.Base(in), filepath.Ext(in)) + "_跟唱"
	}
	a.saveFileAs(path, name+".wav", func(_ string, err error) { k.err = errText(err) })
}

// wav16 把单声道采样编码为 16 位 PCM WAV（超出 ±1 的采样削波）。
func wav16(samples []float32, rate int) []byte {
	data := make([]byte, 44+2*len(samples))
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+2*len(samples)))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1) // PCM
	binary.LittleEndian.PutUint16(data[22:], 1) // 单声道
	binary.LittleEndian.PutUint32(data[24:], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(2*len(samples)))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(data[44+2*i:], uint16(int16(max(-1, min(1, v))*32767)))
	}
	return data
}
