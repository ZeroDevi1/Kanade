package score

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// syntheticExpression 按 vocalSample 的音符造 4 秒的特征：音符内的基频比音符高 cents 音分（octave 为 true 时
// 再高一个八度，模拟八度误差），周期性 0.9；音符外为无声。
func syntheticExpression(a *Analysis, cents float64, octave bool) *Expression {
	const hop = 0.01
	n := 400
	e := &Expression{Version: 1, Hop: hop}
	for i := range n {
		t := float64(i) * hop
		f0, per, loud := 0.0, 0.1, -80.0
		for _, nt := range a.Melody.Notes {
			if t >= nt.Start && t < nt.End {
				f0 = midiFreq(int(nt.Pitch+0.5)) * math.Pow(2, cents/1200)
				if octave {
					f0 *= 2
				}
				per, loud = 0.9, -20+5*math.Sin(t*3)
			}
		}
		e.F0 = append(e.F0, f0)
		e.Periodicity = append(e.Periodicity, per)
		e.Loudness = append(e.Loudness, loud)
		e.Tilt = append(e.Tilt, -10+float64(i%7))
		e.Noise = append(e.Noise, -30+float64(i%5))
	}
	return e
}

// curvePoints 把 SV 曲线拆成（位置, 值）对。
func curvePoints(t *testing.T, c svpCurve) (pos, val []float64) {
	t.Helper()
	if len(c.Points)%2 != 0 {
		t.Fatalf("odd points %d", len(c.Points))
	}
	for i := 0; i < len(c.Points); i += 2 {
		if i > 0 && c.Points[i] <= c.Points[i-2] {
			t.Fatalf("points not increasing at %d: %v", i, c.Points[i-2:i+1])
		}
		pos, val = append(pos, c.Points[i]), append(val, c.Points[i+1])
	}
	return pos, val
}

func TestExpressionCurves(t *testing.T) {
	a := vocalSample()
	notes, tm, err := a.vocalTrack()
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultSVPreset()
	for _, octave := range []bool{false, true} {
		curves := expressionCurves(notes, tm, syntheticExpression(a, 30, octave), p)
		for _, k := range []string{"pitchDelta", "loudness", "breathiness", "tension", "voicing"} {
			if len(curves[k].Points) == 0 {
				t.Fatalf("octave %v: no %s curve", octave, k)
			}
		}
		// 音符内的音高偏差为 +30 音分（八度误差折回）；空隙处补的默认值为 0。
		_, vals := curvePoints(t, curves["pitchDelta"])
		inNote := 0
		for _, v := range vals {
			if v != 0 && math.Abs(v-30) > 0.5 {
				t.Fatalf("octave %v: pitch delta %v", octave, v)
			}
			if v != 0 {
				inNote++
			}
		}
		if inNote < 50 {
			t.Errorf("octave %v: only %d pitch points", octave, inNote)
		}
	}
	// 响度相对中位数、只取一半起伏：在 −6–+3 dB 内；周期性 0.9 时发声不降低。
	curves := expressionCurves(notes, tm, syntheticExpression(a, 0, false), p)
	for _, k := range []string{"loudness", "voicing", "breathiness", "tension"} {
		_, vals := curvePoints(t, curves[k])
		for _, v := range vals {
			switch k {
			case "loudness":
				if v > svLoudMax || v < svLoudMin {
					t.Errorf("loudness %v", v)
				}
			case "voicing":
				if v < 0.4 || v > 1 {
					t.Errorf("voicing %v", v)
				}
			default:
				if math.Abs(v) > 1 {
					t.Errorf("%s %v", k, v)
				}
			}
		}
	}
	// 强度为 0 的曲线不写。
	p.BreathStrength, p.PitchStrength = 0, 0
	curves = expressionCurves(notes, tm, syntheticExpression(a, 0, false), p)
	if _, ok := curves["breathiness"]; ok {
		t.Error("breathiness written with zero strength")
	}
	if _, ok := curves["pitchDelta"]; ok {
		t.Error("pitchDelta written with zero strength")
	}
}

