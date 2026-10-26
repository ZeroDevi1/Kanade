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

	"kanade/internal/score"
	"kanade/internal/score/compose"
	"kanade/internal/score/hum"
	"kanade/internal/score/synth"
	"kanade/internal/score/vocal"
	"kanade/internal/ui/fluent"
)

// 灵感页的「发展」卡片：把动机按句式发展成 8 或 16 小节的乐段（compose.Develop），看三套方案与每段用了什么变换，
// 锁定满意的段落后重新生成其余段落，试听（旋律与和声）并导出。动机被修改后方案作废，需要重新生成。

// inspireDevelop 是动机发展的状态（属于 inspireState）。
type inspireDevelop struct {
	bars     int    // 0 为 8 小节，1 为 16 小节
	form     string // compose.DevForms 的 Key；空为自动（该长度的全部句式）
	periods  []compose.Period
	pick     int
	locked   map[int]bool      // 锁定的格（序号）
	harmony  []compose.Harmony // 所选方案的和声（第一套）
	player   *audio.Player
	version  int // 改动计数：与 built 不同时重新合成试听
	built    int
	chords   bool // 试听叠加和弦
	selected int  // 选中查看说明的格（-1 为未选）
	view     int  // 显示方式（inspireView*）
}

// refill 按拟声字组 filler 重填各方案的拟声字（方案不变）。
func (d *inspireDevelop) refill(filler int) {
	for i := range d.periods {
		h := &d.periods[i].Hum
		hum.FillLyrics(h.Notes, filler, h.Settings.Beat())
	}
	d.version++
}

// reset 在动机改变后作废已生成的方案（锁定一并清除：格里的音来自旧动机）。
func (d *inspireDevelop) reset() {
	if d.player != nil {
		d.player.Stop()
	}
	*d = inspireDevelop{bars: d.bars, form: d.form, chords: d.chords, selected: -1, view: d.view}
}

// period 返回选中的方案（没有时为 nil）。
func (d *inspireDevelop) period() *compose.Period {
	if d.pick < len(d.periods) {
		return &d.periods[d.pick]
	}
	return nil
}

// develop 生成乐段：有锁定的格时只重新生成其余格，句式固定为当前方案的句式。
func (a *app) develop() {
	s := &a.inspire
	d := &s.dev
	opt := compose.DevelopOptions{Bars: []int{8, 16}[d.bars], Form: d.form, Filler: s.filler}
	if p := d.period(); p != nil && len(d.locked) > 0 {
		opt.Locked = map[int]compose.DevSlot{}
		for i := range d.locked {
			opt.Locked[i] = p.Slots[i]
		}
		for _, f := range compose.DevForms {
			if f.Title == p.Form {
				opt.Form = f.Key
			}
		}
	}
	d.periods, d.pick, d.selected = compose.Develop(*s.hum, opt, inspireHarmonies), 0, -1
	if len(d.periods) == 0 {
		d.locked = nil
	}
	s.devHarmonize()
}

// devHarmonize 为选中的方案配和声（与和声卡片相同的风格）。
func (s *inspireState) devHarmonize() {
	d := &s.dev
	d.harmony = nil
	if p := d.period(); p != nil {
		d.harmony = p.Harmonize(s.style, 1, nil)
	}
	d.version++
	s.dirty = true
}

// devAnalysis 返回选中方案（含和声）的分析结果，用于合成与导出。
func (d *inspireDevelop) devAnalysis() *score.Analysis {
	an := d.period().Hum.Analysis()
	if len(d.harmony) > 0 {
		an.Chords = d.harmony[0].Chords
	}
	return an
}

// rebuildDevPlayer 合成选中方案的试听（旋律，可叠加和弦），保持播放位置与状态。
func (a *app) rebuildDevPlayer() {
	d := &a.inspire.dev
	p := d.period()
	if p == nil || d.built == d.version {
		return
	}
	d.built = d.version
	an := d.devAnalysis()
	notes := synth.MelodyPiano(an)
	if d.chords {
		notes = append(notes, synth.ChordPad(an)...)
	}
	length := p.Hum.Settings.Length() + p.Hum.Settings.Beat()
	pcm := synth.RenderPiano(notes, inspireRate, length)
	var pos time.Duration
	playing, loop, vol := false, false, 1.0
	if d.player != nil {
		pos, playing, loop, vol = d.player.Position(), d.player.Playing(), d.player.Loop(), d.player.Volume()
		d.player.Stop()
	}
	d.player = audio.NewSound(inspireRate, 1, pcm).NewPlayer().SetLoop(loop).SetVolume(vol)
	d.player.Seek(min(pos, d.player.Duration()))
	if playing {
		d.player.Play()
	}
}

