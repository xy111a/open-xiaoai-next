#!/usr/bin/env bash
# preflight.sh — open-xiaoai-next NAS 部署前置检查（fail-fast）。
#
# 用法：
#   ./deploy/preflight.sh                          # 本机检查（docker/端口/镜像查本机）
#   NAS_SSH=huajun@192.168.88.247 ./deploy/preflight.sh
#                                                  # docker/端口/镜像改查 NAS（经 SSH）
#   ./deploy/preflight.sh --skip-speaker           # 跳过某一项检查（见 --help）
#   SPEAKER_IP=192.168.88.190 ./deploy/preflight.sh
#
# 模式说明：
#   - .env / 配置 / 证书种子这三项永远查本机（它们是将要传到 NAS 的文件）。
#   - docker / 端口 / 镜像三项：NAS_SSH 已设时经 SSH 查 NAS；未设时查本机。
#     所以在 NAS 上直接跑本脚本（cd 到部署目录）同样有效——此时 NAS_SSH
#     不用设，查的就是本机（NAS）自己。
#   - 音箱可达性永远从本机查（音箱在局域网内，本机 ping/TCP 直连即可）。
#
# 行为：
#   - 第一项失败即退出（exit 1），并打印哪一项失败、为什么、怎么修。
#   - 全部通过时 exit 0 并打印汇总。
#   - 只做只读检查（docker inspect / manifest inspect / ping / TCP 连接 /
#     ss / openssl 验签），无任何副作用，可反复跑。
#   - 绝不打印任何 Token 值（只报长度/是否为空）。
#
# 可跳过项（flag 与环境变量二选一，跳过时会明确打印 SKIP）：
#   --skip-docker   PREFLIGHT_SKIP_DOCKER=1
#   --skip-env      PREFLIGHT_SKIP_ENV=1
#   --skip-ports    PREFLIGHT_SKIP_PORTS=1
#   --skip-images   PREFLIGHT_SKIP_IMAGES=1
#   --skip-certs    PREFLIGHT_SKIP_CERTS=1
#   --skip-speaker  PREFLIGHT_SKIP_SPEAKER=1
#   --skip-config   PREFLIGHT_SKIP_CONFIG=1
#
# 其他覆盖（flag 优先于环境变量，环境变量优先于 .env/默认值）：
#   --speaker-ip IP | SPEAKER_IP        音箱 IP（.env 里也可写 SPEAKER_IP=）；
#                                       未设时音箱检查跳过（不失败），设了连不上才失败
#   --certs-dir DIR | HUB_CERTS_DIR     Hub TLS 种子目录（默认搜索见 find_certs_dir）
#   --env-file FILE | PREFLIGHT_ENV_FILE .env 路径（默认 deploy/.env）
#   --client-toml FILE | CLIENT_TOML    音箱 client.toml（默认搜索见 find_client_toml）
#   PREFLIGHT_ROOT                       仓库根目录（默认：脚本所在 deploy/ 的上一级）
#   NAS_SSH                              如 huajun@192.168.88.247（见上，docker/端口/镜像走 NAS）
#   HUB_PORT                             Hub 端口（默认 4399，.env 里也可写）
#
# 已知失败模式（本脚本专门防的）：
#   - 未播种证书就 up：Hub 会生成全新 CA，指纹变掉，client.toml 的
#     hub_ca_fingerprint 与音箱 mTLS 客户端证书同时失效（见证书检查）。
#   - HUB_PORT 被旧 Hub 占着：割接时是预期的（停旧 Hub 或改 HUB_PORT=4400
#     并行）；被未知进程占着则必须先处理（见端口检查）。
set -uo pipefail

ROOT="${PREFLIGHT_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
ENV_FILE="${PREFLIGHT_ENV_FILE:-$ROOT/deploy/.env}"
COMPOSE_FILE="$ROOT/deploy/docker-compose.yml"
CONFIG_FILE="$ROOT/hub/config.yaml"
HUB_DOCKERFILE="$ROOT/deploy/Dockerfile.hub"
KWS_DOCKERFILE="$ROOT/kws-sidecar/Dockerfile"
WHISPER_DOCKERFILE="$ROOT/deploy/Dockerfile.whisper"
CERTS_DIR_OVERRIDE="${HUB_CERTS_DIR:-}"
CLIENT_TOML_OVERRIDE="${CLIENT_TOML:-}"
SPEAKER_IP_OVERRIDE="${SPEAKER_IP:-}"
NAS_SSH="${NAS_SSH:-}"

SKIP_DOCKER="${PREFLIGHT_SKIP_DOCKER:-0}"
SKIP_ENV="${PREFLIGHT_SKIP_ENV:-0}"
SKIP_PORTS="${PREFLIGHT_SKIP_PORTS:-0}"
SKIP_IMAGES="${PREFLIGHT_SKIP_IMAGES:-0}"
SKIP_CERTS="${PREFLIGHT_SKIP_CERTS:-0}"
SKIP_SPEAKER="${PREFLIGHT_SKIP_SPEAKER:-0}"
SKIP_CONFIG="${PREFLIGHT_SKIP_CONFIG:-0}"

