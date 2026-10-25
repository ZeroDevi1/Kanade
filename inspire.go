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

// 灵感页：哼一段动机（自由哼唱自动识别速度，或跟着节拍器），整理成量化好的乐句（音符、转音分组、拟声字），
// 自动配几套不同风格的和声；可在卷帘里修改、试听并导出。整理算法见 score.TranscribeHum，和声见 score.Harmonize；
// 后续阶段（动机发展）见 docs/inspiration-page-research.md。
//
// 生命周期：startHum 开始录音（节拍器模式同时播放节拍器），录音中每帧检查是否该结束（节拍器播完或自由模式到达
// 最长时长），结束或点「停止」时 finishHum 停止录音，transcribe 整理并配和声。速度、拍号、小节线的调整会用同一段
// 录音重新整理（覆盖手动修改）；对音符的编辑改 hum.Notes 后重填拟声字、重新配和声。改动后 rebuildInspirePlayer 在
// 下一帧重新合成试听。只在 UI 线程修改。

// 节拍器模式的选项。
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

const (
	// inspireRate 是试听的合成采样率：与录音相同，叠加时不必重采样。
	inspireRate = mic.Rate
	// inspireMaxFree 是自由哼唱的最长录音时长：到时自动停止（足够哼一个 8 小节的乐句）。
	inspireMaxFree = 60 * time.Second
	// inspireHarmonies 是每种风格最多给出的和声方案数。
	inspireHarmonies = 3
)

// inspireState 是灵感页的状态。
type inspireState struct {
	// 录音设置：free 为自由哼唱（不用节拍器）；节拍器模式用 bpm、meter、bars。
	free        bool
	bpm         float64 // 节拍器速度（50–200，取整使用）
	meter, bars int     // inspireMeters / inspireBars 的序号
	// 自由模式对自动整理结果的调整：tempoHint 为速度提示（0 为自动），meterPick 为拍号（0 自动，1–3 对应 2/3/4 拍），
	// barShift 为小节线的手动挪动（拍，见 score.HumSettings.BarShift）。
	tempoHint float64
	meterPick int
	barShift  int
	// 录音：capture 非 nil 时正在录音。recSet 是录音时的节拍设置；clickPlayer 是节拍器模式的节拍器（录音的时间轴是
	// 它的播放位置），自由模式为 nil（时间轴是已录到的时长）；recStart 是开始录音的时刻（只用于显示与自动停止）。
	recSet      score.HumSettings
	clickPlayer *audio.Player
	capture     *mic.Capture
	sess        *score.KaraokeSession
	recStart    time.Time
	recording   []float32 // 上一次的原始录音（mic.Rate）
	frames      []score.KaraokeFrame
	// 整理结果：hum 为 nil 时还没有录过；sel 是选中的音符（-1 为未选）。
	hum    *score.Hum
	sel    int
	filler int // score.FillerSets 的序号
	// 和声：style 为 score.Style*；harmonies 是当前风格的方案，pick 为选中的方案，chordSel 为选中查看理由的和弦（-1 为未选）。
	style     int
	harmonies []score.Harmony
	pick      int
	chordSel  int
	// 试听：source 为音源，click 叠加节拍器，chords 叠加和弦铺底。播放器的时间轴：合成旋律时为乐谱时间；含原始哼唱
	// 时为录音时间减去 r0（乐谱第一拍对应的录音时刻），voiceTime 标出这一点。
	source        int
	click, chords bool
	player        *audio.Player
	voiceTime     bool
	r0            float64
	playerVersion int // 改动计数：与 builtVersion 不同时重新合成试听
	builtVersion  int
	err           string
}

func newInspireState() inspireState {
	return inspireState{free: true, bpm: 90, meter: 2, bars: 1, sel: -1, chordSel: -1, chords: true, playerVersion: 1}
}

// metronomeSettings 返回节拍器模式的节拍设置（预备拍固定一小节）。
func (s *inspireState) metronomeSettings() score.HumSettings {
	return score.HumSettings{BPM: math.Round(s.bpm), BeatsPerBar: inspireMeters[s.meter], Bars: inspireBars[s.bars], CountIn: 1}
}

