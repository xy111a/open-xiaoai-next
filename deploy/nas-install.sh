#!/usr/bin/env bash
# nas-install.sh — open-xiaoai-next NAS 部署脚本（UGREEN DXP6800 Pro + UGOS）
#
# 用法（在本机 open-xiaoai-next/ 目录下跑）：
#   1. cp deploy/.env.example deploy/.env，填好 NAS_IP / HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN / OPEN_XIAOAI_WEB_TOKEN
#   2. NAS_SSH=root@192.168.1.10 ./deploy/nas-install.sh        # 只传文件 + 检查，不 up
#      NAS_SSH=root@192.168.1.10 ./deploy/nas-install.sh up     # 传文件 + up + 逐项检查
#
# 依赖：本机有 scp/ssh；NAS 上已开 SSH、已装 Docker（含 docker compose 插件）。
# 注意：本脚本只在本机做过 bash -n 语法校验，NAS 上实际运行未验证（关口 2 BLOCKED）。
# 任何一步 FAIL 都先停下来，把输出贴回来再改，不要跳过。
set -u
cd "$(dirname "$0")/.." || exit 1   # 切到 open-xiaoai-next/

NAS_SSH="${NAS_SSH:-}"                       # 如 root@192.168.1.10（必填）
NAS_DIR="${NAS_DIR:-/volume1/docker/open-xiaoai-next}"  # NAS 上的部署目录
NAS_IP="${NAS_IP:-}"                         # 如 192.168.1.10（不填则从 deploy/.env 读）
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

[ -n "$NAS_SSH" ] || { echo "FAIL: 请先设置 NAS_SSH，如 NAS_SSH=root@192.168.1.10 $0 up"; exit 1; }
if [ -z "$NAS_IP" ] && [ -f deploy/.env ]; then
  NAS_IP="$(grep -E '^NAS_IP=' deploy/.env | cut -d= -f2 | tr -d '[:space:]')"
fi
[ -n "$NAS_IP" ] || { echo "FAIL: NAS_IP 未设置（环境变量或 deploy/.env 里的 NAS_IP）"; exit 1; }

SSH="ssh -o BatchMode=yes -o ConnectTimeout=10 $NAS_SSH"
SCP="scp -o BatchMode=yes -o ConnectTimeout=10"

# ---------- 0. 本机预检查 ----------
info "0. 本机预检查"
[ -f deploy/hub-linux-amd64 ]        && ok "Hub 二进制存在 deploy/hub-linux-amd64" || bad "缺 deploy/hub-linux-amd64，先在 hub/ 下 go build"
[ -f deploy/docker-compose.yml ]     && ok "docker-compose.yml 存在" || bad "缺 deploy/docker-compose.yml"
[ -f deploy/Dockerfile.hub ]          && ok "Dockerfile.hub 存在" || bad "缺 deploy/Dockerfile.hub"
[ -f deploy/snapserver.conf ]         && ok "snapserver.conf 存在" || bad "缺 deploy/snapserver.conf"
[ -f hub/config.yaml ]                && ok "hub/config.yaml 存在" || bad "缺 hub/config.yaml"
python3 -c "import yaml; yaml.safe_load(open('deploy/docker-compose.yml'))" \
  && ok "docker-compose.yml YAML 语法 OK" || bad "docker-compose.yml YAML 语法错误"
if [ -f deploy/.env ]; then
  ok ".env 存在"
  for k in HASS_TOKEN LLM_API_KEY OPEN_XIAOAI_TOKEN OPEN_XIAOAI_WEB_TOKEN; do
    v="$(grep -E "^${k}=" deploy/.env | cut -d= -f2- || true)"
    [ -n "$v" ] && ok ".env 里 $k 已填" || bad ".env 里 $k 为空"
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
$SSH "[ -d /volume1/music ]" \
  && ok "/volume1/music 存在" || bad "/volume1/music 不存在（先在 UGOS 文件管理里建，并放几首歌）"
$SSH "[ \"\$(ls -A /volume1/music 2>/dev/null | head -1)\" ]" \
  && ok "/volume1/music 非空" || echo "WARN: /volume1/music 是空的（MPD 库会是空的，先放几首歌）"
# P1-4：1883 已随 mosquitto 删除而去掉；4399（Hub 主端口）与 1704/1705
# （Snapcast 流/控制）补上——之前漏 4399 时 Hub 绑定失败脚本还报全部空闲。
for p in 10300 10200 10400 4399 1704 1705 8095 3000 8080; do
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
      deploy/snapserver.conf deploy/.env "$NAS_SSH:$NAS_DIR/deploy/" \
  && ok "部署文件已传到 $NAS_DIR/deploy/" || { bad "scp 传部署文件失败"; exit 1; }
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

