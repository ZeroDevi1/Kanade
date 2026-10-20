# Windows 测试清单

macOS 只做冒烟测试（引擎能启动、界面链路通、错误提示正确）；推理与训练的实测在 Windows + NVIDIA GPU 上进行。
本清单列出 macOS 上**没有覆盖**、需要在 Windows 验证的项目，按依赖顺序排列。

构建：在 macOS 上执行 `go tool mygo build -platform windows/amd64`，产物为 `build/windows-amd64/Kanade.exe`。
（Windows 安装包需要 NSIS，目前只验证裸 exe。）

## 0. 外壳与 Conductor

- [ ] 窗口显示 Mica 材质、隐藏标题栏，右上角系统按钮可用，标题栏可拖动。
- [ ] 设置 → 推理引擎 → 安装 dev-mock：uv 若不在 PATH 会自动下载（校验 SHA-256），安装过程**不弹出黑色控制台窗口**。
- [ ] 设置 → 环境检查：各引擎显示 `CUDA 12.x` 与显卡型号；显示 CPU 说明驱动或 torch 构建有问题。
  - CosyVoice：torch 2.3.1+cu121；分轨：torch 2.14.1+cu126；RVC：torch 2.11.0+cu128。
  - RTX 50 系列：分轨引擎的 cu126 构建不支持 Blackwell，需要把 `engines/stems/pyproject.toml` 的索引改为 cu128 / cu130。
- [ ] 数据根目录在 OneDrive「文档」下时，设置页显示云同步警告。
- [ ] 用户名或数据目录含中文 / 空格时，安装与推理正常。
- [ ] 推理过程中用任务管理器结束 Kanade：Python worker 随之退出（Job Object），显存释放。

## 1. 语音生成（CosyVoice）

- [ ] 下载 Fun-CosyVoice 3（ModelScope 与 Hugging Face 各试一次；勾选 hf-mirror 后 Hugging Face 走镜像）。
- [ ] Fun-CosyVoice 3：零样本克隆、跨语种复刻（含 `[laughter]` `[breath]`）、自然语言控制（方言、情绪、语速预设）。
- [ ] CosyVoice 2：同上三种模式。
- [ ] CosyVoice 1 SFT 模型：预训练音色（7 个内置音色）；Instruct 模型：音色 + 英文人物描述指令。
- [ ] 声音转换（CosyVoice 1 基础模型）。
- [ ] 已保存音色：在不同模型间复用；修改后重新注册。
- [ ] 长文本（多句）进度逐句更新，可中途取消。

## 2. 说话人微调（CosyVoice 3 SFT）

- [ ] 数据准备：数据集目录带同名 `.list`；输出「训练 N 条，验证 M 条」及跳过统计。
- [ ] 数据准备：没有 `.list` 时选择 Whisper 转写（small / large-v3）。
- [ ] 训练（只训 LLM，先用 2–3 轮试跑）：进度按轮更新；gloo 后端单进程启动成功；可取消且训练进程树被结束。
- [ ] 训练完成后语音生成页出现「<音色名>（微调音色）」，预训练音色模式可合成；风格引导可用。
- [ ] 训练 LLM + Flow。
- 已知风险：上游训练代码没有在 Windows 上正式验证过，gloo + `--use_amp` 组合可能需要调整。

## 3. 分轨

- [ ] 下载 5 个模型（约 5.1 GB；网络中断后能续传）。
- [ ] 整首歌跑完整净化链，输出 6 个文件，后缀与 VoiceLab 一致：
  `_other` `_vocals` `_vocals_karaoke` `_vocals_other` `_vocals_karaoke_noreverb` `_vocals_karaoke_noreverb_dry`。
- [ ] 只勾选「降噪」时自动补上前置步骤（干声、去和声、去混响）。
- [ ] MP3 / FLAC / M4A 输入（ffmpeg 预转换）。

## 4. 音色转换（RVC）

- [ ] 模型 → 导入 VoiceLab 训练的模型（`.pth` + `.index`），转换一段说话人声，三档预设都试。
- [ ] 歌声：「更干净」预设 + crepe；立体声输入分别试 mono / pan / dual。
- [ ] 翻唱：整首歌走「分轨 → 转换 → 混音」，混入和声、调节和声增益。
- [ ] 训练：带 `.list` 的数据集（白名单生效）；48k、batch 4（6 GB 显存时用 2）；中途取消；勾选跳过预处理与提取后续训到更多轮。
- [ ] 训练产物 `<名称>_<轮>e_<步>s.pth` 与 `<名称>.index` 出现在模型列表。

## macOS 冒烟已覆盖

- 四个引擎在 macOS 上的 uv 锁定与安装、hello 握手、环境检查（MPS / CPU）。
- CosyVoice-300M 的零样本、跨语种、声音转换、音色复用（CPU）。
- 混音任务、各任务的参数错误提示、SFT 训练在无 CUDA 时的提示。
- RVC 训练的白名单与预处理（发现并修复了 Applio 在 MPS 上提取特征失败却返回成功的问题；macOS 上特征提取改用 CPU）。
