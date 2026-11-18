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
    _reuse_root_hash,
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
    verify_markers,
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
    # F-2：另两行控制台同样走 /bin/login，全表无免密 ash。
    assert "tts/0::askfirst:/bin/login" in inittab
    assert "tty1::askfirst:/bin/login" in inittab
    assert "/bin/ash --login" not in inittab
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


def test_patch_login_rewrites_all_consoles(tmp_path):
    # F-2：三行控制台统一走 /bin/login。原厂 inittab 是 tts/0 + tty1 免密
    # ash、ttyS0 login；打完补丁三行都是 login，全表无 ash --login。
    root = copy_rootfs(tmp_path / "rootfs")
    before = (root / "etc/inittab").read_text()
    assert "tts/0::askfirst:/bin/ash --login" in before
    assert "tty1::askfirst:/bin/ash --login" in before
    patch_login(root)
    after = (root / "etc/inittab").read_text()
    for dev in ("tts/0", "ttyS0", "tty1"):
        assert f"{dev}::askfirst:/bin/login" in after
    assert "/bin/ash --login" not in after
    # 幂等：第二遍不报错、不改内容。
    patch_login(root)
    assert (root / "etc/inittab").read_text() == after
    # sysinit/shutdown 行未动。
    assert "::sysinit:/etc/init.d/rcS S boot" in after
    assert "::shutdown:/etc/init.d/rcS K shutdown" in after


def test_check_login_accepts_factory_and_patched_inittab(tmp_path):
    # F-2：check_login 接受原厂态（tts/0 + tty1 是 ash）与已打补丁态
    # （三行都是 login）；缺一行或换成别的 shell 即拒绝。
    root = copy_rootfs(tmp_path / "rootfs")
    check_login(root)  # 原厂态通过
    patch_login(root)
    check_login(root)  # 已打补丁态通过
    inittab = root / "etc/inittab"
    text = inittab.read_text()
    inittab.write_text(text.replace("tty1::askfirst:/bin/login\n", ""))
    with pytest.raises(PatchError):
        check_login(root)
    inittab.write_text(text.replace("tty1::askfirst:/bin/login",
                                    "tty1::askfirst:/bin/sh"))
    with pytest.raises(PatchError):
        check_login(root)


def test_patch_login_reuses_hash_from_file(tmp_path, monkeypatch):
    # F-2 重编支持：PATCH_REUSE_ROOT_HASH_FILE 指向含一行完整 $6$ hash 的
    # 文件时，patch_login 直接沿用该 hash，不读密码、不生成新盐，root 密码不变。
    root = copy_rootfs(tmp_path / "rootfs")
    reused = sha512_crypt(TEST_SSH_PASSWORD, "reusesalt1234567")
    hash_file = tmp_path / "reuse-hash.txt"
    hash_file.write_text(reused + "\n")
    monkeypatch.setenv("PATCH_REUSE_ROOT_HASH_FILE", str(hash_file))
    monkeypatch.delenv("SSH_PASSWORD", raising=False)
    monkeypatch.delenv("SSH_PASSWORD_FILE", raising=False)
    monkeypatch.setattr(sys, "stdin", io.StringIO())  # 非终端：无密码也必须成功
    got = patch_login(root)
    assert got == reused
    assert f"root:{reused}:" in (root / "etc/shadow").read_text()
    assert hash_matches_password(TEST_SSH_PASSWORD, got)
    # 第二遍沿用同一 hash（不像随机盐每次都变）。
    assert patch_login(root) == reused
    assert _reuse_root_hash() == reused