PASS_COUNT=0
SKIP_COUNT=0
TOTAL_CHECKS=7

usage() {
  sed -n '2,40p' "$0"
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --help|-h) usage ;;
    --skip-docker)  SKIP_DOCKER=1 ;;
    --skip-env)     SKIP_ENV=1 ;;
    --skip-ports)   SKIP_PORTS=1 ;;
    --skip-images)  SKIP_IMAGES=1 ;;
    --skip-certs)   SKIP_CERTS=1 ;;
    --skip-speaker) SKIP_SPEAKER=1 ;;
    --skip-config)  SKIP_CONFIG=1 ;;
    --speaker-ip)   SPEAKER_IP_OVERRIDE="${2:-}"; shift ;;
    --speaker-ip=*) SPEAKER_IP_OVERRIDE="${1#--speaker-ip=}" ;;
    --certs-dir)    CERTS_DIR_OVERRIDE="${2:-}"; shift ;;
    --certs-dir=*)  CERTS_DIR_OVERRIDE="${1#--certs-dir=}" ;;
    --env-file)     ENV_FILE="${2:-}"; shift ;;
    --env-file=*)   ENV_FILE="${1#--env-file=}" ;;
    --client-toml)  CLIENT_TOML_OVERRIDE="${2:-}"; shift ;;
    --client-toml=*) CLIENT_TOML_OVERRIDE="${1#--client-toml=}" ;;
    *) echo "FAIL: preflight: 未知参数：$1（看 --help）"; exit 1 ;;
  esac
  shift
done

# ---- 输出 helpers（FAIL 即退出，fail-fast）----
pass() { PASS_COUNT=$((PASS_COUNT+1)); echo "PASS: [$1] $2"; }
skip() { SKIP_COUNT=$((SKIP_COUNT+1)); echo "SKIP: [$1] $2"; }
warn() { echo "WARN: [$1] $2"; }
info() { echo "INFO: $1"; }
fail() { # $1=检查名 $2=原因 $3=修复建议
  echo "FAIL: [$1] $2"
  [ -n "${3:-}" ] && echo "      修复：$3"
  exit 1
}

# ---- 目标执行：本地或经 NAS_SSH ----
# run_target <cmd...>：NAS_SSH 已设时经 SSH 在 NAS 上跑，否则在本机跑。
# SSH 用 BatchMode（不提示输密码，失败即报错）+ 10 秒连接超时。
if [ -n "$NAS_SSH" ]; then
  SSH_CMD="ssh -o BatchMode=yes -o ConnectTimeout=10 $NAS_SSH"
  TARGET_DESC="NAS $NAS_SSH（经 SSH）"
else
  SSH_CMD=""
  TARGET_DESC="本机"
fi
run_target() {
  if [ -n "$SSH_CMD" ]; then
    # shellcheck disable=SC2086
    $SSH_CMD "$@"
  else
    "$@"
  fi
}
# run_target_sh <shell-string>：需要在目标上跑一段 shell 管道时用。
run_target_sh() {
  if [ -n "$SSH_CMD" ]; then
    # shellcheck disable=SC2086
    $SSH_CMD "bash -c $(printf '%q' "$1")"
  else
    bash -c "$1"
  fi
}

# ================= 1. Docker =================
check_docker() {
  local name="docker"
  [ "$SKIP_DOCKER" = "1" ] && { skip "$name" "经 --skip-docker / PREFLIGHT_SKIP_DOCKER=1 跳过"; return; }
  echo "---- [1/$TOTAL_CHECKS] docker（目标：$TARGET_DESC）"
  if [ -n "$SSH_CMD" ]; then
    $SSH_CMD true 2>/dev/null \
      || fail "$name" "SSH 连不上 $NAS_SSH" "检查 NAS 是否开 SSH、NAS_SSH 的 user@host 对不对、密钥认证是否已配（本脚本不输密码）"
  fi
  command -v docker >/dev/null 2>&1 || [ -n "$SSH_CMD" ] \
    || fail "$name" "本机找不到 docker 命令" "先装 Docker；或在 NAS 上跑本脚本；或设 NAS_SSH=<user>@<NAS_IP> 让 docker/端口/镜像检查走 NAS"
  run_target docker --version >/dev/null 2>&1 \
    || fail "$name" "目标上 docker 不可用（docker --version 失败）" "在 $TARGET_DESC 上确认 Docker 已装且当前用户能跑 docker（NAS 上看 UGOS Docker 管理器）"
  if run_target docker compose version >/dev/null 2>&1; then
    pass "$name" "docker + docker compose 可用（$TARGET_DESC）"
  elif run_target docker-compose version >/dev/null 2>&1; then
    pass "$name" "docker + docker-compose（旧版独立命令）可用（$TARGET_DESC）"
  else
    fail "$name" "目标上有 docker，但 docker compose 不可用" "NAS 上确认 Docker Compose 插件已装（docker compose version 应有输出）"
  fi
}

