#!/usr/bin/env bash
# 刷机前干跑检查（P0-6）：只做 VM 上能做的本地校验，不碰真机。
#
# 用法：flash-check.sh [--strict] lx06 1.94.13
#   环境：PATCH_WORK_DIR 可覆盖 work 目录（测试用）；
#         OTA_BIN 可直接指定官方 .bin 路径。
#   --strict：补丁镜像 SHA256 与记录值不一致时 FAIL（默认只警告）；
#         刷机当天用 --strict 跑一遍，防止刷错重编的镜像。
#
# 检查（硬检查失败即 exit 1；警告不影响退出码）：
#   1. 官方 .bin 若找得到（OTA_BIN → work/<型号>_<版本>.bin →
#      ../../firmware/<型号>_<版本>.bin），按白名单验 SHA256/MD5/大小
#   2. 原始 root.squashfs 若在，按白名单验 SHA256/大小
#   3. mico_version 若在，须含型号与版本
#   4. 补丁镜像 root-patched.squashfs 必须存在、< 40MB、squashfs 魔数
#      hsq s、unsquashfs -s 可解析（无 unsquashfs 时只警告）
#   4b. 解开补丁镜像核对补丁标记（FW-P2-5）：走
#       patch_firmware.py:verify_markers（dropbear / inittab / shadow /
#       PAM / asound / OTA 三处 / libxaudio）+ .bak + init 包装器；
#       缺失即 FAIL（init 包装器本身缺失只警告，旧版镜像里没有）
#   5. 打印补丁镜像 SHA256：与 FLASH_RUNBOOK.md 记录的实测值
#      一致则提示一致；不一致默认只警告（镜像可能已重编），--strict 则失败
# 设备侧检查（读 boot_part、/proc/mtd、刷后校验）无法在 VM 上跑，
# 统一列在最后的 BLOCKED 清单里，按 FLASH_RUNBOOK.md 在真机上执行。
set -uo pipefail

STRICT=0
if [ "${1:-}" = "--strict" ]; then STRICT=1; shift; fi
MODEL_RAW="${1:?用法: flash-check.sh [--strict] lx06 1.94.13}"
VERSION="${2:?用法: flash-check.sh [--strict] lx06 1.94.13}"
MODEL="$(printf '%s' "$MODEL_RAW" | tr 'A-Z' 'a-z')"
MODEL_UPPER="$(printf '%s' "$MODEL" | tr 'a-z' 'A-Z')"

case "$MODEL" in
    lx06|oh2p) ;;
    *) echo "未知型号：$MODEL_RAW（只支持 lx06 / oh2p）" >&2; exit 1 ;;
esac
# P2-9：版本号必须严格是 x.y.z——case 的 `.` 是通配符，"1/94/13" 也能
# 通过，拼进路径就是穿越；用正则锚定首尾（与 patch.sh / unpack.sh 同款）。
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "版本号格式不对：$VERSION（应为 x.y.z，如 1.94.13）" >&2; exit 1
fi

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${PATCH_WORK_DIR:-$HERE/work}"
DIR="$WORK/${MODEL}_${VERSION}"
MAX_SIZE=$((0x02800000))  # 40MB，与 patch.sh 同值

FAIL=0
ok()   { echo "  PASS $*"; }
bad()  { echo "  FAIL $*"; FAIL=1; }
warn() { echo "  WARN $*"; }

# 2026-11-18 重编的补丁镜像 SHA256（见 FLASH_RUNBOOK.md §1）。
# 仅作“与记录值一致”的提示；不一致时警告、不失败（镜像重编后值会变）。
case "$MODEL:$VERSION" in
    lx06:1.94.13) KNOWN_PATCHED_SHA256="7dafb9507bdc4894715f2978ffaf5fd6a11af18ad61ed268f48c6309f0a05e21" ;;
    oh2p:1.58.6)  KNOWN_PATCHED_SHA256="fe2bab99eb15e423b689c24bace6fec1b665b1073dc12ec1c9fa2e4433b5c926" ;;
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
    # P2-30：38MB 预警线（上限 95%），与 patch.sh §5 同值。
    if [ "$SIZE" -ge $((38 * 1024 * 1024)) ]; then
        warn "补丁镜像 $SIZE 字节已超 38MB 预警线（上限 95%）"
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
            ok "补丁镜像 SHA256 与记录值一致"
        elif [ "$STRICT" -eq 1 ]; then
            bad "补丁镜像 SHA256 与记录值不同（--strict：拒绝继续，请核对重编来源）"
        else
            warn "补丁镜像 SHA256 与记录值不同（镜像可能已重编）；请核对重编来源与 FLASH_RUNBOOK.md §1 的记录，再决定是否刷"
        fi
    fi

    # --- 4b. 镜像内补丁内容验证（FW-P2-5）：解开核对补丁标记 ---
    # 标记核对走 patch_firmware.py:verify_markers（与 patch.sh §6 同一套
    # 断言，F-1），缺失即 FAIL；.bak 缺失也 FAIL（OH2P 的 asound.conf
    # 只能由 .bak 证明打过补丁）；init 包装器缺失只警告（旧版镜像里没有），
    # 但包装器在而 pin 不是必填即 FAIL（F-3）。
    VDIR="$(mktemp -d)"
    FAIL_BEFORE_4B="$FAIL"
    if unsquashfs -d "$VDIR/rootfs" -f -no-exit-code "$PATCHED" >/dev/null 2>&1; then
        RF="$VDIR/rootfs"
        # F-1：补丁标记核对走 patch_firmware.py:verify_markers（与 patch.sh §6
        # 同一套断言，原厂镜像必须 FAIL）；失败原因由 Python 打印到 stderr。
        MARKER_ERR="$(PYTHONPATH="$HERE/scripts" python3 -c \
            'import sys