// inspireDevelopCard 是「发展」卡片。
func (a *app) inspireDevelopCard(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	d := &s.dev
	a.rebuildDevPlayer()
	bars := []int{8, 16}[d.bars]
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &d.bars, "8 小节", "16 小节").Label("乐段长度").Changed() {
			d.form = ""
		}
		titles := []string{"自动"}
		keys := []string{""}
		for _, f := range compose.DevForms {
			if f.Bars == []int{8, 16}[d.bars] {
				titles, keys = append(titles, f.Title), append(keys, f.Key)
			}
		}
		sel := titles[0]
		for i, k := range keys {
			if k == d.form {
				sel = titles[i]
			}
		}
		if ui.Select(c, &sel, titles).Label("句式").Width(160).Changed() {
			for i, x := range titles {
				if x == sel {
					d.form = keys[i]
				}
			}
		}
		label := "生成乐段"
		if len(d.locked) > 0 {
			label = fmt.Sprintf("重新生成其余段落（锁定 %d 格）", len(d.locked))
		} else if len(d.periods) > 0 {
			label = "重新生成"
		}
		if ui.PrimaryButton(c, label).Clicked() {
			a.develop()
		}
	})
	if s.hum.Settings.Bars > 4 {
		ui.Text(c, "动机长于 4 小节：发展时只用前 4 小节。").FontSize(12).TextColor(t.TextMuted)
	}
	if len(d.periods) == 0 {
		ui.Text(c, fmt.Sprintf("按句式把动机发展成 %d 小节的乐段：模进、倒影、裂解、句尾改写等手法，段末放半终止或全终止，"+
			"并按段落配和声。", bars)).FontSize(12).TextColor(t.TextMuted)
		return
	}
	// 方案。
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		for i, p := range d.periods {
			b := ui.Button(c.Key(fmt.Sprintf("period-%d", i)), fmt.Sprintf("方案 %d · %s · %.0f 分", i+1, p.Form, p.Score)).Tooltip(p.Notes)
			if i == d.pick {
				b = b.Disabled(true)
			}
			if b.Clicked() {
				d.pick, d.locked, d.selected = i, nil, -1
				s.devHarmonize()
			}
		}
	})
	p := d.period()
	// 段落：每格一个按钮，点击查看说明；锁形按钮锁定 / 解锁。
	ui.Row(c).Wrap().Gap(6).AlignItems(ui.Center).Children(func() {
		for i, sl := range p.Slots {
			ui.Row(c.Key(fmt.Sprintf("slot-%d", i))).Gap(2).AlignItems(ui.Center).Children(func() {
				b := ui.ButtonBase(c.Key("slot-btn")).Padding(4, 10).Radius(fluent.ControlRadius).Border(1, tk.CardStroke).
					Label(fmt.Sprintf("第 %d–%d 小节 %s", sl.Bar+1, sl.Bar+2, sl.Label))
				switch {
				case i == d.selected:
					b.Background(tk.SubtlePressed).Border(1, t.Accent)
				case b.Hovered():
					b.Background(tk.SubtleHover)
				}
				b.Children(func() {
					ui.Text(c, sl.Label).FontSize(14).FontWeight(600)
					ui.Textf(c, " %d–%d", sl.Bar+1, sl.Bar+2).FontSize(11).TextColor(t.TextMuted)
				})
				if b.Clicked() {
					d.selected = i
					if d.player != nil {
						beat := p.Hum.Settings.Beat()
						d.player.Seek(time.Duration(float64(sl.Bar*p.Hum.Settings.BeatsPerBar) * beat * float64(time.Second)))
					}
				}
				lk := ui.ButtonBase(c.Key("slot-lock")).Size(24, 24).Radius(fluent.ControlRadius).Center().
					Label("锁定这一格").Tooltip("锁定这一格，重新生成时保留")
				if d.locked[i] {
					lk.Background(tk.SubtlePressed).TextColor(t.Accent)
				} else if lk.Hovered() {
					lk.Background(tk.SubtleHover)
				}
				lk.Children(func() { ui.Icon(c, iconLock).Size(12, 12) })
				if lk.Clicked() {
					if d.locked == nil {
						d.locked = map[int]bool{}
					}
					if d.locked[i] {
						delete(d.locked, i)
					} else {
						d.locked[i] = true
					}
				}
			})
		}
	})
	if d.selected >= 0 && d.selected < len(p.Slots) {
		sl := p.Slots[d.selected]
		ui.Text(c, fmt.Sprintf("%s（第 %d–%d 小节）：%s", sl.Label, sl.Bar+1, sl.Bar+2, sl.Desc)).FontSize(13)
		a.slotEditor(c, d.selected)
	} else {
		ui.Text(c, "点击段落查看用了什么变换（同时跳到该处）；点锁形按钮锁定满意的段落，再重新生成其余段落。").
			FontSize(12).TextColor(t.TextMuted)
	}
	ui.Segmented(c, &d.view, inspireViews...).Label("乐段视图")
	if d.view == inspireViewRoll {
		a.devRoll(c, tk, p)
	} else {
		seek := func(sec float64) {
			if d.player != nil {
				d.player.Seek(time.Duration(max(sec, 0) * float64(time.Second)))
			}
		}
		a.inspireSheet(c, tk, d.devAnalysis(), d.view, playerPos(d.player), seek)
	}
	if len(d.harmony) > 0 {
		h := d.harmony[0]
		ui.Textf(c, "和声（%s）：旋律匹配 %.0f · 进行流畅 %.0f", compose.HarmonyStyles[s.style], h.Melody, h.Prog).
			FontSize(12).TextColor(t.TextMuted)
	}
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Checkbox(c, &d.chords, "和弦").Changed() {
			d.version++
		}
		ui.Box(c).Grow(1)
		a.devExports(c)
	})
	if d.player != nil {
		transport(c, tk, "develop", d.player)
	}
}

