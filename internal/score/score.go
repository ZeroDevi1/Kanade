// Package score 组装音乐分析引擎（engines/score）的任务参数、运行「分离伴奏 → 分析」流水线并解析结果。
package score

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kanade/internal/conductor"
	"kanade/internal/paths"
	"kanade/internal/stems"
)

// Engine 是 Conductor 中音乐分析引擎的名称。
const Engine = "score"

// Options 是 score.analyze 的分析参数，零值字段使用引擎默认（见引擎 analyze.py）。
type Options struct {
	BeatMethod string // BeatMethods 之一
	// TempoMin / TempoMax 非零时把 BPM 按倍数折叠到区间内（半速 / 倍速误判时用）。
	TempoMin, TempoMax float64
	KeyProfile         string  // KeyProfiles 之一
	KeySegmentS        float64 // 分段估计调号的窗口（秒），0 为不分段
	ChordVocab         string  // ChordVocabs 之一
	ChordSelfProb      float64 // 和弦平滑（HMM 自转移概率），0.5–0.99
	ChordMinBeats      int     // 最短和弦（拍）
	Emotion            bool
	// SlurModel 是有歌词时字与音符的对应方法（SlurModels 之一），空为引擎默认 align。
	SlurModel string
	// Transcribe 为 true 时用 MuScriptor 对原曲做多乐器转录（权重为 CC BY-NC 4.0 非商用许可，下载需要 HF 令牌）。
	Transcribe bool
	// Instruments 非空时转录只在这些乐器组（InstrumentGroups 的 Key）中识别：作为条件输入模型，其余乐器的事件在解码时
	// 被屏蔽（上游 transcribe 的 instruments 参数）。用于纠正音色相近的误判，例如钢琴伴奏被识别成吉他。
	Instruments []string
}

// InstrumentGroup 是 MuScriptor 的一个乐器组：Program 是它的代表 GM 音色（上游 MT3_FULL_PLUS 分组中的第一个音色，
// 与上游写 MIDI 时相同），鼓为 -1。
type InstrumentGroup struct {
	Key, Title string
	Program    int
}

// InstrumentGroups 是 MuScriptor 的全部乐器组（上游 MT3_FULL_PLUS_GROUP_NAMES 与 get_group_program_map，顺序同上游）。
var InstrumentGroups = func() []InstrumentGroup {
	groups := []struct {
		key     string
		program int
	}{
		{"acoustic_piano", 0}, {"electric_piano", 2}, {"chromatic_percussion", 8}, {"organ", 16}, {"acoustic_guitar", 24},
		{"clean_electric_guitar", 26}, {"distorted_electric_guitar", 29}, {"acoustic_bass", 32}, {"electric_bass", 33},
		{"violin", 40}, {"viola", 41}, {"cello", 42}, {"contrabass", 43}, {"orchestral_harp", 46}, {"timpani", 47},
		{"string_ensemble", 48}, {"synth_strings", 50}, {"voice", 52}, {"orchestra_hit", 55}, {"trumpet", 56},
		{"trombone", 57}, {"tuba", 58}, {"french_horn", 60}, {"brass_section", 61}, {"soprano_and_alto_sax", 64},
		{"tenor_sax", 66}, {"baritone_sax", 67}, {"oboe", 68}, {"english_horn", 69}, {"bassoon", 70}, {"clarinet", 71},
		{"flutes", 72}, {"synth_lead", 80}, {"synth_pad", 88}, {"drums", -1},
	}
	out := make([]InstrumentGroup, len(groups))
	for i, g := range groups {
		out[i] = InstrumentGroup{g.key, instrumentTitles[g.key], g.program}
	}
	return out
}()

