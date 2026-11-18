#!/usr/bin/env python3
"""对 LX06 / OH2P 解包后的 rootfs 副本打补丁（Phase 1，纯 Python 自研实现）。

补丁效果移植自参考实现（只读 Spec，见文件头“来源”；本文件是独立重写，
未复制参考代码）：
  - 通用 SSH / 登录 / 禁 OTA / 启动：references/open-xiaoai/packages/
    client-patch/patches/{01-ssh,02-login,03-ota,04-start}.patch
  - LX06 asound / libxaudio：同上 patches/LX06/{01-asound,02-asound-dts}.patch
    与 03-libxaudio_engine.sh；asound 的 dsnoop 参数与 REA_ANALYSIS.md 一致
    （hw:0,3，48kHz，S32_LE，8ch，period 384，buffer 6144）。
  - 签名算法 / 分区上限等已验证参数见 ../../firmware/REA_ANALYSIS.md。

设计：
  - 每个补丁都是 check（只读前置条件）-> apply（改写）-> verify（校验结果）
    三段，可被 pytest 单独调用和断言。
  - patch_all() 先对全部步骤做 preflight（只读），任一前置条件不满足就
    立即抛 PatchError 且不写任何文件；全部通过后才逐个应用。
  - 绝不原地改 firmware/rootfs：调用方（scripts/patch.sh）先把 rootfs
    复制到 work/<型号>_<版本>/squashfs-root 再传 --rootfs 进来。

用法：
  SSH_PASSWORD=$(openssl rand -hex 16) python3 patch_firmware.py --model lx06 --rootfs work/lx06_1.94.13/squashfs-root
  python3 patch_firmware.py --model oh2p --rootfs ... --cert-dir work/certs  # 另装 mTLS 客户端证书

密码来源（P0-2）：只从环境变量 SSH_PASSWORD（或 SSH_PASSWORD_FILE 指
向的 0600 文件，一行一个密码，行尾换行会被去掉）、或交互式 getpass 读，
未设置且非交互终端时直接失败退出；仓库里没有任何默认密码。
密码 hash 为 sha512crypt（$6$，glibc 2.25 的 libcrypt 支持的最强算法，
yescrypt 需 libxcrypt/glibc ≥2.36，固件的 glibc 2.25 不支持），
盐每次用 secrets 随机生成 16 字符；hash 永不打印到终端/日志。
"""
import argparse
import getpass
import hashlib
import os
import re
import secrets
import sys
import tempfile
from pathlib import Path

# ---------------------------------------------------------------------------
# 常量
# ---------------------------------------------------------------------------

MIN_PASSWORD_LENGTH = 16             # root 密码最短长度（P0-2：强制强随机密码）
MAX_IMAGE_SIZE = 0x02800000            # 40MB 分区上限（REA_ANALYSIS.md）
LIBXAUDIO_TARGET = b"hw:0,3"           # libxaudio_engine.so 里要改掉的设备名
LIBXAUDIO_REPLACEMENT = b"noop\x00\x00"
RC_LOCAL_LINE = '[ -f "/data/init.sh" ] && sh /data/init.sh >/dev/null 2>&1 &'
# P2-34：rc.local 不再直接执行 /data/init.sh，改走包装器
# （etc/open-xiaoai/init.sh）。旧直行只在迁移判断里用：已打过旧补丁的
# rootfs 重跑 patch_start 时，旧行会被替换为包装器行。
INIT_WRAPPER_PATH = "etc/open-xiaoai/init.sh"
RC_LOCAL_WRAPPER_LINE = ('[ -x /etc/open-xiaoai/init.sh ] '
                         '&& /etc/open-xiaoai/init.sh >/dev/null 2>&1 &')
INIT_WRAPPER_SCRIPT = """#!/bin/sh
# open-xiaoai-next 开机钩子包装器（P2-34）。
# /data/init.sh 是客户端安装留下的开机钩子；rc.local 不再直接执行它，
# 而是先由本包装器校验——直接执行等于无条件信任任何能写 /data 的一方。
# 执行条件（缺一不可）：
#   1. /data/init.sh 存在；
#   2. 属主 uid 为 0（root），且组/全局均不可写（ls -ln 的权限串里
#      第 6 位与第 9 位都不是 w；设备上没有 stat 命令，见下）；
#   3. 若 /data/init.sh.sha256 存在，文件 SHA256 必须与其中第一字段一致
#      （客户端安装流程写该文件来钉死钩子内容；没有该文件时只做 1+2）。
# 任一条件不满足都拒绝执行，并把原因记到 /data/open-xiaoai-init.log。
# 本文件烤进 squashfs（只读），改它要重打补丁重刷机。
# 只用 busybox 自带命令（ls/sha256sum/cut/date 均在原厂 rootfs 里实测存在；
# stat 不在 rootfs 里，所以属主与权限用 ls -ln 的输出判断）。
INIT="/data/init.sh"
PIN="/data/init.sh.sha256"
LOG="/data/open-xiaoai-init.log"
[ -f "$INIT" ] || exit 0
set -- $(ls -ln "$INIT")
# $1=权限串（如 -rw-r--r--），$3=属主 uid（-n 不支持时是名字 root，也接受）
if [ "$3" != "0" ] && [ "$3" != "root" ]; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') refuse: $INIT owner uid=$3 (want 0)" >> "$LOG" 2>/dev/null
    exit 0
fi
case "$1" in
    ?????w????|????????w?)
        echo "$(date '+%Y-%m-%d %H:%M:%S') refuse: $INIT perms $1 group/world-writable" >> "$LOG" 2>/dev/null
        exit 0
        ;;
esac
if [ -f "$PIN" ]; then
    want="$(cut -d' ' -f1 "$PIN" 2>/dev/null)"
    got="$(sha256sum "$INIT" 2>/dev/null | cut -d' ' -f1)"
    if [ -z "$want" ] || [ "$want" != "$got" ]; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') refuse: $INIT sha256 mismatch" >> "$LOG" 2>/dev/null
        exit 0
    fi
fi
exec sh "$INIT"
"""
RATE_CONVERTER_LINE = 'defaults.pcm.rate_converter "speexrate_medium"'

