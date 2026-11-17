"""pytest 共享配置：把 scripts/ 加进 sys.path，并定位真实固件。"""
import importlib.util
import os
import sys
import tempfile
from pathlib import Path

FIRMWARE_PATCH = Path(__file__).resolve().parent.parent
SCRIPTS = FIRMWARE_PATCH / "scripts"
PROJECT = FIRMWARE_PATCH.parents[1]  # speaker-open-xiaoai-rebuild/
FIRMWARE_ROOTFS = PROJECT / "firmware" / "rootfs"
FIRMWARE_BIN = PROJECT / "firmware" / "lx06_1.94.13.bin"

# /tmp 是 512MB tmpfs，装不下多份 92MB 的 rootfs 副本：测试临时目录
# （pytest tmp_path 与 shell 测试里的 mktemp）一律改到 workspace 磁盘。
# 注意：tempfile.gettempdir() 的结果在进程启动时就被缓存，只改环境变量
# 不够，必须直接覆盖 tempfile.tempdir。
_PYTEST_TMP = FIRMWARE_PATCH / ".pytest-tmp"
_PYTEST_TMP.mkdir(exist_ok=True)
tempfile.tempdir = str(_PYTEST_TMP)
os.environ["TMPDIR"] = str(_PYTEST_TMP)

sys.path.insert(0, str(SCRIPTS))

# download-ota.py 文件名带连字符，不能直接 import：按路径加载并注册。
_spec = importlib.util.spec_from_file_location(
    "download_ota", SCRIPTS / "download-ota.py")
_mod = importlib.util.module_from_spec(_spec)
sys.modules["download_ota"] = _mod
_spec.loader.exec_module(_mod)
