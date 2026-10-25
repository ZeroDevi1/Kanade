package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/mic"
	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 灵感页：跟着节拍器哼一段动机，整理成量化好的乐句（音符、转音分组、拟声字），可在卷帘里修改、试听并导出。
// 整理算法见 score.TranscribeHum；后续阶段（和声配置、动机发展）见 docs/inspiration-page-research.md。
//
// 生命周期：startHum 播放节拍器（含一小节预备拍）并录音，每帧检查节拍器是否播完，播完或点「停止」时 finishHum
// 停止录音并整理；之后的编辑都改 hum.Notes，改动后 rebuildInspirePlayer 重新合成试听。只在 UI 线程修改。

// 节拍设置的选项。
var (
	inspireMeters = []int{2, 3, 4}
	inspireBars   = []int{2, 4, 8}
)

// 试听的音源（inspireState.source）。
const (
	inspireSynth = iota // 合成旋律
	inspireVoice        // 原始哼唱
	inspireBoth
)

// inspireRate 是试听的合成采样率：与录音相同，叠加时不必重采样。
const inspireRate = mic.Rate

// inspireState 是灵感页的状态。
type inspireState struct {
	bpm           float64 // 速度（50–200，取整使用）
	meter, bars   int     // inspireMeters / inspireBars 的序号
	filler        int     // score.FillerSets 的序号
	source        int     // inspireSynth 等
	click         bool    // 试听时叠加节拍器
	set           score.HumSettings
	clickPlayer   *audio.Player // 录音时的节拍器；录音的时间轴是它的播放位置
	capture       *mic.Capture  // 非 nil 时正在录音
	sess          *score.KaraokeSession
	recording     []float32 // 上一次的原始录音（mic.Rate），含预备拍
	frames        []score.KaraokeFrame
	hum           *score.Hum // 整理结果；nil 为还没有录过
	sel           int        // 选中的音符序号，-1 为未选
	player        *audio.Player
	playerVersion int // 改动计数：与 builtVersion 不同时重新合成试听
	builtVersion  int
	err           string
}

func newInspireState() inspireState {
	return inspireState{bpm: 90, meter: 2, bars: 1, sel: -1, playerVersion: 1}
}

// settings 返回当前选择的节拍设置（预备拍固定一小节）。
func (s *inspireState) settings() score.HumSettings {
	return score.HumSettings{BPM: math.Round(s.bpm), BeatsPerBar: inspireMeters[s.meter], Bars: inspireBars[s.bars], CountIn: 1}
}

func (s *inspireState) recordingNow() bool { return s.capture != nil }

// changed 标记乐句被修改：重新填拟声字，下一帧重新合成试听。
func (s *inspireState) changed() {
	if s.hum != nil {
		score.FillLyrics(s.hum.Notes, s.filler, s.set.Beat())
	}
	s.playerVersion++
}

// startHum 开始录音：暂停其他页的播放，播放节拍器并以它的位置为录音时间轴。
func (a *app) startHum() {
	s := &a.inspire
	a.pauseAllPlayers()
	s.set = s.settings()
	click := audio.NewSound(inspireRate, 1, score.MetronomePCM(s.set, inspireRate)).NewPlayer()
	sess := score.NewKaraokeSession(mic.Rate)
	capture, err := mic.Start(a.be.settings.KaraokeMic, func(b []float32) { sess.Push(b, click.Position().Seconds()) })
	if err != nil {
		s.err = err.Error()
		return
	}
	s.clickPlayer, s.capture, s.sess, s.err = click, capture, sess, ""
	click.Play()
}

// finishHum 停止录音并整理成乐句。
func (a *app) finishHum() {
	s := &a.inspire
	if !s.recordingNow() {
		return
	}
	s.capture.Stop()
	s.clickPlayer.Stop()
	s.capture, s.clickPlayer = nil, nil
	s.frames, s.recording = s.sess.Frames(), s.sess.Recording()
	s.sess = nil
	h := score.TranscribeHum(s.frames, s.set)
	s.hum, s.sel = &h, -1
	if h.Voiced == 0 || len(h.Notes) == 0 {
		s.err = "没有录到哼唱：请检查录音设备是否选对、麦克风音量是否足够。"
	}
	s.changed()
}

