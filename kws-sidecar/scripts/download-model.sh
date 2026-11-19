#!/bin/bash
# 下载 sherpa-onnx WenetSpeech KWS 模型（边车服务端用）。
#
# 与 client/scripts/download-kws-model.sh 同源同 SHA（同一模型包），
# 区别：默认目标目录是边车的数据卷 /data/kws/models，且不含音箱侧
# 的 scp/ARM 说明。模型文件不要提交进 git 仓库（大文件，部署时下载）。
#
# 用法：
#   ./scripts/download-model.sh [目标目录] [--fp32]
#   WAKE_WORD="你好小李" ./scripts/download-model.sh /data/kws/models
#
# 来源（sherpa-onnx 官方文档，2026-11-17 核实）：
#   https://k2-fsa.github.io/sherpa/onnx/kws/pretrained_models/index.html
#   模型：sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01
#   （WenetSpeech L 10,000 小时训练，3.3M 参数，拼音建模单元，支持自定义唤醒词）
#   下载地址：GitHub k2-fsa/sherpa-onnx releases，tag kws-models
#   许可：Apache License 2.0（模型包内 README.md 有写）
#   SHA256（2026-11-17 下载实测值；若上游重新打包会变，脚本会报错并提示更新）：
#   b2f7c89690dc8ce4c6ed6afeab7cd800c36ad1421fb6b6302b4a4b194cf7f35f
#
# 产出（目标目录内，约 5.6MB，int8 编码器）：
#   encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx  4,777,666 字节
#   decoder-epoch-12-avg-2-chunk-16-left-64.onnx        675,349 字节
#   joiner-epoch-12-avg-2-chunk-16-left-64.onnx         253,410 字节
#   tokens.txt                                            1,627 字节
#   keywords.txt  （唤醒词 token 行，见下）
# --fp32 时另保留 encoder-epoch-12-avg-2-chunk-16-left-64.onnx（12,152,560 字节）。
# 其余文件（epoch-99 变体、test_wavs、fp32 decoder/joiner）不保留。
#
# keywords.txt 生成：
#   优先用 sherpa-onnx-cli text2token（需 pip install sherpa-onnx sentencepiece
#   pypinyin）从 WAKE_WORD 现算，--tokens-type ppinyin；
#   若该工具不可用且 WAKE_WORD 就是默认的“你好小李”，写入本次实验验证过的
#   token 行：n ǐ h ǎo x iǎo l ǐ @你好小李
#   其它唤醒词无 text2token 时直接失败（不瞎写 token）。
set -euo pipefail

MODEL_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01.tar.bz2"
MODEL_SHA256="b2f7c89690dc8ce4c6ed6afeab7cd800c36ad1421fb6b6302b4a4b194cf7f35f"
MODEL_DIR_NAME="sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01"
WAKE_WORD="${WAKE_WORD:-你好小李}"
OUT_DIR="${1:-${KWS_MODEL_DIR:-/data/kws/models}}"
FP32=0
for arg in "$@"; do
    case "$arg" in
        --fp32) FP32=1 ;;
        --*) echo "unknown option: $arg" >&2; exit 2 ;;
    esac
done
# 第一个位置参数若为 --fp32 则 OUT_DIR 保持默认
if [ "${1:-}" = "--fp32" ]; then OUT_DIR="${KWS_MODEL_DIR:-/data/kws/models}"; fi

mkdir -p "$OUT_DIR"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $MODEL_URL"
# SC-P2-13：curl 加 --fail——无 --fail 时 HTTP 404/403 也会写出一个
# 错误页面文件并 exit 0，随后 SHA 校验失败才退出，报错是误导性的
# “SHA256 校验失败”（会误以为上游重新打包）；--fail 让 HTTP 错误
# 在下载这一步就失败，报错即真实原因。
curl -sSLf --retry 2 -o "$TMP/model.tar.bz2" "$MODEL_URL"

echo "==> verifying SHA256 ($(stat -c%s "$TMP/model.tar.bz2") bytes)"
if ! echo "$MODEL_SHA256  $TMP/model.tar.bz2" | sha256sum -c -; then
    # SC-P2-13：SHA 失败的报错同时打印文件大小，并说明下载本身已成功
    # （HTTP 错误已被 --fail 拦下），避免误判为下载失败。
    echo "ERROR: SHA256 mismatch for the downloaded model tarball (size: $(stat -c%s "$TMP/model.tar.bz2") bytes)." >&2
    echo "The download itself succeeded (HTTP errors fail earlier via curl --fail);" >&2
    echo "either the upstream release was repackaged (update MODEL_SHA256) or the file was damaged in transit." >&2
    exit 1
fi
echo "==> extracting"
tar --no-same-owner -xjf "$TMP/model.tar.bz2" -C "$TMP"

SRC="$TMP/$MODEL_DIR_NAME"
cp "$SRC/encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx" "$OUT_DIR/"
cp "$SRC/decoder-epoch-12-avg-2-chunk-16-left-64.onnx" "$OUT_DIR/"
cp "$SRC/joiner-epoch-12-avg-2-chunk-16-left-64.onnx" "$OUT_DIR/"
cp "$SRC/tokens.txt" "$OUT_DIR/"
if [ "$FP32" = 1 ]; then
    cp "$SRC/encoder-epoch-12-avg-2-chunk-16-left-64.onnx" "$OUT_DIR/"
fi

echo "==> generating keywords.txt for wake word: $WAKE_WORD"
# 关键词生成抽到 scripts/regen-keywords.sh（与 entrypoint 启动时
# 的重生成同一份逻辑，避免两处各写一份漂移）。
"$(dirname "$0")/regen-keywords.sh" "$WAKE_WORD" "$OUT_DIR"

echo "==> done: $OUT_DIR"
ls -la "$OUT_DIR"
echo "--- keywords.txt:"
cat "$OUT_DIR/keywords.txt"
