package vocal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"kanade/internal/score"
)

func TestSingerKey(t *testing.T) {
	for name, want := range map[string]string{
		"Kyomachi Seika AI": "kyomachi seika", "Muxin AI 2": "muxin", "Cheng Xiao 2": "cheng xiao",
		"Oscar Deluxe Edition": "oscar", "Tsurumaki Maki (JPN)": "tsurumaki maki", "ANRI Arcane": "anri", "Mo Xu": "mo xu",
	} {
		if got := singerKey(name); got != want {
			t.Errorf("%q → %q, want %q", name, got, want)
		}
	}
}

// Flat 的声库目录：取版本号最大的 info；"(base)" 等内部模式不列出；名称精确匹配或按键匹配商店目录补上音域。
func TestScanFlatSingers(t *testing.T) {
	dir := t.TempDir()
	write := func(db, file, body string) {
		p := filepath.Join(dir, "databases", db)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, file), []byte(body), 0o644)
	}
	write("Feng Yi", "info.110.nofs", `{"name": "Feng Yi", "version": "110", "language": "mandarin", "phoneset": "xsampa",
		"support_languages": ["english", "mandarin"], "styles": [{"name": "(base)"}, {"name": "Chest"}, {"name": "Soft"}]}`)
	write("AVER AI 2", "info.200.nofs", `{"name": "AVER AI 2", "version": "200", "language": "english", "support_languages": []}`)
	write("AVER AI 2", "info.200a0.nofs", `{"name": "AVER AI 2", "version": "200a0", "language": "english", "support_languages": ["mandarin"]}`)
	write("Broken", "info.1.nofs", `{`)
	catalog := []SVSinger{{Name: "Feng Yi 2", PitchRange: "A#3 - B4", Genres: []string{"Pop"}}, {Name: "AVER AI 2", PitchRange: "C3 - C4"}}
	got := ScanFlatSingers(dir, catalog)
	if len(got) != 2 {
		t.Fatalf("singers %+v", got)
	}
	aver, feng := got[0], got[1]
	if aver.Database["version"] != "200a0" || !reflect.DeepEqual(aver.Languages, []string{"english", "mandarin"}) || aver.PitchRange != "C3 - C4" {
		t.Errorf("aver %+v", aver)
	}
	if !reflect.DeepEqual(feng.Modes, []string{"Chest", "Soft"}) || feng.PitchRange != "A#3 - B4" || !feng.Installed() ||
		feng.Database["phoneset"] != "xsampa" || feng.Languages[0] != "mandarin" {
		t.Errorf("feng %+v", feng)
	}
}

// 推荐：普通话母语、音域合适的在前；音域合适的跨语种歌姬胜过要移两个八度的母语歌姬；不会普通话的最后。
func TestRecommendSingers(t *testing.T) {
	a := vocalSample() // 旋律 E4–F4 附近
	a.Emotion = &score.Emotion{Valence: 0.5, Arousal: 0.8}
	singers := []SVSinger{
		{Name: "EN", Languages: []string{"english"}, PitchRange: "C4 - C5"},
		{Name: "Cross", Languages: []string{"japanese", "mandarin"}, PitchRange: "C4 - C5"},
		{Name: "Fit", Languages: []string{"mandarin"}, PitchRange: "C4 - C5", Modes: []string{"Soft", "Power", "Random1"}},
		{Name: "Low", Languages: []string{"mandarin"}, PitchRange: "C2 - C3"},
	}
	picks := RecommendSingers(a, singers)
	var order []string
	for _, p := range picks {
		order = append(order, singers[p.Index].Name)
	}
	if !reflect.DeepEqual(order, []string{"Fit", "Cross", "Low", "EN"}) {
		t.Fatalf("order %v", order)
	}
	if picks[2].Transpose != -24 || !strings.Contains(strings.Join(picks[0].Reasons, "；"), "Power") {
		t.Errorf("picks %+v", picks[:3])
	}
}
