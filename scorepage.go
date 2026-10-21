package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/score"
	"kanade/internal/stems"
	"kanade/internal/ui/fluent"
)

// scoreState 是音乐分析页的界面状态。
type scoreState struct {
	mode     int    // modeSmart / modePro
	input    string // 要分析的歌曲
	separate bool   // 先分离伴奏，调号与和弦基于伴奏分析
	// 专业模式参数。
	stemChoice                          string // coverStemChoices 之一（与翻唱共用选项）
	beatTitle, profileTitle, vocabTitle string
	fold                                bool
	foldMin, foldMax                    float64
	keySegment                          float64
	selfProb                            float64
	minBeats                            float64
	emotion                             bool

	// runs 是本次运行的分析，最新在前；shown 是正在显示的一项。切换页面不丢失。
	runs    []*score.Run
	shown   int
	preview audioPreview
	sheet   sheetState
	dlJob   string
	err     string
}

func newScoreState() scoreState {
	d := score.DefaultOptions()
	return scoreState{separate: true, stemChoice: coverStemChoices[0], beatTitle: score.BeatMethods[0].Title,
		profileTitle: score.KeyProfiles[0].Title, vocabTitle: score.ChordVocabs[0].Title, foldMin: 70, foldMax: 180,
		keySegment: d.KeySegmentS, selfProb: d.ChordSelfProb, minBeats: float64(d.ChordMinBeats), emotion: true,
		sheet: newSheetState()}
}

func keyOf(list []struct{ Key, Title string }, title string) string {
	for _, x := range list {
		if x.Title == title {
			return x.Key
		}
	}
	return list[0].Key
}

// request 按当前模式组装分析请求：智能模式只有「分离伴奏」一项，其余用引擎默认。
func (a *app) scoreRequest() score.Request {
	s := &a.score
	r := score.Request{Input: s.input, Separate: s.separate, StemModels: stems.Recommended, Options: score.DefaultOptions()}
	if s.mode == modeSmart {
		return r
	}
	switch s.stemChoice {
	case coverStemChoices[1]:
		r.StemModels = nil
	case coverStemChoices[2]:
		r.StemModels = a.stems.models
	}
	o := &r.Options
	o.BeatMethod, o.KeyProfile, o.ChordVocab = keyOf(score.BeatMethods, s.beatTitle), keyOf(score.KeyProfiles, s.profileTitle), keyOf(score.ChordVocabs, s.vocabTitle)
	o.KeySegmentS, o.ChordSelfProb, o.ChordMinBeats, o.Emotion = s.keySegment, s.selfProb, int(s.minBeats), s.emotion
	if s.fold {
		o.TempoMin, o.TempoMax = s.foldMin, s.foldMax
	}
	return r
}

// scorePage 是音乐分析页：调号、拍号、BPM、和弦与情绪。
func (a *app) scorePage(c *ui.Context, tk *fluent.Tokens) {
	pageHeader(c, "音乐分析", "Score · 调号、拍号、BPM、和弦与情绪识别。")
	if a.backendUnavailable(c, tk) {
		return
	}
	s := &a.score
	a.scoreModelsCard(c, tk)
	a.scoreRunCard(c, tk)
	if len(s.runs) == 0 {
		return
	}
	st := s.runs[s.shown].Snapshot()
	if st.Result == nil {
		return
	}
	s.preview.load(a, st.Audio)
	s.preview.title = filepath.Base(s.runs[s.shown].Request.Input)
	res := st.Result
	a.scoreSummary(c, tk, s.runs[s.shown], st)
	card(c, tk, "试听", func() { a.scorePlayer(c, tk, res) })
	a.scoreSheetCard(c, tk, res)
	a.scoreColorCard(c, tk, res)
	if res.Emotion != nil {
		card(c, tk, "情绪", func() { a.scoreEmotion(c, res) })
	}
}