// 可选值，与引擎一致。Title 用于界面。
var (
	BeatMethods = []struct{ Key, Title string }{{"beat_this", "beat_this（神经网络，推荐）"}, {"librosa", "librosa（快速，无需下载）"}}
	KeyProfiles = []struct{ Key, Title string }{{"krumhansl", "Krumhansl–Kessler"}, {"temperley", "Temperley"}}
	ChordVocabs = []struct{ Key, Title string }{
		{"full", "完整和弦（神经网络，推荐）"}, {"basic", "大小三和弦（模板匹配）"}, {"extended", "含七和弦、减三、挂四（模板匹配）"},
	}
	// SlurModels：align 把字分配给 SOME 识别的音符；game 由 GAME 按字切分音符（每个字至少一个音），
	// 其权重为 CC BY-NC-SA 4.0 非商用许可。
	SlurModels = []struct{ Key, Title string }{{"align", "SOME + 字音分配（默认）"}, {"game", "GAME 按字切分（非商用许可）"}}
)

// DefaultOptions 是智能模式使用的参数（引擎默认值）。
func DefaultOptions() Options {
	return Options{BeatMethod: "beat_this", KeyProfile: "krumhansl", KeySegmentS: 30, ChordVocab: "full",
		ChordSelfProb: 0.9, ChordMinBeats: 1, Emotion: true}
}

func (o Options) params() (map[string]any, error) {
	if o.ChordSelfProb != 0 && (o.ChordSelfProb < 0.5 || o.ChordSelfProb > 0.99) {
		return nil, errors.New("和弦平滑须在 0.5–0.99 之间")
	}
	if o.TempoMin != 0 || o.TempoMax != 0 {
		if o.TempoMin < 20 || o.TempoMax > 400 || o.TempoMax < o.TempoMin*2 {
			return nil, errors.New("BPM 范围须在 20–400 之间，且上限至少为下限的 2 倍（才能容纳倍速折叠）")
		}
	}
	if o.ChordMinBeats < 0 || o.KeySegmentS < 0 {
		return nil, errors.New("最短和弦与分段长度不能为负")
	}
	p := map[string]any{"emotion": o.Emotion, "key_segment_s": o.KeySegmentS, "transcribe": o.Transcribe}
	if o.Transcribe && len(o.Instruments) > 0 {
		for _, k := range o.Instruments {
			if _, ok := instrumentTitles[k]; !ok {
				return nil, fmt.Errorf("未知的乐器组 %q", k)
			}
		}
		p["instruments"] = o.Instruments
	}
	for k, v := range map[string]string{"beat_method": o.BeatMethod, "key_profile": o.KeyProfile, "chord_vocab": o.ChordVocab,
		"slur_model": o.SlurModel} {
		if v != "" {
			p[k] = v
		}
	}
	if o.ChordSelfProb != 0 {
		p["chord_self_prob"] = o.ChordSelfProb
	}
	if o.ChordMinBeats > 0 {
		p["chord_min_beats"] = o.ChordMinBeats
	}
	if o.TempoMin != 0 {
		p["tempo_range"] = []float64{o.TempoMin, o.TempoMax}
	}
	return p, nil
}

// Request 是一次分析的输入。Separate 为 true 时先用分轨引擎分离伴奏，调号与和弦基于伴奏分析
// （人声旋律音会干扰色度，分离后和弦更准，但要多跑一次 GPU 分离）。
type Request struct {
	Input    string
	Separate bool
	// Melody 为 true 时分离出主唱（干声 → 去和声）并识别旋律线；需要两步 GPU 分离。
	Melody bool
	// Lyrics 是 LRC 歌词文件，非空且 Melody 为 true 时把歌词逐字对到旋律音符上（连音 / 转音按字分组）。
	Lyrics     string
	StemModels map[string]string // 分离伴奏用的模型，见 stems.Recommended
	Options    Options
	// History 非空时，成功的分析保存为该目录下的记录（见 HistoryDir），并复用同一首歌已分离的分轨。
	History string
}

