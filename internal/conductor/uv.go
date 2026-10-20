package conductor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"kanade/internal/paths"
)

// UVVersion 是下载时锁定的 uv 版本。升级时同步更新 uvChecksums（取自对应 Release 的 *.sha256 文件）。
const UVVersion = "0.13.1"

// uvChecksums 是 UVVersion 各平台发布包的 SHA-256，键为 GOOS/GOARCH。
var uvChecksums = map[string]struct{ asset, sha256 string }{
	"darwin/arm64":  {"uv-aarch64-apple-darwin.tar.gz", "cfe454573412ff7380d15fc7dae5997ab53ffce72fce0a78e91ddbc84001acd2"},
	"darwin/amd64":  {"uv-x86_64-apple-darwin.tar.gz", "bb7b805bdcf3d0bc8c74ceb94569e6b3e8aa35bd492acdbb9d46bf46d9d4ff6b"},
	"windows/amd64": {"uv-x86_64-pc-windows-msvc.zip", "3b2661a463a5baea7f5076ea2b25ab71d77f36a6c0d809e5350651792f80a672"},
	"windows/arm64": {"uv-aarch64-pc-windows-msvc.zip", "ec7846a5939b4654afee24f2d82bf6fde71bd7a6952d7748f1135ab301dd0a49"},
	"linux/amd64":   {"uv-x86_64-unknown-linux-gnu.tar.gz", "eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220"},
	"linux/arm64":   {"uv-aarch64-unknown-linux-gnu.tar.gz", "9041c73c99fdc5892cf128140beb354eca9231d502068cc36e6cb53a8e59ad14"},
}

// uvReleaseBase 是 uv 发布包的下载地址前缀；测试中替换为本地服务器。
var uvReleaseBase = "https://github.com/astral-sh/uv/releases/download/"

// UVSource 说明正在使用的 uv 从何而来。
type UVSource string

const (
	UVFromPath       UVSource = "PATH"
	UVFromDownloaded UVSource = "downloaded"
)

// UV 是一个已定位、可执行的 uv。
type UV struct {
	Path   string
	Source UVSource
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// FindUV 按优先级查找 uv：PATH 中的 uv，其次是根目录下已下载的 uv。都没有时返回 nil。
func FindUV(l paths.Layout) *UV {
	if p, err := exec.LookPath("uv"); err == nil {
		return &UV{Path: p, Source: UVFromPath}
	}
	p := filepath.Join(l.UVDir, exeName("uv"))
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return &UV{Path: p, Source: UVFromDownloaded}
	}
	return nil
}

// DownloadUV 下载 UVVersion 的 uv 到 l.UVDir，校验 SHA-256 后解压，返回下载的 uv。
//
// 当前平台不在 uvChecksums 中、下载失败、校验不符或压缩包内缺少 uv 时返回错误，且不改动 l.UVDir。
// 压缩包约 20 MB，全部读入内存后再校验，校验通过前不写任何文件。
func DownloadUV(ctx context.Context, l paths.Layout) (*UV, error) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	pkg, ok := uvChecksums[key]
	if !ok {
		return nil, fmt.Errorf("uv %s 没有 %s 的发布包", UVVersion, key)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uvReleaseBase+UVVersion+"/"+pkg.asset, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 uv: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 uv: HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("下载 uv: %w", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != pkg.sha256 {
		return nil, fmt.Errorf("uv 校验失败：期望 %s，实际 %s", pkg.sha256, got)
	}

	want := exeName("uv")
	var bin []byte
	if strings.HasSuffix(pkg.asset, ".zip") {
		bin, err = fromZip(data, want)
	} else {
		bin, err = fromTarGz(data, want)
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.UVDir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(l.UVDir, want)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	return &UV{Path: dst, Source: UVFromDownloaded}, nil
}

// fromZip 返回 zip 中名为 name 的文件内容（Windows 发布包的文件位于压缩包根目录）。
func fromZip(data []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("uv 压缩包中没有 %s", name)
}

// fromTarGz 返回 tar.gz 中名为 name 的文件内容（macOS / Linux 发布包放在 uv-<target>/ 子目录中）。
func fromTarGz(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("uv 压缩包中没有 %s", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name {
			return io.ReadAll(tr)
		}
	}
}

// Env 返回运行 uv 及引擎 Python 时使用的环境变量：把缓存、Python 运行时固定到数据根目录，
// 并强制 UTF-8，避免 Windows 中文用户名路径与非 UTF-8 控制台编码导致的解码错误。
func Env(l paths.Layout) []string {
	return append(os.Environ(),
		"UV_CACHE_DIR="+l.UVCache,
		"UV_PYTHON_INSTALL_DIR="+l.PythonDir,
		// 只用 uv 管理的 Python，避免不同机器上系统 Python 版本不一致。
		"UV_PYTHON_PREFERENCE=only-managed",
		"UV_NO_PROGRESS=1",
		"PYTHONUTF8=1",
		"PYTHONIOENCODING=utf-8",
		"PYTHONUNBUFFERED=1",
		// 引擎据此把模型与第三方库缓存（modelscope、huggingface）放到数据根目录。
		"KANADE_MODELS_DIR="+l.Models,
	)
}

// Version 运行 uv --version 并返回其输出（去掉首尾空白）。
func (u *UV) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, u.Path, "--version").Output()
	return strings.TrimSpace(string(out)), err
}