# ---------- 3. NAS 端 config 调整 ----------
# Hub 跑在 host 网络下，Docker 容器名（music-assistant / netease-api / kws-sidecar）解析不到，
# 改成 127.0.0.1（这些服务都把端口映射到了宿主机，或与 Hub 一样走 host 网络）；HA 地址改成真实 NAS_IP。
# kws.url 那行是防旧副本：随仓库发的 config.yaml 已是 ws://127.0.0.1:10400/v1/kws，
# 旧副本若还是 ws://kws-sidecar:10400 也会在这里被改过来（否则 Hub 启动即报“唤醒服务暂不可用”）。
info "3. NAS 端 config 调整（只改 NAS 上的副本，不动本机 hub/config.yaml）"
# P1-33：这里不再 cp .bak——备份在第 2 步做过了（带时间戳的才是旧文件备份；
# 以前这里 cp 的是刚传上去的本机文件，备错了文件）。
# FW-P1-2：sed 后逐项断言结果——以前 sed 静默无操作、grep 只打印不比对，
# HA 地址没改也会照常 ok。
$SSH "sed -i 's#http://music-assistant:8095#http://127.0.0.1:8095#' $NAS_DIR/hub/config.yaml
  sed -i 's#http://netease-api:3000#http://127.0.0.1:3000#' $NAS_DIR/hub/config.yaml
  sed -i 's#ws://kws-sidecar:10400#ws://127.0.0.1:10400#' $NAS_DIR/hub/config.yaml
  sed -i \"s#192.168.1.10:8123#$NAS_IP:8123#\" $NAS_DIR/hub/config.yaml
  grep -q 'http://127.0.0.1:8095' $NAS_DIR/hub/config.yaml \
    && grep -q 'http://127.0.0.1:3000' $NAS_DIR/hub/config.yaml \
    && grep -q 'ws://127.0.0.1:10400' $NAS_DIR/hub/config.yaml \
    && grep -q \"://$NAS_IP:8123\" $NAS_DIR/hub/config.yaml \
    && grep -E 'music_assistant:|api:|url:|kws:|  url:' $NAS_DIR/hub/config.yaml" \
  && ok "config.yaml 已改：netease 走 127.0.0.1，kws 走 ws://127.0.0.1:10400/v1/kws，HA 走 $NAS_IP:8123（保持原有 ws/wss 协议，四处改写均已断言）" \
  || bad "config.yaml 调整失败（sed 后断言不通过：HA 地址或 127.0.0.1 改写未生效，见上）"

[ "$DO_UP" != "up" ] && {
  echo; echo "文件已传好，未执行 up。如需启动：NAS_SSH=$NAS_SSH $0 up"
  echo "汇总：PASS=$PASS FAIL=$FAIL"; exit 0
}

# ---------- 4. 拉镜像并启动 ----------
info "4. docker compose pull + up"
$SSH "cd $NAS_DIR/deploy && docker compose pull" \
  && ok "镜像拉取完成" || bad "镜像拉取失败（看是哪一个镜像报错；netease-api 已改为 binaryify/netease_cloud_music_api）"
$SSH "cd $NAS_DIR/deploy && docker compose up -d" \
  && ok "docker compose up -d 已执行" || { bad "up 失败，把报错贴回来"; exit 1; }
sleep 15   # 等 Whisper 首次加载模型、Hub 启动

# ---------- 5. 逐项检查 ----------
info "5. 逐项检查"
# FW-P1-2 / FW-P2-7：以前只打印 ps、不数数，ps 成功即 ok；现在数 Up 的容器数，
# 必须为 8 个（mosquitto 已删，P1-37）。
$SSH "cd $NAS_DIR/deploy && docker compose ps --format '{{.Name}} {{.Status}}'" \
  || bad "docker compose ps 失败"
_up="$($SSH "cd $NAS_DIR/deploy && docker compose ps --format '{{.Status}}' | grep -c '^Up'" 2>/dev/null)"
if [ "$_up" = "8" ]; then
  ok "8 个容器都 Up（hub/whisper/piper/kws-sidecar/snapserver/mpd/music-assistant/netease-api；kws-sidecar 首次启动要下载约 5.6MB 唤醒模型，Up 之前会多等一阵）"
else
  bad "Up 的容器是 ${_up:-?} 个，不是 8 个（见上 ps 输出，查哪个容器没起来）"
