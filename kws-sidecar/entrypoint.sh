#!/bin/bash
# kws-sidecar 容器入口：模型缺失时先下载，下载失败明确报错退出；
# 模型就绪后启动 server.py。模型绝不 bake 进镜像/仓库。
set -euo pipefail

MODEL_DIR="${KWS_MODEL_DIR:-/data/kws/models}"
REQUIRED=(
    "encoder-epoch-12-avg-2-chunk-16-left-64.int8.onnx"
    "decoder-epoch-12-avg-2-chunk-16-left-64.onnx"
    "joiner-epoch-12-avg-2-chunk-16-left-64.onnx"
    "tokens.txt"
    "keywords.txt"
)

model_complete() {
    for f in "${REQUIRED[@]}"; do
        [ -f "$MODEL_DIR/$f" ] || return 1
    done
    return 0
}

if ! model_complete; then
    echo "==> KWS model incomplete in $MODEL_DIR; downloading..." >&2
    if ! /app/scripts/download-model.sh "$MODEL_DIR"; then
        echo "FATAL: failed to download KWS model to $MODEL_DIR; refusing to start" >&2
        exit 1
    fi
fi

if ! model_complete; then
    echo "FATAL: KWS model still incomplete in $MODEL_DIR after download; refusing to start" >&2
    ls -la "$MODEL_DIR" >&2 || true
    exit 1
fi

echo "==> KWS model ready in $MODEL_DIR"
exec python /app/server.py "$@"
