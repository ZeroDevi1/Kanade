package hum

import (
	"math"
	"math/rand/v2"
	"testing"

	"kanade/internal/score/karaoke"
	"kanade/internal/score/synth"
)

// synthNote 是合成哼唱中的一个音：起点与时长以拍为单位，syl 为音节编号（相同编号的相邻音是转音）。
type synthNote struct {
	start, dur float64
	midi       int
	syl        int
}

// synthOpts 是合成哼唱的扰动。
type synthOpts struct {
	tune           float64 // 整体音高偏移（音分）
	shift          float64 // 整体节奏偏移（秒，负值为抢拍）
	vibAmp, vibHz  float64 // 颤音幅度（音分）与频率
	glide          float64 // 同音节相邻音之间的滑音时长（秒）
	octaveBurstsAt []float64
	drift          float64 // 音高随时间的线性漂移（音分 / 秒，负值为越唱越低）
	jitter         float64 // 每个边界的随机时间偏差的标准差（秒）；紧接的音共用边界
}

// synthFrames 按 Fable 建议的模型合成 10 ms 一帧的哼唱特征：持续段清晰度约 0.9、响度 −20 dBFS；从静音起音时
// 响度 30 ms 爬升；新音节前 40 ms 有 −8 dB 的谷、最后 30 ms 清晰度跌到 0.3（辅音）；休止处无周期、−60 dBFS。
func synthFrames(s HumSettings, notes []synthNote, o synthOpts) []karaoke.KaraokeFrame {
	rng := rand.New(rand.NewPCG(1, 2))
	beat := s.Beat()
	// 各音起止（秒）：节奏抖动按边界施加，前一音终点与后一音起点重合时共用同一偏差，保持紧接。
	bounds := make([][2]float64, len(notes))
	jrng := rand.New(rand.NewPCG(5, 6))
	jit := func() float64 { return max(-2.5*o.jitter, min(2.5*o.jitter, jrng.NormFloat64()*o.jitter)) }
	for k, n := range notes {
		a := s.Start() + n.start*beat + o.shift
		if k > 0 && math.Abs(notes[k-1].start+notes[k-1].dur-n.start) < 1e-9 {
			a = bounds[k-1][1]
		} else if k > 0 {
			a += jit()
		}
		bounds[k] = [2]float64{a, s.Start() + (n.start+n.dur)*beat + o.shift + jit()}
	}
	idx := map[synthNote]int{}
	for k, n := range notes {
		idx[n] = k
	}
	at := func(n synthNote) (float64, float64) {
		b := bounds[idx[n]]
		return b[0], b[1]
	}
	total := s.Start() + s.Length() + beat
	var out []karaoke.KaraokeFrame
	for t := 0.0; t < total; t += humFrame {
		f := karaoke.KaraokeFrame{T: t, DB: -60, Clarity: 0.1}
		for k, n := range notes {
			a, b := at(n)
			if t < a || t >= b {
				continue
			}
			c := 100*float64(n.midi) + o.tune + o.vibAmp*math.Sin(2*math.Pi*o.vibHz*(t-a)) + rng.NormFloat64()*8 +
				o.drift*(t-s.Start())
			f.DB, f.Clarity = -20, 0.9+rng.NormFloat64()*0.03
			prevAdj := k > 0 && math.Abs(func() float64 { _, pb := at(notes[k-1]); return pb }()-a) < 1e-9
			if prevAdj && notes[k-1].syl == n.syl && t-a < o.glide {
				p := 100*float64(notes[k-1].midi) + o.tune + o.drift*(t-s.Start())
				c = p + (c-p)*(t-a)/o.glide
			}
			if !prevAdj && t-a < 0.03 {
				f.DB = -45 + 25*(t-a)/0.03
			}
			if k+1 < len(notes) && notes[k+1].syl != n.syl {
				if na, _ := at(notes[k+1]); math.Abs(na-b) < 1e-9 {
					if b-t <= 0.04 {
						f.DB = -28
					}
					if b-t <= 0.03 {
						f.Clarity = 0.3
					}
				}
			}
			f.F0 = 440 * math.Pow(2, (c-6900)/1200)
			for _, burst := range o.octaveBurstsAt {
				if t >= burst && t < burst+0.02 {
					f.F0 *= 2
				}
			}
		}
		out = append(out, f)
	}
	return out
}

