#!/usr/bin/env bash
# open-xiaoai-next 工具链环境（持久安装在 ~/workspace/tools/，/usr 重启会丢）
# 用法：source ~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/env.sh
export RUSTUP_HOME="$HOME/workspace/tools/rustup"
export CARGO_HOME="$HOME/workspace/tools/cargo"
export PATH="$HOME/workspace/tools/go/bin:$HOME/workspace/tools/cargo/bin:$PATH"
export GOPATH="$HOME/workspace/tools/gopath"
export GOMODCACHE="$HOME/workspace/tools/gopath/pkg/mod"
export REA_UNBLOB_COMMAND="$HOME/workspace/tools/rea-venv/bin/unblob"
# libopus/libopusfile/libogg（源码自编，持久化在 ~/workspace/tools/libopus；CGO 用）
export PKG_CONFIG_PATH="$HOME/workspace/tools/libopus/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
export CGO_ENABLED=1
