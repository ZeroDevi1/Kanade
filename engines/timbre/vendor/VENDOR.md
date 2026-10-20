# Vendored 上游源码

Applio 不是可安装的 Python 包，推理与训练代码原样复制到这里，运行时由 `kanade_timbre.runtime` 加入 `sys.path`。
不修改其中的文件；升级时整体替换并同步 `../pyproject.toml` 的依赖版本。

| 目录 | 上游 | 提交 | 许可证 |
|---|---|---|---|
| `rvc/`（不含 `realtime/`）、`assets/config_template.json` | https://github.com/IAHispano/Applio | `5171e1dcf31a33d1afeb14ef7c6aac6dadae5dc1`（2026-10-19） | MIT，见 `LICENSE.Applio` |
| `logs/mute`、`logs/reference`（仅 contentvec 部分） | 同上：训练用的静音样本与试听参考特征 | 同上 | 同上 |
