"""unpack.sh / patch.sh 的测试：用真实 lx06_1.94.13.bin 在 /tmp 临时目录
完整重跑，并与 REA_ANALYSIS.md 记录的数值核对。

注意：unpack（unblob）与 patch（mksquashfs 37MB）都比较慢，模块级
fixture 只解包一次，patch 端到端用同一份解包结果。
"""
import os
import re
import shutil
import subprocess

import pytest

from conftest import (
    FIRMWARE_BIN,
    FIRMWARE_PATCH,
    FIRMWARE_ROOTFS,
    SCRIPTS,
    require_firmware_bin,
    require_patched_work,
)

MAX_IMAGE_SIZE = 0x02800000  # 40MB
EXPECTED_SQFS_SIZE = 37500699  # REA_ANALYSIS.md：root.squashfs 字节数
EXPECTED_SQFS_INODES = 2137


def run(cmd, env_extra=None, timeout=900):
    env = dict(os.environ)
    env.pop("SSH_PASSWORD", None)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(cmd, capture_output=True, text=True,
                          timeout=timeout, env=env)


@pytest.fixture(scope="module")
def unpacked_work(tmp_path_factory):
    """在 /tmp 临时 work 目录里用真实 .bin 跑 unpack.sh（模块级，只跑一次）。"""
    require_firmware_bin()  # CI 干净 checkout 里无官方 .bin：依赖测试跳过
    work = tmp_path_factory.mktemp("work")
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    env = {"PATCH_WORK_DIR": str(work)}
    # unblob 位置：优先 env.sh 里的 REA_UNBLOB_COMMAND，否则 PATH
    rea_unblob = os.path.expanduser("~/workspace/tools/rea-venv/bin/unblob")
    if os.path.exists(rea_unblob):
        env["REA_UNBLOB_COMMAND"] = rea_unblob
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra=env, timeout=1200)
    assert r.returncode == 0, f"unpack.sh 失败：\n{r.stdout}\n{r.stderr}"
    return work


# --- bash -n 与 py_compile ---

def test_shell_scripts_syntax():
    for name in ("patch.sh", "unpack.sh"):
        r = run(["bash", "-n", str(SCRIPTS / name)])
        assert r.returncode == 0, f"{name} bash -n 失败：{r.stderr}"


def test_python_files_compile():
    for name in ("patch_firmware.py", "download-ota.py"):
        r = run(["python3", "-m", "py_compile", str(SCRIPTS / name)])
        assert r.returncode == 0, f"{name} py_compile 失败：{r.stderr}"


# --- unpack.sh：真实解包 + REA 数值核对 ---

def test_unpack_real_firmware(unpacked_work):
    out = unpacked_work / "lx06_1.94.13"
    sqfs = out / "root.squashfs"
    assert sqfs.exists()
    assert sqfs.stat().st_size == EXPECTED_SQFS_SIZE
    mico = out / "mico_version"
    assert mico.exists()
    text = mico.read_text(errors="replace")
    assert "1.94.13" in text
    assert "LX06" in text
    rootfs = out / "rootfs"
    assert rootfs.is_dir()
    assert (rootfs / "etc/init.d/dropbear").exists()
    assert (rootfs / "bin/ota").exists()
    assert (rootfs / "usr/lib/libxaudio_engine.so").exists()


def test_unpack_squashfs_header_matches_rea(unpacked_work):
    sqfs = unpacked_work / "lx06_1.94.13" / "root.squashfs"
    r = run(["unsquashfs", "-s", str(sqfs)])
    assert r.returncode == 0, r.stderr
    out = r.stdout + r.stderr
    assert "xz" in out
    assert str(EXPECTED_SQFS_INODES) in out  # 2137 inodes
    r2 = run(["file", str(sqfs)])
    assert "Squashfs" in r2.stdout
    assert "xz" in r2.stdout
    assert "131072" in r2.stdout  # blocksize


def test_unpack_fails_without_bin(tmp_path):
    work = tmp_path / "work"
    work.mkdir()
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)})
    assert r.returncode != 0
    assert not (work / "lx06_1.94.13" / "rootfs").exists()


