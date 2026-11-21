#!/usr/bin/env bash
# nas-install.sh — open-xiaoai-next NAS 部署脚本（UGREEN DXP6800 Pro + UGOS）
#
# 用法（在本机 open-xiaoai-next/ 目录下跑）：
#   1. cp deploy/.env.example deploy/.env，填好 NAS_IP / HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN / OPEN_XIAOAI_WEB_TOKEN
#   2. NAS_SSH=huajun@192.168.88.247 ./deploy/nas-install.sh        # 只传文件 + 检查，不 up
#      NAS_SSH=huajun@192.168.88.247 ./deploy/nas-install.sh up     # 传文件 + up + 逐项检查
#   部署时端口（HUB_PORT）：默认 4399（停掉老 Hub、新 Hub 直接接管 4399，走切换路线）。
#   若部署时决定新 Hub 与老 Hub 并行（老 Hub 继续占 4399），则设 HUB_PORT=4400
#   （或其它空闲端口），脚本会把 config.yaml 的 listen 改过去；音箱端 client.toml
#   的 hub_url 端口也要改成同一个值（见 release/flash-kit/CHECKLIST.md）。
#
# 依赖：本机有 scp/ssh；NAS 上已开 SSH、已装 Docker（含 docker compose 插件）。
# 注意：本脚本只在本机做过 bash -n 语法校验，NAS 上实际运行未验证（关口 2 BLOCKED）。
# 任何一步 FAIL 都先停下来，把输出贴回来再改，不要跳过。
set -u
cd "$(dirname "$0")/.." || exit 1   # 切到 open-xiaoai-next/

NAS_SSH="${NAS_SSH:-}"                       # 如 huajun@192.168.88.247（必填）
NAS_DIR="${NAS_DIR:-/volume4/docker/open-xiaoai-next}"  # NAS 上的部署目录（2026-11-19：Docker 根目录是 /volume4/docker，不是 /volume1/docker）
NAS_IP="${NAS_IP:-}"                         # 如 192.168.88.247（不填则从 deploy/.env 读）
HUB_PORT="${HUB_PORT:-4399}"                 # Hub 监听端口，部署时定：4399=停老 Hub 直接接管；其它值=与老 Hub 并行
CLIENT_CERT_FINGERPRINT="${CLIENT_CERT_FINGERPRINT:-97c1ed53e3cbd112cbbe217a0efd3400b519bd2ad331108dbf2c911d9cfd2103}"  # 音箱 mTLS 客户端证书的 SHA-256 指纹（LX06 与 OH2P 共用同一张证书）。部署时注入 NAS 端 config.yaml 的 discovery_fingerprints（随仓库发的 config.yaml 里保持为空，见 TestShippedConfigRegistrationDefaults）。若客户端证书重新生成，须同步更新此值（openssl x509 -in client.crt -noout -fingerprint -sha256）。
DO_UP="${1:-}"                               # 传 up 才执行 docker compose up

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $*"; }
info() { echo "---- $*"; }

# 强 Token 校验（P0-3 / FW-P1-1：与 hub/internal/transport.ValidateToken 同规则）：
# 长度 <16、或等于 CHANGE_ME/secret/password 等常见值即拒绝部署。
check_strong_token() {
  local name="$1" tok="$2" lower weak=0
  lower="$(printf '%s' "$tok" | tr 'A-Z' 'a-z')"
  [ "${#tok}" -lt 16 ] && weak=1
  case "$lower" in
    change_me|changeme|secret|password|123456|12345678|123456789|1234567890|admin|letmein|qwerty|open-xiaoai|open_xiaoai|xiaoai|test|token|0000000000000000|aaaaaaaaaaaaaaaa) weak=1 ;;
  esac
  if [ "$weak" -eq 1 ]; then
    bad "$name 太弱（<16 位或为公开/常见值，Hub 启动会直接拒绝）"
    echo "生成一个强 Token 再填进 deploy/.env：openssl rand -hex 16"
    exit 1
  else
    ok "$name 强度通过（≥16 位，非常见值）"
  fi
}

[ -n "$NAS_SSH" ] || { echo "FAIL: 请先设置 NAS_SSH，如 NAS_SSH=huajun@192.168.88.247 $0 up"; exit 1; }
if [ -z "$NAS_IP" ] && [ -f deploy/.env ]; then
  NAS_IP="$(grep -E '^NAS_IP=' deploy/.env | cut -d= -f2 | tr -d '[:space:]')"