// transcribeSettings 返回整理用的节拍设置：节拍器模式为录音时的设置，自由模式带上用户的调整。
func (s *inspireState) transcribeSettings() score.HumSettings {
	if !s.recSet.Free {
		return s.recSet
	}
	meter := 0
	if s.meterPick > 0 {
		meter = inspireMeters[s.meterPick-1]
	}
	return score.HumSettings{Free: true, BPM: s.tempoHint, BeatsPerBar: meter, BarShift: s.barShift}
}

func (s *inspireState) recordingNow() bool { return s.capture != nil }

// set 返回当前乐句的节拍设置（整理后由 TranscribeHum 写回估计的速度、拍号与小节数）。
func (s *inspireState) set() score.HumSettings { return s.hum.Settings }

// analysis 返回乐句（含选中的和声方案）的分析结果，用于合成与导出。
func (s *inspireState) analysis() *score.Analysis {
	an := s.hum.Analysis()
	if s.pick < len(s.harmonies) {
		an.Chords = s.harmonies[s.pick].Chords
	}
	return an
}

// changed 标记乐句被修改：重新填拟声字、重新配和声，下一帧重新合成试听。
func (s *inspireState) changed() {
	if s.hum != nil {
		score.FillLyrics(s.hum.Notes, s.filler, s.set().Beat())
		s.harmonize()
	}
	s.playerVersion++
}

// harmonize 按当前风格重新配和声；方案数变少时选中的方案退回第一个。
func (s *inspireState) harmonize() {
	s.harmonies = score.Harmonize(*s.hum, s.style, inspireHarmonies)
	if s.pick >= len(s.harmonies) {
		s.pick = 0
	}
	s.chordSel = -1
	s.playerVersion++
}

// startHum 开始录音：暂停其他页的播放；节拍器模式播放节拍器并以它的位置为录音时间轴，自由模式以已录到的时长为时间轴。
func (a *app) startHum() {
	s := &a.inspire
	a.pauseAllPlayers()
	sess := score.NewKaraokeSession(mic.Rate)
	var click *audio.Player
	var onSamples func([]float32)
	if s.free {
		s.recSet = score.HumSettings{Free: true}
		// 录音回调只在录音线程上调用：elapsed 只由它读写。
		elapsed := 0.0
		onSamples = func(b []float32) {
			elapsed += float64(len(b)) / mic.Rate
			sess.Push(b, elapsed)
		}
	} else {
		s.recSet = s.metronomeSettings()
		click = audio.NewSound(inspireRate, 1, score.MetronomePCM(s.recSet, inspireRate)).NewPlayer()
		onSamples = func(b []float32) { sess.Push(b, click.Position().Seconds()) }
	}
	capture, err := mic.Start(a.be.settings.KaraokeMic, onSamples)
	if err != nil {
		s.err = err.Error()
		return
	}
	s.clickPlayer, s.capture, s.sess, s.recStart, s.err = click, capture, sess, time.Now(), ""
	s.tempoHint, s.meterPick, s.barShift = 0, 0, 0
	if click != nil {
		click.Play()
	}
}

// finishHum 停止录音并整理成乐句。
func (a *app) finishHum() {
	s := &a.inspire
	if !s.recordingNow() {
		return
	}
	s.capture.Stop()
	if s.clickPlayer != nil {
		s.clickPlayer.Stop()
	}
	s.capture, s.clickPlayer = nil, nil
	s.frames, s.recording = s.sess.Frames(), s.sess.Recording()
	s.sess = nil
	a.transcribe()
}

