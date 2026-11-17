#!/bin/bash
# 下载参考实现 kws.tar.gz（过渡，TRANSITIONAL，P1-40）。
#
# 用法：./scripts/download-kws-reference.sh [目标目录]
# 默认目标：./kws/vendor-arm（脚本所在 client/ 目录下）。
#
# 来源（2026-11-17 核实）：
#   Gitee idootop/artifacts releases，tag open-xiaoai-kws
#   https://gitee.com/idootop/artifacts/releases/download/open-xiaoai-kws/kws.tar.gz
#   上游出处：idootop/open-xiaoai examples/kws/（init.sh / boot.sh）
#   Tarball SHA256（2026-11-17 下载实测；上游重新打包会变，脚本会报错并提示更新）：
#   690a38b94159f30f600494d5d6501f22083cbf6a967512833fb8e216707b07b9
#   解出的 kws 二进制 SHA256：
#   efc1bbbf7611416ae7d8b895a84a26ec606abc7bc312d31d4001982b7397541f
#
# 产出（目标目录内）：
#   reference-kws.tar.gz   9,483,096 字节（整包，含 kws 二进制 + zipformer2 模型）
#   reference/kws         11,243,180 字节（ARM 32-bit glibc 二进制）
#   reference/models/     zipformer2 布局模型（encoder.onnx 等，与本项目
#                         transducer 布局的 kws-models/ 不同，不要混用）
# 二进制不要提交进 git 仓库（大文件；见 kws/vendor-arm/SOURCE.md）。
#
# 过渡用法（Rust 直连 .so 尚不可用时）：
#   kws 二进制不含 .so、不能被 dlopen；在音箱上作 sidecar 独占麦克风跑唤醒，
#   把 "<timestamp_ms>@<关键词>" 写进 /tmp/open-xiaoai/kws.log
#   （__STARTED__ 表示启动完成），Rust 侧用 src/kws.rs 的 KwsLogMonitor
#   尾随该日志取唤醒事件。ALSA 争用（sidecar 与 Client 同时开麦克风）
#   尚未在真机上验证，见 kws/README.md §ARM 部署。
set -euo pipefail

TARBALL_URL="https://gitee.com/idootop/artifacts/releases/download/open-xiaoai-kws/kws.tar.gz"
TARBALL_SHA256="690a38b94159f30f600494d5d6501f22083cbf6a967512833fb8e216707b07b9"
KWS_SHA256="efc1bbbf7611416ae7d8b895a84a26ec606abc7bc312d31d4001982b7397541f"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${1:-$SCRIPT_DIR/../kws/vendor-arm}"
mkdir -p "$OUT_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $TARBALL_URL"
curl -sSL --retry 2 -o "$TMP/kws.tar.gz" "$TARBALL_URL"

echo "==> verifying tarball SHA256"
echo "$TARBALL_SHA256  $TMP/kws.tar.gz" | sha256sum -c -
cp "$TMP/kws.tar.gz" "$OUT_DIR/reference-kws.tar.gz"

echo "==> extracting"
rm -rf "$OUT_DIR/reference"
mkdir -p "$OUT_DIR/reference"
tar --no-same-owner -xzf "$TMP/kws.tar.gz" -C "$OUT_DIR/reference"

echo "==> verifying kws binary SHA256"
echo "$KWS_SHA256  $OUT_DIR/reference/kws" | sha256sum -c -

echo "==> verifying kws binary is ARM glibc (must run on the speaker)"
file "$OUT_DIR/reference/kws"
if ! file "$OUT_DIR/reference/kws" | grep -q "ARM"; then
    echo "ERROR: reference/kws is not an ARM binary" >&2
    exit 1
fi
# readelf 输出先落盘再 grep：pipefail 下 `readelf | grep -q` 会因 grep
# 提早关管道、readelf SIGPIPE 而误判（见 download-kws-lib.sh 注释）。
readelf -V "$OUT_DIR/reference/kws" > "$TMP/readelf-v.txt" 2>/dev/null || true
if grep -qoE "GLIBC_2\.(2[6-9]|3[0-9])" "$TMP/readelf-v.txt"; then
    echo "WARNING: reference/kws requires a newer glibc than the speaker's 2.25; check before deploying." >&2
fi

echo "==> done: $OUT_DIR"
ls -la "$OUT_DIR/reference-kws.tar.gz" "$OUT_DIR/reference/kws"
ls "$OUT_DIR/reference/models/"
echo "--- next steps (TRANSITIONAL sidecar, on the speaker):"
echo "  1. scp $OUT_DIR/reference-kws.tar.gz root@<音箱IP>:/data/open-xiaoai/kws/"
echo "  2. On the speaker: tar -xzf /data/open-xiaoai/kws/reference-kws.tar.gz -C /data/open-xiaoai/kws/reference"
echo "  3. Run it like references/open-xiaoai/examples/kws/init.sh does (model-type=zipformer2,"
echo "     keywords-file=/data/open-xiaoai/kws/keywords.txt); it logs wake lines to /tmp/open-xiaoai/kws.log"
echo "  4. Rust side: KwsLogMonitor in src/kws.rs tails that log (wiring into main.rs still TODO, see kws/README.md)"
