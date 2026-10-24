package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"

	"kanade/internal/score"
	"kanade/internal/ui/fluent"
)

// 导出 Synthesizer V 工程的对话框：选择目标软件（本机装的 SV Studio Flat / SV Studio 2）与歌姬（按分析结果推荐排序，
// 附推荐理由），编辑该歌姬的预设（音色、演唱模式、移调与表情强度，可保存；没有保存过时自动按歌曲建议演唱模式），
// 导出时按需从主唱提取表情特征（score.expression）并换算成参数曲线，导出后可直接在目标软件中打开。

// svNoSinger 是「不指定歌姬」的选项：SV 打开工程后由用户选择声库。
const svNoSinger = "不指定（打开后在 SV 中选择）"

// svRecommendTop 是在选择框中标「推荐」的名次数。
const svRecommendTop = 3

// svExportState 是对话框的状态，属于 scoreState。
type svExportState struct {
	open bool
	run  *score.Run // 打开对话框时所选的记录
	name string     // 导出的文件名与音轨名
	// targets 在第一次打开时检测，之后沿用（新装声库后重新打开应用即可）；target 是所选目标的序号。
	targets []score.SVTarget
	scanned bool
	target  int
	// picks 是当前目标的歌姬按推荐排序的结果（打开对话框或换目标时按当前记录重算）；options 是选择框的选项，
	// optionSinger 把选项映射到 targets[target].Singers 的序号。
	picks        []score.SingerPick
	options      []string
	optionSinger map[string]int
	singer       string // 所选选项（options 之一）
	// presets 是已保存的预设（键见 presetKey），preset 是正在编辑的预设，presetFor 是它的键。
	presets   map[string]score.SVPreset
	preset    score.SVPreset
	presetFor string
	transpose float64 // preset.Transpose 的输入框值
	busy      bool    // 正在提取表情或生成工程
	status    string
	err       string
	cancel    context.CancelFunc
	exported  string // 刚导出的文件（提供「在 … 中打开」）；换目标或重新打开对话框时清空
}

var svLanguageTitles = map[string]string{"mandarin": "普通话", "cantonese": "粤语", "japanese": "日语", "english": "英语",
	"korean": "韩语", "spanish": "西班牙语"}

// singerLabel 是歌姬在选择框中的文字：名称、母语、推荐音域；rank 为推荐名次（从 0 起），前几名标「推荐」。
func singerLabel(s score.SVSinger, rank int) string {
	label := s.Name
	if len(s.Languages) > 0 {
		lang := s.Languages[0]
		if t, ok := svLanguageTitles[lang]; ok {
			lang = t
		}
		label += " · " + lang
	}
	if s.PitchRange != "" {
		label += " · " + strings.ReplaceAll(s.PitchRange, " ", "")
	}
	if rank < svRecommendTop {
		label = "★ " + label + " · 推荐"
	}
	return label
}

// openSVExport 打开对话框：第一次时检测目标软件并读取预设；按当前记录重新推荐歌姬并选中第一名。
func (a *app) openSVExport(run *score.Run, name string) {
	d := &a.score.sv
	d.open, d.run, d.name, d.err, d.status, d.exported = true, run, name, "", "", ""
	if !d.scanned {
		d.scanned = true
		d.targets = score.DetectSVTargets()
		var err error
		if d.presets, err = score.LoadSVPresets(a.be.userData); err != nil {
			d.err = "读取歌姬预设失败：" + err.Error()
			d.presets = map[string]score.SVPreset{}
		}
	}
	a.recommendSingers()
}