def test_unpack_with_ota_bin_env(tmp_path):
    """OTA_BIN 可直接指定已有的官方包路径，不必先拷进 work 目录。"""
    require_firmware_bin()  # CI 干净 checkout 里无官方 .bin：跳过
    work = tmp_path / "work2"
    work.mkdir()
    env = {"PATCH_WORK_DIR": str(work), "OTA_BIN": str(FIRMWARE_BIN)}
    rea_unblob = os.path.expanduser("~/workspace/tools/rea-venv/bin/unblob")
    if os.path.exists(rea_unblob):
        env["REA_UNBLOB_COMMAND"] = rea_unblob
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra=env, timeout=1200)
    assert r.returncode == 0, f"unpack.sh（OTA_BIN）失败：\n{r.stdout}\n{r.stderr}"
    sqfs = work / "lx06_1.94.13" / "root.squashfs"
    assert sqfs.exists()
    assert sqfs.stat().st_size == EXPECTED_SQFS_SIZE
    assert (work / "lx06_1.94.13" / "mico_version").exists()


# --- patch.sh：端到端 ---

def test_patch_sh_end_to_end(unpacked_work, tmp_path):
    work = unpacked_work
    # P0-2：patch.sh → patch_firmware.py 不再有默认密码，必须显式给强密码。
    test_pw = "test-password-0123456789"
    env = {"PATCH_WORK_DIR": str(work), "SSH_PASSWORD": test_pw}
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "1.94.13"],
            env_extra=env, timeout=1200)
    assert r.returncode == 0, f"patch.sh 失败：\n{r.stdout}\n{r.stderr}"
    assert "打包完成" in r.stdout
    out = work / "lx06_1.94.13" / "root-patched.squashfs"
    assert out.exists()
    size = out.stat().st_size
    assert 0 < size < MAX_IMAGE_SIZE, f"补丁镜像 {size} 字节，不在 (0, 40MB) 内"

    # 解开验证关键补丁都在
    verify = tmp_path / "verify"
    r2 = run(["unsquashfs", "-d", str(verify), "-f", str(out)], timeout=600)
    assert r2.returncode == 0, r2.stderr
    dropbear = (verify / "etc/init.d/dropbear").read_text()
    assert "mkdir -p /data/etc/dropbear" in dropbear
    assert "micocfg_work_mode" not in dropbear
    assert (verify / "etc/inittab").read_text().count(
        "ttyS0::askfirst:/bin/login") == 1
    assert "ttyS0::askfirst:/bin/ash --login" not in \
        (verify / "etc/inittab").read_text()
    ota = (verify / "bin/ota").read_text()
    for func_re in (r"^download_upgrade\(\) \{$", r"^upgrade\(\) \{$"):
        m = re.search(func_re, ota, re.MULTILINE)
        assert m is not None, func_re
        assert ota[m.end():m.end() + 60].splitlines()[1].strip() == "return 1"
    flash = (verify / "bin/flash.sh").read_text()
    m = re.search(r"^upgrade_param_check\(\) \{$", flash, re.MULTILINE)
    assert m is not None
    assert flash[m.end():m.end() + 60].splitlines()[1].strip() == "return 1"
    wireless = (verify / "etc/init.d/wireless").read_text()
    idx = wireless.index("    identity=$4\n")
    assert 'echo "download_fail"' in wireless[idx:idx + 120]
    rc = (verify / "etc/rc.local").read_text()
    # P2-34：rc.local 走包装器，不再直接执行 /data/init.sh。
    wrapper_line = ('[ -x /etc/open-xiaoai/init.sh ] '
                    '&& /etc/open-xiaoai/init.sh >/dev/null 2>&1 &')
    assert wrapper_line in rc
    assert '[ -f "/data/init.sh" ] && sh /data/init.sh' not in rc
    wrapper = (verify / "etc/open-xiaoai/init.sh").read_text()
    assert "sha256sum" in wrapper and "ls -ln" in wrapper
    assert "init.sh.sha256" in wrapper
    # P2-33：asound 原厂备份在镜像里（.bak 是原厂内容，不含补丁）。
    for rel in ("etc/asound.conf", "etc/asound.conf.dts"):
        bak = verify / (rel + ".bak")
        assert bak.is_file(), f"缺 {rel}.bak"
        assert "slave.pcm Capture" not in bak.read_text()
    asound = (verify / "etc/asound.conf").read_text()
    assert "slave.pcm Capture" in asound
    assert "period_size 384" in asound
    assert "buffer_size 6144" in asound
    lib = (verify / "usr/lib/libxaudio_engine.so").read_bytes()
    assert b"hw:0,3" not in lib
    assert lib[773628:773634] == b"noop\x00\x00"
    # root 密码已换成 sha512crypt（$6$，随机盐），且能用测试密码验证通过。
    shadow = (verify / "etc/shadow").read_text()
    m = re.search(r"^root:(\$6\$[^:]+):", shadow, re.MULTILINE)
    assert m, f"shadow 里 root 行不是 $6$ hash: {shadow.splitlines()[0]!r}"
    import crypt
    assert crypt.crypt(test_pw, m.group(1)) == m.group(1)


