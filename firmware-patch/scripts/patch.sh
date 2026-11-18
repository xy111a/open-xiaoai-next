#!/usr/bin/env bash
# 打补丁 + 重新打包（Phase 1 实现）
# 目标：从解包后的 rootfs 出发，产出可刷的补丁固件 root-patched.squashfs。
#
# 已验证的补丁参数（证据见 ../../firmware/REA_ANALYSIS.md，别凭记忆改）：
#   - 分区上限 40MB（0x02800000）：补丁后 squashfs 必须小于 40MB，否则刷机变砖
#   - A/B 分区：system0 / system1；目标槽以 FLASH_RUNBOOK.md §3 决策表为准
#     （按实测 boot_part 定，不要默认刷 boot0；boot1 不一定是原厂槽）
#   - 禁用自动升级：OTA 升级入口在 /bin/ota，补丁后不再接受小米 OTA
#   - asound.conf：录音链走 Capture（dsnoop hw:0,3，48kHz，S32_LE，8ch，
#     period 384，buffer 6144）；DTS 链路刷机后不再启用，别修它
#   - 校验：补丁包不走 miso（RSA + SHA1，小米私钥签的原包才能过），
#     之后走 Hub 自有 OTA
#
# 实现以 idootop/open-xiaoai packages/client-patch/src/squashfs.sh 与
# patches/*.patch 为 Spec 逐行对照移植（效果移植，非代码复制）；
# 每一步都是“失败即停、不写分区”：任何一步失败都 exit 非零，并删掉
# 半成品镜像，不留下“看起来成功”的输出。
#
# 用法：patch.sh lx06 1.94.13
#   前置：先跑 unpack.sh lx06 1.94.13（得到 work/lx06_1.94.13/rootfs
#   与 work/lx06_1.94.13/root.squashfs）
#   输出：work/lx06_1.94.13/root-patched.squashfs
#   环境：PATCH_WORK_DIR 可覆盖 work 目录（测试用）；OTA_BIN 可直接指定 .bin
#         路径（白名单校验用它；不指定则用 work/<型号>_<版本>.bin，找不到
#         则退而校验原始 root.squashfs）；SSH_PASSWORD 改 root 密码；
#         CERT_DIR 可覆盖 mTLS 证书目录（默认 $HERE/work/certs，gen-certs.sh
#         的输出）；PATCH_NO_CERTS=1 跳过证书装入（仅测试用）。
set -euo pipefail

MODEL_RAW="${1:?用法: patch.sh lx06 1.94.13}"
VERSION="${2:?用法: patch.sh lx06 1.94.13}"
MODEL="$(printf '%s' "$MODEL_RAW" | tr 'A-Z' 'a-z')"

# --- 入参校验 ---
case "$MODEL" in
    lx06|oh2p) ;;
    *) echo "未知型号：$MODEL_RAW（只支持 lx06 / oh2p）" >&2; exit 1 ;;
esac
# P2-9：版本号必须严格是 x.y.z——case 的 `.` 是通配符，"1/94/13" 也能
# 通过，拼进路径就是穿越；用正则锚定首尾。
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "版本号格式不对：$VERSION（应为 x.y.z，如 1.94.13）" >&2; exit 1
fi

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${PATCH_WORK_DIR:-$HERE/work}"
DIR="$WORK/${MODEL}_${VERSION}"
SRC_ROOTFS="$DIR/rootfs"
SQFS_ROOT="$DIR/squashfs-root"
OUT="$DIR/root-patched.squashfs"
MAX_SIZE=$((0x02800000))
VERIFY_DIR=""