// devRoll 是乐段的卷帘（只读）：旋律、拟声字与和弦名，以及试听的播放头。
func (a *app) devRoll(c *ui.Context, tk *fluent.Tokens, p *compose.Period) {
	t := c.Theme()
	d := &a.inspire.dev
	set := p.Hum.Settings
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, n := range p.Hum.Notes {
		lo, hi = min(lo, n.Pitch), max(hi, n.Pitch)
	}
	lo, hi = lo-3, hi+3
	if hi-lo < 18 {
		lo -= math.Floor((18 - (hi - lo)) / 2)
		hi = lo + 18
	}
	rv := rollView{lo: lo, hi: hi, length: set.Length(), beat: set.Beat(), bpb: set.BeatsPerBar, sub: set.Sub(), notes: p.Hum.Notes,
		tonic: p.Hum.TonicPC, sel: -1, accent: t.Accent, toX: func(x float64) float64 { return x }}
	if len(d.harmony) > 0 {
		rv.chords = d.harmony[0].Chords
	}
	player := d.player
	if player != nil {
		rv.pos = func() (float64, bool) { return player.Position().Seconds(), player.Playing() }
	}
	box := ui.Box(c.Key("develop-roll")).Height(rollHeight).Radius(fluent.ControlRadius).Background(rollPaper.white).
		Border(1, tk.CardStroke).Clip().Cursor(ui.CursorPointer).Label("乐段卷帘")
	if b := box.Bounds(); box.Clicked() && player != nil && b.W > rollKeysW {
		x, _, _ := box.PointerPosition()
		sec := float64((x-rollKeysW)/(b.W-rollKeysW)) * set.Length()
		player.Seek(time.Duration(max(sec, 0) * float64(time.Second)))
	}
	box.Draw(func(pa *ui.Painter, r ui.Rect) { paintRoll(pa, r, rv) })
}

// devExports 是乐段的导出：MIDI（旋律与和声）、OpenUtau 与 Synthesizer V 工程（带拟声字）。
func (a *app) devExports(c *ui.Context) {
	s := &a.inspire
	d := &s.dev
	name := fmt.Sprintf("乐段_%s", time.Now().Format("0102_1504"))
	save := func(label, file string, data func() ([]byte, error)) {
		if !ui.Button(c, label).Clicked() {
			return
		}
		b, err := data()
		if err != nil {
			s.err = err.Error()
			return
		}
		path := filepath.Join(a.be.layout.Scratch, file)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			s.err = err.Error()
			return
		}
		a.saveFileAs(path, name, func(_ string, err error) { s.err = errText(err) })
	}
	save("导出 MIDI…", "period.mid", func() ([]byte, error) { return synth.MIDI(d.devAnalysis()) })
	save("导出 OpenUtau 工程…", "period.ustx", func() ([]byte, error) { return vocal.USTX(d.period().Hum.Analysis(), name) })
	save("导出 Synthesizer V 工程…", "period.svp", func() ([]byte, error) { return vocal.SVP(d.period().Hum.Analysis(), name) })
}

