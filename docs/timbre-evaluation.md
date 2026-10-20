# Timbre（人声音色转换）方案评估

日期：2026-10-20。仓库状态取自 GitHub API 当日查询；依赖取自各项目 `main` 分支的 requirements。

## 结论

**Timbre 以 Applio（RVC 推理核心）为主引擎，Seed-VC 类零样本方案暂不接入，so-vits-svc 不采用。**

| 方案 | 类型 | 维护状态 | 许可证 | 结论 |
|---|---|---|---|---|
| Applio | RVC v2 推理 + 训练，整理后的工程化分支 | 活跃（v3.7.0，2026-10-16 发布） | MIT | **采用**：作为 `timbre-rvc` 引擎的推理核心 |
| RVC-Project WebUI | RVC 原版 | 活跃（2.3.260718），已提供 py3.12 依赖 | MIT | 备选：模型格式与 Applio 相同，作为兼容性参照 |
| rvc-python | RVC 推理封装库 | 停滞（最后提交 2024-10） | MIT | 不采用：跟不上 torch / Python 版本 |
| so-vits-svc | SVC（扩散 / VITS） | **已归档**（2023-11） | **AGPL-3.0** | 不采用：停止维护，AGPL 与随应用分发的方式冲突 |
| DDSP-SVC | DDSP + 扩散的 SVC | 活跃 | MIT | 观察：每个音色需单独训练，推理资源小，可作为第二引擎 |
| Seed-VC | 零样本歌声转换 | **已归档**（2025-04） | GPL-3.0 | 暂不接入：零样本效果有吸引力，但已停更且 GPL；如需零样本 VC，先用 CosyVoice 1 的声音转换 |
| GPT-SoVITS | 少样本 TTS（约 1 分钟数据微调） | 活跃 | MIT | 归入 Cantus 评估：与 CosyVoice 功能重叠，优势是少样本微调 |

## 选择 Applio 的理由

1. **依赖可控**：去掉了 RVC 原版的 fairseq（HuBERT 改由 transformers 加载），依赖是现代版本
   （torch 2.11、numpy 2.x、transformers 5），uv 双平台锁定的风险比原版小。
2. **模型生态兼容**：社区的 RVC v2 音色模型（`.pth` + `.index`）可直接使用，Kanade 只需要模型管理界面。
3. **推理能力完整**：音高算法（rmvpe、fcpe、crepe）、index 检索比例、音高偏移、保护辅音、
   音量包络混合等参数齐全，正好对应 Timbre 页面需要的控件。
4. **许可证**：MIT，可以像 CosyVoice 那样 vendor 推理代码。

## 接入要点（M4）

- **引擎**：新建 `engines/timbre-rvc`，独立 uv 环境。Applio 用 torch 2.11（Windows 用 cu12x 索引），
  不能和 CosyVoice 的 torch 2.3.1 共存，正好验证「每个引擎一个 venv」的设计。
- **只 vendor 推理部分**：`rvc/infer`、`rvc/lib` 及所需配置，不要 gradio、tensorboard、edge-tts、
  训练脚本。Windows 依赖里的 onnxruntime-gpu 和 nvidia-* 运行时包按 marker 保留。
- **底模**：HuBERT / ContentVec 与 rmvpe 权重首次使用时下载到 `models/rvc/`，
  做法与 CosyVoice 下载器相同（按文件筛选、断点续传）。
- **整首歌曲流水线**：Stems（分轨）→ Timbre（只处理人声）→ 与伴奏混音，由 Go 端串起 3 个任务。
  因此 M3 Stems 仍排在 M4 之前。
- **训练**：Applio 支持训练，但需要 GPU 和较长时间，先只做推理，训练放到之后单独评估。

## 待验证

- Applio 的 torch 2.11 + Windows CUDA 组合下 `uv lock` 能否只用预编译 wheel 完成解析（关注 faiss-cpu、webrtcvad）。
- rmvpe 在 macOS CPU 上的速度能否满足冒烟测试。
- DDSP-SVC 与 Applio 在同一首歌上的主观对比（需要 Windows GPU 环境）。