# 失败出口：报错 + 删掉半成品镜像、验证目录与工作副本，再非零退出。
# 工作副本（SQFS_ROOT）是 SRC_ROOTFS 的复制，删了可重跑，不留半打状态。
fail() {
    echo "失败：$*" >&2
    rm -f "$OUT"
    [ -n "$VERIFY_DIR" ] && rm -rf "$VERIFY_DIR"
    [ -n "$SQFS_ROOT" ] && rm -rf "$SQFS_ROOT"
    exit 1
}
# 未经 fail() 的意外失败（set -e 触发）同样清理半成品镜像与工作副本。
trap 'rm -f "$OUT"; [ -n "$VERIFY_DIR" ] && rm -rf "$VERIFY_DIR"; [ -n "$SQFS_ROOT" ] && rm -rf "$SQFS_ROOT"' ERR

# mTLS 客户端证书必须打进镜像（否则音箱连不上 Hub 的 mTLS 4399）：
# 默认用 $HERE/work/certs（gen-certs.sh 的输出）；CERT_DIR 可覆盖。
# 目录不存在时直接失败——静默产出无证书镜像是发布事故（2026-11-17 的
# 首版镜像是绕过 patch.sh、直接调 patch_firmware.py --cert-dir 打的，
# patch.sh 本身从未传过证书）。仅测试可设 PATCH_NO_CERTS=1 跳过。
CERT_DIR="${CERT_DIR:-$HERE/work/certs}"
CERT_ARGS=()
if [ -d "$CERT_DIR" ]; then
    CERT_ARGS=(--cert-dir "$CERT_DIR")
elif [ -n "${PATCH_NO_CERTS:-}" ]; then
    echo "警告：PATCH_NO_CERTS=1，跳过证书装入（仅测试）" >&2
else
    fail "证书目录不存在：$CERT_DIR（先跑 scripts/gen-certs.sh，或设 CERT_DIR / PATCH_NO_CERTS=1）"
fi

# P1-8：find 必须恰好命中一个；多个即失败（任选其一是任意的），
# 零个也失败。只打印命中的唯一路径（与 unpack.sh 同款辅助函数）。
find_exactly_one() {
    local dir="$1" name="$2"
    local -a hits
    mapfile -t hits < <(find "$dir" -name "$name" 2>/dev/null)
    if [ "${#hits[@]}" -eq 0 ]; then
        return 1
    fi
    if [ "${#hits[@]}" -gt 1 ]; then
        echo "找到多个 $name（${#hits[@]} 个），拒绝任选：" >&2
        printf '  %s\n' "${hits[@]}" >&2
        return 1
    fi
    printf '%s' "${hits[0]}"
}

# --- 前置：rootfs 与原始 root.squashfs 必须存在 ---
[ -d "$SRC_ROOTFS" ] || fail "找不到 $SRC_ROOTFS，先跑 unpack.sh $MODEL $VERSION"
ORIG_SQFS="$DIR/root.squashfs"
if [ ! -f "$ORIG_SQFS" ]; then
    ORIG_SQFS="$(find_exactly_one "$DIR-extract" 'root.squashfs')" \
        || fail "找不到原始 root.squashfs（$DIR/root.squashfs 或 $DIR-extract 下恰好一个）"
fi
[ -n "$ORIG_SQFS" ] && [ -f "$ORIG_SQFS" ] || fail "找不到原始 root.squashfs（$DIR/root.squashfs 或 $DIR-extract 下）"

# --- 0. 白名单哈希校验（P0-5）：拒绝用未验证的官方包打补丁 ---
# .bin 若在（常规流程 unpack.sh 已把它拷进 work），校验它；若不在（例如
# .bin 已删，只剩解包产物），退而校验原始 root.squashfs——它是从已验证
# .bin 解出的唯一产物，白名单在 download-ota.py 的 EXPECTED_SQFS 里。
# 校验逻辑以 download-ota.py 为唯一源，这里不另写哈希表。
BIN_CANDIDATE="${OTA_BIN:-$WORK/${MODEL}_${VERSION}.bin}"
if [ -f "$BIN_CANDIDATE" ]; then
    python3 "$HERE/scripts/download-ota.py" verify-bin "$MODEL" "$VERSION" "$BIN_CANDIDATE" \
        || fail "拒绝打补丁：$BIN_CANDIDATE 未通过白名单校验"
