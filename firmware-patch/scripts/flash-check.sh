#!/usr/bin/env bash
# 刷机前干跑检查（P0-6）：只做 VM 上能做的本地校验，不碰真机。
#
# 用法：flash-check.sh lx06 1.94.13
#   环境：PATCH_WORK_DIR 可覆盖 work 目录（测试用）；
#         OTA_BIN 可直接指定官方 .bin 路径。
#
# 检查（硬检查失败即 exit 1；警告不影响退出码）：
#   1. 官方 .bin 若找得到（OTA_BIN → work/<型号>_<版本>.bin →
#      ../../firmware/<型号>_<版本>.bin），按白名单验 SHA256/MD5/大小
#   2. 原始 root.squashfs 若在，按白名单验 SHA256/大小
#   3. mico_version 若在，须含型号与版本
#   4. 补丁镜像 root-patched.squashfs 必须存在、< 40MB、squashfs 魔数
#      hsq s、unsquashfs -s 可解析（无 unsquashfs 时只警告）
#   5. 打印补丁镜像 SHA256：与 FLASH_RUNBOOK.md 记录的 2026-11-17 实测值
#      一致则提示一致；不一致只警告（镜像可能已重编），请核对重编来源
# 设备侧检查（读 boot_part、/proc/mtd、刷后校验）无法在 VM 上跑，
# 统一列在最后的 BLOCKED 清单里，按 FLASH_RUNBOOK.md 在真机上执行。
set -uo pipefail

MODEL_RAW="${1:?用法: flash-check.sh lx06 1.94.13}"
VERSION="${2:?用法: flash-check.sh lx06 1.94.13}"
MODEL="$(printf '%s' "$MODEL_RAW" | tr 'A-Z' 'a-z')"
MODEL_UPPER="$(printf '%s' "$MODEL" | tr 'a-z' 'A-Z')"

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
MAX_SIZE=$((0x02800000))  # 40MB，与 patch.sh 同值

FAIL=0
ok()   { echo "  PASS $*"; }
bad()  { echo "  FAIL $*"; FAIL=1; }
warn() { echo "  WARN $*"; }

# 2026-11-17 实测的补丁镜像 SHA256（见 FLASH_RUNBOOK.md §1）。
# 仅作“与记录值一致”的提示；不一致时警告、不失败（镜像重编后值会变）。
case "$MODEL:$VERSION" in
    lx06:1.94.13) KNOWN_PATCHED_SHA256="7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3" ;;
    oh2p:1.58.6)  KNOWN_PATCHED_SHA256="139de363d39d0a5472bf9b00c015effd9a24c379ca5fa2436cc21c43e8706415" ;;
    *) KNOWN_PATCHED_SHA256="" ;;
esac

echo "== flash-check $MODEL $VERSION（work=$WORK）=="

# --- 1. 官方 .bin 白名单校验（找得到才验） ---
BIN=""
if [ -n "${OTA_BIN:-}" ] && [ -f "$OTA_BIN" ]; then
    BIN="$OTA_BIN"
elif [ -f "$DIR/${MODEL}_${VERSION}.bin" ]; then
    BIN="$DIR/${MODEL}_${VERSION}.bin"
elif [ -f "$WORK/${MODEL}_${VERSION}.bin" ]; then
    BIN="$WORK/${MODEL}_${VERSION}.bin"
elif [ -f "$HERE/../../firmware/${MODEL}_${VERSION}.bin" ]; then
    BIN="$HERE/../../firmware/${MODEL}_${VERSION}.bin"
fi
echo "-- 官方 .bin：${BIN:-未找到}"
if [ -n "$BIN" ]; then
    if python3 "$HERE/scripts/download-ota.py" verify-bin "$MODEL" "$VERSION" "$BIN"; then
        ok "官方 .bin 白名单 SHA256/MD5/大小"
    else
        bad "官方 .bin 未通过白名单校验：$BIN"
    fi
else
    warn "未找到官方 .bin（OTA_BIN / $DIR / $HERE/../../firmware 均无）；回滚材料无法核对，刷机前须先备好官方包"
fi

