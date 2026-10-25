package score

import (
	"fmt"
	"math"
)

// HumSettings 是录一段哼唱动机时的节拍设置。节拍器模式下先播放 CountIn 小节预备拍，再播放 Bars 小节节拍器供跟唱；
// 自由模式（Free）不播放节拍器，速度、拍号与小节数由 TranscribeHum 从哼唱估计后写回。
type HumSettings struct {
	// Free 为 true 时自由哼唱：BPM 非 0 时作为速度提示（例如用户把估计的速度减半或加倍），BeatsPerBar 非 0 时固定拍号，
	// 0 为自动估计；CountIn 与 Bars 不使用。
	Free        bool
	BPM         float64 // 每分钟拍数（以四分音符为一拍）
	BeatsPerBar int     // 每小节拍数（2、3、4）
	Bars        int     // 动机的小节数
	CountIn     int     // 预备拍的小节数
	// BarShift 是自由模式下对估计出的小节线的手动调整（拍）：正值把小节线往后挪（例如 1 使第一个音变成一拍弱起），
	// 负值往前挪，0 为不调整。
	BarShift int
	// Compound 为 true 时是复合拍：一拍为附点四分音符（BPM 也按附点四分音符计），每拍三个八分音符；6/8 即
	// BeatsPerBar = 2。自由模式下 Compound 与 BeatsPerBar 同时给出时固定为该复合拍，BeatsPerBar 为 0 时由 TranscribeHum
	// 判断并写回。
	Compound bool
}

// Sub 返回一拍里的八分音符数：简单拍 2，复合拍 3。
func (s HumSettings) Sub() int {
	if s.Compound {
		return 3
	}
	return 2
}

// Grid 返回量化与编辑的最小网格（秒）：十六分音符。
func (s HumSettings) Grid() float64 { return s.Beat() / float64(2*s.Sub()) }

// MeterLabel 返回拍号的写法（4/4、6/8…）。
func (s HumSettings) MeterLabel() string {
	if s.Compound {
		return fmt.Sprintf("%d/8", 3*s.BeatsPerBar)
	}
	return fmt.Sprintf("%d/4", s.BeatsPerBar)
}

// Click 是节拍器的一声：T 为时刻（秒），Level 为 2 强拍（小节首）、1 拍、0 复合拍里拍内的八分音符。
type Click struct {
	T     float64
	Level int
}

// Clicks 返回从第一拍起 bars 小节的节拍器点击：简单拍每拍一声，复合拍每个八分音符一声。
func (s HumSettings) Clicks(bars int) []Click {
	var out []Click
	step, per := s.Beat(), 1
	if s.Compound {
		step, per = s.Beat()/3, 3
	}
	for k := range bars * s.BeatsPerBar * per {
		level := 0
		switch {
		case k%(s.BeatsPerBar*per) == 0:
			level = 2
		case k%per == 0:
			level = 1
		}
		out = append(out, Click{float64(k) * step, level})
	}
	return out
}

// Beat 返回一拍的时长（秒）。
func (s HumSettings) Beat() float64 { return 60 / s.BPM }

// Start 返回动机第一拍在录音时间轴（节拍器播放位置）上的时刻（秒），即预备拍的总时长。
func (s HumSettings) Start() float64 { return float64(s.CountIn*s.BeatsPerBar) * s.Beat() }

// Length 返回动机的总时长（秒，不含预备拍）。
func (s HumSettings) Length() float64 { return float64(s.Bars*s.BeatsPerBar) * s.Beat() }

// 节拍器的咔嗒声：短促的正弦衰减音，强拍高、弱拍低（音高与时长为经验值：在人声频段之上、足够短，
// 戴耳机时不干扰演唱；外放时漏进麦克风的咔嗒声因为没有持续的周期，YIN 判为无声帧）。
const (
	clickStrongHz = 1760
	clickWeakHz   = 1320
	clickLen      = 0.03
	clickDecay    = 0.006 // 指数衰减的时间常数（秒）
)

// MetronomePCM 合成单声道节拍器：预备拍加动机共 CountIn+Bars 小节，每拍一声，强拍音高较高且较响；
// 末尾多留一拍，使最后一拍的演唱也能录完。
func MetronomePCM(s HumSettings, rate int) []float32 {
	beat := s.Beat()
	beats := (s.CountIn + s.Bars) * s.BeatsPerBar
	out := make([]float32, int((float64(beats)+1)*beat*float64(rate)))
	for _, c := range s.Clicks(s.CountIn + s.Bars) {
		AddClick(out, rate, c.T, c.Level)
	}
	return out
}

// AddClick 在 pcm 的时刻 t（秒）叠加一声节拍器点击；level 见 Click。超出 pcm 范围的部分丢弃。
func AddClick(pcm []float32, rate int, t float64, level int) {
	hz, gain := clickWeakHz, 0.35
	switch level {
	case 2:
		hz, gain = clickStrongHz, 0.6
	case 0:
		gain = 0.18 // 复合拍的拍内八分音符：只提示细分，比拍轻
	}
	at := int(math.Round(t * float64(rate)))
	n := int(clickLen * float64(rate))
	for i := max(0, -at); i < n && at+i < len(pcm); i++ {
		x := float64(i) / float64(rate)
		pcm[at+i] += float32(gain * math.Exp(-x/clickDecay) * math.Sin(2*math.Pi*float64(hz)*x))
	}
}
