package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"

	"kanade/internal/s3sync"
	"kanade/internal/score"
	"kanade/internal/settings"
	"kanade/internal/ui/fluent"
)

// s3State 是云端同步卡片的编辑值与同步任务状态。
type s3State struct {
	// loaded 为 false 时从已保存设置初始化编辑值。timeout / concurrency 用 float64 是因为 ui.NumberInput 只编辑 float64。
	loaded                                              bool
	endpoint, accessKey, secretKey, bucket, region, pfx string
	addressing                                          string // s3Addressing 之一
	skipTLS                                             bool
	timeout, concurrency                                float64
	saved                                               bool
	err                                                 string

	// busy 为 true 时同步进行中，cancel 取消它；progress 为「已完成 / 总数」，result 为最近一次同步的摘要。
	busy     bool
	cancel   context.CancelFunc
	progress string
	result   string
	failed   []string
}

var s3Addressing = []string{"路径式（Path-style）", "虚拟主机式（Virtual-hosted）"}

// s3Remote 是分析记录在存储桶前缀下的相对目录；s3StateFile 是同步状态文件（以 . 开头，不参与同步，也不被当成记录）。
const (
	s3Remote    = "score-history"
	s3StateFile = ".s3sync.json"
)

// s3Card 编辑 S3 兼容存储的连接参数，并手动发起分析记录的增量同步。
func (a *app) s3Card(c *ui.Context, tk *fluent.Tokens) ui.Element {
	t := c.Theme()
	s := &a.settings.s3
	if !s.loaded {
		s.loaded = true
		cfg := a.be.settings.S3
		s.endpoint, s.accessKey, s.secretKey, s.bucket, s.region, s.pfx =
			cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Bucket, cfg.Region, cfg.Prefix
		if s.pfx == "" && !cfg.Configured() {
			s.pfx = "kanade"
		}
		s.addressing = s3Addressing[1]
		if cfg.PathStyle || !cfg.Configured() {
			s.addressing = s3Addressing[0]
		}
		s.skipTLS = cfg.SkipTLSVerify
		s.timeout, s.concurrency = float64(cmp0(cfg.Timeout, s3sync.DefaultTimeout)), float64(cmp0(cfg.Concurrency, s3sync.DefaultConcurrency))
	}
	return card(c, tk, "云端同步（S3）", func() {
		ui.Text(c, "把音乐分析记录同步到 S3 兼容存储（AWS S3、Cloudflare R2、MinIO 等）。仅手动同步，只传输变化的文件；"+
			"不同步分离出的分轨，不删除云端数据。").FontSize(12).TextColor(t.TextMuted)
		ui.Field(c, "Endpoint", func() {
			ui.TextInput(c, &s.endpoint).Label("Endpoint").Placeholder("https://<account>.r2.cloudflarestorage.com").Width(420)
		})
		ui.Field(c, "Access Key", func() { ui.TextInput(c, &s.accessKey).Label("Access Key").Width(420) })
		ui.Field(c, "Secret Key", func() { ui.TextInput(c, &s.secretKey).Label("Secret Key").Password().Width(420) })
		ui.Row(c).Wrap().Gap(8).Children(func() {
			ui.Field(c, "Bucket", func() { ui.TextInput(c, &s.bucket).Label("Bucket").Width(200) })
			ui.Field(c, "Region", func() {
				ui.TextInput(c, &s.region).Label("Region").Placeholder("R2 填 auto，留空为 us-east-1").Width(212)
			})
		})
		ui.Field(c, "路径前缀", func() {
			ui.TextInput(c, &s.pfx).Label("路径前缀").Placeholder("留空为存储桶根目录").Width(420)
		})
		ui.Row(c).Wrap().Gap(12).AlignItems(ui.End).Children(func() {
			ui.Field(c, "寻址方式", func() { ui.Select(c, &s.addressing, s3Addressing).Label("寻址方式").Width(220) })
			ui.Field(c, "超时（秒）", func() { ui.NumberInput(c, &s.timeout, 5, 600, 5).Width(130) })
			ui.Field(c, "并发数", func() { ui.NumberInput(c, &s.concurrency, 1, s3sync.MaxConcurrency, 1).Width(120) })
		})
		ui.Checkbox(c, &s.skipTLS, "跳过 TLS 证书校验（仅用于自签名证书的自建服务）")
		ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "保存").Disabled(s.busy).Clicked() {
				a.saveS3()
			}
			if s.saved {
				ui.Text(c, "已保存").FontSize(12).TextColor(t.Success)
			}
		})
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(t.Danger).Selectable()
		}
		a.s3SyncRow(c)
	})
}