fi
[ -n "$NAS_IP" ] || { echo "FAIL: NAS_IP 未设置（环境变量或 deploy/.env 里的 NAS_IP）"; exit 1; }
# HUB_PORT：部署时定的 Hub 监听端口（默认 4399=停老 Hub 直接接管；其它值=并行）。
# 只允许 1-65535 的数字。
if ! [[ "$HUB_PORT" =~ ^[0-9]+$ ]] || [ "$HUB_PORT" -lt 1 ] || [ "$HUB_PORT" -gt 65535 ]; then
  echo "FAIL: HUB_PORT 格式不对：$HUB_PORT（应为 1-65535 的数字，如 4399 或 4400）"; exit 1
fi
# P2-24：NAS_IP / NAS_DIR / NAS_SSH 未校验即拼进 `$SSH "…"`——注入面。
# 只允许安全字符集，含 shell 元字符（; $ ` \ " ' ( ) < > | & 空格等）即拒绝。
if ! [[ "$NAS_IP" =~ ^[A-Za-z0-9.-]+$ ]]; then
  echo "FAIL: NAS_IP 格式不对：$NAS_IP（只允许字母/数字/./-，如 192.168.88.247）"; exit 1
fi
if ! [[ "$NAS_DIR" =~ ^/[A-Za-z0-9._/-]+$ ]] || [[ "$NAS_DIR" =~ \.\. ]]; then
  echo "FAIL: NAS_DIR 格式不对：$NAS_DIR（只允许绝对路径安全字符，不含 ..）"; exit 1
fi
if ! [[ "$NAS_SSH" =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+$ ]]; then
  echo "FAIL: NAS_SSH 格式不对：$NAS_SSH（应为 user@host，如 huajun@192.168.88.247）"; exit 1
fi

# ---------- preflight 门禁（feat/preflight：deploy/preflight.sh）----------
# 选择：在 nas-install.sh 开头加门禁，不另起 deploy/install.sh 包装器——
# nas-install.sh 本身就是唯一的一键部署入口（NAS_CHECKLIST.md 与
# deploy/README.md 都这么写），再包一层只会多一个入口、检查逻辑还与
# 下面 §0 的预检查重叠。preflight 失败即停在传文件之前。
# preflight 自己会读 NAS_SSH：已设时 docker/端口/镜像查 NAS，否则查本机；
# .env/配置/证书种子永远查本机（将要传过去的文件）。
# 紧急绕过：SKIP_PREFLIGHT=1（会明确打印；只应在已知原因时用）。
export NAS_SSH
if [ "${SKIP_PREFLIGHT:-0}" != "1" ]; then
  info "preflight 门禁（deploy/preflight.sh；失败即停，不传文件）"
  if [ -x deploy/preflight.sh ]; then
    ./deploy/preflight.sh \
      || { echo "preflight 未通过：先修好上面 FAIL 的项再部署（单项可用 --skip-* 跳过，见 ./deploy/preflight.sh --help；紧急绕过：SKIP_PREFLIGHT=1）"; exit 1; }
  else
    echo "WARN: 找不到可执行的 deploy/preflight.sh，跳过门禁（直接跑 §0 预检查）"
  fi
else
  echo "WARN: SKIP_PREFLIGHT=1，preflight 门禁已跳过（后果自负）"
fi

SSH="ssh -o BatchMode=yes -o ConnectTimeout=10 $NAS_SSH"
SCP="scp -o BatchMode=yes -o ConnectTimeout=10"

# ---------- 0. 本机预检查 ----------
info "0. 本机预检查"
[ -f deploy/hub-linux-amd64 ]        && ok "Hub 二进制存在 deploy/hub-linux-amd64" || bad "缺 deploy/hub-linux-amd64，先在 hub/ 下 go build"
# P2-55：Hub 二进制悄悄过期——清单 deploy/hub-linux-amd64.sha256 在时
# 比对 SHA256 与大小，不一致即 FAIL（重编后忘更新清单，或二进制被换过）；
# 清单不存在时只警告（无记录可核对）。清单格式（由重编步骤生成）：
#   <sha256>  deploy/hub-linux-amd64
#   size: <字节数>
if [ -f deploy/hub-linux-amd64 ] && [ -f deploy/hub-linux-amd64.sha256 ]; then
  _hub_sha="$(sha256sum deploy/hub-linux-amd64 | cut -d' ' -f1)"
  _hub_size="$(stat -L -c %s deploy/hub-linux-amd64)"
  if grep -qx "$_hub_sha  deploy/hub-linux-amd64" deploy/hub-linux-amd64.sha256 \
     && grep -qx "size: $_hub_size" deploy/hub-linux-amd64.sha256; then
    ok "Hub 二进制与 deploy/hub-linux-amd64.sha256 清单一致（$_hub_size 字节）"
  else
    bad "Hub 二进制与 deploy/hub-linux-amd64.sha256 清单不符（重编后忘更新清单，或二进制被换过）"
  fi