fi
# FW-P1-2：端口表与 checklist §3 对齐——补 4399（Hub 主端口）与 10400（边车），
# 1883 随 mosquitto 删除而去掉；8080 走下面的 HTTP 检查，不在这里测 TCP。
for p in 10300 10200 10400 4399 8095 3000; do
  $SSH "timeout 3 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/$p'" 2>/dev/null \
    && ok "端口 $p 通" || bad "端口 $p 不通"
done
$SSH "ss -tln 2>/dev/null | grep -q ':1705 '" \
  && ok "Snapcast 控制端口 1705 在监听（host 网络）" || bad "1705 不在监听（snapserver 没起来）"
$SSH "ss -tln 2>/dev/null | grep -q ':1704 '" \
  && ok "Snapcast 流端口 1704 在监听" || bad "1704 不在监听"
$SSH "curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:8095" | grep -q 200 \
  && ok "Music Assistant Web 正常（http://$NAS_IP:8095 返回 200）" || bad "MA Web 不正常"
# P1-34：以前 curl 只要连上（哪怕 404/500）即 ok；现在断言 HTTP 200。
$SSH "curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/" | grep -q 200 \
  && ok "NeteaseCloudMusicApi 正常（http://127.0.0.1:3000/ 返回 200）" || bad "netease-api 不正常（/ 返回的不是 200）"
# KWS 边车健康检查：模型加载好才返回 200（模型缺失/下载失败时容器报错退出，
# 这里会直接不通；见 deploy/NAS_CHECKLIST.md §3 的 10400 行）。
# FW-P1-2：以前只看 curl 退出码——/health 返回 503 时 curl 照样 exit 0；
# 现在断言响应含 model_loaded:true（进程活着但模型未加载时不算通过）。
$SSH "curl -s -m 5 http://127.0.0.1:10400/health" | grep -q '"model_loaded":true' \
  && ok "KWS 边车 /health 正常（model_loaded:true，模型已加载）" \
  || bad "KWS 边车 /health 不正常（应为 200 + model_loaded:true；看 docker compose logs kws-sidecar，首次启动下载模型失败会明确报错退出）"
# FW-P1-1：带 Web Token 调 /api/status，断言 HTTP 200 且响应含 wake_word。
# 以前无 token 的 curl 对 401 也 exit 0、恒 PASS；现在 401 即 FAIL。
# Token 取环境变量 OPEN_XIAOAI_WEB_TOKEN，优先于 .env（与 §0 预检查同规则）。
_web_tok="${OPEN_XIAOAI_WEB_TOKEN:-$(grep -E '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env 2>/dev/null | cut -d= -f2- || true)}"
_status="$($SSH "curl -s -m 5 -w ' %{http_code}' -H 'Authorization: Bearer $_web_tok' http://127.0.0.1:8080/api/status" 2>/dev/null)"
case "$_status" in
  *wake_word*200) ok "Hub Web 面板正常（/api/status 返回 200，含 wake_word 你好小李）" ;;
  *) bad "Hub /api/status 不正常（应为 200 + wake_word；401 说明 OPEN_XIAOAI_WEB_TOKEN 不对或没传进容器，见 .env 与 compose；实际：${_status:-无响应}）" ;;
esac
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=50 hub 2>&1 | grep -q '已启动'" \
  && ok "Hub 日志出现“已启动”（唤醒词/监听端口见日志）" || bad "Hub 日志里没有“已启动”，看 docker compose logs hub"
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=50 hub 2>&1 | grep -q 'KWS 边车健康检查通过'" \
  && ok "Hub 日志出现“KWS 边车健康检查通过”（Hub 已连上 127.0.0.1:10400 的边车，语音唤醒可用）" \
  || bad "Hub 日志里没有“KWS 边车健康检查通过”（看 docker compose logs hub + logs kws-sidecar；/api/status 的 kws_status 也会是 down）"
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=30 hub 2>&1 | grep -qi '自动发现\|扫描\|discover'" \
  && ok "Hub 日志出现自动发现相关行" || echo "WARN: Hub 日志里没看到自动发现关键词（可能还没扫到设备，先看完整日志）"
$SSH "cd $NAS_DIR/deploy && docker compose logs --tail=100 2>&1 | grep -qiE 'fatal|panic'" \
  && bad "有容器日志出现 FATAL/PANIC，看 docker compose logs" || ok "无 FATAL/PANIC 日志"

echo
echo "汇总：PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "有 FAIL 项：先看 deploy/NAS_CHECKLIST.md 的故障速查，再把对应日志贴回来。"
  exit 1
fi
echo "全部检查通过。下一步：按 deploy/NAS_CHECKLIST.md 做网易云扫码登录（MA 界面点 QR Login）。"
