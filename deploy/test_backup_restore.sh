#!/usr/bin/env bash
# test_backup_restore.sh — backup.sh / restore.sh 的 mock 测试
#
# 用法：./deploy/test_backup_restore.sh
#   或在仓库根目录：bash deploy/test_backup_restore.sh
#
# 测法：搭一个假环境（临时目录里的假卷目录 + 假 config/.env + 假 docker），
# 不碰真 Docker、不碰真 NAS、不碰真数据。假 docker 只是一个记日志的 shim：
# mock 模式下脚本只调 `docker compose stop/start`（卷数据走本地目录），
# shim 若收到 `docker run` 会直接报错退出——以此证明脚本没绕过测试缝。
#
# 用例（见末尾矩阵）：
#   1. backup 生成带 manifest 的归档，内容与 manifest 一致
#   2. backup 可重复跑，两次生成不同的归档、互不覆盖
#   3. restore 字节级还原（清空后恢复，sha256 全对上）
#   4. restore 拒绝缺 manifest / 损坏的归档（非零退出，目标文件不动）
#   5. restore 无 --force 又未确认时拒绝（EOF / 输入 no 都拒绝；输入 YES 才恢复）
#   6. backup 输出里不出现任何假 Token 值
#   7. snapfifo 只有 fifo 时被跳过并在 manifest 里注明
#   8. backup 在 .env 缺失时快速失败，且失败后服务仍被重新 start
#
# 真 NAS / 真 Docker 卷的实测不在此列，见 deploy/BACKUP.md“未验证”一节。
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_SH="$SCRIPT_DIR/backup.sh"
RESTORE_SH="$SCRIPT_DIR/restore.sh"

PASS_COUNT=0
FAIL_COUNT=0
FAILED_CASES=()
pass() { PASS_COUNT=$((PASS_COUNT+1)); echo "PASS: $1"; }
fail() { FAIL_COUNT=$((FAIL_COUNT+1)); FAILED_CASES+=("$1"); echo "FAIL: $1"; }
check() { # check <name> <command...>
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}

# ---------- 搭假环境 ----------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAKE="$TMP/fake"
VOLROOT="$FAKE/volumes"
BACKUPS="$TMP/backups"
mkdir -p "$VOLROOT" "$BACKUPS" "$TMP/bin" "$FAKE/deploy" "$FAKE/hub"

# 假 docker shim：只记调用；stop/start 返回 0；config --volumes 印卷名；
# mock 模式下不该出现 docker run，出现了就报错（证明脚本走了测试缝）。
cat > "$TMP/bin/docker" <<'SHIM'
#!/usr/bin/env bash
echo "docker $*" >> "${FAKE_DOCKER_LOG:-/dev/null}"
case "$*" in
  *"compose -f "*" config --volumes"*)
    printf '%s\n' deploy_hub-data deploy_whisper-data deploy_piper-data deploy_kws-data deploy_mpd-data deploy_snapfifo
    exit 0 ;;
  *"compose -f "*" stop"|*"compose -f "*" start")
    exit 0 ;;
  *" run "*)
    echo "SHIM-ERROR: mock 模式下不应调 docker run：$*" >&2
    exit 1 ;;
  *) exit 0 ;;
esac
SHIM
chmod +x "$TMP/bin/docker"
export FAKE_DOCKER_LOG="$TMP/docker.log"
export PATH="$TMP/bin:$PATH"

# 假卷数据（带假证书、假 Cookie、假模型文件；Token 全是 DUMMY_ 前缀的假值）。
mkdir -p "$VOLROOT/hub-data/tls" "$VOLROOT/hub-data/reminders" \
         "$VOLROOT/whisper-data" "$VOLROOT/piper-data" \
         "$VOLROOT/kws-data/models" "$VOLROOT/mpd-data" "$VOLROOT/snapfifo"