# Capture 录音链：dsnoop 直采 hw:0,3（来源：LX06/01-asound.patch 的效果）。
CAPTURE_BLOCK = """pcm.Capture {
    type plug
    slave.pcm {
        type dsnoop
        ipc_key 1024
        ipc_perm 0666
        slave {
            pcm "hw:0,3"
            rate 48000
            format S32_LE
            channels 8
            period_size 384
            buffer_size 6144
        }
    }
}"""

MICO_RECORD_BLOCK = """pcm.mico_record {
    type plug
    slave.pcm Capture
}"""

NOOP_BLOCK = """pcm.noop {
    type plug
    slave.pcm Capture
}"""

ASOUND_FILES = ("etc/asound.conf", "etc/asound.conf.dts")


class PatchError(Exception):
    """任一步前置条件不满足或校验失败时抛出；调用方应以非零退出。"""


# ---------------------------------------------------------------------------
# 小工具
# ---------------------------------------------------------------------------

def _read(path: Path) -> str:
    try:
        return path.read_text(errors="surrogateescape")
    except FileNotFoundError:
        raise PatchError(f"缺文件：{path}")


def _write_bytes(path: Path, data: bytes, mode: int | None = None) -> None:
    """原子写（P1-30）：同目录临时文件 → 写盘 + fsync → os.replace。

    mode 为 None 时保留原文件权限（不存在则 0644）；显式给 mode 时按它来。
    临时文件失败即删，不留半成品；os.replace 是同文件系统内的原子改名，
    读方要么看到旧文件全文、要么看到新文件全文，永不看到一半内容。
    """
    dir_path = path.parent
    fd, tmp_name = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp",
                                    dir=str(dir_path))
    tmp = Path(tmp_name)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        if mode is None:
            mode = path.stat().st_mode & 0o7777 if path.exists() else 0o644
        os.chmod(tmp, mode)
        os.replace(tmp, path)
        try:  # fsync 目录，让改名落盘（best-effort，失败不报错）
            dfd = os.open(str(dir_path), os.O_RDONLY)
            try:
                os.fsync(dfd)
            finally:
                os.close(dfd)
        except OSError:
            pass
    except BaseException:
        try:
            tmp.unlink()
        except OSError:
            pass
        raise


def _write(path: Path, text: str) -> None:
    _write_bytes(path, text.encode("utf-8", errors="surrogateescape"))


def _require(path: Path, what: str) -> Path:
    if not path.is_file():
        raise PatchError(f"缺文件：{path}（{what}）")
    return path


def _require_in(text: str, needle: str, where: str) -> None:
    if needle not in text:
        raise PatchError(f"前置条件不满足：{where} 里找不到 {needle!r}")


def _require_not_in(text: str, needle: str, where: str) -> None:
    if needle in text:
        raise PatchError(f"校验失败：{where} 里仍有 {needle!r}")


def _extract_pcm_block(text: str, name: str, where: str) -> str:
    """用花括号计数取出 `pcm.<name> { ... }` 整块（含嵌套，如 ttable）。"""
    start = text.find(f"pcm.{name} {{")
    if start < 0:
        raise PatchError(f"前置条件不满足：{where} 里找不到 pcm.{name} 块")
    brace = text.find("{", start)
    depth = 0
    i = brace
    while i < len(text):
        ch = text[i]
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                break
        i += 1
    if depth != 0:
        raise PatchError(f"前置条件不满足：{where} 里 pcm.{name} 块花括号不配对")
    return text[start:i + 1]  # 含右花括号


def _replace_pcm_block(text: str, name: str, replacement: str, where: str) -> str:
    """用花括号计数定位 `pcm.<name> { ... }` 整块并替换为 replacement。"""
    start = text.find(f"pcm.{name} {{")
    if start < 0:
        raise PatchError(f"前置条件不满足：{where} 里找不到 pcm.{name} 块")
    block = _extract_pcm_block(text, name, where)
    return text[:start] + replacement + text[start + len(block):]


# ---------------------------------------------------------------------------
# sha512crypt（$6$，Ulrich Drepper SHA-crypt 规范）
#
# 选型依据（P0-2）：固件 rootfs 的 libcrypt 是 glibc 2.25
# （lib/libcrypt-2.25.so，LX06 与 OH2P 同一文件，md5 相同），pam_unix.so
# 引用 ENCRYPT_METHOD / SHA_CRYPT_MAX_ROUNDS，登录走 glibc crypt()；
# glibc 的 $6$ 自 2.7 起支持，$5$/$6$ 是该版本最强的口令 hash。
# yescrypt（$y$）需要 libxcrypt / glibc ≥ 2.36，固件里没有对应符号
# （strings libcrypt-2.25.so 无 yescrypt/$y$），写 $y$ 会导致登录时
# crypt() 认不出算法而拒绝密码，故选 sha512crypt + 5000 轮（默认轮数）。
# 实现为纯 Python（与原 md5_crypt 同风格），已用 openssl 逐字节校验：
#   sha512_crypt("testpw", "abcdefghijklmnop")
#   == openssl passwd -6 -salt abcdefghijklmnop testpw
#   == "$6$abcdefghijklmnop$N6qTiVZoBdrJ.R/wUkMfGcrb6WwbbgenZ1D1DI8mwGvn6PHXmN59VJCqib9qkoy8cm/v1LLUoaXuJ96NBPEMr0"
# ---------------------------------------------------------------------------

_ITOA64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"


def _to64(value: int, n: int) -> str:
    out = []
    for _ in range(n):
        out.append(_ITOA64[value & 0x3F])
        value >>= 6
    return "".join(out)


def _random_salt(length: int = 16) -> str:
    """用 secrets 随机生成盐（crypt 字母表，默认 16 字符，$6$ 上限）。"""
    return "".join(secrets.choice(_ITOA64) for _ in range(length))