// Params 返回 score.analyze 的任务参数；harmonic 为分离出的伴奏、melody 为分离出的主唱（均可为空）。
func (r Request) Params(harmonic, melody string) (map[string]any, error) {
	if r.Input == "" {
		return nil, errors.New("请选择要分析的歌曲")
	}
	opts, err := r.Options.params()
	if err != nil {
		return nil, err
	}
	p := map[string]any{"input": r.Input, "options": opts}
	if harmonic != "" {
		p["harmonic_input"] = harmonic
	}
	if melody != "" {
		p["melody_input"] = melody
		if r.Lyrics != "" {
			p["lyrics_input"] = r.Lyrics
		}
	}
	return p, nil
}

// ModelDir 是分析模型目录，与引擎的 MODEL_DIR 一致。
func ModelDir(l paths.Layout) string { return filepath.Join(l.Models, "score") }

// ModelGroup 是一组需要下载的模型文件。
type ModelGroup struct {
	Key, Title string
	Files      []string // 相对 ModelDir
	SizeMB     int
}

// ModelGroups 与引擎 models.GROUPS 一致。
var ModelGroups = []ModelGroup{
	{"beat_this", "节拍（beat_this）", []string{"beat_this/final0.ckpt"}, 81},
	{"clap", "情绪（CLAP）", []string{"clap/clap-htsat-unfused/pytorch_model.bin", "clap/clap-htsat-unfused/config.json"}, 615},
	{"some", "旋律（SOME）", []string{"some/0119_continuous256_5spk/model_ckpt_steps_100000_simplified.ckpt"}, 435},
	{"btc", "和弦（BTC）", []string{"btc/btc_model_large_voca.pt"}, 12},
	{"lyrics", "歌词对齐（wav2vec2）", []string{"lyrics/wav2vec2-large-xlsr-53-chinese-zh-cn/pytorch_model.bin",
		"lyrics/wav2vec2-large-xlsr-53-chinese-zh-cn/vocab.json"}, 1276},
	{"game", "连音（GAME，非商用）", []string{"game/GAME-1.0-medium/model.pt", "game/GAME-1.0-medium/config.yaml"}, 185},
	{"muscriptor", "多乐器转录（MuScriptor，非商用）", []string{"muscriptor/muscriptor-medium/model.safetensors",
		"muscriptor/muscriptor-medium/config.json"}, 1228},
}

// MuScriptorPage 是 MuScriptor 权重的 Hugging Face 页面：下载前须登录并在此接受许可。
const MuScriptorPage = "https://huggingface.co/MuScriptor/muscriptor-medium"

