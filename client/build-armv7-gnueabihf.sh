#!/bin/bash
# ARMv7 gnueabihf 交叉编译（open-xiaoai-client → LX06/OH2P）——默认交付脚本。
# P1-40 方案 B（2026-11-17 决策）：客户端从 armv7-unknown-linux-musleabihf
# （musl 静态链接，不能 dlopen）切换到 armv7-unknown-linux-gnueabihf
# （glibc 2.25，zig 作链接器），使 sherpa-onnx 后端能在音箱上 dlopen。
# 旧脚本 ./build-armv7.sh（musl）仅作 fallback/应急保留，不再是默认交付。
#
# 用法：./build-armv7-gnueabihf.sh [--no-sherpa-kws] [其余参数原样透传给 cargo build]
# 默认开 --features sherpa-kws（P1-40：ARM 交付二进制必须含 KWS）；
# --no-sherpa-kws 仅供应急构建。构建末尾断言：ARM ELF（EABI5、硬浮点）、
# sherpa 标记 3 个、Version needs 里 GLIBC 最高 ≤2.25（超了即失败退出）、
# NEEDED 的每个库都能在音箱 rootfs 里找到、rpath 含 $ORIGIN/lib。
# 产物：target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
source ~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/env.sh
# /tmp 是 512MB tmpfs 且常满，cc 的临时文件改走磁盘目录。
export TMPDIR="${TMPDIR:-$HOME/workspace/.cargo-tmp}"
mkdir -p "$TMPDIR"
ZIG="$HOME/workspace/tools/zig-x86_64-linux-0.15.2/zig"
# zig cc 做 ARM C 编译器 + 链接器（包装器过滤 cc-rs 传的 --target=*，
# 强制 -target arm-linux-gnueabihf.2.25，见 ~/workspace/tools/zig-cc-arm-gnueabihf.sh）。
export CC_armv7_unknown_linux_gnueabihf="$HOME/workspace/tools/zig-cc-arm-gnueabihf.sh"
export AR_armv7_unknown_linux_gnueabihf="$ZIG ar"
export CARGO_TARGET_ARMV7_UNKNOWN_LINUX_GNUEABIHF_LINKER="$HOME/workspace/tools/zig-cc-arm-gnueabihf.sh"
# 关键：关掉 rustc 自带 CRT，否则与 zig 自带的 glibc crt1.o 重复定义 _start。
# rpath 写 $ORIGIN/lib：部署时 /data/open-xiaoai/lib/ 下的 .so
# （libsherpa-onnx-c-api.so、libonnxruntime.so）能被动态链接器找到。
# -L native 指链接期 libasound staging 目录（见下）。
LINK_DIR="$PWD/.link-gnueabihf/lib"
export RUSTFLAGS="-C link-self-contained=no -L native=$LINK_DIR -C link-arg=-Wl,-rpath,\$ORIGIN/lib"

# --- ALSA：链接音箱自带的 libasound.so.2（动态链接，不静态编）---
# 音箱 rootfs（只读参考）里有 /usr/lib/libasound.so.2（libasound.so.2.0.0），
# 但没有 ALSA 头文件；头文件用 ~/workspace/tools/libasound-arm 构建树里的
# （alsa-lib 1.2.14，架构无关），库用 rootfs 里抽出的 .so 链接期占位。
# 本脚本每次运行都重建 staging 目录（.link-gnueabihf/，已进 .gitignore，
# 不进仓库），缺 rootfs 或缺头文件时直接失败退出，不静默退回无 ALSA 构建。
ROOTFS="$HOME/workspace/speaker-open-xiaoai-rebuild/firmware/rootfs"
ALSA_INCLUDE="$HOME/workspace/tools/libasound-arm/install/include"
ALSA_PC_DIR="$PWD/.link-gnueabihf/pkgconfig"
if [ ! -f "$ROOTFS/usr/lib/libasound.so.2.0.0" ]; then
  echo "ASSERT FAIL: speaker libasound not found at $ROOTFS/usr/lib/libasound.so.2.0.0" >&2
  exit 1
fi
if [ ! -f "$ALSA_INCLUDE/asoundlib.h" ]; then
  echo "ASSERT FAIL: ALSA headers not found at $ALSA_INCLUDE/asoundlib.h" >&2
  exit 1