def sha512_crypt(password: str, salt: str | None = None, rounds: int = 5000) -> str:
    """返回 $6$<salt>$<86 字符> 形式的 sha512crypt hash。

    salt 为 None 时每次随机生成；rounds 默认 5000（glibc 默认值，
    输出里不写 rounds= 段，与 openssl passwd -6 默认输出一致）。
    """
    if salt is None:
        salt = _random_salt()
    pw = password.encode()
    salt_b = salt.encode()[:16]
    # 备用摘要 B：sha512(pw + salt + pw)
    alt = hashlib.sha512(pw + salt_b + pw).digest()
    # 摘要 A：sha512(pw + salt)，再按 pw 长度混入 B（64 字节分块 + 余数），
    # 再按 pw 长度的二进制位混入：位为 1 加整个 B，位为 0 加整个 pw。
    ctx = hashlib.sha512(pw + salt_b)
    remaining = len(pw)
    while remaining > 0:
        take = min(remaining, 64)
        ctx.update(alt[:take])
        remaining -= take
    i = len(pw)
    while i:
        if i & 1:
            ctx.update(alt)
        else:
            ctx.update(pw)
        i >>= 1
    digest = ctx.digest()
    # 字节序列 P：sha512(pw 重复 len(pw) 次)的摘要按 pw 长度取字节；
    # 字节序列 S：sha512(salt 重复 16 + digest[0] 次)的摘要按 salt 长度取字节。
    dp = hashlib.sha512()
    for _ in range(len(pw)):
        dp.update(pw)
    dp_digest = dp.digest()
    p_seq = bytes(dp_digest[j % 64] for j in range(len(pw)))
    ds = hashlib.sha512()
    for _ in range(16 + digest[0]):
        ds.update(salt_b)
    ds_digest = ds.digest()
    s_seq = bytes(ds_digest[j % 64] for j in range(len(salt_b)))
    # 轮次：奇数轮加 P、偶数轮加摘要；轮数 %3 != 0 加 S；%7 != 0 加 P；
    # 奇数轮再加摘要、偶数轮再加 P。
    for i in range(rounds):
        c = hashlib.sha512()
        if i & 1:
            c.update(p_seq)
        else:
            c.update(digest)
        if i % 3:
            c.update(s_seq)
        if i % 7:
            c.update(p_seq)
        if i & 1:
            c.update(digest)
        else:
            c.update(p_seq)
        digest = c.digest()
    out = "$6$"
    if rounds != 5000:
        out += f"rounds={rounds}$"
    out += salt_b.decode() + "$"
    for a, b, c3 in ((0, 21, 42), (22, 43, 1), (44, 2, 23), (3, 24, 45),
                    (25, 46, 4), (47, 5, 26), (6, 27, 48), (28, 49, 7),
                    (50, 8, 29), (9, 30, 51), (31, 52, 10), (53, 11, 32),
                    (12, 33, 54), (34, 55, 13), (56, 14, 35), (15, 36, 57),
                    (37, 58, 16), (59, 17, 38), (18, 39, 60), (40, 61, 19),
                    (62, 20, 41)):
        out += _to64((digest[a] << 16) | (digest[b] << 8) | digest[c3], 4)
    out += _to64(digest[63], 2)
    return out


def _password() -> str:
    """读 root 密码：只从 SSH_PASSWORD_FILE、环境变量 SSH_PASSWORD 或交互式 getpass。

    优先 SSH_PASSWORD_FILE（文件内容不进进程环境，/proc/<pid>/environ 里
    看不到；P2-8 残留：环境变量对同机同用户进程可见，故文件方式更安全）。
    未设置且非交互终端时抛 PatchError（直接失败退出，永不 fallback
    到默认值）；密码短于 16 位、含 ':' 或换行时同样失败。
    """
    pw_file = os.environ.get("SSH_PASSWORD_FILE", "")
    if pw_file:
        try:
            pw = Path(pw_file).read_text().strip("\r\n")
        except OSError as exc:
            raise PatchError(f"读 SSH_PASSWORD_FILE 失败：{pw_file}：{exc}")
        source = "SSH_PASSWORD_FILE"
    else:
        pw = os.environ.get("SSH_PASSWORD", "")
        if pw:
            source = "环境变量 SSH_PASSWORD"
        elif sys.stdin.isatty():
            pw = getpass.getpass("root 密码（≥16 位，不回显）: ")
            source = "交互式输入"
        else:
            raise PatchError(
                "未设置 SSH_PASSWORD / SSH_PASSWORD_FILE 且非交互终端："
                "拒绝使用默认密码。请用强随机密码重跑，例如 "
                "SSH_PASSWORD=$(openssl rand -hex 16) python3 patch_firmware.py ..."
            )
    if len(pw) < MIN_PASSWORD_LENGTH:
        raise PatchError(
            f"{source}的密码只有 {len(pw)} 位，至少 {MIN_PASSWORD_LENGTH} 位"
            "（建议 openssl rand -hex 16 生成）"
        )
    if ":" in pw or "\n" in pw or "\r" in pw:
        raise PatchError(f"{source}的密码不能含 ':' 或换行（shadow 字段分隔符）")
    return pw


# ---------------------------------------------------------------------------
# 1. SSH：/etc/init.d/dropbear 改为始终启动
#    （来源：01-ssh.patch 的效果；只做任务指定的三处门控拆除 + mkdir）
# ---------------------------------------------------------------------------

_DROPBEAR_ENABLE_LINE = '\t[ "${enable}" = "0" ] && return 1\n'
_DROPBEAR_BLUETOOTH_RE = re.compile(
    r'\t\[ x"bluetooth" == x`micocfg_work_mode` \] && \{\n'
    r'.*?return\n    \}\n',
    re.DOTALL,
)
_DROPBEAR_SSH_EN_RE = re.compile(
    r'\tssh_en=`cat /data/ssh_en 2>/dev/null`\n'
    r'.*?return 0 *\n\tfi\n',
    re.DOTALL,
)
_DROPBEAR_KEYGEN_LINE = "\t[ -s /data/etc/dropbear/dropbear_rsa_host_key ] || keygen\n"
_DROPBEAR_MKDIR_LINE = "\tmkdir -p /data/etc/dropbear\n"


