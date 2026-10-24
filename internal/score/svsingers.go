package score

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// 本机的 Synthesizer V 歌姬：导出工程时选择，写入音轨的 database，SV 打开工程时据此加载声库。
//
// 来源（Synthesizer V Studio 2 的数据目录，Windows 为 %APPDATA%\Dreamtonics\Synthesizer V Studio 2）：
//   - databases/meta/*.json：SV 缓存的声库商店目录（名称、厂商、语言、音域），包含未安装的声库。
//     已安装声库的目录（databases/<uuid>/<版本>/）中的元数据是加密的，读不出名称，因此用商店目录列出全部歌姬。
//   - recovery/*/*.svp：SV 自动保存的恢复工程。其中出现过的声库确定已安装，并带有完整的 database 字段
//     （后端类型、版本、音素集），排在列表前面并原样写入。
// 名称（name）与工程中 database.name 一致（2026-10-24 用 Mo Xu、Mo Chen、Nanami、Kevin 核对）。
//
// Synthesizer V Studio Flat（基于 SV 1.x 的第三方版本，工程格式 153）的数据目录在 文档\OPSV\Dreamtonics\Synthesizer V Studio：
//   - databases/<声库名>/info.<版本>.nofs：明文 JSON，有名称、版本、厂商、母语、音素集、可跨语种演唱的语言，
//     以及 styles（演唱模式；"(base)"、"(default)" 是内部项）。目录里的都已安装。
//   - 没有音域：按名称（去掉 AI / PLUS / 版本号等后缀）到 SV2 的商店目录中查，查到时补上音域与曲风。

// SVSinger 是一个 Synthesizer V 歌姬。
type SVSinger struct {
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
	// Languages 的第一项是母语（Flat 的声库），其余为可跨语种演唱的语言；SV2 商店目录给出的是声库支持的语言。
	Languages []string `json:"languages"`
	// PitchRange 是商店标注的推荐音域，如 "A#3 - G#4"；为空表示未标注（例如合唱声库）或查不到。
	PitchRange string   `json:"pitchRange"`
	Genres     []string `json:"genres"` // 商店标注的曲风，如 Pop、Rock；可为空
	// Modes 是声库的演唱模式名称（Flat 的声库有；SV2 的读不到，为空）。
	Modes []string `json:"-"`
	// Database 是写入工程的 database 字段：SV2 取自恢复工程（原样写入），Flat 取自声库信息；nil 为无法确认已安装。
	Database map[string]any `json:"-"`
}

// Installed 报告能否确认该歌姬已安装（SV2：在本机的 SV 中用过；Flat：在声库目录中）。
func (s SVSinger) Installed() bool { return s.Database != nil }

// Range 返回推荐音域的 MIDI 音高；没有标注或无法解析时 ok 为 false。
func (s SVSinger) Range() (lo, hi int, ok bool) {
	a, b, found := strings.Cut(s.PitchRange, "-")
	if !found {
		return 0, 0, false
	}
	lo, ok1 := parseNoteName(strings.TrimSpace(a))
	hi, ok2 := parseNoteName(strings.TrimSpace(b))
	return lo, hi, ok1 && ok2 && lo <= hi
}

// parseNoteName 解析 "A#3"、"Db4" 这样的音名（60 = C4）。
func parseNoteName(s string) (int, bool) {
	if len(s) < 2 {
		return 0, false
	}
	pcs := map[byte]int{'C': 0, 'D': 2, 'E': 4, 'F': 5, 'G': 7, 'A': 9, 'B': 11}
	pc, ok := pcs[s[0]]
	if !ok {
		return 0, false
	}
	rest := s[1:]
	switch {
	case strings.HasPrefix(rest, "#"):
		pc, rest = pc+1, rest[1:]
	case strings.HasPrefix(rest, "b"):
		pc, rest = pc-1, rest[1:]
	}
	if len(rest) != 1 || rest[0] < '0' || rest[0] > '9' {
		return 0, false
	}
	return 12*(int(rest[0]-'0')+1) + pc, true
}

// SVDataDir 返回 Synthesizer V Studio 2 的数据目录（不保证存在）。
func SVDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Dreamtonics", "Synthesizer V Studio 2")
}

// ScanSVSingers 列出 dataDir 下的歌姬：本机用过的在前，其余按会唱普通话优先、再按名称排序。
// 目录不存在（没装 SV）时返回 nil；单个文件损坏时跳过。
func ScanSVSingers(dataDir string) []SVSinger {
	byName := map[string]*SVSinger{}
	metas, _ := filepath.Glob(filepath.Join(dataDir, "databases", "meta", "*.json"))
	for _, f := range metas {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s SVSinger
		if json.Unmarshal(data, &s) == nil && s.Name != "" {
			byName[s.Name] = &s
		}
	}
	// 恢复工程按文件名（日期目录 / 时间）排序，较新的覆盖较旧的 database 字段。
	recs, _ := filepath.Glob(filepath.Join(dataDir, "recovery", "*", "*.svp"))
	slices.Sort(recs)
	for _, f := range recs {
		for _, db := range svpDatabases(f) {
			name, _ := db["name"].(string)
			s := byName[name]
			if s == nil {
				s = &SVSinger{Name: name}
				byName[name] = s
			}
			if lang, _ := db["language"].(string); lang != "" && len(s.Languages) == 0 {
				s.Languages = []string{lang}
			}
			s.Database = db
		}
	}
	if len(byName) == 0 {
		return nil
	}
	out := make([]SVSinger, 0, len(byName))
	for _, s := range byName {
		out = append(out, *s)
	}
	rank := func(s SVSinger) int {
		switch {
		case s.Installed():
			return 0
		case slices.Contains(s.Languages, "mandarin"):
			return 1
		}
		return 2
	}
	slices.SortFunc(out, func(a, b SVSinger) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// svpDatabases 返回工程文件中各音轨（主音符组与其他音符组的引用）的 database 字段（name 非空的）。
func svpDatabases(path string) []map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = bytes.TrimPrefix(bytes.TrimRight(data, "\x00"), []byte{0xEF, 0xBB, 0xBF})
	var p struct {
		Tracks []struct {
			MainRef struct {
				Database map[string]any `json:"database"`
			} `json:"mainRef"`
			Groups []struct {
				Database map[string]any `json:"database"`
			} `json:"groups"`
		} `json:"tracks"`
	}
	if json.Unmarshal(data, &p) != nil {
		return nil
	}
	var out []map[string]any
	add := func(db map[string]any) {
		if name, _ := db["name"].(string); name != "" {
			out = append(out, db)
		}
	}
	for _, t := range p.Tracks {
		add(t.MainRef.Database)
		for _, g := range t.Groups {
			add(g.Database)
		}
	}
	return out
}

// SuggestTranspose 返回把旋律移进歌姬推荐音域的移调（半音，12 的倍数，保持调性）：旋律音高的中位数
// 离音域中点超过半个八度时按八度移动，否则为 0。歌姬没有标注音域或没有旋律时为 0。
func (a *Analysis) SuggestTranspose(s SVSinger) int {
	lo, hi, ok := s.Range()
	if !ok || a.Melody == nil || len(a.Melody.Notes) == 0 {
		return 0
	}
	pitches := make([]float64, len(a.Melody.Notes))
	for i, n := range a.Melody.Notes {
		pitches[i] = n.Pitch
	}
	mid := float64(lo+hi) / 2
	return 12 * int(math.Round((mid-median(pitches))/12))
}