// scoreModelsCard 显示分析模型的下载状态。
func (a *app) scoreModelsCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.score
	l := a.be.layout
	var missing []any
	mb := 0
	for _, g := range score.ModelGroups {
		if !g.Installed(l) {
			missing, mb = append(missing, g.Key), mb+g.SizeMB
		}
	}
	dl, active := a.jobStatusPeek(s.dlJob)
	if len(missing) == 0 && !active && dl.State != conductor.JobFailed {
		return
	}
	card(c, tk, "模型", func() {
		for _, g := range score.ModelGroups {
			ui.Row(c.Key("score-model-" + g.Key)).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, g.Title).Width(160)
				if g.Installed(l) {
					ui.Text(c, "已下载").FontSize(12).TextColor(t.Success)
				} else {
					ui.Textf(c, "未下载 · %d MB", g.SizeMB).FontSize(12).TextColor(t.TextMuted)
				}
			})
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c, fmt.Sprintf("下载缺少的模型（约 %d MB）", mb)).Disabled(active || len(missing) == 0).Clicked() {
				id, err := a.be.cond.Submit(score.Engine, "score.download", map[string]any{"models": missing})
				s.dlJob, s.err = id, errText(err)
			}
			if active {
				ui.Text(c, dl.Message).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
			}
		})
		if dl.State == conductor.JobFailed {
			ui.Text(c, "下载失败："+dl.Err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		ui.Text(c, "未下载的模型也会在首次分析时自动下载。模型保存在 "+score.ModelDir(l)).FontSize(12).TextColor(t.TextMuted).Selectable()
	})
}

// scoreRunCard 选择歌曲与参数并启动分析，显示流水线进度。
func (a *app) scoreRunCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.score
	var cur *score.Run
	var st score.RunState
	if len(s.runs) > 0 {
		cur = s.runs[0]
		st = cur.Snapshot()
	}
	card(c, tk, "分析", func() {
		modeSwitch(c, "score-mode", &s.mode, "只需选择歌曲；节拍、调号、和弦与情绪都用推荐参数。")
		a.audioPicker(c, "歌曲", &s.input, anyAudioExts())
		ui.Checkbox(c, &s.separate, "先分离伴奏再分析调号与和弦（人声会干扰和弦判断；分离需要 GPU，CPU 上较慢）")
		if s.separate {
			if files, mb := stems.Missing(a.be.layout, []string{"other"}, a.scoreRequest().StemModels); len(files) > 0 {
				ui.Textf(c, "首次分离会下载分轨模型（约 %d MB）。", mb).FontSize(12).TextColor(t.TextMuted)
			}
		}
		if s.mode == modePro {
			a.scoreProParams(c)
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.PrimaryButton(c, "开始分析").Disabled(cur != nil && st.Running()).Clicked() {
				run, err := score.Start(a.be.cond, a.scoreRequest(), func() { a.post(func() {}) })
				if err == nil {
					s.runs, s.shown = append([]*score.Run{run}, s.runs...), 0
					cur, st = run, run.Snapshot()
				}
				s.err = errText(err)
			}
		})
		if cur != nil && (st.Running() || st.Err != "") {
			for i, title := range score.StageTitles[:score.StageDone] {
				mark, color := "○", t.TextMuted
				switch {
				case i == score.StageSeparate && st.Skipped:
					mark, title = "–", title+"（跳过）"
				case st.Err != "" && i == st.Stage:
					mark, color = "✕", t.Danger
				case i < st.Stage:
					mark, color = "✓", t.Success
				case i == st.Stage:
					mark, color = "●", t.Accent
				}
				ui.Row(c.Key("score-stage-" + title)).Gap(8).Children(func() {
					ui.Text(c, mark).TextColor(color).Width(16)
					ui.Text(c, title).TextColor(color)
				})
			}
			if st.Job != "" && st.Running() {
				a.jobStatus(c, st.Job)
			}
			if st.Err != "" {
				ui.Text(c, st.Err).FontSize(12).TextColor(t.Danger).Selectable()
			}
		}
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(t.Danger)
		}
		// 本次运行分析过的歌曲，可切换查看。
		if len(s.runs) > 1 {
			names := make([]string, len(s.runs))
			for i, r := range s.runs {
				names[i] = fmt.Sprintf("%d. %s", len(s.runs)-i, filepath.Base(r.Request.Input))
			}
			sel := names[s.shown]
			if ui.Select(c, &sel, names).Label("查看结果").Width(420).Changed() {
				for i, n := range names {
					if n == sel {
						s.shown = i
					}
				}
			}
		}
	})
}

