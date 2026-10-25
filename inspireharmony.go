package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 灵感页的和声卡片：选风格与方案、查看每个和弦的理由，锁定或替换和弦后其余部分重新求解（score.HarmonizeWith）。

// inspireQualities 是「替换为」可选的和弦性质（score 支持的全部性质，常用的在前）。
var inspireQualities = []string{"maj", "min", "7", "maj7", "min7", "6", "min6", "sus2", "sus4", "dim", "dim7", "hdim7", "aug", "minmaj7"}

// iconLock 标出被锁定的和弦。
var iconLock = icon(`<rect x="3.5" y="7" width="9" height="6.5" rx="1"/><path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2"/>`)

// lockedAt 返回覆盖和弦段 ch 的锁定序号（-1 为没有锁定）。
func (s *inspireState) lockedAt(ch score.Segment) int {
	return slices.IndexFunc(s.locks, func(l score.ChordLock) bool { return l.Start < ch.End-1e-9 && l.End > ch.Start+1e-9 })
}

// setLock 把和弦段 ch 的时段锁定为 root / quality（替换已有的重叠锁定），然后重新配和声。
func (s *inspireState) setLock(ch score.Segment, root int, quality string) {
	s.locks = slices.DeleteFunc(s.locks, func(l score.ChordLock) bool { return l.Start < ch.End-1e-9 && l.End > ch.Start+1e-9 })
	s.locks = append(s.locks, score.ChordLock{Start: ch.Start, End: ch.End, Root: root, Quality: quality})
	s.relock()
}

// relock 在锁定改变后重新配和声，并选中第一个方案（锁定后旧的方案序号不再对应）。
func (s *inspireState) relock() {
	sel := s.chordSel
	s.pick = 0
	s.harmonize()
	s.chordSel = min(sel, len(s.harmonies[0].Chords)-1)
}

// inspireHarmony 是和声卡片：风格、方案（附评分）、和弦序列（锁定的带锁形图标）与选中和弦的理由、锁定与替换。
func (a *app) inspireHarmony(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	s := &a.inspire
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
		if ui.Segmented(c, &s.style, score.HarmonyStyles...).Label("和声风格").Changed() {
			s.pick = 0
			s.harmonize()
			s.devHarmonize() // 乐段的和声用同一种风格
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
				s.dirty = true
			}
		}
		if len(s.locks) > 0 {
			ui.Box(c).Grow(1)
			if ui.Button(c, fmt.Sprintf("解除全部锁定（%d）", len(s.locks))).Clicked() {
				s.locks = nil
				s.relock()
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
				Border(1, tk.CardStroke).Label(ch.Label).Gap(4).AlignItems(ui.Center)
			switch {
			case i == s.chordSel:
				b.Background(tk.SubtlePressed).Border(1, t.Accent)
			case b.Hovered():
				b.Background(tk.SubtleHover)
			}
			b.Children(func() {
				ui.Text(c, score.Symbol(ch, score.SymbolJazz, tonic)).FontSize(14).FontWeight(600)
				if s.lockedAt(ch) >= 0 {
					ui.Icon(c, iconLock).Size(12, 12).TextColor(t.TextMuted)
				}
			})
			if b.Clicked() {
				s.chordSel = i
				s.seekScore(ch.Start)
				s.replaceRoot, s.replaceQuality = "", ""
			}
		}
	})
	if s.chordSel < 0 || s.chordSel >= len(h.Chords) {
		ui.Text(c, "点击和弦查看理由（同时跳到该和弦），可以锁定它或替换成别的和弦，其余部分会重新配。改动乐句后也会重新配和声。").
			FontSize(12).TextColor(t.TextMuted)
		return
	}
	ch := h.Chords[s.chordSel]
	ui.Text(c, h.Reasons[s.chordSel]).FontSize(13)
	a.chordLockRow(c, ch)
}

// chordLockRow 是选中和弦的锁定与替换：锁定当前和弦，或选根音与性质替换并锁定。
func (a *app) chordLockRow(c *ui.Context, ch score.Segment) {
	s := &a.inspire
	flats := score.KeySignature(s.hum.TonicPC, s.hum.Mode) < 0
	roots := make([]string, 12)
	for pc := range roots {
		roots[pc] = score.PitchName(pc, flats)
	}
	qualities := make([]string, len(inspireQualities))
	for i, q := range inspireQualities {
		// 只显示爵士写法的后缀（去掉占位的根音 C）；大三和弦没有后缀。
		qualities[i] = cmp.Or(strings.TrimPrefix(score.Symbol(score.Segment{Label: "C", Root: new(int), Quality: q}, score.SymbolJazz, 0), "C"), "大三")
	}
	if s.replaceRoot == "" {
		s.replaceRoot = roots[*ch.Root]
	}
	if s.replaceQuality == "" {
		if i := slices.Index(inspireQualities, ch.Quality); i >= 0 {
			s.replaceQuality = qualities[i]
		}
	}
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		if li := s.lockedAt(ch); li >= 0 {
			if ui.Button(c, "解除锁定").Clicked() {
				s.locks = slices.Delete(s.locks, li, li+1)
				s.relock()
			}
		} else if ui.Button(c, "锁定此和弦").Tooltip("保留这个和弦，重新配其余部分").Clicked() {
			s.setLock(ch, *ch.Root, ch.Quality)
		}
		ui.Text(c, "替换为").FontSize(12)
		ui.Select(c, &s.replaceRoot, roots).Label("根音").Width(80)
		ui.Select(c, &s.replaceQuality, qualities).Label("性质").Width(110)
		if ui.Button(c, "替换并锁定").Clicked() {
			root, q := slices.Index(roots, s.replaceRoot), slices.Index(qualities, s.replaceQuality)
			if root >= 0 && q >= 0 {
				s.setLock(ch, root, inspireQualities[q])
			}
		}
	})
}
