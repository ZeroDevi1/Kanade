package cantus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kanade/internal/paths"
)

func installFake(t *testing.T, l paths.Layout, m Model) {
	t.Helper()
	if err := os.MkdirAll(m.Dir(l), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(m.Dir(l), completeMarker), []byte("{}"), 0o644)
}

func model(key string) Model {
	for _, m := range Models {
		if m.Key == key {
			return m
		}
	}
	panic(key)
}

func TestParamsValidation(t *testing.T) {
	l := paths.New(t.TempDir())
	v3, sft, v1 := model("Fun-CosyVoice3-0.5B"), model("CosyVoice-300M-SFT"), model("CosyVoice-300M")

	if _, err := (Request{Model: v3, Mode: ModeZeroShot, Text: "x"}).Params(l); err == nil || !strings.Contains(err.Error(), "下载") {
		t.Errorf("not installed: %v", err)
	}
	installFake(t, l, v3)
	installFake(t, l, sft)
	installFake(t, l, v1)

	cases := []struct {
		r    Request
		want string // 错误片段；空表示应成功
	}{
		{Request{Model: v3, Mode: ModeVC, SourceWav: "a"}, "不支持"},
		{Request{Model: v3, Mode: ModeZeroShot, Text: "x", PromptWav: "p.wav"}, "参考音频对应的文本"},
		{Request{Model: v3, Mode: ModeZeroShot, Text: "x", PromptWav: "p.wav", PromptText: "t"}, ""},
		{Request{Model: v3, Mode: ModeCrossLingual, Text: "x", PromptWav: "p.wav"}, ""},
		{Request{Model: v3, Mode: ModeZeroShot, Text: "x", Speaker: &Speaker{ID: "s", Name: "无文本"}}, "没有参考文本"},
		{Request{Model: v3, Mode: ModeInstruct, Text: "x", PromptWav: "p.wav"}, "指令"},
		{Request{Model: sft, Mode: ModeSFT, Text: "x"}, "预训练音色"},
		{Request{Model: sft, Mode: ModeSFT, Text: "x", SpkID: "中文女"}, ""},
		{Request{Model: v1, Mode: ModeVC, PromptWav: "p.wav"}, "源音频"},
		{Request{Model: v1, Mode: ModeVC, PromptWav: "p.wav", SourceWav: "s.wav"}, ""},
		{Request{Model: v1, Mode: ModeZeroShot, Text: "x", PromptWav: "p.wav", PromptText: "t", Speed: 3}, "语速"},
	}
	for _, c := range cases {
		p, err := c.r.Params(l)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s/%s: unexpected %v", c.r.Model.Key, c.r.Mode, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s/%s: err %v, want %q", c.r.Model.Key, c.r.Mode, err, c.want)
		case c.want == "" && p["model_dir"] != c.r.Model.Dir(l):
			t.Errorf("%s/%s: params %v", c.r.Model.Key, c.r.Mode, p)
		}
	}
}

func TestSpeakerStore(t *testing.T) {
	l := paths.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "voice.WAV")
	os.WriteFile(src, []byte("RIFF"), 0o644)

	if _, err := SaveSpeaker(l, " ", "", src); err == nil {
		t.Error("empty name accepted")
	}
	s, err := SaveSpeaker(l, "小明", "你好", src)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(s.Dir(l), "prompt.wav")); string(data) != "RIFF" {
		t.Errorf("audio copy %q", data)
	}
	list, _ := ListSpeakers(l)
	if len(list) != 1 || list[0].Name != "小明" || list[0].ID != s.ID {
		t.Fatalf("list %+v", list)
	}
	if err := DeleteSpeaker(l, "../x"); err == nil {
		t.Error("path traversal accepted")
	}
	if err := DeleteSpeaker(l, s.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := ListSpeakers(l); len(list) != 0 {
		t.Errorf("after delete %+v", list)
	}
}

func TestSFTCheckpoints(t *testing.T) {
	l := paths.New(t.TempDir())
	if _, err := SFTAssembleParams(l, "xuan"); err == nil {
		t.Error("assemble without checkpoints accepted")
	}
	exp := filepath.Join(Root(l), "_sft", "xuan", "exp", "llm")
	os.MkdirAll(exp, 0o755)
	for _, f := range []string{"epoch_0_whole.yaml", "epoch_1_whole.yaml", "epoch_1_whole.pt"} {
		os.WriteFile(filepath.Join(exp, f), nil, 0o644)
	}
	if parts, n := SFTCheckpoints(l, " xuan "); len(parts) != 1 || parts[0] != "llm" || n != 2 {
		t.Errorf("parts %v n %d", parts, n)
	}
	if p, err := SFTAssembleParams(l, "xuan"); err != nil || p["name"] != "xuan" {
		t.Errorf("%v %v", p, err)
	}
}

func TestTextEstimate(t *testing.T) {
	for _, tc := range []struct {
		text       string
		chars, sen int
		secs       float64
	}{
		{"今天天气很好。\n\n明天下雨。", 12, 2, 2.2},
		{"Hello world. How are you?", 21, 2, 5 * secPerWord},
		{"没有句号的一句", 7, 1, 7 * secPerHan},
		{"[laughter]<|zh|>  ", 0, 0, 0},
	} {
		c, n, s := TextEstimate(tc.text, 1)
		if c != tc.chars || n != tc.sen || s < tc.secs-1e-9 || s > tc.secs+1e-9 {
			t.Errorf("%q: %d 字 %d 句 %.2f 秒", tc.text, c, n, s)
		}
	}
	if _, _, s := TextEstimate("一二三四", 2); s != 4*secPerHan/2 {
		t.Errorf("speed 2: %v", s)
	}
}