elif [ -f deploy/hub-linux-amd64 ]; then
  echo "WARN: 无 deploy/hub-linux-amd64.sha256 清单，无法核对 Hub 二进制是否最新（重编后用 sha256sum 生成清单，见 docs/RELEASE.md）"
fi
[ -f deploy/docker-compose.yml ]     && ok "docker-compose.yml 存在" || bad "缺 deploy/docker-compose.yml"
[ -f deploy/Dockerfile.hub ]          && ok "Dockerfile.hub 存在" || bad "缺 deploy/Dockerfile.hub"
[ -f deploy/snapserver.conf ]         && ok "snapserver.conf 存在" || bad "缺 deploy/snapserver.conf"
[ -f deploy/mpd.conf ]                && ok "mpd.conf 存在" || bad "缺 deploy/mpd.conf（MPD 的 fifo 输出配置，缺了点歌不出声）"
[ -f hub/config.yaml ]                && ok "hub/config.yaml 存在" || bad "缺 hub/config.yaml"
python3 -c "import yaml; yaml.safe_load(open('deploy/docker-compose.yml'))" \
  && ok "docker-compose.yml YAML 语法 OK" || bad "docker-compose.yml YAML 语法错误"
if [ -f deploy/.env ]; then
  ok ".env 存在"
  for k in HASS_TOKEN LLM_API_KEY OPEN_XIAOAI_TOKEN OPEN_XIAOAI_WEB_TOKEN; do
    v="$(grep -E "^${k}=" deploy/.env | cut -d= -f2- || true)"
    # 2026-11-19 bai 定：对话用本地 OVMS Qwen3.5-4B（NAS 9000/v3，无需 key），
    # 所以 LLM_API_KEY 为空是正常的（只在改用云端模型时才需要填），不算 FAIL，
    # 只提示一声；其它三个 Token 为空才是真缺失。
    if [ "$k" = "LLM_API_KEY" ] && [ -z "$v" ]; then
      echo "WARN: .env 里 LLM_API_KEY 为空（正常：对话走本地 OVMS Qwen3.5-4B，无需 key；改用云端模型时才需填）"
    else
      [ -n "$v" ] && ok ".env 里 $k 已填" || bad ".env 里 $k 为空"
    fi
  done
  # 环境变量优先于 .env（部署时以环境变量为准时）。
  _tok_client="${OPEN_XIAOAI_TOKEN:-$(grep -E '^OPEN_XIAOAI_TOKEN=' deploy/.env | cut -d= -f2- || true)}"
  _tok_web="${OPEN_XIAOAI_WEB_TOKEN:-$(grep -E '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env | cut -d= -f2- || true)}"
  check_strong_token "OPEN_XIAOAI_TOKEN" "$_tok_client"
  # FW-P1-1：Web Token 与 Client Token 独立校验（同一弱值规则）。
  check_strong_token "OPEN_XIAOAI_WEB_TOKEN" "$_tok_web"
  if [ "$_tok_client" = "$_tok_web" ]; then
    bad "OPEN_XIAOAI_WEB_TOKEN 与 OPEN_XIAOAI_TOKEN 相同（两个 Token 必须不同，否则 Web 泄露即等于 4399 泄露）"
    exit 1
  fi
else
  bad "缺 deploy/.env（先 cp deploy/.env.example deploy/.env 再填）"
fi
[ "$FAIL" -gt 0 ] && { echo "本机预检查有 FAIL，先修好再继续。"; exit 1; }

# ---------- 1. NAS 连通与环境检查 ----------
info "1. NAS 连通与环境检查"
$SSH true && ok "SSH 连通 $NAS_SSH" || { bad "SSH 连不上 $NAS_SSH（检查 NAS 是否开 SSH、IP 对不对）"; exit 1; }
$SSH "docker --version && docker compose version" \
  && ok "NAS 上 docker + docker compose 可用" || bad "NAS 上 docker / docker compose 不可用"