printf 'FAKE-CA-CERT\n' > "$VOLROOT/hub-data/tls/ca.crt"
printf 'FAKE-CA-KEY\n' > "$VOLROOT/hub-data/tls/ca.key"
printf 'FAKE-HUB-CERT\n' > "$VOLROOT/hub-data/tls/hub.crt"
printf 'FAKE-HUB-KEY\n' > "$VOLROOT/hub-data/tls/hub.key"
printf 'FAKE-CLIENT-CERT\n' > "$VOLROOT/hub-data/tls/client-lx06.crt"
printf 'FAKE-CLIENT-KEY\n' > "$VOLROOT/hub-data/tls/client-lx06.key"
printf '{"MUSIC_U":"FAKE_COOKIE_VALUE"}\n' > "$VOLROOT/hub-data/netease-cookie.json"
printf '{"reminders":[]}\n' > "$VOLROOT/hub-data/reminders/reminders.json"
printf 'FAKE-WHISPER-MODEL\n' > "$VOLROOT/whisper-data/ggml-small-int8.bin"
printf 'FAKE-PIPER-VOICE\n' > "$VOLROOT/piper-data/zh_CN-huayan-medium.onnx"
printf 'FAKE-KWS-MODEL\n' > "$VOLROOT/kws-data/models/keywords.txt"
printf 'FAKE-MPD-DB\n' > "$VOLROOT/mpd-data/database"
printf 'FAKE-MPD-STATE\n' > "$VOLROOT/mpd-data/state"
mkfifo "$VOLROOT/snapfifo/snapfifo"   # 只有 fifo，无持久文件 → 应被跳过

# 假 config / .env / compose / snapserver.conf（用仓库里的真 compose 更像）。
printf 'hub:\n  wake_word: "你好小李"\n  data_dir: "./hub-data"\n' > "$FAKE/hub/config.yaml"
cat > "$FAKE/deploy/.env" <<'ENV'
NAS_IP=192.168.1.99
MUSIC_FOLDER=/volume1/music
HASS_TOKEN=DUMMY_HASS_TOKEN_aaaa1111
LLM_API_KEY=DUMMY_LLM_KEY_bbbb2222
OPEN_XIAOAI_TOKEN=DUMMY_CLIENT_TOKEN_cccc3333
OPEN_XIAOAI_WEB_TOKEN=DUMMY_WEB_TOKEN_dddd4444
ENV
cp "$SCRIPT_DIR/docker-compose.yml" "$FAKE/deploy/docker-compose.yml"
cp "$SCRIPT_DIR/snapserver.conf" "$FAKE/deploy/snapserver.conf"
cp "$BACKUP_SH" "$FAKE/deploy/backup.sh"
cp "$RESTORE_SH" "$FAKE/deploy/restore.sh"
chmod +x "$FAKE/deploy/backup.sh" "$FAKE/deploy/restore.sh"

export OXN_COMPOSE_FILE="$FAKE/deploy/docker-compose.yml"
export OXN_CONFIG_FILE="$FAKE/hub/config.yaml"
export OXN_ENV_FILE="$FAKE/deploy/.env"
export OXN_BACKUP_DIR="$BACKUPS"
export OXN_VOLUME_ROOT="$VOLROOT"

# 目录树快照：每行 "<sha256>  <相对路径>"（只含文件），供恢复前后比对。
tree_snapshot() { (cd "$1" && find . -type f ! -name '*.bak.*' -exec sha256sum {} + | sort); }

# ---------- 用例 1：backup 生成归档，内容与 manifest 一致 ----------
echo "---- 用例 1：backup 生成归档"
BACKUP_OUT="$TMP/backup1.out"
"$FAKE/deploy/backup.sh" > "$BACKUP_OUT" 2>&1
BACKUP_RC=$?
check "1a. backup 退出 0" test "$BACKUP_RC" -eq 0
ARC1="$(ls -t "$BACKUPS"/open-xiaoai-next-backup-*.tar.gz 2>/dev/null | head -n1)"
check "1b. 生成了时间戳归档" test -n "$ARC1" -a -f "$ARC1"
ARC1_LIST="$TMP/arc1.list"
tar -tzf "$ARC1" > "$ARC1_LIST" 2>/dev/null
check "1c. 归档里有 manifest.json" grep -qx './manifest.json' "$ARC1_LIST"
check "1d. 归档里有 SHA256SUMS" grep -qx './SHA256SUMS' "$ARC1_LIST"
for m in volumes/hub-data.tar.gz volumes/whisper-data.tar.gz volumes/piper-data.tar.gz \
         volumes/kws-data.tar.gz volumes/mpd-data.tar.gz \
         files/config.yaml files/.env files/docker-compose.yml files/snapserver.conf; do
  check "1e. 归档里有 $m" grep -qx "./$m" "$ARC1_LIST"
