#!/bin/bash
# 下载官方 sherpa-onnx ARM 预编译库（P1-40）。
#
# 用法：./scripts/download-kws-lib.sh [目标目录]
# 默认目标：./kws/vendor-arm（脚本所在 client/ 目录下）。
#
# 来源（2026-11-17 核实）：
#   sherpa-onnx 官方 GitHub releases，v2.0.1
#   https://github.com/k2-fsa/sherpa-onnx/releases/download/v2.0.1/sherpa-onnx-v2.0.1-linux-arm-gnueabihf-shared.tar.bz2
#   许可：Apache License 2.0
#   Tarball SHA256（2026-11-17 下载实测；上游重新打包会变，脚本会报错并提示更新）：
#   099c6bc956b26df246712ff9902419ff9cbc938c014177ea3e205b32675b48d5
#
# 产出（目标目录内，约 25MB）：
#   libsherpa-onnx-c-api.so  4,733,132 字节
#     SHA256 46d5250538b5bcf97d921204f7fe61eca7bac119ad2dcb1c95b557a314d8bc6b
#   libonnxruntime.so       20,617,061 字节
#     SHA256 4636872e9b50f985b7ecaa55a6829dee81bdb96f77010b5bc0a4059e51d872dc
# 其余文件（CLI 二进制、libasound 副本、cxx-api）不保留。
# .so 文件不要提交进 git 仓库（大文件；见 kws/vendor-arm/SOURCE.md）。
#
# !!! 兼容性警告（2026-11-17 实测，见 kws/vendor-arm/SOURCE.md）：
#   该官方 .so 要求 GLIBC 最高 2.34 + GLIBCXX_3.4.29，音箱只有 glibc 2.25 /
#   libstdc++ 3.4.22，直接 dlopen 会失败；且早先 ARM Client 是 musl
#   静态链接时 dlopen 本身就不被支持（"Dynamic loading not supported"），
#   当前默认构建目标已改为 gnueabihf（动态链接），链接方式这层障碍已不存在。
#   本脚本只负责“把官方 ARM .so 取到本地并校验”，不代表“能上音箱跑”。
#   真机可用之前，须先解决 glibc 版本问题（从源码重编 .so），
#   或走服务端 KWS 路线（NAS kws-sidecar，当前唯一能工作的唤醒路径）。
set -euo pipefail

TARBALL_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/v2.0.1/sherpa-onnx-v2.0.1-linux-arm-gnueabihf-shared.tar.bz2"
TARBALL_SHA256="099c6bc956b26df246712ff9902419ff9cbc938c014177ea3e205b32675b48d5"
C_API_SHA256="46d5250538b5bcf97d921204f7fe61eca7bac119ad2dcb1c95b557a314d8bc6b"
ORT_SHA256="4636872e9b50f985b7ecaa55a6829dee81bdb96f77010b5bc0a4059e51d872dc"
INNER_DIR="sherpa-onnx-v2.0.1-linux-arm-gnueabihf-shared"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${1:-$SCRIPT_DIR/../kws/vendor-arm}"
mkdir -p "$OUT_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $TARBALL_URL"
curl -sSL --retry 2 -o "$TMP/arm.tar.bz2" "$TARBALL_URL"

echo "==> verifying tarball SHA256"
echo "$TARBALL_SHA256  $TMP/arm.tar.bz2" | sha256sum -c -
echo "==> extracting lib/"
tar --no-same-owner -xjf "$TMP/arm.tar.bz2" -C "$TMP" "$INNER_DIR/lib/libsherpa-onnx-c-api.so" "$INNER_DIR/lib/libonnxruntime.so"

cp "$TMP/$INNER_DIR/lib/libsherpa-onnx-c-api.so" "$OUT_DIR/"
cp "$TMP/$INNER_DIR/lib/libonnxruntime.so" "$OUT_DIR/"

echo "==> verifying .so SHA256"
echo "$C_API_SHA256  $OUT_DIR/libsherpa-onnx-c-api.so" | sha256sum -c -
echo "$ORT_SHA256  $OUT_DIR/libonnxruntime.so" | sha256sum -c -