// specFrag 是一个裂解选项：取开头（frag 1）或末尾（2）beats 拍，重复 reps 次。
type specFrag struct {
	title             string
	frag, beats, reps int
}

// 逐格编辑的选项（下拉框的文字与对应的值）。
var (
	specSources = []string{"动机前 2 小节", "动机第 3–4 小节"}
	specFrags   = []specFrag{{"不裂解", 0, 0, 0}, {"开头 1 拍 ×2", 1, 1, 2}, {"开头 1 拍 ×4", 1, 1, 4}, {"开头 2 拍 ×2", 1, 2, 2}, {"末尾 2 拍 ×2", 2, 2, 2}}
	specSteps   = []string{"−3", "−2", "−1", "0", "+1", "+2", "+3"}
	specVars    = []string{"合并同音", "节奏不变", "加经过音"}
	specCads    = []string{"无终止", "半终止", "全终止"}
	specRegions = []string{"和声不偏向", "转向下属", "转向关系调"}
)

// slotEditor 是选中格的变换链编辑：素材、裂解（与每次的模进）、倒影、模进、节奏变奏、终止与和声区域。
// 任何修改都按新的设置重新生成这一格（compose.EditSlot），并把它锁定，重新生成其余段落时保留。
func (a *app) slotEditor(c *ui.Context, i int) {
	s := &a.inspire
	d := &s.dev
	p := d.period()
	sp := p.Slots[i].Spec
	step := func(k int) string { return specSteps[min(max(k+3, 0), len(specSteps)-1)] }
	fragTitle := specFrags[0].title
	for _, f := range specFrags {
		if f.frag == sp.Frag && f.beats == sp.FragBeats && f.reps == sp.FragReps {
			fragTitle = f.title
		}
	}
	src, fr, fk, inv, sq := specSources[min(sp.Source, 1)], fragTitle, step(sp.FragK), compose.SpecInvAxes[min(max(sp.Inv, 0), len(compose.SpecInvAxes)-1)], step(sp.Seq)
	vr, cd, rg := specVars[sp.Var+1], specCads[sp.Cadence], specRegions[sp.Region]
	changed := false
	ui.Row(c.Key("slot-editor")).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		sel := func(label string, v *string, options []string, w float32) {
			ui.Row(c.Key("edit-" + label)).Gap(4).AlignItems(ui.Center).Children(func() {
				ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
				if ui.Select(c, v, options).Label(label).Width(w).Changed() {
					changed = true
				}
			})
		}
		sel("素材", &src, specSources, 140)
		sel("裂解", &fr, titlesOf(specFrags), 130)
		if fr != specFrags[0].title {
			sel("裂解每次模进", &fk, specSteps, 70)
		}
		sel("倒影", &inv, compose.SpecInvAxes, 120)
		sel("模进级数", &sq, specSteps, 70)
		sel("节奏", &vr, specVars, 110)
		sel("终止", &cd, specCads, 100)
		sel("和声区域", &rg, specRegions, 120)
	})
	if !changed {
		return
	}
	sp = compose.SlotSpec{Source: slices.Index(specSources, src), Inv: slices.Index(compose.SpecInvAxes, inv),
		Seq: slices.Index(specSteps, sq) - 3, Var: slices.Index(specVars, vr) - 1, Cadence: slices.Index(specCads, cd),
		Region: slices.Index(specRegions, rg)}
	for _, f := range specFrags {
		if f.title == fr {
			sp.Frag, sp.FragBeats, sp.FragReps = f.frag, f.beats, f.reps
		}
	}
	if sp.Frag > 0 {
		sp.FragK = slices.Index(specSteps, fk) - 3
	}
	d.periods[d.pick] = compose.EditSlot(*s.hum, *p, i, sp, s.filler)
	if d.locked == nil {
		d.locked = map[int]bool{}
	}
	d.locked[i] = true // 手动改过的格在重新生成其余段落时保留
	s.devHarmonize()
}

// titlesOf 取出裂解选项的名称。
func titlesOf(fs []specFrag) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.title
	}
	return out
}