# 2026-11-18 融合 B：music_folder 是可选本地兜底，不存在只警告、
# 不记 FAIL、不退出（网易云直连是主路；Hub 侧 ValidateStartup 的
# Fatal 也要由 Hub 面改掉，见 MUSIC_FUSION.md“待 Hub 面确认”）。
$SSH "[ -d /volume1/Music ]" \
  && ok "/volume1/Music 存在" || echo "WARN: /volume1/Music 不存在（可选本地兜底，缺了只警告；想放本地歌时在 UGOS 文件管理里建，并放几首歌）"
$SSH "[ \"\$(ls -A /volume1/Music 2>/dev/null | head -1)\" ]" \
  && ok "/volume1/Music 非空" || echo "WARN: /volume1/Music 是空的（MPD 库会是空的，先放几首歌；不影响网易云直连）"
# P1-4：1883 已随 mosquitto 删除而去掉；Hub 主端口（$HUB_PORT，默认 4399）与 1704/1705
# （Snapcast 流/控制）补上——之前漏 4399 时 Hub 绑定失败脚本还报全部空闲。
# 2026-11-18 融合 B：8095 已随 Music Assistant 删除而去掉（不再占用）。
# 2026-11-19：Hub 端口是部署时定的（HUB_PORT）。HUB_PORT=4399（默认，停老 Hub 直接接管）时
# 4399 被占是正常的——部署前要先停老 Hub；HUB_PORT 为其它值（并行）时 4399 被占是预期的，
# 只要求 $HUB_PORT 本身空闲。下面只警告不记 FAIL、不退出，具体看 WARN 内容再定。
for p in 10300 10200 10400 "$HUB_PORT" 1704 1705 3000 8081; do
  if $SSH "ss -tln 2>/dev/null | grep -q \":$p \""; then
    echo "WARN: 端口 $p 已被占用（up 之后对应服务可能起不来，先查谁占了：ss -tlnp | grep $p）"
  else
    ok "端口 $p 空闲"
  fi
done

# ---------- 2. 传文件 ----------
info "2. 传文件到 $NAS_SSH:$NAS_DIR"
$SSH "mkdir -p $NAS_DIR/deploy $NAS_DIR/hub" \
  && ok "NAS 目录已建 $NAS_DIR/deploy + $NAS_DIR/hub" || { bad "NAS 上建目录失败"; exit 1; }
$SCP deploy/docker-compose.yml deploy/Dockerfile.hub deploy/hub-linux-amd64 \
      deploy/snapserver.conf deploy/mpd.conf deploy/.env "$NAS_SSH:$NAS_DIR/deploy/" \
  && ok "部署文件已传到 $NAS_DIR/deploy/" || { bad "scp 传部署文件失败"; exit 1; }
# P2-55：kws-sidecar 的构建上下文不在 deploy/ 里——compose 里它是
# build: ../kws-sidecar，且无 image:，只传 deploy/ 的话 NAS 上
# docker compose up 会因构建上下文缺失而失败。只传构建所需文件
# （Dockerfile、server.py、entrypoint.sh、requirements.txt、scripts/；
# tests/ 与 .venv 不传）到 $NAS_DIR/kws-sidecar。
if [ -d kws-sidecar ]; then
  tar -czf /tmp/kws-sidecar-src.tar.gz -C . \
      kws-sidecar/Dockerfile kws-sidecar/server.py kws-sidecar/entrypoint.sh \
      kws-sidecar/requirements.txt kws-sidecar/scripts \
    || { bad "打包 kws-sidecar 构建源文件失败"; exit 1; }
  $SCP /tmp/kws-sidecar-src.tar.gz "$NAS_SSH:$NAS_DIR/" \
    && $SSH "mkdir -p $NAS_DIR/kws-sidecar && tar -xzf $NAS_DIR/kws-sidecar-src.tar.gz -C $NAS_DIR && rm -f $NAS_DIR/kws-sidecar-src.tar.gz" \
    && ok "kws-sidecar 构建源文件已传到 $NAS_DIR/kws-sidecar/" \
    || { bad "传 kws-sidecar 构建源文件失败"; exit 1; }
  rm -f /tmp/kws-sidecar-src.tar.gz
else
  bad "缺 kws-sidecar/ 目录（compose 里边车是 build: ../kws-sidecar，缺了 up 会失败）"
  exit 1