# ---- .env 解析：只读已知键，值去引号；环境变量优先于 .env ----
# shellcheck disable=SC2034
load_env_file() {
  local file="$1" line key val
  [ -f "$file" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    key="${line%%=*}"; val="${line#*=}"
    key="$(printf '%s' "$key" | tr -d '[:space:]')"
    # 去掉值两端空格与一层引号
    val="$(printf '%s' "$val" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    case "$val" in
      \"*\") val="${val#\"}"; val="${val%\"}" ;;
      \'*\') val="${val#\'}"; val="${val%\'}" ;;
    esac
    case "$key" in
      NAS_IP|MUSIC_FOLDER|HASS_TOKEN|LLM_API_KEY|OPEN_XIAOAI_TOKEN|OPEN_XIAOAI_WEB_TOKEN|HUB_PORT|SPEAKER_IP)
        printf -v "f_$key" '%s' "$val" ;;
    esac
  done < "$file"
  return 0
}
# env_or_file <VAR名>：环境变量非空优先，否则用 .env 里的值。
env_or_file() {
  local var="$1" file_val
  eval "file_val=\${f_$var:-}"
  eval "printf '%s' \"\${$var:-\$file_val}\""
}

# 弱 Token（与 nas-install.sh 的 check_strong_token 同规则 + hub 的 ValidateToken）：
# 长度 <16，或等于常见/公开值。
is_weak_token() {
  local tok="$1" lower
  [ "${#tok}" -lt 16 ] && return 0
  lower="$(printf '%s' "$tok" | tr 'A-Z' 'a-z')"
  case "$lower" in
    change_me|changeme|secret|password|123456|12345678|123456789|1234567890|admin|letmein|qwerty|open-xiaoai|open_xiaoai|xiaoai|test|token|0000000000000000|aaaaaaaaaaaaaaaa) return 0 ;;
  esac
  return 1
}