// transcribe 用录到的帧（重新）整理乐句并配和声；手动修改被覆盖。
func (a *app) transcribe() {
	s := &a.inspire
	h := score.TranscribeHum(s.frames, s.transcribeSettings())
	s.hum, s.sel, s.err = &h, -1, ""
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

// rebuildInspirePlayer 按当前乐句、和声与音源重新合成试听，保持播放位置（按乐谱时间）与播放状态。
// 含原始哼唱时以录音时间为轴：合成的音按跟踪出的拍点放到录音时间上，与哼唱对齐（哼唱不做时间拉伸）。
func (a *app) rebuildInspirePlayer() {
	s := &a.inspire
	if s.hum == nil || s.builtVersion == s.playerVersion {
		return
	}
	s.builtVersion = s.playerVersion
	h := *s.hum
	set := h.Settings
	beat := set.Beat()
	voice := s.source != inspireSynth
	r0 := 0.0
	toPlayer := func(tau float64) float64 { return tau }
	if voice {
		r0 = h.RecTime(0)
		toPlayer = func(tau float64) float64 { return h.RecTime(tau) - r0 }
	}
	length := toPlayer(set.Length()) + beat // 多留一拍，最后一个音的余音不被截断
	n := int(length * inspireRate)
	pcm := make([]float32, n)
	// 合成的旋律（只听原始哼唱时不要）与和弦铺底，时间换到播放器时间轴。
	an := s.analysis()
	var notes []score.PianoNote
	if s.source != inspireVoice {
		notes = append(notes, an.MelodyPiano()...)
	}
	if s.chords {
		notes = append(notes, an.ChordPad()...)
	}
	for i := range notes {
		notes[i].Start, notes[i].End = toPlayer(notes[i].Start), toPlayer(notes[i].End)
	}
	if len(notes) > 0 {
		for i, v := range score.RenderPiano(notes, inspireRate, length) {
			pcm[i] += v
		}
	}
	if voice {
		// 原始哼唱：播放器时刻 x 对应录音时刻 r0 + x（r0 为负时开头补静音）。
		off := int(math.Round(r0 * mic.Rate))
		for i := range pcm {
			if j := off + i; j >= 0 && j < len(s.recording) {
				pcm[i] += s.recording[j]
			}
		}
	}
	if s.click {
		for k := 0; float64(k)*beat < set.Length()+1e-9; k++ {
			score.AddClick(pcm, inspireRate, toPlayer(float64(k)*beat), k%set.BeatsPerBar == 0)
		}
	}
	var pos time.Duration
	playing := false
	loop, vol := false, 1.0
	if s.player != nil {
		tau := s.scoreTime(s.player.Position().Seconds())
		pos = time.Duration(max(toPlayer(tau), 0) * float64(time.Second))
		playing, loop, vol = s.player.Playing(), s.player.Loop(), s.player.Volume()
		s.player.Stop()
	}
	s.player = audio.NewSound(inspireRate, 1, pcm).NewPlayer().SetLoop(loop).SetVolume(vol)
	s.voiceTime, s.r0 = voice, r0
	s.player.Seek(pos)
	if playing {
		s.player.Play()
	}
}

// scoreTime 把试听播放器的位置（秒）换成乐谱时间。
func (s *inspireState) scoreTime(x float64) float64 {
	if !s.voiceTime || s.hum == nil {
		return x
	}
	return s.hum.ScoreTime(x + s.r0)
}

// seekScore 把试听跳到乐谱时间 tau。
func (s *inspireState) seekScore(tau float64) {
	if s.player == nil {
		return
	}
	x := tau
	if s.voiceTime {
		x = s.hum.RecTime(tau) - s.r0
	}
	s.player.Seek(time.Duration(max(x, 0) * float64(time.Second)))
}

// inspirePage 是灵感页。
func (a *app) inspirePage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "灵感", "哼一段动机，整理成乐句（音符量化到节拍、转音归为一个字、拟声字填词），再自动配和声。")
	s := &a.inspire
	if s.recordingNow() {
		if (s.clickPlayer != nil && !s.clickPlayer.Playing()) || (s.clickPlayer == nil && time.Since(s.recStart) >= inspireMaxFree) {
			a.finishHum()
		} else {
			c.After(33 * time.Millisecond)
		}
	}
	a.rebuildInspirePlayer()
	cardRow(c, cardMinWidth, func() ui.Element { return a.inspireRecordCard(c, tk) },
		func() ui.Element { return a.inspireInfoCard(c, tk) })
	card(c, tk, "乐句", func() { a.inspirePhrase(c, tk) })
	if s.hum != nil && len(s.hum.Notes) > 0 && !s.recordingNow() {
		card(c, tk, "和声", func() { a.inspireHarmony(c, tk) })
	}
}

