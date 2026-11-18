"""patch_firmware.py 的测试：在真实 firmware/rootfs 的副本上跑真实补丁，
断言每个被改文件的改后内容；另断言前置条件不满足时失败且不写输出。
"""
import hashlib
import io
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

import patch_firmware
from patch_firmware import (
    PatchError,
    _random_salt,
    check_asound,
    check_certs,
    check_disable_ota,
    check_libxaudio,
    check_login,
    check_ssh,
    check_start,
    find_libxaudio_offset,
    patch_all,
    patch_asound,
    patch_certs,
    patch_disable_ota,
    patch_libxaudio,
    patch_login,
    patch_ssh,
    patch_start,
    sha512_crypt,
    _write,
    _write_bytes,
)
from conftest import FIRMWARE_ROOTFS, SCRIPTS, require_firmware_rootfs

# 测试专用 root 密码（≥16 位，仅测试用；生产密码必须现场随机生成，
# 永不写进仓库——本常量只出现在测试里）。
TEST_SSH_PASSWORD = "test-password-0123456789"
os.environ["SSH_PASSWORD"] = TEST_SSH_PASSWORD

# openssl 实测基准：openssl passwd -6 -salt <salt> <pw>（与 sha512_crypt
# 逐字节一致，见 patch_firmware.py 头注释）。
OPENSSL_SHA512_VECTORS = [
    ("testpw", "abcdefghijklmnop",
     "$6$abcdefghijklmnop$N6qTiVZoBdrJ.R/wUkMfGcrb6WwbbgenZ1D1DI8mwGvn6PHXmN59VJCqib9qkoy8cm/v1LLUoaXuJ96NBPEMr0"),
    ("open-xiaoai-test", "salt1234salt1234",
     "$6$salt1234salt1234$nxadK4OP9Yr80LgvVoJRI8RfiMm7zPosh77CqGdsKvyFTPLHnl19kl7OZuv7b8g3o5kGYYayg5koGMlvsDx330"),
    ("a" * 100, "shortsalt1234567",
     "$6$shortsalt1234567$apDB5cOY/dKUFQohAINK8phGwo3RvcQW60uuN8NtbMttNMg9pCaoo2l38YX2Le7n6BPyy5O18BCh9hUw6wyMP0"),
]
LIBXAUDIO_OFFSET = 773628  # strings -t d 实测：hw:0,3 在 1.94.13 里唯一一次出现


def hash_matches_password(password: str, hash_value: str) -> bool:
    """用 hash 自带的盐重算，断言该 hash 就是 password 的 sha512crypt。"""
    m = re.match(r"^\$6\$(rounds=\d+\$)?([^$]+)\$", hash_value)
    assert m, f"hash 格式不对: {hash_value!r}"
    rounds = int(m.group(1)[len("rounds="):-1]) if m.group(1) else 5000
    assert sha512_crypt(password, m.group(2), rounds=rounds) == hash_value
    return True


def shadow_root_hash(rootfs: Path) -> str:
    for line in (rootfs / "etc/shadow").read_text().splitlines():
        if line.startswith("root:"):
            return line.split(":")[1]
    raise AssertionError("shadow 里没有 root 行")


def copy_rootfs(dst):
    # 单一卡口：固件 rootfs 不在（CI 干净 checkout）时整批依赖测试跳过，
    # 理由见 conftest.require_firmware_rootfs；本地有材料时照常复制执行。
    require_firmware_rootfs()
    shutil.copytree(FIRMWARE_ROOTFS, dst, symlinks=True)
    return dst


@pytest.fixture(scope="module")
def patched_rootfs(tmp_path_factory):
    """一份跑完 patch_all(lx06) 的真实 rootfs 副本（模块级，只打一次）。"""
    root = copy_rootfs(tmp_path_factory.mktemp("patched") / "squashfs-root")
    result = patch_all(root, "lx06")
    assert result["libxaudio_offset"] == LIBXAUDIO_OFFSET
    return root


# --- sha512crypt 与 openssl 一致 ---

def test_sha512_crypt_matches_openssl():
    for pw, salt, expected in OPENSSL_SHA512_VECTORS:
        assert sha512_crypt(pw, salt) == expected
    # 自定义轮数也与 openssl/crypt 一致（openssl 无 rounds 选项，
    # 用系统 crypt 模块作第二基准；crypt 在 3.12 已弃用，仅测试里用）。
    import warnings
    with warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        import crypt
        assert sha512_crypt("testpw", "abcdefghijklmnop", rounds=10000) == \
            crypt.crypt("testpw", "$6$rounds=10000$abcdefghijklmnop$")
        assert sha512_crypt("testpw", "abcdefghijklmnop", rounds=1000) == \
            crypt.crypt("testpw", "$6$rounds=1000$abcdefghijklmnop$")


