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
	"kanade/internal/score/compose"
	"kanade/internal/score/hum"
	"kanade/internal/score/karaoke"
	"kanade/internal/score/synth"
	"kanade/internal/score/vocal"
	"kanade/internal/ui/fluent"
)

// 灵感页：哼一段动机（自由哼唱自动识别速度，或跟着节拍器），整理成量化好的乐句（音符、转音分组、拟声字），
// 自动配几套不同风格的和声；可在卷帘里修改、试听并导出。整理算法见 hum.TranscribeHum，和声见 compose.Harmonize；
// 后续阶段（动机发展）见 docs/inspiration-page-research.md。
//
// 生命周期：startHum 开始录音（节拍器模式同时播放节拍器），录音中每帧检查是否该结束（节拍器播完或自由模式到达
// 最长时长），结束或点「停止」时 finishHum 停止录音，transcribe 整理并配和声。速度、拍号、小节线的调整会用同一段
// 录音重新整理（覆盖手动修改）；对音符的编辑改 hum.Notes 后重填拟声字、重新配和声。改动后 rebuildInspirePlayer 在
// 下一帧重新合成试听。只在 UI 线程修改。

// 节拍器模式的选项。
var (
	inspireMeters = []inspireMeter{{"2/4", 2, false}, {"3/4", 3, false}, {"4/4", 4, false}, {"6/8", 2, true}}
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
	// barShift 为小节线的手动挪动（拍，见 hum.HumSettings.BarShift）。
	tempoHint float64
	meterPick int
	barShift  int
	// 录音：capture 非 nil 时正在录音。recSet 是录音时的节拍设置；clickPlayer 是节拍器模式的节拍器（录音的时间轴是
	// 它的播放位置），自由模式为 nil（时间轴是已录到的时长）；recStart 是开始录音的时刻（只用于显示与自动停止）。
	recSet      hum.HumSettings
	clickPlayer *audio.Player
	capture     *mic.Capture
	sess        *karaoke.KaraokeSession
	recStart    time.Time
	recording   []float32 // 上一次的原始录音（mic.Rate）
	frames      []karaoke.KaraokeFrame
	// 整理结果：hum 为 nil 时还没有录过；sel 是选中的音符（-1 为未选）。undo / redo 是编辑前后的音符快照
	// （最多 inspireUndoMax 步，重新整理时清空），drag 是卷帘里正在进行的拖动。
	hum *hum.Hum
	sel int
	// raw 是整理出的原始音符（精简之前）；simplify 是精简程度（hum.SimplifyLevels 的序号）。hum.Notes 为 raw 按
	// simplify 精简后再经手动编辑的结果；改精简程度时从 raw 重新精简（作为一步可撤销的编辑）。
	raw        []score.MelodyNote
	simplify   int
	filler     int // hum.FillerSets 的序号
	undo, redo [][]score.MelodyNote
	drag       rollDrag
	// view 是乐句的显示方式（inspireView*：卷帘可编辑，五线谱、爵士谱只读）。
	view int
	// dev 是动机发展；history 是灵感记录（保存与打开），dirty 为 true 时下一帧把当前记录写回磁盘。
	dev     inspireDevelop
	history inspireHistory
	dirty   bool
	// 和声：style 为 compose.Style*；harmonies 是当前风格的方案，pick 为选中的方案，chordSel 为选中查看理由的和弦（-1 为未选）。
	// locks 是用户锁定或替换的和弦（乐谱时间），重新配和声时保留；重新整理节奏时清空（时间轴变了）。
	// replaceRoot / replaceQuality 是「替换为」下拉框的当前选择。
	style          int
	harmonies      []compose.Harmony
	pick           int
	chordSel       int
	locks          []compose.ChordLock
	replaceRoot    string
	replaceQuality string
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
	return inspireState{free: true, bpm: 90, meter: 2, bars: 1, sel: -1, chordSel: -1, chords: true, playerVersion: 1, simplify: 1,
		dev: inspireDevelop{chords: true, selected: -1}}
}

// metronomeSettings 返回节拍器模式的节拍设置（预备拍固定一小节）。
func (s *inspireState) metronomeSettings() hum.HumSettings {
	m := inspireMeters[s.meter]
	return hum.HumSettings{BPM: math.Round(s.bpm), BeatsPerBar: m.bpb, Compound: m.compound, Bars: inspireBars[s.bars], CountIn: 1}
}

// inspireMeter 是一个拍号选项：每小节拍数与是否复合拍（6/8 的一拍为附点四分音符）。
type inspireMeter struct {
	title    string
	bpb      int
	compound bool
}

// meterTitles 返回拍号选项的名称。
func meterTitles() []string {
	out := make([]string, len(inspireMeters))
	for i, m := range inspireMeters {
		out[i] = m.title
	}
	return out
}