def check_ssh(rootfs: Path) -> None:
    """接受两种状态：原厂（enable=0 返回、蓝牙门控、ssh_en 门控都在）与
    已打过本补丁（三个门控都已拆 + mkdir 行已在）；其他状态一律拒绝。

    （P2-32：旧版只认原厂状态，已打过补丁的 rootfs 重跑 patch_ssh 必
    在这里失败；与 7662dcf 修 patch_login 的做法一致。）
    """
    p = _require(rootfs / "etc/init.d/dropbear", "SSH 启动脚本")
    text = _read(p)
    has_enable = '[ "${enable}" = "0" ] && return 1' in text
    has_bluetooth = bool(_DROPBEAR_BLUETOOTH_RE.search(text))
    has_ssh_en = bool(_DROPBEAR_SSH_EN_RE.search(text))
    _require_in(text, "[ -s /data/etc/dropbear/dropbear_rsa_host_key ] || keygen", p)
    factory = has_enable and has_bluetooth and has_ssh_en
    patched = (not has_enable and not has_bluetooth and not has_ssh_en
               and "mkdir -p /data/etc/dropbear" in text)
    if not (factory or patched):
        raise PatchError(
            f"前置条件不满足：{p} 既不是原厂状态也不是已打补丁状态"
            f"（enable={has_enable} 蓝牙={has_bluetooth} ssh_en={has_ssh_en}）")


def patch_ssh(rootfs: Path) -> None:
    """去掉 enable=0 返回、蓝牙模式不启动、/data/ssh_en 门控；启动时建目录。

    已打过补丁时每一步都跳过（只做缺失的子步骤），重跑不报错。"""
    check_ssh(rootfs)
    p = rootfs / "etc/init.d/dropbear"
    text = _read(p)
    text = text.replace(_DROPBEAR_ENABLE_LINE, "")
    text, n = _DROPBEAR_BLUETOOTH_RE.subn("", text, count=1)
    if n not in (0, 1):
        raise PatchError(f"蓝牙模式块替换失败：{p}")
    text, n = _DROPBEAR_SSH_EN_RE.subn("", text, count=1)
    if n not in (0, 1):
        raise PatchError(f"ssh_en 门控块替换失败：{p}")
    if "mkdir -p /data/etc/dropbear" not in text:
        text = text.replace(_DROPBEAR_KEYGEN_LINE,
                            _DROPBEAR_MKDIR_LINE + _DROPBEAR_KEYGEN_LINE, 1)
    _write(p, text)
    # 校验
    after = _read(p)
    _require_not_in(after, '[ "${enable}" = "0" ] && return 1', p)
    _require_not_in(after, "micocfg_work_mode", p)
    _require_not_in(after, "/data/ssh_en", p)
    _require_not_in(after, "/data/.ssh_en", p)
    _require_not_in(after, "/tmp/ssh_en", p)
    _require_in(after, "mkdir -p /data/etc/dropbear", p)
    _require_in(after, "[ -s /data/etc/dropbear/dropbear_rsa_host_key ] || keygen", p)


# ---------------------------------------------------------------------------
# 2. 登录：shadow / inittab / common-auth（来源：02-login.patch 的效果）
# ---------------------------------------------------------------------------

def check_login(rootfs: Path) -> None:
    shadow = _require(rootfs / "etc/shadow", "密码文件")
    text = _read(shadow)
    # 原厂 root 行是 md5crypt（$1$）；已打过本补丁的 rootfs 是 sha512crypt
    # （$6$），sha256crypt（$5$）也接受。其他算法一律拒绝。
    if not re.search(r"^root:\$[156]\$[^:]+:", text, re.MULTILINE):
        raise PatchError(f"前置条件不满足：{shadow} 里 root 行不是 $1$/$5$/$6$ hash")
    inittab = _require(rootfs / "etc/inittab", "init 表")
    _require_in(_read(inittab), "ttyS0::askfirst:/bin/login", inittab)
    pam = _require(rootfs / "etc/pam.d/common-auth", "PAM 配置")
    pam_text = _read(pam)
    # 接受两种状态：原厂（libmico 未注释、pam_unix 被注释）与已打过本补丁
    # （libmico 已注释、pam_unix 已启用）；其他状态一律拒绝。
    # （2026-11-17：旧版只认原厂状态，已打过补丁的 rootfs 重跑 patch_login
    # 必在这里失败；见 test_patch_login_twice_on_sha512_shadow。）
    for line in (r"auth sufficient libmico-pam\.so",
                 r"account sufficient libmico-pam\.so"):
        if not re.search(r"^#?\s*" + line + r"\s*$",
                         pam_text, re.MULTILINE):
            raise PatchError(f"前置条件不满足：{pam} 里找不到 {line} 行")
    if not re.search(r"^#?\s*auth\s+\[success=1 default=ignore\]\s+pam_unix\.so",
                     pam_text, re.MULTILINE):
        raise PatchError(f"前置条件不满足：{pam} 里找不到 pam_unix.so 行")


def patch_login(rootfs: Path) -> str:
    """换 root 密码 hash；串口仍走 /bin/login（需 root 密码，P1-36：
    原先改成 /bin/ash --login 是拆机串口即免密 root）；PAM 去 libmico、启用 pam_unix。

    密码只从 SSH_PASSWORD_FILE / 环境变量 SSH_PASSWORD 或交互式 getpass
    读（见 _password），hash 为 sha512crypt（$6$）+ 每次随机盐。返回实际写入的 hash，
    供测试校验；调用方（main）不得打印它。
    """
    check_login(rootfs)
    password = _password()
    new_hash = sha512_crypt(password)

    shadow = rootfs / "etc/shadow"
    text = _read(shadow)
    text, n = re.subn(r"^root:\$[156]\$[^:]+:", f"root:{new_hash}:", text,
                      count=1, flags=re.MULTILINE)
    if n != 1:
        raise PatchError(f"root 密码行替换失败：{shadow}")
    _write(shadow, text)

    # P1-36：inittab 不动——串口仍走 ttyS0::askfirst:/bin/login，登录要输
    # root 密码（打补丁时设的强密码）。串口恢复仍可用（有密码就能进），
    # 只是不再是拆机即免密 root。
    inittab = rootfs / "etc/inittab"

    pam = rootfs / "etc/pam.d/common-auth"
    text = _read(pam)
    # 只注释尚未注释的行（已打过补丁时跳过，避免 "# # " 双重注释）。
    text = re.sub(r"^auth sufficient libmico-pam\.so\s*$",
                  "# auth sufficient libmico-pam.so", text,
                  count=1, flags=re.MULTILINE)
    text = re.sub(r"^account sufficient libmico-pam\.so\s*$",
                  "# account sufficient libmico-pam.so", text,
                  count=1, flags=re.MULTILINE)
    # 只启用仍被注释的 pam_unix 行（已启用时跳过）。
    text, n = re.subn(r"^#\s*auth(\s+\[success=1 default=ignore\]\s+pam_unix\.so.*)$",
                      r"auth\1", text, count=1, flags=re.MULTILINE)
    if n == 0 and not re.search(r"^auth\s+\[success=1 default=ignore\]\s+pam_unix\.so",
                                text, re.MULTILINE):
        raise PatchError(f"pam_unix.so 行启用失败：{pam}")
    _write(pam, text)

    # 校验
    after = _read(shadow)
    _require_in(after, f"root:{new_hash}:", shadow)
    after = _read(inittab)
    _require_in(after, "ttyS0::askfirst:/bin/login", inittab)
    _require_not_in(after, "ttyS0::askfirst:/bin/ash --login", inittab)
    after = _read(pam)
    _require_not_in(after, "\nauth sufficient libmico-pam.so", pam)
    _require_not_in(after, "\naccount sufficient libmico-pam.so", pam)
    if not re.search(r"^auth\s+\[success=1 default=ignore\]\s+pam_unix\.so",
                     after, re.MULTILINE):
        raise PatchError(f"校验失败：{pam} 里 pam_unix.so 未启用")
    return new_hash