# ================= 2. .env / Tokens =================
check_env() {
  local name="env"
  [ "$SKIP_ENV" = "1" ] && { skip "$name" "经 --skip-env / PREFLIGHT_SKIP_ENV=1 跳过"; return; }
  echo "---- [2/$TOTAL_CHECKS] .env / Tokens（$ENV_FILE）"
  [ -f "$ENV_FILE" ] \
    || fail "$name" "找不到 $ENV_FILE" "先 cp deploy/.env.example deploy/.env 再填（见 deploy/NAS_CHECKLIST.md §0.4）"
  load_env_file "$ENV_FILE" \
    || fail "$name" "读 $ENV_FILE 失败" "确认文件可读、每行是 KEY=VALUE 格式"
  # M5：提示 .env 当前权限（0600 以外即提醒收紧；nas-install.sh 会在预检查通过后 chmod 600）。
  _env_perm="$(stat -c %a "$ENV_FILE" 2>/dev/null || true)"
  [ -n "$_env_perm" ] && [ "$_env_perm" != "600" ] && warn "$name" "$ENV_FILE 当前权限为 $_env_perm（含 4 个 Token），建议收紧到 600：chmod 600 $ENV_FILE"

  OPEN_XIAOAI_TOKEN="$(env_or_file OPEN_XIAOAI_TOKEN)"
  OPEN_XIAOAI_WEB_TOKEN="$(env_or_file OPEN_XIAOAI_WEB_TOKEN)"
  HASS_TOKEN="$(env_or_file HASS_TOKEN)"
  LLM_API_KEY="$(env_or_file LLM_API_KEY)"
  NAS_IP="$(env_or_file NAS_IP)"
  MUSIC_FOLDER="$(env_or_file MUSIC_FOLDER)"
  HUB_PORT="$(env_or_file HUB_PORT)"
  SPEAKER_IP="${SPEAKER_IP_OVERRIDE:-$(env_or_file SPEAKER_IP)}"
  HUB_PORT="${HUB_PORT:-4399}"

  [ -n "$OPEN_XIAOAI_TOKEN" ] \
    || fail "$name" "OPEN_XIAOAI_TOKEN 为空" "在 deploy/.env 里填一个强随机值：openssl rand -hex 16（Hub 未设它会直接退出）"
  is_weak_token "$OPEN_XIAOAI_TOKEN" \
    && fail "$name" "OPEN_XIAOAI_TOKEN 太弱（长度 ${#OPEN_XIAOAI_TOKEN}，<16 位或为常见/公开值）" "重新生成：openssl rand -hex 16，再填进 deploy/.env"
  [ -n "$OPEN_XIAOAI_WEB_TOKEN" ] \
    || fail "$name" "OPEN_XIAOAI_WEB_TOKEN 为空" "在 deploy/.env 里填一个与 OPEN_XIAOAI_TOKEN 不同的强随机值：openssl rand -hex 16（否则 Web 面板 /api/* 一律 401）"
  is_weak_token "$OPEN_XIAOAI_WEB_TOKEN" \
    && fail "$name" "OPEN_XIAOAI_WEB_TOKEN 太弱（长度 ${#OPEN_XIAOAI_WEB_TOKEN}，<16 位或为常见/公开值）" "重新生成：openssl rand -hex 16，再填进 deploy/.env"
  [ "$OPEN_XIAOAI_TOKEN" != "$OPEN_XIAOAI_WEB_TOKEN" ] \
    || fail "$name" "OPEN_XIAOAI_WEB_TOKEN 与 OPEN_XIAOAI_TOKEN 相同" "两个 Token 必须不同（否则 Web 泄露即等于 4399 泄露）；各用 openssl rand -hex 16 生成一个"
  [ -n "$HASS_TOKEN" ] \
    || fail "$name" "HASS_TOKEN 为空" "在 HA（个人资料 → 长期访问令牌）生成一个填进 deploy/.env；首道关口可用占位值，但 HA 控制会一直死，直到填真值"
  [ -n "$NAS_IP" ] \
    || fail "$name" "NAS_IP 为空" "在 deploy/.env 里填 NAS 内网 IP（如 192.168.1.10）"
  if ! printf '%s' "$NAS_IP" | grep -Eq '^[A-Za-z0-9.-]+$'; then
    fail "$name" "NAS_IP 格式不对：$NAS_IP" "只允许字母/数字/./-，如 192.168.1.10"
  fi
  [ -n "$MUSIC_FOLDER" ] \
    || fail "$name" "MUSIC_FOLDER 为空" "在 deploy/.env 里填 NAS 上的音乐目录（如 /volume1/music；网易云直连是主路，本地歌只是兜底）"
  if ! printf '%s' "$HUB_PORT" | grep -Eq '^[0-9]+$' || [ "$HUB_PORT" -lt 1 ] || [ "$HUB_PORT" -gt 65535 ]; then
    fail "$name" "HUB_PORT 不合法：$HUB_PORT" "填 1–65535 的端口号，默认 4399（4400 = 与旧 Hub 并行，不抢 4399）"
  fi
  if [ -z "$LLM_API_KEY" ]; then
    info "LLM_API_KEY 为空——按本地 OVMS 处理，不阻塞（LLM 对话走本地模型；如需 DeepSeek/Qwen 云端模型再填）"
  else
    info "LLM_API_KEY 已填（长度 ${#LLM_API_KEY}，值不打印）"
  fi
  pass "$name" "Tokens 强度通过（OPEN_XIAOAI_TOKEN ${#OPEN_XIAOAI_TOKEN} 位、OPEN_XIAOAI_WEB_TOKEN ${#OPEN_XIAOAI_WEB_TOKEN} 位、互不相同）；NAS_IP=$NAS_IP，MUSIC_FOLDER=$MUSIC_FOLDER，HUB_PORT=$HUB_PORT"
}

# ---- 端口占用：ss 优先，/dev/tcp 兜底 ----
port_in_use() { # $1=端口（在目标上查）
  local p="$1"
  if run_target_sh "command -v ss >/dev/null 2>&1"; then
    run_target_sh "ss -tln 2>/dev/null | grep -q ':$p[[:space:]]'"
  else
    run_target_sh "timeout 2 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/$p' 2>/dev/null"
  fi
}
# 目标上正在跑的容器名（逗号分隔；docker 不可用时为空）
running_containers() {
  run_target_sh "docker ps --format '{{.Names}}' 2>/dev/null | tr '\n' ','" 2>/dev/null || true
}