fi
mkdir -p "$LINK_DIR" "$ALSA_PC_DIR"
cp -f "$ROOTFS/usr/lib/libasound.so.2.0.0" "$LINK_DIR/libasound.so.2.0.0"
ln -sf libasound.so.2.0.0 "$LINK_DIR/libasound.so.2"
ln -sf libasound.so.2 "$LINK_DIR/libasound.so"
cat > "$ALSA_PC_DIR/alsa.pc" <<EOF
prefix=$HOME/workspace/tools/libasound-arm/install
exec_prefix=\${prefix}
libdir=$LINK_DIR
includedir=\${prefix}/include
Name: alsa
Description: Advanced Linux Sound Architecture (link-time stub: speaker rootfs libasound.so.2)
Version: 1.2.14
Libs: -L\${libdir} -lasound
Cflags: -I\${includedir}
EOF
# alsa-sys 经 pkg-config 找 libasound：只认上面这个 .pc（不认宿主机 glibc 版）。
export PKG_CONFIG_ALLOW_CROSS=1
export PKG_CONFIG_PATH_armv7_unknown_linux_gnueabihf="$ALSA_PC_DIR"
export PKG_CONFIG_PATH="$ALSA_PC_DIR:${PKG_CONFIG_PATH:-}"
unset ALSA_STATIC || true

# P1-40：默认开 sherpa-kws feature（ARM 交付二进制必须含 KWS，
# 否则刷进音箱的 Client 永远不会唤醒）。显式传 --no-sherpa-kws 可关掉
# （仅供无唤醒场景的应急构建；该参数不传给 cargo）。
SHERPA_FEATURE_ARGS=(--features sherpa-kws)
CARGO_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --no-sherpa-kws) SHERPA_FEATURE_ARGS=() ;;
    *) CARGO_ARGS+=("$arg") ;;
  esac
done
cargo build --release --target armv7-unknown-linux-gnueabihf "${SHERPA_FEATURE_ARGS[@]}" "${CARGO_ARGS[@]}"
BIN="target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client"
ls -la "$BIN"
file "$BIN"

# --- P1-40 方案 B 产物断言（构建脚本末尾，失败即非零退出）---
# 注意：本脚本开 set -o pipefail，`strings "$BIN" | grep -q ...` 这类写法
# 会误判——grep -q 命中后立即关管道，strings 因 SIGPIPE 以 141 退出，
# pipefail 下整个管道判失败。所以一律先落盘再 grep 文件。
STRINGS_TXT="$TMPDIR/open-xiaoai-client-gnueabihf.strings.txt"
strings "$BIN" > "$STRINGS_TXT"
# 1) 必须是 ARM ELF（EABI5、硬浮点、动态链接、解释器为音箱的 ld-linux-armhf）。
FILE_OUT="$(file "$BIN")"
case "$FILE_OUT" in
  *ARM*EABI5*|*ARM,*EABI5*) ;;
  *) echo "ASSERT FAIL: $BIN is not an ARM EABI5 binary: $FILE_OUT" >&2; exit 1 ;;
esac
case "$FILE_OUT" in
  *"dynamically linked"*) ;;
  *) echo "ASSERT FAIL: $BIN is not dynamically linked (gnueabihf must be dynamic so dlopen works): $FILE_OUT" >&2; exit 1 ;;
esac
case "$FILE_OUT" in
  *ld-linux-armhf.so.3*) ;;
  *) echo "ASSERT FAIL: $BIN interpreter is not /lib/ld-linux-armhf.so.3: $FILE_OUT" >&2; exit 1 ;;
esac
READELF_H_TXT="$TMPDIR/open-xiaoai-client-gnueabihf.readelf-h.txt"
readelf -h "$BIN" > "$READELF_H_TXT" 2>/dev/null || true
if ! grep -q "hard-float ABI" "$READELF_H_TXT"; then
  echo "ASSERT FAIL: $BIN is not hard-float (ELF header Flags lack hard-float ABI):" >&2
  grep -i "flags" "$READELF_H_TXT" >&2
  exit 1
