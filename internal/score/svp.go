package score

import (
	"encoding/json"
	"maps"
	"math"
)

// 导出 Synthesizer V Studio 工程（.svp，UTF-8 JSON）：一条人声音轨，音符与歌词来自旋律线，速度与拍号同 MIDI 导出。
// 格式依据 LibreSVIP 的 svp 插件（libresvip/plugins/svp/model.py）：时间单位为 blick，每四分音符 705600000；
// 拍号按小节序号（index）记录。版本号写 135（1.10.0–1.11.2 的格式），Synthesizer V Studio 1.10 起与 2.x 都能打开；
// 2.0 以前的版本要求文件以一个 NUL 字节结尾，2.x 读取时忽略它。
// 歌词按普通话解释（languageOverride），连音写 "-"，没有歌词的音写 Synthesizer V 新建音符的默认歌词 "la"。
//
// SVPExport 另可指定歌姬（写入 database，SV 打开时加载该声库）、歌姬预设（音色默认值、演唱模式、移调）与
// 主唱表情特征（换算成参数曲线，见 svexpr.go）。音高曲线有两种写法：
//   - 覆盖式（没有基准渲染时）：音符设 instantMode=false（与 LibreSVIP 导出 1.10–1.11 格式时相同），SV 按手动音高
//     渲染，pitchDelta 是相对音符音高的偏差；音符的滑音缩到最短、颤音深度设为 0，避免与原唱曲线里的滑音、颤音叠加。
//     这会丢掉 SV 自己的起音、过渡与颤音，实测听起来不自然（docs/roadmap.md 第 3 节）。
//   - 引导式（给了基准渲染 Base 时，见 svcompare.go）：音符保持 AI 音高，pitchDelta 只补原唱与基准差得多的地方。
//
// Breaths 为 true 时在乐句前（停顿 ≥ svBreathMinGap）插入换气音符：音素写 br（SV 的换气音素，部分声库没有，
// 没有时该音符不发声），时长取停顿减去 svBreathMargin、不超过 svBreathMax，紧接在乐句第一个音之前。

const (
	// svpBlicksPerTick 是每个 midiPPQ tick 的 blick 数（705600000 / 480）。
	svpBlicksPerTick = 705600000 / midiPPQ
	// svpVersion 是写入的工程格式版本，见文件头说明。
	svpVersion = 135
)

type svpCurve struct {
	Mode   string    `json:"mode"`
	Points []float64 `json:"points"` // 偏移（blick）与值交替排列；导出时为空，即全程默认值
}

type svpNote struct {
	Onset      int64          `json:"onset"`
	Duration   int64          `json:"duration"`
	Lyrics     string         `json:"lyrics"`
	Phonemes   string         `json:"phonemes"`
	Pitch      int            `json:"pitch"`
	Detune     int            `json:"detune"`
	Attributes map[string]any `json:"attributes"`
	// InstantMode 为 false 时 SV 不为该音符自动生成音高（手动音高）；nil 为不写，沿用 SV 的默认。
	InstantMode *bool `json:"instantMode,omitempty"`
}

type svpGroup struct {
	Name       string              `json:"name"`
	UUID       string              `json:"uuid"`
	Parameters map[string]svpCurve `json:"parameters"`
	VocalModes map[string]svpCurve `json:"vocalModes"`
	Notes      []svpNote           `json:"notes"`
}

type svpRef struct {
	GroupID            string   `json:"groupID"`
	BlickAbsoluteBegin int64    `json:"blickAbsoluteBegin"`
	BlickAbsoluteEnd   int64    `json:"blickAbsoluteEnd"`
	BlickOffset        int64    `json:"blickOffset"`
	PitchOffset        int      `json:"pitchOffset"`
	IsInstrumental     bool     `json:"isInstrumental"`
	SystemPitchDelta   svpCurve `json:"systemPitchDelta"`
	// Database 是声库：name、language、phoneset、languageOverride、phonesetOverride、backendType、version；
	// 用 map 以便原样写入恢复工程中读到的字段（SVSinger.Database）。
	Database   map[string]any `json:"database"`
	Dictionary string         `json:"dictionary"`
	Voice      map[string]any `json:"voice"`
}

type svpMixer struct {
	GainDecibel float64 `json:"gainDecibel"`
	Pan         float64 `json:"pan"`
	Mute        bool    `json:"mute"`
	Solo        bool    `json:"solo"`
	Display     bool    `json:"display"`
}

