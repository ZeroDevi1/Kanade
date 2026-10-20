package dataset

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseAndInspect(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "XingTong")
	os.MkdirAll(dir, 0o755)
	for _, f := range []string{"a.wav", "b.wav", "c.flac", "notes.txt"} {
		write(t, filepath.Join(dir, f), "x")
	}
	write(t, filepath.Join(dir, "XingTong.list"), "\ufeff# 注释\n"+
		`C:\AIGC\XingTong\a.wav|xt|ZH|你好`+"\n"+
		"/mnt/c/x/b.mp3|xt|ZH|文本|含竖线\n"+ // stem 匹配 b.wav；多余的 | 并入文本
		"\n"+
		"c.flac|xt|JA|\n"+
		"missing.wav|xt|ZH|没有音频\n")

	entries, err := Parse(filepath.Join(dir, "XingTong.list"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || entries[1].Text != "文本|含竖线" || entries[0].Basename() != "a.wav" {
		t.Fatalf("entries %+v", entries)
	}

	s, err := Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{Dir: dir, Audio: 3, List: filepath.Join(dir, "XingTong.list"), Listed: 4, Matched: 3, WithText: 3, Missing: []string{"missing.wav"}}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("summary %+v, want %+v", s, want)
	}
}

func TestFindListRules(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Xuan")
	os.MkdirAll(dir, 0o755)
	if FindList(dir) != "" {
		t.Error("empty dir has a list")
	}
	write(t, filepath.Join(dir, "other.list"), "")
	if got := FindList(dir); filepath.Base(got) != "other.list" {
		t.Errorf("single list: %q", got)
	}
	write(t, filepath.Join(dir, "second.list"), "")
	if got := FindList(dir); got != "" {
		t.Errorf("ambiguous lists picked %q", got)
	}
	write(t, filepath.Join(dir, "xuan.list"), "")
	// 不区分大小写的文件系统（macOS / Windows 默认）上 Xuan.list 与 xuan.list 是同一个文件。
	if got := FindList(dir); !strings.EqualFold(filepath.Base(got), "xuan.list") {
		t.Errorf("lowercase same-name: %q", got)
	}
}
