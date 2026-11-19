#!/usr/bin/env bash
# test_preflight.sh — deploy/preflight.sh 的 mock 测试。
#
# 用法：bash deploy/test_preflight.sh（或 ./deploy/test_preflight.sh）
#
# 做法：建一个假仓库根（临时目录），拷真实的 docker-compose.yml /
# Dockerfile.hub / kws-sidecar/Dockerfile / hub/config.yaml，用 openssl
# 现场生成一套真 CA + Hub 证书（所以指纹比对是真验签，不是摆拍），再用
# PATH 上的假 docker / ss / ping / nc / ssh 垫片控制外部世界。
#
# 绝不碰真服务：没有真 docker、没有真 NAS、没有真音箱、没有真 SSH。
# 假 docker/ss/ping 只在测试进程的 PATH 里，前缀目录用完即删。
#
# 每个用例：跑一次 preflight，断言 exit code + 输出里出现期望的标识文案。
# 覆盖：成功路径、每项检查逐个失败（.env 缺失、Token 空/短/弱/相同、
# HASS 空、HUB_PORT 被未知进程占、镜像缺且拉不到、证书种子缺/不全、
# 指纹不一致、音箱不可达、YAML 坏），以及 --skip-speaker、旧 Hub 占
# 4399（应通过）、镜像缺但仓库可拉（应通过）、SSH 远程模式。
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PREFLIGHT="$REPO_ROOT/deploy/preflight.sh"
WORK="$(mktemp -d /tmp/preflight-test.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

TEMPLATE="$WORK/template"
FAKEBIN="$WORK/fakebin"
TOOLS="$WORK/tools"
CASES_RUN=0
CASES_OK=0
FAILED_CASES=""

note() { echo "== $*"; }

# ---------- 假工具 ----------
# tools/：preflight 需要的真工具软链（不含 docker/ss/ping/ssh，那四个是垫片）。
mkdir -p "$FAKEBIN" "$TOOLS"
for t in bash env grep sed tr awk diff timeout uname head openssl python3 cat cut wc sort dirname basename sleep; do
  ln -sf "$(command -v "$t")" "$TOOLS/$t" 2>/dev/null || true
done

# 假 docker：行为由环境变量控制。
#   FAKE_IMAGES_PRESENT：空格分隔的已存在镜像 ref 列表（image inspect 成功）
#   FAKE_MANIFEST_OK=1：manifest inspect 成功（镜像仓库可拉取），否则失败
#   FAKE_DOCKER_PS：逗号分隔的运行中容器名
#   FAKE_DOCKER_NO_COMPOSE=1：docker compose version 失败
cat > "$FAKEBIN/docker" <<'SHIM'
#!/usr/bin/env bash
# fake docker — 只认 preflight 调的几个子命令。
case "${1:-}" in
  --version) echo "Docker version 99.0.0-fake"; exit 0 ;;
  compose)
    if [ "${2:-}" = "version" ]; then
      [ "${FAKE_DOCKER_NO_COMPOSE:-0}" = "1" ] && { echo "fake: no compose plugin" >&2; exit 1; }
      echo "Docker Compose version v99.0.0-fake"; exit 0
    fi
    echo "fake docker: unsupported compose subcommand: $*" >&2; exit 1 ;;
  image)
    if [ "${2:-}" = "inspect" ]; then
      ref="${3:-}"
      case " ${FAKE_IMAGES_PRESENT:-} " in *" $ref "*) exit 0 ;; *) exit 1 ;; esac
    fi
    echo "fake docker: unsupported image subcommand: $*" >&2; exit 1 ;;
  manifest)
    if [ "${2:-}" = "inspect" ]; then
      if [ "${FAKE_MANIFEST_OK:-0}" = "1" ]; then
        echo '{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json"}'
        exit 0
      fi
      echo "no such manifest: ${3:-}" >&2; exit 1
    fi
    echo "fake docker: unsupported manifest subcommand: $*" >&2; exit 1 ;;
  ps)
    # FAKE_DOCKER_PS 是逗号分隔的容器名，每行一个。
    printf '%s' "${FAKE_DOCKER_PS:-}" | tr ',' '\n' | grep -v '^$' || true
    exit 0 ;;
  *) echo "fake docker: unsupported: $*" >&2; exit 1 ;;
