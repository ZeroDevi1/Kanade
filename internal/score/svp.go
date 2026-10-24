package score

import (
	"encoding/json"
)

// 导出 Synthesizer V Studio 工程（.svp，UTF-8 JSON）：一条人声音轨，音符与歌词来自旋律线，速度与拍号同 MIDI 导出。
// 格式依据 LibreSVIP 的 svp 插件（libresvip/plugins/svp/model.py）：时间单位为 blick，每四分音符 705600000；
// 拍号按小节序号（index）记录。版本号写 135（1.10.0–1.11.2 的格式），Synthesizer V Studio 1.10 起与 2.x 都能打开；
// 2.0 以前的版本要求文件以一个 NUL 字节结尾，2.x 读取时忽略它。
// 歌词按普通话解释（languageOverride），连音写 "-"，没有歌词的音写 Synthesizer V 新建音符的默认歌词 "la"。

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
}

type svpGroup struct {
	Name       string              `json:"name"`
	UUID       string              `json:"uuid"`
	Parameters map[string]svpCurve `json:"parameters"`
	VocalModes map[string]svpCurve `json:"vocalModes"`
	Notes      []svpNote           `json:"notes"`
}

type svpDatabase struct {
	Name             string `json:"name"`
	Language         string `json:"language"`
	Phoneset         string `json:"phoneset"`
	LanguageOverride string `json:"languageOverride"`
	PhonesetOverride string `json:"phonesetOverride"`
	BackendType      string `json:"backendType"`
}

type svpRef struct {
	GroupID            string         `json:"groupID"`
	BlickAbsoluteBegin int64          `json:"blickAbsoluteBegin"`
	BlickAbsoluteEnd   int64          `json:"blickAbsoluteEnd"`
	BlickOffset        int64          `json:"blickOffset"`
	PitchOffset        int            `json:"pitchOffset"`
	IsInstrumental     bool           `json:"isInstrumental"`
	SystemPitchDelta   svpCurve       `json:"systemPitchDelta"`
	Database           svpDatabase    `json:"database"`
	Dictionary         string         `json:"dictionary"`
	Voice              map[string]any `json:"voice"`
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

// SVP 返回旋律线的 Synthesizer V Studio 工程；name 为音轨名。没有旋律线时返回错误。
func (a *Analysis) SVP(name string) ([]byte, error) {
	notes, tm, err := a.vocalTrack()
	if err != nil {
		return nil, err
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
		group.Parameters[k] = empty()
	}
	for _, n := range notes {
		lyric := n.lyric
		switch {
		case n.slur:
			lyric = "-"
		case lyric == "":
			lyric = "la"
		}
		group.Notes = append(group.Notes, svpNote{
			Onset: int64(n.on) * svpBlicksPerTick, Duration: int64(n.off-n.on) * svpBlicksPerTick,
			Lyrics: lyric, Pitch: n.pitch, Attributes: map[string]any{},
		})
	}
	p.Library = []any{}
	p.Tracks = []svpTrack{{
		Name: name, DispColor: "ff7db235", RenderEnabled: true,
		Mixer:     svpMixer{Display: true},
		MainGroup: group,
		MainRef: svpRef{
			GroupID: group.UUID, BlickAbsoluteEnd: -1, SystemPitchDelta: empty(),
			Database: svpDatabase{LanguageOverride: "mandarin", PhonesetOverride: "xsampa"},
			Voice:    map[string]any{},
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
