"""pytest 共享配置：把 scripts/ 加进 sys.path，并定位真实固件。"""
import importlib.util
import os
import sys
import tempfile
from pathlib import Path

import pytest

FIRMWARE_PATCH = Path(__file__).resolve().parent.parent
SCRIPTS = FIRMWARE_PATCH / "scripts"
PROJECT = FIRMWARE_PATCH.parents[1]  # speaker-open-xiaoai-rebuild/
FIRMWARE_ROOTFS = PROJECT / "firmware" / "rootfs"
FIRMWARE_BIN = PROJECT / "firmware" / "lx06_1.94.13.bin"

# --- 固件材料缺失时跳过（CI 用；见 .github/workflows/ci.yml） ---
# 固件材料（原厂 rootfs 解包目录、官方 .bin、work/ 已交付补丁镜像）都不在
# 仓库里：firmware/ 在仓库之外的 speaker-open-xiaoai-rebuild/ 下，
# firmware-patch/work/ 被 .gitignore 排除。干净 checkout（CI）里这些文件
# 不存在，依赖它们的测试必须显式跳过——带明确理由（pytest -rs 可见），
# 而不是 FileNotFoundError 炸掉整个 job。本地有材料时照常执行。
# 不删测试、不静默跳过、不把 rootfs/.bin 塞进仓库、不让 CI 现场下载解固件。
HAS_FIRMWARE_ROOTFS = FIRMWARE_ROOTFS.is_dir()
HAS_FIRMWARE_BIN = FIRMWARE_BIN.is_file()
PATCHED_WORK_IMAGES = {
    model: FIRMWARE_PATCH / "work" / f"{model}_{version}" / "root-patched.squashfs"
    for model, version in (("lx06", "1.94.13"), ("oh2p", "1.58.6"))
}
HAS_PATCHED_WORK = all(p.is_file() for p in PATCHED_WORK_IMAGES.values())


def require_firmware_rootfs():
    """依赖真实固件 rootfs 的测试入口：缺失时 pytest.skip（理由可见）。"""
    if not HAS_FIRMWARE_ROOTFS:
        pytest.skip(
            f"缺固件 rootfs fixture：{FIRMWARE_ROOTFS} 不存在——"
            "原厂固件不在仓库里；本地跑需先把固件解包到 "
            "speaker-open-xiaoai-rebuild/firmware/rootfs（见 firmware-patch/README.md）")


def require_firmware_bin():
    """依赖官方 .bin 的测试入口：缺失时 pytest.skip（理由可见）。"""
    if not HAS_FIRMWARE_BIN:
        pytest.skip(
            f"缺官方固件包 fixture：{FIRMWARE_BIN} 不存在——"
            "官方 .bin 不在仓库里；本地跑需先用 download-ota.py 下载到 "
            "speaker-open-xiaoai-rebuild/firmware/lx06_1.94.13.bin")


def require_patched_work():
    """依赖 work/ 已交付补丁镜像的测试入口：缺失时 pytest.skip（理由可见）。"""
    if not HAS_PATCHED_WORK:
        missing = [str(p) for p in PATCHED_WORK_IMAGES.values() if not p.is_file()]
        pytest.skip(
            "缺已交付补丁镜像 fixture（work/ 是 .gitignore 排除的本地构建产物）："
            + "、".join(missing)
            + "；本地跑需先跑 unpack.sh + patch.sh 产出补丁镜像")

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