else
    echo "未找到 $BIN_CANDIDATE，改校验原始 root.squashfs"
    python3 "$HERE/scripts/download-ota.py" verify-sqfs "$MODEL" "$VERSION" "$ORIG_SQFS" \
        || fail "拒绝打补丁：$ORIG_SQFS 未通过白名单校验"
fi

# --- 1. 复制 rootfs 到工作副本（绝不原地改解包结果） ---
echo "复制 rootfs → $SQFS_ROOT"
rm -rf "$SQFS_ROOT"
mkdir -p "$DIR"
cp -a "$SRC_ROOTFS" "$SQFS_ROOT"

# --- 2. 打补丁（纯 Python，见 scripts/patch_firmware.py） ---
echo "打补丁（$MODEL）..."
python3 "$HERE/scripts/patch_firmware.py" --model "$MODEL" --rootfs "$SQFS_ROOT" \
    "${CERT_ARGS[@]}" \
    || fail "patch_firmware.py 失败"

# --- 3. 用 file 读原始 root.squashfs 的压缩方式与 blocksize ---
SQFS_INFO="$(file "$ORIG_SQFS")"
echo "原始固件信息：$SQFS_INFO"
# P2-31：两条管道末尾必须加 `|| true`。set -euo pipefail 下，grep 无匹配
# 时整条管道失败，set -e 直接退出，根本走不到下面的显式 `[ -n … ] || fail`——
# 失败分支不可达，报错信息也出不来。加 `|| true` 让显式检查真正跑到。
COMPRESSION="$(printf '%s' "$SQFS_INFO" | grep -oE '(xz|gzip|lzo|lz4|zstd) compressed' | cut -d' ' -f1 | head -1 || true)"
BLOCKSIZE="$(printf '%s' "$SQFS_INFO" | grep -oE 'blocksize: [0-9]+' | grep -oE '[0-9]+' | head -1 || true)"
[ -n "$COMPRESSION" ] || fail "读不到压缩方式：$SQFS_INFO"
[ -n "$BLOCKSIZE" ] || fail "读不到 blocksize：$SQFS_INFO"
echo "打包参数：-comp $COMPRESSION -b $BLOCKSIZE"

# --- 4. mksquashfs 重新打包（与原始参数一致） ---
command -v mksquashfs >/dev/null || fail "缺 mksquashfs（apt 装 squashfs-tools）"
# P2-30：mksquashfs 参数与原厂不完全一致——原厂 xz 选项是 14 字节非默认
# 值（超级块后 0c 80 00 00 02 00 14 00 09 00 90 00 40 00），stock 工具
# 读不出，无法精确复现；当前镜像 38,453,248 字节，比原厂 +952,549。
# `-Xbcj arm` 实测可再省约 1MB（37,380,096），但音箱内核是否支持 BCJ
# 未证实，猜错会挂不上 rootfs——默认不开。首刷关口可在备用槽 A/B 测试：
#   PATCH_XZ_BCJ=arm patch.sh lx06 1.94.13
BCJ_ARGS=()
if [ "${PATCH_XZ_BCJ:-}" = "arm" ]; then
    [ "$COMPRESSION" = "xz" ] || fail "PATCH_XZ_BCJ 只对 xz 压缩有效，当前是 $COMPRESSION"
    BCJ_ARGS=(-Xbcj arm)
    echo "注意：PATCH_XZ_BCJ=arm 已开（实验性，未证实内核支持 BCJ；只刷备用槽）"
fi
echo "重新打包 → $OUT"
mksquashfs "$SQFS_ROOT" "$OUT" \
    -comp "$COMPRESSION" -b "$BLOCKSIZE" \
    -noappend -all-root -always-use-fragments -no-xattrs -no-exports \
    "${BCJ_ARGS[@]}" \
    || fail "mksquashfs 打包失败"