// pauseAllPlayers 暂停各页的试听与 K 歌伴奏（录音时外放会被录进去）。
func (a *app) pauseAllPlayers() {
	for _, p := range []*audio.Player{a.score.preview.player, a.score.karaoke.player(), a.inspire.player} {
		if p != nil {
			p.Pause()
		}
	}
}

// rebuildInspirePlayer 按当前乐句与音源重新合成试听，保持播放位置与播放状态。
func (a *app) rebuildInspirePlayer() {
	s := &a.inspire
	if s.hum == nil || s.builtVersion == s.playerVersion {
		return
	}
	s.builtVersion = s.playerVersion
	length := s.set.Length() + s.set.Beat() // 多留一拍，最后一个音的余音不被截断
	n := int(length * inspireRate)
	pcm := make([]float32, n)
	if s.source != inspireVoice {
		an := s.hum.Analysis()
		copy(pcm, score.RenderPiano(an.MelodyPiano(), inspireRate, length))
	}
	if s.source != inspireSynth {
		// 原始哼唱从预备拍之后开始（录音的第 0 个采样约对应节拍器的起点）。
		if from := int(s.set.Start() * mic.Rate); from < len(s.recording) {
			for i, v := range s.recording[from:min(len(s.recording), from+n)] {
				pcm[i] += v
			}
		}
	}
	if s.click {
		noCount := s.set
		noCount.CountIn = 0
		for i, v := range score.MetronomePCM(noCount, inspireRate)[:min(n, int(s.set.Length()*inspireRate))] {
			pcm[i] += 0.6 * v
		}
	}
	var pos time.Duration
	playing := false
	loop, vol := false, 1.0
	if s.player != nil {
		pos, playing, loop, vol = s.player.Position(), s.player.Playing(), s.player.Loop(), s.player.Volume()
		s.player.Stop()
	}
	s.player = audio.NewSound(inspireRate, 1, pcm).NewPlayer().SetLoop(loop).SetVolume(vol)
	s.player.Seek(pos)
	if playing {
		s.player.Play()
	}
}

// inspirePage 是灵感页。
func (a *app) inspirePage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "灵感", "跟着节拍器哼一段动机，整理成乐句：音符量化到节拍、转音归为一个字、用拟声字填词。")
	s := &a.inspire
	if s.recordingNow() {
		if !s.clickPlayer.Playing() {
			a.finishHum()
		} else {
			c.After(33 * time.Millisecond)
		}
	}
	a.rebuildInspirePlayer()
	cardRow(c, cardMinWidth, func() ui.Element { return a.inspireRecordCard(c, tk) },
		func() ui.Element { return a.inspireInfoCard(c, tk) })
	card(c, tk, "乐句", func() { a.inspirePhrase(c, tk) })
}

// inspireRecordCard 是节拍设置与录音。
func (a *app) inspireRecordCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.inspire
	return card(c, tk, "录动机", func() {
		busy := s.recordingNow()
		labeledSlider(c, "速度（BPM）", &s.bpm, 50, 200, "%.0f").Disabled(busy)
		ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Segmented(c, &s.meter, "2/4", "3/4", "4/4").Label("拍号").Disabled(busy)
			ui.Segmented(c, &s.bars, "2 小节", "4 小节", "8 小节").Label("长度").Disabled(busy)
			a.micChoice(c)
		})
		ui.Text(c, "先响一小节预备拍，然后跟着节拍器哼；建议戴耳机。音节之间带上辅音（「哒」「啦」）更容易分清每个字，"+
			"同一个字里唱几个音（转音）会自动连在一起。").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			if busy {
				if ui.PrimaryButton(c, "停止").Clicked() {
					a.finishHum()
				}
				ui.Text(c, a.humProgress()).FontSize(14).Font("monospace").TextColor(t.Danger)
			} else {
				label := "● 录音"
				if s.hum != nil {
					label = "● 重新录"
				}
				if ui.PrimaryButton(c, label).Clicked() {
					a.startHum()
				}
			}
		})
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(t.Danger).Selectable()
		}
	})
}