// recommendSingers 按当前记录给所选目标的歌姬排序，重建选项并选中推荐第一名。
func (a *app) recommendSingers() {
	d := &a.score.sv
	d.picks, d.options, d.optionSinger, d.singer = nil, []string{svNoSinger}, map[string]int{}, svNoSinger
	if res := d.run.Snapshot().Result; res != nil && d.target < len(d.targets) {
		singers := d.targets[d.target].Singers
		d.picks = res.RecommendSingers(singers)
		for rank, p := range d.picks {
			label := singerLabel(singers[p.Index], rank)
			d.options = append(d.options, label)
			d.optionSinger[label] = p.Index
		}
		if len(d.picks) > 0 {
			d.singer = d.options[1]
		}
	}
	a.loadSVPreset()
}

// selectedSinger 返回所选歌姬与它的推荐结果；不指定时为 nil。
func (d *svExportState) selectedSinger() (*score.SVSinger, *score.SingerPick) {
	i, ok := d.optionSinger[d.singer]
	if !ok {
		return nil, nil
	}
	for k := range d.picks {
		if d.picks[k].Index == i {
			return &d.targets[d.target].Singers[i], &d.picks[k]
		}
	}
	return nil, nil
}

// presetKey 是预设的键：SV2 的歌姬直接用名称（与之前保存的预设兼容），其他目标加上目标前缀。
func (d *svExportState) presetKey(s *score.SVSinger) string {
	if s == nil {
		return ""
	}
	if d.targets[d.target].Key == "sv2" {
		return s.Name
	}
	return d.targets[d.target].Key + ":" + s.Name
}

// loadSVPreset 把正在编辑的预设换成所选歌姬已保存的预设；没有保存过时用默认预设，并按歌曲建议演唱模式。
func (a *app) loadSVPreset() {
	d := &a.score.sv
	s, _ := d.selectedSinger()
	key := d.presetKey(s)
	p, ok := d.presets[key]
	if !ok {
		p = score.DefaultSVPreset()
		if res := d.run.Snapshot().Result; s != nil && res != nil {
			p.VocalModes = res.SuggestModes(*s)
		}
	}
	d.preset, d.presetFor, d.transpose = p, key, float64(p.Transpose)
}

