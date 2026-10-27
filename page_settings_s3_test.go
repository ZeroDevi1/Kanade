package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"kanade/engines"
	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/settings"
)

// 云端同步卡片：未配置时同步按钮不可用；保存时校验 Endpoint；全部清空视为清除配置。
func TestSettingsS3Card(t *testing.T) {
	l := paths.New(t.TempDir())
	cond, _ := conductor.New(l, engines.FS, nil)
	a := newApp()
	a.be, a.current = backend{layout: l, cond: cond, userData: t.TempDir()}, "settings"
	tt := ui.NewTester(a.view, 1200, 2400)
	if !tt.HasText("云端同步（S3）") || !tt.HasText("填写并保存配置后可同步") {
		t.Fatalf("texts %q", tt.Texts())
	}

	s := &a.settings.s3
	s.endpoint, s.accessKey, s.secretKey, s.bucket = "ftp://x", "ak", "sk", "bkt"
	a.saveS3()
	if s.err == "" || a.be.settings.S3.Configured() {
		t.Fatalf("invalid endpoint accepted: %q", s.err)
	}
	s.endpoint = "acc.r2.cloudflarestorage.com"
	a.saveS3()
	saved, err := settings.Load(a.be.userData)
	if s.err != "" || err != nil || saved.S3.Bucket != "bkt" || !saved.S3.PathStyle || saved.S3.Prefix != "kanade" {
		t.Fatalf("save: %q %v %+v", s.err, err, saved.S3)
	}
	tt.Frame()
	if tt.HasText("填写并保存配置后可同步") || tt.HasText("配置有未保存的修改") {
		t.Errorf("sync not enabled after save: %q", tt.Texts())
	}

	s.endpoint, s.accessKey, s.secretKey, s.bucket = "", "", "", ""
	a.saveS3()
	if saved, _ := settings.Load(a.be.userData); saved.S3 != (settings.Settings{}).S3 {
		t.Errorf("not cleared: %+v", saved.S3)
	}
}
