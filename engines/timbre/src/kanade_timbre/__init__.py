"""Kanade 的音色转换引擎（Timbre）：RVC 推理、训练与混音，核心代码 vendor 自 Applio。

任务（参数由 Go 端 internal/timbre 组装）：

- timbre.infer  {model, index?, input, pitch, f0_method, index_rate, protect, volume_envelope,
                 stereo_mode, normalize}：转换一段人声。
- timbre.train  {name, dataset_dir, list?, sample_rate, f0_method, epochs, batch_size, save_every,
                 skip_preprocess?, skip_extract?}：预处理 → 提取 f0 / 特征 → 训练（自动续训）→ 建索引。
- timbre.mix    {tracks: [{path, gain_db}], name}：把转换后的人声与伴奏等混合为一个文件。
- timbre.prepare {train?, sample_rate?}：预下载推理（及训练）所需的底模与预测器。

Applio 以工作目录解析模型路径（rvc/models/...），并在导入时记录 os.getcwd()，因此必须在导入 rvc 之前
切换到运行目录。运行目录与训练目录都在数据根目录下（见 runtime.py），代码只读地留在引擎目录的 vendor 中。
"""

from __future__ import annotations

from kanade_engine import serve

from . import runtime

ENGINE = "timbre"
VERSION = "0.1.0"


def main() -> None:
    runtime.setup()
    from . import infer, mix, train

    serve(ENGINE, VERSION, {
        "timbre.prepare": train.run_prepare,
        "timbre.infer": infer.run_infer,
        "timbre.train": train.run_train,
        "timbre.mix": mix.run_mix,
    })