# ================= 3. 端口 =================
check_ports() {
  local name="ports"
  [ "$SKIP_PORTS" = "1" ] && { skip "$name" "经 --skip-ports / PREFLIGHT_SKIP_PORTS=1 跳过"; return; }
  # HUB_PORT 来自 check_env（.env/环境变量）；--skip-env 时用默认 4399。
  local hub_port="${HUB_PORT:-4399}"
  echo "---- [3/$TOTAL_CHECKS] 端口（目标：$TARGET_DESC；HUB_PORT=$hub_port）"
  local p others="8081 3000 10300 10200 10400 1704 1705 6600"
  local busy="" containers
  # HUB_PORT：被占时必须分清“旧 Hub”与“未知进程”。
  if port_in_use "$hub_port"; then
    containers="$(running_containers)"
    case "$containers" in
      *oxn-hub*|*open-xiaoai*hub*|*openxiaoai*hub*)
        warn "$name" "端口 $hub_port 已被旧 Hub 占用（容器：$containers）——割接时是预期的：up 前先停旧 Hub，或改 HUB_PORT=4400 并行（client.toml 的 hub_url 端口要同步改）"
        busy="$busy $hub_port(old-hub)"
        ;;
      *)
        if [ -n "$containers" ]; then
          fail "$name" "端口 $hub_port 已被未知进程占用（目标上跑着的容器：$containers，其中没有旧 Hub）" "先查谁占了它（目标上跑：ss -tlnp | grep $hub_port），停掉或改 HUB_PORT；割接时旧 Hub 应显示为 oxn-hub"
        else
          fail "$name" "端口 $hub_port 已被未知进程占用（目标上没有跑着的 Docker 容器，所以不是旧 Hub）" "先查谁占了它（目标上跑：ss -tlnp | grep $hub_port），停掉或改 HUB_PORT"
        fi
        ;;
    esac
  else
    info "端口 $hub_port 空闲"
  fi
  # 其他端口：只报告，不 FAIL（割接时旧栈可能还占着它们；up 失败会再响亮报错）。
  for p in $others; do
    if [ "$p" = "$hub_port" ]; then continue; fi
    if port_in_use "$p"; then
      containers="$(running_containers)"
      warn "$name" "端口 $p 已被占用（目标上跑着的容器：${containers:-无}）——up 之后对应服务可能起不来，先确认是不是旧栈的同名服务"
      busy="$busy $p"
    else
      info "端口 $p 空闲"
    fi
  done
  if [ -n "$busy" ]; then
    pass "$name" "端口已核对（被占：$busy ；其余空闲；HUB_PORT 未被未知进程占用）"
  else
    pass "$name" "全部 9 个端口空闲（$hub_port 8081 3000 10300 10200 10400 1704 1705 6600）"
  fi
}

# ================= 4. 镜像 =================
# compose 里的 image: 行 + 三个 Dockerfile 的 FROM 行。
required_images() {
  grep -E '^[[:space:]]*image:[[:space:]]*' "$COMPOSE_FILE" 2>/dev/null \
    | sed -e 's/^[[:space:]]*image:[[:space:]]*//' -e 's/[[:space:]]*#.*//' -e 's/[[:space:]]*$//'
  for df in "$HUB_DOCKERFILE" "$KWS_DOCKERFILE" "$WHISPER_DOCKERFILE"; do
    [ -f "$df" ] || continue
    grep -E '^FROM[[:space:]]' "$df" 2>/dev/null | awk '{print $2}'
  done
}
check_images() {
  local name="images"
  [ "$SKIP_IMAGES" = "1" ] && { skip "$name" "经 --skip-images / PREFLIGHT_SKIP_IMAGES=1 跳过"; return; }
  echo "---- [4/$TOTAL_CHECKS] 镜像（目标：$TARGET_DESC）"
  [ -f "$COMPOSE_FILE" ] \
    || fail "$name" "找不到 $COMPOSE_FILE" "确认在仓库根目录跑本脚本（PREFLIGHT_ROOT 指向仓库根）"
  # 两个本地构建的输入文件（构建上下文缺了 up 必 fail，见 nas-install.sh P2-55）。
  [ -f "$HUB_DOCKERFILE" ] \
    || fail "$name" "缺 deploy/Dockerfile.hub（hub 是本地构建，缺了 up 会失败）" "确认 deploy/Dockerfile.hub 在仓库里"
  [ -f "$ROOT/deploy/hub-linux-amd64" ] \
    || fail "$name" "缺 deploy/hub-linux-amd64（预编译 Hub 二进制，Dockerfile.hub 要 COPY 它）" "先在 hub/ 下 go build -o deploy/hub-linux-amd64 ./cmd/hub，再重编"
  for f in "$KWS_DOCKERFILE" "$ROOT/kws-sidecar/server.py" "$ROOT/kws-sidecar/entrypoint.sh" "$ROOT/kws-sidecar/requirements.txt"; do
    [ -f "$f" ] \
      || fail "$name" "缺 $f（kws-sidecar 是本地构建，缺了 up 会失败）" "确认 kws-sidecar/ 构建源文件都在仓库里"
  done
  # whisper 也是本地构建（2026-11-21 换 Paraformer）：Dockerfile.whisper
  # 缺了 up 会失败；底座 rhasspy/wyoming-whisper 由上面的 required_images
  # 经 FROM 行收进镜像检查。
  [ -f "$WHISPER_DOCKERFILE" ] \
    || fail "$name" "缺 deploy/Dockerfile.whisper（whisper 是本地构建，缺了 up 会失败）" "确认 deploy/Dockerfile.whisper 在仓库里"
  local img manifest_out
  while IFS= read -r img; do
    [ -n "$img" ] || continue
    if run_target docker image inspect "$img" >/dev/null 2>&1; then
      info "镜像已在目标本地：$img"
      continue
    fi
    # 本地没有：看仓库里能不能拉到（只读查询，不拉取）。
    if command -v timeout >/dev/null 2>&1; then
      manifest_out="$(run_target timeout 30 docker manifest inspect "$img" 2>&1)" \
        && { info "镜像本地暂无，但仓库可拉取：$img"; continue; }
    else
      manifest_out="$(run_target docker manifest inspect "$img" 2>&1)" \
        && { info "镜像本地暂无，但仓库可拉取：$img"; continue; }
    fi
    case "$manifest_out" in
      *experimental*|*"manifest"*"not supported"*|*"unknown command"*)
        warn "$name" "无法验证 $img 是否可拉取（目标 docker 不支持 manifest inspect）；安装时会试 docker compose pull，失败再处理"
        ;;
      *)
        fail "$name" "镜像缺失且仓库里找不到：$img" "本地没有、manifest inspect 也失败（可能镜像名/ digest 错了，或目标没网/连不上 registry）。先在目标上跑 docker compose pull 看具体报错；manifest 输出：$(printf '%s' "$manifest_out" | head -c 300)"
        ;;
    esac
  done <<EOF
