# 备份与恢复（deploy/backup.sh / restore.sh）

> 状态：脚本只做过 `bash -n` 与 mock 测试（`deploy/test_backup_restore.sh`，
> 假卷 + 假 docker，全过）。**真 NAS 上的实测还没做过**——第一次在 NAS 上
> 跑之前，先按下面“首次在 NAS 上验证”一步步来，不要直接在有数据的栈上试。

## 什么时候备份

- **每次更新之前**（拉新镜像、`nas-install.sh up`、改 config/.env 之前）。
- **部署日之前**（flash 当天早上先备一份，出问题能回到当天早上）。
- 建议平时每月备一份；备份本身只要几十秒停机。

## 怎么备份

```bash
cd deploy
./backup.sh
# 备份完成：deploy/backups/open-xiaoai-next-backup-20261119-173000.tar.gz
# 大小：12M（12345678 字节）
```

备份完把归档拷到 NAS 外（如电脑上）再存一份。归档含 `.env` 里的 Token，
是机密文件——**妥善保管，不要分享、不要提交进仓库**
（`deploy/backups/` 已在 `.gitignore` 里）。
归档写完后自动 `chmod 600`。加密（如 age/gpg）是中长期项，暂未做——
归档不要长期留在共享目录。

### 备份里有什么

| 内容 | 来源 | 说明 |
|---|---|---|
| 5 个具名卷 | hub-data / whisper-data / piper-data / kws-data / mpd-data | 各打成一个 `volumes/<卷名>.tar.gz` |
| hub/config.yaml | 仓库 `hub/config.yaml` 的 NAS 副本 | 语音认领的房间、Web 改的配置都在这里 |
| deploy/.env | NAS 上的 `.env` | 含 HASS_TOKEN / LLM_API_KEY / 两个 Token（只记数量，不打印值） |
| deploy/docker-compose.yml、snapserver.conf | 部署文件 | 恢复时自包含，不用再从仓库拷 |
| manifest.json + SHA256SUMS | 备份时生成 | 每个成员的文件数、大小、SHA256；TLS 证书与网易云 Cookie 单独列出 |

`snapfifo` 卷只有 fifo 文件、无持久数据时**跳过**，并在 manifest 的
`skipped` 里注明；它里面有持久文件时则正常打包。
TLS 证书（`hub-data/tls/`：ca.crt/ca.key、hub.crt/hub.key、
client-\*.crt/.key）与网易云 Cookie（`hub-data/netease-cookie.json`）
在 `hub-data` 卷里，随卷一起备份；manifest 里单独列出，供恢复时核对。

### 备份里没有（也不会恢复）什么

- **`/volume1/music`**：用户音乐库，不是系统状态；恢复时不碰。
- **Docker 镜像**：恢复后 `docker compose up -d` 时重新 pull/build。
- **音箱固件**：备份只管 NAS 端；音箱侧用 flash kit 另行处理。

### 一致性：为什么先停栈

备份前先 `docker compose stop` 停掉整个栈，打完包立刻 `docker compose start`。
停了就没人写卷，快照不可能撕裂；在线热备抓到半写文件，恢复出来是坏的。
停机只有几十秒（备份本身不下行、无需等模型加载）。若备份中途失败，
trap 会尽力把服务重新 start 起来。

## 怎么恢复

```bash
cd deploy
./restore.sh /path/to/open-xiaoai-next-backup-20261119-173000.tar.gz
# 确认恢复请输入 YES（其他输入取消）：YES
```

默认必须人工确认（输入 `YES`）；非交互场景加 `--force` 跳过确认：

```bash
./restore.sh --force /path/to/open-xiaoai-next-backup-20261119-173000.tar.gz
```

恢复流程：校验归档（gzip 完整 + manifest 存在 + SHA256 全过 + 关键文件
都在，**不过直接拒绝，什么都不动**）→ 确认 → `docker compose stop` 停栈 →
卷先清空再解包（保证与备份时完全一致）→ 恢复 config/.env/compose/
snapserver.conf（现有文件先存一份 `*.bak.时间戳`）→ 校验恢复结果
（文件数对 manifest + TLS 证书/Cookie 非空）→ 打印下一步。
**备份归档本身永远不会被删除或修改。**

恢复后：

```bash
cd deploy
docker compose up -d
docker compose ps          # 应为 7 个 Up
docker compose logs hub     # 看 Hub 启动与 KWS 健康检查
```

然后打开 Hub Web 面板确认；网易云显示未登录时重新扫码（Cookie 过期或
备份里没有时正常）。若音箱连不上 Hub，核对 client.toml 的
`hub_ca_fingerprint` 与恢复后的证书指纹是否一致（证书重下发后指纹会变）。

### 恢复失败 / 拒绝时

| 现象 | 原因 / 做法 |
|---|---|
| `归档里没有 manifest.json` | 不是本工具生成的备份，或已损坏；换一份备份 |
| `SHA256 不符` | 备份已损坏（传了一半/磁盘损坏）；换一份备份 |
| `关键文件缺失：manifest 说备份里有 TLS 证书，但包里找不到` | 备份已损坏；换一份备份 |
| `未确认` | 没输 YES 或没传 `--force`；确认要恢复就重跑 |
| 恢复中途 FAIL（如卷写不进） | 栈保持停止，先看报错；可重跑恢复，或 `docker compose start` 回到恢复前 |

## 首次在 NAS 上验证（没做过，先做这一步）

1. 在 NAS 上先跑一次 `./backup.sh`，确认归档生成、服务自动恢复、
   `docker compose ps` 仍为 7 个 Up。
2. 把归档拷到电脑上保存（`scp root@NAS_IP:/volume1/docker/open-xiaoai-next/deploy/backups/*.tar.gz .`）。
3. 不要直接在有数据的栈上试恢复——先在确认备份无误后，挑一次维护窗口
   按上面步骤恢复一次，验证 `up -d` 后 Hub、KWS、网易云都正常。
4. 验证结果记到这里（日期 + 归档名 + 是否通过）。

## 测试（本机 mock）

```bash
bash deploy/test_backup_restore.sh
```

假卷 + 假 docker shim，不碰真 Docker、不碰真 NAS。覆盖：归档内容与
manifest 一致、可重复跑、字节级还原、拒绝缺 manifest/损坏/篡改的归档、
无确认拒绝恢复、输出不含 Token、snapfifo 跳过、失败后服务恢复。
