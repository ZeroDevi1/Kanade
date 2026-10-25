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
	"kanade/internal/settings"
	"kanade/internal/ui/fluent"
)

// K 歌跟唱卡片：开启「K 歌模式」后，页面底部的播放控制条改为播放分离出的伴奏（没有时播放原曲），播放即从麦克风
// 录下演唱，实时显示目标旋律与唱出的音高；暂停或播完时对照旋律线给出音准、覆盖与气息的评价
// （score.AnalyzeKaraoke），并列出最需要练习的乐句。
//
// 生命周期：karaokeFollow 每帧跟随伴奏的播放状态——开始播放时 record 开始录音，暂停、播完或跳转时 finish 结束
// 这一段并评价（跳转后若仍在播放，下一帧从新位置重新录）。关闭 K 歌模式时结束演唱、把试听移到伴奏的位置。
// 换记录、离开页面不会自动停止（与试听一致），切换记录时 sync 发现记录变了会先结束当前演唱。

// karaokeView 是实时视图显示的时间范围（秒）：当前位置之前 karaokeBehind、之后 karaokeAhead。
const (
	karaokeBehind = 3.0
	karaokeAhead  = 5.0
)

// karaokeJumpBack / karaokeJumpAhead 是判定跳转的位置变化（秒）：两次检查之间位置倒退超过前者或前进超过后者
// 视为用户拖动了进度条（经验值：前进留出界面卡顿时正常播放走过的时长）。
const (
	karaokeJumpBack  = 0.3
	karaokeJumpAhead = 1.5
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
	// on 是 K 歌模式开关：开启时页面的播放控制条控制伴奏，播放即跟唱（见 karaokeFollow）。
	on bool
	// accomp 是该记录分离出的伴奏路径（打开记录时查找一次；空为没有）；source 是所选音源。
	accomp string
	source int
	// accompAudio 解码分离出的伴奏（不显示波形）；播放原曲时直接用试听的原文件播放器（audioPreview.original），
	// 不再解码一遍。src 是当前所用的一个，由 sync 设置。
	accompAudio audioPreview
	src         *audioPreview
	// 进行中的演唱：capture 非 nil 时正在录音。start 是开始时的歌曲位置（秒），last 是上一次检查时的位置
	// （用于发现跳转）。
	capture     *mic.Capture
	sess        *score.KaraokeSession
	start, last float64
	// 上一次的评价与录音（16 位 WAV 字节在保存时才生成）。
	report    *score.KaraokeReport
	recording []float32
	err       string
	// mics 是可选的录音设备（mic.Devices），micsAt 是上次开始枚举的时间。枚举较慢（设备多时上百毫秒），
	// 所以在后台进行、至多每 2 秒一次（演唱中不枚举），插拔设备后自动更新；micsBusy 为 true 时正在枚举。
	mics     []string
	micsAt   time.Time
	micsBusy bool
}

// karaokeMicDefault 是录音设备下拉框中「系统默认」一项（对应设置中的空串）。
const karaokeMicDefault = "系统默认"