// scoreSVDialog 构建导出对话框（关闭时什么也不做）。
func (a *app) scoreSVDialog(c *ui.Context, tk *fluent.Tokens) {
	d := &a.score.sv
	if !d.open || d.run == nil {
		return
	}
	t := c.Theme()
	res := d.run.Snapshot().Result
	if res == nil {
		d.open = false
		return
	}
	ui.Modal(c, &d.open, func() {
		ui.Column(c).Width(620).Gap(12).AlignItems(ui.Stretch).Children(func() {
			ui.Text(c, "导出 Synthesizer V 工程").FontSize(20).FontWeight(600)

			// 目标软件与歌姬（按推荐排序）。
			if len(d.targets) > 1 {
				titles := make([]string, len(d.targets))
				for i, tg := range d.targets {
					titles[i] = tg.Title
				}
				if ui.Segmented(c, &d.target, titles...).Label("目标软件").Changed() {
					d.exported = ""
					a.recommendSingers()
				}
			}
			if ui.Select(c, &d.singer, d.options).Label("歌姬").Changed() {
				a.loadSVPreset()
			}
			s, pick := d.selectedSinger()
			switch {
			case len(d.targets) == 0:
				ui.Text(c, "没有找到 Synthesizer V Studio Flat 或 Synthesizer V Studio 2 的数据目录，只能导出不指定歌姬的工程。").
					FontSize(12).TextColor(t.TextMuted)
			case s != nil:
				ui.Text(c, "推荐理由："+strings.Join(pick.Reasons, "；")).FontSize(12).TextColor(t.TextMuted)
				if !s.Installed() {
					ui.Text(c, "该歌姬没有在本机的 SV 中用过，无法确认已安装；未安装时 SV 打开工程会提示找不到声库。").
						FontSize(12).TextColor(t.Warning)
				}
			}

			// 音色（整首生效，写入音轨的默认值）。
			p := &d.preset
			ui.Text(c, "音色").FontSize(14).FontWeight(600)
			svSlider(c, "性别", &p.Gender, -1, 1, "%+.2f", "负值偏男声（共振峰下移），正值偏女声")
			svSlider(c, "音区偏移", &p.ToneShift, -1200, 1200, "%+.0f 音分", "像在更高（正）或更低（负）的音区演唱，音高不变")
			svSlider(c, "响度", &p.Loudness, -12, 12, "%+.1f dB", "")
			svSlider(c, "气声", &p.Breathiness, -1, 1, "%+.2f", "")
			svSlider(c, "张力", &p.Tension, -1, 1, "%+.2f", "")
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				hint := "按歌姬音域自动移调"
				if s != nil {
					hint += fmt.Sprintf("（建议 %+d 个半音）", res.SuggestTranspose(*s))
				}
				ui.Checkbox(c, &p.AutoTranspose, hint)
				if !p.AutoTranspose {
					if ui.NumberInput(c, &d.transpose, -24, 24, 1).Label("移调（半音）").Width(100).Changed() {
						p.Transpose = int(d.transpose)
					}
				}
			})
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "演唱模式").Width(72)
				ui.TextInput(c, &p.VocalModes).Label("演唱模式").Grow(1).
					Placeholder("可选，例如 Clear=60, Belt=20（名称见 SV 中该声库的演唱模式面板，0–150）")
				if s != nil && len(s.Modes) > 0 && ui.Button(c, "按歌曲建议").Clicked() {
					p.VocalModes = res.SuggestModes(*s)
				}
			})
			if s != nil && len(s.Modes) > 0 {
				ui.Text(c, "该声库的演唱模式："+strings.Join(s.Modes, "、")).FontSize(12).TextColor(t.TextMuted)
			}

			// 表情（从原唱提取）。
			ui.Text(c, "表情强度（从原唱提取，0 为由 SV 自行生成）").FontSize(14).FontWeight(600)
			svStrength(c, "音高", &p.PitchStrength, "音高曲线：转音、颤音与滑音跟随原唱")
			svStrength(c, "动态", &p.DynamicsStrength, "响度曲线")
			svStrength(c, "气声", &p.BreathStrength, "由高频噪声与非周期性推测")
			svStrength(c, "张力", &p.TensionStrength, "由频谱亮度推测")
			svStrength(c, "发声", &p.VoicingStrength, "周期性低的段落（耳语、气声）降低发声")

			// 操作。
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				if s != nil && ui.Button(c, "保存为 "+s.Name+" 的预设").Disabled(d.busy).Clicked() {
					a.saveSVPreset(d.presetKey(s), s.Name)
				}
				if ui.Button(c, "恢复默认").Disabled(d.busy).Clicked() {
					d.preset, d.transpose = score.DefaultSVPreset(), 0
					if s != nil {
						d.preset.VocalModes = res.SuggestModes(*s)
					}
				}
				ui.Box(c).Grow(1)
				if ui.Button(c, "取消").Clicked() {
					if d.cancel != nil {
						d.cancel()
					}
					d.open = false
				}
				if ui.PrimaryButton(c, "导出…").Disabled(d.busy).Clicked() {
					a.exportSV(res, s)
				}
			})
			switch {
			case d.busy:
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Spinner(c)
					ui.Text(c, d.status).FontSize(12).TextColor(t.TextMuted)
				})
			case d.err != "":
				ui.Text(c, d.err).FontSize(12).TextColor(t.Danger).Selectable()
			case d.status != "":
				ui.Text(c, d.status).FontSize(12).TextColor(t.TextMuted)
			}
			// 导出后在目标软件中打开（找到了可执行文件时）。
			if tg := d.currentTarget(); d.exported != "" && tg != nil && tg.Exe != "" {
				if ui.Button(c, "在 "+tg.Title+" 中打开").Clicked() {
					if err := exec.Command(tg.Exe, d.exported).Start(); err != nil {
						d.err = "启动 " + tg.Title + " 失败：" + err.Error()
					}
				}
			}
		})
	})
	if !d.open && d.cancel != nil {
		d.cancel() // Esc 或点击对话框外关闭时取消进行中的提取
	}
}

