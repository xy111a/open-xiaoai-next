#!/usr/bin/env bash
# restore.sh — open-xiaoai-next NAS 部署恢复
#
# 用法（在 NAS 的部署目录里跑）：
#   cd deploy && ./restore.sh /path/to/open-xiaoai-next-backup-YYYYMMDD-HHMMSS.tar.gz
#
# 恢复前会先校验归档（gzip 完整 + manifest.json 存在 + SHA256SUMS 全过 +
# 关键文件都在），校验不过直接拒绝，什么都不动。
#
# 安全规则：
#   - 默认必须人工确认：在终端输入 YES 才继续；也可以传 --force 跳过确认
#     （如在脚本里调用）。非交互终端又没传 --force 时直接拒绝。
#   - 恢复前先 `docker compose stop` 停栈；恢复后不自动启动（下一步见末尾提示，
#     由人确认状态再 up）。若恢复中途失败，栈保持停止——手动
#     `docker compose start` 可回到恢复前的运行状态（卷可能已部分恢复，
#     先看报错再决定是重跑恢复还是回滚）。
#   - 恢复前把现有的 config.yaml / .env / docker-compose.yml / snapserver.conf
#     各存一份带时间戳的 .bak.YYYYMMDDHHMMSS（在原位置旁边）。
#   - 卷是先清空再解包（保证恢复结果与备份时完全一致，不留备份里没有的旧文件）。
#   - 备份归档本身永远不会被删除或修改（只读）。
#
# 恢复内容：5 个具名卷（hub-data / whisper-data / piper-data / kws-data / mpd-data，
# snapfifo 若备份里有也恢复）+ hub/config.yaml + deploy/.env +
# deploy/docker-compose.yml + deploy/snapserver.conf。
# TLS 证书与网易云 Cookie 在 hub-data 卷里，随卷一起恢复；manifest 里记了
# 它们在备份时是否存在——备份里有、恢复后缺了即 FAIL；备份里本来就没有时
# 只警告（证书需按 flash kit 流程重下发，Cookie 需去 Web 面板重扫）。
#
# 不恢复的东西：/volume1/music（用户音乐库，恢复时不碰）、Docker 镜像
# （恢复后 docker compose up -d 时重新 pull/build）。
#
# 环境变量（一般不用改；测试缝见 deploy/test_backup_restore.sh）：
#   OXN_COMPOSE_FILE / OXN_CONFIG_FILE / OXN_ENV_FILE（同 backup.sh）
#   OXN_VOLUME_ROOT   设了它就用本地目录代替 Docker 卷（只给测试用）
#   DOCKER_BIN        docker 命令（默认 docker）
#   OXN_HELPER_IMAGE  清空/解包卷用的 helper 镜像（默认 busybox:latest）
#
# 退出码：0 成功；1 失败或未确认（FAIL 行里写清检查名 + 原因 + 修复办法）。
# 注意：本脚本只做过 bash -n 与 mock 测试，真 NAS 上的实测见 deploy/BACKUP.md。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${OXN_COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.yml}"
CONFIG_FILE="${OXN_CONFIG_FILE:-$SCRIPT_DIR/../hub/config.yaml}"
ENV_FILE="${OXN_ENV_FILE:-$SCRIPT_DIR/.env}"
VOLUME_ROOT="${OXN_VOLUME_ROOT:-}"
DOCKER_BIN="${DOCKER_BIN:-docker}"
HELPER_IMAGE="${OXN_HELPER_IMAGE:-busybox:latest}"

VOLUMES=(hub-data whisper-data piper-data kws-data mpd-data)
SNAPFIFO="snapfifo"
WORK=""

die()  { echo "FAIL: $*" >&2; exit 1; }
info() { echo "---- $*"; }
note() { echo "NOTE: $*"; }
warn() { echo "WARN: $*"; }
ok()   { echo "PASS: $*"; }

usage() {
  echo "用法：$0 [--force] <备份归档.tar.gz>"
  echo "  --force   跳过人工确认（默认要输入 YES 才恢复）"
  exit 1
}

compose() { "$DOCKER_BIN" compose -f "$COMPOSE_FILE" "$@"; }

