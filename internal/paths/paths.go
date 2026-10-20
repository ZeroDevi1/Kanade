// Package paths 定义 Kanade 数据根目录下的目录布局。
//
// 根目录默认为「文档/Kanade」，存放 uv、Python 运行时、各引擎虚拟环境、模型与输出，体积可达数十 GB；
// 小型配置（指向根目录的 settings.json）放在系统的应用数据目录，不随根目录迁移。
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Layout 是根目录下各子目录的绝对路径，由 New 一次性推导，之后只读。
type Layout struct {
	Root string
	// UVDir 存放下载的 uv 可执行文件（PATH 中已有 uv 时不使用）。
	UVDir string
	// PythonDir 是 UV_PYTHON_INSTALL_DIR，uv 管理的 Python 解释器装在这里。
	PythonDir string
	// UVCache 是 UV_CACHE_DIR；与 Engines 同盘，uv 才能用硬链接安装依赖，节省空间。
	UVCache string
	// Engines 下每个子目录是一个引擎的 uv 项目，其 .venv 由 uv sync 生成。
	Engines string
	Models  string
	Outputs string
	Logs    string
}

// DirName 是默认根目录在「文档」下的名称。
const DirName = "Kanade"

// New 返回以 root 为根的目录布局，不创建任何目录。
func New(root string) Layout {
	return Layout{
		Root:      root,
		UVDir:     filepath.Join(root, "runtime", "uv"),
		PythonDir: filepath.Join(root, "runtime", "python"),
		UVCache:   filepath.Join(root, "cache", "uv"),
		Engines:   filepath.Join(root, "engines"),
		Models:    filepath.Join(root, "models"),
		Outputs:   filepath.Join(root, "outputs"),
		Logs:      filepath.Join(root, "logs"),
	}
}

// Ensure 创建布局中的所有目录。
func (l Layout) Ensure() error {
	for _, d := range []string{l.UVDir, l.PythonDir, l.UVCache, l.Engines, l.Models, l.Outputs, l.Logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// SyncedFolderRisk 报告 root 是否位于常见的云同步目录中（OneDrive、iCloud Drive、Dropbox）。
//
// 这类目录会把虚拟环境与模型上传到云端，并可能把文件替换为占位符导致 Python 导入失败。
// 判断基于路径片段，属于启发式：返回 true 时应提示用户，而不是自动迁移。
func SyncedFolderRisk(root string) bool {
	// 同时按 / 与 \ 拆分：filepath.ToSlash 只转换当前系统的分隔符，设置文件可能来自另一平台。
	parts := strings.FieldsFunc(root, func(r rune) bool { return r == '/' || r == '\\' })
	for _, part := range parts {
		p := strings.ToLower(part)
		if strings.HasPrefix(p, "onedrive") || p == "mobile documents" || p == "dropbox" {
			return true
		}
	}
	return false
}
