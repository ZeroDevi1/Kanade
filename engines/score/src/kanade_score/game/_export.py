"""代替上游 deployment.context：本引擎只做 PyTorch 推理，不导出 ONNX，导出模式恒为关闭。"""


def is_export_mode() -> bool:
    return False