from pathlib import Path
from patch_firmware import PatchError, verify_markers
try:
    verify_markers(Path(sys.argv[1]), sys.argv[2])
except PatchError as exc:
    print(f"{exc}", file=sys.stderr)
    sys.exit(1)' \
            "$RF" "$MODEL" 2>&1)" \
            || bad "镜像内补丁标记核对失败：$MARKER_ERR"
        # mTLS 客户端证书必须在镜像里（patch.sh 无证书目录会直接失败）。
        for f in etc/open-xiaoai/tls/ca.crt etc/open-xiaoai/tls/client.crt; do
            [ -f "$RF/$f" ] || bad "镜像内补丁缺失：缺 $f（mTLS 客户端证书）"
        done
        [ -f "$RF/etc/open-xiaoai/tls/client.key" ] \
            || bad "镜像内补丁缺失：缺 etc/open-xiaoai/tls/client.key（mTLS 客户端私钥）"
        [ "$(stat -c %a "$RF/etc/open-xiaoai/tls/client.key")" = "600" ] \
            || bad "镜像内补丁缺失：client.key 权限不是 0600"
        # rc.local：包装器行或旧直行均接受（旧版补丁产物）。
        if grep -q '^\[ -x /etc/open-xiaoai/init.sh \]' "$RF/etc/rc.local"; then
            ok "rc.local 走 init 包装器（P2-34）"
        elif grep -q 'sh /data/init.sh' "$RF/etc/rc.local"; then
            warn "rc.local 仍是旧直行 /data/init.sh（旧版补丁产物；下次重编会换成包装器）"
        else
            bad "镜像内补丁缺失：rc.local 里既无包装器行也无旧直行"
        fi
        # .bak 是 OH2P asound.conf 唯一的“打过补丁”证据（原厂即 Capture，
        # Capture 块参数与补丁后逐字节相同，见 F-1）：缺失即 FAIL；原始
        # root.squashfs 在时再逐字节比对原厂（与 patch.sh §6 同款）。
        for rel in etc/asound.conf.bak etc/asound.conf.dts.bak; do
            [ -f "$RF/$rel" ] || bad "镜像内补丁缺失：缺 $rel（asound 原厂备份）"
        done
        if [ -f "$SQFS" ]; then
            if unsquashfs -d "$VDIR/orig" -f -no-exit-code "$SQFS" \
                    etc/asound.conf etc/asound.conf.dts >/dev/null 2>&1; then
                for rel in etc/asound.conf etc/asound.conf.dts; do
                    cmp -s "$RF/$rel.bak" "$VDIR/orig/$rel" \
                        || bad "镜像内补丁缺失：$rel.bak 与原厂内容不一致（备份应是原厂原文）"
                done
            else
                warn "解不出原始 root.squashfs 里的 asound 文件，跳过 .bak 逐字节比对"
            fi
        else
            warn "未找到原始 root.squashfs，跳过 .bak 逐字节比对（只验 .bak 存在）"
        fi
        if [ -f "$RF/etc/open-xiaoai/init.sh" ]; then
            grep -q 'sha256sum' "$RF/etc/open-xiaoai/init.sh" \
                || bad "镜像内补丁缺失：init 包装器里没有 sha256sum 校验"
            # F-3：pin 必填——包装器里必须有“缺 pin 文件即拒绝”的分支。
            grep -q 'sha256 pin missing' "$RF/etc/open-xiaoai/init.sh" \
                || bad "镜像内补丁缺失：init 包装器里 pin 不是必填（缺 pin 文件时必须拒绝执行）"
        else
            warn "镜像里缺 etc/open-xiaoai/init.sh（旧版补丁产物；下次重编会带上）"
        fi
        [ "$FAIL" -eq "$FAIL_BEFORE_4B" ] && ok "镜像内补丁标记核对通过"
    else
        bad "解开补丁镜像失败，无法验证镜像内补丁内容"
    fi
    rm -rf "$VDIR"
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