$(required_images)
EOF
  pass "$name" "构建输入齐全（Dockerfile.hub + hub-linux-amd64 + kws-sidecar 源文件 + Dockerfile.whisper）；其余镜像本地已有或仓库可拉取"
}

# ================= 5. 证书种子 =================
find_certs_dir() {
  if [ -n "$CERTS_DIR_OVERRIDE" ]; then printf '%s' "$CERTS_DIR_OVERRIDE"; return 0; fi
  local d
  for d in "$ROOT/deploy/hub-certs" "$ROOT/release/flash-kit/hub-certs" \
           "$ROOT/../release/flash-kit/hub-certs" "$ROOT/../../release/flash-kit/hub-certs"; do
    if [ -f "$d/ca.crt" ] && [ -f "$d/ca.key" ] && [ -f "$d/hub.crt" ] && [ -f "$d/hub.key" ]; then
      printf '%s' "$d"; return 0
    fi
  done
  return 1
}
find_client_toml() {
  if [ -n "$CLIENT_TOML_OVERRIDE" ]; then printf '%s' "$CLIENT_TOML_OVERRIDE"; return 0; fi
  local f
  for f in "$ROOT/deploy/client.toml" "$ROOT/deploy/client-oh2p.toml" \
           "$ROOT/release/flash-kit/client-oh2p.toml" "$ROOT/release/flash-kit/client.toml" \
           "$ROOT/../release/flash-kit/client-oh2p.toml" "$ROOT/../../release/flash-kit/client-oh2p.toml"; do
    [ -f "$f" ] && { printf '%s' "$f"; return 0; }
  done
  return 1
}
# 归一化指纹：去冒号/空格，转小写。
norm_fp() { printf '%s' "$1" | tr -d ':[:space:]-' | tr 'A-Z' 'a-z'; }
check_certs() {
  local name="certs"
  [ "$SKIP_CERTS" = "1" ] && { skip "$name" "经 --skip-certs / PREFLIGHT_SKIP_CERTS=1 跳过"; return; }
  echo "---- [5/$TOTAL_CHECKS] Hub TLS 证书种子"
  local certs_dir
  certs_dir="$(find_certs_dir)" \
    || fail "$name" "找不到 Hub TLS 种子目录（ca.crt/ca.key/hub.crt/hub.key 四个文件齐全的目录）" "把 flash-kit/hub-certs 拷到 deploy/hub-certs，或设 HUB_CERTS_DIR=<种子目录>；搜过的位置：deploy/hub-certs、release/flash-kit/hub-certs 及其上两级。没种子就 up 的话，Hub 会生成全新 CA，client.toml 的 hub_ca_fingerprint 与音箱 mTLS 证书同时失效"
  local missing=""
  for f in ca.crt ca.key hub.crt hub.key; do
    [ -f "$certs_dir/$f" ] || missing="$missing $f"
  done
  [ -z "$missing" ] \
    || fail "$name" "证书种子不全（$certs_dir 缺：$missing）" "补齐四个文件（gen-certs.sh 的产物），或重做一套后同步更新 client.toml 的 hub_ca_fingerprint"
  command -v openssl >/dev/null 2>&1 \
    || fail "$name" "本机找不到 openssl，无法验签" "先装 openssl"
  openssl verify -CAfile "$certs_dir/ca.crt" "$certs_dir/hub.crt" >/dev/null 2>&1 \
    || fail "$name" "hub.crt 不是 ca.crt 签发的（openssl verify 失败）" "检查种子是不是同一套 gen-certs.sh 产物（ca.crt 与 hub.crt 是否配对）；错配的种子 up 后 Hub 与音箱 mTLS 握手会失败"
  if ! diff <(openssl x509 -in "$certs_dir/hub.crt" -pubkey -noout 2>/dev/null) \
            <(openssl pkey -in "$certs_dir/hub.key" -pubout 2>/dev/null) >/dev/null 2>&1; then
    fail "$name" "hub.key 与 hub.crt 不匹配（公钥对不上）" "检查种子是不是同一套产物；错配的密钥 Hub 启动即报错"
  fi
  local fp_raw fp
  fp_raw="$(openssl x509 -in "$certs_dir/hub.crt" -noout -fingerprint -sha256 2>/dev/null)" \
    || fail "$name" "读 hub.crt 指纹失败" "确认 hub.crt 是有效的 PEM 证书"
  fp="$(norm_fp "${fp_raw#*=}")"
  [ "${#fp}" -eq 64 ] \
    || fail "$name" "hub.crt 指纹格式不对：$fp_raw" "确认 hub.crt 是 SHA-256 证书"
  if ! openssl x509 -in "$certs_dir/hub.crt" -noout -checkend 2592000 >/dev/null 2>&1; then
    warn "$name" "hub.crt 30 天内过期（种子证书将到期，轮换后要同步更新 client.toml 的 hub_ca_fingerprint）"
  fi
  # 指纹比对：优先 client.toml，其次种子目录里的 fingerprints.txt。
  local toml expected_toml
  if toml="$(find_client_toml)"; then
    expected_toml="$(grep -E '^[[:space:]]*hub_ca_fingerprint[[:space:]]*=' "$toml" 2>/dev/null | head -1 \
      | sed -e 's/^[^=]*=//' -e 's/["'"'"']//g' -e 's/[[:space:]]//g')"
    expected_toml="$(norm_fp "$expected_toml")"
    if [ -z "$expected_toml" ]; then
      warn "$name" "$toml 里没有 hub_ca_fingerprint（种子指纹 $fp；刷机前先写进 client.toml，否则音箱 TLS 握手会被拒）"
    elif [ "${#expected_toml}" -ne 64 ] || ! printf '%s' "$expected_toml" | grep -Eq '^[0-9a-f]{64}$'; then
      fail "$name" "$toml 里的 hub_ca_fingerprint 格式不对：$expected_toml" "应为 64 位 hex（可带冒号），从种子 hub.crt 的 SHA-256 指纹取：$fp"
    elif [ "$expected_toml" != "$fp" ]; then
      fail "$name" "client.toml 的 hub_ca_fingerprint 与种子 hub.crt 指纹不一致（client.toml：$expected_toml；种子 hub.crt：$fp）" "以种子为准：把 $fp 写进 $toml 的 hub_ca_fingerprint（或换一套与 client.toml 匹配的种子）；不一致时音箱 TLS 握手会被拒"
    else
      info "client.toml（$toml）的 hub_ca_fingerprint 与种子 hub.crt 一致"
    fi
  else
    local expected_txt=""
    if [ -f "$certs_dir/fingerprints.txt" ]; then
      expected_txt="$(grep -E '^[[:space:]]*hub:' "$certs_dir/fingerprints.txt" 2>/dev/null | head -1 | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/[[:space:]]//g')"
      expected_txt="$(norm_fp "$expected_txt")"
    fi
    if [ -n "$expected_txt" ] && [ "$expected_txt" != "$fp" ]; then
      fail "$name" "种子 fingerprints.txt 里的 hub 指纹与 hub.crt 不一致（txt：$expected_txt；hub.crt：$fp）" "以 hub.crt 为准更新 fingerprints.txt 与 client.toml"
    fi
    warn "$name" "没找到 client.toml（种子指纹 $fp；刷机前务必把它写进 client.toml 的 hub_ca_fingerprint，否则音箱 TLS 握手会被拒）"
  fi
  pass "$name" "证书种子齐全且配对（$certs_dir）；hub.crt SHA-256 指纹 $fp"
}

# ================= 6. 音箱可达性 =================
check_speaker() {
  local name="speaker"
  [ "$SKIP_SPEAKER" = "1" ] && { skip "$name" "经 --skip-speaker / PREFLIGHT_SKIP_SPEAKER=1 跳过"; return; }
  echo "---- [6/$TOTAL_CHECKS] 音箱可达性（只 ping/TCP，不登录）"
  if [ -z "${SPEAKER_IP:-}" ]; then
    # 未设 IP 不是失败：音箱可能还没准备好；设了 IP 但连不上才是失败。
    # （nas-install.sh 的门禁也会跑本脚本——不能让没填 SPEAKER_IP 就
    # 堵住一次纯 NAS 部署；想查时填 IP 再跑，或跑 preflight --skip-speaker。）
    skip "$name" "SPEAKER_IP 未设置（在 deploy/.env 里写 SPEAKER_IP=<音箱 LAN IP>，或用 --speaker-ip <IP>，再跑本项）"
    return
  fi
  local ip="$SPEAKER_IP" ok=0
  if command -v ping >/dev/null 2>&1; then
    if [ "$(uname -s)" = "Darwin" ]; then
      ping -c 1 -t 3 "$ip" >/dev/null 2>&1 && ok=1
    else
      ping -c 1 -W 3 "$ip" >/dev/null 2>&1 && ok=1
    fi
  fi
  if [ "$ok" -eq 0 ]; then
    # TCP 探针：优先 nc -z（可被测试垫片替换；本机沙箱会代接任意出站 TCP，
    # 故 /dev/tcp 在本机不可靠，只作无 nc 时的兜底）。
    if command -v nc >/dev/null 2>&1; then
      nc -z -w 3 "$ip" 22 >/dev/null 2>&1 && ok=1
    elif command -v timeout >/dev/null 2>&1; then
      timeout 3 bash -c "cat < /dev/null > /dev/tcp/$ip/22" 2>/dev/null && ok=1
    else
      bash -c "cat < /dev/null > /dev/tcp/$ip/22" 2>/dev/null && ok=1
    fi
  fi
  [ "$ok" -eq 1 ] \
    || fail "$name" "音箱 $ip 不可达（ping 与 TCP :22 均失败）" "确认音箱已上电、在同一局域网、IP 没变（OH2P 看 NAS ARP 表或路由器）；本脚本不登录音箱，只测可达性"
  pass "$name" "音箱 $ip 可达（ping 或 TCP :22 通；未登录，未做任何操作）"
}

# ================= 7. 配置 =================
check_config() {
  local name="config"
  [ "$SKIP_CONFIG" = "1" ] && { skip "$name" "经 --skip-config / PREFLIGHT_SKIP_CONFIG=1 跳过"; return; }
  echo "---- [7/$TOTAL_CHECKS] 配置 YAML"
  [ -f "$CONFIG_FILE" ] \
    || fail "$name" "找不到 $CONFIG_FILE" "确认在仓库根目录跑本脚本（PREFLIGHT_ROOT 指向仓库根）"
  [ -f "$COMPOSE_FILE" ] \
    || fail "$name" "找不到 $COMPOSE_FILE" "确认在仓库根目录跑本脚本"
  if command -v python3 >/dev/null 2>&1 && python3 -c "import yaml" 2>/dev/null; then
    python3 - "$CONFIG_FILE" <<'PY' || fail "$name" "hub/config.yaml 解析失败（见上 python 报错）" "修 YAML 语法（注意缩进用空格、不要用 Tab；冒号后要空格）"
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f)
assert isinstance(cfg, dict) and cfg, "config.yaml 为空或顶层不是 mapping"
assert isinstance(cfg.get("hub"), dict) and cfg["hub"].get("listen"), "config.yaml 缺 hub.listen"
assert cfg.get("wake_word"), "config.yaml 缺 wake_word"
PY
    python3 - "$COMPOSE_FILE" <<'PY' || fail "$name" "deploy/docker-compose.yml 解析失败（见上 python 报错）" "修 YAML 语法（注意缩进用空格、不要用 Tab）"
import sys, yaml
with open(sys.argv[1]) as f:
    c = yaml.safe_load(f)
assert isinstance(c, dict) and isinstance(c.get("services"), dict), "docker-compose.yml 缺 services"
PY
  else
    fail "$name" "本机没有 python3 + PyYAML，无法校验 YAML" "先装 python3 与 PyYAML（pip install pyyaml）；或用 --skip-config 跳过本项（不推荐）"
  fi
  local svc missing_svc=""
  for svc in hub whisper piper kws-sidecar snapserver mpd netease-api; do
    grep -Eq "^  $svc:" "$COMPOSE_FILE" || missing_svc="$missing_svc $svc"
  done
  [ -z "$missing_svc" ] \
    || fail "$name" "docker-compose.yml 缺服务：$missing_svc" "确认 compose 里 7 个服务都在（hub/whisper/piper/kws-sidecar/snapserver/mpd/netease-api）"
  pass "$name" "hub/config.yaml 与 deploy/docker-compose.yml 均可解析；compose 含 7 个服务"
}

# ================= 主流程（fail-fast：任一 fail() 即退出）====
check_docker
check_env
check_ports
check_images
check_certs
check_speaker
check_config

echo
echo "preflight: 全部通过（PASS=$PASS_COUNT SKIP=$SKIP_COUNT，目标：$TARGET_DESC；HUB_PORT=${HUB_PORT:-4399}）"
[ "$SKIP_COUNT" -gt 0 ] && echo "preflight: 注意：有 $SKIP_COUNT 项被跳过，部署前请确认跳过是有意的"
exit 0