def test_patch_sh_does_not_modify_source_rootfs(unpacked_work):
    """patch.sh 只改 work 里的副本；firmware/rootfs 原物与解包 rootfs 都不动。"""
    orig_shadow = (FIRMWARE_ROOTFS / "etc/shadow").read_text()
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in orig_shadow
    unpacked_shadow = (unpacked_work / "lx06_1.94.13" / "rootfs"
                       / "etc/shadow").read_text()
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in unpacked_shadow


def test_patch_sh_rejects_bad_args(tmp_path):
    work = tmp_path / "work"
    work.mkdir()
    env = {"PATCH_WORK_DIR": str(work)}
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx99", "1.94.13"],
            env_extra=env)
    assert r.returncode != 0
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "not-a-version"],
            env_extra=env)
    assert r.returncode != 0
    # 无 rootfs 时失败，且不留下半成品镜像
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "1.94.13"],
            env_extra=env)
    assert r.returncode != 0
    assert not (work / "lx06_1.94.13" / "root-patched.squashfs").exists()


# --- P0-5：unpack.sh / patch.sh 入口白名单哈希校验 ---

def _tamper(path, offset=1000):
    data = bytearray(path.read_bytes())
    data[offset] ^= 0xFF
    path.write_bytes(bytes(data))


# P1-8 测试替身 unblob：不真的解固件，只按 -e 参数建 extract 目录，并按
# FAKE_SQFS_COUNT / FAKE_MICO_TEXT 放文件，供 unpack.sh 的 find 恰好一个
# 与 mico_version 版本比对走完整流程（白名单校验仍用真实 .bin）。
FAKE_UNBLOB = """#!/usr/bin/env bash
set -u
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-e" ]; then out="$a"; fi
  prev="$a"
done
[ -n "$out" ] || exit 1
mkdir -p "$out/a" "$out/b"
n="${FAKE_SQFS_COUNT:-1}"
i=0
while [ "$i" -lt "$n" ]; do
  d="$out"
  [ "$i" -gt 0 ] && d="$out/a"
  : > "$d/root.squashfs"
  i=$((i + 1))
done
printf '%s' "$FAKE_MICO_TEXT" > "$out/mico_version"
"""


def _write_fake_unblob(path):
    path.write_text(FAKE_UNBLOB)
    path.chmod(0o755)
    return str(path)


def test_unpack_rejects_unknown_version(tmp_path):
    """P0-5：不在白名单的版本拒绝解包（发任何解包动作之前）。"""
    work = tmp_path / "work"
    work.mkdir()
    (work / "lx06_9.9.9.bin").write_bytes(b"fake")
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "9.9.9"],
            env_extra={"PATCH_WORK_DIR": str(work)})
    assert r.returncode != 0
    assert "白名单" in (r.stdout + r.stderr)
    assert not (work / "lx06_9.9.9" / "rootfs").exists()


def test_unpack_rejects_tampered_bin(tmp_path):
    """P0-5：.bin 被改过一个字节就拒绝解包，且不产出 rootfs。"""
    require_firmware_bin()  # CI 干净 checkout 里无官方 .bin：跳过
    work = tmp_path / "work"
    work.mkdir()
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    _tamper(work / "lx06_1.94.13.bin")
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)})
    assert r.returncode != 0
    assert "白名单" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "rootfs").exists()