// inspireRecordCard 是录音模式、节拍器设置与录音按钮。
func (a *app) inspireRecordCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.inspire
	return card(c, tk, "录动机", func() {
		busy := s.recordingNow()
		mode := 1
		if s.free {
			mode = 0
		}
		ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
			if ui.Segmented(c, &mode, "自由哼唱", "跟节拍器").Label("录音方式").Disabled(busy).Changed() {
				s.free = mode == 0
			}
			a.micChoice(c)
		})
		if s.free {
			ui.Text(c, "按自己的速度哼，速度、拍号和小节线自动识别，哼唱中的渐快渐慢也会跟上；识别得不对可以在「节奏」里调整。"+
				"音节之间带上辅音（「哒」「啦」）更容易分清每个字。建议戴耳机。").FontSize(12).TextColor(t.TextMuted)
		} else {
			labeledSlider(c, "速度（BPM）", &s.bpm, 50, 200, "%.0f").Disabled(busy)
			ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Segmented(c, &s.meter, "2/4", "3/4", "4/4").Label("拍号").Disabled(busy)
				ui.Segmented(c, &s.bars, "2 小节", "4 小节", "8 小节").Label("长度").Disabled(busy)
			})
			ui.Text(c, "先响一小节预备拍，然后跟着节拍器哼；整体抢拍、拖拍会自动校正。建议戴耳机。").FontSize(12).TextColor(t.TextMuted)
		}
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			if busy {
				// 进度须在「停止」之前取：finishHum 会释放节拍器播放器。
				progress := a.humProgress()
				if ui.PrimaryButton(c, "停止").Clicked() {
					a.finishHum()
				}
				ui.Text(c, progress).FontSize(14).Font("monospace").TextColor(t.Danger)
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

// humProgress 返回录音进度：节拍器模式预备拍时倒数、之后为「小节 · 拍」；自由模式为已录时长。
func (a *app) humProgress() string {
	s := &a.inspire
	if s.clickPlayer == nil {
		return fmt.Sprintf("录音中 · %.1f 秒（最长 %.0f 秒）", time.Since(s.recStart).Seconds(), inspireMaxFree.Seconds())
	}
	set := s.recSet
	beat := int(s.clickPlayer.Position().Seconds() / set.Beat())
	bpb := set.BeatsPerBar
	if beat < set.CountIn*bpb {
		return fmt.Sprintf("预备 %d", set.CountIn*bpb-beat)
	}
	beat -= set.CountIn * bpb
	return fmt.Sprintf("录音中 · 第 %d 小节第 %d 拍", min(beat/bpb+1, set.Bars), beat%bpb+1)
}

// inspireInfoCard 是整理结果的概况与节奏调整：调、速度与拍号、音符与音节数、整体偏移。
func (a *app) inspireInfoCard(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.inspire
	return card(c, tk, "概况", func() {
		h := s.hum
		if h == nil || len(h.Notes) == 0 {
			ui.Text(c, "录完后在这里显示调、速度、拍号与整体偏移。").TextColor(t.TextMuted)
			return
		}
		set := h.Settings
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
		tempo := fmt.Sprintf("♩ = %.0f · %d/4 · %d 小节", set.BPM, set.BeatsPerBar, set.Bars)
		if set.Free && s.tempoHint == 0 && s.meterPick == 0 {
			tempo += "（自动识别）"
		}
		rows := [][2]string{
			{"调", key},
			{"节奏", tempo},
			{"音符", fmt.Sprintf("%d 个音、%d 个字（%d 个转音）", len(h.Notes), syl, len(h.Notes)-syl)},
			{"音高", tuningText(h.Tuning)},
		}
		if !set.Free {
			rows = append(rows, [2]string{"拍点", shiftText(h.RecTime(0) - set.Start())})
		}
		for _, r := range rows {
			ui.Row(c.Key("info-" + r[0])).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, r[0]).FontSize(12).TextColor(t.TextMuted).Width(48)
				ui.Text(c, r[1]).FontSize(13).Grow(1)
			})
		}
		if set.Free {
			a.inspireRhythmFix(c)
		}
	})
}

