package score

import (
	"math"
	"testing"
)

func ip(v int) *int { return &v }

func strength(root int, q string) float64 {
	x, y := fifthsVector(PitchClasses(root, q))
	return math.Hypot(x, y)
}

func TestFifthsStrength(t *testing.T) {
	// 集中程度的排序：挂留 > 大小三和弦（相同）> 七和弦 > 减三。
	want := map[string]float64{"sus4": 0.911, "maj": 0.644, "min": 0.644, "7": 0.483, "maj7": 0.483, "dim": 0.333}
	for q, w := range want {
		if got := strength(0, q); math.Abs(got-w) > 0.001 {
			t.Errorf("%s strength %.3f, want %.3f", q, got, w)
		}
	}
	// 增三和弦（三音均分五度圈）没有方向。
	if x, y := fifthsVector([]int{0, 4, 8}); math.Hypot(x, y) > 1e-9 {
		t.Errorf("augmented %v %v", x, y)
	}
}

func TestColorOrientationAndSteps(t *testing.T) {
	a := &Analysis{Duration: 8, Downbeats: []float64{0, 4}}
	a.Key.TonicPC, a.Key.Mode = 0, "major"
	a.Chords = []Segment{
		{0, 2, "C", ip(0), "maj"}, {2, 4, "G", ip(7), "maj"}, {4, 6, "F", ip(5), "maj"}, {6, 7, "N", nil, ""}, {7, 8, "C", ip(0), "maj"},
	}
	ca := a.ColorAnalysis()
	if len(ca.Points) != 4 || len(ca.Steps) != 3 {
		t.Fatalf("points %d steps %d", len(ca.Points), len(ca.Steps))
	}
	c, g, f := ca.Points[0], ca.Points[1], ca.Points[2]
	// 主和弦朝正上方；属和弦在升号方向（右），下属和弦在降号方向（左），两者对称。
	if math.Abs(c.X) > 1e-9 || c.Y <= 0 || g.X <= 0 || f.X >= 0 || math.Abs(g.X+f.X) > 1e-9 {
		t.Errorf("C %+v G %+v F %+v", c, g, f)
	}
	// 同为大三和弦：张力差为 0，新鲜感等于色差；C→G 与 G→F 相比，后者跨过主和弦，色差更大。
	if ca.Steps[0].Tension > 1e-9 || ca.Steps[1].Color <= ca.Steps[0].Color {
		t.Errorf("steps %+v", ca.Steps)
	}
	if ca.BarFreshness[0] == 0 || len(ca.BarFreshness) != 2 {
		t.Errorf("bar freshness %v", ca.BarFreshness)
	}
	if sp := ca.Spots(); sp[0].Label != "C" || sp[0].Seconds != 3 {
		t.Errorf("spots %+v", sp)
	}
	// 外圈音名与和弦点同一朝向：朝上的是主三和弦的方向（C、G、E 的合向量），主音 C 在其左侧 45°。
	if ring := a.RingLabels(); ring[0].Label != "C" || math.Abs(ring[0].X+math.Sqrt2/2) > 1e-9 || math.Abs(ring[0].Y-math.Sqrt2/2) > 1e-9 {
		t.Errorf("ring C %+v", ring[0])
	}
}

func TestSpotsMergeAndSmooth(t *testing.T) {
	a := &Analysis{Duration: 6}
	a.Key.TonicPC, a.Key.Mode = 1, "major"
	// Db 与 Ebm 的 f₅ 相同，合并为一个点；时长长的排在前面。
	a.Chords = []Segment{{0, 1, "Ebm", ip(3), "min"}, {1, 4, "Db", ip(1), "maj"}, {4, 6, "Ab", ip(8), "maj"}}
	sp := a.ColorAnalysis().Spots()
	if len(sp) != 2 || sp[0].Label != "Db / Ebm" || sp[0].Seconds != 4 {
		t.Errorf("spots %+v", sp)
	}
	if got := Smooth([]float64{0, 3, 0, 3}, 3); got[0] != 1.5 || got[1] != 1 || got[2] != 2 || got[3] != 1.5 {
		t.Errorf("smooth %v", got)
	}
}