def test_sha512_crypt_random_salt_differs():
    # 盐缺省时每次随机：同一密码两次 hash 的盐不同、hash 不同。
    h1 = sha512_crypt(TEST_SSH_PASSWORD)
    h2 = sha512_crypt(TEST_SSH_PASSWORD)
    assert h1.startswith("$6$") and h2.startswith("$6$")
    assert len(h1) == len(h2) == len("$6$") + 16 + 1 + 86
    assert h1.split("$")[2] != h2.split("$")[2]
    assert h1 != h2
    assert hash_matches_password(TEST_SSH_PASSWORD, h1)
    assert hash_matches_password(TEST_SSH_PASSWORD, h2)


def test_random_salt_alphabet_and_length():
    for _ in range(20):
        s = _random_salt()
        assert len(s) == 16
        assert all(c in "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
                   for c in s)


# --- SSH ---

def test_patch_ssh(patched_rootfs):
    text = (patched_rootfs / "etc/init.d/dropbear").read_text()
    assert '[ "${enable}" = "0" ] && return 1' not in text
    assert "micocfg_work_mode" not in text
    assert "in bluetooth mode" not in text
    assert "/data/ssh_en" not in text
    assert "/data/.ssh_en" not in text
    assert "/tmp/ssh_en" not in text
    assert "mkdir -p /data/etc/dropbear" in text
    assert "[ -s /data/etc/dropbear/dropbear_rsa_host_key ] || keygen" in text
    # mkdir 必须在 keygen 之前
    assert text.index("mkdir -p /data/etc/dropbear") < text.index("|| keygen")
    # 其余逻辑未动：validate_section 与 procd 调用还在
    assert "validate_section_dropbear" in text
    assert "procd_open_instance" in text