# --- 5. 大小校验：必须 < 40MB，否则失败且不输出镜像 ---
SIZE="$(stat -L -c %s "$OUT")"
ORIG_SIZE="$(stat -L -c %s "$ORIG_SQFS")"
WARN_SIZE=$((38 * 1024 * 1024))  # 38MB = 上限的 95%，预警线
echo "补丁后大小：$SIZE 字节（上限 $MAX_SIZE；原厂 $ORIG_SIZE，增长 $((SIZE - ORIG_SIZE))）"
if [ "$SIZE" -ge "$MAX_SIZE" ]; then
    fail "补丁后 squashfs $SIZE 字节，超过 40MB 上限，拒绝输出镜像"
fi
# P2-30：38MB 预警 + 比原厂增长超 2MB 警告（当前基线 +952,549，未触发）。
if [ "$SIZE" -ge "$WARN_SIZE" ]; then
    echo "警告：补丁后大小 $SIZE 已超 38MB 预警线（上限 95%），下次加功能前先减体积" >&2
fi
if [ "$((SIZE - ORIG_SIZE))" -gt $((2 * 1024 * 1024)) ]; then
    echo "警告：补丁后比原厂大 $((SIZE - ORIG_SIZE)) 字节（超 2MB），检查是否带了多余文件" >&2
fi
echo "大小检查通过，剩余 $((MAX_SIZE - SIZE)) 字节"

# --- 6. 解开验证：关键文件存在且补丁都在 ---
command -v unsquashfs >/dev/null || fail "缺 unsquashfs（apt 装 squashfs-tools）"
VERIFY_DIR="$(mktemp -d)"
# P2-39：-no-exit-code 会掩盖解包错误（容器里建 /dev/* 设备节点会报
# Operation not permitted，非致命；原厂 rootfs 的 dev/ 本就是空的）。
# 所以把输出收到日志里单独校验：只允许“建设备节点失败（无权限）”与
# “xz 压缩选项读不出”这两类良性行，其余含 error/failed 的行即 FAIL；
# 再用 inode 数硬门：期望解出数 = unsquashfs -s 的 inodes − 建设备失败行数
# （find 含根目录本身，inodes 也含根目录，两边口径一致，不用减 1）。
UNSQUASH_LOG="$VERIFY_DIR/unsquashfs.log"
unsquashfs -d "$VERIFY_DIR/rootfs" -f -no-exit-code "$OUT" >"$UNSQUASH_LOG" 2>&1 \
    || fail "unsquashfs 解开验证失败"
EXPECT_INODES="$(unsquashfs -s "$OUT" 2>/dev/null | grep -oE 'Number of inodes [0-9]+' | grep -oE '[0-9]+$' || true)"
DEV_FAILS="$(grep -cE 'failed to create (character|block) device .* Operation not permitted' "$UNSQUASH_LOG" || true)"
FOUND="$(find "$VERIFY_DIR/rootfs" | wc -l)"
if [ -n "$EXPECT_INODES" ]; then
    EXPECT_FOUND="$((EXPECT_INODES - DEV_FAILS))"
    [ "$FOUND" -eq "$EXPECT_FOUND" ] || fail "解包数量不符：解出 $FOUND，期望 $EXPECT_FOUND（inodes $EXPECT_INODES − 建设备失败 $DEV_FAILS）"
else
    echo "警告：读不到 unsquashfs -s 的 inode 数，跳过数量硬门" >&2
fi
# stderr 过滤：除两类良性行外，含 error/failed（不区分大小写）即 FAIL。
if grep -iE 'error|failed' "$UNSQUASH_LOG" \
    | grep -vE 'failed to create (character|block) device .* Operation not permitted|error reading stored compressor options' \
    | grep -q .; then
    grep -iE 'error|failed' "$UNSQUASH_LOG" >&2
    fail "unsquashfs 日志里有非良性 error/failed 行（见上）"