# --- P1-8：find 任选 / 旧产物不清 / mico_version 不比对 ---

def test_unpack_rejects_multiple_root_sqfs(tmp_path):
    """P1-8：extract 里 carve 出多个 root.squashfs 时拒绝任选，直接失败。"""
    require_firmware_bin()  # 白名单校验仍需真实 .bin：CI 里跳过
    work = tmp_path / "work"
    work.mkdir()
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    fake = _write_fake_unblob(tmp_path / "fake-unblob.sh")
    env = {"PATCH_WORK_DIR": str(work), "REA_UNBLOB_COMMAND": fake,
           "FAKE_SQFS_COUNT": "2", "FAKE_MICO_TEXT": "LX06\n1.94.13\n"}
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra=env)
    assert r.returncode != 0
    assert "多个" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "rootfs").exists()


def test_unpack_rejects_mico_version_mismatch(tmp_path):
    """P1-8：mico_version 里没有入参版本时拒绝解包。"""
    require_firmware_bin()  # 白名单校验仍需真实 .bin：CI 里跳过
    work = tmp_path / "work"
    work.mkdir()
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    fake = _write_fake_unblob(tmp_path / "fake-unblob.sh")
    env = {"PATCH_WORK_DIR": str(work), "REA_UNBLOB_COMMAND": fake,
           "FAKE_SQFS_COUNT": "1", "FAKE_MICO_TEXT": "LX06\n1.94.12\n"}
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra=env)
    assert r.returncode != 0
    assert "mico_version" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "rootfs").exists()


def test_unpack_cleans_stale_products_before_unblob(tmp_path):
    """P1-8：重跑先清旧产物——旧 rootfs / extract / 旧补丁镜像都不残留。"""
    require_firmware_bin()  # 白名单校验仍需真实 .bin：CI 里跳过
    work = tmp_path / "work"
    work.mkdir()
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    out = work / "lx06_1.94.13"
    (out / "rootfs").mkdir(parents=True)
    (out / "rootfs" / "SENTINEL").write_text("stale")
    (work / "lx06_1.94.13-extract").mkdir()
    (work / "lx06_1.94.13-extract" / "SENTINEL").write_text("stale")
    (out / "root-patched.squashfs").write_bytes(b"stale-patched")
    fake = _write_fake_unblob(tmp_path / "fake-unblob.sh")
    env = {"PATCH_WORK_DIR": str(work), "REA_UNBLOB_COMMAND": fake,
           "FAKE_SQFS_COUNT": "1", "FAKE_MICO_TEXT": "LX06\n1.94.13\n"}
    r = run(["bash", str(SCRIPTS / "unpack.sh"), "lx06", "1.94.13"],
            env_extra=env)
    # 假 sqfs 解不开 unsquashfs，本次运行必然失败；关键是旧产物已被清掉。
    assert r.returncode != 0
    assert not (out / "rootfs" / "SENTINEL").exists()
    assert not (work / "lx06_1.94.13-extract" / "SENTINEL").exists()
    assert not (out / "root-patched.squashfs").exists()


def test_patch_rejects_tampered_bin(unpacked_work, tmp_path):
    """P0-5：.bin 被改过就拒绝打补丁，且不留下半成品镜像。"""
    work = tmp_path / "work"
    work.mkdir()
    # symlinks=True：解包出的 rootfs 里有悬空软链（如 etc/resolv.conf
    # 指向 /tmp 下的目标），跟随复制会 ENOENT；保留软链原样复制才行。
    shutil.copytree(unpacked_work / "lx06_1.94.13", work / "lx06_1.94.13",
                    symlinks=True)
    shutil.rmtree(work / "lx06_1.94.13" / "squashfs-root", ignore_errors=True)
    shutil.copyfile(FIRMWARE_BIN, work / "lx06_1.94.13.bin")
    _tamper(work / "lx06_1.94.13.bin")
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)}, timeout=300)
    assert r.returncode != 0
    assert "白名单" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "root-patched.squashfs").exists()


