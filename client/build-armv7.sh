#!/bin/bash
# ARMv7 musl 交叉编译（open-xiaoai-client → LX06/OH2P）——musl 应急版（fallback）。
# 注意：自 P1-40 方案 B（2026-11-17）起，默认交付脚本是 ./build-armv7-gnueabihf.sh
# （glibc 2.25 动态链接，支持 dlopen）；本脚本（musl 静态链接）只作应急保留——
# 静态 musl 程序不能 dlopen，刷进去的 Client 永远唤醒不了（见 kws/vendor-arm/SOURCE.md）。
# 只有 gnueabihf 路线不可用时才用本脚本，且用前须知：KWS 在真机上仍是 BLOCKED。
# 用法：./build-armv7.sh [--no-sherpa-kws] [--allow-no-alsa] [其余参数原样透传给 cargo build]
# 默认开 --features sherpa-kws（P1-40：ARM 交付二进制必须含 KWS）；
# --no-sherpa-kws 仅供无唤醒场景的应急构建。构建末尾断言产物含 sherpa
# 标记、ALSA 标记、vendor ARM .so 含 10 个必需导出符号（见脚本末尾）。
# P2-52：musl libasound 缺失时默认失败退出（exit 1），不再静默产出
# “无 ALSA、无 KWS”应急版；只有显式传 --allow-no-alsa 时才降级编
# 无 ALSA 版（该参数不传给 cargo；应急版产物无 ALSA 标记，断言跳过）。
# 产物：target/armv7-unknown-linux-musleabihf/release/open-xiaoai-client
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
source ~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/env.sh
# /tmp 是 512MB tmpfs 且常满，cc 的临时文件改走磁盘目录。
export TMPDIR="${TMPDIR:-$HOME/workspace/.cargo-tmp}"
mkdir -p "$TMPDIR"
ZIG="$HOME/workspace/tools/zig-x86_64-linux-0.15.2/zig"
# zig cc 做 ARM C 编译器 + 链接器（见 ~/workspace/tools/zig-cc-arm.sh 注释：
# cc-rs 传的 --target=arm-unknown-linux-musleabihf 会被 zig 拒绝，包装器负责过滤）。
export CC_armv7_unknown_linux_musleabihf="$HOME/workspace/tools/zig-cc-arm.sh"
export AR_armv7_unknown_linux_musleabihf="$ZIG ar"
export CARGO_TARGET_ARMV7_UNKNOWN_LINUX_MUSLEABIHF_LINKER="$HOME/workspace/tools/zig-cc-arm.sh"
# 关键：关掉 rustc 自带 CRT，否则与 zig 自带的 musl crt1.o 重复定义 _start。
export RUSTFLAGS="-C link-self-contained=no"
# P2-N7（可复现构建）：--remap-path-prefix 把构建机绝对路径（$HOME、$PWD）
# 从 panic 位置等嵌入信息里抹掉，不同目录构建出的二进制才可能字节相同；
# SOURCE_DATE_EPOCH 取当前提交时间（同一提交恒定），供构建脚本/工具链
# 使用（rustc 本身不直接读它，但它是可复现构建的约定俗成项）。
# 工具链版本另由 client/rust-toolchain.toml 钉死（rustc 1.100.0）。
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
export RUSTFLAGS="$RUSTFLAGS --remap-path-prefix $HOME=/home/build --remap-path-prefix $PWD=/client-src"
# ALSA（任务 E）：alsa-sys 经 pkg-config 找 libasound；ARMv7 musl 交叉不能用
# 宿主机的 glibc libasound，改用本机自编的 musl 版静态库
# （~/workspace/tools/libasound-arm：alsa-lib 1.2.14，zig cc -target
# arm-linux-musleabihf，--disable-shared --enable-static，见 client/README.md）。
# ALSA_STATIC=1 让 pkg-config 走 --static，链 libasound.a 进二进制
# （音箱 OpenWrt 精简系统不保证有 libasound.so.2）。
# 若该目录缺失（重装/换机），默认失败退出（P2-52）：无 ALSA 的 Client
# 不能采集、不能播放，静默产出应急版只会让刷机后才发现音箱是聋哑的。
# 只有显式传 --allow-no-alsa 时才降级为 --no-default-features（能跑传输、
# KWS 只报 RMS，采集/播放报明确错误），且该构建的产物断言会跳过 ALSA 标记。
ALSA_ARM_PREFIX="$HOME/workspace/tools/libasound-arm/install"
ALSA_FEATURE_ARGS=()
ALLOW_NO_ALSA=0
for arg in "$@"; do
  case "$arg" in
    --allow-no-alsa) ALLOW_NO_ALSA=1 ;;
  esac
done
if [ -f "$ALSA_ARM_PREFIX/lib/pkgconfig/alsa.pc" ]; then
  export PKG_CONFIG_ALLOW_CROSS=1
  export PKG_CONFIG_PATH_armv7_unknown_linux_musleabihf="$ALSA_ARM_PREFIX/lib/pkgconfig"
  export ALSA_STATIC=1
elif [ "$ALLOW_NO_ALSA" = "1" ]; then
  echo "WARNING: musl libasound not found at $ALSA_ARM_PREFIX — building WITHOUT the alsa feature (--allow-no-alsa given explicitly; no on-device capture/playback)"
  ALSA_FEATURE_ARGS=(--no-default-features)
else
  echo "ASSERT FAIL: musl libasound not found at $ALSA_ARM_PREFIX — refusing to silently build a no-ALSA client (P2-52)" >&2
  echo "  (rebuild the musl libasound staging dir, or pass --allow-no-alsa explicitly if you really want the no-ALSA emergency build)" >&2
  exit 1