// wantNote 是期望的量化结果：起止以拍为单位。
type wantNote struct {
	start, end float64
	midi       int
	slur       bool
}

func checkNotes(t *testing.T, h Hum, want []wantNote) {
	t.Helper()
	beat := h.Settings.Beat()
	if len(h.Notes) != len(want) {
		t.Fatalf("got %d notes %+v, want %d", len(h.Notes), h.Notes, len(want))
	}
	for i, w := range want {
		n := h.Notes[i]
		if math.Abs(n.Start/beat-w.start) > 1e-6 || math.Abs(n.End/beat-w.end) > 1e-6 || int(n.Pitch) != w.midi || n.Slur != w.slur {
			t.Errorf("note %d = {%.3f %.3f %v %v} beats, want %+v", i, n.Start/beat, n.End/beat, n.Pitch, n.Slur, w)
		}
	}
}

var testHum = HumSettings{BPM: 120, BeatsPerBar: 4, Bars: 2, CountIn: 1}

func TestTranscribeHum(t *testing.T) {
	quarters := []synthNote{{0, 1, 60, 0}, {1, 1, 62, 1}, {2, 1, 64, 2}, {3, 1, 65, 3}}
	quarterWant := []wantNote{{0, 1, 60, false}, {1, 2, 62, false}, {2, 3, 64, false}, {3, 4, 65, false}}
	cases := []struct {
		name  string
		notes []synthNote
		opts  synthOpts
		want  []wantNote
	}{
		{"平直四分音符，不同音节", quarters, synthOpts{}, quarterWant},
		{"颤音 ±100 音分 6 Hz 的二分音符", []synthNote{{0, 2, 67, 0}}, synthOpts{vibAmp: 100, vibHz: 6},
			[]wantNote{{0, 2, 67, false}}},
		{"同音节滑音 C→E", []synthNote{{0, 1, 60, 0}, {1, 1, 64, 0}}, synthOpts{glide: 0.08},
			[]wantNote{{0, 1, 60, false}, {1, 2, 64, true}}},
		{"同音节十六分音符转音", []synthNote{{0, 0.25, 60, 0}, {0.25, 0.25, 62, 0}, {0.5, 0.25, 64, 0}, {0.75, 0.25, 62, 0}, {1, 1, 67, 1}},
			synthOpts{glide: 0.02},
			[]wantNote{{0, 0.25, 60, false}, {0.25, 0.5, 62, true}, {0.5, 0.75, 64, true}, {0.75, 1, 62, true}, {1, 2, 67, false}}},
		{"同音节中 50 ms 的经过音并入", []synthNote{{0, 1, 60, 0}, {1, 0.1, 62, 0}, {1.1, 0.9, 64, 0}}, synthOpts{},
			[]wantNote{{0, 1, 60, false}, {1, 2, 64, true}}},
		{"整体偏高 40 音分", quarters, synthOpts{tune: 40}, quarterWant},
		{"整体抢拍 40 ms", quarters, synthOpts{shift: -0.04}, quarterWant},
		{"八度突跳被折回", quarters, synthOpts{octaveBurstsAt: []float64{2.7, 3.3}}, quarterWant},
		{"八分三连音与直拍", []synthNote{{0, 1.0 / 3, 60, 0}, {1.0 / 3, 1.0 / 3, 62, 1}, {2.0 / 3, 1.0 / 3, 64, 2}, {1, 1, 65, 3}}, synthOpts{},
			[]wantNote{{0, 1.0 / 3, 60, false}, {1.0 / 3, 2.0 / 3, 62, false}, {2.0 / 3, 1, 64, false}, {1, 2, 65, false}}},
		{"休止分开的乐句", []synthNote{{0, 1, 60, 0}, {2, 1, 64, 1}}, synthOpts{},
			[]wantNote{{0, 1, 60, false}, {2, 3, 64, false}}},
		{"真实的八度大跳与十六分音符的大七度跳不被当成倍频错误折回",
			[]synthNote{{0, 1, 57, 0}, {1, 1, 69, 1}, {2, 1, 57, 2}, {3, 0.25, 57, 3}, {3.25, 0.25, 68, 4}, {3.5, 0.5, 60, 5}},
			synthOpts{vibAmp: 30, vibHz: 5.5},
			[]wantNote{{0, 1, 57, false}, {1, 2, 69, false}, {2, 3, 57, false}, {3, 3.25, 57, false}, {3.25, 3.5, 68, false}, {3.5, 4, 60, false}}},
		{"越唱越高（偏高 30 音分起，每秒再高 15 音分）带颤音的长音",
			[]synthNote{{0, 2, 60, 0}, {2, 2, 64, 1}, {4, 2, 62, 2}, {6, 2, 67, 3}}, synthOpts{tune: 30, drift: 15, vibAmp: 30, vibHz: 5.5},
			[]wantNote{{0, 2, 60, false}, {2, 4, 64, false}, {4, 6, 62, false}, {6, 8, 67, false}}},
		{"边界前后偏 30 ms 的八分音符节奏不被量化成三连音或附点十六分",
			[]synthNote{{0, 1.5, 60, 0}, {1.5, 0.5, 62, 1}, {2, 0.5, 64, 2}, {2.5, 1.5, 65, 3}, {4.5, 1, 67, 4}, {5.5, 0.5, 65, 5}, {6, 2, 64, 6}},
			synthOpts{jitter: 0.03, vibAmp: 30, vibHz: 5.5},
			[]wantNote{{0, 1.5, 60, false}, {1.5, 2, 62, false}, {2, 2.5, 64, false}, {2.5, 4, 65, false}, {4.5, 5.5, 67, false}, {5.5, 6, 65, false}, {6, 8, 64, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := TranscribeHum(synthFrames(testHum, tc.notes, tc.opts), testHum)
			checkNotes(t, h, tc.want)
			// 有漂移时 Tuning 是漂移曲线在有声帧上的平均（线性漂移约为中点的偏移）。
			if want := tc.opts.tune + tc.opts.drift*testHum.Length()/2; want != 0 && math.Abs(h.Tuning-want) > 8 {
				t.Errorf("tuning %.1f, want %.0f", h.Tuning, want)
			}
			// 4 秒内每秒高 15 音分：开头到结尾的漂移约 60 音分（曲线经 1 秒的高斯窗平滑，两端略收窄）。
			if want := tc.opts.drift * testHum.Length(); want != 0 && math.Abs(h.DriftSpan()-want) > 20 {
				t.Errorf("drift span %.1f, want about %.0f", h.DriftSpan(), want)
			}
			// 整体抢拍 / 拖拍由节拍跟踪吸收：乐谱第一拍对应的录音时刻随之移动。
			if want := testHum.Start() + tc.opts.shift; math.Abs(h.RecTime(0)-want) > 0.025 { // 点击的伪起音会把拍点稍微拉回节拍器
				t.Errorf("first beat at %.3f s, want %.3f", h.RecTime(0), want)
			}
		})
	}
}

// 没有录到声音时没有音符；随机乐句的输出满足基本性质：不重叠、起止在网格上、在动机范围内。
func TestTranscribeHumProperties(t *testing.T) {
	if h := TranscribeHum(synthFrames(testHum, nil, synthOpts{}), testHum); h.Voiced != 0 || len(h.Notes) != 0 {
		t.Fatalf("silence: %+v", h)
	}
	// 自由模式没有录到音符时，节拍设置仍合法（速度与拍号为默认值）。
	for _, frames := range [][]karaoke.KaraokeFrame{nil, synthFrames(testHum, nil, synthOpts{})} {
		h := TranscribeHum(frames, HumSettings{Free: true})
		if h.Settings.BPM <= 0 || h.Settings.BeatsPerBar <= 0 || h.Settings.Bars <= 0 || math.IsNaN(h.Settings.Length()) {
			t.Fatalf("free silence settings %+v", h.Settings)
		}
	}
	rng := rand.New(rand.NewPCG(7, 8))
	for trial := range 30 {
		var notes []synthNote
		for b, syl := 0.0, 0; b < 7.5; syl++ {
			d := []float64{0.5, 1, 1.5, 2}[rng.IntN(4)]
			notes = append(notes, synthNote{b, min(d, 8-b), 55 + rng.IntN(17), syl})
			b += d
			if rng.IntN(4) == 0 {
				b += 0.5 // 休止
			}
		}
		h := TranscribeHum(synthFrames(testHum, notes, synthOpts{vibAmp: 30, vibHz: 5.5}), testHum)
		beat := h.Settings.Beat()
		for i, n := range h.Notes {
			onGrid := func(v float64) bool {
				return gridDist(v, beat/4) < 1e-6 || gridDist(v, beat/3) < 1e-6
			}
			if n.End <= n.Start || !onGrid(n.Start) || !onGrid(n.End) || n.Start < 0 || n.End > testHum.Length()+1e-9 ||
				(i > 0 && n.Start < h.Notes[i-1].End-1e-9) {
				t.Fatalf("trial %d note %d %+v in %+v", trial, i, n, h.Notes)
			}
		}
		if len(h.Notes) != len(notes) {
			t.Errorf("trial %d: %d notes, want %d", trial, len(h.Notes), len(notes))
		}
	}
}

func TestFillLyricsAndAnalysis(t *testing.T) {
	h := TranscribeHum(synthFrames(testHum, []synthNote{{0, 0.5, 60, 0}, {0.5, 0.5, 62, 0}, {1, 0.5, 64, 1}, {2, 0.5, 65, 2}, {4, 2, 67, 3}},
		synthOpts{glide: 0.02}), testHum)
	FillLyrics(h.Notes, 0, testHum.Beat())
	var got []string
	for _, n := range h.Notes {
		got = append(got, n.Lyric)
	}
	// 第 1 音节两个八分音符共一拍（长音字），连音不填字；第 2 音节为八分音符、其后的休止不到一拍（短音字）；
	// 第 3 音节后有一拍半的休止与最后一个音节都用句尾字。
	want := []string{"啦", "", "哒", "啦", "啦"}
	if len(got) != len(want) {
		t.Fatalf("lyrics %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lyrics %q, want %q", got, want)
			break
		}
	}
	a := h.Analysis()
	if len(a.Downbeats) != 2 || len(a.Measures()) != 2 || len(a.Beats) != 9 || a.Key.Label == "" || len(a.Melody.Notes) != 5 {
		t.Fatalf("analysis %+v", a)
	}
	if b, err := synth.MIDI(a); err != nil || len(b) == 0 {
		t.Fatalf("midi: %v", err)
	}
}