// tempoLabel 返回速度的写法：复合拍以附点四分音符计。
func tempoLabel(set hum.HumSettings) string {
	if set.Compound {
		return fmt.Sprintf("♩. = %.0f", set.BPM)
	}
	return fmt.Sprintf("♩ = %.0f", set.BPM)
}

// transcribeSettings 返回整理用的节拍设置：节拍器模式为录音时的设置，自由模式带上用户的调整。
func (s *inspireState) transcribeSettings() hum.HumSettings {
	if !s.recSet.Free {
		return s.recSet
	}
	set := hum.HumSettings{Free: true, BPM: s.tempoHint, BarShift: s.barShift}
	if s.meterPick > 0 {
		m := inspireMeters[s.meterPick-1]
		set.BeatsPerBar, set.Compound = m.bpb, m.compound
	}
	return set
}

func (s *inspireState) recordingNow() bool { return s.capture != nil }

// set 返回当前乐句的节拍设置（整理后由 TranscribeHum 写回估计的速度、拍号与小节数）。
func (s *inspireState) set() hum.HumSettings { return s.hum.Settings }

// analysis 返回乐句（含选中的和声方案）的分析结果，用于合成与导出。
func (s *inspireState) analysis() *score.Analysis {
	an := s.hum.Analysis()
	if s.pick < len(s.harmonies) {
		an.Chords = s.harmonies[s.pick].Chords
	}
	return an
}

// changed 标记乐句被修改：重新填拟声字、重新配和声，作废已发展的乐段，下一帧重新合成试听并保存记录。
func (s *inspireState) changed() {
	if s.hum != nil {
		hum.FillLyrics(s.hum.Notes, s.filler, s.set().Beat())
		s.harmonize()
	}
	s.dev.reset()
	s.playerVersion++
	s.dirty = true
}

// harmonize 按当前风格与锁定的和弦重新配和声；方案数变少时选中的方案退回第一个。
func (s *inspireState) harmonize() {
	s.harmonies = compose.HarmonizeWith(*s.hum, s.style, inspireHarmonies, compose.HarmonyOptions{Locks: s.locks})
	if s.pick >= len(s.harmonies) {
		s.pick = 0
	}
	s.chordSel = -1
	s.playerVersion++
	s.dirty = true
}