// Installed 报告模型组是否已下载。
func (g ModelGroup) Installed(l paths.Layout) bool {
	for _, f := range g.Files {
		if st, err := os.Stat(filepath.Join(ModelDir(l), f)); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// Jobs 是流水线需要的 Conductor 能力，测试中可替换。
type Jobs interface {
	Submit(engine, task string, params map[string]any) (string, error)
	Wait(ctx context.Context, id string) (conductor.Job, error)
	Cancel(id string)
}

// 流水线阶段。
const (
	StageSeparate = iota
	StageAnalyze
	StageDone
)

// StageTitles 是各阶段的显示名称。
var StageTitles = []string{"分离伴奏", "分析", "完成"}

// Run 是运行中的分析流水线。状态由后台 goroutine 更新，Snapshot 可从任意 goroutine 读取。
type Run struct {
	Request  Request
	jobs     Jobs
	onChange func()
	cancel   context.CancelFunc

	mu      sync.Mutex
	stage   int
	job     string
	err     string
	result  *Analysis
	audio   string // 预览用的解码 WAV
	files   map[string]string
	skipSep bool
	reused  bool // 分轨全部复用自已有记录，没有运行分离
	// dir 是保存的记录目录（未保存时为空）；created 是记录的创建时间（运行中的分析为启动时间）。
	dir     string
	created time.Time
	saveErr string
}

// RunState 是 Run 的快照。
type RunState struct {
	Stage   int
	Job     string
	Err     string
	Skipped bool // 未分离伴奏
	Reused  bool // 分轨复用自已有记录
	Result  *Analysis
	Audio   string
	// Files 是导出用的文件：analysis（JSON）与 chords（.lab）。
	Files map[string]string
	// Saved 报告结果是否已保存为记录；SaveErr 是保存失败的原因（结果仍在暂存区，可查看与导出）。
	Saved   bool
	SaveErr string
	Created time.Time
}

// Running 报告流水线是否仍在进行。
func (s RunState) Running() bool { return s.Err == "" && s.Stage < StageDone }

// Start 校验输入并在后台启动流水线；onChange 在状态变化时调用（可为 nil）。
func Start(jobs Jobs, r Request, onChange func()) (*Run, error) {
	if _, err := r.Params("", ""); err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{Request: r, jobs: jobs, onChange: onChange, cancel: cancel, files: map[string]string{},
		skipSep: !r.Separate && !r.Melody, created: time.Now()}
	go run.run(ctx)
	return run, nil
}

// Snapshot 返回当前状态。
func (r *Run) Snapshot() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	files := make(map[string]string, len(r.files))
	for k, v := range r.files {
		files[k] = v
	}
	return RunState{Stage: r.stage, Job: r.job, Err: r.err, Skipped: r.skipSep, Reused: r.reused, Result: r.result,
		Audio: r.audio, Files: files, Saved: r.dir != "", SaveErr: r.saveErr, Created: r.created}
}

// Cancel 取消当前任务并停止流水线。
func (r *Run) Cancel() {
	r.mu.Lock()
	job := r.job
	r.mu.Unlock()
	r.cancel()
	if job != "" {
		r.jobs.Cancel(job)
	}
}

func (r *Run) set(fn func()) {
	r.mu.Lock()
	fn()
	r.mu.Unlock()
	r.onChange()
}

func (r *Run) step(ctx context.Context, stage int, engine, task string, params map[string]any) ([]string, error) {
	if ctx.Err() != nil {
		return nil, errors.New("已取消")
	}
	id, err := r.jobs.Submit(engine, task, params)
	if err != nil {
		return nil, err
	}
	r.set(func() { r.stage, r.job = stage, id })
	j, err := r.jobs.Wait(ctx, id)
	if err != nil {
		return nil, err
	}
	switch j.State {
	case conductor.JobDone:
		return j.Artifacts, nil
	case conductor.JobCanceled:
		return nil, errors.New("已取消")
	default:
		return nil, fmt.Errorf("%s失败：%s", StageTitles[stage], j.Err)
	}
}

func (r *Run) run(ctx context.Context) {
	fail := func(err error) { r.set(func() { r.err = err.Error() }) }
	// 分离：伴奏（调号与和弦）与主唱（旋律）在同一次分轨任务中完成；主唱取去和声后的 _vocals_karaoke。
	// 已有记录分离过同一首歌时直接复用，只分离缺少的部分。
	reuse := findStems(r.Request.History, r.Request)
	harmonic, melody := "", ""
	var steps []string
	if r.Request.Separate {
		if harmonic = reuse.harmonic; harmonic == "" {
			steps = append(steps, "other")
		}
	}
	if r.Request.Melody {
		if melody = reuse.melody; melody == "" {
			steps = append(steps, "karaoke")
		}
	}
	// separated 是本次新分离出的分轨，保存记录时一并移入。
	separated := map[string]string{}
	if len(steps) > 0 {
		params, _ := stems.ChainParams(r.Request.Input, steps, r.Request.StemModels)
		arts, err := r.step(ctx, StageSeparate, stems.Engine, "stems.chain", params)
		if err != nil {
			fail(err)
			return
		}
		for _, a := range arts {
			switch {
			case strings.HasSuffix(a, "_vocals_karaoke.wav"):
				if melody == "" {
					melody, separated["melody"] = a, a
				}
			case strings.HasSuffix(a, "_vocals_other.wav"): // 和声，不用
			case strings.HasSuffix(a, "_other.wav"):
				if harmonic == "" {
					harmonic, separated["harmonic"] = a, a
				}
			}
		}
		if r.Request.Separate && harmonic == "" || r.Request.Melody && melody == "" {
			fail(errors.New("分轨结果缺少伴奏或主唱"))
			return
		}
	} else if !r.skipSep {
		r.set(func() { r.reused = true })
	}
	params, err := r.Request.Params(harmonic, melody)
	if err != nil {
		fail(err)
		return
	}
	arts, err := r.step(ctx, StageAnalyze, Engine, "score.analyze", params)
	if err != nil {
		fail(err)
		return
	}
	files := map[string]string{}
	audio := ""
	for _, a := range arts {
		switch filepath.Ext(a) {
		case ".json":
			files["analysis"] = a
		case ".lab":
			files["chords"] = a
		case ".wav":
			audio = a
		}
	}
	res, err := ReadAnalysis(files["analysis"])
	if err != nil {
		fail(fmt.Errorf("读取分析结果：%w", err))
		return
	}
	// 保存记录：失败不影响本次结果（仍在暂存区），只提示原因。
	dir, saveErr := "", ""
	if r.Request.History != "" {
		all := maps.Clone(files)
		if audio != "" {
			all["audio"] = audio
		}
		d, moved, _, err := save(r.Request.History, r.Request, r.created, all, separated)
		if err != nil {
			saveErr = "保存分析记录失败：" + err.Error()
		} else {
			dir, audio = d, moved["audio"]
			delete(moved, "audio")
			files = moved
		}
	}
	r.set(func() {
		r.result, r.audio, r.files, r.stage, r.job, r.dir, r.saveErr = res, audio, files, StageDone, "", dir, saveErr
	})
}

// Analysis 是引擎 analysis.json 的内容。
type Analysis struct {
	Duration float64 `json:"duration"`
	Tempo    struct {
		BPM         *float64     `json:"bpm"`
		Stable      *bool        `json:"stable"`
		SteadyShare float64      `json:"steady_share"`
		Curve       [][2]float64 `json:"curve"` // [小节起点秒, 局部 BPM]
		Folded      bool         `json:"folded"`
	} `json:"tempo"`
	Meter struct {
		Label       *string `json:"label"`
		BeatsPerBar int     `json:"beats_per_bar"`
		Confidence  float64 `json:"confidence"`
		// Denominator 是拍号的分母（拍点 Beats 所代表的音符：4 为四分音符，8 为八分音符）；0 视为 4。
		// 分析引擎的结果总是以四分音符为拍；灵感页的复合拍（6/8）以八分音符为拍。
		Denominator int `json:"denominator,omitempty"`
	} `json:"meter"`
	Beats     []float64 `json:"beats"`
	Downbeats []float64 `json:"downbeats"`
	Key       struct {
		// Label 是整首的调号；有转调时（Segments 多于一段）它是各段的折中，界面以 Segments 为准。
		Label       string  `json:"label"`
		TonicPC     int     `json:"tonic_pc"` // 主音音级，0 = C
		Mode        string  `json:"mode"`     // major / minor
		Flats       bool    `json:"flats"`    // 是否用降号拼写
		Relative    string  `json:"relative"`
		Confidence  string  `json:"confidence"` // 高 / 中 / 低
		Correlation float64 `json:"correlation"`
		Candidates  []struct {
			Label       string  `json:"label"`
			Score       float64 `json:"score"` // 轮廓相关系数 + 和弦吻合度
			Correlation float64 `json:"correlation"`
		} `json:"candidates"`
		Segments []Segment `json:"segments"`
	} `json:"key"`
	Chords []Segment `json:"chords"`
	Melody *Melody   `json:"melody"` // 旋律线；分析时没有分离主唱则为 nil
	// Transcription 是原曲的多乐器转录；未开启时为 nil。
	Transcription *Transcription `json:"transcription"`
	ChordSource   string         `json:"chord_source"` // accompaniment / mix
	Emotion       *Emotion       `json:"emotion"`      // 未做情绪识别时为 nil
}

// Emotion 是情绪识别的结果（CLAP 零样本分类，见引擎 emotion.py）。
type Emotion struct {
	Moods []struct {
		Key   string  `json:"key"`
		Label string  `json:"label"`
		Score float64 `json:"score"`
	} `json:"moods"` // 按分数从高到低
	Valence  float64   `json:"valence"` // 效价：消极 0 — 积极 1
	Arousal  float64   `json:"arousal"` // 唤醒度：舒缓 0 — 激烈 1
	Segments []Segment `json:"segments"`
}

// Transcription 是 MuScriptor 的多乐器转录结果（见引擎 transcribe.py）。
type Transcription struct {
	Model string `json:"model"`
	// OnsetDelay 是已从全部音符中扣除的整体起音延迟（秒），0 为未校正。
	OnsetDelay  float64      `json:"onset_delay"`
	Instruments []Instrument `json:"instruments"` // 按音符数从多到少
	// Hint 是分析时给的乐器提示（Options.Instruments），为空时是自动识别。
	Hint []string `json:"hint,omitempty"`
}

// Instrument 是一种乐器（上游的乐器组）的音符。
type Instrument struct {
	Name    string           `json:"name"`    // 上游乐器组名，如 electric_bass；未知组形如 program_42
	Program int              `json:"program"` // 代表 GM 音色 0–127；鼓为 -1
	Drum    bool             `json:"drum"`
	Notes   []InstrumentNote `json:"notes"`
}

// InstrumentNote 是转录出的一个音（秒）。鼓的时长是引擎给的固定短音。
type InstrumentNote struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Pitch int     `json:"pitch"` // MIDI 音高；鼓为 GM 打击乐键位
}

