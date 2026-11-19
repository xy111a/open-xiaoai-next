#!/bin/bash
# 仅重生成边车关键词表 keywords.txt（不下载模型）。
#
# 用途：面板改唤醒词后，边车重启时按 config.yaml 里的新唤醒词
# 重新生成 keywords.txt（entrypoint.sh 每次启动都会调它）。
# 与 scripts/download-model.sh 的 keywords 生成段同源（同一 token
# 化逻辑），避免两处各写一份漂移。
#
# 用法：
#   ./scripts/regen-keywords.sh "<唤醒词>" [模型目录]
#   WAKE_WORD="你好小李" ./scripts/regen-keywords.sh /data/kws/models
#
# 依赖：模型目录里须已有 tokens.txt（download-model.sh 已下载）；
# token 化用 sherpa-onnx-cli text2token（--tokens-type ppinyin）。
# 只有“你好小李”有内置的已验证 token 行（2026-11-17 实验验证），
# 其它词必须有 sherpa-onnx-cli，否则报错退出（不写一个错的表，
# 避免边车带着旧词/空表启动——空表时 sherpa 恒无检出）。
set -euo pipefail

WAKE_WORD="${1:-${WAKE_WORD:-}}"
MODEL_DIR="${2:-${KWS_MODEL_DIR:-/data/kws/models}}"
if [ -z "$WAKE_WORD" ]; then
    echo "ERROR: usage: $0 \"<唤醒词>\" [模型目录]（或设 WAKE_WORD 环境变量）" >&2
    exit 2
fi
if [ ! -f "$MODEL_DIR/tokens.txt" ]; then
    echo "ERROR: $MODEL_DIR/tokens.txt 不存在（模型未下载，先跑 download-model.sh）" >&2
    exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> generating keywords.txt for wake word: $WAKE_WORD"
if command -v sherpa-onnx-cli >/dev/null 2>&1; then
    printf '%s @%s\n' "$WAKE_WORD" "$WAKE_WORD" > "$TMP/keywords_raw.txt"
    sherpa-onnx-cli text2token \
        --tokens "$MODEL_DIR/tokens.txt" \
        --tokens-type ppinyin \
        "$TMP/keywords_raw.txt" "$MODEL_DIR/keywords.txt"
elif [ "$WAKE_WORD" = "你好小李" ]; then
    echo "sherpa-onnx-cli not found; writing the verified token line for 你好小李" >&2
    cat > "$MODEL_DIR/keywords.txt" <<'EOF'
n ǐ h ǎo x iǎo l ǐ @你好小李
EOF
    echo "NOTE: this fallback line was verified in the 2026-11-17 experiment; for any other wake word, install sherpa-onnx and re-run." >&2
else
    echo "ERROR: sherpa-onnx-cli not found and no verified token line for wake word '$WAKE_WORD'." >&2
    echo "Install it first: pip install sherpa-onnx sentencepiece pypinyin" >&2
    exit 1
fi

echo "--- keywords.txt:"
cat "$MODEL_DIR/keywords.txt"
