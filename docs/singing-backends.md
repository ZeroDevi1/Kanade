# 其他歌声改词后端：SoulX SVS / Vevo2 AR+FM

「歌声改词」页现在可选择 YingMusic、SoulX SVS、Vevo2 AR+FM。三者使用不同的、用户预先准备的上游 Python 环境和本地模型，互不污染 RVC 或彼此的依赖。切换后保留各自页面输入与结果；任务运行中阻止跨后端重复提交，取消或结束后可再切换。配置目前只保存在页面状态，应用重启后需重新填写。

**这是重新合成，不保证歌词、旋律、时序、音色完全保真。首次请测试短片段，试听后再保存。仅使用获授权的音频及音色。** 本实现及测试没有下载模型权重，没有进行真实 GPU 合成、显存或音质验收。环境检查只确认本地文件、配置和依赖可读，不代表模型能装入显存或真实推理成功。

## SoulX-Singer：明确使用 SVS，而非 SVC

核验版本：[SoulX-Singer 81aeb3a](https://github.com/Soul-AILab/SoulX-Singer/tree/81aeb3ae772c70093c3de74dc23c92d983801ae4)。模型：[Soul-AILab/SoulX-Singer](https://huggingface.co/Soul-AILab/SoulX-Singer/tree/40493ad90286056c7a9095035164434a79daa8c9)，使用 **`model.pt`**，不是 `model-svc.pt`。

### 环境准备

按该版本上游说明准备独立 Python 3.10 环境及 `requirements.txt`，在本地保存完整源码（含配置和 phoneset）及 SVS `model.pt`。依赖版本和 CUDA 必须由部署者匹配；本集成不代装系统依赖或下载权重。默认设备 CUDA；界面可显式选择 CPU，但其实际速度与模型兼容性未测试。不会下载上游预处理模型。

上游 `build_model` 使用 `torch.load(weights_only=False)`，因此 **只加载可信来源的官方 checkpoint**；该格式可执行 pickle 内容。Doctor 不加载权重，不能判定 checkpoint 安全。

### 实际改词工作流

1. 使用已有「分轨」得到干净人声。在上游预处理或兼容编辑器中准备参考和目标 metadata，并**手工校准**原词、音素、音符、时长与对齐；本次集成没有内置自动转谱/对齐编辑器。
2. 音色参考音频必须预先裁成一个完整片段；对应参考 JSON 仅一个 segment，`time` 从 0 开始。不要直接用整曲音频配中段 metadata——上游 DataProcessor 不会按起始时间自动裁剪。
3. 目标 JSON 必须实际修改新歌词对应的 `phoneme`，并按需要调整 `duration`、`note_pitch`、`note_type`。**只改 JSON 的 `text` 字段不会改词**，模型不读取该字段。
4. 默认 `score` 控制读取已校准的音符；`melody` 控制还要求参考和目标两份 metadata 都有对齐的 F0。默认 `auto_shift=False`，不按参考中位音高自动转调。
5. 选择参考干声、参考 JSON、目标 JSON，检查环境并生成。输出为 24 kHz `vocal.wav`；当前 SoulX 路径不自动混伴奏，可导出后另行对齐混音。

metadata 是 JSON segment 数组：
- `time`: `[start_ms,end_ms]`，毫秒；目标段必须排序、不重叠。
- `duration`: 空格分隔的每音符时长，单位秒。
- `phoneme`、`note_pitch`、`note_type`: 空格分隔字符串，条目数与 `duration` 一致。英文一个音符内可用 `en_HH-ER0` 表示多个底层音素；不是每字母/每底层音素分配一个时长。
- 常见类型 `1` 静音、`2` 正常音符、`3` 连音；音素必须在上游 phoneset 中。支持 `<SP>`/`<AP>`。
- `f0`: 空格分隔的 Hz 数值，50 Hz 帧率，melody 模式必填。

请优先从上游 `example/audio/*.json` 和官方预处理/编辑器获得真实格式，勿将随意的歌词文本当作 metadata。本适配器限制单段最多 120 秒、总时间轴最多 1 小时、每份 JSON 最多 16 MiB。合成段按目标绝对 `time` 放回时间轴：比段长短则补零，长则裁剪；务必试听边界。更多 API 参数见 [`engines/soulx/README.md`](../engines/soulx/README.md)。

SoulX 官方代码及模型标注 Apache-2.0；保留来源/许可并另行核对部署依赖许可。这不替代音频/音色权利人的授权。

## Vevo2：明确使用 AR + FM 编辑，而非 FM-only 换音色

核验源码：[Amphion 26f6883](https://github.com/open-mmlab/Amphion/tree/26f6883110181f1dbfe95c70a7c7dbaf4de5f42a)，模型：[RMSnow/Vevo2](https://huggingface.co/RMSnow/Vevo2/tree/2674843cbaa50aa89ee7ccaf5bb15d6ccf46c6c8)。

### 环境与模型

准备独立 Python 3.10 Amphion 推理环境，包含 Vevo2 所需完整依赖。上游 Vevo2 子目录 requirements 仅列少量附加依赖，**不能据此承诺只安装三个包就可运行**。Torch、Transformers、音频处理依赖及系统库仍需在实际设备验证。

模型目录必须保留官方结构：AR `contentstyle_modeling/posttrained`、FM `acoustic_modeling/fm_emilia101k_singnet7k_repa`、两种 tokenizer 和 `vocoder` 的推理权重/配置；详细必需文件列表见 [`engines/vevo2/README.md`](../engines/vevo2/README.md)。不要求训练优化器状态，也不自动拉整个 HF 仓库。

额外明确提供本地 **Whisper `medium.pt`**。Whisper 统计文件默认来自 FM 目录 `whisper_stats.pt`，可指定本地覆盖路径。上游代码硬编码 `whisper.load_model('medium')` 会自动下载，因此本适配器在独立子进程内把该调用限定到已验证的本地 `medium.pt`，任务结束后还原 loader；不能只靠 HF 离线环境变量。

### 编辑流程与许可

- **Vevo2 权重是 CC-BY-NC-ND 4.0**，与 Amphion 代码许可不同。界面必须主动勾选已阅读限制才能检查/运行；勾选不产生商用或衍生权重分发授权，也不把该许可自动解释为所有输出的统一法律结论。商业用途或再分发需向权利人核实，不要默认允许。
- 选择原曲干净主唱，填写其实际原歌词和目标新歌词。参考原词不能用新词替代或自动猜测。
- 原唱同时用作 prosody/style/timbre 参考，调用 `inference_ar_and_fm`，`use_prosody_code=True`、`use_pitch_shift=False`；不是 `inference_fm`。
- 输出 24 kHz `vocal.wav`；可选伴奏生成 `mixed.wav`。混音从起点开始，重采样、短轨补零并进行削波保护，**不自动修复生成节奏漂移或延迟**。

两个适配器均不会调用上游 snapshot 自动下载包装器，设置 HF/Transformers 离线模式；Vevo2 AR 限定本地加载且禁用 remote custom code。取消会终止独立推理进程。失败详情在任务输出目录 `soulx.log` / `vevo2.log`。

## 开发验证命令

```sh
CGO_ENABLED=0 go test ./...
go tool mygo vet .
PYTHONPATH=engines/sdk/src:engines/soulx/src python -m unittest discover -s engines/soulx/tests -v
PYTHONPATH=engines/sdk/src:engines/vevo2/src python -m unittest discover -s engines/vevo2/tests -v
uv lock --check --offline --project engines/soulx
uv lock --check --offline --project engines/vevo2
```

契约测试、离线加载检查和 fake-model 测试不是实际模型质量验收。发布后仍需授权样本、完整本地模型及目标设备，检验歌词准确率、音高/节奏偏差、音色相似度、取消释放显存及混音对齐。