echo "==> verifying required exports (SherpaOnnx* KWS symbols)"
# 注意：不要写 `nm -D ... | grep -q`：本脚本开了 pipefail，而 grep -q
# 命中后立即关管道，nm 会因 SIGPIPE 以 141 退出，pipefail 下整个管道
# 判失败，造成“符号明明在却误报缺失”（2026-11-17 实测，非确定性误报）。
# 所以先把 nm 输出落盘一次，再对文件逐个 grep。
nm -D "$OUT_DIR/libsherpa-onnx-c-api.so" > "$TMP/nm-d.txt" 2>/dev/null
REQUIRED_SYMBOLS=(
    SherpaOnnxCreateKeywordSpotter
    SherpaOnnxDestroyKeywordSpotter
    SherpaOnnxCreateKeywordStream
    SherpaOnnxDestroyOnlineStream
    SherpaOnnxOnlineStreamAcceptWaveform
    SherpaOnnxIsKeywordStreamReady
    SherpaOnnxDecodeKeywordStream
    SherpaOnnxResetKeywordStream
    SherpaOnnxGetKeywordResult
    SherpaOnnxDestroyKeywordResult
)
MISSING=0
for sym in "${REQUIRED_SYMBOLS[@]}"; do
    if ! grep -q " T $sym$" "$TMP/nm-d.txt"; then
        echo "ERROR: missing export: $sym" >&2
        MISSING=1
    fi
done
if [ "$MISSING" -ne 0 ]; then
    echo "ERROR: libsherpa-onnx-c-api.so is missing required symbols; refusing to trust it." >&2
    exit 1
fi
echo "==> all 10 required symbols present"

echo "==> checking speaker compatibility (informational only, does not fail)"
# 同上：readelf 输出先落盘再 grep，避免 pipefail + grep -q 提早关管道
# 导致 readelf SIGPIPE、if 条件被误判为假（警告悄悄不打印）。
readelf -V "$OUT_DIR/libsherpa-onnx-c-api.so" > "$TMP/readelf-v.txt" 2>/dev/null || true
# 模式覆盖 GLIBC_2.29 及以上——实测 .so 最高 2.34，只匹配 2.29 会漏报。
if grep -Eq "GLIBC_2\.(29|[3-9][0-9])" "$TMP/readelf-v.txt"; then
    MAX_GLIBC=$(grep -o "GLIBC_2\.[0-9.]*" "$TMP/readelf-v.txt" | sort -Vu | tail -n 1)
    echo "WARNING: libsherpa-onnx-c-api.so requires ${MAX_GLIBC:-GLIBC_2.29+}, but the speaker (LX06 1.94.13 / OH2P 1.58.6) only has glibc 2.25 — it will NOT load on-device as-is. See kws/vendor-arm/SOURCE.md."
fi
if grep -q "GLIBCXX_3.4.29" "$TMP/readelf-v.txt"; then
    echo "WARNING: libsherpa-onnx-c-api.so requires GLIBCXX_3.4.29, but the speaker only has libstdc++ 3.4.22 — it will NOT load on-device as-is. See kws/vendor-arm/SOURCE.md."
fi

echo "==> done: $OUT_DIR"
ls -la "$OUT_DIR/libsherpa-onnx-c-api.so" "$OUT_DIR/libonnxruntime.so"
echo "--- next steps:"
echo "  1. Deploy both .so files together to the speaker: /data/open-xiaoai/kws/lib/"
echo "     (libsherpa needs libonnxruntime.so in the same dir; RPATH is \$ORIGIN)"
echo "  2. client.toml: kws_lib = \"/data/open-xiaoai/kws/lib/libsherpa-onnx-c-api.so\""
echo "  3. NOTE: before this works on-device, the glibc/libstdc++ gap and the"
echo "     musl-static dlopen gap must be closed (see kws/vendor-arm/SOURCE.md);"
echo "     until then use the transitional sidecar: ./scripts/download-kws-reference.sh"
