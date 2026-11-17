#!/usr/bin/env bash
# 解包 OTA：OTA bin → root.squashfs → rootfs 目录
# 工具：unblob（解固件）+ unsquashfs（解 squashfs）
# 已验证（2026-11-16）：LX06 1.94.13 解出 9 个文件，82.93MB；
# root.squashfs 是 Squashfs v4.0 xz，2137 inodes，解出 1348 个文件。
#
# 用法：unpack.sh lx06 1.94.13
#   输入：work/lx06_1.94.13.bin（先跑 download-ota.py，或放已有的官方包）
#   输出：work/lx06_1.94.13/rootfs、work/lx06_1.94.13/root.squashfs、
#         work/lx06_1.94.13/mico_version、work/lx06_1.94.13-extract/
#   环境：PATCH_WORK_DIR 可覆盖 work 目录（测试用）；
#         OTA_BIN 可直接指定 .bin 路径（测试用已有的官方包）；
#         REA_UNBLOB_COMMAND 指定 unblob（见 ../env.sh）
set -euo pipefail

MODEL_RAW="${1:?用法: unpack.sh lx06 1.94.13}"
VERSION="${2:?用法: unpack.sh lx06 1.94.13}"
MODEL="$(printf '%s' "$MODEL_RAW" | tr 'A-Z' 'a-z')"

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
BIN="${OTA_BIN:-$WORK/${MODEL}_${VERSION}.bin}"
OUT="$WORK/${MODEL}_${VERSION}"

[ -f "$BIN" ] || { echo "找不到 $BIN，先跑 download-ota.py" >&2; exit 1; }
mkdir -p "$WORK"

# 0. 白名单哈希校验（P0-5）：.bin 必须在白名单里且 SHA256/MD5/大小全对，
#    否则拒绝解包。校验逻辑以 scripts/download-ota.py 的 EXPECTED 为唯一源，
#    这里不另写一份哈希表（避免两处白名单漂移）。
python3 "$HERE/scripts/download-ota.py" verify-bin "$MODEL" "$VERSION" "$BIN" \
    || { echo "拒绝解包：$BIN 未通过白名单校验（见上）" >&2; exit 1; }

# 1. unblob 解固件（需要 REA_UNBLOB_COMMAND 或 PATH 里有 unblob）
#    注意：unblob 26.6.4 的解包目录参数是 -e/--extract-dir，没有 -x。
#    另注意：unblob 在缺提取器（sasquatch 解 squashfs 内容、7z 解 boot.img
#    里 gzip）时会以非零退出，但 root.squashfs 与 mico_version 仍会被
#    正确 carve 出来——那两个提取器本就不需要（squashfs 内容由系统
#    unsquashfs 解，boot.img 不参与打补丁）。所以这里不以 unblob 的
#    退出码为准，真正的门在下一步的产物校验。
UNBLOB="${REA_UNBLOB_COMMAND:-unblob}"
command -v "$UNBLOB" >/dev/null 2>&1 || [ -x "$UNBLOB" ] \
    || { echo "找不到 unblob：$UNBLOB（source ../env.sh 或装 unblob）" >&2; exit 1; }
"$UNBLOB" -e "$OUT-extract" --log "$OUT-unblob.log" "$BIN" || {
    echo "警告：unblob 退出码 $?（缺 sasquatch/7z 等提取器是已知的；继续按产物校验）" >&2
}

# 2. 校验：解包后必须存在 root.squashfs 与 mico_version，否则失败退出
SQFS="$(find "$OUT-extract" -name 'root.squashfs' | head -1)"
[ -n "$SQFS" ] || { echo "解包失败：没找到 root.squashfs" >&2; exit 1; }
MICOVER="$(find "$OUT-extract" -name 'mico_version' | head -1)"
[ -n "$MICOVER" ] || { echo "解包失败：没找到 mico_version" >&2; exit 1; }
mkdir -p "$OUT"
cp -f "$SQFS" "$OUT/root.squashfs"
cp -f "$MICOVER" "$OUT/mico_version"
echo "root.squashfs：$(stat -c %s "$OUT/root.squashfs") 字节"
echo "mico_version："
cat "$OUT/mico_version"

# 3. unsquashfs 解开 rootfs
#    注意：容器里无 CAP_MKNOD，建 /dev/* 设备节点会报
#    "Operation not permitted"（非致命错误；原厂 rootfs 的 dev/ 本就
#    是空的——启动时由 devtmpfs 接管）。加 -no-exit-code 让这类非致命
#    错误不影响退出码；真正的门是下面的产物校验。
command -v unsquashfs >/dev/null || { echo "缺 unsquashfs（apt 装 squashfs-tools）" >&2; exit 1; }
unsquashfs -d "$OUT/rootfs" -f -no-exit-code "$OUT/root.squashfs"
[ -d "$OUT/rootfs" ] || { echo "解包失败：$OUT/rootfs 不存在" >&2; exit 1; }
for f in etc/init.d/dropbear bin/ota bin/flash.sh; do
    [ -f "$OUT/rootfs/$f" ] || { echo "解包失败：rootfs 里缺 $f" >&2; exit 1; }
done
# libxaudio_engine.so 只有 LX06 有；OH2P 没有这个文件（音频引擎不同），
# patch_firmware.py 里 OH2P 也跳过 libxaudio 这一步，所以只对 lx06 要求。
if [ "$MODEL" = "lx06" ]; then
    [ -f "$OUT/rootfs/usr/lib/libxaudio_engine.so" ] || { echo "解包失败：rootfs 里缺 usr/lib/libxaudio_engine.so" >&2; exit 1; }
fi

echo "解包完成：$OUT/rootfs"
echo "关键文件核对（缺失只警告，不因此失败）："
for f in bin/ota bin/flash.sh bin/miso usr/bin/matool usr/lib/libdts.so; do
    if [ -e "$OUT/rootfs/$f" ]; then echo "  OK $f"; else echo "  缺 $f"; fi
done