// humProgress 返回录音进度：预备拍时倒数，之后为「小节 · 拍」。
func (a *app) humProgress() string {
	s := &a.inspire
	beat := int(s.clickPlayer.Position().Seconds() / s.set.Beat())
	bpb := s.set.BeatsPerBar
	if beat < s.set.CountIn*bpb {
		return fmt.Sprintf("预备 %d", s.set.CountIn*bpb-beat)
	}
	beat -= s.set.CountIn * bpb
	return fmt.Sprintf("录音中 · 第 %d 小节第 %d 拍", min(beat/bpb+1, s.set.Bars), beat%bpb+1)
}

// inspireInfoCard 是整理结果的概况：调、整体音高与节奏偏移、音符与音节数。
func (a *app) inspireInfoCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.inspire
	return card(c, tk, "概况", func() {
		h := s.hum
		if h == nil || len(h.Notes) == 0 {
			ui.Text(c, "录完后在这里显示调、音高与节奏的整体偏移。").TextColor(t.TextMuted)
			return
		}
		syl := 0
		for _, n := range h.Notes {
			if !n.Slur {
				syl++
			}
		}
		key := score.KeyLabel(h.TonicPC, h.Mode, score.KeySignature(h.TonicPC, h.Mode) < 0)
		if !h.KeyClear {
			key += "（不确定，音太少或调性模糊）"
		}
		rows := [][2]string{
			{"调", key},
			{"音符", fmt.Sprintf("%d 个音、%d 个字（%d 个转音）", len(h.Notes), syl, len(h.Notes)-syl)},
			{"音高", tuningText(h.Tuning)},
			{"节奏", shiftText(h.Shift)},
		}
		for _, r := range rows {
			ui.Row(c.Key("info-" + r[0])).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, r[0]).FontSize(12).TextColor(t.TextMuted).Width(48)
				ui.Text(c, r[1]).FontSize(13).Grow(1)
			})
		}
	})
}

func tuningText(cents float64) string {
	switch {
	case cents > 5:
		return fmt.Sprintf("整体偏高 %.0f 音分（已校正）", cents)
	case cents < -5:
		return fmt.Sprintf("整体偏低 %.0f 音分（已校正）", -cents)
	}
	return "音高准"
}

func shiftText(sec float64) string {
	switch {
	case sec > 0.005:
		return fmt.Sprintf("整体拖拍 %.0f ms（已校正）", sec*1000)
	case sec < -0.005:
		return fmt.Sprintf("整体抢拍 %.0f ms（已校正）", -sec*1000)
	}
	return "节奏准"
}

// inspirePhrase 是乐句卡片：卷帘（录音时显示实时音高）、编辑按钮、试听与导出。
func (a *app) inspirePhrase(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	if s.hum == nil && !s.recordingNow() {
		ui.Text(c, "还没有录音。").TextColor(t.TextMuted)
		return
	}
	a.inspireRoll(c, tk)
	if s.recordingNow() || s.hum == nil {
		return
	}
	a.inspireEditBar(c)
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &s.source, "合成旋律", "原始哼唱", "叠加").Label("试听音源").Changed() {
			s.playerVersion++
		}
		if ui.Checkbox(c, &s.click, "节拍器").Changed() {
			s.playerVersion++
		}
		fillers := make([]string, len(score.FillerSets))
		for i, f := range score.FillerSets {
			fillers[i] = f.Title
		}
		if ui.Segmented(c, &s.filler, fillers...).Label("拟声字").Changed() {
			s.changed()
		}
		ui.Box(c).Grow(1)
		a.inspireExports(c)
	})
	if s.player != nil {
		transport(c, tk, "inspire", s.player)
	}
}

