// Package settings 读写用户设置文件 settings.json（位于系统应用数据目录）。
package settings

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Settings 是持久化的用户设置。零值表示全部使用默认值。
type Settings struct {
	// DataRoot 为空时使用默认根目录（文档/Kanade）。修改后需重启应用生效，
	// 因为运行中的 Conductor 已按旧根目录启动 worker。
	DataRoot string `json:"dataRoot,omitempty"`
	// Mirror 为 MirrorCN 时 Hugging Face 下载走 hf-mirror.com；空串为直连。对之后启动的引擎生效。
	Mirror string `json:"mirror,omitempty"`
	// GitHubMirror 是 GitHub 下载的镜像前缀（例如 https://ghfast.top/），空串为直连；
	// 用于分轨模型与 uv 下载的 Python 运行时。
	GitHubMirror string `json:"githubMirror,omitempty"`
}

// MirrorCN 表示使用国内镜像。
const MirrorCN = "cn"

// HFMirror 是国内 Hugging Face 镜像（与 VoiceLab 的 cn profile 相同）。
const HFMirror = "https://hf-mirror.com"

// pythonBuildStandalone 是 uv 下载 Python 运行时的 GitHub 发布地址。
const pythonBuildStandalone = "https://github.com/astral-sh/python-build-standalone/releases/download"

// Env 返回镜像设置对应的环境变量，交给 Conductor 注入 uv 与引擎进程（变量含义见 engines/sdk/src/kanade_engine/download.py）。
func (s Settings) Env() []string {
	var env []string
	if s.Mirror == MirrorCN {
		env = append(env, "KANADE_HF_ENDPOINT="+HFMirror)
	}
	if gh := strings.TrimSpace(s.GitHubMirror); gh != "" {
		env = append(env, "KANADE_GITHUB_MIRROR="+gh,
			"UV_PYTHON_INSTALL_MIRROR="+strings.TrimRight(gh, "/")+"/"+pythonBuildStandalone)
	}
	return env
}

// FileName 是设置文件在应用数据目录中的名称。
const FileName = "settings.json"

// Load 读取 dir 下的设置文件；文件不存在时返回零值且无错误，格式错误时返回错误。
func Load(dir string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// Save 把设置写入 dir：先写临时文件再重命名，避免写到一半崩溃留下损坏的文件。
func Save(dir string, s Settings) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, FileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, FileName))
}
