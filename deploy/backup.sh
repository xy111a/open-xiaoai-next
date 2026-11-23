#!/usr/bin/env bash
# backup.sh — open-xiaoai-next NAS 部署备份
#
# 用法（在 NAS 的部署目录里跑，或在本机跑再传上去）：
#   cd deploy && ./backup.sh
#   # 或指定备份目录：
#   OXN_BACKUP_DIR=/volume1/backups ./backup.sh
#
# 备份内容（见 deploy/BACKUP.md）：
#   具名卷 hub-data / whisper-data / piper-data / kws-data / mpd-data
#   （snapfifo 只有 fifo 文件、无持久数据时跳过并在 manifest 里注明）
#   + hub/config.yaml + deploy/.env + deploy/docker-compose.yml
#   + deploy/snapserver.conf。
#   TLS 证书（hub-data/tls/）与网易云 Cookie（hub-data/netease-cookie.json）
#   在卷里，随 hub-data 一起备份；manifest 里单独列出，供恢复时核对。
#
# 一致性策略：备份前先 `docker compose stop` 停掉整个栈（最简单的正确做法——
# 停了就没人写卷，快照不可能撕裂），打完包立刻 `docker compose start` 恢复。
# 不做在线热备：Hub/MPD/KWS 随时可能写卷，live tar 抓到半写文件恢复出来
# 是坏的，而 stop/start 的停机只有几十秒（备份本身不下行、无需等模型加载）。
# 若脚本中途失败，trap 会尽力把服务重新 start 起来。
#
# 安全：备份归档里含 deploy/.env（HASS_TOKEN / LLM_API_KEY / 两个 Token），
# 是机密文件——只记“已包含 + 变量个数”，绝不打印任何 Token 值；归档要妥善
# 保管、不要分享、不要提交进仓库（deploy/backups/ 已在 .gitignore 里）。
#
# 环境变量（一般不用改；测试缝见 deploy/test_backup_restore.sh）：
#   OXN_COMPOSE_FILE  compose 文件（默认 本目录/docker-compose.yml）
#   OXN_CONFIG_FILE   Hub 配置（默认 ../hub/config.yaml）
#   OXN_ENV_FILE      环境变量文件（默认 本目录/.env）
#   OXN_BACKUP_DIR    备份归档存放目录（默认 本目录/backups）
#   OXN_VOLUME_ROOT   设了它就用本地目录 $OXN_VOLUME_ROOT/<卷名> 代替 Docker 卷
#                     （只给测试用；不设时走 docker run + helper 镜像）
#   DOCKER_BIN        docker 命令（默认 docker）
#   OXN_HELPER_IMAGE  打包/解包卷用的 helper 镜像（默认 busybox:latest）
#
# 退出码：0 成功；1 失败（FAIL 行里写清检查名 + 原因 + 修复办法）。
# 注意：本脚本只做过 bash -n 与 mock 测试，真 NAS 上的实测见 deploy/BACKUP.md。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${OXN_COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.yml}"
CONFIG_FILE="${OXN_CONFIG_FILE:-$SCRIPT_DIR/../hub/config.yaml}"
ENV_FILE="${OXN_ENV_FILE:-$SCRIPT_DIR/.env}"
BACKUP_DIR="${OXN_BACKUP_DIR:-$SCRIPT_DIR/backups}"
VOLUME_ROOT="${OXN_VOLUME_ROOT:-}"
DOCKER_BIN="${DOCKER_BIN:-docker}"
HELPER_IMAGE="${OXN_HELPER_IMAGE:-busybox:latest}"
TOOL_VERSION=1

# 与 deploy/docker-compose.yml 的 volumes: 段一致（snapfifo 单独处理，见下）。
VOLUMES=(hub-data whisper-data piper-data kws-data mpd-data)
SNAPFIFO="snapfifo"

STOPPED=0
RESTARTED=0
STAGE=""

die()  { echo "FAIL: $*" >&2; exit 1; }
info() { echo "---- $*"; }
note() { echo "NOTE: $*"; }
ok()   { echo "PASS: $*"; }

compose() { "$DOCKER_BIN" compose -f "$COMPOSE_FILE" "$@"; }

# 失败/中断时尽力把服务恢复起来（stop 之后、start 之前挂了才需要）。
restart_stack() {
  if [ "$STOPPED" -eq 1 ] && [ "$RESTARTED" -eq 0 ]; then
    echo "---- 备份中断，尝试恢复启动服务"
    if compose start >/dev/null 2>&1; then
      RESTARTED=1
      echo "PASS: 服务已恢复启动（备份失败，但栈已恢复）"
    else
      echo "FAIL: 服务恢复启动失败——手动跑：docker compose -f $COMPOSE_FILE start" >&2
    fi
  fi
  if [ -n "$STAGE" ] && [ -d "$STAGE" ]; then
    rm -rf "$STAGE"
  fi
}
trap restart_stack EXIT

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

