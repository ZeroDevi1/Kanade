package score

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 导出的目标软件：Synthesizer V Studio 2 与 Synthesizer V Studio Flat（见 svsingers.go 的说明）。两者都能打开
// 本程序写出的 135 版本工程（Flat 自己保存为 153）；歌姬列表、预设与「导出后打开」按目标区分。

// SVTarget 是一个检测到的目标软件。
type SVTarget struct {
	Key   string // "sv2" / "flat"：预设键的前缀等
	Title string
	Exe   string // 可执行文件；找不到时为空（不提供「导出后打开」）
	// Singers 是该软件的歌姬，按 ScanSVSingers / ScanFlatSingers 的顺序。
	Singers []SVSinger
}

// DetectSVTargets 返回本机装了的目标软件（有数据目录且读到了歌姬），Flat 在前（声库多，且都已安装）。
func DetectSVTargets() []SVTarget {
	sv2 := ScanSVSingers(SVDataDir())
	var out []SVTarget
	if dir := FlatDataDir(); dir != "" {
		if singers := ScanFlatSingers(dir, sv2); len(singers) > 0 {
			out = append(out, SVTarget{Key: "flat", Title: "Synthesizer V Studio Flat",
				Exe: firstExisting(programFiles("Synthesizer V Studio Flat", "synthv-flat.exe")), Singers: singers})
		}
	}
	if len(sv2) > 0 {
		out = append(out, SVTarget{Key: "sv2", Title: "Synthesizer V Studio 2",
			Exe: firstExisting(programFiles("Synthesizer V Studio 2*", "synthv-studio.exe")), Singers: sv2})
	}
	return out
}

// programFiles 返回 Program Files 下匹配 dir（可含通配符）的目录中的 exe 路径（Windows 以外为空）。
func programFiles(dir, exe string) []string {
	var out []string
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
		if root == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, dir, exe))
		out = append(out, matches...)
	}
	return out
}

func firstExisting(paths []string) string {
	for _, p := range paths {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// FlatDataDir 返回 Synthesizer V Studio Flat 的数据目录（含 databases 子目录）；找不到时为空。
// Flat 把 SV 1.x 的 Dreamtonics 数据目录移到了 文档\OPSV 下；也接受 SV 1.x 原本的 文档\Dreamtonics 位置。
func FlatDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, d := range []string{
		filepath.Join(home, "Documents", "OPSV", "Dreamtonics", "Synthesizer V Studio"),
		filepath.Join(home, "Documents", "Dreamtonics", "Synthesizer V Studio"),
	} {
		if st, err := os.Stat(filepath.Join(d, "databases")); err == nil && st.IsDir() {
			return d
		}
	}
	return ""
}

// flatInfo 是 Flat 声库目录中 info.<版本>.nofs 的字段（只取用到的）。
type flatInfo struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Vendor    string   `json:"vendor"`
	Language  string   `json:"language"`
	Phoneset  string   `json:"phoneset"`
	Supported []string `json:"support_languages"`
	Styles    []struct {
		Name string `json:"name"`
	} `json:"styles"`
}

// ScanFlatSingers 列出 dataDir/databases 下的声库（同一声库有多个版本时取版本号最大的），按名称排序；
// catalog（SV2 的商店目录，可为 nil）用来按名称补上音域与曲风。
func ScanFlatSingers(dataDir string, catalog []SVSinger) []SVSinger {
	// 先按原名精确匹配，查不到再按去掉后缀的键（同一角色的其他版本，音域相同或相近）。
	byName, byKey := map[string]SVSinger{}, map[string]SVSinger{}
	for _, s := range catalog {
		if s.PitchRange == "" {
			continue
		}
		byName[s.Name] = s
		if k := singerKey(s.Name); byKey[k].Name == "" {
			byKey[k] = s
		}
	}
	dirs, _ := os.ReadDir(filepath.Join(dataDir, "databases"))
	var out []SVSinger
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		info, ok := newestFlatInfo(filepath.Join(dataDir, "databases", d.Name()))
		if !ok {
			continue
		}
		s := SVSinger{Name: info.Name, Vendor: info.Vendor, Languages: []string{info.Language},
			Database: map[string]any{"name": info.Name, "language": info.Language, "phoneset": info.Phoneset,
				"backendType": "", "version": info.Version}}
		for _, l := range info.Supported {
			if !slices.Contains(s.Languages, l) {
				s.Languages = append(s.Languages, l)
			}
		}
		for _, st := range info.Styles {
			if !strings.HasPrefix(st.Name, "(") {
				s.Modes = append(s.Modes, st.Name)
			}
		}
		c, ok := byName[info.Name]
		if !ok {
			c, ok = byKey[singerKey(info.Name)]
		}
		if ok {
			s.PitchRange, s.Genres = c.PitchRange, c.Genres
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b SVSinger) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// newestFlatInfo 读取声库目录中版本号最大的 info.<版本>.nofs。
func newestFlatInfo(dir string) (flatInfo, bool) {
	files, _ := filepath.Glob(filepath.Join(dir, "info.*.nofs"))
	best, bestVer := "", -1
	for _, f := range files {
		// 版本形如 200 或 200a0：按开头的数字比较，相同时取文件名较大的。
		ver := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "info."), ".nofs")
		end := strings.IndexFunc(ver, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(ver)
		}
		n, _ := strconv.Atoi(ver[:end])
		if n > bestVer || n == bestVer && f > best {
			best, bestVer = f, n
		}
	}
	if best == "" {
		return flatInfo{}, false
	}
	data, err := os.ReadFile(best)
	if err != nil {
		return flatInfo{}, false
	}
	var info flatInfo
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if json.Unmarshal(data, &info) != nil || info.Name == "" {
		return flatInfo{}, false
	}
	return info, true
}

var (
	// singerSuffix 是声库名中表示版本 / 引擎的后缀：AI、PLUS、Arcane、版本号、Deluxe / Standard Edition。
	singerSuffix = regexp.MustCompile(`(?i)\s+(ai|plus|arcane|deluxe edition|standard edition|\d+)$`)
	singerParen  = regexp.MustCompile(`\s*\(.*?\)`)
)

// singerKey 是歌姬名的匹配键：去掉括号内容与 singerSuffix（可重复）并转小写，同一角色的不同版本得到相同的键。
func singerKey(name string) string {
	k := strings.TrimSpace(singerParen.ReplaceAllString(name, ""))
	for {
		next := strings.TrimSpace(singerSuffix.ReplaceAllString(k, ""))
		if next == k {
			break
		}
		k = next
	}
	return strings.ToLower(k)
}
