package timbre

import (
	"errors"
)

// 混音音轨的角色，与引擎 mix.py 的 ROLES 一致。
const (
	RoleVocal   = "vocal"
	RoleHarmony = "harmony"
	RoleInst    = "inst"
	RoleOther   = "other" // 原样相加，不对齐、不加混响
)

// MixTrack 是 timbre.mix 的一条音轨。
type MixTrack struct {
	Path string
	Role string
	// Ref 是对齐响度的参考轨（原曲对应分轨），演唱段 RMS 对齐到它；为空时由引擎按角色智能推荐。
	Ref string
	// GainDB 是在对齐 / 推荐电平之上的偏移。
	GainDB float64
}

// MixOptions 是混音参数。增益都是相对智能推荐电平的偏移，0 即推荐值。
type MixOptions struct {
	VocalGainDB, HarmonyGainDB, InstGainDB float64

	Reverb bool
	// ReverbWetDB 是湿声相对干声的电平（负值），RT60 为混响时间（秒），PreDelayMS 为预延迟（毫秒）。
	ReverbWetDB, RT60, PreDelayMS float64

	// Widen 为 true 时给和声加宽（引擎 mix.py 的 widen）：左右声道各自轻微错位，避免与主唱叠在正中。
	// Width 为侧声道倍数（0 单声道～1 左右各自错位），WidenDelayMS 为和声平均晚于主唱的毫秒数，DetuneCents 为音高摆动幅度。
	Widen                            bool
	Width, WidenDelayMS, DetuneCents float64

	// MP3 为 true 时在 24 bit WAV 之外再导出 320 kbps MP3。
	MP3 bool
}

// 混响默认值与范围，默认值见引擎 mix.py 的 REVERB_DEFAULT（取自手工后期的实测参数）。
const (
	DefaultWetDB       = -12.0
	DefaultRT60        = 1.6
	DefaultPreDelayMS  = 25.0
	MinWetDB, MaxWetDB = -30.0, -3.0
	MinRT60, MaxRT60   = 0.3, 4.0
	MaxPreDelayMS      = 120.0
	// 和声加宽默认值与范围，默认值见引擎 mix.py 的 WIDEN_DEFAULT。延迟超过约 30 ms 会听成"晚了"，失谐超过约 15 cent 会像跑调。
	DefaultWidth        = 0.8
	DefaultWidenDelayMS = 12.0
	DefaultDetuneCents  = 6.0
	MaxWidenDelayMS     = 30.0
	MaxDetuneCents      = 15.0
	// MaxGainOffsetDB 是各轨偏移滑块的范围（±）。
	MaxGainOffsetDB = 12.0
)

// DefaultMix 是智能推荐的混音参数：对齐原曲响度、加混响、只导出 WAV。
func DefaultMix() MixOptions {
	return MixOptions{Reverb: true, ReverbWetDB: DefaultWetDB, RT60: DefaultRT60, PreDelayMS: DefaultPreDelayMS,
		Widen: true, Width: DefaultWidth, WidenDelayMS: DefaultWidenDelayMS, DetuneCents: DefaultDetuneCents}
}

func (m MixOptions) validate() error {
	for _, g := range []float64{m.VocalGainDB, m.HarmonyGainDB, m.InstGainDB} {
		if g < -MaxGainOffsetDB || g > MaxGainOffsetDB {
			return errors.New("音轨增益偏移须在 ±12 dB 之间")
		}
	}
	if m.Reverb && (m.ReverbWetDB < MinWetDB || m.ReverbWetDB > MaxWetDB || m.RT60 < MinRT60 || m.RT60 > MaxRT60 ||
		m.PreDelayMS < 0 || m.PreDelayMS > MaxPreDelayMS) {
		return errors.New("混响参数超出范围")
	}
	if m.Widen && (m.Width < 0 || m.Width > 1 || m.WidenDelayMS < 0 || m.WidenDelayMS > MaxWidenDelayMS ||
		m.DetuneCents < 0 || m.DetuneCents > MaxDetuneCents) {
		return errors.New("和声加宽参数超出范围")
	}
	return nil
}

// Params 返回 timbre.mix 的任务参数；name 是输出文件名（不含扩展名）。
func (m MixOptions) Params(tracks []MixTrack, name string) map[string]any {
	ts := make([]map[string]any, len(tracks))
	for i, t := range tracks {
		ts[i] = map[string]any{"path": t.Path, "role": t.Role, "gain_db": t.GainDB}
		if t.Ref != "" {
			ts[i]["ref"] = t.Ref
		}
	}
	return map[string]any{
		"tracks": ts, "name": name,
		"reverb": map[string]any{"enabled": m.Reverb, "wet_db": m.ReverbWetDB, "rt60": m.RT60, "predelay_ms": m.PreDelayMS},
		"widen":  map[string]any{"enabled": m.Widen, "width": m.Width, "delay_ms": m.WidenDelayMS, "detune_cents": m.DetuneCents},
		"master": map[string]any{"peak_db": -1.0, "mp3": m.MP3},
	}
}
