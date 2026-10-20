package main

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"

	"kanade/internal/ui/fluent"
	"kanade/internal/ui/waveform"
)

// 测试音频参数：3 分钟单声道 48 kHz，约 864 万采样（35 MB），用于检验大数据量下的波形绘制。
const (
	synthRate    = 48000
	synthSeconds = 180
)

// labState 是 M0 验证页的状态。
type labState struct {
	waveHandle ui.Handle

	// generating 为 true 时后台正在生成测试音频；生成完成后 peaks 与 player 同时赋值。
	generating bool
	peaks      *waveform.Peaks
	player     *audio.Player
	// genCost / peakCost 记录合成与峰值计算耗时，用于评估是否需要放到 Python 端预计算。
	genCost, peakCost time.Duration

	// jobProgress ∈ [0, 1]；jobCancel 非 nil 表示模拟任务进行中。
	jobProgress float64
	jobCancel   context.CancelFunc
	jobStatus   string

	material mygo.Vibrancy
}

// playback 把 audio.Player 适配为波形控件的 Playback。Player 的方法可在任意 goroutine 调用。
type playback struct{ p *audio.Player }

func (pb playback) Progress() (float64, bool) {
	d := pb.p.Duration()
	if d <= 0 {
		return 0, false
	}
	return float64(pb.p.Position()) / float64(d), pb.p.Playing()
}

func (pb playback) Seek(f float64) {
	pb.p.Seek(time.Duration(f * float64(pb.p.Duration())))
}

// generate 在后台合成测试音频并计算峰值，完成后经 post 回到 UI 线程发布结果。
func (a *app) generate() {
	if a.lab.generating {
		return
	}
	a.lab.generating = true
	go func() {
		t0 := time.Now()
		samples := synth(synthRate, synthSeconds)
		t1 := time.Now()
		peaks := waveform.Compute(samples, 1, synthRate, waveform.DefaultFramesPerBucket)
		t2 := time.Now()
		player := audio.NewSound(synthRate, 1, samples).NewPlayer()
		a.post(func() {
			if a.lab.player != nil {
				a.lab.player.Stop()
			}
			a.lab.generating = false
			a.lab.peaks, a.lab.player = &peaks, player
			a.lab.genCost, a.lab.peakCost = t1.Sub(t0), t2.Sub(t1)
		})
	}()
}

// synth 生成带节拍包络的和弦音：120 BPM 每拍一次衰减，每 4 小节换和弦，并叠加缓慢起伏的音量，
// 使波形接近真实音乐的疏密变化。
func synth(rate, seconds int) []float32 {
	chords := [][]float64{
		{261.63, 329.63, 392.00}, // C
		{220.00, 261.63, 329.63}, // Am
		{174.61, 220.00, 261.63}, // F
		{196.00, 246.94, 293.66}, // G
	}
	n := rate * seconds
	out := make([]float32, n)
	beat := float64(rate) / 2 // 120 BPM
	for i := range n {
		t := float64(i) / float64(rate)
		chord := chords[int(float64(i)/(beat*16))%len(chords)]
		phase := math.Mod(float64(i), beat) / beat
		env := math.Exp(-4 * phase)
		swell := 0.55 + 0.45*math.Sin(2*math.Pi*t/23)
		var s float64
		for _, f := range chord {
			s += math.Sin(2 * math.Pi * f * t)
		}
		out[i] = float32(0.22 * env * swell * s)
	}
	return out
}

// startJob 启动一个模拟推理任务：后台 goroutine 每 50 ms 推进一次进度，验证 Window.Update 推送链路。
func (a *app) startJob() {
	if a.lab.jobCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.lab.jobCancel, a.lab.jobProgress, a.lab.jobStatus = cancel, 0, "运行中"
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for step := 1; step <= 100; step++ {
			select {
			case <-ctx.Done():
				a.post(func() { a.lab.jobCancel, a.lab.jobStatus = nil, "已取消" })
				return
			case <-tick.C:
			}
			p := float64(step) / 100
			a.post(func() { a.lab.jobProgress = p })
		}
		a.post(func() { a.lab.jobCancel, a.lab.jobStatus = nil, "已完成" })
	}()
}