func TestMetronomePCM(t *testing.T) {
	pcm := MetronomePCM(testHum, 8000)
	if want := int((3*4 + 1) * testHum.Beat() * 8000); len(pcm) != want {
		t.Fatalf("len %d, want %d", len(pcm), want)
	}
	if pcm[1] == 0 || pcm[int(testHum.Beat()*8000)+1] == 0 || pcm[int(testHum.Beat()*8000/2)] != 0 {
		t.Errorf("clicks not on beats")
	}
}

// 任意帧（噪声、提前停止、只有预备拍）都不应让整理崩溃。
func TestTranscribeHumRobust(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for trial := range 200 {
		n := rng.IntN(600)
		frames := make([]karaoke.KaraokeFrame, n)
		for i := range frames {
			f := karaoke.KaraokeFrame{T: float64(i) * humFrame, DB: -70 + 60*rng.Float64(), Clarity: rng.Float64()}
			if rng.IntN(3) > 0 {
				f.F0 = 60 + 1000*rng.Float64()
			}
			frames[i] = f
		}
		for _, set := range []HumSettings{testHum, {Free: true}} {
			h := TranscribeHum(frames, set)
			FillLyrics(h.Notes, trial%len(FillerSets), h.Settings.Beat())
			_ = h.Analysis()
			if l := h.Settings.Length(); math.IsNaN(l) || l <= 0 {
				t.Fatalf("trial %d settings %+v", trial, h.Settings)
			}
		}
	}
}

