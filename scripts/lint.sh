#!/usr/bin/env bash
# open-xiaoai-next lint 入口：本地可跑的全部 lint 检查（规则 1–5 + 基础检查）。
#
# 用法（仓库根目录，或任意目录）：
#   scripts/lint.sh            # 跑全部检查，任一失败即 exit 1
#   scripts/lint.sh go         # 只跑 Go 部分
#   scripts/lint.sh semgrep    # 只跑 Semgrep
#   scripts/lint.sh rust       # 只跑 Rust 部分
#   scripts/lint.sh shell      # 只跑 Shell 脚本 bash -n
#
# 工具链：先 source env.sh（go/cargo 在 ~/workspace/tools/ 下持久安装）；
# golangci-lint 与 semgrep 要求在 ~/workspace/tools/bin/（见仓库根 README
# 或向 Friday 要安装位置）；缺失时对应步骤报错，不静默跳过。
# 检查内容：
#   Go：gofmt、go vet、golangci-lint（.golangci.yml）
#   规则 1 接线覆盖：go run scripts/check-wiring.go
#   规则 2 nil 守卫：go run scripts/check-nil-guards.go
#   规则 3 上限/超时：go run scripts/check-bounds.go（Go 部分）
#   规则 3/4/5：semgrep --config .semgrep/rules.yml（Rust/Python + Go 精确坏形状）
#   Rust：cargo fmt --check、cargo clippy --all-targets、vec! 长度检查
#   Shell：bash -n（firmware-patch/scripts + deploy + scripts；shellcheck 未装）
# 测试（go test / cargo test / pytest）不在此脚本里，由 .github/workflows/ci.yml 跑。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2
# shellcheck disable=SC1091
source "$ROOT/env.sh"
export PATH="$HOME/workspace/tools/bin:$PATH"
export SEMGREP_ENABLE_VERSION_CHECK=0
export SEMGREP_SEND_METRICS=off

fail=0
step() { echo "== lint: $*"; }
note() { echo "   $*"; }
bad() { echo "   FAIL: $*"; fail=1; }

run_go() {
	step "gofmt（hub + scripts）"
	out="$(gofmt -l hub scripts)"
	if [ -n "$out" ]; then
		bad "以下文件格式不对，请跑 gofmt -w：$out"
	else
		note "ok"
	fi

	step "go vet ./...（hub）"
	if (cd hub && go vet ./...); then note "ok"; else bad "go vet 报错"; fi

	step "golangci-lint（hub，配置 .golangci.yml）"
	if ! command -v golangci-lint >/dev/null 2>&1; then
		bad "未找到 golangci-lint（应装在 ~/workspace/tools/bin/）"
	elif (cd hub && golangci-lint run ./...); then note "ok"; else bad "golangci-lint 报错"; fi

	step "规则 1：main.go 接线必须有集成测试覆盖（scripts/check-wiring.go）"
	if go run scripts/check-wiring.go --hub hub; then note "ok"; else bad "见上文 FAIL"; fi

	step "规则 2：STT/TTS/MPD 调用前判 nil（scripts/check-nil-guards.go）"
	if go run scripts/check-nil-guards.go --hub hub; then note "ok"; else bad "见上文 FAIL"; fi

	step "规则 3 Go 部分：外部长度设上限、网络读带超时（scripts/check-bounds.go）"
	if go run scripts/check-bounds.go --hub hub; then note "ok"; else bad "见上文 FAIL"; fi
}

run_semgrep() {
	step "Semgrep 规则 3（Rust/Python）/4/5（.semgrep/rules.yml）"
	if ! command -v semgrep >/dev/null 2>&1; then
		bad "未找到 semgrep（应装在 ~/workspace/tools/bin/）"
	elif semgrep --config .semgrep/rules.yml --error --quiet \
		--exclude '*_test.go' --exclude '*/tests/*' --exclude 'test_*.py' \
		hub client/src firmware-patch/scripts; then note "ok"; else bad "见上文 findings"; fi
}

run_rust() {
	# FW-P2-8：semgrep 规则 3 覆盖不到 vec![x; 变量]（无界分配）——
	# 见 scripts/check-vec-len.sh（变量长度即 FAIL，常量长度放行，
	# #[cfg(test)] 模块不查）。CI 里有同款调用（rust job）。
	step "规则 3 Rust vec! 长度检查（client/src）"
	if bash scripts/check-vec-len.sh; then
		note "ok"
	else
		bad "vec![…; 变量] 长度未锚定（见上）"
	fi

	step "cargo fmt --check（client）"
	if ! command -v cargo >/dev/null 2>&1; then
		bad "未找到 cargo（先 source env.sh）"
	elif (cd client && cargo fmt --check); then note "ok"; else bad "格式不对，请跑 cargo fmt"; fi

	step "cargo clippy --all-targets（client，警告不致命，只拦错误）"
	if (cd client && cargo clippy --all-targets > /tmp/open-xiaoai-clippy.log 2>&1); then
		note "ok（警告见 /tmp/open-xiaoai-clippy.log 末尾）"
		tail -5 /tmp/open-xiaoai-clippy.log
	else
		bad "clippy 报错，见 /tmp/open-xiaoai-clippy.log"
		tail -20 /tmp/open-xiaoai-clippy.log
	fi
}

run_shell() {
	step "bash -n（firmware-patch/scripts + deploy + scripts）"
	# FW-P2-8：shellcheck 未装，只跑 bash -n 语法检查，不做风格检查。
	# CI 里有同款独立 job（shell job），两边范围一致。
	local f ok_shell=1
	for f in firmware-patch/scripts/*.sh deploy/*.sh scripts/*.sh; do
		bash -n "$f" || { bad "bash -n 失败：$f"; ok_shell=0; }
	done
	[ "$ok_shell" -eq 1 ] && note "ok（shellcheck 未装，只做 bash -n）"
}

case "${1:-all}" in
	go) run_go ;;
	semgrep) run_semgrep ;;
	rust) run_rust ;;
	shell) run_shell ;;
	all) run_go; run_semgrep; run_rust; run_shell ;;
	*) echo "用法：scripts/lint.sh [all|go|semgrep|rust|shell]" >&2; exit 2 ;;
esac

if [ "$fail" -ne 0 ]; then
	echo "lint: FAIL（见上文 FAIL）"
	exit 1
fi
echo "lint: 全部通过"
