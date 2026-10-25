package score

import (
	"math"
	"math/rand/v2"
	"testing"
)

// tempoNote 是合成节奏中的一个音：起点与时长以拍为单位，accent 为是否音节起点。
type tempoNote struct {
	beat, dur float64
	midi      int
}

// synthEvents 按逐拍拍长 period(k)（秒）把拍坐标换成录音时间，加 25 ms 的起音抖动，返回节奏事件与真值拍点。
func synthEvents(notes []tempoNote, period func(k int) float64, seed uint64) ([]humEvent, []float64, float64) {
	rng := rand.New(rand.NewPCG(seed, 1))
	last := 0.0
	for _, n := range notes {
		last = max(last, n.beat+n.dur)
	}
	truth := []float64{0.5}
	for k := 0; float64(k) < last+1; k++ {
		truth = append(truth, truth[k]+period(k))
	}
	var ev []humEvent
	for _, n := range notes {
		t := beatTime(truth, n.beat) + rng.NormFloat64()*0.025
		dur := beatTime(truth, n.beat+n.dur) - beatTime(truth, n.beat)
		ev = append(ev, humEvent{t: t, w: 0.6 + 0.4*min(dur/0.4, 1), dur: dur * 0.9, midi: n.midi, onset: true})
	}
	end := beatTime(truth, last)
	return ev, truth, end
}

// run 生成按小节重复的节奏型（长–短…，强拍长音），共 bars 小节，从拍 pickup 起。
func rhythmPattern(pattern []float64, bars, pickup int) []tempoNote {
	var out []tempoNote
	b := float64(pickup)
	midi := 60
	for range bars {
		for i, d := range pattern {
			m := midi + []int{0, 2, 4, 2, 5, 4}[i%6]
			if i == 0 {
				m += 3 // 强拍上跳，形成音高重音
			}
			out = append(out, tempoNote{b, d, m})
			b += d
		}
	}
	return out
}

func TestRhythmFree(t *testing.T) {
	cases := []struct {
		name    string
		notes   []tempoNote
		period  func(int) float64
		bpm     float64 // 真值平均速度
		meter   int
		pickupB int // 第一个音之前补的空拍数（弱起）
	}{
		{"恒速 100 BPM 4/4", rhythmPattern([]float64{1.5, 0.5, 1, 1}, 4, 0), func(int) float64 { return 0.6 }, 100, 4, 0},
		{"90→115 BPM 渐快", rhythmPattern([]float64{1.5, 0.5, 0.5, 0.5, 1}, 6, 0),
			func(k int) float64 { return 60 / (90 + 25*float64(min(k, 24))/24) }, 0, 4, 0},
		{"3/4 长–短–短，80 BPM", rhythmPattern([]float64{2, 0.5, 0.5}, 6, 0), func(int) float64 { return 0.75 }, 80, 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, truth, end := synthEvents(tc.notes, tc.period, 9)
			beats, origin, set, ok := rhythm(ev, end, HumSettings{Free: true})
			if !ok {
				t.Fatal("not ok")
			}
			want := tc.bpm
			if want == 0 {
				want = 60 * float64(len(truth)-1) / (truth[len(truth)-1] - truth[0])
			}
			if r := set.BPM / want; r < 0.93 || r > 1.07 {
				t.Errorf("BPM %.0f, want ≈ %.0f", set.BPM, want)
			}
			if set.BeatsPerBar != tc.meter {
				t.Errorf("meter %d, want %d", set.BeatsPerBar, tc.meter)
			}
			// 每个真值拍点（在音符范围内的）附近都有一个跟踪到的拍点。
			missed := 0
			inRange := 0
			for _, tb := range truth {
				if tb < ev[0].t-0.05 || tb > end {
					continue
				}
				inRange++
				best := math.Inf(1)
				for _, b := range beats {
					best = min(best, math.Abs(b-tb))
				}
				if best > max(0.03, 0.08*0.6) {
					missed++
				}
			}
			if float64(missed) > 0.1*float64(inRange) {
				t.Errorf("%d of %d true beats missed", missed, inRange)
			}
			// 第一个音落在乐谱的第 pickup 拍。
			if u := beatCoord(beats, ev[0].t) - origin; math.Abs(u-float64(tc.pickupB)) > 0.2 {
				t.Errorf("first note at score beat %.2f, want %d", u, tc.pickupB)
			}
		})
	}
}

// 均匀的四分音符流（倍速 / 半速歧义最强）：结果须与真值同速或恰为一半 / 两倍，并且量化后的网格等价。
func TestRhythmAmbiguity(t *testing.T) {
	for _, bpm := range []float64{60, 150} {
		var notes []tempoNote
		for k := range 16 {
			notes = append(notes, tempoNote{float64(k), 1, 60 + k%5})
		}
		ev, _, end := synthEvents(notes, func(int) float64 { return 60 / bpm }, 5)
		_, _, set, _ := rhythm(ev, end, HumSettings{Free: true})
		r := set.BPM / bpm
		if math.Abs(r-1) > 0.05 && math.Abs(r-0.5) > 0.03 && math.Abs(r-2) > 0.1 {
			t.Errorf("%v BPM estimated as %v", bpm, set.BPM)
		}
	}
}

// 自由哼唱端到端：不播放节拍器、速度未知，整理出的音符与真值一致。
func TestTranscribeHumFree(t *testing.T) {
	truth := HumSettings{BPM: 100, BeatsPerBar: 4, Bars: 2}
	notes := []synthNote{{0, 2, 60, 0}, {2, 1, 62, 1}, {3, 0.5, 64, 2}, {3.5, 0.5, 62, 3},
		{4, 2, 67, 4}, {6, 1, 65, 5}, {7, 1, 64, 6}}
	frames := synthFrames(truth, notes, synthOpts{})
	for i := range frames {
		frames[i].T += 0.4 // 录音开始后过一会儿才开口
	}
	h := TranscribeHum(frames, HumSettings{Free: true})
	if h.Settings.BPM < 95 || h.Settings.BPM > 105 || h.Settings.BeatsPerBar != 4 || h.Settings.Bars != 2 {
		t.Fatalf("settings %+v", h.Settings)
	}
	checkNotes(t, h, []wantNote{{0, 2, 60, false}, {2, 3, 62, false}, {3, 3.5, 64, false}, {3.5, 4, 62, false},
		{4, 6, 67, false}, {6, 7, 65, false}, {7, 8, 64, false}})

	// 手动把小节线往后挪一拍：第一个音变成一拍弱起（落在第 4 拍），多出一个小节。
	h = TranscribeHum(frames, HumSettings{Free: true, BarShift: 1})
	if h.Settings.Bars != 3 || math.Abs(h.Notes[0].Start-h.Settings.Beat()*3) > 1e-6 {
		t.Errorf("bar shift: %+v first %+v", h.Settings, h.Notes[0])
	}
}