// cmp0 在 v ≤ 0（未设置）时返回 def。
func cmp0(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// config 由编辑值组装配置（不校验）。
func (s *s3State) config() s3sync.Config {
	return s3sync.Config{
		Endpoint: strings.TrimSpace(s.endpoint), AccessKey: strings.TrimSpace(s.accessKey), SecretKey: strings.TrimSpace(s.secretKey),
		Bucket: strings.TrimSpace(s.bucket), Region: strings.TrimSpace(s.region), Prefix: strings.Trim(strings.TrimSpace(s.pfx), "/"),
		PathStyle: s.addressing == s3Addressing[0], SkipTLSVerify: s.skipTLS,
		Timeout: int(s.timeout), Concurrency: int(s.concurrency),
	}
}

// saveS3 校验并保存 S3 配置。全部留空视为清除配置。
func (a *app) saveS3() {
	s := &a.settings.s3
	cfg := s.config()
	if cfg.Endpoint != "" || cfg.AccessKey != "" || cfg.SecretKey != "" || cfg.Bucket != "" {
		if _, err := s3sync.New(cfg); err != nil {
			s.err, s.saved = err.Error(), false
			return
		}
	} else {
		cfg = s3sync.Config{}
	}
	next := a.be.settings
	next.S3 = cfg
	if err := settings.Save(a.be.userData, next); err != nil {
		s.err, s.saved = "保存设置失败："+err.Error(), false
		return
	}
	a.be.settings, s.err, s.saved = next, "", true
}

// s3SyncRow 是「立即同步」按钮、进度与上次结果。同步使用已保存的配置，编辑后须先保存。
func (a *app) s3SyncRow(c *ui.Context) {
	t := c.Theme()
	s := &a.settings.s3
	cfg := a.be.settings.S3
	dirty := cfg != s.config() && !(cfg == s3sync.Config{} && !s.config().Configured())
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
		if s.busy {
			if ui.Button(c, "取消同步").Clicked() && s.cancel != nil {
				s.cancel()
			}
			ui.Spinner(c)
			ui.Text(c, "同步中 "+s.progress).FontSize(12).TextColor(t.TextMuted)
			return
		}
		if ui.PrimaryButton(c, "立即同步分析记录").Disabled(!cfg.Configured() || dirty).Clicked() {
			a.startS3Sync()
		}
		switch {
		case !cfg.Configured():
			ui.Text(c, "填写并保存配置后可同步").FontSize(12).TextColor(t.TextMuted)
		case dirty:
			ui.Text(c, "配置有未保存的修改").FontSize(12).TextColor(t.TextMuted)
		case s.result != "":
			ui.Text(c, s.result).FontSize(12).TextColor(t.TextMuted)
		}
	})
	if len(s.failed) > 0 {
		ui.Text(c, strings.Join(s.failed, "\n")).FontSize(12).TextColor(t.Danger).Selectable()
	}
}

// s3FailedShown 是界面列出的失败文件数上限，其余只计数。
const s3FailedShown = 8

// startS3Sync 在后台按已保存的配置同步分析记录目录；完成后若下载了文件，重新读入音乐分析页的记录。
func (a *app) startS3Sync() {
	s := &a.settings.s3
	cl, err := s3sync.New(a.be.settings.S3)
	if err != nil {
		s.err = err.Error()
		return
	}
	root := score.HistoryDir(a.be.layout)
	ctx, cancel := context.WithCancel(context.Background())
	s.busy, s.cancel, s.progress, s.result, s.failed, s.err = true, cancel, "", "", nil, ""
	go func() {
		res, err := s3sync.Sync(ctx, cl, s3sync.Options{
			Root: root, Remote: s3Remote, StateFile: filepath.Join(root, s3StateFile),
			Include: score.SyncInclude(root), Last: score.IsRecordFile,
			Progress: func(done, total int) {
				a.post(func() { a.settings.s3.progress = fmt.Sprintf("%d / %d", done, total) })
			},
		})
		a.post(func() {
			s := &a.settings.s3
			s.busy, s.cancel = false, nil
			cancel()
			status := "同步完成"
			switch {
			case ctx.Err() != nil:
				status = "已取消"
			case err != nil:
				status, s.err = "同步中断", "同步失败："+err.Error()
			}
			s.result = status + fmt.Sprintf("：上传 %d 个文件，下载 %d 个文件", res.Uploaded, res.Downloaded)
			if n := len(res.Failed); n > 0 {
				s.result += fmt.Sprintf("，%d 个失败", n)
				s.failed = res.Failed[:min(n, s3FailedShown)]
				if n > s3FailedShown {
					s.failed = append(s.failed, fmt.Sprintf("……另有 %d 个", n-s3FailedShown))
				}
			}
			if res.Downloaded > 0 {
				a.reloadScoreHistory()
			}
		})
	}()
}

// reloadScoreHistory 重新读入已保存的分析记录（同步下载了新记录或更新了已有记录），保留本次运行中未保存的分析，
// 并尽量保持正在显示的记录不变。音乐分析页还没打开过时不做任何事，首次进入时自然会读入。
func (a *app) reloadScoreHistory() {
	s := &a.score
	if !s.historyLoaded {
		return
	}
	shown := ""
	if s.shown < len(s.runs) {
		shown = s.runs[s.shown].Snapshot().Files["analysis"]
	}
	var runs []*score.Run
	for _, r := range s.runs {
		if !r.Snapshot().Saved {
			runs = append(runs, r)
		}
	}
	runs = append(runs, score.LoadHistory(score.HistoryDir(a.be.layout))...)
	s.runs, s.shown, s.confirmDelete = runs, 0, false
	for i, r := range runs {
		if shown != "" && r.Snapshot().Files["analysis"] == shown {
			s.shown = i
			break
		}
	}
	s.preview.reset()
}