# 具名卷在 Docker 里的实际名字（compose 会加项目名前缀，如 deploy_hub-data）。
# 先问 compose，问不到再按“compose 文件所在目录名_卷名”拼。
resolve_volume() {
  local short="$1" name=""
  name="$(compose config --volumes 2>/dev/null | grep -E "(^|_)${short}$" | head -n1 || true)"
  if [ -z "$name" ]; then
    name="$(basename "$(dirname "$COMPOSE_FILE")")_${short}"
  fi
  printf '%s' "$name"
}

# 卷存在吗？（docker run -v 不存在的卷名会自动建一个空卷，备份前必须先拦住。）
volume_exists() {
  local short="$1"
  if [ -n "$VOLUME_ROOT" ]; then
    [ -d "$VOLUME_ROOT/$short" ]
  else
    "$DOCKER_BIN" volume inspect "$(resolve_volume "$short")" >/dev/null 2>&1
  fi
}

# 卷里有没有持久文件（regular file）？snapfifo 只有 fifo/空时返回 1。
volume_has_regular_files() {
  local short="$1"
  if [ -n "$VOLUME_ROOT" ]; then
    [ -d "$VOLUME_ROOT/$short" ] && find "$VOLUME_ROOT/$short" -type f -print -quit | grep -q .
  else
    "$DOCKER_BIN" run --rm -v "$(resolve_volume "$short"):/src:ro" "$HELPER_IMAGE" \
      sh -c 'find /src -type f -print -quit | grep -q .' >/dev/null 2>&1
  fi
}

# 把一个卷打成 staging/volumes/<卷名>.tar.gz。
snapshot_volume() {
  local short="$1"
  if [ -n "$VOLUME_ROOT" ]; then
    tar -czf "$STAGE/volumes/$short.tar.gz" -C "$VOLUME_ROOT/$short" . \
      || die "打包卷 $short 失败（tar 报错，见上）"
  else
    "$DOCKER_BIN" run --rm \
      -v "$(resolve_volume "$short"):/src:ro" \
      -v "$STAGE:/dst" \
      "$HELPER_IMAGE" \
      tar -czf "/dst/volumes/$short.tar.gz" -C /src . \
      || die "打包卷 $short 失败（拉取 $HELPER_IMAGE 失败，或卷不存在；先确认卷名：docker volume ls）"
  fi
}

# tar.gz 里的非目录条目数（即文件数）。
tar_file_count() { tar -tzf "$1" | grep -vc '/$'; }
# tar.gz 里的文件字节总数（未压缩）。
tar_total_bytes() { tar -tvzf "$1" 2>/dev/null | awk '{s+=$3} END {print s+0}'; }
file_bytes() { wc -c < "$1" | tr -d ' '; }
file_sha256() { sha256sum "$1" | cut -d' ' -f1; }

# ---------- 0. 预检查 ----------
info "0. 预检查"
[ -f "$COMPOSE_FILE" ] || die "compose 文件缺失：$COMPOSE_FILE（在 deploy/ 目录下跑，或设 OXN_COMPOSE_FILE）"
[ -f "$CONFIG_FILE" ]  || die "Hub 配置缺失：$CONFIG_FILE（先确认部署目录对不对）"
[ -f "$ENV_FILE" ]     || die ".env 缺失：$ENV_FILE（先 cp .env.example .env 再填；恢复时没有它起不来）"
for v in "${VOLUMES[@]}"; do
  volume_exists "$v" || die "卷 $v 不存在（docker volume ls 里找不到；还没 up 过的话先 up 一次再备份）"
done
ok "预检查通过（compose/config/.env 存在，5 个卷都存在）"
mkdir -p "$BACKUP_DIR" || die "建备份目录失败：$BACKUP_DIR（权限或磁盘问题）"

# ---------- 1. 停栈（保证快照一致） ----------
info "1. 停止服务（docker compose stop，备份完立刻 start）"
compose stop || die "停止服务失败（docker compose stop 报错；备份未开始，服务状态未变）"
STOPPED=1
ok "服务已停止"

# ---------- 2. 打包 ----------
STAGE="$(mktemp -d)" || die "建临时打包目录失败（mktemp 报错；TMPDIR 或 /tmp 写不了？df -h 看看）"
mkdir -p "$STAGE/volumes" "$STAGE/files"
info "2. 打包卷与文件"
for v in "${VOLUMES[@]}"; do
  snapshot_volume "$v"
  ok "卷 $v 已打包（$(tar_file_count "$STAGE/volumes/$v.tar.gz") 个文件）"
done

# snapfifo：只有 fifo 文件（Snapcast 的流管道），没有持久数据就跳过。
SKIPPED_NOTE=""
if volume_has_regular_files "$SNAPFIFO"; then
  snapshot_volume "$SNAPFIFO"
  ok "卷 $SNAPFIFO 里有持久文件，已打包"