def test_patch_rejects_tampered_sqfs_when_bin_absent(unpacked_work, tmp_path):
    """P0-5：.bin 不在时退而校验原始 root.squashfs；被改过就拒绝打补丁。"""
    work = tmp_path / "work"
    work.mkdir()
    # symlinks=True：理由同 test_patch_rejects_tampered_bin。
    shutil.copytree(unpacked_work / "lx06_1.94.13", work / "lx06_1.94.13",
                    symlinks=True)
    shutil.rmtree(work / "lx06_1.94.13" / "squashfs-root", ignore_errors=True)
    _tamper(work / "lx06_1.94.13" / "root.squashfs")
    # work 里没有 .bin：patch.sh 必须走 sqfs 校验分支并失败
    assert not (work / "lx06_1.94.13.bin").exists()
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)}, timeout=300)
    assert r.returncode != 0
    assert "白名单" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "root-patched.squashfs").exists()


def test_patch_rejects_multiple_orig_sqfs(unpacked_work, tmp_path):
    """P1-8：$DIR/root.squashfs 缺失时回退到 $DIR-extract；多个同名文件即失败。"""
    work = tmp_path / "work"
    work.mkdir()
    shutil.copytree(unpacked_work / "lx06_1.94.13", work / "lx06_1.94.13",
                    symlinks=True)
    shutil.rmtree(work / "lx06_1.94.13" / "squashfs-root", ignore_errors=True)
    (work / "lx06_1.94.13" / "root.squashfs").unlink()
    extract = work / "lx06_1.94.13-extract"
    (extract / "a").mkdir(parents=True)
    (extract / "root.squashfs").write_bytes(b"fake1")
    (extract / "a" / "root.squashfs").write_bytes(b"fake2")
    r = run(["bash", str(SCRIPTS / "patch.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)}, timeout=300)
    assert r.returncode != 0
    assert "多个" in (r.stdout + r.stderr)
    assert not (work / "lx06_1.94.13" / "root-patched.squashfs").exists()


# --- P0-6：flash-check.sh 干跑检查 ---

def test_flash_check_ok_on_real_work():
    """P0-6：真实 work 目录干跑检查通过（lx06 与 oh2p）。"""
    require_patched_work()  # work/ 是 gitignore 的本地构建产物：CI 里跳过
    for model, version in (("lx06", "1.94.13"), ("oh2p", "1.58.6")):
        r = run(["bash", str(SCRIPTS / "flash-check.sh"), model, version])
        assert r.returncode == 0, f"flash-check.sh {model} 失败：\n{r.stdout}\n{r.stderr}"
        assert "干跑检查通过" in r.stdout
        assert "BLOCKED" in r.stdout


def test_flash_check_rejects_tampered_image(tmp_path):
    """P0-6：补丁镜像首字节被改，干跑检查失败（魔数 + 超块解析）。"""
    require_patched_work()  # work/ 是 gitignore 的本地构建产物：CI 里跳过
    work = tmp_path / "work"
    (work / "lx06_1.94.13").mkdir(parents=True)
    for name in ("root.squashfs", "mico_version"):
        shutil.copyfile(FIRMWARE_PATCH / "work" / "lx06_1.94.13" / name,
                        work / "lx06_1.94.13" / name)
    patched = work / "lx06_1.94.13" / "root-patched.squashfs"
    # 用仓库 work 里的已交付补丁镜像，不依赖 end-to-end 测试先跑过。
    shutil.copyfile(FIRMWARE_PATCH / "work" / "lx06_1.94.13"
                    / "root-patched.squashfs", patched)
    _tamper(patched, offset=0)
    r = run(["bash", str(SCRIPTS / "flash-check.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)})
    assert r.returncode != 0
    assert "FAIL" in r.stdout
    assert "干跑检查失败" in r.stdout


def test_flash_check_fails_without_patched_image(tmp_path):
    """P0-6：没有补丁镜像时干跑检查失败。"""
    work = tmp_path / "work"
    (work / "lx06_1.94.13").mkdir(parents=True)
    r = run(["bash", str(SCRIPTS / "flash-check.sh"), "lx06", "1.94.13"],
            env_extra={"PATCH_WORK_DIR": str(work)})
    assert r.returncode != 0
    assert "找不到补丁镜像" in r.stdout
