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
    # S-2：下载前先预检卷目录可写——全新具名卷若属主仍是 root，uid 1000
    # 的下载会在 download-model.sh 里的 cp 上失败，报错藏在脚本深处；
    # 这里先给一句可读的 FATAL 再退出（Dockerfile 的 mkdir + chown
    # 负责新卷，nas-install.sh §4 的 chown one-shot 负责旧卷迁移）。
    if [ ! -d "$MODEL_DIR" ]; then
        if ! mkdir -p "$MODEL_DIR" 2>/dev/null; then
            echo "FATAL: KWS model dir $MODEL_DIR does not exist and cannot be created (running as uid $(id -u)); check the kws-data volume ownership" >&2
            exit 1
        fi
    fi
    if [ ! -w "$MODEL_DIR" ]; then
        echo "FATAL: KWS model dir $MODEL_DIR is not writable by uid $(id -u); refusing to download (it would fail inside download-model.sh)" >&2
        ls -ld "$MODEL_DIR" >&2 || true
        echo "Fix: on the NAS run the chown one-shot from deploy/nas-install.sh §4 (docker compose run --rm --user root --no-deps --entrypoint /bin/chown kws-sidecar -R 1000:1000 /data/kws/models), or delete the stale volume so a fresh one is created from the image (Dockerfile mkdir + chown, copy-up)" >&2
        exit 1
    fi
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

# 唤醒词以 Hub 的 config.yaml 为准：每次启动都按当前唤醒词重生成
# keywords.txt（面板改唤醒词后重启边车即生效；不重生成的话边车
# 仍用旧词，Hub 的新词永远唤不醒——静默失配，比启动失败更糟）。
# config.yaml 由 compose 以只读挂载进边车（/app/config.yaml）；
# 未挂载时退回 WAKE_WORD 环境变量（默认“你好小李”）。
WAKE_WORD="${WAKE_WORD:-}"
if [ -f /app/config.yaml ]; then
    CFG_WAKE="$(grep -E '^[[:space:]]*wake_word[[:space:]]*:' /app/config.yaml | head -1 | sed -E 's/^[^:]*:[[:space:]]*//; s/^"//; s/"[[:space:]]*$//; s/^'"'"'//; s/'"'"'[[:space:]]*$//')"
    if [ -n "$CFG_WAKE" ]; then WAKE_WORD="$CFG_WAKE"; fi
fi
WAKE_WORD="${WAKE_WORD:-你好小李}"
echo "==> regenerating keywords.txt for wake word: $WAKE_WORD" >&2
# REGEN_KEYWORDS_SCRIPT 可覆盖 regen 脚本路径（默认容器内 /app/scripts/regen-keywords.sh；
# 测试时指向仓库内的 kws-sidecar/scripts/regen-keywords.sh）。
if ! "${REGEN_KEYWORDS_SCRIPT:-/app/scripts/regen-keywords.sh}" "$WAKE_WORD" "$MODEL_DIR"; then
    echo "FATAL: failed to regenerate keywords.txt for wake word '$WAKE_WORD'; refusing to start with a stale keyword table" >&2
    exit 1
fi

echo "==> KWS model ready in $MODEL_DIR"
exec python /app/server.py "$@"