# ---------------------------------------------------------------------------
# 3. 禁 OTA：bin/ota、bin/flash.sh、etc/init.d/wireless（来源：03-ota.patch）
# ---------------------------------------------------------------------------

def _func_disabled(text: str, func_re: str, path: Path) -> bool:
    """函数头后 80 字符内已有 return 1 即视为已禁（P2-32：重跑时跳过）。"""
    m = re.search(func_re, text, re.MULTILINE)
    if m is None:
        raise PatchError(f"前置条件不满足：{path} 里找不到 {func_re}")
    return "return 1" in text[m.end():m.end() + 80]


def _insert_return1_after_func(text: str, func_re: str, path: Path) -> str:
    """在 `name() {` 的下一行插入 return 1（函数开头直接返回）。

    已禁过时原样返回（不报错），由调用方按返回值判断是否改动；
    找不到函数头时抛 PatchError。"""
    m = re.search(func_re, text, re.MULTILINE)
    if not m:
        raise PatchError(f"前置条件不满足：{path} 里找不到 {func_re}")
    insert_at = m.end()
    rest = text[insert_at:]
    indent = "\t" if rest.startswith("\n\t") else "    "
    if rest.startswith(f"\n{indent}return 1\n"):
        return text  # 已禁过：幂等跳过
    return text[:insert_at] + f"\n{indent}return 1" + text[insert_at:]


def check_disable_ota(rootfs: Path) -> None:
    """接受原厂与已禁两种状态（每处函数头必须在；已禁的函数不再要求
    “未禁”。P2-32：旧版把“已被禁过”当成前置失败，重跑必炸。）"""
    ota = _require(rootfs / "bin/ota", "OTA 脚本")
    text = _read(ota)
    _require_in(text, "download_upgrade() {", ota)
    if not re.search(r"^upgrade\(\) \{$", text, re.MULTILINE):
        raise PatchError(f"前置条件不满足：{ota} 里找不到 upgrade() 函数头")
    flash = _require(rootfs / "bin/flash.sh", "刷机脚本")
    _require_in(_read(flash), "upgrade_param_check() {", flash)
    wireless = _require(rootfs / "etc/init.d/wireless", "无线启动脚本")
    wtext = _read(wireless)
    if not re.search(r"^ota\(\)\n\{\n    ota_url=\$1\n    ssid=\$2\n    password=\$3\n"
                     r"    identity=\$4\n", wtext, re.MULTILINE):
        raise PatchError(f"前置条件不满足：{wireless} 里找不到 ota() 函数头")


def patch_disable_ota(rootfs: Path) -> None:
    """bin/ota 的 download_upgrade/upgrade、flash.sh 的参数检查开头 return 1；
    wireless 的 ota() 开头 echo download_fail + exit 3。"""
    check_disable_ota(rootfs)

    ota = rootfs / "bin/ota"
    text = _read(ota)
    text = _insert_return1_after_func(text, r"^download_upgrade\(\) \{$", ota)
    text = _insert_return1_after_func(text, r"^upgrade\(\) \{$", ota)
    _write(ota, text)

    flash = rootfs / "bin/flash.sh"
    text = _read(flash)
    text = _insert_return1_after_func(text, r"^upgrade_param_check\(\) \{$", flash)
    _write(flash, text)

    wireless = rootfs / "etc/init.d/wireless"
    text = _read(wireless)
    header_re = (r"(^ota\(\)\n\{\n    ota_url=\$1\n    ssid=\$2\n    password=\$3\n"
                 r"    identity=\$4\n)")
    m = re.search(header_re, text, re.MULTILINE)
    if not m:
        raise PatchError(f"ota() 函数头替换失败：{wireless}")
    if not text[m.end():m.end() + 60].startswith(
            '\n    echo "download_fail"\n    exit 3'):
        # 未禁过才插；已禁过跳过（P2-32 幂等）。
        # 注意：m.end() 紧跟在 `identity=$4\n` 之后，插入块以 `\n` 开头，
        # 所以判断也要带上开头的 `\n`（旧代码漏了它，判断恒为假，重跑会重复插）。
        text = text[:m.end()] + '\n    echo "download_fail"\n    exit 3\n' + text[m.end():]
        _write(wireless, text)

    # 校验
    after = _read(ota)
    for func_re in (r"^download_upgrade\(\) \{$", r"^upgrade\(\) \{$"):
        m = re.search(func_re, after, re.MULTILINE)
        if m is None or "return 1" not in after[m.end():m.end() + 80]:
            raise PatchError(f"校验失败：{ota} 里 {func_re} 后没有 return 1")
    after = _read(flash)
    m = re.search(r"^upgrade_param_check\(\) \{$", after, re.MULTILINE)
    if m is None or "return 1" not in after[m.end():m.end() + 80]:
        raise PatchError(f"校验失败：{flash} 里 upgrade_param_check() 后没有 return 1")
    after = _read(wireless)
    m = re.search(header_re, after, re.MULTILINE)
    _require_in(after[m.end():m.end() + 60], 'echo "download_fail"', wireless)
    _require_in(after[m.end():m.end() + 60], "exit 3", wireless)