// scoreProParams 是专业模式参数。
func (a *app) scoreProParams(c *ui.Context) {
	t := c.Theme()
	s := &a.score
	titlesOf := func(list []struct{ Key, Title string }) []string {
		return titles(list, func(x struct{ Key, Title string }) string { return x.Title })
	}
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		if s.separate {
			ui.Field(c, "分离模型", func() { ui.Select(c, &s.stemChoice, coverStemChoices).Width(220) })
		}
		ui.Field(c, "节拍算法", func() { ui.Select(c, &s.beatTitle, titlesOf(score.BeatMethods)).Width(240) })
		ui.Field(c, "调性轮廓", func() { ui.Select(c, &s.profileTitle, titlesOf(score.KeyProfiles)).Width(180) })
	})
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Checkbox(c, &s.fold, "限定 BPM 范围")
		if s.fold {
			ui.Field(c, "下限", func() { ui.NumberInput(c, &s.foldMin, 20, 200, 5).Width(100) })
			ui.Field(c, "上限", func() { ui.NumberInput(c, &s.foldMax, 40, 400, 5).Width(100) })
		}
		ui.Field(c, "转调检测窗口（秒，0 关闭）", func() { ui.NumberInput(c, &s.keySegment, 0, 120, 5).Width(120) })
	})
	ui.Text(c, "限定范围后，BPM 会按倍数折叠到区间内（例如抒情歌被识别成倍速时设为 60–120）。").FontSize(12).TextColor(t.TextMuted)
	ui.Row(c).Gap(12).AlignItems(ui.End).Children(func() {
		ui.Field(c, "和弦词汇", func() { ui.Select(c, &s.vocabTitle, titlesOf(score.ChordVocabs)).Width(220) })
		ui.Field(c, "最短和弦（拍）", func() { ui.NumberInput(c, &s.minBeats, 1, 8, 1).Width(100) })
	})
	labeledSlider(c, "和弦平滑", &s.selfProb, 0.5, 0.99, "%.2f")
	ui.Text(c, "平滑越大，和弦越不容易切换（减少逐拍抖动，但可能漏掉快速换和弦）。").FontSize(12).TextColor(t.TextMuted)
	ui.Checkbox(c, &s.emotion, "情绪识别（CLAP，首次需下载约 615 MB）")
}

// scoreSummary 是结果概览：调号、拍号、BPM、情绪，以及导出按钮。
func (a *app) scoreSummary(c *ui.Context, tk *fluent.Tokens, run *score.Run, st score.RunState) {
	t := c.Theme()
	s := &a.score
	res := st.Result
	name := strings.TrimSuffix(filepath.Base(run.Request.Input), filepath.Ext(run.Request.Input))
	card(c, tk, "概览 · "+name, func() {
		ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
			tile := func(title, value, note string) {
				ui.Column(c.Key("tile-"+title)).Grow(1).Basis(0).Padding(12).Gap(4).Radius(fluent.ControlRadius).
					Background(tk.Layer).Border(1, tk.CardStroke).Children(func() {
					ui.Text(c, title).FontSize(12).TextColor(t.TextMuted)
					ui.Text(c, value).FontSize(20).FontWeight(600)
					ui.Text(c, note).FontSize(12).TextColor(t.TextMuted)
				})
			}
			if segs := res.Key.Segments; len(segs) > 1 {
				// 有转调时整首调号是各段的折中，直接显示各段的调。
				labels := make([]string, len(segs))
				for i, sg := range segs {
					labels[i] = sg.Label
				}
				tile("调号（转调）", strings.Join(labels, " → "), fmt.Sprintf("转调于 %s", score.Clock(segs[1].Start)))
			} else {
				tile("调号", res.Key.Label, fmt.Sprintf("置信度%s · 关系调 %s", res.Key.Confidence, res.Key.Relative))
			}
			meter, meterNote := "—", "未检测到小节线"
			if res.Meter.Label != nil {
				meter, meterNote = *res.Meter.Label, fmt.Sprintf("%.0f%% 的小节符合", res.Meter.Confidence*100)
			}
			tile("拍号", meter, meterNote)
			bpm, bpmNote := "—", ""
			if res.Tempo.BPM != nil {
				bpm = fmt.Sprintf("%.1f", *res.Tempo.BPM)
				bpmNote = "匀速"
				if res.Tempo.Stable != nil && !*res.Tempo.Stable {
					bpmNote = "有变速"
				}
				bpmNote += fmt.Sprintf("（%.0f%% 小节在 ±5%% 内）", res.Tempo.SteadyShare*100)
				if res.Tempo.Folded {
					bpmNote += " · 已折叠"
				}
			}
			tile("BPM", bpm, bpmNote)
			mood, moodNote := "—", "未做情绪识别"
			if e := res.Emotion; e != nil && len(e.Moods) > 0 {
				mood = e.Moods[0].Label
				if len(e.Moods) > 1 {
					mood += " · " + e.Moods[1].Label
				}
				moodNote = fmt.Sprintf("效价 %.2f · 唤醒度 %.2f", e.Valence, e.Arousal)
			}
			tile("情绪", mood, moodNote)
		})
		if res.Key.Confidence == "低" && len(res.Key.Segments) == 0 && len(res.Key.Candidates) > 1 {
			ui.Textf(c, "调号难以区分：也可能是 %s。", res.Key.Candidates[1].Label).FontSize(12).TextColor(t.Warning)
		}
		if segs := res.Key.Segments; len(segs) > 1 {
			parts := make([]string, len(segs))
			for i, sg := range segs {
				parts[i] = fmt.Sprintf("%s–%s %s", score.Clock(sg.Start), score.Clock(sg.End), sg.Label)
			}
			ui.Text(c, "转调："+strings.Join(parts, " → ")).FontSize(12).TextColor(t.TextMuted)
		}
		src := "原曲（含人声）"
		if res.ChordSource == "accompaniment" {
			src = "分离出的伴奏"
		}
		ui.Text(c, "调号与和弦基于"+src+"分析。").FontSize(12).TextColor(t.TextMuted)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			export := func(label, src, name string) {
				if src != "" && ui.Button(c.Key("export-"+label), label).Clicked() {
					a.saveFileAs(src, name, func(_ string, err error) { s.err = errText(err) })
				}
			}
			export("导出分析 JSON…", st.Files["analysis"], name+"_analysis")
			export("导出和弦标注 .lab…", st.Files["chords"], name+"_chords")
			if ui.Button(c, "导出和弦谱 .txt…").Clicked() {
				// 和弦谱由界面生成：先写到暂存区，再走统一的另存流程。
				p := filepath.Join(filepath.Dir(st.Files["analysis"]), "chord_chart.txt")
				if err := os.WriteFile(p, []byte(res.ChordChart(name)), 0o644); err != nil {
					s.err = err.Error()
				} else {
					a.saveFileAs(p, name+"_和弦谱", func(_ string, err error) { s.err = errText(err) })
				}
			}
		})
	})
}