type svpTrack struct {
	Name          string   `json:"name"`
	DispColor     string   `json:"dispColor"`
	DispOrder     int      `json:"dispOrder"`
	RenderEnabled bool     `json:"renderEnabled"`
	Mixer         svpMixer `json:"mixer"`
	MainGroup     svpGroup `json:"mainGroup"`
	MainRef       svpRef   `json:"mainRef"`
	Groups        []any    `json:"groups"`
}

type svpMeter struct {
	Index       int `json:"index"`
	Numerator   int `json:"numerator"`
	Denominator int `json:"denominator"`
}

type svpTempo struct {
	Position int64   `json:"position"`
	BPM      float64 `json:"bpm"`
}

type svpProject struct {
	Version int `json:"version"`
	Time    struct {
		Meter []svpMeter `json:"meter"`
		Tempo []svpTempo `json:"tempo"`
	} `json:"time"`
	Library      []any          `json:"library"`
	Tracks       []svpTrack     `json:"tracks"`
	RenderConfig map[string]any `json:"renderConfig"`
}

// SVExport 是导出 Synthesizer V 工程的选项。零值即不指定歌姬、不写表情曲线（与 SVP 相同）。
type SVExport struct {
	// Singer 为 nil 时不指定声库（SV 打开后由用户选择）。
	Singer *SVSinger
	// Preset 的音色与移调总是生效；表情强度只在 Expr 非 nil 时生效。
	Preset SVPreset
	Expr   *Expression
	// Base 是同一首歌不写表情曲线时 SV 渲染的人声特征（按移调后的音高）；非 nil 且音高强度 > 0 时音高改为引导式。
	Base *Expression
	// Breaths 为 true 时在乐句前插入 br 换气音符。
	Breaths bool
}

// 换气音符（SVExport.Breaths）：乐句前的停顿至少 svBreathMinGap 秒才插入，换气与上一个音之间至少留
// svBreathMargin 秒，最长 svBreathMax 秒（真人换气约 0.2–0.5 秒，更长的 br 在 SV 中容易出现金属感的杂音）。
const (
	svBreathMinGap = 0.3
	svBreathMargin = 0.05
	svBreathMax    = 0.45
)

// SVP 返回旋律线的 Synthesizer V Studio 工程（不指定歌姬、不写表情曲线）；name 为音轨名。没有旋律线时返回错误。
func (a *Analysis) SVP(name string) ([]byte, error) { return a.SVPExport(name, SVExport{}) }