# ---------------------------------------------------------------------------
# 4. 启动：/etc/rc.local 追加 /data/init.sh（来源：04-start.patch）
# ---------------------------------------------------------------------------

def check_start(rootfs: Path) -> None:
    rc = _require(rootfs / "etc/rc.local", "启动脚本")
    _require_in(_read(rc), "exit 0", rc)


def patch_start(rootfs: Path) -> None:
    """rc.local 里在 exit 0 前加包装器启动行（P2-34），并装入包装器文件。

    迁移：已有旧直行（RC_LOCAL_LINE）时替换为包装器行；已有包装器行
    时不重复加；都没有时新加。包装器内容固定，不同即重写（0755）。
    重跑不报错、不重复。"""
    check_start(rootfs)
    wrapper = rootfs / INIT_WRAPPER_PATH
    wrapper.parent.mkdir(parents=True, exist_ok=True)
    _write_bytes(wrapper, INIT_WRAPPER_SCRIPT.encode("utf-8"), 0o755)
    rc = rootfs / "etc/rc.local"
    text = _read(rc)
    if RC_LOCAL_WRAPPER_LINE in text:
        pass  # 已有包装器行：不重复加
    elif RC_LOCAL_LINE in text:
        text = text.replace(RC_LOCAL_LINE, RC_LOCAL_WRAPPER_LINE, 1)
        _write(rc, text)
    else:
        text = text.replace("exit 0", RC_LOCAL_WRAPPER_LINE + "\n\nexit 0", 1)
        _write(rc, text)
    after = _read(rc)
    if after.count(RC_LOCAL_WRAPPER_LINE) != 1:
        raise PatchError(f"校验失败：{rc} 里包装器启动行出现 "
                         f"{after.count(RC_LOCAL_WRAPPER_LINE)} 次")
    if RC_LOCAL_LINE in after:
        raise PatchError(f"校验失败：{rc} 里仍有旧的 init.sh 直行")
    if after.index(RC_LOCAL_WRAPPER_LINE) > after.index("exit 0"):
        raise PatchError(f"校验失败：{rc} 里包装器启动行在 exit 0 之后")
    wtext = wrapper.read_text(errors="surrogateescape")
    for needle in ("ls -ln", "sha256sum", "init.sh.sha256", "exec sh"):
        _require_in(wtext, needle, wrapper)
    if (wrapper.stat().st_mode & 0o777) != 0o755:
        raise PatchError(f"校验失败：{wrapper} 权限不是 0755")


# ---------------------------------------------------------------------------
# 5. LX06/OH2P asound：mico_record / noop 走 Capture（来源：LX06 01/02-asound.patch）
# ---------------------------------------------------------------------------

def check_asound(rootfs: Path) -> None:
    for rel in ASOUND_FILES:
        p = _require(rootfs / rel, "ALSA 配置")
        text = _read(p)
        _require_in(text, "pcm.mico_record {", p)
        _require_in(text, "pcm.noop {", p)


def patch_asound(rootfs: Path) -> list[str]:
    """两份 asound 配置：mico_record/noop 改走 Capture，补 Capture 块与
    rate_converter（已存在则不重复加）。返回改动的文件列表。

    P2-33：第一次改动前先把原厂内容存为 <文件>.bak（只在 .bak 不存在时
    写，绝不覆盖已有的备份——重跑时 .bak 里永远是原厂内容，设备端可
    `cp asound.conf.bak asound.conf` 回滚，不用重解官方包）。"""
    check_asound(rootfs)
    changed = []
    for rel in ASOUND_FILES:
        p = rootfs / rel
        text = _read(p)
        bak = rootfs / (rel + ".bak")
        if not bak.is_file():
            _write_bytes(bak, text.encode("utf-8", errors="surrogateescape"),
                         p.stat().st_mode & 0o7777)
        text = _replace_pcm_block(text, "mico_record", MICO_RECORD_BLOCK, p)
        text = _replace_pcm_block(text, "noop", NOOP_BLOCK, p)
        had_capture = "pcm.Capture {" in text
        if not had_capture:
            # 放在刚替换好的 noop 块之后
            idx = text.index(NOOP_BLOCK) + len(NOOP_BLOCK)
            text = text[:idx] + "\n\n" + CAPTURE_BLOCK + text[idx:]
        if RATE_CONVERTER_LINE not in text:
            text = text.rstrip("\n") + "\n\n" + RATE_CONVERTER_LINE + "\n"
        _write(p, text)
        changed.append(rel)

        # 校验：Capture 块的参数只在该块内查——文件里其他 PCM 块自有参数
        # （如 OH2P asound.conf 播放链里的 period_size 480），与录音无关。
        after = _read(p)
        _require_in(after, "pcm.mico_record {\n    type plug\n    slave.pcm Capture\n}", p)
        _require_in(after, "pcm.noop {\n    type plug\n    slave.pcm Capture\n}", p)
        capture = _extract_pcm_block(after, "Capture", p)
        for needle in ('pcm "hw:0,3"', "type dsnoop", "rate 48000", "format S32_LE",
                       "period_size 384", "buffer_size 6144"):
            _require_in(capture, needle, p)
        _require_not_in(capture, "period_size 480", p)
        _require_not_in(capture, "buffer_size 3840", p)
        if had_capture:
            # 原厂已有的 Capture 块（如 OH2P asound.conf 的 channels 4）保持不动，
            # 只要求它有 channels 行，不强求 LX06 的 8 声道。
            if not re.search(r"channels \d+", capture):
                raise PatchError(f"校验失败：{p} 里 Capture 块没有 channels")
        else:
            _require_in(capture, "channels 8", p)
        if after.count(RATE_CONVERTER_LINE) != 1:
            raise PatchError(f"校验失败：{p} 里 rate_converter 出现 "
                             f"{after.count(RATE_CONVERTER_LINE)} 次")
    return changed