// scorePosition 返回当前播放位置（秒，没有播放器时为 -1）与跳转函数。
func (a *app) scorePosition() (float64, func(float64)) {
	pv := &a.score.preview
	pos := -1.0
	if p := pv.player; p != nil && !pv.loading {
		pos = p.Position().Seconds()
	}
	return pos, func(sec float64) {
		if p := pv.player; p != nil {
			p.Seek(time.Duration(sec * float64(time.Second)))
		}
	}
}

// scorePlayer 是试听：波形与播放，播放时显示当前小节、拍与和弦。
func (a *app) scorePlayer(c *ui.Context, tk *fluent.Tokens, res *score.Analysis) {
	t := c.Theme()
	s := &a.score
	s.preview.show(c, tk, "", nil)
	a.scorePlaySource(c, res)
	pos, seek := a.scorePosition()
	bar, beat := res.BarAt(pos)
	cur, next := "—", ""
	if i := res.ChordAt(pos); i >= 0 {
		cur = res.Chords[i].Label
		if i+1 < len(res.Chords) {
			next = res.Chords[i+1].Label
		}
	}
	timeline(c, "chord-line", res.Chords, res.Duration, pos, chordWindowS, seek, nil)
	ui.Row(c).Gap(24).AlignItems(ui.End).Children(func() {
		ui.Column(c).Gap(2).Children(func() {
			ui.Text(c, "当前和弦").FontSize(12).TextColor(t.TextMuted)
			ui.Text(c, cur).FontSize(28).FontWeight(600)
		})
		ui.Column(c).Gap(2).Children(func() {
			ui.Text(c, "下一个").FontSize(12).TextColor(t.TextMuted)
			ui.Text(c, next).FontSize(16).TextColor(t.TextMuted)
		})
		if bar > 0 {
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, "小节 · 拍").FontSize(12).TextColor(t.TextMuted)
				ui.Textf(c, "%d · %d", bar, beat).FontSize(16).Font("monospace")
			})
		}
	})
}