esac
SHIM

# 假 ss：FAKE_SS_BUSY 是空格分隔的被占端口列表。
cat > "$FAKEBIN/ss" <<'SHIM'
#!/usr/bin/env bash
# fake ss -tln：只打印被占端口的监听行。
echo "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process"
for p in ${FAKE_SS_BUSY:-}; do
  echo "LISTEN 0 4096 127.0.0.1:$p 0.0.0.0:*"
done
exit 0
SHIM

# 假 ping：FAKE_PING_FAIL=1 时失败。
cat > "$FAKEBIN/ping" <<'SHIM'
#!/usr/bin/env bash
if [ "${FAKE_PING_FAIL:-0}" = "1" ]; then
  echo "fake ping: ${*: -1}: Destination Host Unreachable" >&2; exit 1
fi
echo "fake ping: ${*: -1} reachable"; exit 0
SHIM

# 假 nc：FAKE_NC_FAIL=1 时 TCP 探针失败（本机沙箱会代接任意出站 TCP，
# 所以 /dev/tcp 测不出“不可达”，preflight 的 TCP 探针优先用 nc -z）。
cat > "$FAKEBIN/nc" <<'SHIM'
#!/usr/bin/env bash
if [ "${FAKE_NC_FAIL:-0}" = "1" ]; then exit 1; fi
exit 0
SHIM

# 假 ssh：ssh [选项] host <命令...>，把远程命令在本地 eval（PATH 含垫片）。
# FAKE_SSH_FAIL=1 时连接直接失败。只用于测试，不连任何真主机。
cat > "$FAKEBIN/ssh" <<'SHIM'
#!/usr/bin/env bash
if [ "${FAKE_SSH_FAIL:-0}" = "1" ]; then
  echo "fake ssh: connect to host failed" >&2; exit 255
fi
while [ $# -gt 0 ]; do
  case "$1" in
    -o|-p|-i|-l) shift 2 ;;
    -*) shift ;;
    *) shift; break ;;  # host，吃掉
  esac