# ---------------------------------------------------------------------------
# 6. LX06 libxaudio：hw:0,3 -> noop（来源：LX06/03-libxaudio_engine.sh）
#    OH2P 没有这一步。
# ---------------------------------------------------------------------------

def find_libxaudio_offset(rootfs: Path) -> int:
    """用字节搜索定位 libxaudio_engine.so 里 "hw:0,3" 的偏移。

    与参考脚本的 `strings -t d ... | grep hw:0,3` 等价（实测 1.94.13：
    唯一一次出现，偏移 773628），但不依赖外部 strings 命令。
    """
    p = _require(rootfs / "usr/lib/libxaudio_engine.so", "音频引擎库")
    data = p.read_bytes()
    if data.count(LIBXAUDIO_TARGET) != 1:
        raise PatchError(f"前置条件不满足：{p} 里 hw:0,3 出现 "
                         f"{data.count(LIBXAUDIO_TARGET)} 次（要求恰好 1 次）")
    return data.find(LIBXAUDIO_TARGET)


def _libxaudio_state(rootfs: Path) -> tuple[str, int]:
    """返回 ("factory", hw:0,3 偏移) 或 ("patched", noop 偏移)。

    （P2-32：旧版只认 hw:0,3 恰好 1 次，已打过补丁的 rootfs
    重跑必在 check_libxaudio 失败。）"""
    p = _require(rootfs / "usr/lib/libxaudio_engine.so", "音频引擎库")
    data = p.read_bytes()
    n_target = data.count(LIBXAUDIO_TARGET)
    n_noop = data.count(LIBXAUDIO_REPLACEMENT)
    if n_target == 1 and n_noop == 0:
        return "factory", data.find(LIBXAUDIO_TARGET)
    if n_target == 0 and n_noop == 1:
        return "patched", data.find(LIBXAUDIO_REPLACEMENT)
    raise PatchError(f"前置条件不满足：{p} 里 hw:0,3 出现 {n_target} 次、"
                     f"noop 出现 {n_noop} 次（要求恰好其一为 1、另一为 0）")


def check_libxaudio(rootfs: Path) -> int:
    """原厂与已打补丁态都接受，返回要改（或已改）的偏移。"""
    return _libxaudio_state(rootfs)[1]


def patch_libxaudio(rootfs: Path) -> int:
    """把 hw:0,3 的 6 字节改成 noop\\x00\\x00。返回改动的偏移。

    已打过补丁时不重写，直接返回已有的 noop 偏移。"""
    state, offset = _libxaudio_state(rootfs)
    if state == "patched":
        return offset
    p = rootfs / "usr/lib/libxaudio_engine.so"
    data = bytearray(p.read_bytes())
    data[offset:offset + len(LIBXAUDIO_TARGET)] = LIBXAUDIO_REPLACEMENT
    _write_bytes(p, bytes(data))  # 原子写（P1-30），保留原文件权限
    after = p.read_bytes()
    if after[offset:offset + len(LIBXAUDIO_REPLACEMENT)] != LIBXAUDIO_REPLACEMENT:
        raise PatchError(f"校验失败：{p} 偏移 {offset} 处不是 noop")
    if LIBXAUDIO_TARGET in after:
        raise PatchError(f"校验失败：{p} 里仍有 hw:0,3")
    return offset


# ---------------------------------------------------------------------------
# 7. mTLS 客户端证书（P0-3）：把 CA + 客户端证书装进 rootfs（可选）
#    证书由 scripts/gen-certs.sh 生成；装入位置 /etc/open-xiaoai/tls/
#    （squashfs 内，刷机后一定存在；/data 是可写分区挂载点，烤进
#    rootfs/data 的文件会被挂载遮住，故不用 /data 路径）。
#    客户端（client/src/transport.rs）按此路径找证书。
# ---------------------------------------------------------------------------

CERT_FILES = ("ca.crt", "client.crt", "client.key")
CERT_DEST_DIR = "etc/open-xiaoai/tls"


def check_certs(rootfs: Path, cert_dir: Path) -> None:
    if not cert_dir.is_dir():
        raise PatchError(f"证书目录不存在：{cert_dir}（先跑 scripts/gen-certs.sh）")
    for name in CERT_FILES:
        p = cert_dir / name
        if not p.is_file():
            raise PatchError(f"证书目录缺文件：{p}")
        if p.stat().st_size == 0:
            raise PatchError(f"证书文件为空：{p}")
    for name in ("ca.crt", "client.crt"):
        text = (cert_dir / name).read_text(errors="surrogateescape")
        if "-----BEGIN CERTIFICATE-----" not in text:
            raise PatchError(f"证书格式不对：{cert_dir / name} 里没有 CERTIFICATE PEM 块")
    key_text = (cert_dir / "client.key").read_text(errors="surrogateescape")
    # 私钥可以是 PKCS#8（openssl genpkey / gen-certs.sh）或传统 PKCS#1
    #（Go 的 tlscert 生成）；两种 rustls 都能读。
    if ("-----BEGIN PRIVATE KEY-----" not in key_text
            and "-----BEGIN RSA PRIVATE KEY-----" not in key_text):
        raise PatchError(f"私钥格式不对：{cert_dir / 'client.key'} 里没有 PRIVATE KEY PEM 块")


def patch_certs(rootfs: Path, cert_dir: Path) -> list[str]:
    """把 CA/客户端证书拷进 rootfs 的 /etc/open-xiaoai/tls/。

    权限：证书 0644、私钥 0600。返回装入的文件列表。"""
    check_certs(rootfs, cert_dir)
    dest = rootfs / CERT_DEST_DIR
    dest.mkdir(parents=True, exist_ok=True)
    installed = []
    for name in CERT_FILES:
        src = cert_dir / name
        dst = dest / name
        # 原子写（P1-30）：证书 0644、私钥 0600。
        _write_bytes(dst, src.read_bytes(),
                     0o600 if name.endswith(".key") else 0o644)
        installed.append(f"{CERT_DEST_DIR}/{name}")
    # 校验：文件都在、PEM 头对、私钥权限 0600。
    for name in CERT_FILES:
        p = dest / name
        if not p.is_file():
            raise PatchError(f"校验失败：{p} 未装入")
        if name.endswith(".key") and (p.stat().st_mode & 0o777) != 0o600:
            raise PatchError(f"校验失败：{p} 权限不是 0600")
    return installed