fi
# P1-33：种子配置只在首次部署写。NAS 上已有 config.yaml 时不覆盖——NAS 端经
# 语音认领的房间与 Web 面板改的配置都在这个文件里，重传本机旧副本会全部丢掉。
# 已有文件先在 NAS 端做带时间戳的备份（.bak.YYYYMMDDHHMMSS），再跳过传文件。
if $SSH "[ -f $NAS_DIR/hub/config.yaml ]"; then
  _bak="$NAS_DIR/hub/config.yaml.bak.$(date +%Y%m%d%H%M%S)"
  $SSH "cp $NAS_DIR/hub/config.yaml $_bak" \
    && ok "NAS 上已有 config.yaml，不覆盖；已备份为 $_bak（第 3 步只做 sed 调整）" \
    || { bad "备份 NAS 端 config.yaml 失败"; exit 1; }
else
  $SCP hub/config.yaml "$NAS_SSH:$NAS_DIR/hub/config.yaml" \
    && ok "hub/config.yaml 已传到 $NAS_DIR/hub/（首次部署，种子配置）" \
    || { bad "scp 传 config.yaml 失败"; exit 1; }
fi
# P1-4：Hub 容器以 uid 1000（appuser）运行，config.yaml 须属主 1000，
# 否则 Hub 读不到配置。每次部署都 chown（幂等）。
$SSH "chown 1000:1000 $NAS_DIR/hub/config.yaml" \
  && ok "config.yaml 属主已设为 1000:1000（Hub 容器 appuser 可读）" \
  || { bad "chown config.yaml 失败"; exit 1; }

# ---------- 3. NAS 端 config 调整 ----------
# Hub 跑在 host 网络下，Docker 容器名（netease-api / kws-sidecar）解析不到，
# 改成 127.0.0.1（这些服务都把端口映射到了宿主机，或与 Hub 一样走 host 网络）；HA 地址改成真实 NAS_IP。
# 2026-11-18 融合 B：music-assistant 已砍，不再改写 8095 地址；
# hub/config.yaml 里 netease.music_assistant 一行由 Hub 面删除（种子配置已删）；
# 但 NAS 上旧副本可能还留着该行——新 Hub 读到它会直接拒绝启动（config.go 的
# checkDeprecatedMusicAssistant），所以第 3 步用 sed 删掉该行并断言已删。
# kws.url 那行是防旧副本：随仓库发的 config.yaml 已是 ws://127.0.0.1:10400/v1/kws，
# 旧副本若还是 ws://kws-sidecar:10400 也会在这里被改过来（否则 Hub 启动即报“唤醒服务暂不可用”）。
info "3. NAS 端 config 调整（只改 NAS 上的副本，不动本机 hub/config.yaml）"
# P1-33：这里不再 cp .bak——备份在第 2 步做过了（带时间戳的才是旧文件备份；
# 以前这里 cp 的是刚传上去的本机文件，备错了文件）。
# FW-P1-2：sed 后逐项断言结果——以前 sed 静默无操作、grep 只打印不比对，
# HA 地址没改也会照常 ok。
# 2026-11-19：Hub 监听端口是部署时定的（HUB_PORT，默认 4399=停老 Hub 直接接管；
# 其它值=与老 Hub 并行）。种子 config.yaml 的 listen 是 ":4399"，这里按 HUB_PORT 改写，
# 并断言改写结果；音箱端 client.toml 的 hub_url 端口必须与这里一致（见 CHECKLIST）。
# agents 与 music_folder 的改写是防旧副本：种子已是正确值（NAS 的 LAN IP、/volume1/Music），
# 但 NAS 上旧副本可能还是容器名 /volume1/music，也在这里一并改过来并断言。
# 指纹注入（部署时值，幂等）：随仓库发的 config.yaml 里 discovery_fingerprints
# 为空；这里把 CLIENT_CERT_FINGERPRINT 注入 NAS 端副本（已存在则跳过），
# 再由下面第 3 步的 grep 断言确认。指纹是部署时才定的值，不写进仓库。
$SSH "grep -q '$CLIENT_CERT_FINGERPRINT' $NAS_DIR/hub/config.yaml || sed -i 's#^[[:space:]]*discovery_fingerprints:\$#  discovery_fingerprints:\n    - \"$CLIENT_CERT_FINGERPRINT\"#' $NAS_DIR/hub/config.yaml" \
  && ok "discovery_fingerprints 已含客户端证书指纹" \
  || bad "注入 discovery_fingerprints 指纹失败"
