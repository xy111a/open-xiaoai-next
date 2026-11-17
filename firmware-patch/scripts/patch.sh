#!/usr/bin/env bash
# 打补丁 + 重新打包（Phase 1 实现）
# 目标：从解包后的 rootfs 出发，产出可刷的补丁固件 root-patched.squashfs。
#
# 已验证的补丁参数（证据见 ../../firmware/REA_ANALYSIS.md，别凭记忆改）：
#   - 分区上限 40MB（0x02800000）：补丁后 squashfs 必须小于 40MB，否则刷机变砖
#   - A/B 分区：system0 / system1；刷 boot0（补丁系统），boot1 保留原厂系统用于回滚
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
#         则退而校验原始 root.squashfs）；SSH_PASSWORD 改 root 密码
set -euo pipefail

MODEL_RAW="${1:?用法: patch.sh lx06 1.94.13}"
VERSION="${2:?用法: patch.sh lx06 1.94.13}"
MODEL="$(printf '%s' "$MODEL_RAW" | tr 'A-Z' 'a-z')"

# --- 入参校验 ---
case "$MODEL" in
    lx06|oh2p) ;;
    *) echo "未知型号：$MODEL_RAW（只支持 lx06 / oh2p）" >&2; exit 1 ;;
esac
case "$VERSION" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) echo "版本号格式不对：$VERSION（应为 x.y.z，如 1.94.13）" >&2; exit 1 ;;
esac

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${PATCH_WORK_DIR:-$HERE/work}"
DIR="$WORK/${MODEL}_${VERSION}"
SRC_ROOTFS="$DIR/rootfs"
SQFS_ROOT="$DIR/squashfs-root"
OUT="$DIR/root-patched.squashfs"
MAX_SIZE=$((0x02800000))
VERIFY_DIR=""

# 失败出口：报错 + 删掉半成品镜像与验证目录，再非零退出。
fail() {
    echo "失败：$*" >&2
    rm -f "$OUT"
    [ -n "$VERIFY_DIR" ] && rm -rf "$VERIFY_DIR"
    exit 1
}
# 未经 fail() 的意外失败（set -e 触发）同样清理半成品镜像。
trap 'rm -f "$OUT"; [ -n "$VERIFY_DIR" ] && rm -rf "$VERIFY_DIR"' ERR

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
    || fail "patch_firmware.py 失败"

# --- 3. 用 file 读原始 root.squashfs 的压缩方式与 blocksize ---
SQFS_INFO="$(file "$ORIG_SQFS")"
echo "原始固件信息：$SQFS_INFO"
COMPRESSION="$(printf '%s' "$SQFS_INFO" | grep -oE '(xz|gzip|lzo|lz4|zstd) compressed' | cut -d' ' -f1 | head -1)"
BLOCKSIZE="$(printf '%s' "$SQFS_INFO" | grep -oE 'blocksize: [0-9]+' | grep -oE '[0-9]+' | head -1)"
[ -n "$COMPRESSION" ] || fail "读不到压缩方式：$SQFS_INFO"
[ -n "$BLOCKSIZE" ] || fail "读不到 blocksize：$SQFS_INFO"
echo "打包参数：-comp $COMPRESSION -b $BLOCKSIZE"

# --- 4. mksquashfs 重新打包（与原始参数一致） ---
command -v mksquashfs >/dev/null || fail "缺 mksquashfs（apt 装 squashfs-tools）"
echo "重新打包 → $OUT"
mksquashfs "$SQFS_ROOT" "$OUT" \
    -comp "$COMPRESSION" -b "$BLOCKSIZE" \
    -noappend -all-root -always-use-fragments -no-xattrs -no-exports \
    || fail "mksquashfs 打包失败"

# --- 5. 大小校验：必须 < 40MB，否则失败且不输出镜像 ---
SIZE="$(stat -L -c %s "$OUT")"
echo "补丁后大小：$SIZE 字节（上限 $MAX_SIZE）"
if [ "$SIZE" -ge "$MAX_SIZE" ]; then
    fail "补丁后 squashfs $SIZE 字节，超过 40MB 上限，拒绝输出镜像"
fi
echo "大小检查通过，剩余 $((MAX_SIZE - SIZE)) 字节"

# --- 6. 解开验证：关键文件存在且补丁都在 ---
command -v unsquashfs >/dev/null || fail "缺 unsquashfs（apt 装 squashfs-tools）"
VERIFY_DIR="$(mktemp -d)"
# -no-exit-code：容器里建 /dev/* 设备节点会报 Operation not permitted
# （非致命；原厂 rootfs 的 dev/ 本就是空的）。真正的门是下面的文件校验。
unsquashfs -d "$VERIFY_DIR/rootfs" -f -no-exit-code "$OUT" >/dev/null \
    || fail "unsquashfs 解开验证失败"
for f in etc/init.d/dropbear etc/shadow etc/inittab etc/pam.d/common-auth \
         bin/ota bin/flash.sh etc/init.d/wireless etc/rc.local \
         etc/asound.conf etc/asound.conf.dts; do
    [ -f "$VERIFY_DIR/rootfs/$f" ] || fail "验证失败：解开后缺 $f"
done
# libxaudio_engine.so 只有 LX06 有；OH2P 没有这个文件（见 unpack.sh）。
if [ "$MODEL" = "lx06" ]; then
    [ -f "$VERIFY_DIR/rootfs/usr/lib/libxaudio_engine.so" ] || fail "验证失败：解开后缺 usr/lib/libxaudio_engine.so"
fi
# 补丁标记抽查
grep -q 'mkdir -p /data/etc/dropbear' "$VERIFY_DIR/rootfs/etc/init.d/dropbear" \
    || fail "验证失败：dropbear 未打补丁"
grep -q 'ttyS0::askfirst:/bin/login' "$VERIFY_DIR/rootfs/etc/inittab" \
    || fail "验证失败：inittab 串口不是 /bin/login（P1-36：串口必须走密码登录）"
grep -q 'slave.pcm Capture' "$VERIFY_DIR/rootfs/etc/asound.conf" \
    || fail "验证失败：asound.conf 未打补丁"
if [ "$MODEL" = "lx06" ]; then
    if grep -q 'hw:0,3' "$VERIFY_DIR/rootfs/usr/lib/libxaudio_engine.so" 2>/dev/null; then
        fail "验证失败：libxaudio_engine.so 里仍有 hw:0,3"
    fi
fi
rm -rf "$VERIFY_DIR"
VERIFY_DIR=""

echo "打包完成：$OUT（$SIZE 字节）"
