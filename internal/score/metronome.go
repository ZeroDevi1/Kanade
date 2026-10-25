package score

import "math"

// HumSettings 是录一段哼唱动机时的节拍设置。录音时先播放 CountIn 小节预备拍，再播放 Bars 小节节拍器供跟唱。
type HumSettings struct {
	BPM         float64 // 每分钟拍数（以四分音符为一拍）
	BeatsPerBar int     // 每小节拍数（2、3、4）
	Bars        int     // 动机的小节数
	CountIn     int     // 预备拍的小节数
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
	n := int(clickLen * float64(rate))
	for b := range beats {
		hz, gain := clickWeakHz, 0.35
		if b%s.BeatsPerBar == 0 {
			hz, gain = clickStrongHz, 0.6
		}
		at := int(float64(b) * beat * float64(rate))
		for i := 0; i < n && at+i < len(out); i++ {
			t := float64(i) / float64(rate)
			out[at+i] = float32(gain * math.Exp(-t/clickDecay) * math.Sin(2*math.Pi*float64(hz)*t))
		}
	}
	return out
}