$SSH "sed -i 's#http://netease-api:3000#http://127.0.0.1:3000#' $NAS_DIR/hub/config.yaml
  sed -i 's#ws://kws-sidecar:10400#ws://127.0.0.1:10400#' $NAS_DIR/hub/config.yaml
  sed -i \"s#192.168.1.10:8123#$NAS_IP:8123#\" $NAS_DIR/hub/config.yaml
  sed -i \"s#http://hermes:8642#http://$NAS_IP:8642#\" $NAS_DIR/hub/config.yaml
  sed -i \"s#http://openclaw:18789#http://$NAS_IP:18789#\" $NAS_DIR/hub/config.yaml
  sed -i 's#/volume1/music#/volume1/Music#' $NAS_DIR/hub/config.yaml
  sed -i 's#listen: \":4399\"#listen: \":'$HUB_PORT'\"#' $NAS_DIR/hub/config.yaml
  sed -i '/^[[:space:]]*music_assistant:/d' $NAS_DIR/hub/config.yaml
  grep -q 'http://127.0.0.1:3000' $NAS_DIR/hub/config.yaml \
    && grep -q 'ws://127.0.0.1:10400' $NAS_DIR/hub/config.yaml \
    && grep -q \"://$NAS_IP:8123\" $NAS_DIR/hub/config.yaml \
    && grep -q \"http://$NAS_IP:8642/v1\" $NAS_DIR/hub/config.yaml \
    && grep -q \"http://$NAS_IP:18789/v1\" $NAS_DIR/hub/config.yaml \
    && grep -q 'music_folder: /volume1/Music' $NAS_DIR/hub/config.yaml \
    && grep -q \"listen: \\\":$HUB_PORT\\\"\" $NAS_DIR/hub/config.yaml \
    && grep -q '$CLIENT_CERT_FINGERPRINT' $NAS_DIR/hub/config.yaml \
    && ! grep -q 'music_assistant:' $NAS_DIR/hub/config.yaml \
    && grep -E 'api:|url:|kws:|  url:' $NAS_DIR/hub/config.yaml" \
  && ok "config.yaml 已改：netease.api 走 127.0.0.1:3000，kws 走 ws://127.0.0.1:10400/v1/kws，HA 走 $NAS_IP:8123，agents 走 $NAS_IP:8642 / $NAS_IP:18789，music_folder 是 /volume1/Music，Hub listen 是 :$HUB_PORT，discovery_fingerprints 含客户端证书指纹 97c1ed53…（以上改写均已断言；已废弃的 music_assistant 行如有也已删——新 Hub 读到该行会直接拒绝启动）" \
  || bad "config.yaml 调整失败（sed 后断言不通过：HA/agents/music_folder/listen/指纹白名单任一改写未生效，或 music_assistant 行没删掉，见上）"

[ "$DO_UP" != "up" ] && {
  echo; echo "文件已传好，未执行 up。如需启动：NAS_SSH=$NAS_SSH $0 up"
  echo "汇总：PASS=$PASS FAIL=$FAIL"; exit 0
}

# ---------- 4. 拉镜像并启动 ----------
info "4. docker compose pull + up"
$SSH "cd $NAS_DIR/deploy && docker compose pull" \
  && ok "镜像拉取完成" || bad "镜像拉取失败（看是哪一个镜像报错；netease-api 已改为 binaryify/netease_cloud_music_api）"
# P1-4：先构建 hub、kws-sidecar 与 whisper（三者都是 build:，pull 拉不到），
# 再做卷属主迁移——已存在的数据卷属主还是 root（旧版容器以 root 跑过），
# 新版容器以 uid 1000 运行会写不进卷。up 之前用 root 一次性 chown
# 两个具名卷的挂载点（只改属主，不动数据；首次部署卷是空的，这一步
# 是空操作；新卷的首次 copy-up 由 Dockerfile 里的 mkdir + chown 负责）。
$SSH "cd $NAS_DIR/deploy && docker compose build hub kws-sidecar whisper" \
  && ok "Hub、kws-sidecar 与 whisper 镜像已构建" || { bad "构建 hub/kws-sidecar/whisper 镜像失败"; exit 1; }
$SSH "cd $NAS_DIR/deploy && docker compose run --rm --user root --no-deps --entrypoint /bin/chown hub -R 1000:1000 /app/hub-data" \
  && ok "hub-data 卷属主已设为 1000:1000" || { bad "hub-data 卷 chown 失败"; exit 1; }
$SSH "cd $NAS_DIR/deploy && docker compose run --rm --user root --no-deps --entrypoint /bin/chown kws-sidecar -R 1000:1000 /data/kws/models" \
  && ok "kws models 卷属主已设为 1000:1000" || { bad "kws models 卷 chown 失败"; exit 1; }
$SSH "cd $NAS_DIR/deploy && docker compose up -d" \
  && ok "docker compose up -d 已执行" || { bad "up 失败，把报错贴回来"; exit 1; }
