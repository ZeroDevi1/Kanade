# 歌声改词：YingMusic-Singer-Plus（可选引擎）

此功能重新合成歌声，不是 RVC 换音色。输入干净主唱旋律、音色参考及其**实际原歌词**、目标新歌词；输出 `vocal.wav`（44.1 kHz）及可选 `mixed.wav`。旋律、时长、发音和音色只是模型条件，不保证完全保留；先用短片段试听。仅处理获授权的录音及音色。

## 独立环境与本地权重

Kanade 内的 `yingmusic` 引擎仅依赖共享 SDK。真正推理在用户预先准备的独立 Python 进程运行，不往 timbre/RVC 环境安装包，不调用上游自动下载入口。此改动不附带、也不会自动下载模型权重。

验证过的上游源码版本：[baa409c2e7e5e775f09b4e92a220808f4827d2cc](https://github.com/ASLP-lab/YingMusic-Singer-Plus/tree/baa409c2e7e5e775f09b4e92a220808f4827d2cc)。部署者需自行准备：

1. 上游完整源码 checkout；不要只复制 `infer_api.py`。词典、g2p 资源和配置有相对路径。
2. 独立 Python 3.10 环境，按该版本上游 `requirements.txt` 安装依赖；其 torch/torchaudio 固定为 2.6.0，torchvision 0.21.0。上游 Dockerfile 还要求 `espeak-ng`、`libespeak-ng1`、`ffmpeg`、`libsndfile1`。安装方式按操作系统适配，本集成不代装系统组件。
3. [ASLP-lab/YingMusic-Singer](https://huggingface.co/ASLP-lab/YingMusic-Singer) 的本地推理快照：`config.json` 和 `model.safetensors`（约 2.91 GB）。这是实际 API 使用的模型 ID，不是 Singer-Plus。无需下载 8 GB 训练 checkpoint；本功能也不需要额外分离模型。配置引用的源码文件必须存在。
4. 在「歌声改词」填写 Python 解释器绝对路径、上游源码根目录、模型快照目录，点击「检查本地环境」。路径随页面状态保留，当前不写入全局设置。

子进程以源码根目录为 cwd，设 `HF_HUB_OFFLINE=1`、`TRANSFORMERS_OFFLINE=1`、`HF_DATASETS_OFFLINE=1`，调用 `YingMusicSinger.from_pretrained(local_dir, local_files_only=True)`。不运行上游 `get_model()` / `initialization.download_files()`。Doctor 验证配置及依赖导入，不加载权重，不代表 GPU 可用、显存足够或生成质量达标。CUDA 可用则使用 CUDA，否则 CPU；未实测最低显存及 CPU 性能。

## 操作流程

1. 先在已有「分轨」页得到干净主唱和伴奏；保存需要的文件。
2. 选择主唱作为旋律参考。音色参考可留空（同一主唱）或选择另一段已授权的干净歌声。
3. 填写**音色参考音频**实际唱出的原词。若音色参考留空，则填写旋律参考原词；不能用新词冒充原词。填写目标新词，使用 `|` 分句；多行也会转成分句。
4. 可选添加原伴奏。混音从两条轨道起点开始，不自动做时序对齐；重采样伴奏到 44.1 kHz，短轨补零，混合峰值超 1 时统一缩放。原人声文件仍单独保留，便于检查漂移或自行后期调整。
5. 生成、试听并通过现有结果列表保存。取消会终止推理子进程；失败日志在任务输出目录 `yingmusic.log`。当前不做自动分块或整曲对齐，建议先试短片段。

可在现有 RVC 页对生成干声另行换音色，但这不是必需步骤，也不承诺精准复刻原音色。

## 协议与验证

任务 `yingmusic.edit` 参数：`python_path`, `source_dir`, `model_dir`, `melody_audio`, `ref_audio`（可选）, `ref_text`, `target_text`, `accompaniment_audio`（可选）。高级 API 可传 `seed`（-1 随机或 0–2147483647）、`nfe_step`（1–128，默认 32）、`cfg_strength`（0–10，默认 3）。输出目录由 Conductor 提供。任务 `yingmusic.doctor` 只需前三个路径。

```sh
CGO_ENABLED=0 go test ./...
go tool mygo vet .
PYTHONPATH=engines/sdk/src:engines/yingmusic/src python -m unittest discover -s engines/yingmusic/tests -v
uv lock --check --project engines/yingmusic
```

Python 契约测试用 fake torch/模型和临时文件检查实际调用参数、离线加载、混音及取消，不等价于真实模型推理。真实验收还需用户的独立环境、完整权重及授权样本，比较改词准确率、旋律/节奏偏差、音色相似度和混音对齐。

## 许可与来源

- [YingMusic-Singer-Plus LICENSE](https://github.com/ASLP-lab/YingMusic-Singer-Plus/blob/baa409c2e7e5e775f09b4e92a220808f4827d2cc/LICENSE)：CC BY 4.0，保留 ASLP-lab / YingMusic 作者署名及来源。
- 使用的 Stable Audio VAE 另受 [LICENSE-STABILITY](https://github.com/ASLP-lab/YingMusic-Singer-Plus/blob/baa409c2e7e5e775f09b4e92a220808f4827d2cc/LICENSE-STABILITY) 约束。不能把整个模型栈描述为无条件商用；分发、商业使用需自行核对协议的 Notice、Powered by Stability AI、注册与收入门槛等要求。
- 本仓库仅提供 API 适配器，不复制上游模型代码或权重。实际部署/再分发应保留相关许可及 notices，并核对其他依赖的独立许可。
