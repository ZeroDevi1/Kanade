package stems

import (
	"reflect"
	"testing"
)

func TestResolveAddsPrerequisites(t *testing.T) {
	cases := map[string][]string{
		"dry":     {"vocals", "karaoke", "noreverb", "dry"},
		"other":   {"other"},
		"karaoke": {"vocals", "karaoke"},
	}
	for in, want := range cases {
		if got := Resolve([]string{in}); !reflect.DeepEqual(got, want) {
			t.Errorf("Resolve(%s) = %v, want %v", in, got, want)
		}
	}
	if got := Resolve([]string{"dry", "other"}); len(got) != 5 || got[0] != "other" {
		t.Errorf("all steps: %v", got)
	}
}

func TestOutputTitleLongestSuffix(t *testing.T) {
	cases := map[string]string{
		"/x/歌_vocals.wav":                      "干声",
		"/x/歌_vocals_karaoke.wav":              "主唱（去和声）",
		"/x/歌_vocals_karaoke_noreverb_dry.wav": "主唱干声（降噪）",
		"/x/歌_other.wav":                       "伴奏",
		"/x/歌_vocals_other.wav":                "和声",
		"/x/unknown.wav":                       "",
	}
	for p, want := range cases {
		if got := OutputTitle(p); got != want {
			t.Errorf("OutputTitle(%s) = %q, want %q", p, got, want)
		}
	}
}

func TestChainParamsModelOverrides(t *testing.T) {
	p, err := ChainParams("/a.wav", []string{"karaoke"}, Recommended)
	if err != nil {
		t.Fatal(err)
	}
	// 伴奏不在要执行的步骤中，不传它的覆盖；默认模型也不传。
	want := map[string]any{"vocals": "BS-Roformer-SW.ckpt", "karaoke": "mel_band_roformer_karaoke_gabox_v2.ckpt"}
	if !reflect.DeepEqual(p["models"], want) {
		t.Errorf("models %v", p["models"])
	}
	if p, _ := ChainParams("/a.wav", []string{"dry"}, nil); p["models"] != nil {
		t.Errorf("default chain sent overrides %v", p["models"])
	}
	// 不在可选列表中的模型回退为默认。
	if Steps[0].Pick(map[string]string{"other": "unknown.ckpt"}).File != Steps[0].Model {
		t.Error("unknown model not ignored")
	}
}
