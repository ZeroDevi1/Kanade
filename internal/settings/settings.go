// Package settings 读写用户设置文件 settings.json（位于系统应用数据目录）。
package settings

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Settings 是持久化的用户设置。零值表示全部使用默认值。
type Settings struct {
	// DataRoot 为空时使用默认根目录（文档/Kanade）。修改后需重启应用生效，
	// 因为运行中的 Conductor 已按旧根目录启动 worker。
	DataRoot string `json:"dataRoot,omitempty"`
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