else
  SKIPPED_NOTE="$SNAPFIFO: 卷里没有持久文件（只有 fifo/空），已跳过；恢复时由 MPD/snapserver 重建"
  note "$SKIPPED_NOTE"
fi

# 散文件：config + .env 必备；compose + snapserver.conf 一起备，恢复时自包含。
cp "$CONFIG_FILE" "$STAGE/files/config.yaml" \
  || die "复制 hub/config.yaml 失败"
cp "$ENV_FILE" "$STAGE/files/.env" \
  || die "复制 deploy/.env 失败"
cp "$COMPOSE_FILE" "$STAGE/files/docker-compose.yml" \
  || die "复制 docker-compose.yml 失败"
cp "$SCRIPT_DIR/snapserver.conf" "$STAGE/files/snapserver.conf" \
  || die "复制 snapserver.conf 失败（deploy/snapserver.conf 缺失）"
chmod 600 "$STAGE/files/.env"
ENV_KEYS="$(grep -cE '^[A-Za-z_][A-Za-z0-9_]*=' "$ENV_FILE" || true)"
ok "散文件已复制（config.yaml + .env（$ENV_KEYS 个变量，值不打印）+ docker-compose.yml + snapserver.conf）"

# ---------- 3. manifest ----------
info "3. 生成 manifest（文件数 + 大小 + SHA256 + 关键文件清单）"
HUB_LIST="$(tar -tzf "$STAGE/volumes/hub-data.tar.gz" | sed 's|^\./||')"
TLS_FILES="$(printf '%s\n' "$HUB_LIST" | grep '^tls/' | grep -v '/$' || true)"
TLS_PRESENT=false; [ -n "$TLS_FILES" ] && TLS_PRESENT=true
COOKIE_PRESENT=false; printf '%s\n' "$HUB_LIST" | grep -qx 'netease-cookie.json' && COOKIE_PRESENT=true
CREATED_AT="$(date -Iseconds)"
HOSTNAME="$(hostname 2>/dev/null || echo unknown)"

# 先算 SHA256SUMS（staging 内所有成员；恢复时用它校验）。
(cd "$STAGE" && find volumes files -type f -exec sha256sum {} + | sort > SHA256SUMS) \
  || die "生成 SHA256SUMS 失败"
sha_in_sums() { grep -F "  $1" "$STAGE/SHA256SUMS" | cut -d' ' -f1; }

{
  echo '{'
  echo "  \"tool\": \"open-xiaoai-next backup.sh\","
  echo "  \"version\": $TOOL_VERSION,"
  echo "  \"created_at\": \"$(json_escape "$CREATED_AT")\","
  echo "  \"hostname\": \"$(json_escape "$HOSTNAME")\","
  echo '  "volumes": {'
  _n=0
  for v in "${VOLUMES[@]}"; do
    _n=$((_n+1)); _comma=","; [ "$_n" -eq "${#VOLUMES[@]}" ] && _comma=""
    echo "    \"$(json_escape "$v")\": {\"archive\": \"volumes/$v.tar.gz\", \"files\": $(tar_file_count "$STAGE/volumes/$v.tar.gz"), \"bytes\": $(tar_total_bytes "$STAGE/volumes/$v.tar.gz"), \"archive_bytes\": $(file_bytes "$STAGE/volumes/$v.tar.gz"), \"sha256\": \"$(sha_in_sums "volumes/$v.tar.gz")\"}$_comma"
  done
  if [ -f "$STAGE/volumes/$SNAPFIFO.tar.gz" ]; then
    echo "    ,\"$(json_escape "$SNAPFIFO")\": {\"archive\": \"volumes/$SNAPFIFO.tar.gz\", \"files\": $(tar_file_count "$STAGE/volumes/$SNAPFIFO.tar.gz"), \"bytes\": $(tar_total_bytes "$STAGE/volumes/$SNAPFIFO.tar.gz"), \"archive_bytes\": $(file_bytes "$STAGE/volumes/$SNAPFIFO.tar.gz"), \"sha256\": \"$(sha_in_sums "volumes/$SNAPFIFO.tar.gz")\"}"
  fi
  echo '  },'
  echo '  "files": {'
  echo "    \"hub/config.yaml\": {\"archive\": \"files/config.yaml\", \"bytes\": $(file_bytes "$STAGE/files/config.yaml"), \"sha256\": \"$(sha_in_sums "files/config.yaml")\"},"
  echo "    \"deploy/.env\": {\"archive\": \"files/.env\", \"bytes\": $(file_bytes "$STAGE/files/.env"), \"sha256\": \"$(sha_in_sums "files/.env")\"},"
  echo "    \"deploy/docker-compose.yml\": {\"archive\": \"files/docker-compose.yml\", \"bytes\": $(file_bytes "$STAGE/files/docker-compose.yml"), \"sha256\": \"$(sha_in_sums "files/docker-compose.yml")\"},"
  echo "    \"deploy/snapserver.conf\": {\"archive\": \"files/snapserver.conf\", \"bytes\": $(file_bytes "$STAGE/files/snapserver.conf"), \"sha256\": \"$(sha_in_sums "files/snapserver.conf")\"}"
  echo '  },'
  echo '  "key_files": {'
  echo "    \"tls_present\": $TLS_PRESENT,"
  echo '    "tls_files": ['
  if [ -n "$TLS_FILES" ]; then
    printf '%s\n' "$TLS_FILES" | sed 's/.*/      \"&\",/' | sed '$ s/,$//'
  fi
  echo '    ],'
  echo "    \"netease_cookie_present\": $COOKIE_PRESENT,"
  echo '    "netease_cookie": "netease-cookie.json"'
  echo '  },'
  echo '  "skipped": ['
  if [ -n "$SKIPPED_NOTE" ]; then
    echo "    \"$(json_escape "$SKIPPED_NOTE")\""
  fi
  echo '  ],'
  echo '  "excluded": ['
  echo '    "/volume1/music（用户音乐库，不是系统状态；恢复时不碰）",'
  echo '    "Docker 镜像（恢复时 docker compose pull / build 重新拉取/构建）"'
  echo '  ]'
  echo '}'
} > "$STAGE/manifest.json"
ok "manifest.json 已生成（TLS 证书：$TLS_PRESENT；网易云 Cookie：$COOKIE_PRESENT）"