cleanup() {
  if [ -n "$WORK" ] && [ -d "$WORK" ]; then
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

resolve_volume() {
  local short="$1" name=""
  name="$(compose config --volumes 2>/dev/null | grep -E "(^|_)${short}$" | head -n1 || true)"
  if [ -z "$name" ]; then
    name="$(basename "$(dirname "$COMPOSE_FILE")")_${short}"
  fi
  printf '%s' "$name"
}

# 卷里恢复后的文件数（与 manifest 里的 files 数比对）。
volume_file_count() {
  local short="$1"
  if [ -n "$VOLUME_ROOT" ]; then
    find "$VOLUME_ROOT/$short" -type f 2>/dev/null | wc -l | tr -d ' '
  else
    "$DOCKER_BIN" run --rm -v "$(resolve_volume "$short"):/src:ro" "$HELPER_IMAGE" \
      sh -c 'find /src -type f | wc -l' 2>/dev/null | tr -d ' '
  fi
}

# 卷里某个相对路径的文件存在且非空吗？
volume_file_nonempty() {
  local short="$1" rel="$2"
  if [ -n "$VOLUME_ROOT" ]; then
    [ -s "$VOLUME_ROOT/$short/$rel" ]
  else
    "$DOCKER_BIN" run --rm -v "$(resolve_volume "$short"):/src:ro" "$HELPER_IMAGE" \
      test -s "/src/$rel" >/dev/null 2>&1
  fi
}

# manifest 里某卷的 files 数（manifest 每卷一行，见 backup.sh）。
manifest_count() {
  grep -o "\"$1\": {\"archive\": \"[^\"]*\", \"files\": [0-9]*" "$WORK/manifest.json" \
    | grep -o '"files": [0-9]*' | grep -o '[0-9]*' | head -n1
}

# ---------- 0. 参数 ----------
FORCE=0
ARCHIVE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --force|-f) FORCE=1; shift ;;
    --help|-h) usage ;;
    --*) die "未知参数：$1（用法：$0 [--force] <备份归档.tar.gz>）" ;;
    *) [ -z "$ARCHIVE" ] || die "只接受一个备份归档，多了：$1"; ARCHIVE="$1"; shift ;;
  esac
done
[ -n "$ARCHIVE" ] || usage
[ -f "$ARCHIVE" ] || die "备份归档不存在：$ARCHIVE（路径写错了？ls 看看备份目录）"

# ---------- 1. 校验归档（不过就拒绝，什么都不动） ----------
info "1. 校验备份归档"
gzip -t "$ARCHIVE" 2>/dev/null \
  || die "归档损坏：gzip 校验失败（文件传了一半或磁盘损坏；换一份备份，删掉这份）"
ARC_LIST="$(tar -tzf "$ARCHIVE" 2>/dev/null)" \
  || die "归档损坏：tar 列不出内容（不是有效的 tar.gz；换一份备份）"
echo "$ARC_LIST" | grep -qx './manifest.json' \
  || die "归档里没有 manifest.json（不是本工具生成的备份，或已损坏；拒绝恢复）"
WORK="$(mktemp -d)"
tar -xzf "$ARCHIVE" -C "$WORK" \
  || die "解包归档失败（磁盘满了？df -h 看看；备份归档本身没动）"
[ -f "$WORK/manifest.json" ] || die "manifest 缺失（解包后找不到 manifest.json；拒绝恢复）"
[ -f "$WORK/SHA256SUMS" ] || die "SHA256SUMS 缺失（不是本工具生成的备份；拒绝恢复）"
(cd "$WORK" && sha256sum -c SHA256SUMS >/dev/null) \
  || die "校验失败：归档内文件 SHA256 不符（备份已损坏；拒绝恢复，换一份备份）"
for m in volumes/hub-data.tar.gz volumes/whisper-data.tar.gz volumes/piper-data.tar.gz \
         volumes/kws-data.tar.gz volumes/mpd-data.tar.gz files/config.yaml files/.env; do
  [ -f "$WORK/$m" ] || die "关键文件缺失：归档里没有 $m（备份不完整；拒绝恢复）"
done
for m in files/docker-compose.yml files/snapserver.conf; do
  [ -f "$WORK/$m" ] || warn "归档里没有 $m（旧版备份可能没有；恢复后用仓库里的现版文件）"
done
ok "归档校验通过（gzip 完整，manifest 存在，SHA256 全过，关键文件都在）"

# 关键文件核对：manifest 说备份时有，恢复后就必须有；本来就没有只警告。
HUB_LIST="$(tar -tzf "$WORK/volumes/hub-data.tar.gz" | sed 's|^\./||')"
TLS_IN_BACKUP="$(printf '%s\n' "$HUB_LIST" | grep '^tls/' | grep -v '/$' || true)"
COOKIE_IN_BACKUP=""
printf '%s\n' "$HUB_LIST" | grep -qx 'netease-cookie.json' && COOKIE_IN_BACKUP="netease-cookie.json"
MANIFEST_TLS="$(grep -o '"tls_present": [a-z]*' "$WORK/manifest.json" | grep -o '[a-z]*$' || true)"
MANIFEST_COOKIE="$(grep -o '"netease_cookie_present": [a-z]*' "$WORK/manifest.json" | grep -o '[a-z]*$' || true)"
if [ "$MANIFEST_TLS" = "true" ] && [ -z "$TLS_IN_BACKUP" ]; then
  die "关键文件缺失：manifest 说备份里有 TLS 证书，但 hub-data 包里找不到 tls/ 文件（备份已损坏；拒绝恢复）"