def test_patch_login_rejects_bad_reuse_hash_file(tmp_path, monkeypatch):
    # 复用文件不存在、内容不是合法 $6$ hash 时直接失败，不写 shadow。
    root = copy_rootfs(tmp_path / "rootfs")
    monkeypatch.setenv("PATCH_REUSE_ROOT_HASH_FILE",
                       str(tmp_path / "nope.txt"))
    with pytest.raises(PatchError):
        patch_login(root)
    bad = tmp_path / "bad-hash.txt"
    bad.write_text("not-a-hash\n")
    monkeypatch.setenv("PATCH_REUSE_ROOT_HASH_FILE", str(bad))
    with pytest.raises(PatchError):
        patch_login(root)
    assert "$1$N0Iz0LLs$kZ5zG/Y2AUWHNE5I3ElWm1" in (root / "etc/shadow").read_text()
    # $5$ hash 不是 $6$，同样拒绝（复用只接受 $6$）。
    bad.write_text(sha512_crypt(TEST_SSH_PASSWORD, "salt1234salt1234")
                   .replace("$6$", "$5$", 1) + "\n")
    with pytest.raises(PatchError):
        patch_login(root)


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
    # F-3：pin 必填——缺 pin 文件即拒绝，不只是“有 pin 时才校验”。
    assert 'if [ ! -f "$PIN" ]' in wrapper
    assert "sha256 pin missing" in wrapper
    assert 'exec sh "$INIT"' in wrapper
    assert (patched_rootfs / "etc/open-xiaoai/init.sh").stat().st_mode & 0o777 == 0o755


def _sandbox_wrapper(patched_rootfs, tmp_path):
    """把包装器拷到临时目录并把 /data 路径改写为临时路径，返回
    (wrapper, init, pin, log, marker)。只改三行路径定义，不改校验逻辑。"""
    d = tmp_path / "data"
    d.mkdir()
    wrapper = d / "init-wrapper.sh"
    text = (patched_rootfs / "etc/open-xiaoai/init.sh").read_text()
    text = text.replace('INIT="/data/init.sh"', f'INIT="{d}/init.sh"')
    text = text.replace('PIN="/data/init.sh.sha256"', f'PIN="{d}/init.sh.sha256"')
    text = text.replace('LOG="/data/open-xiaoai-init.log"', f'LOG="{d}/init.log"')
    wrapper.write_text(text)
    return (wrapper, d / "init.sh", d / "init.sh.sha256", d / "init.log",
            d / "marker")


def _run_wrapper(wrapper):
    return subprocess.run(["sh", str(wrapper)], capture_output=True, text=True)


@pytest.mark.skipif(os.geteuid() != 0,
                    reason="包装器要求 init.sh 属主 uid 0，非 root 跑必被属主检查拒绝")
def test_init_wrapper_requires_pin(patched_rootfs, tmp_path):
    # F-3 回归：/data/init.sh 在、无 pin 文件时包装器必须拒绝执行（旧的
    # 可选 pin 写法会直接放行）。分四种场景跑真实包装器脚本。
    wrapper, init, pin, log, marker = _sandbox_wrapper(patched_rootfs, tmp_path)
    hook = '#!/bin/sh\necho RAN > "' + str(marker) + '"\n'
    # 1. 无 init.sh：直接退出 0，不执行、不写日志。
    r = _run_wrapper(wrapper)
    assert r.returncode == 0
    assert not marker.exists()
    assert not log.exists()
    # 2. 有 init.sh、无 pin：拒绝执行，日志记 pin missing。
    init.write_text(hook)
    init.chmod(0o644)
    r = _run_wrapper(wrapper)
    assert r.returncode == 0
    assert not marker.exists()
    assert "refuse" in log.read_text() and "pin missing" in log.read_text()
    # 3. pin 内容不对：拒绝执行，日志记 mismatch。
    pin.write_text("0" * 64 + "  init.sh\n")
    r = _run_wrapper(wrapper)
    assert r.returncode == 0
    assert not marker.exists()
    assert "sha256 mismatch" in log.read_text()
    # 4. pin 正确：执行钩子。
    pin.write_text(hashlib.sha256(hook.encode()).hexdigest() + "  init.sh\n")
    r = _run_wrapper(wrapper)
    assert r.returncode == 0
    assert marker.exists()
    assert marker.read_text().strip() == "RAN"


@pytest.mark.skipif(os.geteuid() != 0,
                    reason="包装器要求 init.sh 属主 uid 0，非 root 跑必被属主检查拒绝")
def test_init_wrapper_refuses_group_writable(patched_rootfs, tmp_path):
    # 包装器原有检查不受 F-3 影响：组可写的 init.sh 即使 pin 正确也拒绝。
    wrapper, init, pin, log, marker = _sandbox_wrapper(patched_rootfs, tmp_path)
    hook = '#!/bin/sh\necho RAN > "' + str(marker) + '"\n'
    init.write_text(hook)
    init.chmod(0o664)
    pin.write_text(hashlib.sha256(hook.encode()).hexdigest() + "  init.sh\n")
    r = _run_wrapper(wrapper)
    assert r.returncode == 0
    assert not marker.exists()
    assert "group/world-writable" in log.read_text()


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