done
MANIFEST_DIR="$TMP/manifest1"
mkdir -p "$MANIFEST_DIR"
tar -xzf "$ARC1" -C "$MANIFEST_DIR" ./manifest.json 2>/dev/null
check "1f. manifest 列出 TLS 证书" grep -q '"tls/ca.crt"' "$MANIFEST_DIR/manifest.json"
check "1g. manifest 列出网易云 Cookie" grep -q '"netease_cookie_present": true' "$MANIFEST_DIR/manifest.json"
check "1h. manifest 记了 snapfifo 跳过" grep -q 'snapfifo' "$MANIFEST_DIR/manifest.json"
check "1i. manifest 是合法 JSON" python3 -c "import json; json.load(open('$MANIFEST_DIR/manifest.json'))"
# manifest 里的 files 数与卷包实际文件数对得上。
mkdir -p "$TMP/arc1full" && tar -xzf "$ARC1" -C "$TMP/arc1full" 2>/dev/null
HUB_COUNT="$(tar -tzf "$TMP/arc1full/volumes/hub-data.tar.gz" | grep -vc '/$')"
MANIFEST_HUB_COUNT="$(grep -o '"hub-data": {"archive": "[^"]*", "files": [0-9]*' "$MANIFEST_DIR/manifest.json" | grep -o '[0-9]*$')"
check "1j. manifest 的 hub-data 文件数与卷包一致（$MANIFEST_HUB_COUNT == $HUB_COUNT）" test "$MANIFEST_HUB_COUNT" = "$HUB_COUNT"
check "1k. docker shim 收到 compose stop 与 start" sh -c 'grep -q "compose -f .* stop" "$0" && grep -q "compose -f .* start" "$0"' "$TMP/docker.log"
check "1l. 输出含归档路径与大小" grep -q "备份完成：$ARC1" "$BACKUP_OUT"

# ---------- 用例 6：backup 输出不含任何 Token 值 ----------
echo "---- 用例 6：输出不含 Token"
for tok in DUMMY_HASS_TOKEN_aaaa1111 DUMMY_LLM_KEY_bbbb2222 DUMMY_CLIENT_TOKEN_cccc3333 DUMMY_WEB_TOKEN_dddd4444; do
  if grep -qF "$tok" "$BACKUP_OUT"; then fail "6. 输出泄露了 Token（$tok）"; else pass "6. 输出未泄露 $tok"; fi
done

# ---------- 用例 2：backup 可重复跑 ----------
echo "---- 用例 2：backup 可重复跑"
sleep 1
"$FAKE/deploy/backup.sh" > "$TMP/backup2.out" 2>&1
ARC2="$(ls -t "$BACKUPS"/open-xiaoai-next-backup-*.tar.gz 2>/dev/null | head -n1)"
check "2a. 第二次 backup 也成功" test -f "$ARC2"
check "2b. 两次归档不同名" test "$ARC1" != "$ARC2"
check "2c. 第一次归档还在（没被覆盖）" test -f "$ARC1"
check "2d. 备份目录里有两个归档" test "$(ls "$BACKUPS"/open-xiaoai-next-backup-*.tar.gz | wc -l)" -eq 2

# ---------- 用例 3：restore 字节级还原 ----------
echo "---- 用例 3：restore 字节级还原"
BEFORE_SNAP="$TMP/before.snap"
tree_snapshot "$FAKE" > "$BEFORE_SNAP"
# 注意：快照含假 deploy/ 里的 backup.sh/restore.sh 与 compose——恢复后应原样。
#  wipe：清空所有卷 + config + .env（模拟丢数据）。
find "$VOLROOT" -mindepth 1 -delete
rm -f "$FAKE/hub/config.yaml" "$FAKE/deploy/.env"
check "3a. wipe 后卷已空" test -z "$(find "$VOLROOT" -type f | head -n1)"
"$FAKE/deploy/restore.sh" --force "$ARC1" > "$TMP/restore3.out" 2>&1
RESTORE_RC=$?
check "3b. restore --force 退出 0" test "$RESTORE_RC" -eq 0
AFTER_SNAP="$TMP/after.snap"
tree_snapshot "$FAKE" > "$AFTER_SNAP"
check "3c. 恢复后整树 sha256 与备份前一致" cmp -s "$BEFORE_SNAP" "$AFTER_SNAP"
check "3d. 证书恢复且非空" test -s "$VOLROOT/hub-data/tls/ca.crt"
check "3e. Cookie 恢复且非空" test -s "$VOLROOT/hub-data/netease-cookie.json"
check "3f. 恢复输出含下一步提示" grep -q "docker compose up -d" "$TMP/restore3.out"
check "3g. 备份归档本身还在（恢复没删它）" test -f "$ARC1"
check "3h. 旧文件存了 .bak（compose/snapserver.conf 未被 wipe，恢复时应留 .bak）" sh -c 'ls "$0"/deploy/docker-compose.yml.bak.* "$0"/deploy/snapserver.conf.bak.* >/dev/null 2>&1' "$FAKE"

# ---------- 用例 4：restore 拒绝坏归档 ----------
echo "---- 用例 4：restore 拒绝坏归档"
# 4a：缺 manifest 的归档。
mkdir -p "$TMP/nomanifest" && printf 'x\n' > "$TMP/nomanifest/stray.txt"
tar -czf "$TMP/bad-nomanifest.tar.gz" -C "$TMP/nomanifest" stray.txt
SENTINEL="$VOLROOT/hub-data/tls/ca.crt"
SENTINEL_SHA_BEFORE="$(sha256sum "$SENTINEL" | cut -d' ' -f1)"
"$FAKE/deploy/restore.sh" --force "$TMP/bad-nomanifest.tar.gz" > "$TMP/restore4a.out" 2>&1
check "4a. 缺 manifest 的归档被拒绝（非零退出）" test "$?" -ne 0
check "4a2. 拒绝后目标文件没动" test "$(sha256sum "$SENTINEL" | cut -d' ' -f1)" = "$SENTINEL_SHA_BEFORE"
check "4a3. 拒绝原因提到 manifest" grep -qi 'manifest' "$TMP/restore4a.out"
# 4b：截断/损坏的 gzip。
head -c 100 "$ARC1" > "$TMP/bad-truncated.tar.gz"
"$FAKE/deploy/restore.sh" --force "$TMP/bad-truncated.tar.gz" > "$TMP/restore4b.out" 2>&1
check "4b. 损坏的 gzip 被拒绝（非零退出）" test "$?" -ne 0
check "4b2. 拒绝后目标文件没动" test "$(sha256sum "$SENTINEL" | cut -d' ' -f1)" = "$SENTINEL_SHA_BEFORE"
# 4c：SHA256 被篡改的归档（manifest 在，但内容被改过）。
mkdir -p "$TMP/tampered" && tar -xzf "$ARC1" -C "$TMP/tampered" 2>/dev/null
printf 'TAMPERED\n' >> "$TMP/tampered/files/config.yaml"
tar -czf "$TMP/bad-tampered.tar.gz" -C "$TMP/tampered" .
"$FAKE/deploy/restore.sh" --force "$TMP/bad-tampered.tar.gz" > "$TMP/restore4c.out" 2>&1
check "4c. 篡改过的归档被拒绝（SHA256 不符）" test "$?" -ne 0
check "4c2. 拒绝后目标文件没动" test "$(sha256sum "$SENTINEL" | cut -d' ' -f1)" = "$SENTINEL_SHA_BEFORE"

# ---------- 用例 5：restore 无 --force 又未确认时拒绝 ----------
echo "---- 用例 5：无 --force 时要确认"
"$FAKE/deploy/restore.sh" "$ARC1" < /dev/null > "$TMP/restore5a.out" 2>&1
check "5a. stdin EOF 时拒绝（非零退出）" test "$?" -ne 0
check "5a2. 拒绝后目标文件没动" test "$(sha256sum "$SENTINEL" | cut -d' ' -f1)" = "$SENTINEL_SHA_BEFORE"
printf 'no\n' | "$FAKE/deploy/restore.sh" "$ARC1" > "$TMP/restore5b.out" 2>&1
check "5b. 输入 no 时拒绝" test "$?" -ne 0
check "5b2. 拒绝后目标文件没动" test "$(sha256sum "$SENTINEL" | cut -d' ' -f1)" = "$SENTINEL_SHA_BEFORE"
# 输入 YES 应恢复（先 wipe 再验）。
find "$VOLROOT" -mindepth 1 -delete
printf 'YES\n' | "$FAKE/deploy/restore.sh" "$ARC1" > "$TMP/restore5c.out" 2>&1
check "5c. 输入 YES 时恢复成功" test "$?" -eq 0
check "5c2. YES 恢复后证书回来了" test -s "$VOLROOT/hub-data/tls/ca.crt"
check "5c3. YES 恢复时旧 config/.env 存了 .bak" sh -c 'ls "$0"/hub/config.yaml.bak.* "$0"/deploy/.env.bak.* >/dev/null 2>&1' "$FAKE"

# ---------- 用例 8：backup 失败时快速失败，且停栈后失败仍会恢复启动 ----------
echo "---- 用例 8：backup 失败处理"
# 8a：.env 缺失 → 预检查失败（还没停栈）。
mv "$FAKE/deploy/.env" "$TMP/env.saved"
"$FAKE/deploy/backup.sh" > "$TMP/backup8a.out" 2>&1
check "8a. 缺 .env 时 backup 非零退出" test "$?" -ne 0
check "8a2. 失败原因提到 .env" grep -q '\.env' "$TMP/backup8a.out"
check "8a3. 失败时没新建归档" test "$(ls "$BACKUPS"/open-xiaoai-next-backup-*.tar.gz | wc -l)" -eq 2
mv "$TMP/env.saved" "$FAKE/deploy/.env"
# 8b：临时目录建不起来 → 打包中途失败（已停栈），trap 应把服务 start 回来。
# （mktemp 在停栈后执行；TMPDIR 指到一个普通文件上，mktemp 必失败。
#  注：本机是 root，chmod 000 的文件 tar 照样读得动，所以不用权限法。）
touch "$TMP/notadir"
: > "$TMP/docker.log"
TMPDIR="$TMP/notadir" "$FAKE/deploy/backup.sh" > "$TMP/backup8b.out" 2>&1
check "8b. 打包失败时 backup 非零退出" test "$?" -ne 0
check "8b2. 失败后服务被重新 start（trap 生效）" sh -c 'awk "/stop/{s=1} /start/{if(s) ok=1} END{exit !ok}" "$0"' "$TMP/docker.log"
check "8b3. 失败时没新建归档" test "$(ls "$BACKUPS"/open-xiaoai-next-backup-*.tar.gz | wc -l)" -eq 2

# ---------- 汇总 ----------
echo
echo "汇总：PASS=$PASS_COUNT FAIL=$FAIL_COUNT"
if [ "$FAIL_COUNT" -gt 0 ]; then
  echo "失败用例："
  printf '  - %s\n' "${FAILED_CASES[@]}"
  exit 1
fi
echo "全部 mock 测试通过。真 NAS / 真 Docker 卷的实测仍需在 NAS 上做，见 deploy/BACKUP.md。"