// 随机乐句在音准漂移与节奏抖动下的准确率（相对第一个音比较音高，整句移调不算错）。阈值比当前结果略低，
// 防止调参时某一类扰动的准确率明显退化：漂移在切分前扣除、拍位先验与孤立三分点代价都由这里把关。
func TestTranscribeHumAccuracy(t *testing.T) {
	cases := []struct {
		name               string
		durs               []float64 // 时值候选（拍）
		opts               synthOpts
		minPitch, minOnset float64 // 音数正确的乐句里音高、起点正确的比例下限
	}{
		{"八分音符节奏，越唱越低", []float64{0.5, 1, 1.5, 2}, synthOpts{drift: -12}, 1, 1},
		{"八分音符节奏，偏高并越唱越高", []float64{0.5, 1, 1.5, 2}, synthOpts{tune: 30, drift: 15}, 1, 1},
		{"八分音符节奏，边界抖动 30 ms", []float64{0.5, 1, 1.5, 2}, synthOpts{jitter: 0.03}, 1, 0.98},
		{"含十六分音符，越唱越高", []float64{0.25, 0.5, 0.75, 1, 1.5}, synthOpts{drift: 15}, 0.99, 1},
		{"含十六分音符，边界抖动 30 ms", []float64{0.25, 0.5, 0.75, 1, 1.5}, synthOpts{jitter: 0.03}, 0.97, 0.92},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(11, 12))
			o := tc.opts
			o.vibAmp, o.vibHz = 30, 5.5
			pitch, onset, total, phrases := 0, 0, 0, 0
			for range 40 {
				var notes []synthNote
				for b, syl := 0.0, 0; b < 7.5; syl++ {
					d := tc.durs[rng.IntN(len(tc.durs))]
					notes = append(notes, synthNote{b, min(d, 8-b), 57 + rng.IntN(14), syl})
					b += d
					if rng.IntN(4) == 0 {
						b += 0.5
					}
				}
				h := TranscribeHum(synthFrames(testHum, notes, o), testHum)
				if len(h.Notes) != len(notes) {
					continue
				}
				phrases++
				beat, off := h.Settings.Beat(), int(h.Notes[0].Pitch)-notes[0].midi
				for i, n := range notes {
					total++
					if int(h.Notes[i].Pitch)-off == n.midi {
						pitch++
					}
					if math.Abs(h.Notes[i].Start/beat-n.start) < 1e-6 {
						onset++
					}
				}
			}
			if phrases < 36 {
				t.Errorf("only %d/40 phrases with the right note count", phrases)
			}
			if p, o := float64(pitch)/float64(total), float64(onset)/float64(total); p < tc.minPitch || o < tc.minOnset {
				t.Errorf("pitch %.3f (min %.2f) onset %.3f (min %.2f) over %d notes", p, tc.minPitch, o, tc.minOnset, total)
			}
		})
	}
}
