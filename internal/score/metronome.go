package score

import "math"

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
	for b := range beats {
		AddClick(out, rate, float64(b)*beat, b%s.BeatsPerBar == 0)
	}
	return out
}

// AddClick 在 pcm 的时刻 t（秒）叠加一声节拍器点击；strong 为强拍。超出 pcm 范围的部分丢弃。
func AddClick(pcm []float32, rate int, t float64, strong bool) {
	hz, gain := clickWeakHz, 0.35
	if strong {
		hz, gain = clickStrongHz, 0.6
	}
	at := int(math.Round(t * float64(rate)))
	n := int(clickLen * float64(rate))
	for i := max(0, -at); i < n && at+i < len(pcm); i++ {
		x := float64(i) / float64(rate)
		pcm[at+i] += float32(gain * math.Exp(-x/clickDecay) * math.Sin(2*math.Pi*float64(hz)*x))
	}
}
