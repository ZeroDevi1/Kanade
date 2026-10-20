# Windows 测试清单

macOS 只做冒烟测试（引擎能启动、界面链路通、错误提示正确）；推理与训练的实测在 Windows + NVIDIA GPU 上进行。
本清单列出 macOS 上**没有覆盖**、需要在 Windows 验证的项目，按依赖顺序排列。

构建：执行 `go tool mygo build -platform windows/amd64`，产物为 `build/windows-amd64/Kanade.exe`。
（Windows 上直接构建也会产出 NSIS 安装包 `Kanade Setup <版本> amd64.exe`，目前只验证裸 exe。）

2026-10-20 在 Windows 11 + RTX 5060（8 GB，Blackwell）上完成了一轮实测，结果标在各项后面；
分轨模型的评估见 [stems-evaluation.md](stems-evaluation.md)。

## 0. 外壳与 Conductor

- [x] 窗口显示 Mica 材质、隐藏标题栏，右上角系统按钮可用，标题栏可拖动。
- [x] 设置 → 推理引擎 → 安装 dev-mock：uv 若不在 PATH 会自动下载（校验 SHA-256），安装过程**不弹出黑色控制台窗口**。
  （实测机器的 PATH 中已有 uv，自动下载只由单元测试覆盖。）
- [x] 设置 → 环境检查：各引擎显示 CUDA 版本与显卡型号；显示 CPU 说明驱动或 torch 构建有问题。
  - CosyVoice：torch 2.7.1+cu128；分轨：torch 2.14.1+cu130；RVC：torch 2.11.0+cu128。均支持 RTX 50（sm_120）。
  - 原先的 CosyVoice 2.3.1+cu121 与分轨 cu126 不含 sm_120，已升级；cu130 要求驱动 580 及以上，cu128 要求 570 及以上。
  - 曾发现：worker 主线程阻塞读 stdin 时，任务线程 import numpy / torch 会卡死（Windows 同步管道上的 GetFileType），
    界面上表现为环境检查与所有 GPU 任务一直不返回；已改为 PeekNamedPipe 轮询（`kanade_engine/runtime.py`）。
- [x] 数据根目录在 OneDrive「文档」下时，设置页显示云同步警告。（用名为 `OneDrive-…` 的本地目录验证，未写入真实 OneDrive。）
- [x] 用户名或数据目录含中文 / 空格时，安装与推理正常。
  - 曾发现：RVC 模型名含中文时 faiss 无法写入 / 读取 `.index`（推理端还会静默忽略索引），已由 `kanade_timbre/faiss_compat.py` 修复。
- [x] 推理过程中用任务管理器结束 Kanade：Python worker 随之退出（Job Object），显存释放。（用 `taskkill /F` 验证）
- 注意：应用没有单实例保护，重复启动会出现两个实例，各自启动 worker 争用 GPU。

## 1. 语音生成（CosyVoice）

- [x] 下载 Fun-CosyVoice 3（ModelScope 与 Hugging Face 各试一次；勾选 hf-mirror 后 Hugging Face 走镜像）。
  - 曾发现：Hugging Face 的 resolve 地址返回 308，Python 3.10 的 urllib 不跟随，下载报 `HTTP Error 308`；SDK 已补上 308 处理。
- [x] Fun-CosyVoice 3：零样本克隆、跨语种复刻（含 `[laughter]` `[breath]`）、自然语言控制（方言、情绪、语速预设）。
- [x] CosyVoice 2：同上三种模式。
  - 曾发现：自然语言控制 + 已保存音色时，会把参考音频的文本也念出来（上游 spk2info 缓存的 prompt_text 覆盖了指令）；已改为不复用缓存。
- [x] CosyVoice 1 SFT 模型：预训练音色（7 个内置音色）；Instruct 模型：音色 + 英文人物描述指令。
- [x] 声音转换（CosyVoice 1 基础模型）。
- [x] 已保存音色：在不同模型间复用；修改后重新注册（未单独测修改后重新注册）。
- [x] 长文本（多句）进度逐句更新，可中途取消。（取消在句与句之间生效，最长要等当前一句合成完。）

## 2. 说话人微调（CosyVoice 3 SFT）

