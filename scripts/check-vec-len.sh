#!/usr/bin/env bash
# 规则 3 Rust vec! 长度检查（FW-P2-8）：semgrep 规则 3 覆盖不到
# vec![x; 变量]（无界分配）。变量长度即 FAIL；常量长度（数字/全大写
# 常量/常量表达式）放行。
# 只查生产代码：#[cfg(test)] 模块里的 vec! 不查——测试分配有界
# （如 client/src/aec.rs 测试里 total_samples = TOTAL_FRAMES * FRAME_SAMPLES，
# 两个都是常量），查它只会误报。实现用 awk 按花括号深度跳过 test 模块；
# 本仓库的 test 模块都在文件末尾，过滤后生产代码行数可用 wc 核对。
# 用法：scripts/check-vec-len.sh [client 目录，默认 client]
# 退出码：0 通过，1 发现变量长度 vec!。
set -u
DIR="${1:-client}"
hits=""
for f in "$DIR"/src/*.rs; do
  [ -f "$f" ] || continue
  out="$(awk '
    /^#\[cfg\(test\)\]$/ { skip_mod = 1; next }
    skip_mod && /mod [a-zA-Z0-9_]+ *\{/ { skip_mod = 0; in_test = 1; depth = 0 }
    in_test {
      n = gsub(/\{/, "{"); m = gsub(/\}/, "}")
      depth += n - m
      if (depth <= 0) in_test = 0
      next
    }
    /vec!\[[^]]*;[[:space:]]*[a-z][a-zA-Z0-9_]*[[:space:]]*\]/ { print FILENAME ":" FNR ":" $0 }
  ' "$f")"
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
    hits=1
  fi
done
if [ -n "$hits" ]; then
  echo "FAIL: vec![…; 变量] 长度未锚定（常量长度放行；测试模块不查）" >&2
  exit 1
fi
echo "vec! 长度检查通过（生产代码无变量长度 vec!）"
