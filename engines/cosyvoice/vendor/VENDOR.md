# Vendored 上游源码

上游项目没有可安装的 Python 包，推理代码原样复制到这里，运行时由 `kanade_cosyvoice` 加入 `sys.path`。
不修改其中的文件；升级时整体替换并同步 `../pyproject.toml` 的依赖版本。

| 目录 | 上游 | 提交 | 许可证 |
|---|---|---|---|
| `cosyvoice/`（不含 `bin/`） | https://github.com/FunAudioLLM/CosyVoice | `074ca6dc9e80a2f424f1f74b48bdd7d3fea531cc`（2026-05-26） | Apache-2.0，见 `LICENSE.CosyVoice` |
| `matcha/` | https://github.com/shivammehta25/Matcha-TTS（CosyVoice 的子模块） | `dd9105b34bf2be2230f4aa1e4769fb586a3c824e` | MIT，见 `LICENSE.Matcha-TTS` |