// svSlider 是一个音色参数的滑块与数值；note 非空时作为提示框说明。
func svSlider(c *ui.Context, label string, v *float64, lo, hi float64, format, note string) {
	t := c.Theme()
	ui.Row(c.Key("sv-" + label)).Gap(12).AlignItems(ui.Center).Children(func() {
		name := ui.Text(c, label).Width(72)
		if note != "" {
			name.Tooltip(note)
		}
		ui.Slider(c, v, lo, hi).Label(label).Grow(1)
		ui.Textf(c, format, *v).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(96)
	})
}

// svStrength 是一项表情强度（0–1）的滑块与百分比。
func svStrength(c *ui.Context, label string, v *float64, note string) {
	t := c.Theme()
	ui.Row(c.Key("sv-strength-" + label)).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).Width(72).Tooltip(note)
		ui.Slider(c, v, 0, 1).Label(label + "强度").Grow(1)
		ui.Textf(c, "%.0f%%", *v*100).FontSize(12).Font("monospace").TextColor(t.TextMuted).Width(96)
	})
}

// currentTarget 返回所选的目标软件；没有检测到任何目标时为 nil。
func (d *svExportState) currentTarget() *score.SVTarget {
	if d.target < len(d.targets) {
		return &d.targets[d.target]
	}
	return nil
}

// saveSVPreset 把正在编辑的预设保存到键 key 下；name 是歌姬名（用于提示）。
func (a *app) saveSVPreset(key, name string) {
	d := &a.score.sv
	if _, err := d.preset.Modes(); err != nil {
		d.err = err.Error()
		return
	}
	next := maps.Clone(d.presets)
	next[key] = d.preset
	if err := score.SaveSVPresets(a.be.userData, next); err != nil {
		d.err = "保存预设失败：" + err.Error()
		return
	}
	d.presets, d.err, d.status = next, "", "已保存 "+name+" 的预设"
}

// exportSV 生成工程并弹出另存对话框。需要表情曲线时先在后台取得表情特征（首次约 10 秒），期间可取消。
func (a *app) exportSV(res *score.Analysis, singer *score.SVSinger) {
	d := &a.score.sv
	if _, err := d.preset.Modes(); err != nil {
		d.err = err.Error()
		return
	}
	p := d.preset
	o := score.SVExport{Singer: singer, Preset: p}
	run, name := d.run, d.name
	finish := func(o score.SVExport) {
		data, err := res.SVPExport(name, o)
		path := filepath.Join(a.be.layout.Scratch, "score.svp")
		if err == nil {
			err = os.WriteFile(path, data, 0o644)
		}
		d.busy = false
		if err != nil {
			d.err = err.Error()
			return
		}
		d.err, d.status = "", ""
		a.saveFileAs(path, name, func(dst string, err error) {
			if err != nil {
				d.err = errText(err)
			} else if dst != "" {
				d.status, d.exported = "已导出到 "+dst, dst
			}
		})
	}
	wantsExpr := p.PitchStrength > 0 || p.DynamicsStrength > 0 || p.BreathStrength > 0 || p.TensionStrength > 0 ||
		p.VoicingStrength > 0
	if !wantsExpr {
		finish(o)
		return
	}
	if a.be.cond == nil {
		d.err = "后端未启动，无法提取主唱表情"
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.busy, d.err, d.status, d.cancel = true, "", "正在提取主唱表情（首次导出这首歌约需 10 秒）…", cancel
	go func() {
		e, err := run.Expression(ctx, a.be.cond)
		a.post(func() {
			d.cancel = nil
			if err != nil {
				d.busy, d.err = false, err.Error()
				return
			}
			o.Expr = e
			finish(o)
		})
	}()
}
