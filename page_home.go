package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"kanade/internal/conductor"
	"kanade/internal/ui/fluent"
)

// homeEntry 是主页的一个功能入口；open 在点击时切换到对应页面（及标签）。
type homeEntry struct {
	key, title, desc string
	icon             *ui.SVG
	open             func(a *app)
}

var homeEntries = []homeEntry{
	{"cantus", "语音生成", "CosyVoice 克隆音色朗读文本，或微调自己的说话人", iconCantus, func(a *app) { a.current = "cantus" }},
	{"cover", "一键翻唱", "整首歌或干声 → RVC 转换 → 自动混音", iconTimbre, func(a *app) { a.current, a.timbre.tab = "timbre", timbreTabCover }},
	{"stems", "分轨", "伴奏 / 干声分离、去和声、去混响、降噪", iconStems, func(a *app) { a.current = "stems" }},
	{"score", "音乐分析", "调号、拍号、BPM、和弦谱与情绪", iconScore, func(a *app) { a.current = "score" }},
}

// homeResult 是主页「本次生成」列表的一项。
type homeResult struct {
	r     *result
	page  string // 来源页面名称
	open  func(a *app)
	added time.Time
}

// sessionResults 汇总各页面本次运行生成的结果，最新在前。
func (a *app) sessionResults() []homeResult {
	var out []homeResult
	collect := func(rs *resultSet, page string, open func(a *app)) {
		for _, r := range rs.items {
			r := r
			out = append(out, homeResult{r, page, func(a *app) { open(a); rs.preview.load(a, r.path) }, r.added})
		}
	}
	collect(&a.cantus.results, "语音生成", func(a *app) { a.current = "cantus" })
	collect(&a.timbre.convertOut, "音色转换", func(a *app) { a.current, a.timbre.tab = "timbre", timbreTabConvert })
	collect(&a.timbre.coverOut, "翻唱", func(a *app) { a.current, a.timbre.tab = "timbre", timbreTabCover })
	collect(&a.stems.results, "分轨", func(a *app) { a.current = "stems" })
	slices.SortStableFunc(out, func(x, y homeResult) int { return y.added.Compare(x.added) })
	return out
}