// labPage 是 M0 验证页：波形与播放、后台任务进度、窗口材质切换。
func (a *app) labPage(c *ui.Context, tk *fluent.Tokens) {
	t := c.Theme()
	pageHeader(c, "M0 验证", "检验 MyGo Native UI 能否承载 Kanade：Fluent 外观、波形绘制、后台进度。")

	card(c, tk, "波形与播放", func() {
		l := &a.lab
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			label := "生成测试音频"
			if l.generating {
				label = "生成中…"
			}
			if ui.PrimaryButton(c, label).Disabled(l.generating).Clicked() {
				a.generate()
			}
			if l.player != nil {
				label := "播放"
				if l.player.Playing() {
					label = "暂停"
				}
				if ui.Button(c, label).Width(72).Clicked() {
					if l.player.Playing() {
						l.player.Pause()
					} else {
						l.player.Play()
					}
				}
				ui.Textf(c, "%s / %s", clock(l.player.Position()), clock(l.player.Duration())).
					TextColor(t.TextMuted).Font("monospace").FontSize(12)
				if l.player.Playing() {
					// 播放头由波形的 Draw 逐帧刷新；时间文本属于视图，每 100 ms 重建一次即可。
					c.After(100 * time.Millisecond)
				}
			}
			if l.generating {
				ui.Spinner(c)
			}
		})
		var pb waveform.Playback
		if l.player != nil {
			pb = playback{l.player}
		}
		waveform.Show(c, &l.waveHandle, l.peaks, pb).Height(120).Background(tk.Card).
			Border(1, tk.CardStroke).Radius(fluent.OverlayRadius)
		if l.peaks != nil {
			ui.Textf(c, "%d 采样 · %d 个峰值桶 · 合成 %s · 峰值计算 %s · 设置 MYGO_FRAME_STATS=1 运行可在日志查看帧耗时",
				l.peaks.Frames, len(l.peaks.Min), l.genCost.Round(time.Millisecond), l.peakCost.Round(time.Millisecond)).
				FontSize(12).TextColor(t.TextMuted)
		}
	})

	card(c, tk, "后台任务进度", func() {
		l := &a.lab
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if l.jobCancel == nil {
				if ui.PrimaryButton(c, "开始模拟任务").Clicked() {
					a.startJob()
				}
			} else if ui.Button(c, "取消").Clicked() {
				l.jobCancel()
			}
			ui.Textf(c, "%s %.0f%%", l.jobStatus, l.jobProgress*100).TextColor(t.TextMuted)
		})
		ui.Progress(c, l.jobProgress)
	})

	card(c, tk, "窗口材质", func() {
		l := &a.lab
		state := "当前窗口未显示材质（Windows 10、Windows 11 22H2 之前或 Linux），已回退为纯色背景。"
		if c.Vibrancy() {
			state = "当前窗口正在显示材质。"
		}
		ui.Text(c, fmt.Sprintf("平台 %s/%s。%s", runtime.GOOS, runtime.GOARCH, state)).TextColor(t.TextMuted)
		ui.Row(c).Gap(8).Children(func() {
			for _, m := range []struct {
				v     mygo.Vibrancy
				label string
			}{{mygo.VibrancyMica, "Mica"}, {mygo.VibrancyAcrylic, "Acrylic"}, {mygo.VibrancyTabbed, "Mica Alt"}} {
				b := ui.Button(c, m.label)
				if (l.material == "" && m.v == mygo.VibrancyMica) || l.material == m.v {
					b = b.Disabled(true)
				}
				if b.Clicked() && a.win != nil {
					l.material = m.v
					a.win.SetVibrancy(m.v)
				}
			}
		})
	})
}

// clock 把时长格式化为 m:ss.d。
func clock(d time.Duration) string {
	s := d.Seconds()
	return fmt.Sprintf("%d:%04.1f", int(s)/60, math.Mod(s, 60))
}