// SVPExport 按 o 返回旋律线的 Synthesizer V Studio 工程；name 为音轨名。没有旋律线或演唱模式格式错误时返回错误。
func (a *Analysis) SVPExport(name string, o SVExport) ([]byte, error) {
	notes, tm, err := a.vocalTrack()
	if err != nil {
		return nil, err
	}
	modes, err := o.Preset.Modes()
	if err != nil {
		return nil, err
	}
	transpose := o.Preset.Transpose
	if o.Preset.AutoTranspose {
		transpose = 0
		if o.Singer != nil {
			transpose = a.SuggestTranspose(*o.Singer)
		}
	}
	// 曲线按移调前的音高计算（与原唱基频比较），音符再移调。
	var curves svCurves
	if o.Expr != nil {
		curves = expressionCurves(notes, tm, o.Expr, o.Preset)
	}
	_, manualPitch := curves["pitchDelta"]
	if manualPitch && o.Base != nil {
		// 引导式：保留 SV 的 AI 音高，只写与基准的差值。
		owner := frameOwners(notes, tm, o.Expr.Hop, len(o.Expr.F0))
		guide := guidedPitch(notes, owner, o.Expr, o.Base, transpose, o.Preset.PitchStrength)
		tick := make([]int, len(guide))
		for i := range tick {
			tick[i] = tm.tick(float64(i) * o.Expr.Hop)
		}
		curves["pitchDelta"] = curveOverNotes(notes, tick, owner[:len(guide)], svPitchStride, 0, func(i int) (float64, bool) {
			return guide[i], !math.IsNaN(guide[i])
		})
		manualPitch = false
	}
	var p svpProject
	p.Version = svpVersion
	for _, m := range tm.meters() {
		p.Time.Meter = append(p.Time.Meter, svpMeter{m.bar, m.beats, 4})
	}
	for _, t := range tm.tempos() {
		p.Time.Tempo = append(p.Time.Tempo, svpTempo{int64(t.tick) * svpBlicksPerTick, t.bpm()})
	}

	empty := func() svpCurve { return svpCurve{Mode: "cubic", Points: []float64{}} }
	group := svpGroup{Name: "main", UUID: newUUID(), Parameters: map[string]svpCurve{}, VocalModes: map[string]svpCurve{}}
	for _, k := range []string{"pitchDelta", "vibratoEnv", "loudness", "tension", "breathiness", "voicing", "gender", "toneShift"} {
		if c, ok := curves[k]; ok {
			group.Parameters[k] = c
		} else {
			group.Parameters[k] = empty()
		}
	}
	for i, n := range notes {
		if o.Breaths && !n.slur {
			if b, ok := breathBefore(notes, i, tm, transpose); ok {
				group.Notes = append(group.Notes, b)
			}
		}
		lyric := n.lyric
		switch {
		case n.slur:
			lyric = "-"
		case lyric == "":
			lyric = "la"
		}
		note := svpNote{
			Onset: int64(n.on) * svpBlicksPerTick, Duration: int64(n.off-n.on) * svpBlicksPerTick,
			Lyrics: lyric, Pitch: min(max(n.pitch+transpose, 0), 127), Attributes: map[string]any{},
		}
		if manualPitch {
			// 手动音高：滑音缩到 10 ms、不过冲，颤音深度 0（音高的起伏全部来自 pitchDelta）。
			off := false
			note.InstantMode = &off
			note.Attributes = map[string]any{"tF0Left": 0.01, "tF0Right": 0.01, "dF0Left": 0.0, "dF0Right": 0.0, "dF0Vbr": 0.0}
		}
		group.Notes = append(group.Notes, note)
	}
	// 音轨的声库与音色默认值。
	db := map[string]any{"name": "", "language": "", "phoneset": "", "backendType": ""}
	if s := o.Singer; s != nil {
		if s.Database != nil {
			db = maps.Clone(s.Database)
		} else {
			db["name"] = s.Name
			if len(s.Languages) > 0 {
				db["language"] = s.Languages[0]
			}
		}
	}
	db["languageOverride"], db["phonesetOverride"] = "mandarin", "xsampa"
	voice := map[string]any{}
	for k, v := range map[string]float64{"paramGender": o.Preset.Gender, "paramToneShift": o.Preset.ToneShift,
		"paramLoudness": o.Preset.Loudness, "paramBreathiness": o.Preset.Breathiness, "paramTension": o.Preset.Tension} {
		if v != 0 {
			voice[k] = v
		}
	}
	if len(modes) > 0 {
		voice["vocalModeInherited"], voice["vocalModeParams"] = false, modes
	}
	p.Library = []any{}
	p.Tracks = []svpTrack{{
		Name: name, DispColor: "ff7db235", RenderEnabled: true,
		Mixer:     svpMixer{Display: true},
		MainGroup: group,
		MainRef: svpRef{
			GroupID: group.UUID, BlickAbsoluteEnd: -1, SystemPitchDelta: empty(),
			Database: db,
			Voice:    voice,
		},
		Groups: []any{},
	}}
	p.RenderConfig = map[string]any{
		"destination": "./", "filename": name, "numChannels": 1, "aspirationFormat": "noAspiration",
		"bitDepth": 16, "sampleRate": 44100, "exportMixDown": true,
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append(data, 0), nil
}

// breathBefore 返回第 i 个音之前的换气音符；停顿不够长时返回 false。音高同乐句的第一个音（已移调）。
func breathBefore(notes []vocalNote, i int, tm *midiTimeMap, transpose int) (svpNote, bool) {
	on := tm.seconds(notes[i].on)
	prev := 0.0
	if i > 0 {
		prev = tm.seconds(notes[i-1].off)
	}
	gap := on - prev
	if gap < svBreathMinGap {
		return svpNote{}, false
	}
	start := tm.tick(on - min(gap-svBreathMargin, svBreathMax))
	if start >= notes[i].on {
		return svpNote{}, false
	}
	return svpNote{
		Onset: int64(start) * svpBlicksPerTick, Duration: int64(notes[i].on-start) * svpBlicksPerTick,
		Lyrics: "br", Phonemes: "br", Pitch: min(max(notes[i].pitch+transpose, 0), 127), Attributes: map[string]any{},
	}, true
}