# ---------------------------------------------------------------------------
# 总入口
# ---------------------------------------------------------------------------

MODELS = ("lx06", "oh2p")


def preflight(rootfs: Path, model: str, cert_dir: Path | None = None) -> None:
    """只读检查全部步骤；任一失败即抛 PatchError，且此时未写任何文件。"""
    if model not in MODELS:
        raise PatchError(f"未知型号：{model}（只支持 lx06 / oh2p）")
    if not rootfs.is_dir():
        raise PatchError(f"rootfs 不存在：{rootfs}")
    check_ssh(rootfs)
    check_login(rootfs)
    check_disable_ota(rootfs)
    check_start(rootfs)
    check_asound(rootfs)
    if model == "lx06":
        check_libxaudio(rootfs)
    if cert_dir is not None:
        check_certs(rootfs, cert_dir)


# patch_all 可能改动的文件（P1-30 回滚快照用；新增文件记为不存在）。
# asound .bak（P2-33）与 init 包装器（P2-34）也是 patch 产物：失败回滚
# 时一并恢复/删除，否则半打状态的 rootfs 里会留一份新文件。
_PATCHED_FILES = (
    "etc/init.d/dropbear",
    "etc/shadow",
    "etc/inittab",
    "etc/pam.d/common-auth",
    "bin/ota",
    "bin/flash.sh",
    "etc/init.d/wireless",
    "etc/rc.local",
    "etc/asound.conf",
    "etc/asound.conf.dts",
    "etc/asound.conf.bak",
    "etc/asound.conf.dts.bak",
    "etc/open-xiaoai/init.sh",
    "usr/lib/libxaudio_engine.so",
)


def _snapshot(rootfs: Path, model: str,
              cert_dir: Path | None) -> dict[str, tuple[bytes | None, int | None]]:
    """记下全部可能被改文件的原内容与权限；不存在记 (None, None)。"""
    rels = list(_PATCHED_FILES)
    if model != "lx06":
        rels.remove("usr/lib/libxaudio_engine.so")
    if cert_dir is not None:
        rels += [f"{CERT_DEST_DIR}/{name}" for name in CERT_FILES]
    snap: dict[str, tuple[bytes | None, int | None]] = {}
    for rel in rels:
        p = rootfs / rel
        if p.is_file():
            snap[rel] = (p.read_bytes(), p.stat().st_mode & 0o7777)
        else:
            snap[rel] = (None, None)
    return snap


def _restore(rootfs: Path, snap: dict[str, tuple[bytes | None, int | None]],
             cert_dir: Path | None) -> None:
    """把快照原样写回（原子写、恢复权限）；快照里不存在的文件删掉。"""
    for rel, (data, mode) in snap.items():
        p = rootfs / rel
        if data is None:
            try:
                p.unlink()
            except FileNotFoundError:
                pass
        else:
            _write_bytes(p, data, mode)
    if cert_dir is not None:
        # 证书目录是 patch_certs 新建的：快照前不存在且现已空就删掉。
        try:
            (rootfs / CERT_DEST_DIR).rmdir()
        except OSError:
            pass
    # 包装器目录（P2-34）同理：包装器删掉后若目录已空就删掉。
    try:
        (rootfs / "etc/open-xiaoai").rmdir()
    except OSError:
        pass


def patch_all(rootfs: Path, model: str, cert_dir: Path | None = None) -> dict:
    """先 preflight 再逐项应用。OH2P 跳过 libxaudio。返回每步的结果摘要。

    注意：返回里不含密码 hash（P0-2：hash 永不打印）；cert_dir 给出时
    另装 mTLS 客户端证书。
    P1-30：任一步失败即把已改文件按快照恢复原样再抛错——失败的 patch_all
    不留半打状态的 rootfs，重跑 patch.sh（或 patch_firmware.py）可直接重试。
    """
    model = model.lower()
    preflight(rootfs, model, cert_dir)
    snap = _snapshot(rootfs, model, cert_dir)
    try:
        return _patch_all_apply(rootfs, model, cert_dir)
    except Exception:
        try:
            _restore(rootfs, snap, cert_dir)
        except Exception:
            pass
        raise


def _patch_all_apply(rootfs: Path, model: str,
                     cert_dir: Path | None = None) -> dict:
    result: dict = {}
    patch_ssh(rootfs)
    result["ssh"] = "dropbear 始终启动"
    patch_login(rootfs)
    result["login"] = "root 密码已换 sha512crypt（$6$，随机盐）+ 串口仍走密码登录 + PAM pam_unix"
    patch_disable_ota(rootfs)
    result["disable_ota"] = "bin/ota + flash.sh + wireless ota() 已禁"
    patch_start(rootfs)
    result["start"] = "rc.local 已加包装器启动行 + etc/open-xiaoai/init.sh（P2-34：校验属主/权限/可选 SHA256 pin 后才执行 /data/init.sh）"
    result["asound"] = patch_asound(rootfs)
    if model == "lx06":
        result["libxaudio_offset"] = patch_libxaudio(rootfs)
    else:
        result["libxaudio_offset"] = None  # OH2P 没有这一步，跳过
        print("OH2P：跳过 libxaudio 补丁（该型号无此步骤）", file=sys.stderr)
    if cert_dir is not None:
        result["certs"] = patch_certs(rootfs, cert_dir)
    return result


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="给解包后的 rootfs 副本打补丁")
    ap.add_argument("--model", required=True, choices=list(MODELS))
    ap.add_argument("--rootfs", required=True, type=Path)
    ap.add_argument("--cert-dir", default=None, type=Path,
                    help="mTLS 证书目录（gen-certs.sh 的输出，含 ca.crt/client.crt/client.key），"
                         "给出则装进 rootfs 的 /etc/open-xiaoai/tls/")
    args = ap.parse_args(argv)
    try:
        result = patch_all(args.rootfs, args.model, args.cert_dir)
    except PatchError as exc:
        print(f"补丁失败：{exc}", file=sys.stderr)
        return 1
    print(f"补丁完成：型号={args.model} rootfs={args.rootfs}")
    for key, value in result.items():
        print(f"  {key}: {value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