// inspireRoll 绘制卷帘：小节、拍与十六分网格，量化后的音符（标出拟声字，转音与前一音相连），录到的音高曲线，
// 以及试听的播放头。点击音符选中它，点击空白处取消选择并跳转。
func (a *app) inspireRoll(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	set := s.set
	if s.recordingNow() {
		s.frames = s.sess.Frames()
	}
	var notes []score.MelodyNote
	tuning := 0.0
	if s.hum != nil && !s.recordingNow() {
		notes, tuning = s.hum.Notes, s.hum.Tuning
	}
	// 纵轴：音符（没有时取录到的音高）的音域上下各留 3 个半音，至少一个八度半。
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, n := range notes {
		lo, hi = min(lo, n.Pitch), max(hi, n.Pitch)
	}
	for _, f := range s.frames {
		if f.F0 > 0 && f.Clarity > 0.7 {
			m := 69 + 12*math.Log2(f.F0/440)
			lo, hi = min(lo, m), max(hi, m)
		}
	}
	if math.IsInf(lo, 1) {
		lo, hi = 60, 72
	}
	lo, hi = math.Floor(lo)-3, math.Ceil(hi)+3
	if hi-lo < 18 {
		lo -= math.Floor((18 - (hi - lo)) / 2)
		hi = lo + 18
	}
	length := set.Length()
	box := ui.Box(c.Key("inspire-roll")).Height(260).Radius(fluent.ControlRadius).Background(rollPaper.white).
		Border(1, tk.CardStroke).Clip().Cursor(ui.CursorPointer).Label("乐句卷帘")
	const keysW = 40
	b := box.Bounds() // 上一帧的尺寸，用于把点击位置换成时间与音高
	rowH := b.H / float32(hi-lo+1)
	if box.Clicked() && b.W > keysW && !s.recordingNow() {
		x, y, _ := box.PointerPosition()
		sec := float64((x-keysW)/(b.W-keysW)) * length
		m := hi - float64(y/rowH) + 0.5
		s.sel = -1
		for i, n := range notes {
			if sec >= n.Start && sec < n.End && math.Abs(n.Pitch-m) < 1 {
				s.sel = i
			}
		}
		if s.sel < 0 && s.player != nil {
			s.player.Seek(time.Duration(max(sec, 0) * float64(time.Second)))
		}
	}
	accent := t.Accent
	sel := s.sel
	frames := s.frames
	player := s.player
	clickPlayer := s.clickPlayer
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		xOf := func(sec float64) float32 { return r.X + keysW + float32(sec/length)*(r.W-keysW) }
		rowH := r.H / float32(hi-lo+1)
		yOf := func(m float64) float32 { return r.Y + float32(hi-m)*rowH }
		// 行：黑键行深色，每个 C 标出音名。
		for m := int(lo); m <= int(hi); m++ {
			y := yOf(float64(m)) - rowH/2
			if slices.Contains([]int{1, 3, 6, 8, 10}, m%12) {
				p.Fill(ui.Rect{X: r.X + keysW, Y: y, W: r.W - keysW, H: rowH}, rollPaper.black, 0)
			}
			if m%12 == 0 {
				p.Line(r.X, y+rowH, r.X+r.W, y+rowH, 1, rollPaper.cLine)
				p.Text(r.X+4, y+(rowH-10)/2, midiNoteName(m), 10, rollPaper.keyText)
			}
		}
		// 网格：十六分、拍、小节。
		g := set.Beat() / 4
		for k := 0; float64(k)*g <= length+1e-9; k++ {
			color, w := rollPaper.rowLine, float32(1)
			switch {
			case k%(4*set.BeatsPerBar) == 0:
				color = rollPaper.bar
			case k%4 == 0:
				color = rollPaper.beat
			}
			x := xOf(float64(k) * g)
			p.Line(x, r.Y, x, r.Y+r.H, w, color)
		}
		// 录到的音高（扣除预备拍与整体偏移），淡色细点。
		for _, f := range frames {
			if f.F0 <= 0 || f.Clarity < 0.7 {
				continue
			}
			sec := f.T - set.Start()
			if sec < 0 || sec > length {
				continue
			}
			m := 69 + 12*math.Log2(f.F0/440) - tuning/100
			p.Fill(ui.Rect{X: xOf(sec) - 1, Y: yOf(m) - 1, W: 2, H: 2}, ui.RGBA(60, 60, 60, 0.45), 1)
		}
		// 音符：转音与前一音之间画连线；选中的音加深描边；拟声字写在音符上方。
		for i, n := range notes {
			x0, x1 := xOf(n.Start), xOf(n.End)
			y := yOf(n.Pitch) - rowH/2
			if n.Slur && i > 0 {
				pn := notes[i-1]
				p.Line(xOf(pn.End)-2, yOf(pn.Pitch), x0+2, yOf(n.Pitch), 2, accent)
			}
			rc := ui.Rect{X: x0 + 1, Y: y + 1, W: max(x1-x0-2, 2), H: max(rowH-2, 3)}
			p.Fill(rc, accent, 3)
			if i == sel {
				p.Stroke(rc, ui.RGB(20, 20, 20), 3, 2)
			}
			if n.Lyric != "" {
				p.Text(x0+2, y-14, n.Lyric, 12, ui.RGB(30, 30, 30))
			}
		}
		// 播放头：录音时为节拍器的位置，否则为试听的位置。
		pos, playing := -1.0, false
		switch {
		case clickPlayer != nil:
			pos, playing = clickPlayer.Position().Seconds()-set.Start(), true
		case player != nil:
			pos, playing = player.Position().Seconds(), player.Playing()
		}
		if pos >= 0 && pos <= length {
			x := xOf(pos)
			p.Line(x, r.Y, x, r.Y+r.H, 2, rollPaper.playhead)
		}
		if playing {
			p.AnimationFrame()
		}
	})
}