// inspireRhythmFix 是自由模式下对自动识别的节奏的调整：速度减半 / 加倍、拍号、挪动小节线。调整后用同一段录音重新整理。
func (a *app) inspireRhythmFix(c *ui.Context) {
	t := c.Theme()
	s := &a.inspire
	set := s.hum.Settings
	redo := func() {
		a.transcribe()
	}
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		if ui.Button(c, "速度减半").Tooltip("识别成了两倍速时用：每拍变长").Disabled(set.BPM/2 < 30).Clicked() {
			s.tempoHint = set.BPM / 2
			redo()
		}
		if ui.Button(c, "速度加倍").Tooltip("识别成了一半速度时用：每拍变短").Disabled(set.BPM*2 > 240).Clicked() {
			s.tempoHint = set.BPM * 2
			redo()
		}
		if s.tempoHint != 0 && ui.Button(c, "自动速度").Clicked() {
			s.tempoHint = 0
			redo()
		}
		if ui.Segmented(c, &s.meterPick, "自动", "2/4", "3/4", "4/4").Label("拍号").Changed() {
			redo()
		}
		if ui.Button(c, "小节线左移").Tooltip("把小节线往前挪一拍").Clicked() {
			s.barShift--
			redo()
		}
		if ui.Button(c, "小节线右移").Tooltip("把小节线往后挪一拍（第一个音变成弱起）").Clicked() {
			s.barShift++
			redo()
		}
	})
	ui.Text(c, "调整后会用同一段录音重新整理，乐句里的手动修改会被覆盖。").FontSize(12).TextColor(t.TextMuted)
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
	return "跟拍准确"
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
		if ui.Checkbox(c, &s.chords, "和弦").Changed() {
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

// inspireRoll 绘制卷帘：小节、拍与十六分网格，所选和声的和弦名，量化后的音符（标出拟声字，转音与前一音相连），
// 录到的音高曲线，以及播放头。录音中横轴为录音时间（节拍器模式扣除预备拍）；整理后为乐谱时间。点击音符选中它，
// 点击空白处取消选择并跳转。
func (a *app) inspireRoll(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	recording := s.recordingNow()
	if recording {
		s.frames = s.sess.Frames()
	}
	// 横轴：录音中为录音时间（toX 把帧时间换成横轴时间），整理后为乐谱时间。
	var (
		notes  []score.MelodyNote
		chords []score.Segment
		length float64
		beat   float64
		bpb    int
		toX    func(float64) float64
		tuning float64
	)
	switch {
	case recording && s.clickPlayer == nil:
		elapsed := time.Since(s.recStart).Seconds()
		length, toX = max(8, math.Ceil(elapsed/4+0.5)*4), func(t float64) float64 { return t }
	case recording:
		set := s.recSet
		length, beat, bpb = set.Length(), set.Beat(), set.BeatsPerBar
		toX = func(t float64) float64 { return t - set.Start() }
	default:
		h := *s.hum
		set := h.Settings
		notes, tuning = h.Notes, h.Tuning
		length, beat, bpb = set.Length(), set.Beat(), set.BeatsPerBar
		toX = h.ScoreTime
		if s.pick < len(s.harmonies) {
			chords = s.harmonies[s.pick].Chords
		}
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
	const keysW, chordH = 40, 18
	box := ui.Box(c.Key("inspire-roll")).Height(280).Radius(fluent.ControlRadius).Background(rollPaper.white).
		Border(1, tk.CardStroke).Clip().Cursor(ui.CursorPointer).Label("乐句卷帘")
	b := box.Bounds() // 上一帧的尺寸，用于把点击位置换成时间与音高
	rowH := (b.H - chordH) / float32(hi-lo+1)
	if box.Clicked() && b.W > keysW && !recording {
		x, y, _ := box.PointerPosition()
		sec := float64((x-keysW)/(b.W-keysW)) * length
		m := hi - float64((y-chordH)/rowH) + 0.5
		s.sel = -1
		for i, n := range notes {
			if sec >= n.Start && sec < n.End && math.Abs(n.Pitch-m) < 1 {
				s.sel = i
			}
		}
		if s.sel < 0 {
			s.seekScore(max(sec, 0))
		}
	}
	accent := t.Accent
	sel, frames, player, clickPlayer := s.sel, s.frames, s.player, s.clickPlayer
	recStart := s.recStart
	tonic := 0
	if s.hum != nil {
		tonic = s.hum.TonicPC
	}
	scoreTime := s.scoreTime
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		xOf := func(sec float64) float32 { return r.X + keysW + float32(sec/length)*(r.W-keysW) }
		top := r.Y + chordH
		rowH := (r.H - chordH) / float32(hi-lo+1)
		yOf := func(m float64) float32 { return top + float32(hi-m)*rowH }
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
		// 网格：有节拍时画十六分、拍与小节线；自由录音中只画秒线。
		if beat > 0 {
			g := beat / 4
			for k := 0; float64(k)*g <= length+1e-9; k++ {
				color := rollPaper.rowLine
				switch {
				case k%(4*bpb) == 0:
					color = rollPaper.bar
				case k%4 == 0:
					color = rollPaper.beat
				}
				x := xOf(float64(k) * g)
				p.Line(x, top, x, r.Y+r.H, 1, color)
			}
		} else {
			for sec := 0.0; sec <= length; sec++ {
				p.Line(xOf(sec), top, xOf(sec), r.Y+r.H, 1, rollPaper.beat)
			}
		}
		// 和弦名（爵士写法）写在顶部一行。
		p.Fill(ui.Rect{X: r.X + keysW, Y: r.Y, W: r.W - keysW, H: chordH}, rollPaper.ruler, 0)
		for _, ch := range chords {
			p.Text(xOf(ch.Start)+3, r.Y+3, score.Symbol(ch, score.SymbolJazz, tonic), 11, rollPaper.rulerText)
		}
		// 录到的音高（扣除整体偏移），淡色细点。
		for _, f := range frames {
			if f.F0 <= 0 || f.Clarity < 0.7 {
				continue
			}
			sec := toX(f.T)
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
		// 播放头：录音时为录音位置，否则为试听位置（换成乐谱时间）。
		pos, playing := -1.0, false
		switch {
		case clickPlayer != nil:
			pos, playing = toX(clickPlayer.Position().Seconds()), true
		case recording:
			pos, playing = time.Since(recStart).Seconds(), true
		case player != nil:
			pos, playing = scoreTime(player.Position().Seconds()), player.Playing()
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
		g := s.set().Beat() / 4
		prevEnd, nextStart := 0.0, s.set().Length()
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

// inspireHarmony 是和声卡片：风格、方案（附评分）、和弦序列与选中和弦的理由。
func (a *app) inspireHarmony(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &s.style, score.HarmonyStyles...).Label("和声风格").Changed() {
			s.pick = 0
			s.harmonize()
		}
		for i, h := range s.harmonies {
			b := ui.Button(c.Key(fmt.Sprintf("harm-%d", i)), fmt.Sprintf("方案 %d · %.0f 分", i+1, h.Total)).
				Tooltip(fmt.Sprintf("旋律匹配 %.0f · 进行流畅 %.0f", h.Melody, h.Prog))
			if i == s.pick {
				b = b.Disabled(true)
			}
			if b.Clicked() {
				s.pick, s.chordSel = i, -1
				s.playerVersion++
			}
		}
	})
	if len(s.harmonies) == 0 {
		ui.Text(c, "这段旋律配不出和声（音太少）。").FontSize(12).TextColor(t.TextMuted)
		return
	}
	h := s.harmonies[s.pick]
	tonic := s.hum.TonicPC
	ui.Row(c).Wrap().Gap(6).AlignItems(ui.Center).Children(func() {
		for i, ch := range h.Chords {
			b := ui.ButtonBase(c.Key(fmt.Sprintf("chord-%d", i))).Padding(4, 10).Radius(fluent.ControlRadius).
				Border(1, tk.CardStroke).Label(ch.Label)
			switch {
			case i == s.chordSel:
				b.Background(tk.SubtlePressed).Border(1, t.Accent)
			case b.Hovered():
				b.Background(tk.SubtleHover)
			}
			b.Children(func() {
				ui.Text(c, score.Symbol(ch, score.SymbolJazz, tonic)).FontSize(14).FontWeight(600)
			})
			if b.Clicked() {
				s.chordSel = i
				s.seekScore(ch.Start)
			}
		}
	})
	if s.chordSel >= 0 && s.chordSel < len(h.Reasons) {
		ui.Text(c, h.Reasons[s.chordSel]).FontSize(13)
	} else {
		ui.Text(c, "点击和弦查看理由（同时跳到该和弦）。改动乐句后会重新配和声。").FontSize(12).TextColor(t.TextMuted)
	}
}

// inspireExports 是导出按钮：MIDI（旋律与所选和声）、OpenUtau 工程（带拟声字，可直接合成歌声）与原始录音。
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
	save("导出 MIDI…", "motif.mid", func() ([]byte, error) { return s.analysis().MIDI() })
	save("导出 OpenUtau 工程…", "motif.ustx", func() ([]byte, error) { return s.hum.Analysis().USTX(name) })
	save("保存录音…", "motif.wav", func() ([]byte, error) { return wav16(s.recording, mic.Rate), nil })
}