def test_patch_ssh_standalone(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    check_ssh(root)
    patch_ssh(root)
    assert "micocfg_work_mode" not in (root / "etc/init.d/dropbear").read_text()


# --- 登录 ---

def test_patch_login(patched_rootfs):
    shadow = (patched_rootfs / "etc/shadow").read_text()
    root_hash = shadow_root_hash(patched_rootfs)
    assert root_hash.startswith("$6$")  # sha512crypt，不再是 $1$ md5crypt
    assert hash_matches_password(TEST_SSH_PASSWORD, root_hash)
    assert "daemon:*:0:0:99999:7:::" in shadow  # 其他用户未动
    inittab = (patched_rootfs / "etc/inittab").read_text()
    # P1-36：串口仍走 /bin/login（需 root 密码），不再是免密 ash。
    assert "ttyS0::askfirst:/bin/login" in inittab
    assert "ttyS0::askfirst:/bin/ash --login" not in inittab
    pam = (patched_rootfs / "etc/pam.d/common-auth").read_text()
    assert "# auth sufficient libmico-pam.so" in pam
    assert "# account sufficient libmico-pam.so" in pam
    assert "\nauth sufficient libmico-pam.so" not in pam
    assert "auth\t[success=1 default=ignore]\tpam_unix.so nullok_secure" in pam


def test_patch_login_custom_password_env(tmp_path, monkeypatch):
    root = copy_rootfs(tmp_path / "rootfs")
    monkeypatch.setenv("SSH_PASSWORD", "my-secret-pw-0123456789")
    got_hash = patch_login(root)
    assert got_hash.startswith("$6$")
    assert hash_matches_password("my-secret-pw-0123456789", got_hash)
    assert f"root:{got_hash}:" in (root / "etc/shadow").read_text()


def test_patch_login_same_password_random_salt(tmp_path):
    # 同一密码打两次补丁，盐不同、hash 不同（固定盐已被删除）。
    root1 = copy_rootfs(tmp_path / "rootfs1")
    root2 = copy_rootfs(tmp_path / "rootfs2")
    h1 = patch_login(root1)
    h2 = patch_login(root2)
    assert h1 != h2
    assert h1.split("$")[2] != h2.split("$")[2]


def test_patch_login_twice_on_sha512_shadow(tmp_path):
    # 已打过补丁的 rootfs 重跑 patch_login 必须成功（幂等）。
    # 旧代码两处失败：(1) shadow 第一次 subn（$[156]$ 全覆盖）的结果被紧接着的
    # 重读文件丢弃，第二次 subn 只处理 $1$，$6$ 行必抛错；(2) check_login 只认
    # 原厂 PAM 状态，已打过补丁的 rootfs 在前置检查就失败。latent bug，2026-11-17 修。
    root = copy_rootfs(tmp_path / "rootfs")
    h1 = patch_login(root)
    assert h1.startswith("$6$")
    h2 = patch_login(root)
    assert h2.startswith("$6$")
    assert h2 != h1  # 新盐、新 hash
    assert f"root:{h2}:" in (root / "etc/shadow").read_text()
    assert hash_matches_password(TEST_SSH_PASSWORD, h2)
    # PAM 两次打补丁后仍是恰好一次注释、无双重注释。
    pam = (root / "etc/pam.d/common-auth").read_text()
    assert "# # auth sufficient" not in pam
    assert "# # account sufficient" not in pam
    assert "auth\t[success=1 default=ignore]\tpam_unix.so nullok_secure" in pam


def test_patch_login_rejects_short_password(tmp_path, monkeypatch):
    root = copy_rootfs(tmp_path / "rootfs")
    monkeypatch.setenv("SSH_PASSWORD", "short")
    with pytest.raises(PatchError):
        patch_login(root)
    # 失败时 shadow 未动（仍是原厂 $1$ 行）
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in (root / "etc/shadow").read_text()


def test_check_login_accepts_sha512_shadow(tmp_path):
    # check_login 的 shadow 前置检查应接受 $6$（只换 shadow 的 root 行，
    # 其余文件保持原厂未打补丁状态，否则 inittab/pam 前置检查会先失败）。
    root = copy_rootfs(tmp_path / "rootfs")
    shadow = root / "etc/shadow"
    text = shadow.read_text()
    new_hash = sha512_crypt(TEST_SSH_PASSWORD, "testsalt12345678")
    text = re.sub(r"^root:\$1\$[^:]+:", f"root:{new_hash}:", text,
                  count=1, flags=re.MULTILINE)
    shadow.write_text(text)
    check_login(root)  # 不抛 PatchError 即通过
    # 反例：未知算法（$y$）仍应拒绝。
    text = shadow.read_text()
    text = re.sub(r"^root:\$6\$[^:]+:",
                  "root:$y$j9T$abcdefghijklmnop$aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:",
                  text, count=1, flags=re.MULTILINE)
    shadow.write_text(text)
    with pytest.raises(PatchError):
        check_login(root)


# --- 禁 OTA ---

def test_patch_disable_ota(patched_rootfs):
    import re
    ota = (patched_rootfs / "bin/ota").read_text()
    for func_re in (r"^download_upgrade\(\) \{$", r"^upgrade\(\) \{$"):
        m = re.search(func_re, ota, re.MULTILINE)
        assert m is not None, func_re
        assert ota[m.end():m.end() + 60].splitlines()[1].strip() == "return 1"
    flash = (patched_rootfs / "bin/flash.sh").read_text()
    m = re.search(r"^upgrade_param_check\(\) \{$", flash, re.MULTILINE)
    assert m is not None
    assert flash[m.end():m.end() + 60].splitlines()[1].strip() == "return 1"
    wireless = (patched_rootfs / "etc/init.d/wireless").read_text()
    idx = wireless.index("    identity=$4\n")
    chunk = wireless[idx:idx + 120]
    assert 'echo "download_fail"' in chunk
    assert "exit 3" in chunk
    # 其余 OTA 逻辑未被删：原函数体还在 return 1 之后
    assert "wget -t3 -T30" in ota


def test_patch_disable_ota_standalone(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    check_disable_ota(root)
    patch_disable_ota(root)
    assert "return 1" in (root / "bin/ota").read_text()


# --- 启动 ---

def test_patch_start(patched_rootfs):
    # P2-34：rc.local 走包装器，不再直接执行 /data/init.sh。
    rc = (patched_rootfs / "etc/rc.local").read_text()
    wrapper_line = ('[ -x /etc/open-xiaoai/init.sh ] '
                    '&& /etc/open-xiaoai/init.sh >/dev/null 2>&1 &')
    assert rc.count(wrapper_line) == 1
    assert rc.index(wrapper_line) < rc.index("exit 0")
    assert '[ -f "/data/init.sh" ] && sh /data/init.sh' not in rc
    wrapper = (patched_rootfs / "etc/open-xiaoai/init.sh").read_text()
    assert wrapper.startswith("#!/bin/sh")
    assert "ls -ln" in wrapper  # 设备上没有 stat，用 ls -ln 判断属主/权限
    assert "sha256sum" in wrapper
    assert "init.sh.sha256" in wrapper
    assert 'exec sh "$INIT"' in wrapper
    assert (patched_rootfs / "etc/open-xiaoai/init.sh").stat().st_mode & 0o777 == 0o755


def test_patch_start_idempotent(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    patch_start(root)
    patch_start(root)  # 第二次不重复加
    wrapper_line = ('[ -x /etc/open-xiaoai/init.sh ] '
                    '&& /etc/open-xiaoai/init.sh >/dev/null 2>&1 &')
    rc = (root / "etc/rc.local").read_text()
    assert rc.count(wrapper_line) == 1
    assert '[ -f "/data/init.sh" ] && sh /data/init.sh' not in rc


def test_patch_start_migrates_old_direct_line(tmp_path):
    # P2-34 迁移：已打过旧补丁（rc.local 里是旧直行）的 rootfs 重跑，
    # 旧行被替换为包装器行，不留两行、不报错。
    root = copy_rootfs(tmp_path / "rootfs")
    rc = root / "etc/rc.local"
    old = '[ -f "/data/init.sh" ] && sh /data/init.sh >/dev/null 2>&1 &'
    rc.write_text(rc.read_text().replace("exit 0", old + "\n\nexit 0", 1))
    patch_start(root)
    after = rc.read_text()
    assert old not in after
    wrapper_line = ('[ -x /etc/open-xiaoai/init.sh ] '
                    '&& /etc/open-xiaoai/init.sh >/dev/null 2>&1 &')
    assert after.count(wrapper_line) == 1
    assert (root / "etc/open-xiaoai/init.sh").is_file()


# --- P2-32：其余补丁步骤也必须可重复执行 ---

def test_patch_ssh_twice(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    check_ssh(root)
    patch_ssh(root)
    check_ssh(root)  # 已打补丁态也接受
    patch_ssh(root)  # 第二次不报错、不重复改
    assert "micocfg_work_mode" not in (root / "etc/init.d/dropbear").read_text()
    assert (root / "etc/init.d/dropbear").read_text().count(
        "mkdir -p /data/etc/dropbear") == 1


def test_patch_disable_ota_twice(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    check_disable_ota(root)
    patch_disable_ota(root)
    check_disable_ota(root)  # 已禁态也接受
    patch_disable_ota(root)  # 第二次不报错、不重复插 return 1
    ota = (root / "bin/ota").read_text()
    # 两个函数头后各恰好一次 return 1（原厂文件里本来就有 14 处 return 1，
    # 不能数总数；只数“函数头下一行就是 return 1”的插入点）。
    inserted = re.findall(
        r"^(?:download_upgrade|upgrade)\(\) \{$\n[ \t]*return 1$",
        ota, re.MULTILINE)
    assert len(inserted) == 2
    wireless = (root / "etc/init.d/wireless").read_text()
    idx = wireless.index("    identity=$4\n")
    chunk = wireless[idx:idx + 120]
    assert 'echo "download_fail"' in chunk
    assert "exit 3" in chunk
    # 插入只做一次（原厂文件别处本来就有 download_fail，不能数总数）。
    assert chunk.count('echo "download_fail"') == 1


def test_patch_libxaudio_twice(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    assert check_libxaudio(root) == LIBXAUDIO_OFFSET
    assert patch_libxaudio(root) == LIBXAUDIO_OFFSET
    assert check_libxaudio(root) == LIBXAUDIO_OFFSET  # 已打补丁态也接受
    assert patch_libxaudio(root) == LIBXAUDIO_OFFSET  # 第二次不重写
    data = (root / "usr/lib/libxaudio_engine.so").read_bytes()
    assert data[LIBXAUDIO_OFFSET:LIBXAUDIO_OFFSET + 6] == b"noop\x00\x00"


def test_patch_all_twice(tmp_path):
    # 整套 patch_all 在同一份 rootfs 上跑两次都必须成功（P2-32 全量）。
    root = copy_rootfs(tmp_path / "rootfs")
    r1 = patch_all(root, "lx06")
    r2 = patch_all(root, "lx06")
    assert r1["libxaudio_offset"] == r2["libxaudio_offset"] == LIBXAUDIO_OFFSET
    assert hash_matches_password(TEST_SSH_PASSWORD, shadow_root_hash(root))
    assert (root / "etc/open-xiaoai/init.sh").is_file()
    assert (root / "etc/asound.conf.bak").is_file()


# --- P2-33：asound 改动留 .bak ---

def test_patch_asound_writes_bak(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    factory = {rel: (root / rel).read_bytes()
               for rel in ("etc/asound.conf", "etc/asound.conf.dts")}
    patch_asound(root)
    for rel, orig in factory.items():
        bak = root / (rel + ".bak")
        assert bak.is_file()
        assert bak.read_bytes() == orig  # .bak 里是原厂内容，不是打过补丁的
        assert (root / rel).read_bytes() != orig
    # 重跑不覆盖已有的 .bak（仍是原厂内容），补丁内容也不变。
    patch_asound(root)
    for rel, orig in factory.items():
        assert (root / (rel + ".bak")).read_bytes() == orig


# --- P2-8：密码也可从文件读（不进进程环境） ---

def test_patch_login_password_from_file(tmp_path, monkeypatch):
    pw_file = tmp_path / "pw.txt"
    pw_file.write_text("file-password-0123456789\n")
    monkeypatch.setenv("SSH_PASSWORD_FILE", str(pw_file))
    monkeypatch.delenv("SSH_PASSWORD", raising=False)
    root = copy_rootfs(tmp_path / "rootfs")
    got_hash = patch_login(root)
    assert hash_matches_password("file-password-0123456789", got_hash)


def test_patch_login_password_file_beats_env(tmp_path, monkeypatch):
    # SSH_PASSWORD_FILE 优先于 SSH_PASSWORD（文件方式更安全）。
    pw_file = tmp_path / "pw.txt"
    pw_file.write_text("file-password-0123456789\n")
    monkeypatch.setenv("SSH_PASSWORD_FILE", str(pw_file))
    monkeypatch.setenv("SSH_PASSWORD", "env-password-0123456789")
    root = copy_rootfs(tmp_path / "rootfs")
    got_hash = patch_login(root)
    assert hash_matches_password("file-password-0123456789", got_hash)


def test_patch_login_password_file_missing(tmp_path, monkeypatch):
    monkeypatch.setenv("SSH_PASSWORD_FILE", str(tmp_path / "nope.txt"))
    monkeypatch.delenv("SSH_PASSWORD", raising=False)
    root = copy_rootfs(tmp_path / "rootfs")
    with pytest.raises(PatchError):
        patch_login(root)
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in (root / "etc/shadow").read_text()


# --- asound ---

def test_patch_asound(patched_rootfs):
    for rel in ("etc/asound.conf", "etc/asound.conf.dts"):
        text = (patched_rootfs / rel).read_text()
        assert "pcm.mico_record {\n    type plug\n    slave.pcm Capture\n}" in text
        assert "pcm.noop {\n    type plug\n    slave.pcm Capture\n}" in text
        assert "ttable" not in text
        assert 'pcm "hw:0,3"' in text
        assert "type dsnoop" in text
        assert "rate 48000" in text
        assert "format S32_LE" in text
        assert "channels 8" in text
        assert "period_size 384" in text
        assert "buffer_size 6144" in text
        assert "period_size 480" not in text
        assert "buffer_size 3840" not in text
        # rate_converter 恰好一次（原文件已有，不重复加）
        assert text.count('defaults.pcm.rate_converter "speexrate_medium"') == 1
        # 播放链未动：dmixer 仍走 hw:0,2
        assert 'pcm "hw:0,2"' in text


def test_patch_asound_adds_rate_converter_when_missing(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    for rel in ("etc/asound.conf", "etc/asound.conf.dts"):
        p = root / rel
        p.write_text(p.read_text().replace(
            'defaults.pcm.rate_converter "speexrate_medium"\n', ""))
    patch_asound(root)
    for rel in ("etc/asound.conf", "etc/asound.conf.dts"):
        text = (root / rel).read_text()
        assert text.count('defaults.pcm.rate_converter "speexrate_medium"') == 1


# --- libxaudio ---

def test_patch_libxaudio(patched_rootfs):
    data = (patched_rootfs / "usr/lib/libxaudio_engine.so").read_bytes()
    assert data[LIBXAUDIO_OFFSET:LIBXAUDIO_OFFSET + 6] == b"noop\x00\x00"
    assert b"hw:0,3" not in data
    assert len(data) == 4282440  # 文件大小不变（原地 6 字节替换）


def test_find_libxaudio_offset_real_firmware(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    assert find_libxaudio_offset(root) == LIBXAUDIO_OFFSET
    assert check_libxaudio(root) == LIBXAUDIO_OFFSET


# --- OH2P：asound 同逻辑，跳过 libxaudio ---

def test_patch_all_oh2p_skips_libxaudio(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    result = patch_all(root, "oh2p")
    assert result["libxaudio_offset"] is None
    assert "slave.pcm Capture" in (root / "etc/asound.conf").read_text()
    data = (root / "usr/lib/libxaudio_engine.so").read_bytes()
    assert data[LIBXAUDIO_OFFSET:LIBXAUDIO_OFFSET + 6] == b"hw:0,3"  # 未动


# 真实 OH2P 1.58.6 rootfs（download-ota.py + unpack.sh 的产物）：
# 原厂 asound.conf 已有 Capture 块（channels 4），DTS 文件的 noop 是
# dsnoop 直连（channels 8 / period 480）。回归：patch_all('oh2p')
# 必须通过，且不强求 LX06 的 channels 8、不动播放链的 period_size 480。
OH2P_ROOTFS = (Path(__file__).resolve().parent.parent
               / "work" / "oh2p_1.58.6" / "rootfs")


@pytest.mark.skipif(not OH2P_ROOTFS.is_dir(),
                    reason="需先跑 download-ota.py + unpack.sh oh2p 1.58.6")
def test_patch_all_real_oh2p_rootfs(tmp_path):
    root = tmp_path / "rootfs"
    shutil.copytree(OH2P_ROOTFS, root, symlinks=True)
    result = patch_all(root, "oh2p")
    assert result["libxaudio_offset"] is None
    assert not (root / "usr/lib/libxaudio_engine.so").exists()  # OH2P 本无此文件
    conf = (root / "etc/asound.conf").read_text()
    assert "channels 4" in conf  # 原厂 Capture 块保留
    assert "period_size 480" in conf  # 播放链原厂参数未动
    dts = (root / "etc/asound.conf.dts").read_text()
    assert "slave.pcm Capture" in dts
    assert "channels 8" in dts  # DTS 文件插入的标准 Capture 块
    assert "period_size 480" not in dts
    assert "buffer_size 3840" not in dts
    inittab = (root / "etc/inittab").read_text()
    # P1-36：串口仍走 /bin/login（需 root 密码），不再是免密 ash。
    assert inittab.count("ttyS0::askfirst:/bin/login") == 1
    assert "ttyS0::askfirst:/bin/ash --login" not in inittab
    shadow = (root / "etc/shadow").read_text()
    root_hash = shadow_root_hash(root)
    assert root_hash.startswith("$6$")
    assert hash_matches_password(TEST_SSH_PASSWORD, root_hash)


# --- 失败时不写输出 ---

def test_patch_all_fails_without_writing_on_missing_file(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    (root / "etc/init.d/dropbear").unlink()  # 前置条件不满足
    before = {
        rel: (root / rel).read_bytes()
        for rel in ("etc/shadow", "etc/inittab", "etc/rc.local",
                    "etc/asound.conf", "bin/ota",
                    "usr/lib/libxaudio_engine.so")
    }
    with pytest.raises(PatchError):
        patch_all(root, "lx06")
    after = {rel: (root / rel).read_bytes() for rel in before}
    assert after == before, "前置条件失败时不应改写任何文件"


def test_patch_all_fails_on_missing_libxaudio_string(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    p = root / "usr/lib/libxaudio_engine.so"
    data = bytearray(p.read_bytes())
    data[LIBXAUDIO_OFFSET:LIBXAUDIO_OFFSET + 6] = b"XXXXXX"  # 破坏特征串
    p.write_bytes(bytes(data))
    with pytest.raises(PatchError):
        patch_all(root, "lx06")


# --- P1-30：patch_all 中途失败回滚 + _write 原子写 ---

_PATCH_ALL_FILES = (
    "etc/init.d/dropbear", "etc/shadow", "etc/inittab",
    "etc/pam.d/common-auth", "bin/ota", "bin/flash.sh",
    "etc/init.d/wireless", "etc/rc.local", "etc/asound.conf",
    "etc/asound.conf.dts", "usr/lib/libxaudio_engine.so",
)


def test_patch_all_rolls_back_when_login_fails(tmp_path, monkeypatch):
    """P1-30：patch_ssh 已写、patch_login 因缺密码失败时，全部文件恢复原样。

    patch_login 在 patch_ssh 之后读密码：SSH_PASSWORD 未设且 stdin 非终端
    时抛 PatchError；此时 dropbear 已被改过——回滚必须把它（及全部文件）
    恢复成补丁前字节与权限，否则 rootfs 留在半打状态、重跑必失败。
    """
    root = copy_rootfs(tmp_path / "rootfs")
    monkeypatch.delenv("SSH_PASSWORD", raising=False)
    monkeypatch.setattr(sys, "stdin", io.StringIO())  # 保底：非终端
    before = {rel: ((root / rel).read_bytes(), (root / rel).stat().st_mode)
              for rel in _PATCH_ALL_FILES}
    with pytest.raises(PatchError):
        patch_all(root, "lx06")
    after = {rel: ((root / rel).read_bytes(), (root / rel).stat().st_mode)
             for rel in _PATCH_ALL_FILES}
    assert after == before, "中途失败后文件未恢复原样（字节或权限变了）"
    assert list(root.rglob("*.tmp")) == [], "回滚后不应残留临时文件"
    # 回滚后可直接重跑（不再是半打状态）：补上密码即成功。
    monkeypatch.setenv("SSH_PASSWORD", TEST_SSH_PASSWORD)
    result = patch_all(root, "lx06")
    assert result["libxaudio_offset"] == LIBXAUDIO_OFFSET


def test_write_is_atomic_and_preserves_mode(tmp_path):
    """P1-30：_write 经临时文件 + rename，不留 .tmp；原文件权限不变。"""
    p = tmp_path / "dropbear"
    p.write_bytes(b"original\n")
    p.chmod(0o755)
    _write(p, "patched\n")
    assert p.read_bytes() == b"patched\n"
    assert p.stat().st_mode & 0o777 == 0o755
    assert list(tmp_path.glob("*.tmp")) == []
    # 二进制写同样保留权限（libxaudio_engine.so 是 0664）。
    q = tmp_path / "lib.so"
    q.write_bytes(b"\x00\x01")
    q.chmod(0o664)
    _write_bytes(q, b"\x02\x03")
    assert q.read_bytes() == b"\x02\x03"
    assert q.stat().st_mode & 0o777 == 0o664
    assert list(tmp_path.glob("*.tmp")) == []


def test_check_functions_raise_on_missing_rootfs(tmp_path):
    missing = tmp_path / "no-such-rootfs"
    with pytest.raises(PatchError):
        check_ssh(missing)
    with pytest.raises(PatchError):
        check_login(missing)
    with pytest.raises(PatchError):
        check_disable_ota(missing)
    with pytest.raises(PatchError):
        check_start(missing)
    with pytest.raises(PatchError):
        check_asound(missing)
    with pytest.raises(PatchError):
        check_libxaudio(missing)


def test_patch_all_rejects_unknown_model(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    with pytest.raises(PatchError):
        patch_all(root, "lx99")


# --- CLI：真实跑脚本入口 ---

def test_cli_on_real_copy(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    r = subprocess.run(
        [sys.executable, str(SCRIPTS / "patch_firmware.py"),
         "--model", "lx06", "--rootfs", str(root)],
        capture_output=True, text=True)
    assert r.returncode == 0, r.stderr
    assert "补丁完成" in r.stdout
    assert "slave.pcm Capture" in (root / "etc/asound.conf").read_text()


def test_cli_fails_on_missing_rootfs(tmp_path):
    r = subprocess.run(
        [sys.executable, str(SCRIPTS / "patch_firmware.py"),
         "--model", "lx06", "--rootfs", str(tmp_path / "nope")],
        capture_output=True, text=True)
    assert r.returncode != 0
    assert "补丁失败" in r.stderr


def _run_cli(args, env_extra=None, stdin=None):
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        [sys.executable, str(SCRIPTS / "patch_firmware.py")] + args,
        capture_output=True, text=True, env=env, stdin=stdin)


def test_cli_fails_without_password(tmp_path):
    # P0-2：未设 SSH_PASSWORD 且非交互终端时直接失败，不写任何文件，
    # 且不在输出里出现任何 hash。
    root = copy_rootfs(tmp_path / "rootfs")
    env = dict(os.environ)
    env.pop("SSH_PASSWORD", None)
    r = subprocess.run(
        [sys.executable, str(SCRIPTS / "patch_firmware.py"),
         "--model", "lx06", "--rootfs", str(root)],
        capture_output=True, text=True, env=env, stdin=subprocess.DEVNULL)
    assert r.returncode != 0, r.stdout
    assert "补丁失败" in r.stderr
    assert "$6$" not in r.stdout + r.stderr
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in (root / "etc/shadow").read_text()


def test_cli_rejects_password_flag(tmp_path):
    # P0-2：禁止命令行传密码，--password 参数不存在。
    root = copy_rootfs(tmp_path / "rootfs")
    r = _run_cli(["--model", "lx06", "--rootfs", str(root),
                  "--password", "x" * 20])
    assert r.returncode != 0
    assert "unrecognized arguments" in r.stderr


def test_cli_never_prints_login_hash(tmp_path):
    # P0-2：main() 不打印 login_hash；输出里只有步骤摘要，不含实际 hash 值。
    # （步骤描述里出现 "$6$" 算法名是允许的，禁的是 86 字符 hash 本身。）
    root = copy_rootfs(tmp_path / "rootfs")
    r = _run_cli(["--model", "lx06", "--rootfs", str(root)])
    assert r.returncode == 0, r.stderr
    assert "login_hash" not in r.stdout
    assert shadow_root_hash(root) not in r.stdout
    assert shadow_root_hash(root) not in r.stderr
    assert hash_matches_password(TEST_SSH_PASSWORD, shadow_root_hash(root))


# --- mTLS 证书装入 ---

def _make_test_certs(tmp_path):
    """用 openssl 生成一套测试证书（CA + 客户端证书，PKCS#8 私钥）。"""
    d = tmp_path / "certs"
    d.mkdir()
    def run(*args):
        r = subprocess.run(["openssl"] + list(args),
                           capture_output=True, text=True, cwd=d)
        assert r.returncode == 0, r.stderr
    run("genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048",
        "-out", "ca.key")
    run("req", "-new", "-x509", "-key", "ca.key", "-out", "ca.crt",
        "-days", "365", "-subj", "/CN=test-ca")
    run("genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048",
        "-out", "client.key")
    run("req", "-new", "-key", "client.key", "-out", "client.csr",
        "-subj", "/CN=test-client")
    run("x509", "-req", "-in", "client.csr", "-CA", "ca.crt", "-CAkey",
        "ca.key", "-CAcreateserial", "-out", "client.crt", "-days", "365")
    return d


def test_patch_certs_installs_into_rootfs(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    cert_dir = _make_test_certs(tmp_path)
    installed = patch_certs(root, cert_dir)
    assert installed == ["etc/open-xiaoai/tls/ca.crt",
                         "etc/open-xiaoai/tls/client.crt",
                         "etc/open-xiaoai/tls/client.key"]
    dest = root / "etc/open-xiaoai/tls"
    assert "-----BEGIN CERTIFICATE-----" in (dest / "ca.crt").read_text()
    assert "-----BEGIN CERTIFICATE-----" in (dest / "client.crt").read_text()
    assert "-----BEGIN PRIVATE KEY-----" in (dest / "client.key").read_text()
    assert (dest / "client.key").stat().st_mode & 0o777 == 0o600
    assert (dest / "ca.crt").stat().st_mode & 0o777 == 0o644
    assert (dest / "client.crt").stat().st_mode & 0o777 == 0o644
    # 与源证书逐字节一致
    for name in ("ca.crt", "client.crt", "client.key"):
        assert (dest / name).read_bytes() == (cert_dir / name).read_bytes()


def test_patch_certs_rejects_missing_file(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    cert_dir = _make_test_certs(tmp_path)
    (cert_dir / "client.key").unlink()
    with pytest.raises(PatchError):
        patch_certs(root, cert_dir)
    assert not (root / "etc/open-xiaoai/tls").exists()


def test_patch_all_with_cert_dir(tmp_path):
    root = copy_rootfs(tmp_path / "rootfs")
    cert_dir = _make_test_certs(tmp_path)
    result = patch_all(root, "lx06", cert_dir)
    assert result["certs"] == ["etc/open-xiaoai/tls/ca.crt",
                               "etc/open-xiaoai/tls/client.crt",
                               "etc/open-xiaoai/tls/client.key"]
    assert "login_hash" not in result  # hash 永不进结果摘要
    assert (root / "etc/open-xiaoai/tls" / "client.crt").exists()