# 只等 15 秒让各容器完成启动——这一步不等 whisper 的模型：
# whisper 首次启动要下载 funasr/paraformer-zh（约 850MB，分钟级，
# 见 NAS_CHECKLIST.md §2.2“等 3 分钟”），下载在容器后台进行；
# 服务端启动即绑 10300 端口（NAS 实测：空数据卷首启 5 秒内端口
# 已通，Ready 日志在模型加载完后才出），所以下面的 Up 计数与
# 端口检查在下载进行中也能通过；15 秒只是启动沉降，不是模型就绪等待。
sleep 15

# P2-24：带 Web Token 的检查不用本地拼命令——token 经本地 shell 拼进
# `$SSH "…"` 会被远端 shell 再解析一次，token 里的特殊字符会破环命令，
# 且 token 会出现在本机进程参数里。remote_curl_api 把检查脚本用带引号
# 的 heredoc（不展开）喂给 NAS 端 bash -s，NAS_DIR 与 API 路径作 $1/$2
# 传进去；token 在 NAS 端从已传过去的 deploy/.env 里读（与
# NAS_CHECKLIST.md §3 的写法一致）。本机只收到响应体 + HTTP 码，不
# 打印 token、不把 token 写进日志。
# 2026-11-19：面板改绑 NAS 内网 IP，健康检查改调 192.168.88.247:8081
#（此前 127.0.0.1:8081 已不监听）。
remote_curl_api() { # $1=API 路径，如 /api/status
  $SSH "bash -s -- $NAS_DIR $1" <<'REMOTE_EOF'
tok="$(grep -E '^OPEN_XIAOAI_WEB_TOKEN=' "$1/deploy/.env" | cut -d= -f2-)"
curl -s -m 5 -w ' %{http_code}' -H "Authorization: Bearer $tok" "http://192.168.88.247:8081$2"
REMOTE_EOF
}

# ---------- 5. 逐项检查 ----------
info "5. 逐项检查"
# FW-P1-2 / FW-P2-7：以前只打印 ps、不数数，ps 成功即 ok；现在数 Up 的容器数，
# 必须为 7 个（mosquitto 已删，P1-37；Music Assistant 已删，2026-11-18 融合 B）。
$SSH "cd $NAS_DIR/deploy && docker compose ps --format '{{.Name}} {{.Status}}'" \
  || bad "docker compose ps 失败"
_up="$($SSH "cd $NAS_DIR/deploy && docker compose ps --format '{{.Status}}' | grep -c '^Up'" 2>/dev/null)"
if [ "$_up" = "7" ]; then
  ok "7 个容器都 Up（hub/whisper/piper/kws-sidecar/snapserver/mpd/netease-api；kws-sidecar 首次启动要下载约 5.6MB 唤醒模型，Up 之前会多等一阵）"
else
  bad "Up 的容器是 ${_up:-?} 个，不是 7 个（见上 ps 输出，查哪个容器没起来）"
fi
# FW-P1-2：端口表与 checklist §3 对齐——补 Hub 主端口（$HUB_PORT，默认 4399）与 10400（边车），
# 1883 随 mosquitto 删除而去掉，8095 随 Music Assistant 删除而去掉（融合 B）；
# 8081 走下面的 HTTP 检查，不在这里测 TCP。
for p in 10300 10200 10400 "$HUB_PORT" 3000; do
  $SSH "timeout 3 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/$p'" 2>/dev/null \
    && ok "端口 $p 通" || bad "端口 $p 不通"
done
$SSH "ss -tln 2>/dev/null | grep -q ':1705 '" \
  && ok "Snapcast 控制端口 1705 在监听（host 网络）" || bad "1705 不在监听（snapserver 没起来）"
$SSH "ss -tln 2>/dev/null | grep -q ':1704 '" \
  && ok "Snapcast 流端口 1704 在监听" || bad "1704 不在监听"
# 2026-11-18 融合 B：MA Web 检查已删（Music Assistant 容器已砍，8095 不再存在）。
# P1-34：以前 curl 只要连上（哪怕 404/500）即 ok；现在断言 HTTP 200。
$SSH "curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/" | grep -q 200 \
  && ok "NeteaseCloudMusicApi 正常（http://127.0.0.1:3000/ 返回 200）" || bad "netease-api 不正常（/ 返回的不是 200）"