fi
echo "ASSERT OK: ARM EABI5 hard-float dynamic binary, interpreter /lib/ld-linux-armhf.so.3"
# 2) sherpa-kws feature 确实编进去了：src/sherpa.rs 的 SHERPA_KWS_MARKER
#    （#[used] 静态标记，LTO/strip 后仍保留）与 C API 符号名
#    （libloading 动态加载用的字符串字面量）必须出现在产物里。
#    只在开了 feature 时断言（--no-sherpa-kws 构建跳过本条）。
if [ "${#SHERPA_FEATURE_ARGS[@]}" -gt 0 ]; then
  for marker in "open-xiaoai-kws-backend=sherpa-onnx-2.0.1" "SherpaOnnxCreateKeywordSpotter" "SherpaOnnxIsKeywordStreamReady"; do
    if ! grep -qF "$marker" "$STRINGS_TXT"; then
      echo "ASSERT FAIL: sherpa-kws feature marker missing from $BIN: $marker" >&2
      echo "  (build ran without --features sherpa-kws? check SHERPA_FEATURE_ARGS)" >&2
      exit 1
    fi
  done
  echo "ASSERT OK: ARM binary contains sherpa-kws markers (feature enabled)"
fi
# 3) Version needs：GLIBC 最高 ≤2.25、GLIBCXX 最高 ≤3.4.22（音箱 rootfs 上限）。
#    超了即失败退出，不许将就——否则 dlopen/启动时报 version not found。
READELF_V_TXT="$TMPDIR/open-xiaoai-client-gnueabihf.readelf-V.txt"
readelf -V "$BIN" > "$READELF_V_TXT" 2>/dev/null || true
check_max_version() {
  local prefix="$1" limit="$2" found=""
  found="$(grep -o "${prefix}_[0-9][0-9.]*" "$READELF_V_TXT" | sed "s/^${prefix}_//" | sort -V | tail -1 || true)"
  if [ -n "$found" ]; then
    if [ "$(printf '%s\n%s\n' "$found" "$limit" | sort -V | tail -1)" != "$limit" ]; then
      echo "ASSERT FAIL: $BIN requires ${prefix}_${found}, speaker max is ${prefix}_${limit}" >&2
      exit 1
    fi
    echo "ASSERT OK: max $prefix version required is ${prefix}_${found} (speaker max ${prefix}_${limit})"
  else
    echo "ASSERT OK: $BIN requires no $prefix versioned symbols"
  fi
}
check_max_version GLIBC 2.25
check_max_version GLIBCXX 3.4.22
# 4) rpath：RUNPATH/RPATH 必须含 $ORIGIN/lib（部署包 lib/ 下的 .so 靠它找到）。
READELF_D_TXT="$TMPDIR/open-xiaoai-client-gnueabihf.readelf-d.txt"
readelf -d "$BIN" > "$READELF_D_TXT" 2>/dev/null || true
if ! grep -q '\$ORIGIN/lib' "$READELF_D_TXT"; then
  echo "ASSERT FAIL: $BIN has no \$ORIGIN/lib in RPATH/RUNPATH:" >&2
  grep -E "RPATH|RUNPATH" "$READELF_D_TXT" >&2 || echo "  (no RPATH/RUNPATH entry at all)" >&2
  exit 1
fi
echo "ASSERT OK: RPATH/RUNPATH contains \$ORIGIN/lib"
# 5) NEEDED：每个库都必须在音箱 rootfs（/lib 或 /usr/lib）里找得到，
#    否则启动即报 shared library not found。缺失的进部署包或上报，不许将就。
ROOTFS_LIB_OK=1
while IFS= read -r needed; do
  [ -n "$needed" ] || continue
  if [ -e "$ROOTFS/lib/$needed" ] || [ -e "$ROOTFS/usr/lib/$needed" ]; then
    echo "ASSERT OK: NEEDED $needed found in speaker rootfs"
  else
    echo "ASSERT FAIL: NEEDED $needed NOT found in speaker rootfs ($ROOTFS/lib, $ROOTFS/usr/lib)" >&2
    ROOTFS_LIB_OK=0
  fi
done <<EOF
$(grep NEEDED "$READELF_D_TXT" | sed 's/.*\[\(.*\)\]/\1/')
EOF
if [ "$ROOTFS_LIB_OK" -ne 1 ]; then
  echo "ASSERT FAIL: one or more NEEDED libraries are missing from the speaker rootfs — add to deploy lib/ or report" >&2
  exit 1