fi
if [ "$MANIFEST_COOKIE" = "true" ] && [ -z "$COOKIE_IN_BACKUP" ]; then
  die "关键文件缺失：manifest 说备份里有网易云 Cookie，但 hub-data 包里找不到 netease-cookie.json（备份已损坏；拒绝恢复）"
fi
[ -n "$TLS_IN_BACKUP" ] && ok "备份里有 TLS 证书（$(printf '%s\n' "$TLS_IN_BACKUP" | wc -l | tr -d ' ') 个文件）" \
  || warn "备份里没有 TLS 证书（恢复后 Hub 会生成新 CA，音箱的证书指纹会失效；按 flash kit 流程重下发证书）"
[ -n "$COOKIE_IN_BACKUP" ] && ok "备份里有网易云 Cookie（netease-cookie.json）" \
  || warn "备份里没有网易云 Cookie（恢复后去 Hub Web 面板重新扫码登录网易云）"

# ---------- 2. 确认（默认要人工确认；--force 跳过） ----------
info "2. 确认恢复"
echo "即将用以下备份覆盖现有系统：$ARCHIVE"
echo "覆盖范围：卷 ${VOLUMES[*]} + hub/config.yaml + deploy/.env（现有文件先存 .bak.时间戳）"
echo "不碰：/volume1/music、Docker 镜像、备份归档本身"
if [ "$FORCE" -eq 0 ]; then
  printf '确认恢复请输入 YES（其他输入取消）：'
  if ! read -r _ans; then
    die "未确认（没读到输入），恢复已取消，什么都没动"
  fi
  [ "$_ans" = "YES" ] || die "未确认（输入的不是 YES），恢复已取消，什么都没动"
  ok "已确认（YES）"
else
  ok "已传 --force，跳过人工确认"
fi

# ---------- 3. 停栈 ----------
info "3. 停止服务（docker compose stop；恢复后不自动启动，见末尾下一步）"
compose stop || die "停止服务失败（docker compose stop 报错；恢复未开始，数据没动）"
ok "服务已停止"

# ---------- 4. 恢复卷（先清空再解包） ----------
info "4. 恢复卷"
for v in "${VOLUMES[@]}"; do
  if [ -n "$VOLUME_ROOT" ]; then
    mkdir -p "$VOLUME_ROOT/$v" || die "建卷目录失败：$VOLUME_ROOT/$v"
    find "$VOLUME_ROOT/$v" -mindepth 1 -delete \
      || die "清空卷 $v 失败（恢复中断：栈已停，卷可能已部分清空——先看报错，确认后再重跑恢复）"
    tar -xzf "$WORK/volumes/$v.tar.gz" -C "$VOLUME_ROOT/$v" \
      || die "恢复卷 $v 失败（恢复中断：栈已停——先看报错，确认后再重跑恢复）"
  else
    "$DOCKER_BIN" run --rm \
      -v "$(resolve_volume "$v"):/dst" \
      -v "$WORK:/src:ro" \
      "$HELPER_IMAGE" \
      sh -c "find /dst -mindepth 1 -delete && tar -xzf /src/volumes/$v.tar.gz -C /dst" \
      || die "恢复卷 $v 失败（拉取 $HELPER_IMAGE 失败或卷写不进；恢复中断：栈已停——先看报错，确认后再重跑恢复）"
  fi
  ok "卷 $v 已恢复"
done
if [ -f "$WORK/volumes/$SNAPFIFO.tar.gz" ]; then
  if [ -n "$VOLUME_ROOT" ]; then
    mkdir -p "$VOLUME_ROOT/$SNAPFIFO"
    find "$VOLUME_ROOT/$SNAPFIFO" -mindepth 1 -delete
    tar -xzf "$WORK/volumes/$SNAPFIFO.tar.gz" -C "$VOLUME_ROOT/$SNAPFIFO" \
      || die "恢复卷 $SNAPFIFO 失败"
  else
    "$DOCKER_BIN" run --rm \
      -v "$(resolve_volume "$SNAPFIFO"):/dst" \
      -v "$WORK:/src:ro" \
      "$HELPER_IMAGE" \
      sh -c "find /dst -mindepth 1 -delete && tar -xzf /src/volumes/$SNAPFIFO.tar.gz -C /dst" \
      || die "恢复卷 $SNAPFIFO 失败"
  fi
  ok "卷 $SNAPFIFO 已恢复"