fi
# P1-40：默认开 sherpa-kws feature（ARM 交付二进制必须含 KWS，
# 否则刷进音箱的 Client 永远不会唤醒）。显式传 --no-sherpa-kws 可关掉
# （仅供无唤醒场景的应急构建；该参数不传给 cargo）。
SHERPA_FEATURE_ARGS=(--features sherpa-kws)
CARGO_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --no-sherpa-kws) SHERPA_FEATURE_ARGS=() ;;
    --allow-no-alsa) ;; # 已在上面处理，不传给 cargo
    *) CARGO_ARGS+=("$arg") ;;
  esac
done
cargo build --release --target armv7-unknown-linux-musleabihf "${ALSA_FEATURE_ARGS[@]}" "${SHERPA_FEATURE_ARGS[@]}" "${CARGO_ARGS[@]}"
BIN="target/armv7-unknown-linux-musleabihf/release/open-xiaoai-client"
ls -la "$BIN"
file "$BIN"

# --- P1-40 产物断言（构建脚本末尾，失败即非零退出）---
# 注意：本脚本开 set -o pipefail，`strings "$BIN" | grep -q ...` 这类写法
# 会误判——grep -q 命中后立即关管道，strings 因 SIGPIPE 以 141 退出，
# pipefail 下整个管道判失败（2026-11-17 在 download-kws-lib.sh 实测过
# 同款非确定性误报）。所以一律先落盘再 grep 文件。
STRINGS_TXT="$TMPDIR/open-xiaoai-client.strings.txt"
strings "$BIN" > "$STRINGS_TXT"
# 1) 必须是 ARM ELF（file 输出里有 ARM；用变量接，避免管道）。
FILE_OUT="$(file "$BIN")"
case "$FILE_OUT" in
  *ARM*) ;;
  *) echo "ASSERT FAIL: $BIN is not an ARM binary: $FILE_OUT" >&2; exit 1 ;;
esac
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
# 2b) ALSA 标记（P2-52）：src/audio.rs 的 ALSA_BACKEND_MARKER
#    （#[used] 静态标记）必须出现在产物里——否则构建悄悄编出了
#    无 ALSA 版。显式 --allow-no-alsa 构建跳过本条（应急版无此标记
#    是预期的，但构建日志里已有 WARNING 说明）。
if [ "${#ALSA_FEATURE_ARGS[@]}" -eq 0 ]; then
  if ! grep -qF "open-xiaoai-audio-backend=alsa" "$STRINGS_TXT"; then
    echo "ASSERT FAIL: ALSA backend marker missing from $BIN (built without the alsa feature?)" >&2
    exit 1
  fi
  echo "ASSERT OK: ARM binary contains ALSA backend marker (alsa feature enabled)"
fi
# 3) vendor ARM .so 若已取到（scripts/download-kws-lib.sh），核对它导出
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
  # 4) 兼容性：官方 .so 要求 GLIBC 最高 2.34 / GLIBCXX_3.4.29，音箱只有
  #    2.25 / 3.4.22（见 kws/vendor-arm/SOURCE.md）。
  #    模式覆盖 GLIBC_2.29 及以上——实测 .so 最高 2.34，只匹配 2.29 会漏报。
  #    2026-11-18 bai 定（gnueabihf，同 gnueabihf 脚本第 7 节）：全局仍只
  #    警告；但本次构建开了 sherpa-kws feature 时不兼容即硬失败。
  #    服务端模式（--no-sherpa-kws）不加载它，只警告。
  READELF_TXT="$TMPDIR/libsherpa-onnx-c-api.readelf.txt"
  readelf -V "$VENDOR_LIB" > "$READELF_TXT" 2>/dev/null || true
  VENDOR_INCOMPAT_REASONS=""
  if grep -Eq "GLIBC_2\.(29|[3-9][0-9])" "$READELF_TXT"; then
    MAX_GLIBC=$(grep -o "GLIBC_2\.[0-9.]*" "$READELF_TXT" | sort -Vu | tail -n 1)
    VENDOR_INCOMPAT_REASONS="requires ${MAX_GLIBC:-GLIBC_2.29+}, speaker has glibc 2.25"
  fi
  if grep -q "GLIBCXX_3.4.29" "$READELF_TXT"; then
    if [ -n "$VENDOR_INCOMPAT_REASONS" ]; then
      VENDOR_INCOMPAT_REASONS="$VENDOR_INCOMPAT_REASONS; requires GLIBCXX_3.4.29, speaker has libstdc++ 3.4.22"
    else
      VENDOR_INCOMPAT_REASONS="requires GLIBCXX_3.4.29, speaker has libstdc++ 3.4.22"
    fi
  fi
  if [ -n "$VENDOR_INCOMPAT_REASONS" ]; then
    if [ "${#SHERPA_FEATURE_ARGS[@]}" -gt 0 ]; then
      echo "ASSERT FAIL: $VENDOR_LIB $VENDOR_INCOMPAT_REASONS — will NOT load on-device as-is (see kws/vendor-arm/SOURCE.md)" >&2
      echo "  (built with --features sherpa-kws, so this .so would be dlopen'd on-device and fail; rebuild the .so from source (path A) or build with --no-sherpa-kws for server mode)" >&2
      exit 1
    fi
    echo "WARNING: $VENDOR_LIB $VENDOR_INCOMPAT_REASONS — will NOT load on-device as-is (see kws/vendor-arm/SOURCE.md)"
  fi
else
  echo "WARNING: ARM libsherpa not found at $VENDOR_LIB — run ./scripts/download-kws-lib.sh first (build itself does not link it; on-device KWS still BLOCKED, see kws/vendor-arm/SOURCE.md)"
fi
# 5) 可选：宿主机模型加载冒烟（默认不开）。设 OPEN_XIAOAI_CHECK_KWS_MODEL=1
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