// startHum 开始录音：暂停其他页的播放；节拍器模式播放节拍器并以它的位置为录音时间轴，自由模式以已录到的时长为时间轴。
func (a *app) startHum() {
	s := &a.inspire
	a.pauseAllPlayers()
	sess := karaoke.NewKaraokeSession(mic.Rate)
	var click *audio.Player
	var onSamples func([]float32)
	if s.free {
		s.recSet = hum.HumSettings{Free: true}
		// 录音回调只在录音线程上调用：elapsed 只由它读写。
		elapsed := 0.0
		onSamples = func(b []float32) {
			elapsed += float64(len(b)) / mic.Rate
			sess.Push(b, elapsed)
		}
	} else {
		s.recSet = s.metronomeSettings()
		click = audio.NewSound(inspireRate, 1, hum.MetronomePCM(s.recSet, inspireRate)).NewPlayer()
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
	s.sess, s.history.dir = nil, "" // 新录音：下一次保存时新建记录
	a.transcribe()
}

// transcribe 用录到的帧（重新）整理乐句并配和声；手动修改被覆盖。
func (a *app) transcribe() {
	s := &a.inspire
	// 录音一并传入：频谱通量补充没有辅音谷的字（「噜噜」）之间的起音证据。
	h := hum.TranscribeHumAudio(s.frames, s.recording, mic.Rate, s.transcribeSettings())
	s.raw = slices.Clone(h.Notes)
	h.Notes = hum.SimplifyMelody(h, s.simplify) // 哼唱的旋律线不稳：去掉滑音、抖动与碎片
	s.hum, s.sel, s.err, s.locks, s.undo, s.redo = &h, -1, "", nil, nil, nil
	if h.Voiced == 0 || len(h.Notes) == 0 {
		s.err = "没有录到哼唱：请检查录音设备是否选对、麦克风音量是否足够。"
	}
	s.changed()
}

// resimplify 按当前精简程度从整理出的原始音符重新精简（覆盖手动修改，作为一步可撤销的编辑）。
func (s *inspireState) resimplify() {
	if s.hum == nil {
		return
	}
	raw := s.raw
	if raw == nil {
		raw = s.hum.Notes
	}
	s.sel = -1
	s.edit(func() {
		s.hum.Notes = hum.SimplifyMelody(hum.Hum{Settings: s.hum.Settings, Notes: raw}, s.simplify)
	})
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
	var notes []synth.PianoNote
	if s.source != inspireVoice {
		notes = append(notes, synth.MelodyPiano(an)...)
	}
	if s.chords {
		notes = append(notes, synth.ChordPad(an)...)
	}
	for i := range notes {
		notes[i].Start, notes[i].End = toPlayer(notes[i].Start), toPlayer(notes[i].End)
	}
	if len(notes) > 0 {
		for i, v := range synth.RenderPiano(notes, inspireRate, length) {
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
		for _, ck := range set.Clicks(set.Bars) {
			hum.AddClick(pcm, inspireRate, toPlayer(ck.T), ck.Level)
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
		card(c, tk, "发展", func() { a.inspireDevelopCard(c, tk) })
	}
	if s.dirty {
		a.saveInspireRecord()
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
			label := "速度（♩ BPM）"
			if inspireMeters[s.meter].compound {
				label = "速度（♩. BPM）"
			}
			labeledSlider(c, label, &s.bpm, 40, 200, "%.0f").Disabled(busy)
			ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Segmented(c, &s.meter, meterTitles()...).Label("拍号").Disabled(busy)
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
		a.inspireHistoryRow(c)
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
		key := hum.KeyLabel(h.TonicPC, h.Mode, score.KeySignature(h.TonicPC, h.Mode) < 0)
		if !h.KeyClear {
			key += "（不确定，音太少或调性模糊）"
		}
		tempo := fmt.Sprintf("%s · %s · %d 小节", tempoLabel(set), set.MeterLabel(), set.Bars)
		if set.Free && s.tempoHint == 0 && s.meterPick == 0 {
			tempo += "（自动识别）"
		}
		rows := [][2]string{
			{"调", key},
			{"节奏", tempo},
			{"音符", fmt.Sprintf("%d 个音、%d 个字（%d 个转音）", len(h.Notes), syl, len(h.Notes)-syl)},
			{"音高", tuningText(h.Tuning, h.DriftSpan())},
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
		if ui.Segmented(c, &s.meterPick, append([]string{"自动"}, meterTitles()...)...).Label("拍号").Changed() {
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

func tuningText(cents, drift float64) string {
	var out string
	switch {
	case cents > 5:
		out = fmt.Sprintf("整体偏高 %.0f 音分", cents)
	case cents < -5:
		out = fmt.Sprintf("整体偏低 %.0f 音分", -cents)
	}
	// 漂移不到约六分之一个半音时听不出来，不提。
	if math.Abs(drift) >= 15 {
		dir := "越唱越高"
		if drift < 0 {
			dir = "越唱越低"
		}
		if out != "" {
			out += "，"
		}
		out += fmt.Sprintf("%s约 %.0f 音分", dir, math.Abs(drift))
	}
	if out == "" {
		return "音高准"
	}
	return out + "（已校正）"
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
	if s.recordingNow() || s.hum == nil {
		a.inspireRoll(c, tk)
		return
	}
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Segmented(c, &s.view, inspireViews...).Label("乐句视图")
		ui.Text(c, "精简").FontSize(12).TextColor(t.TextMuted)
		if ui.Segmented(c, &s.simplify, hum.SimplifyLevels...).Label("精简程度").
			Tooltip("合并滑入、滑出、抖动与碎音，补上被切开的短空隙；「精简」基本每字一音。从整理结果重新精简，会覆盖手动修改（可撤销）").
			Changed() {
			s.resimplify()
		}
	})
	if s.view == inspireViewRoll {
		a.inspireRoll(c, tk)
		a.inspireEditBar(c)
	} else {
		a.inspireSheet(c, tk, s.analysis(), s.view, s.scoreTime(playerPos(s.player)), s.seekScore)
	}
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
		fillers := make([]string, len(hum.FillerSets))
		for i, f := range hum.FillerSets {
			fillers[i] = f.Title
		}
		if ui.Segmented(c, &s.filler, fillers...).Label("拟声字").Changed() {
			hum.FillLyrics(s.hum.Notes, s.filler, s.set().Beat())
			s.dev.refill(s.filler)
			s.playerVersion++
			s.dirty = true
		}
		ui.Box(c).Grow(1)
		a.inspireExports(c)
	})
	if s.player != nil {
		transport(c, tk, "inspire", s.player)
	}
}

// inspireExports 是导出按钮：MIDI（旋律与所选和声）、OpenUtau 与 Synthesizer V 工程（带拟声字，可直接合成歌声）
// 与原始录音。
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
	save("导出 MIDI…", "motif.mid", func() ([]byte, error) { return synth.MIDI(s.analysis()) })
	save("导出 OpenUtau 工程…", "motif.ustx", func() ([]byte, error) { return vocal.USTX(s.hum.Analysis(), name) })
	save("导出 Synthesizer V 工程…", "motif.svp", func() ([]byte, error) { return vocal.SVP(s.hum.Analysis(), name) })
	save("保存录音…", "motif.wav", func() ([]byte, error) { return wav16(s.recording, mic.Rate), nil })
}