# --- 2. 原始 root.squashfs 白名单校验（在才验） ---
SQFS="$DIR/root.squashfs"
echo "-- 原始 root.squashfs：$SQFS"
if [ -f "$SQFS" ]; then
    if python3 "$HERE/scripts/download-ota.py" verify-sqfs "$MODEL" "$VERSION" "$SQFS"; then
        ok "原始 root.squashfs 白名单 SHA256/大小"
    else
        bad "原始 root.squashfs 未通过白名单校验：$SQFS"
    fi
else
    warn "未找到原始 root.squashfs；跳过该项（unpack.sh 产物缺失不影响刷机本身，但建议先跑通 unpack.sh）"
fi

# --- 3. mico_version 与型号/版本一致 ---
MICO="$DIR/mico_version"
echo "-- mico_version：$MICO"
if [ -f "$MICO" ]; then
    if grep -q "$VERSION" "$MICO" && grep -q "$MODEL_UPPER" "$MICO"; then
        ok "mico_version 含 $MODEL_UPPER $VERSION"
    else
        bad "mico_version 与 $MODEL_UPPER $VERSION 不符：$(head -c 200 "$MICO" | tr '\n' ' ')"
    fi
else
    warn "未找到 mico_version；跳过该项"
fi

# --- 4. 补丁镜像：存在、< 40MB、魔数、unsquashfs 可解析 ---
PATCHED="$DIR/root-patched.squashfs"
echo "-- 补丁镜像：$PATCHED"
if [ ! -f "$PATCHED" ]; then
    bad "找不到补丁镜像 $PATCHED，先跑 patch.sh $MODEL $VERSION"
else
    SIZE="$(stat -L -c %s "$PATCHED")"
    echo "  补丁镜像大小：$SIZE 字节（上限 $MAX_SIZE）"
    if [ "$SIZE" -gt 0 ] && [ "$SIZE" -lt "$MAX_SIZE" ]; then
        ok "补丁镜像大小在 (0, 40MB) 内"
    else
        bad "补丁镜像大小 $SIZE 不在 (0, 40MB) 内"
    fi
    MAGIC="$(head -c 4 "$PATCHED" | od -A n -t x1 | tr -d ' \n')"
    if [ "$MAGIC" = "68737173" ]; then
        ok "squashfs 魔数 hsq s"
    else
        bad "squashfs 魔数不对：$MAGIC（期望 68737173，即 hsq s）"
    fi
    if command -v unsquashfs >/dev/null 2>&1; then
        if unsquashfs -s "$PATCHED" >/dev/null 2>&1; then
            ok "unsquashfs -s 可解析（Squashfs 超块正常）"
        else
            bad "unsquashfs -s 解析失败：$PATCHED"
        fi
    else
        warn "无 unsquashfs，跳过超块解析（apt 装 squashfs-tools 后重跑）"
    fi
    PSHA="$(sha256sum "$PATCHED" | cut -d' ' -f1)"
    echo "  补丁镜像 SHA256：$PSHA"
    if [ -n "$KNOWN_PATCHED_SHA256" ]; then
        if [ "$PSHA" = "$KNOWN_PATCHED_SHA256" ]; then
            ok "补丁镜像 SHA256 与 2026-11-17 实测值一致"
        else
            warn "补丁镜像 SHA256 与记录值不同（镜像可能已重编）；请核对重编来源与 FLASH_RUNBOOK.md §1 的记录，再决定是否刷"
        fi
    fi
fi

echo
if [ "$FAIL" -eq 0 ]; then
    echo "干跑检查通过：VM 上能做的校验都过了。"
else
    echo "干跑检查失败：上面 FAIL 的项必须先修好再刷机。"
fi
echo "以下步骤无法在 VM 上验证，真机上按 FLASH_RUNBOOK.md 执行（BLOCKED）："
echo "  - 读当前 boot_part（fw_env -g boot_part，或刷机工具 printenv）"
echo "  - 备份非当前槽之外的原厂分区（kernel + rootfs）并校验 SHA256"
echo "  - 刷后校验：boot_part 已翻到目标槽、分区内容与补丁镜像一致、能开机进 SSH、Hub 能连上"
echo "  - 回滚演练：翻回原槽重启，确认原厂系统仍能启动"
exit "$FAIL"