# ---------- 4. 打总包 + 校验 ----------
TS="$(date +%Y%m%d-%H%M%S)"
ARCHIVE="$BACKUP_DIR/open-xiaoai-next-backup-$TS.tar.gz"
_i=2
while [ -e "$ARCHIVE" ]; do
  ARCHIVE="$BACKUP_DIR/open-xiaoai-next-backup-$TS-$(( _i++ )).tar.gz"
done
info "4. 打总包：$ARCHIVE"
(cd "$STAGE" && sha256sum -c SHA256SUMS >/dev/null) \
  || die "打包前校验失败：staging 内文件 SHA256 不符（磁盘或打包过程出错；删掉重跑）"
tar -czf "$ARCHIVE" -C "$STAGE" . \
  || die "写备份归档失败：$ARCHIVE（磁盘满了？df -h 看看）"
# M6：归档含全部机密（Token + Cookie + TLS 私钥），收紧到 0600。
chmod 600 "$ARCHIVE" || die "备份归档权限收紧失败：$ARCHIVE"

# 校验：gzip 完整 + 归档里有 manifest + SHA256SUMS 的每个成员都在。
gzip -t "$ARCHIVE" || die "备份校验失败：$ARCHIVE 不是有效的 gzip（写入中断；删掉重跑）"
ARC_LIST="$(tar -tzf "$ARCHIVE")" || die "备份校验失败：归档列不出内容（tar 报错；删掉重跑）"
echo "$ARC_LIST" | grep -qx './manifest.json' \
  || die "备份校验失败：归档里缺 manifest.json（打包中断；删掉重跑）"
while IFS= read -r member; do
  echo "$ARC_LIST" | grep -qxF "./$member" \
    || die "备份校验失败：归档里缺 $member（打包中断；删掉重跑）"
done < <(awk '{print $2}' "$STAGE/SHA256SUMS")
ok "备份校验通过（gzip 完整，manifest + 全部成员都在）"

# ---------- 5. 恢复启动服务 ----------
info "5. 恢复启动服务"
compose start || die "备份已写好，但服务恢复启动失败——备份在 $ARCHIVE；手动跑：docker compose -f $COMPOSE_FILE start"
RESTARTED=1
ok "服务已恢复启动"

# ---------- 6. 汇总 ----------
ARC_BYTES="$(file_bytes "$ARCHIVE")"
ARC_HUMAN="$(du -h "$ARCHIVE" | cut -f1)"
echo
echo "备份完成：$ARCHIVE"
echo "大小：$ARC_HUMAN（$ARC_BYTES 字节）"
echo "包含：卷 ${VOLUMES[*]}（snapfifo 见 manifest 的 skipped）+ config.yaml + .env（$ENV_KEYS 个变量，值未打印）+ docker-compose.yml + snapserver.conf"
echo "TLS 证书：$TLS_PRESENT；网易云 Cookie：$COOKIE_PRESENT"
echo "提醒：归档含 .env 里的 Token，是机密文件——妥善保管，不要分享、不要提交进仓库。"
echo "恢复方法：./restore.sh --force $ARCHIVE（用法见 deploy/BACKUP.md）"