// inspireEditBar 是选中音符的编辑按钮：升降半音、左右移动与伸缩（十六分音符）、与前一音连唱、删除。
func (a *app) inspireEditBar(c *ui.Context) {
	t := c.Theme()
	s := &a.inspire
	h := s.hum
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		if s.sel < 0 || s.sel >= len(h.Notes) {
			ui.Text(c, "点击音符可以修改：升降半音、移动、伸缩、与前一个音连唱（转音）或删除。").FontSize(12).TextColor(t.TextMuted)
			return
		}
		i := s.sel
		n := &h.Notes[i]
		g := s.set.Beat() / 4
		prevEnd, nextStart := 0.0, s.set.Length()
		if i > 0 {
			prevEnd = h.Notes[i-1].End
		}
		if i+1 < len(h.Notes) {
			nextStart = h.Notes[i+1].Start
		}
		ui.Textf(c, "第 %d 个音 · %s", i+1, midiNoteName(int(n.Pitch))).FontSize(12).Width(110)
		edit := func(label string, ok bool, fn func()) {
			if ui.Button(c, label).Disabled(!ok).Clicked() {
				fn()
				s.changed()
			}
		}
		edit("升半音", n.Pitch < 96, func() { n.Pitch++ })
		edit("降半音", n.Pitch > 24, func() { n.Pitch-- })
		edit("左移", n.Start-g >= prevEnd-1e-9, func() { n.Start -= g; n.End -= g })
		edit("右移", n.End+g <= nextStart+1e-9, func() { n.Start += g; n.End += g })
		edit("缩短", n.End-n.Start > g+1e-9, func() { n.End -= g })
		edit("延长", n.End+g <= nextStart+1e-9, func() { n.End += g })
		slurLabel := "与前一音连唱"
		if n.Slur {
			slurLabel = "拆成新的字"
		}
		edit(slurLabel, i > 0, func() { n.Slur = !n.Slur })
		edit("删除", true, func() {
			h.Notes = slices.Delete(h.Notes, i, i+1)
			if i < len(h.Notes) && h.Notes[i].Slur && (i == 0 || h.Notes[i-1].End < h.Notes[i].Start-1e-9) {
				h.Notes[i].Slur = false // 删掉的是音节的第一个音：后面的连音成为新的字
			}
			s.sel = -1
		})
	})
}

// inspireExports 是导出按钮：MIDI、OpenUtau 工程（带拟声字，可直接合成歌声）与原始录音。
func (a *app) inspireExports(c *ui.Context) {
	s := &a.inspire
	name := fmt.Sprintf("动机_%s", time.Now().Format("0102_1504"))
	save := func(label, file string, data func() ([]byte, error)) {
		if !ui.Button(c, label).Clicked() {
			return
		}
		b, err := data()
		if err != nil {
			s.err = err.Error()
			return
		}
		p := filepath.Join(a.be.layout.Scratch, file)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			s.err = err.Error()
			return
		}
		a.saveFileAs(p, name, func(_ string, err error) { s.err = errText(err) })
	}
	save("导出 MIDI…", "motif.mid", func() ([]byte, error) { return s.hum.Analysis().MIDI() })
	save("导出 OpenUtau 工程…", "motif.ustx", func() ([]byte, error) { return s.hum.Analysis().USTX(name) })
	save("保存录音…", "motif.wav", func() ([]byte, error) { return wav16(s.recording, mic.Rate), nil })
}