// homePage 是主页：功能入口、进行中的任务、本次生成的结果与引擎环境。
func (a *app) homePage(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	pageHeader(c, "主页", "Kanade · 语音生成、人声转换、分轨与音乐分析。")
	a.homeEntries(c, tk)
	if a.backendUnavailable(c, tk) {
		return
	}

	// 进行中的任务（最多 3 个）。
	var active []conductor.Job
	for _, j := range a.be.cond.Jobs() {
		if !j.State.Finished() {
			active = append(active, j)
		}
	}
	var activeCard func() ui.Element
	if len(active) > 0 {
		activeCard = func() ui.Element {
			return card(c, tk, fmt.Sprintf("进行中（%d）", len(active)), func() {
				for _, j := range active[:min(3, len(active))] {
					ui.Column(c.Key("home-job-" + j.ID)).Gap(4).AlignItems(ui.Stretch).Children(func() {
						ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
							ui.Text(c, j.Engine+" · "+j.Task).FontWeight(600)
							ui.Text(c, jobStateLabel[j.State]).FontSize(12).TextColor(t.TextMuted)
							ui.Text(c, j.Message).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
						})
						ui.Progress(c, j.Progress)
					})
				}
				if ui.Button(c, "查看全部任务").AlignSelf(ui.Start).Clicked() {
					a.current = "tasks"
				}
			})
		}
	}

	// 本次生成的结果（最多 8 个）。
	results := a.sessionResults()
	resultsCard := func() ui.Element {
		return card(c, tk, "本次生成", func() {
			if len(results) == 0 && len(a.score.runs) == 0 {
				ui.Text(c, "还没有生成结果。结果先暂存在系统临时目录，在各页面点「保存…」后才写入磁盘，退出时未保存的会被删除。").
					FontSize(12).TextColor(t.TextMuted)
				return
			}
			unsaved := 0
			for _, h := range results {
				if h.r.saved == "" {
					unsaved++
				}
			}
			if unsaved > 0 {
				ui.Textf(c, "%d 个结果尚未保存，退出 Kanade 后会被删除。", unsaved).FontSize(12).TextColor(t.Warning)
			}
			for _, h := range results[:min(8, len(results))] {
				ui.Row(c.Key("home-result-" + h.r.path)).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Text(c, h.page).FontSize(12).TextColor(t.TextMuted).Width(64)
					title := h.r.title
					if title == "" {
						title = h.r.name
					}
					ui.Text(c, title).Grow(1).SingleLine().Tooltip(h.r.name)
					state := "未保存"
					if h.r.saved != "" {
						state = "已保存"
					}
					ui.Text(c, state).FontSize(12).TextColor(t.TextMuted)
					ui.Text(c, h.added.Format("15:04")).FontSize(12).Font("monospace").TextColor(t.TextMuted)
					if ui.Button(c, "打开").Clicked() {
						h.open(a)
					}
				})
			}
			for i, run := range a.score.runs {
				if st := run.Snapshot(); st.Result != nil {
					ui.Row(c.Key(fmt.Sprintf("home-score-%d", i))).Gap(12).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "音乐分析").FontSize(12).TextColor(t.TextMuted).Width(64)
						name := strings.TrimSuffix(filepath.Base(run.Request.Input), filepath.Ext(run.Request.Input))
						ui.Text(c, name+" · "+st.Result.Key.Label).Grow(1).SingleLine()
						if ui.Button(c, "打开").Clicked() {
							a.current, a.score.shown = "score", i
						}
					})
				}
			}
		})
	}

	// 引擎环境：有未安装或需要更新的引擎时提示。
	var pending []string
	for _, e := range a.be.cond.Engines() {
		if e.Spec.Name == "dev-mock" {
			continue
		}
		if e.State == conductor.EngineMissing || e.State == conductor.EngineFailed {
			label := "未安装"
			if e.Outdated {
				label = "需要更新"
			} else if e.State == conductor.EngineFailed {
				label = "安装失败"
			}
			pending = append(pending, e.Spec.Title+"："+label)
		}
	}
	var engineCard func() ui.Element
	if len(pending) > 0 {
		engineCard = func() ui.Element {
			return card(c, tk, "引擎环境", func() {
				ui.Text(c, strings.Join(pending, " · ")).FontSize(12).TextColor(t.TextMuted)
				ui.Text(c, "首次使用某个功能时会自动安装对应引擎；也可以在设置中提前安装。").FontSize(12).TextColor(t.TextMuted)
				if ui.Button(c, "打开设置").AlignSelf(ui.Start).Clicked() {
					a.current = "settings"
				}
			})
		}
	}
	// 宽屏时两两并排：有进行中的任务时与本次生成一行，否则本次生成与引擎环境一行。
	if activeCard != nil {
		cardRow(c, cardMinWidth, activeCard, resultsCard)
		cardRow(c, cardMinWidth, engineCard)
	} else {
		cardRow(c, cardMinWidth, resultsCard, engineCard)
	}
}

// homeEntryMinWidth 是功能入口卡片的最小宽度：默认窗口中两列，最大化的宽窗口中一行排下全部入口。
const homeEntryMinWidth = 300

// homeEntries 是功能入口卡片，按宽度自动换行排布、每格等宽。
func (a *app) homeEntries(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	ui.Row(c.Key("home-entries")).Wrap().Gap(12).AlignItems(ui.Stretch).Children(func() {
		for _, e := range homeEntries {
			b := ui.ButtonBase(c.Key("home-"+e.key)).Grow(1).Basis(homeEntryMinWidth).MinWidth(0).Padding(16).Radius(fluent.OverlayRadius).
				Border(1, tk.CardStroke).Label(e.title)
			switch {
			case b.Pressed():
				b.Background(tk.SubtlePressed)
			case b.Hovered():
				b.Background(tk.SubtleHover)
			default:
				b.Background(tk.Card)
			}
			b.Children(func() {
				ui.Row(c).Gap(14).AlignItems(ui.Center).Grow(1).Children(func() {
					ui.Icon(c, e.icon).Size(24, 24).Shrink(0).TextColor(t.Accent)
					ui.Column(c).Gap(4).Grow(1).Children(func() {
						ui.Text(c, e.title).FontSize(16).FontWeight(600)
						ui.Text(c, e.desc).FontSize(12).TextColor(t.TextMuted)
					})
				})
			})
			if b.Clicked() {
				e.open(a)
			}
		}
	})
}