- [x] 数据准备：数据集目录带同名 `.list`；输出「训练 N 条，验证 M 条」及跳过统计。（炫狗：训练 359、验证 21）
- [ ] 数据准备：没有 `.list` 时选择 Whisper 转写（small / large-v3）。（未测）
- [x] 训练（只训 LLM，先用 2–3 轮试跑）：进度按轮更新；gloo 后端单进程启动成功；可取消且训练进程树被结束。
  （炫狗 32 分钟数据：约 5 分钟/轮，瓶颈在 CPU 端在线提取语音 token；取消未单独在 SFT 上测。）
- [~] 训练完成后语音生成页出现「<音色名>（微调音色）」，预训练音色模式可合成；风格引导可用。
  （引擎层已验证预训练音色模式合成；界面列表与风格引导未在界面上验证。）
  - 实测：第 10 轮时显卡驱动重置（系统日志 nvlddmkm 153），训练以 `CUDA error: unknown error` 退出；
    已保存的检查点可直接手动平均、组装，但界面没有「从已有检查点组装」的入口。
- [ ] 训练 LLM + Flow。（未测）
- 实测：gloo + `--use_amp` 单进程可以训练。8 GB 显卡上曾依次遇到并已修复：
  - deepspeed 占位模块没有 `__spec__`，transformers 探测可选依赖时报错 → 改为 `_stubs/deepspeed` 真实文件；
  - DataLoader worker 以 spawn 启动后找不到占位模块 → 同上（子进程继承 sys.path）；
  - 推理下载跳过了训练需要的 `speech_tokenizer_v3.batch.onnx` → 训练前按需补下；
  - torch 2.7 的 ProcessGroup 没有 `options`，`cosyvoice_join` 每个 batch 报错 → 单进程时跳过该 barrier；
  - 轮数少于 3 时 `average_model.py` 断言失败 → 平均数量取 min(3, 已有轮数)；
  - LLM 6.4 亿参数全量 Adam 约需 10 GB 显存 → 新增「LLM 训练范围」，默认只训顶部 12 层（可训练 191M）。

## 3. 分轨

- [x] 下载 5 个模型（约 5.1 GB；网络中断后能续传）。（下载中途结束 worker，重新下载从 `.part` 续传）
- [x] 整首歌跑完整净化链，输出 6 个文件，后缀与 VoiceLab 一致（4 分 27 秒的歌约 7.5 分钟）：
  `_other` `_vocals` `_vocals_karaoke` `_vocals_other` `_vocals_karaoke_noreverb` `_vocals_karaoke_noreverb_dry`。
- [x] 只勾选「降噪」时自动补上前置步骤（干声、去和声、去混响）。（引擎层验证）
- [x] MP3 / FLAC / M4A 输入（ffmpeg 预转换）。
  - 曾发现：文件对话框只列出 wav/flac/mp3/ogg，选不到 M4A；分轨页与翻唱的「歌曲」已改为列出全部支持格式。

## 4. 音色转换（RVC）

- [ ] 模型 → 导入 VoiceLab 训练的模型（`.pth` + `.index`），转换一段说话人声，三档预设都试。（没有 VoiceLab 模型，未测）
- [~] 歌声：「更干净」预设 + crepe；立体声输入分别试 mono / pan / dual。（只测了「更干净」参数 + rmvpe + pan）
- [x] 翻唱：整首歌走「分轨 → 转换 → 混音」，混入和声、调节和声增益。（界面上完整跑通一次）
- [x] 训练：带 `.list` 的数据集（白名单生效）；48k、batch 4（6 GB 显存时用 2）；中途取消；勾选跳过预处理与提取后续训到更多轮。
  （RTX 5060：约 65 秒/轮、显存约 3.6 GB；与分轨同时运行时显存超出 8 GB，速度降到几分之一。）
- [x] 训练产物 `<名称>_<轮>e_<步>s.pth` 与 `<名称>.index` 出现在模型列表。

## macOS 冒烟已覆盖

- 四个引擎在 macOS 上的 uv 锁定与安装、hello 握手、环境检查（MPS / CPU）。
- CosyVoice-300M 的零样本、跨语种、声音转换、音色复用（CPU）。
- 混音任务、各任务的参数错误提示、SFT 训练在无 CUDA 时的提示。
- RVC 训练的白名单与预处理（发现并修复了 Applio 在 MPS 上提取特征失败却返回成功的问题；macOS 上特征提取改用 CPU）。
