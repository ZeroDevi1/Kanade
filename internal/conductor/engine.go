package conductor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"kanade/internal/paths"
)

// Spec 是 engine.json 描述的一个推理引擎。
type Spec struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Python 是传给 uv sync --python 的版本，例如 "3.12"。
	Python string `json:"python"`
	// Module 是 worker 入口，以 python -m <Module> 启动。
	Module string   `json:"module"`
	Tasks  []string `json:"tasks"`
}

// sharedDirs 是所有引擎共用、随每个引擎一起释放的目录。
var sharedDirs = []string{"sdk"}

// 引擎目录中的标记文件：stampFile 记录已释放源码的哈希，syncedFile 记录最后一次 uv sync 成功时的哈希。
// 两者与当前嵌入源码的哈希一致时引擎才算就绪。
const (
	stampFile  = ".kanade-source"
	syncedFile = ".kanade-synced"
)

// LoadSpecs 读取 src 中所有含 engine.json 的顶层目录，按名称排序返回。
func LoadSpecs(src fs.FS) ([]Spec, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, err
	}
	var specs []Spec
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(src, path.Join(e.Name(), "engine.json"))
		if err != nil {
			continue // 没有 engine.json 的目录（如 sdk）不是引擎
		}
		var s Spec
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s/engine.json: %w", e.Name(), err)
		}
		if s.Name != e.Name() {
			return nil, fmt.Errorf("%s/engine.json: name %q 与目录名不一致", e.Name(), s.Name)
		}
		specs = append(specs, s)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// sourceHash 是引擎目录与共享目录全部嵌入文件（路径 + 内容）的 SHA-256，代表一个源码版本。
func sourceHash(src fs.FS, name string) (string, error) {
	h := sha256.New()
	for _, dir := range append([]string{name}, sharedDirs...) {
		err := walkFiles(src, dir, func(p string, data []byte) error {
			fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
			h.Write(data)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkFiles 按路径顺序遍历 dir 下的文件，跳过 __pycache__。
func walkFiles(src fs.FS, dir string, fn func(p string, data []byte) error) error {
	return fs.WalkDir(src, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "__pycache__" {
				return fs.SkipDir
			}
			return nil
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return fn(p, data)
	})
}

// Dir 返回引擎在数据根目录中的项目目录。
func Dir(l paths.Layout, name string) string { return filepath.Join(l.Engines, name) }

// VenvPython 返回引擎虚拟环境中 Python 解释器的路径。
func VenvPython(l paths.Layout, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(Dir(l, name), ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(Dir(l, name), ".venv", "bin", "python")
}

// Ready 报告引擎是否已按当前嵌入源码完成 uv sync，可以直接启动 worker。
func Ready(src fs.FS, l paths.Layout, name string) bool {
	want, err := sourceHash(src, name)
	if err != nil {
		return false
	}
	got, err := os.ReadFile(filepath.Join(Dir(l, name), syncedFile))
	if err != nil || strings.TrimSpace(string(got)) != want {
		return false
	}
	_, err = os.Stat(VenvPython(l, name))
	return err == nil
}

// Install 释放引擎源码并执行 uv sync，使引擎就绪；已就绪时直接返回。
//
// 源码哈希变化时先删除旧的 src 目录再释放，避免旧版本遗留的模块被导入；.venv 保留，由 uv sync 增量更新。
// uv 的输出逐行交给 logf。ctx 取消时终止 uv 并返回错误。
func Install(ctx context.Context, src fs.FS, l paths.Layout, uv *UV, spec Spec, logf func(string)) error {
	if Ready(src, l, spec.Name) {
		return nil
	}
	hash, err := sourceHash(src, spec.Name)
	if err != nil {
		return err
	}

	// 释放源码：引擎目录与共享目录保持仓库中的相对位置，pyproject 里的 ../sdk 路径依赖才能解析。
	stamp, _ := os.ReadFile(filepath.Join(Dir(l, spec.Name), stampFile))
	if strings.TrimSpace(string(stamp)) != hash {
		logf("释放引擎源码到 " + Dir(l, spec.Name))
		for _, dir := range append([]string{spec.Name}, sharedDirs...) {
			if err := os.RemoveAll(filepath.Join(l.Engines, dir, "src")); err != nil {
				return err
			}
			err := walkFiles(src, dir, func(p string, data []byte) error {
				dst := filepath.Join(l.Engines, filepath.FromSlash(p))
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return err
				}
				return os.WriteFile(dst, data, 0o644)
			})
			if err != nil {
				return fmt.Errorf("释放 %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(filepath.Join(Dir(l, spec.Name), stampFile), []byte(hash), 0o644); err != nil {
			return err
		}
	}

	// 同步虚拟环境：--frozen 严格按 uv.lock 安装，不重新解析依赖。
	logf(fmt.Sprintf("uv sync（Python %s）", spec.Python))
	cmd := exec.CommandContext(ctx, uv.Path, "sync", "--frozen", "--no-dev", "--python", spec.Python, "--project", Dir(l, spec.Name))
	cmd.Env = Env(l)
	hideWindow(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // uv 把进度和结果都写到 stderr，合并后按行转发
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 uv: %w", err)
	}
	forwardLines(out, logf)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("uv sync 失败: %w", err)
	}
	return os.WriteFile(filepath.Join(Dir(l, spec.Name), syncedFile), []byte(hash), 0o644)
}

// forwardLines 把 r 的每一行（去掉行尾空白）交给logf，直到 r 结束。
func forwardLines(r io.Reader, logf func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimRight(sc.Text(), " \r\t"); line != "" {
			logf(line)
		}
	}
}