// 指定歌姬与预设：database 原样写入（加普通话覆盖），音色写入 voice，演唱模式关闭继承；
// 有音高曲线时音符改为手动音高、关闭颤音；音符按预设移调。
func TestSVPExport(t *testing.T) {
	a := vocalSample()
	singer := &SVSinger{Name: "Mo Xu", PitchRange: "A#3 - G#4",
		Database: map[string]any{"name": "Mo Xu", "language": "mandarin", "backendType": "SVR3", "version": "201"}}
	preset := DefaultSVPreset()
	preset.AutoTranspose, preset.Transpose = false, 2
	preset.Gender, preset.VocalModes = 0.25, "Clear=60, Belt=20"
	data, err := a.SVPExport("秋天", SVExport{Singer: singer, Preset: preset, Expr: syntheticExpression(a, 30, false)})
	if err != nil {
		t.Fatal(err)
	}
	var p svpProject
	if err := json.Unmarshal(bytes.TrimSuffix(data, []byte{0}), &p); err != nil {
		t.Fatal(err)
	}
	ref := p.Tracks[0].MainRef
	if ref.Database["name"] != "Mo Xu" || ref.Database["version"] != "201" || ref.Database["languageOverride"] != "mandarin" {
		t.Errorf("database %v", ref.Database)
	}
	if ref.Voice["paramGender"] != 0.25 || ref.Voice["vocalModeInherited"] != false {
		t.Errorf("voice %v", ref.Voice)
	}
	if modes, _ := ref.Voice["vocalModeParams"].(map[string]any); modes["Clear"] != 60.0 || modes["Belt"] != 20.0 {
		t.Errorf("vocal modes %v", ref.Voice["vocalModeParams"])
	}
	notes := p.Tracks[0].MainGroup.Notes
	if notes[0].Pitch != 64+2 || notes[0].InstantMode == nil || *notes[0].InstantMode || notes[0].Attributes["dF0Vbr"] != 0.0 {
		t.Errorf("first note %+v", notes[0])
	}
	if len(p.Tracks[0].MainGroup.Parameters["pitchDelta"].Points) == 0 {
		t.Error("no pitch curve")
	}
	// 演唱模式格式错误时返回错误。
	preset.VocalModes = "Clear"
	if _, err := a.SVPExport("秋天", SVExport{Singer: singer, Preset: preset}); err == nil {
		t.Error("bad vocal modes accepted")
	}
	// 不指定歌姬、不给特征时与 SVP 相同：没有曲线，音符不改手动音高。
	data, _ = a.SVP("秋天")
	var plain svpProject
	json.Unmarshal(bytes.TrimSuffix(data, []byte{0}), &plain)
	if plain.Tracks[0].MainGroup.Notes[0].InstantMode != nil || len(plain.Tracks[0].MainGroup.Parameters["pitchDelta"].Points) != 0 {
		t.Error("plain SVP should not carry expression")
	}
}

func TestScanSVSingers(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "databases", "meta")
	os.MkdirAll(meta, 0o755)
	for name, js := range map[string]string{
		"a.json": `{"name": "Kevin", "vendor": "Dreamtonics", "languages": ["english"], "pitchRange": "E3 - E4"}`,
		"b.json": `{"name": "Mo Xu", "vendor": "Dreamtonics", "languages": ["mandarin"], "pitchRange": "A#3 - G#4"}`,
		"c.json": `{"name": "Chen Li", "vendor": "Dreamtonics", "languages": ["mandarin"], "pitchRange": "E3 - D#4"}`,
		"d.json": `{"name": "Aria", "vendor": "X", "languages": ["japanese"]}`,
	} {
		os.WriteFile(filepath.Join(meta, name), []byte(js), 0o644)
	}
	rec := filepath.Join(dir, "recovery", "2026-10-24")
	os.MkdirAll(rec, 0o755)
	// SV 写出的恢复工程可能带 UTF-8 BOM，2.0 以前的格式以 NUL 结尾。
	svp := string([]byte{0xEF, 0xBB, 0xBF}) + `{"tracks": [{"mainRef": {"database": {"name": "Kevin", "language": "english", "backendType": "SVR3", "version": "112"}}, "groups": []}]}` + string([]byte{0})
	os.WriteFile(filepath.Join(rec, "x.svp"), []byte(svp), 0o644)
	got := ScanSVSingers(dir)
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	// 用过的在前，其次会唱普通话的（按名称），最后其他。
	if !reflect.DeepEqual(names, []string{"Kevin", "Chen Li", "Mo Xu", "Aria"}) {
		t.Fatalf("order %v", names)
	}
	if !got[0].Installed() || got[0].Database["version"] != "112" || got[1].Installed() {
		t.Errorf("used flags %+v", got[:2])
	}
	if lo, hi, ok := got[2].Range(); !ok || lo != 58 || hi != 68 {
		t.Errorf("range %d %d %v", lo, hi, ok)
	}
	if ScanSVSingers(filepath.Join(dir, "missing")) != nil {
		t.Error("missing dir should give nil")
	}
}

func TestSuggestTranspose(t *testing.T) {
	a := vocalSample() // 旋律中位数 64–67 附近
	for _, tc := range []struct {
		rng  string
		want int
	}{{"A#3 - G#4", 0}, {"C2 - C3", -24}, {"D5 - D6", 12}, {"", 0}} {
		if got := a.SuggestTranspose(SVSinger{PitchRange: tc.rng}); got != tc.want {
			t.Errorf("range %q: %d, want %d", tc.rng, got, tc.want)
		}
	}
}

func TestSVPresets(t *testing.T) {
	dir := t.TempDir()
	if m, err := LoadSVPresets(dir); err != nil || len(m) != 0 {
		t.Fatalf("empty load %v %v", m, err)
	}
	p := DefaultSVPreset()
	p.Gender, p.VocalModes = -0.2, "Clear=60，Soft=30"
	if err := SaveSVPresets(dir, map[string]SVPreset{"Mo Xu": p}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadSVPresets(dir)
	if err != nil || !reflect.DeepEqual(m["Mo Xu"], p) {
		t.Fatalf("round trip %+v %v", m, err)
	}
	modes, err := p.Modes()
	if err != nil || !reflect.DeepEqual(modes, map[string]float64{"Clear": 60, "Soft": 30}) {
		t.Errorf("modes %v %v", modes, err)
	}
	for _, bad := range []string{"Clear", "Clear=x", "=3", "Clear=200"} {
		if _, err := (SVPreset{VocalModes: bad}).Modes(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