fi
for f in etc/init.d/dropbear etc/shadow etc/inittab etc/pam.d/common-auth \
         bin/ota bin/flash.sh etc/init.d/wireless etc/rc.local \
         etc/asound.conf etc/asound.conf.dts \
         etc/asound.conf.bak etc/asound.conf.dts.bak \
         etc/open-xiaoai/init.sh; do
    [ -f "$VERIFY_DIR/rootfs/$f" ] || fail "验证失败：解开后缺 $f"
done
# libxaudio_engine.so 只有 LX06 有；OH2P 没有这个文件（见 unpack.sh）。
if [ "$MODEL" = "lx06" ]; then
    [ -f "$VERIFY_DIR/rootfs/usr/lib/libxaudio_engine.so" ] || fail "验证失败：解开后缺 usr/lib/libxaudio_engine.so"
fi
# 补丁标记抽查（F-1：断言只认补丁特有标记，见
# scripts/patch_firmware.py:verify_markers——原厂 rootfs 跑本检查必须
# FAIL；与 flash-check.sh §4b 是同一套断言）。
# dropbear 三门控已删 / inittab 三行 login / shadow $6$+0600 / PAM /
# OTA 三处 / asound 两文件 / libxaudio（LX06），缺一即 FAIL。
PYTHONPATH="$HERE/scripts" python3 -c \
    'import sys
from pathlib import Path
from patch_firmware import PatchError, verify_markers
try:
    verify_markers(Path(sys.argv[1]), sys.argv[2])
except PatchError as exc:
    print(f"验证失败：{exc}", file=sys.stderr)
    sys.exit(1)' \
    "$VERIFY_DIR/rootfs" "$MODEL" \
    || fail "验证失败：补丁标记核对未通过（见上）"
# rc.local 走包装器行（P2-34；旧直行是旧版补丁产物，这里只接受包装器行）。
grep -q '^\[ -x /etc/open-xiaoai/init.sh \]' "$VERIFY_DIR/rootfs/etc/rc.local" \
    || fail "验证失败：rc.local 里没有 init 包装器启动行"
grep -q 'sha256sum' "$VERIFY_DIR/rootfs/etc/open-xiaoai/init.sh" \
    || fail "验证失败：init 包装器里没有 sha256sum 校验"
grep -q 'init.sh.sha256' "$VERIFY_DIR/rootfs/etc/open-xiaoai/init.sh" \
    || fail "验证失败：init 包装器里没有 SHA256 pin 检查"
[ "$(stat -c %a "$VERIFY_DIR/rootfs/etc/open-xiaoai/init.sh")" = "755" ] \
    || fail "验证失败：init 包装器权限不是 0755"
# .bak 必须是原厂原文——与原始 root.squashfs 里的同名文件逐字节比对。
# 不能只查 "slave.pcm Capture"：OH2P 原厂 asound.conf 里就有该行
# （mico_record/noop 出厂即走 Capture），标记法会误杀 OH2P（2026-11-18
# 重编 OH2P 时实测触发）。逐字节比对是“备份即原厂”的本来定义，与型号无关。
for rel in etc/asound.conf etc/asound.conf.dts; do
    unsquashfs -d "$VERIFY_DIR/orig" -f -no-exit-code "$ORIG_SQFS" "$rel" >/dev/null 2>&1 \
        || fail "验证失败：解不出原厂 $rel（无法比对 .bak）"
    cmp -s "$VERIFY_DIR/rootfs/$rel.bak" "$VERIFY_DIR/orig/$rel" \
        || fail "验证失败：$rel.bak 与原厂内容不一致（备份应是原厂原文）"
done
rm -rf "$VERIFY_DIR"
VERIFY_DIR=""

echo "打包完成：$OUT（$SIZE 字节）"