// micChoice 构建录音设备下拉框，所选设备保存在设置中（Settings.KaraokeMic）。
// 平台不支持选择设备时（mic.Devices 为空）不显示。已保存但当前未连接的设备仍列出并标注，录音时 mic.Start 会报错。
func (a *app) micChoice(c *ui.Context) {
	k := &a.score.karaoke
	if !k.micsBusy && !k.singing() && time.Since(k.micsAt) > 2*time.Second {
		k.micsBusy, k.micsAt = true, time.Now()
		go func() {
			devs := mic.Devices()
			a.post(func() { k.mics, k.micsBusy = devs, false })
		}()
	}
	if len(k.mics) == 0 {
		return
	}
	saved := a.be.settings.KaraokeMic
	options := append([]string{karaokeMicDefault}, k.mics...)
	sel := karaokeMicDefault
	if saved != "" {
		sel = saved
		if !slices.Contains(k.mics, saved) {
			sel = saved + "（未连接）"
			options = append(options, sel)
		}
	}
	if !ui.Select(c, &sel, options).Label("录音设备").Width(220).Disabled(k.singing()).
		Tooltip("跟唱时录音所用的麦克风").Changed() {
		return
	}
	name := sel
	switch {
	case sel == karaokeMicDefault:
		name = ""
	case !slices.Contains(k.mics, sel): // 重新选了未连接的那一项
		return
	}
	next := a.be.settings
	next.KaraokeMic = name
	if err := settings.Save(a.be.userData, next); err != nil {
		k.err = "保存录音设备失败：" + err.Error()
		return
	}
	a.be.settings = next
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

// setKaraoke 打开或关闭 K 歌模式。伴奏与试听是两个播放器：切换时暂停原来的一个，并把新的一个移到它的位置，
// 各视图（都跟随 scoreClock）不会跳动。关闭时先结束进行中的演唱并评价。
func (a *app) setKaraoke(on bool, res *score.Analysis) {
	k := &a.score.karaoke
	from, to := a.score.preview.player, k.player()
	if !on {
		if k.singing() && to != nil {
			a.finish(res, to.Position().Seconds())
		}
		from, to = to, from
	}
	k.on = on
	if from == nil || to == nil || from == to {
		return
	}
	from.Pause()
	to.Seek(from.Position())
}

// sing 从歌曲时间 from（秒）开始跟唱（打开 K 歌模式、播放伴奏，下一帧 karaokeFollow 开始录音）。
func (a *app) sing(from float64, res *score.Analysis) {
	k := &a.score.karaoke
	if !k.on {
		a.setKaraoke(true, res)
	}
	p := k.player()
	if p == nil {
		return
	}
	if k.singing() {
		a.finish(res, p.Position().Seconds())
	}
	p.Seek(time.Duration(from * float64(time.Second)))
	p.Play()
}

// karaokeFollow 在每帧构建 K 歌卡片时跟随伴奏的播放状态：K 歌模式下开始播放时开始录音；暂停、播完或跳转时结束
// 这一段并评价。录音中每 33 ms 重建一次（实时视图与歌词）。
func (a *app) karaokeFollow(c *ui.Context, res *score.Analysis) {
	k := &a.score.karaoke
	p := k.player()
	if !k.on || p == nil {
		return
	}
	pos := p.Position().Seconds()
	switch {
	case k.singing() && !p.Playing():
		a.finish(res, pos)
	case k.singing() && (pos < k.last-karaokeJumpBack || pos > k.last+karaokeJumpAhead):
		a.finish(res, k.last) // 跳转前唱的一段照常评价；仍在播放时下一帧从新位置重新录
	case !k.singing() && p.Playing():
		a.record(pos)
	}
	if k.singing() {
		k.last = pos
		c.After(33 * time.Millisecond)
	}
}

// record 在伴奏已经播放时从歌曲时间 from（秒）开始录音。打不开录音设备时暂停伴奏并显示原因
// （否则下一帧仍在播放，会反复重试）。
func (a *app) record(from float64) {
	k := &a.score.karaoke
	p := k.player()
	sess := score.NewKaraokeSession(mic.Rate)
	// 录音回调在录音线程上：读取播放位置（audio.Player 内部加锁）后交给会话。
	capture, err := mic.Start(a.be.settings.KaraokeMic, func(s []float32) { sess.Push(s, p.Position().Seconds()) })
	if err != nil {
		k.err = err.Error()
		p.Pause()
		return
	}
	k.capture, k.sess, k.start, k.last, k.report, k.recording, k.err = capture, sess, from, from, nil, nil, ""
}

// finish 停止录音（不暂停伴奏）并评价 [start, end] 内的旋律音；end 是歌曲时间（秒）。
func (a *app) finish(res *score.Analysis, end float64) {
	k := &a.score.karaoke
	if !k.singing() {
		return
	}
	k.capture.Stop()
	k.capture = nil
	frames := k.sess.Frames()
	k.recording = k.sess.Recording()
	r := score.AnalyzeKaraoke(res.Melody.Notes, frames, k.start, end)
	k.report, k.sess = &r, nil
	if r.Voiced == 0 {
		k.err = "没有录到演唱：请检查录音设备是否选对、麦克风音量是否足够。"
	}
}

// karaokeCardFunc 返回构建 K 歌卡片的函数（供 cardRow）；没有旋律线时为 nil，不显示卡片。
func karaokeCardFunc(a *app, c *ui.Context, tk *fluent.Tokens, run *score.Run, res *score.Analysis) func() ui.Element {
	if res.Melody == nil || len(res.Melody.Notes) == 0 {
		a.score.karaoke.stop()
		a.score.karaoke.on = false // 没有卡片就没有开关，页面回到试听
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
		ui.Text(c, "开启 K 歌模式后，用页面底部的播放控制条播放伴奏即开始跟唱，实时显示目标旋律与你唱出的音高；"+
			"暂停或播完时按旋律线评价音准、是否唱满与长音的气息。建议戴耳机（外放的伴奏会被录进去，干扰音高检测）。").
			FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
			on := k.on
			if ui.Toggle(c, &on, "K 歌模式").Changed() {
				a.setKaraoke(on, res)
			}
			if k.accomp != "" {
				prev := k.player()
				if ui.Segmented(c, &k.source, "伴奏", "原曲").Label("音源").Disabled(k.singing()).Changed() {
					k.sync(a, run)
					// 模式开启时换音源：停掉原来的音源，新音源接着原来的位置（伴奏解码完之前播放器为 nil，从头开始）。
					if next := k.player(); k.on && prev != nil && next != nil && next != prev {
						prev.Pause()
						next.Seek(prev.Position())
					}
				}
			} else {
				ui.Text(c, "没有分离出的伴奏（分析时未分离），播放原曲。").FontSize(12).TextColor(t.TextMuted)
			}
			a.micChoice(c)
			ui.Box(c).Grow(1)
			if k.src.loading {
				ui.Spinner(c)
				ui.Text(c, "正在载入伴奏…").FontSize(12).TextColor(t.TextMuted)
			} else if k.singing() {
				ui.Text(c, "● 录音中").FontSize(12).TextColor(t.Danger)
			}
		})
		if k.src.err != "" {
			ui.Text(c, k.src.err).FontSize(12).TextColor(t.Danger)
		}
		if k.err != "" {
			ui.Text(c, k.err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		a.karaokeFollow(c, res)
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
				a.sing(max(0, p.Start-2), res) // 提前 2 秒开始，留出进入的时间
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