done
[ $# -eq 0 ] && exit 0
eval "$*"
SHIM
chmod +x "$FAKEBIN/docker" "$FAKEBIN/ss" "$FAKEBIN/ping" "$FAKEBIN/nc" "$FAKEBIN/ssh"

# ---------- 假仓库根模板（真证书 + 真配置文件）----------
mkdir -p "$TEMPLATE/deploy/hub-certs" "$TEMPLATE/hub" "$TEMPLATE/kws-sidecar"
cp "$REPO_ROOT/deploy/docker-compose.yml" "$REPO_ROOT/deploy/Dockerfile.hub" "$TEMPLATE/deploy/"
cp "$REPO_ROOT/kws-sidecar/Dockerfile" "$TEMPLATE/kws-sidecar/"
cp "$REPO_ROOT/hub/config.yaml" "$TEMPLATE/hub/"
touch "$TEMPLATE/deploy/hub-linux-amd64"
touch "$TEMPLATE/kws-sidecar/server.py" "$TEMPLATE/kws-sidecar/entrypoint.sh" "$TEMPLATE/kws-sidecar/requirements.txt"

# 真证书：CA + CA 签发的 hub 证书（openssl 现场生成）。
(cd "$TEMPLATE/deploy/hub-certs" && \
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out ca.key 2>/dev/null && \
  openssl req -x509 -new -key ca.key -days 3650 -subj "/CN=test-ca" -out ca.crt 2>/dev/null && \
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out hub.key 2>/dev/null && \
  openssl req -new -key hub.key -subj "/CN=open-xiaoai-hub" -out hub.csr 2>/dev/null && \
  openssl x509 -req -in hub.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -out hub.crt 2>/dev/null && \
  rm -f hub.csr ca.srl) || { echo "FATAL: 测试证书生成失败"; exit 1; }
FP="$(openssl x509 -in "$TEMPLATE/deploy/hub-certs/hub.crt" -noout -fingerprint -sha256 \
  | cut -d= -f2 | tr -d ':' | tr 'A-Z' 'a-z')"
printf 'hub: %s\n' "$FP" > "$TEMPLATE/deploy/hub-certs/fingerprints.txt"
cat > "$TEMPLATE/deploy/client-oh2p.toml" <<EOF
hub_url = "wss://192.168.1.10:4399"
token = "placeholder-not-checked-by-preflight"
model = "oh2p"
tls = true
hub_ca_fingerprint = "$FP"
EOF

# 全部 7 个镜像 ref（5 个 compose image + 2 个 Dockerfile FROM），供假 docker 用。
ALL_IMAGES="$(grep -E '^[[:space:]]*image:[[:space:]]*' "$TEMPLATE/deploy/docker-compose.yml" \
  | sed -e 's/^[[:space:]]*image:[[:space:]]*//' -e 's/[[:space:]]*#.*//' -e 's/[[:space:]]*$//'; \
  grep -E '^FROM[[:space:]]' "$TEMPLATE/deploy/Dockerfile.hub" "$TEMPLATE/kws-sidecar/Dockerfile" | awk '{print $2}')"
WHISPER_IMG="$(printf '%s' "$ALL_IMAGES" | grep 'wyoming-whisper' | head -1)"

# ---------- 用例脚手架 ----------
# 每个用例先 reset_case 得到一份干净的假仓库根（$ROOT），再按需改动。
reset_case() {
  ROOT="$WORK/case-root"
  rm -rf "$ROOT"; cp -r "$TEMPLATE" "$ROOT"
  TOK_CLIENT="$(openssl rand -hex 16)"
  TOK_WEB="$(openssl rand -hex 16)"
  cat > "$ROOT/deploy/.env" <<EOF
NAS_IP=192.168.1.10
MUSIC_FOLDER=/volume1/music
HASS_TOKEN=$(openssl rand -hex 16)
LLM_API_KEY=
OPEN_XIAOAI_TOKEN=$TOK_CLIENT
OPEN_XIAOAI_WEB_TOKEN=$TOK_WEB
SPEAKER_IP=192.168.1.99
HUB_PORT=4399
EOF
  # 默认假外部世界：docker 齐全、端口全空闲、镜像全在、ping 通、ssh 通。
  export FAKE_IMAGES_PRESENT="$ALL_IMAGES"
  export FAKE_MANIFEST_OK=1
  export FAKE_DOCKER_PS=""
  export FAKE_SS_BUSY=""
  export FAKE_PING_FAIL=0
  export FAKE_NC_FAIL=0
  export FAKE_SSH_FAIL=0
  export FAKE_DOCKER_NO_COMPOSE=0
  unset NAS_SSH HUB_CERTS_DIR CLIENT_TOML
}

# run_case <名> <期望exit> <期望输出片段> [preflight 参数...]
# 默认 PATH=垫片+真工具（不含真 docker/ss/ping/ssh）；CASE_PATH 可覆盖。
run_case() {
  local name="$1" want_exit="$2" want_pat="$3"; shift 3
  local out="$WORK/out.$CASES_RUN.log"
  CASES_RUN=$((CASES_RUN+1))
  env PREFLIGHT_ROOT="$ROOT" PATH="${CASE_PATH:-$FAKEBIN:$TOOLS}" \
    FAKE_IMAGES_PRESENT="$FAKE_IMAGES_PRESENT" FAKE_MANIFEST_OK="$FAKE_MANIFEST_OK" \
    FAKE_DOCKER_PS="$FAKE_DOCKER_PS" FAKE_SS_BUSY="$FAKE_SS_BUSY" \
    FAKE_PING_FAIL="$FAKE_PING_FAIL" FAKE_NC_FAIL="$FAKE_NC_FAIL" FAKE_SSH_FAIL="$FAKE_SSH_FAIL" \
    FAKE_DOCKER_NO_COMPOSE="$FAKE_DOCKER_NO_COMPOSE" \
    ${NAS_SSH:+NAS_SSH="$NAS_SSH"} ${HUB_CERTS_DIR:+HUB_CERTS_DIR="$HUB_CERTS_DIR"} \
    ${CLIENT_TOML:+CLIENT_TOML="$CLIENT_TOML"} \
    bash "$PREFLIGHT" "$@" >"$out" 2>&1
  local got_exit=$?
  if [ "$got_exit" -ne "$want_exit" ]; then
    echo "FAIL: $name —— exit=$got_exit，期望 $want_exit（输出见 $out）"
    sed 's/^/    /' "$out"
    FAILED_CASES="$FAILED_CASES $name(exit:$got_exit!=$want_exit)"
    return
  fi
  if [ -n "$want_pat" ] && ! grep -q "$want_pat" "$out"; then
    echo "FAIL: $name —— 输出里没找到期望片段：$want_pat（输出见 $out）"
    sed 's/^/    /' "$out"
    FAILED_CASES="$FAILED_CASES $name(pattern:$want_pat)"
    return
  fi
  CASES_OK=$((CASES_OK+1))
  echo "ok:   $name （exit=$got_exit，含「$want_pat」）"
}

# 往假 .env 里改一个键的值。
set_env_key() { # $1=KEY $2=VALUE（空值即清空）
  local k="$1" v="$2"
  grep -v "^$k=" "$ROOT/deploy/.env" > "$WORK/env.tmp" || true
  printf '%s=%s\n' "$k" "$v" >> "$WORK/env.tmp"
  mv "$WORK/env.tmp" "$ROOT/deploy/.env"
}

# ================= 用例 =================
note "1. 成功路径（LLM_API_KEY 为空，应只提示、不失败）"
reset_case
run_case "success" 0 "全部通过"
grep -q "$TOK_CLIENT" "$WORK/out.0.log" && { echo "FAIL: success —— 输出泄露了 Token 值"; FAILED_CASES="$FAILED_CASES success(token-leak)"; }
grep -q "LLM_API_KEY 为空" "$WORK/out.0.log" || { echo "FAIL: success —— 没看到 LLM 为空的提示"; FAILED_CASES="$FAILED_CASES success(llm-note)"; }

note "2. .env 缺失"
reset_case; rm -f "$ROOT/deploy/.env"
run_case "missing-env" 1 "FAIL: \[env\] 找不到"

note "3. OPEN_XIAOAI_TOKEN 为空"
reset_case; set_env_key OPEN_XIAOAI_TOKEN ""
run_case "empty-client-token" 1 "OPEN_XIAOAI_TOKEN 为空"

note "4. OPEN_XIAOAI_TOKEN 太短"
reset_case; set_env_key OPEN_XIAOAI_TOKEN "short"
run_case "short-token" 1 "OPEN_XIAOAI_TOKEN 太弱"

note "5. OPEN_XIAOAI_TOKEN 是占位弱值"
reset_case; set_env_key OPEN_XIAOAI_TOKEN "CHANGE_ME"
run_case "weak-placeholder-token" 1 "OPEN_XIAOAI_TOKEN 太弱"

note "6. 两个 Token 相同"
reset_case; set_env_key OPEN_XIAOAI_WEB_TOKEN "$TOK_CLIENT"
run_case "tokens-equal" 1 "OPEN_XIAOAI_WEB_TOKEN 与 OPEN_XIAOAI_TOKEN 相同"

note "7. HASS_TOKEN 为空"
reset_case; set_env_key HASS_TOKEN ""
run_case "empty-hass-token" 1 "HASS_TOKEN 为空"

note "8. HUB_PORT 被未知进程占用（无旧 Hub 容器）"
reset_case; export FAKE_SS_BUSY="4399"; export FAKE_DOCKER_PS="some-other-app"
run_case "hub-port-unknown" 1 "端口 4399 已被未知进程占用"

note "9. HUB_PORT 被旧 Hub 占用（割接预期，应通过）"
reset_case; export FAKE_SS_BUSY="4399"; export FAKE_DOCKER_PS="oxn-hub,oxn-whisper"
run_case "hub-port-old-hub" 0 "旧 Hub 占用"

note "10. 其他端口被占（只警告，应通过）"
reset_case; export FAKE_SS_BUSY="8080"
run_case "other-port-busy" 0 "端口 8080 已被占用"

note "11. 镜像本地缺失且仓库拉不到"
reset_case
export FAKE_IMAGES_PRESENT="$(printf '%s' "$ALL_IMAGES" | grep -v 'wyoming-whisper')"
export FAKE_MANIFEST_OK=0
run_case "image-missing-unpullable" 1 "镜像缺失且仓库里找不到：$WHISPER_IMG"

note "12. 镜像本地缺失但仓库可拉（应通过）"
reset_case
export FAKE_IMAGES_PRESENT="$(printf '%s' "$ALL_IMAGES" | grep -v 'wyoming-whisper')"
export FAKE_MANIFEST_OK=1
run_case "image-missing-pullable" 0 "仓库可拉取：$WHISPER_IMG"

note "13. 证书种子目录缺失"
reset_case; rm -rf "$ROOT/deploy/hub-certs"
run_case "certs-missing" 1 "找不到 Hub TLS 种子目录"

note "14. 证书种子不全（HUB_CERTS_DIR 指向缺 hub.key 的目录）"
reset_case; rm -f "$ROOT/deploy/hub-certs/hub.key"
export HUB_CERTS_DIR="$ROOT/deploy/hub-certs"
run_case "certs-partial" 1 "证书种子不全"

note "15. client.toml 指纹与种子不一致"
reset_case
sed -i 's/^hub_ca_fingerprint = .*/hub_ca_fingerprint = "0000000000000000000000000000000000000000000000000000000000000000"/' "$ROOT/deploy/client-oh2p.toml"
run_case "fingerprint-mismatch" 1 "指纹不一致"

note "16. 音箱不可达（ping 与 TCP :22 均失败）"
reset_case; export FAKE_PING_FAIL=1; export FAKE_NC_FAIL=1; set_env_key SPEAKER_IP "192.0.2.99"
run_case "speaker-unreachable" 1 "音箱 192.0.2.99 不可达"

note "17. --skip-speaker：音箱不可达时仍通过"
reset_case; export FAKE_PING_FAIL=1; export FAKE_NC_FAIL=1; set_env_key SPEAKER_IP "192.0.2.99"
run_case "skip-speaker" 0 "SKIP: \[speaker\]" --skip-speaker

note "17a. SPEAKER_IP 未设置（跳过音箱检查，应通过）"
reset_case; set_env_key SPEAKER_IP ""
run_case "speaker-ip-unset" 0 "SKIP: \[speaker\]"

note "18. hub/config.yaml 语法坏"
reset_case; printf 'bad: [unclosed\n\tfoo: bar\n' >> "$ROOT/hub/config.yaml"
run_case "bad-config-yaml" 1 "hub/config.yaml 解析失败"

note "19. docker-compose.yml 语法坏"
reset_case; printf 'bad: [unclosed\n\tfoo: bar\n' >> "$ROOT/deploy/docker-compose.yml"
run_case "bad-compose-yaml" 1 "docker-compose.yml 解析失败"

note "20. docker 命令缺失（PATH 里无 docker 垫片）"
reset_case
CASE_PATH="$TOOLS"  # 只有真工具，无 docker/ss/ping/ssh 垫片
run_case "docker-missing" 1 "找不到 docker 命令"
unset CASE_PATH

note "21. 远程模式（NAS_SSH + 假 ssh）：应通过，目标显示 NAS"
reset_case; export NAS_SSH="huajun@192.168.88.247"
run_case "remote-mode" 0 "目标：NAS huajun@192.168.88.247"
unset NAS_SSH

note "22. 远程模式 SSH 连不上"
reset_case; export NAS_SSH="huajun@192.168.88.247"; export FAKE_SSH_FAIL=1
run_case "remote-ssh-fail" 1 "SSH 连不上 huajun@192.168.88.247"
unset NAS_SSH

echo
echo "================ 结果：$CASES_OK/$CASES_RUN 通过 ================"
if [ -n "$FAILED_CASES" ]; then
  echo "失败用例：$FAILED_CASES"
  exit 1
fi
echo "全部用例通过。注意：本测试只覆盖本地模式 + 假 ssh 的远程模式；"
echo "真 NAS（真 Docker/真端口/真镜像拉取）与真音箱仍需部署日实测。"
exit 0