fi
# 6) vendor ARM .so 若已取到（scripts/download-kws-lib.sh），核对它导出
#    Rust FFI 要用的 10 个符号；没取到只警告（部署时才需要 .so，
#    构建本身不链接它），不让构建炸掉。
VENDOR_LIB="kws/vendor-arm/libsherpa-onnx-c-api.so"
if [ -f "$VENDOR_LIB" ]; then
  NM_TXT="$TMPDIR/libsherpa-onnx-c-api.nm.txt"
  nm -D "$VENDOR_LIB" > "$NM_TXT" 2>/dev/null
  for sym in SherpaOnnxCreateKeywordSpotter SherpaOnnxDestroyKeywordSpotter SherpaOnnxCreateKeywordStream SherpaOnnxDestroyOnlineStream SherpaOnnxOnlineStreamAcceptWaveform SherpaOnnxIsKeywordStreamReady SherpaOnnxDecodeKeywordStream SherpaOnnxResetKeywordStream SherpaOnnxGetKeywordResult SherpaOnnxDestroyKeywordResult; do
    if ! grep -q " T $sym$" "$NM_TXT"; then
      echo "ASSERT FAIL: $VENDOR_LIB missing export: $sym" >&2
      exit 1
    fi
  done
  echo "ASSERT OK: $VENDOR_LIB exports all 10 required KWS symbols"
  # 7) 兼容性提醒（只警告，不致命）：官方 .so 要求 GLIBC 最高 2.34 /
  #    GLIBCXX_3.4.29，音箱只有 2.25 / 3.4.22——该 .so 仍不能直接用，
  #    待 P1-40 的 .so 从源码重编落定（见 kws/vendor-arm/SOURCE.md）。
  #    模式覆盖 GLIBC_2.29 及以上——实测 .so 最高 2.34，只匹配 2.29 会漏报。
  READELF_TXT="$TMPDIR/libsherpa-onnx-c-api.readelf.txt"
  readelf -V "$VENDOR_LIB" > "$READELF_TXT" 2>/dev/null || true
  if grep -Eq "GLIBC_2\.(29|[3-9][0-9])" "$READELF_TXT"; then
    MAX_GLIBC=$(grep -o "GLIBC_2\.[0-9.]*" "$READELF_TXT" | sort -Vu | tail -n 1)
    echo "WARNING: $VENDOR_LIB requires ${MAX_GLIBC:-GLIBC_2.29+}, speaker has glibc 2.25 — will NOT load on-device as-is (see kws/vendor-arm/SOURCE.md)"
  fi
  if grep -q "GLIBCXX_3.4.29" "$READELF_TXT"; then
    echo "WARNING: $VENDOR_LIB requires GLIBCXX_3.4.29, speaker has libstdc++ 3.4.22 — will NOT load on-device as-is (see kws/vendor-arm/SOURCE.md)"
  fi
else
  echo "WARNING: ARM libsherpa not found at $VENDOR_LIB — run ./scripts/download-kws-lib.sh first (build itself does not link it; on-device KWS still BLOCKED, see kws/vendor-arm/SOURCE.md)"
fi
# 8) 可选：宿主机模型加载冒烟（默认不开）。设 OPEN_XIAOAI_CHECK_KWS_MODEL=1
#    且 SHERPA_KWS_TEST_MODEL_DIR / SHERPA_KWS_TEST_LIB /
#    SHERPA_KWS_TEST_SAMPLES / SHERPA_KWS_TEST_KEYWORDS 四个环境变量齐全时，
#    跑 x86_64 的 sherpa 端到端测试（真实模型 + 真实 .so + 真实 TTS 样本），
#    证明模型本身能加载、能检出。注意这测的是 x86 宿主机，不是 ARM 真机。
if [ "${OPEN_XIAOAI_CHECK_KWS_MODEL:-0}" = "1" ]; then
  if [ -n "${SHERPA_KWS_TEST_MODEL_DIR:-}" ] && [ -n "${SHERPA_KWS_TEST_LIB:-}" ] && [ -n "${SHERPA_KWS_TEST_SAMPLES:-}" ] && [ -n "${SHERPA_KWS_TEST_KEYWORDS:-}" ]; then
    echo "==> host-side KWS model smoke test (x86_64, not the ARM device)"
    cargo test --features sherpa-kws sherpa_detects_nihaoxiaoli_tts_sample
  else
    echo "WARNING: OPEN_XIAOAI_CHECK_KWS_MODEL=1 but SHERPA_KWS_TEST_* env vars are incomplete — skipping host model smoke test"
  fi
fi