# 融合 B 新增：Hub 网易云登录态检查（GET /api/netease/status，带 Web Token）。
# Token 由 remote_curl_api 在 NAS 端从 deploy/.env 读，不经本地 shell（P2-24）。
_netease_status="$(remote_curl_api /api/netease/status 2>/dev/null)"
case "$_netease_status" in
  *'"logged_in":true'*200|*'"logged_in": true'*200) ok "网易云已登录（/api/netease/status 返回 200，logged_in:true）" ;;
  *'"logged_in":false'*200|*'"logged_in": false'*200) echo "WARN: 网易云未登录（/api/netease/status 返回 200，logged_in:false；首次部署是正常的，去 Hub Web 面板扫码登录，见 NAS_CHECKLIST.md §5）" ;;
  *200) ok "Hub 网易云状态接口可达（/api/netease/status 返回 200；登录态以 Hub Web 面板为准，未登录时去面板扫码）" ;;
  *404) echo "WARN: Hub 还没接 /api/netease/status（返回 404，Hub 面待确认）；登录态改去 Hub Web 面板看，部署继续" ;;
  *401) bad "Hub /api/netease/status 返回 401（OPEN_XIAOAI_WEB_TOKEN 不对或没传进容器，见 .env 与 compose）" ;;
  *) echo "WARN: Hub /api/netease/status 未返回预期结果（实际：${_netease_status:-无响应}）；先看 Hub 日志，登录态改去 Hub Web 面板确认" ;;
esac
# KWS 边车健康检查：模型加载好才返回 200（模型缺失/下载失败时容器报错退出，
# 这里会直接不通；见 deploy/NAS_CHECKLIST.md §3 的 10400 行）。
# FW-P1-2：以前只看 curl 退出码——/health 返回 503 时 curl 照样 exit 0；
# 现在断言响应含 model_loaded:true（进程活着但模型未加载时不算通过）。
$SSH "curl -s -m 5 http://127.0.0.1:10400/health" | grep -q '"model_loaded":true' \
  && ok "KWS 边车 /health 正常（model_loaded:true，模型已加载）" \
  || bad "KWS 边车 /health 不正常（应为 200 + model_loaded:true；看 docker compose logs kws-sidecar，首次启动下载模型失败会明确报错退出）"
# FW-P1-1：带 Web Token 调 /api/status，断言 HTTP 200 且响应含 wake_word。
# 以前无 token 的 curl 对 401 也 exit 0、恒 PASS；现在 401 即 FAIL。
# Token 由 remote_curl_api 在 NAS 端从 deploy/.env 读，不经本地 shell（P2-24）。
_status="$(remote_curl_api /api/status 2>/dev/null)"
case "$_status" in
  *wake_word*200) ok "Hub Web 面板正常（/api/status 返回 200，含 wake_word 你好小李）" ;;
  *) bad "Hub /api/status 不正常（应为 200 + wake_word；401 说明 OPEN_XIAOAI_WEB_TOKEN 不对或没传进容器，见 .env 与 compose；实际：${_status:-无响应}）" ;;
esac
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=50 hub 2>&1 | grep -q '已启动'" \
  && ok "Hub 日志出现“已启动”（唤醒词/监听端口见日志）" || bad "Hub 日志里没有“已启动”，看 docker compose logs hub"
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=50 hub 2>&1 | grep -q 'KWS 边车健康检查通过'" \
  && ok "Hub 日志出现“KWS 边车健康检查通过”（Hub 已连上 127.0.0.1:10400 的边车，语音唤醒可用）" \
  || bad "Hub 日志里没有“KWS 边车健康检查通过”（看 docker compose logs hub + logs kws-sidecar；/api/status 的 kws_status 也会是 down）"
# P1-44：自动发现扫描已删，改为注册制——设备在 WS 上发 register 主动注册，
# Hub 启动时必打一行“设备注册制：…”日志。查这行，不再查“自动发现/扫描”。
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=30 hub 2>&1 | grep -q '设备注册制'" \
  && ok "Hub 日志出现“设备注册制”行（P1-44 注册制已启用）" || echo "WARN: Hub 日志里没看到“设备注册制”行（看完整日志；旧版 Hub 才没有这行）"
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=100 2>&1 | grep -qiE 'fatal|panic'" \
  && bad "有容器日志出现 FATAL/PANIC，看 docker compose logs" || ok "无 FATAL/PANIC 日志"

echo
echo "汇总：PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "有 FAIL 项：先看 deploy/NAS_CHECKLIST.md 的故障速查，再把对应日志贴回来。"
  exit 1
fi
echo "全部检查通过。下一步：去 Hub Web 面板扫码登录网易云（见 deploy/NAS_CHECKLIST.md §5；Music Assistant 界面已废弃）。"