# --- F-1：verify_markers 只认补丁特有标记 ---

def test_verify_markers_passes_on_patched(patched_rootfs):
    verify_markers(patched_rootfs, "lx06")  # 不抛即通过


def test_verify_markers_fails_on_factory(tmp_path):
    # F-1 回归：原厂 rootfs 跑 §6 断言必须 FAIL（旧的三处 grep 标记在原厂
    # 就有，会空转放行；新断言第一项 dropbear 门控即拦住原厂）。
    root = copy_rootfs(tmp_path / "rootfs")
    with pytest.raises(PatchError):
        verify_markers(root, "lx06")


def test_verify_markers_fails_when_dropbear_gate_restored(patched_rootfs,
                                                          tmp_path):
    # 门控重现即 FAIL：把 enable=0 返回插回已打补丁的 dropbear（只改
    # 副本，不动模块级 fixture）。
    root = tmp_path / "rootfs"
    shutil.copytree(patched_rootfs, root, symlinks=True)
    p = root / "etc/init.d/dropbear"
    p.write_text('\t[ "${enable}" = "0" ] && return 1\n' + p.read_text())
    with pytest.raises(PatchError, match="enable"):
        verify_markers(root, "lx06")


def test_verify_markers_fails_when_wireless_ota_block_removed(patched_rootfs,
                                                              tmp_path):
    # wireless 的 ota() 禁用块被删即 FAIL：只删插入的那 4 空格缩进块，
    # 原厂 curl 失败分支的 8 空格 download_fail 留着——新断言只认函数头后
    # 60 字符内的 download_fail + exit 3，旧的全文 grep 会被那处空转放行。
    root = tmp_path / "rootfs"
    shutil.copytree(patched_rootfs, root, symlinks=True)
    p = root / "etc/init.d/wireless"
    text = p.read_text()
    assert text.count('\n    echo "download_fail"\n    exit 3\n') == 1
    p.write_text(text.replace('\n    echo "download_fail"\n    exit 3\n',
                              "\n", 1))
    assert 'echo "download_fail"' in p.read_text()  # 原厂分支那处还在
    with pytest.raises(PatchError, match="wireless"):
        verify_markers(root, "lx06")


def test_verify_markers_fails_when_inittab_ash_restored(patched_rootfs,
                                                        tmp_path):
    # inittab 任一行回到免密 ash 即 FAIL（F-2：三行都要 login）。
    root = tmp_path / "rootfs"
    shutil.copytree(patched_rootfs, root, symlinks=True)
    p = root / "etc/inittab"
    p.write_text(p.read_text().replace("tty1::askfirst:/bin/login",
                                       "tty1::askfirst:/bin/ash --login", 1))
    with pytest.raises(PatchError, match="inittab"):
        verify_markers(root, "lx06")


def test_verify_markers_rejects_unknown_model(patched_rootfs):
    with pytest.raises(PatchError):
        verify_markers(patched_rootfs, "lx99")


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
    verify_markers(root, "oh2p")  # F-1：OH2P 打完补丁的断言通过
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
    # F-2：另两行同样走 /bin/login，全表无免密 ash。
    assert "tts/0::askfirst:/bin/login" in inittab
    assert "tty1::askfirst:/bin/login" in inittab
    assert "/bin/ash --login" not in inittab
    shadow = (root / "etc/shadow").read_text()
    root_hash = shadow_root_hash(root)
    assert root_hash.startswith("$6$")
    assert hash_matches_password(TEST_SSH_PASSWORD, root_hash)


@pytest.mark.skipif(not OH2P_ROOTFS.is_dir(),
                    reason="需先跑 download-ota.py + unpack.sh oh2p 1.58.6")
def test_verify_markers_fails_on_factory_oh2p(tmp_path):
    # F-1 回归：OH2P 原厂 rootfs 同样必须 FAIL（它的 asound.conf 原厂即
    # Capture，但 dropbear 门控、inittab、wireless 三项都拦得住原厂）。
    root = tmp_path / "rootfs"
    shutil.copytree(OH2P_ROOTFS, root, symlinks=True)
    with pytest.raises(PatchError):
        verify_markers(root, "oh2p")


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
