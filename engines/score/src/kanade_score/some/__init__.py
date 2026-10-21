"""SOME（Singing-Oriented MIDI Extractor）推理所需的最小代码，vendor 自 https://github.com/openvpi/SOME
（MIT，见同目录 LICENSE），提交 e523eef70777c1370199363de03eb37d66dce76e（2026-03-07）。

只取推理路径：模型 model.py（原 modules/model/Gmidi_conform.py）及其依赖 conform.py / attention.py / conv.py，
梅尔谱 spec.py（原 modules/rmvpe/spec.py），按静音切片 slicer.py（原 utils/slicer2.py），
后处理 decode.py（原 utils/infer_utils.py 去掉写 MIDI 文件的部分）。改动仅限把绝对导入改为包内相对导入、
删除 mido 导入；训练、ONNX 导出与 WebUI 相关代码未引入，因此不依赖 lightning / fairseq / h5py / gradio。
"""
