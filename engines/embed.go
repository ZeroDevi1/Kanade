// Package engines 把 Python 推理引擎的源码与锁文件嵌入 Kanade 可执行文件。
//
// 每个子目录是一个 uv 项目：engine.json 描述引擎，pyproject.toml + uv.lock 锁定依赖，src/ 为源码；
// sdk/ 是各引擎通过 path 依赖共享的协议包。Conductor 首次使用时把它们释放到数据根目录再 uv sync。
// 只嵌入明确列出的文件，开发时在仓库内生成的 .venv 等不会被打包。
package engines

import "embed"

//go:embed sdk/pyproject.toml all:sdk/src
//go:embed dev-mock/engine.json dev-mock/pyproject.toml dev-mock/uv.lock all:dev-mock/src
//go:embed stems/engine.json stems/pyproject.toml stems/uv.lock all:stems/src
//go:embed timbre/engine.json timbre/pyproject.toml timbre/uv.lock all:timbre/src all:timbre/vendor
//go:embed score/engine.json score/pyproject.toml score/uv.lock all:score/src
//go:embed cosyvoice/engine.json cosyvoice/pyproject.toml cosyvoice/uv.lock all:cosyvoice/src all:cosyvoice/vendor
//go:embed qwentts/engine.json qwentts/pyproject.toml qwentts/uv.lock all:qwentts/src
//go:embed voxcpm/engine.json voxcpm/pyproject.toml voxcpm/uv.lock all:voxcpm/src
//go:embed indextts/engine.json indextts/pyproject.toml indextts/uv.lock all:indextts/src
//go:embed yingmusic/engine.json yingmusic/pyproject.toml yingmusic/uv.lock all:yingmusic/src
//go:embed soulx/engine.json soulx/pyproject.toml soulx/uv.lock all:soulx/src
//go:embed vevo2/engine.json vevo2/pyproject.toml vevo2/uv.lock all:vevo2/src
var FS embed.FS