else
  note "备份里没有 $SNAPFIFO（备份时它没有持久文件）；恢复时不建，由 MPD/snapserver 重建"
fi

# ---------- 5. 恢复散文件（先存 .bak） ----------
info "5. 恢复散文件（config.yaml / .env / docker-compose.yml / snapserver.conf）"
TS="$(date +%Y%m%d%H%M%S)"
restore_file() {
  local src="$1" dest="$2"
  [ -f "$src" ] || die "备份里缺 $src（恢复中断：栈已停——先看报错）"
  if [ -f "$dest" ]; then
    cp "$dest" "$dest.bak.$TS" \
      || die "备份现有文件失败：$dest（恢复中断：栈已停——先看报错）"
    note "现有 $dest 已备份为 $dest.bak.$TS"
  fi
  mkdir -p "$(dirname "$dest")" || die "建目录失败：$(dirname "$dest")"
  cp "$src" "$dest" || die "恢复 $dest 失败（恢复中断：栈已停——先看报错）"
}
restore_file "$WORK/files/config.yaml" "$CONFIG_FILE"
restore_file "$WORK/files/.env" "$ENV_FILE"
chmod 600 "$ENV_FILE"
[ -f "$WORK/files/docker-compose.yml" ] && restore_file "$WORK/files/docker-compose.yml" "$COMPOSE_FILE"
[ -f "$WORK/files/snapserver.conf" ] && restore_file "$WORK/files/snapserver.conf" "$SCRIPT_DIR/snapserver.conf"
# Hub 以 uid 1000 运行，config.yaml 须属主 1000 才读得到（同 nas-install.sh §2）。
# 非 root 跑时会失败，只警告不中断（NAS 上以 root 跑恢复时才会生效）。
chown 1000:1000 "$CONFIG_FILE" 2>/dev/null \
  || note "config.yaml 属主未改成 1000:1000（非 root 时正常；NAS 上以 root 跑恢复可再 chown 一次）"
ok "散文件已恢复（.env 权限已设 600）"

# ---------- 6. 校验恢复结果 ----------
info "6. 校验恢复结果（文件数对 manifest + 关键文件非空）"
[ -s "$CONFIG_FILE" ] || die "校验失败：恢复后 config.yaml 为空（恢复不完整；栈已停——重跑恢复）"
[ -s "$ENV_FILE" ] || die "校验失败：恢复后 .env 为空（恢复不完整；栈已停——重跑恢复）"
for v in "${VOLUMES[@]}"; do
  _want="$(manifest_count "$v")"
  _got="$(volume_file_count "$v")"
  if [ -n "$_want" ] && [ "$_want" != "$_got" ]; then
    die "校验失败：卷 $v 恢复后文件数是 $_got，manifest 里是 $_want（恢复不完整；栈已停——重跑恢复）"
  fi
  ok "卷 $v 文件数与 manifest 一致（$_got 个）"
done
if [ -n "$TLS_IN_BACKUP" ]; then
  while IFS= read -r _f; do
    [ -n "$_f" ] || continue
    volume_file_nonempty "hub-data" "$_f" \
      || die "校验失败：恢复后 hub-data 里缺关键文件 $_f（或为空；栈已停——重跑恢复）"
  done <<EOF
$TLS_IN_BACKUP
EOF
  ok "TLS 证书已恢复（manifest 列出的文件都在且非空）"
fi
if [ -n "$COOKIE_IN_BACKUP" ]; then
  volume_file_nonempty "hub-data" "netease-cookie.json" \
    || die "校验失败：恢复后 hub-data 里缺 netease-cookie.json（栈已停——重跑恢复）"
  ok "网易云 Cookie 已恢复（netease-cookie.json 非空）"
fi

# ---------- 7. 下一步 ----------
echo
echo "恢复完成。备份归档本身未删除：$ARCHIVE"
echo "下一步："
echo "  1. 启动服务：cd $(dirname "$COMPOSE_FILE") && docker compose up -d"
echo "  2. 检查：docker compose ps（应为 7 个 Up）与 docker compose logs hub"
echo "  3. 打开 Hub Web 面板确认；网易云显示未登录时重新扫码（Cookie 过期或备份里没有时正常）"
echo "  4. 若音箱连不上 Hub：核对 client.toml 的 hub_ca_fingerprint 与恢复后的证书指纹是否一致"
echo "旧文件备份在原位置旁（*.bak.$TS），确认恢复无误后再删。"