// instrumentTitles 是乐器组的中文名。
var instrumentTitles = map[string]string{
	"acoustic_piano": "原声钢琴", "electric_piano": "电钢琴", "chromatic_percussion": "有音高打击乐", "organ": "风琴",
	"acoustic_guitar": "原声吉他", "clean_electric_guitar": "清音电吉他", "distorted_electric_guitar": "失真电吉他",
	"acoustic_bass": "原声贝斯", "electric_bass": "电贝斯", "violin": "小提琴", "viola": "中提琴", "cello": "大提琴",
	"contrabass": "低音提琴", "orchestral_harp": "竖琴", "timpani": "定音鼓", "string_ensemble": "弦乐组",
	"synth_strings": "合成弦乐", "voice": "人声", "orchestra_hit": "管弦乐齐奏", "trumpet": "小号", "trombone": "长号",
	"tuba": "大号", "french_horn": "圆号", "brass_section": "铜管组", "soprano_and_alto_sax": "高音 / 中音萨克斯",
	"tenor_sax": "次中音萨克斯", "baritone_sax": "上低音萨克斯", "oboe": "双簧管", "english_horn": "英国管",
	"bassoon": "大管", "clarinet": "单簧管", "flutes": "长笛", "synth_lead": "合成主音", "synth_pad": "合成铺底",
	"drums": "鼓",
}