// scoreEmotion 显示情绪分布、效价 / 唤醒度与随时间的变化（与试听同步，点击跳转）。
func (a *app) scoreEmotion(c *ui.Context, res *score.Analysis) {
	t := c.Theme()
	e := res.Emotion
	pos, seek := a.scorePosition()
	for _, m := range e.Moods[:min(5, len(e.Moods))] {
		ui.Row(c.Key("mood-" + m.Key)).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, m.Label).Width(48)
			// 十类的均匀分布是 0.1；以 0.3 为满格，让差异看得出来。
			ui.Progress(c, min(m.Score/0.3, 1)).Width(240)
			ui.Textf(c, "%.0f%%", m.Score*100).FontSize(12).Font("monospace").TextColor(t.TextMuted)
		})
	}
	ui.Textf(c, "效价（消极 0 — 积极 1）%.2f · 唤醒度（舒缓 0 — 激烈 1）%.2f", e.Valence, e.Arousal).FontSize(12).TextColor(t.TextMuted)
	if len(e.Segments) > 1 {
		ui.Text(c, "随时间的变化").FontSize(12).TextColor(t.TextMuted)
		timeline(c, "mood-line", e.Segments, res.Duration, pos, 0, seek, moodColor)
	}
}

// moodColor 给情绪时间线着色：积极、激烈的情绪偏暖，消极、舒缓的偏冷。
func moodColor(t *ui.Theme, label string) ui.Color {
	switch label {
	case "欢快", "激昂", "宏大", "愤怒":
		return t.Warning.Alpha(0.35)
	case "悲伤", "阴郁", "怀旧":
		return t.Accent.Alpha(0.35)
	default:
		return t.Success.Alpha(0.3)
	}
}

// chordWindowS 是和弦时间线显示的时长（秒）：约 10 小节（130 BPM），每个和弦都有空间显示名称。
const chordWindowS = 20

// timelineMinLabelShare 是时间线上显示文字的最小段宽（占全曲时长的比例）：内容区约 900 像素宽时约 27 像素，
// 刚好放下「Bbm7」这样的和弦名；更窄的段只显示色块，悬停可看名称。
const timelineMinLabelShare = 0.03

// timeline 是与时间轴等比的分段条：每段宽度按时长分配，pos 所在段高亮，点击跳到段首。
// window > 0 且短于全曲时只显示 [pos 前 20%, pos 后 80%] 的 window 秒并随播放滚动，段落短（如和弦）时用它保证文字放得下。
// color 为 nil 时用交替的中性色。
func timeline(c *ui.Context, key string, segs []score.Segment, duration, pos, window float64, seek func(float64),
	color func(*ui.Theme, string) ui.Color) {
	t := c.Theme()
	tk := fluent.For(t)
	if duration <= 0 || len(segs) == 0 {
		return
	}
	v0, v1 := 0.0, duration
	if window > 0 && duration > window {
		v0 = min(max(pos-window*0.2, 0), duration-window)
		v1 = v0 + window
	}
	ui.Row(c.Key(key)).Height(30).Gap(1).AlignItems(ui.Stretch).Radius(fluent.ControlRadius).Clip().Children(func() {
		// 窗口起点到第一段之间的空白（例如前奏未检测到和弦）用占位撑开，保证与时间轴对齐。
		cursor := v0
		for i, sg := range segs {
			a, b := max(sg.Start, v0), min(sg.End, v1)
			if b <= a {
				continue
			}
			if a > cursor {
				ui.Box(c.Key(fmt.Sprintf("%s-gap-%d", key, i))).Grow(float32(a - cursor)).Basis(0)
			}
			cursor = b
			dur := b - a
			cur := pos >= sg.Start && pos < sg.End
			btn := ui.ButtonBase(c.Key(fmt.Sprintf("%s-%d", key, i))).Grow(float32(dur)).Basis(0).MinWidth(0).Clip().
				AlignItems(ui.Center).Justify(ui.Center).Label(sg.Label).Tooltip(fmt.Sprintf("%s · %s", score.Clock(sg.Start), sg.Label))
			bg := tk.SubtleHover
			switch {
			case cur:
				bg = t.Accent
			case color != nil:
				bg = color(t, sg.Label)
			case i%2 == 1:
				bg = tk.SubtlePressed
			}
			if btn.Hovered() && !cur {
				bg = tk.SubtlePressed
			}
			btn.Background(bg)
			if dur/(v1-v0) >= timelineMinLabelShare {
				btn.Children(func() {
					txt := ui.Text(c, sg.Label).FontSize(12).SingleLine()
					if cur {
						txt.TextColor(t.AccentText)
					}
				})
			}
			if btn.Clicked() {
				seek(sg.Start)
			}
		}
		if cursor < v1 {
			ui.Box(c.Key(key + "-tail")).Grow(float32(v1 - cursor)).Basis(0)
		}
	})
}