// Title 返回乐器的中文名；没有译名时返回原名。
func (i Instrument) Title() string { return InstrumentTitle(i.Name) }

// InstrumentTitle 返回乐器组 name 的中文名；没有译名时返回原名。
func InstrumentTitle(name string) string {
	if t, ok := instrumentTitles[name]; ok {
		return t
	}
	return name
}

// TrackName 返回 MIDI 轨道名：上游组名换成首字母大写的英文词，如 electric_bass → Electric Bass。
func (i Instrument) TrackName() string {
	words := strings.Fields(strings.ReplaceAll(i.Name, "_", " "))
	for k, w := range words {
		words[k] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// Segment 是一段带标签的时间区间（秒）。和弦段另有根音与性质（无和弦 N 时 Root 为 nil）。
type Segment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Label   string  `json:"label"`
	Root    *int    `json:"root,omitempty"`    // 根音音级，0 = C
	Quality string  `json:"quality,omitempty"` // 和弦性质，见引擎 harmony.QUALITIES
	// Bass 是转位和弦的低音音级（Label 形如 C/E）；为 nil 时低音即根音。
	Bass *int `json:"bass,omitempty"`
}

// ReadAnalysis 读取并解析 analysis.json。
func ReadAnalysis(path string) (*Analysis, error) {
	if path == "" {
		return nil, errors.New("没有分析结果文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Analysis
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ChordAt 返回 t 秒处的和弦与其下标；不在任何和弦内时返回 -1。
func (a *Analysis) ChordAt(t float64) int {
	for i, c := range a.Chords {
		if t >= c.Start && t < c.End {
			return i
		}
	}
	return -1
}

// BarAt 返回 t 秒所在的小节号（从 1 开始）与拍号（从 1 开始）；第一个小节线之前返回 0, 0。
func (a *Analysis) BarAt(t float64) (bar, beat int) {
	for i, d := range a.Downbeats {
		if d > t {
			break
		}
		bar = i + 1
	}
	if bar == 0 {
		return 0, 0
	}
	start := a.Downbeats[bar-1]
	for _, b := range a.Beats {
		if b >= start-1e-3 && b <= t {
			beat++
		}
	}
	return bar, beat
}

// Bar 是和弦谱的一小节：起止时间与小节内依次出现的和弦（相邻重复的只记一次）。
type Bar struct {
	Start, End float64
	Chords     []string
}

// Bars 按小节线把和弦序列排成和弦谱。第一个小节线之前的部分（弱起）作为第 0 小节并入第一小节。
func (a *Analysis) Bars() []Bar {
	if len(a.Downbeats) == 0 {
		return nil
	}
	bounds := append(append([]float64{}, a.Downbeats...), a.Duration)
	bounds[0] = 0
	bars := make([]Bar, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		b := Bar{Start: bounds[i], End: bounds[i+1]}
		for _, c := range a.Chords {
			// 和弦与小节有重叠（且不只是边界上的几十毫秒）才计入。
			if c.End > b.Start+0.05 && c.Start < b.End-0.05 {
				if n := len(b.Chords); n == 0 || b.Chords[n-1] != c.Label {
					b.Chords = append(b.Chords, c.Label)
				}
			}
		}
		bars = append(bars, b)
	}
	return bars
}

// ChordChart 返回纯文本和弦谱：每行 4 小节，行首为小节号与时间，便于导出与打印。
func (a *Analysis) ChordChart(title string) string {
	var sb strings.Builder
	key, meter, bpm := a.Key.Label, "", ""
	if a.Meter.Label != nil {
		meter = *a.Meter.Label
	}
	if a.Tempo.BPM != nil {
		bpm = fmt.Sprintf("%.0f BPM", *a.Tempo.BPM)
	}
	fmt.Fprintf(&sb, "%s\n%s · %s · %s\n\n", title, key, meter, bpm)
	bars := a.Bars()
	for i := 0; i < len(bars); i += 4 {
		fmt.Fprintf(&sb, "%3d  %s  |", i+1, Clock(bars[i].Start))
		for _, b := range bars[i:min(i+4, len(bars))] {
			cell := strings.Join(b.Chords, " ")
			if cell == "" {
				cell = "N"
			}
			fmt.Fprintf(&sb, " %-12s|", cell)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Clock 把秒数格式化为 m:ss。
func Clock(sec float64) string {
	s := int(sec + 0.5)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
