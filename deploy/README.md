# deploy — NAS 部署

UGREEN DXP6800 Pro（UGOS + Docker）上跑 Hub + Whisper + Piper + Snapcast + MPD + NeteaseCloudMusicApi。
音乐走 Hub 直连网易云（2026-11-18 融合 B，Music Assistant 已砍，见下文）。

## 启动

```bash
cd deploy
cp .env.example .env   # 填 NAS_IP / HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN / OPEN_XIAOAI_WEB_TOKEN
docker compose up -d
docker compose logs -f hub
```

按顺序做的检查清单见 `NAS_CHECKLIST.md`；一键传文件 + 检查见 `nas-install.sh`
（用法：`NAS_SSH=huajun@NAS_IP NAS_DIR=/volume4/docker/open-xiaoai-next HUB_PORT=4399 ./nas-install.sh up`，脚本本身只做过 `bash -n` 校验，
NAS 上实际运行未验证）。

## 部署前检查（preflight）

`deploy/preflight.sh` 在传文件之前 fail-fast 检查 7 项，任一失败即停并说明原因与修复方法：

1. docker：`docker` + `docker compose` 可用；
2. .env：`OPEN_XIAOAI_TOKEN` / `OPEN_XIAOAI_WEB_TOKEN` 各 ≥16 位、互不相同、非弱值，`HASS_TOKEN` / `NAS_IP` / `MUSIC_FOLDER` 已填；`LLM_API_KEY` 为空只提示（本地 OVMS，不阻塞）；
3. 端口：`HUB_PORT`（默认 4399）+ 8081/3000/10300/10200/10400/1704/1705/6600——`HUB_PORT` 被旧 Hub（`oxn-hub`）占着是割接预期（不停旧 Hub 可改 `HUB_PORT=4400` 并行），被未知进程占着则失败；其他端口被占只警告；
4. 镜像：compose 里的 5 个拉取镜像 + 两个 Dockerfile 的基础镜像，本地没有时查 `manifest inspect` 确认仓库可拉；hub/kws-sidecar 的本地构建输入（`Dockerfile.hub`、`hub-linux-amd64`、kws-sidecar 源文件）缺了也失败；
5. 证书种子：`hub-certs/`（`ca.crt`/`ca.key`/`hub.crt`/`hub.key`）齐全且配对（`openssl verify` + 公钥比对），`client.toml` 的 `hub_ca_fingerprint` 必须与种子 `hub.crt` 的 SHA-256 指纹一致——没种子就 up 的话 Hub 会生成全新 CA，指纹与音箱 mTLS 同时失效；
6. 音箱可达性：ping 或 TCP :22（只测可达，不登录）；`SPEAKER_IP` 未设时跳过本项；
7. 配置：`hub/config.yaml` 与 `docker-compose.yml` 可解析，compose 含 7 个服务。

```
./deploy/preflight.sh                        # 查本机
NAS_SSH=huajun@192.168.88.247 ./deploy/preflight.sh   # docker/端口/镜像改查 NAS（经 SSH）
./deploy/preflight.sh --skip-speaker         # 单项跳过（--skip-docker/--skip-env/--skip-ports/--skip-images/--skip-certs/--skip-speaker/--skip-config）
SPEAKER_IP=192.168.88.190 ./deploy/preflight.sh       # 音箱 IP（.env 里也可写 SPEAKER_IP=）
HUB_CERTS_DIR=<种子目录> ./deploy/preflight.sh       # 证书种子不在默认搜索位置时指定
```

默认搜索证书种子：`deploy/hub-certs` → `release/flash-kit/hub-certs`（含其上两级 `../release/`、`../../release/`，即闪存套件 `flash-kit/hub-certs/` 的位置）；`client.toml` 同理。`nas-install.sh` 开头已接 preflight 门禁（失败即停，不传文件；紧急绕过：`SKIP_PREFLIGHT=1`）。Mock 测试见 `deploy/test_preflight.sh`（`bash deploy/test_preflight.sh`，假 docker/ss/ping/nc/ssh 垫片，不碰真 NAS/真音箱/真 Docker）。

## 备份与恢复

更新 / 部署日之前先备份，见 `BACKUP.md`：

```bash
cd deploy
./backup.sh                                   # 生成 deploy/backups/open-xiaoai-next-backup-*.tar.gz
./restore.sh backups/open-xiaoai-next-backup-*.tar.gz   # 恢复（默认要输入 YES 确认）
```

备份含 5 个具名卷 + `hub/config.yaml` + `.env` + compose/snapserver.conf，
TLS 证书与网易云 Cookie 随 `hub-data` 卷一起备份（manifest 里单独列出）。
`/volume1/music` 与 Docker 镜像不备份。归档含 Token，妥善保管、不要提交
（`deploy/backups/` 已在 `.gitignore` 里）。

## 服务

| 服务 | 镜像 | 端口 | 说明 |
|---|---|---|---|
| hub | 本地构建（deploy/Dockerfile.hub + 预编译二进制 deploy/hub-linux-amd64） | host | 唯一常驻服务，管仲裁/语音/音乐/HA/OTA |
| whisper | rhasspy/wyoming-whisper | 10300 | small-int8，中文，beam-size 1 |
| piper | rhasspy/wyoming-piper | 10200 | zh_CN-huayan-medium，流式合成 |
| kws-sidecar | 本地构建（kws-sidecar/Dockerfile + server.py，模型首次启动下载到 kws-data 卷） | host（10400） | 服务端唤醒检测（sherpa-onnx WenetSpeech，“你好小李”）；Hub 经 ws://127.0.0.1:10400/v1/kws 连它 |
| snapserver | ivdata/snapserver | host | 多房间同步播放服务端 |
| mpd | vimagick/mpd | host | 读 /volume1/Music（可选本地兜底），写 snapfifo |
| netease-api | binaryify/netease_cloud_music_api | 3000 | 网易云非官方 API，只绑 127.0.0.1；本身不保存登录态，Cookie 由 Hub 存在 hub-data 卷，约 1–2 个月过期需重扫 |

> Music Assistant 已砍（2026-11-18 融合 B）：`music-assistant` 服务、`ma-data` 卷、8095 端口全部删除。
> 旧部署迁移见下文“部署迁移说明（从 MA 版升级）”；完整盘点与 Hub 面待确认项见仓库根目录 `MUSIC_FUSION.md`。

## 2026-11-18 音乐融合 B（砍 Music Assistant，Hub 直连网易云）

链路改成：

```
语音点歌 → Hub → NeteaseCloudMusicApi（:3000）→ 网易云
              → MPD → Snapcast Server → 2 台音箱同步放
```

- 少一个容器、少一次转发、少一个 8095 对外端口；登录从 MA 界面收进 Hub Web 面板。
- 代价：HA 里没了现成的音乐 `media_player`（以前是 MA 提供的）；语音点歌与 Hub Web 控制不受影响。
  2026-11-19 HA 融合 b 已补上：Hub 自己向 HA 上报播放状态（见下文“Home Assistant
  深度融合”与 `docs/HA_MEDIA_PLAYER.md`）。
- 网易云接口漂移仍由 `netease-api` 上游扛（非官方 API，随时可能失效）。

### 部署迁移说明（从 MA 版升级）

1. **`.env` 要删的项**：`MA_TOKEN`——如果你的旧 `.env` 里曾手动加过这一行，删掉它。
   注意：本仓库的 `.env.example` 与 compose 从未传过 `MA_TOKEN`（Hub 代码从环境变量读它，
   但部署一直没接线，所以旧部署里 Hub 的 MA 路径实际一直未启用、点歌走 MPD 降级）。
   `music_assistant` 相关的配置项不在 `.env` 里，它在 `hub/config.yaml` 的 `netease.music_assistant`
   一行——种子配置里的该行由 Hub 面删除（已删）；NAS 上旧副本里的该行由 `nas-install.sh` 第 3 步
   自动删掉（新 Hub 读到它会直接拒绝启动，所以不要留着）。
2. **compose 变化**：`music-assistant` 服务与 `ma-data` 卷已删；容器数 8 → 7，拉取镜像 6 → 5；
   8095 端口不再占用、不再检查。
3. **首次部署扫码**：见下文“网易云扫码登录”——改去 Hub Web 面板扫码，不再开 `NAS_IP:8095`。
4. **旧 MA 数据清理（可选）**：确认新链路能播歌后，可删旧卷 `docker volume ls | grep ma-data`
   确认卷名再删（如 `docker volume rm <卷名>`）。删了 MA 的登录态就作废——没关系，
   Hub 侧登录是独立的，去 Web 面板重扫即可。**不要**用 `docker compose down -v`，那会连
   `hub-data` 等其他卷一起删掉。
5. **`music_folder` 现在是可选本地兜底**：`/volume1/Music` 不建、不放歌也不阻塞启动；
   想放本地歌时再建（MPD 读它作降级/本地播放）。

## 2026-11-17 部署包更新（G）

- **Hub 二进制已编好**（2026-11-18 P2 轮收尾重编，分支 `fix-p2-loose-ends` 尖 afcdcf0，
  干净 HEAD worktree 编译）：`deploy/hub-linux-amd64`，12,093,544 字节，
  SHA256 `973a3b1fa2d34acab7a1feaa5c6fb146bc01167735b4c50ba408d1acd0213091`，
  `go build -o deploy/hub-linux-amd64 ./cmd/hub`（hub/ 下，Go 1.23.12）。
  清单 `deploy/hub-linux-amd64.sha256` 已生成（`nas-install.sh` §0 会比对，
  不一致即 FAIL；重编后须重新生成清单）。本机冒烟验证：`--help` 打印用法退出 0；
  弱 Token（`CHANGE_ME`）启动即 Fatal 退出 1；设强 Token 后启动，
  `curl :8081/api/status` 无 Token 返回 401，带 Web Token 返回
  `{"devices":0,"kws_down":true,"kws_status":"down","online":0,"rooms":null,"wake_word":"你好小李"}`
  （`kws_status: down` 是本机无边车的预期结果）；收信号后干净退出，未常驻。
  未设 `OPEN_XIAOAI_TOKEN` 时 Hub 直接退出（日志 `环境变量 OPEN_XIAOAI_TOKEN 未设置`），
  所以 compose 里补上了这个环境变量。`ldd` 实测二进制只依赖 libc/libm，
  Opus 已静态编入，NAS 上不需要装 libopus。旧值 11,747,288 字节（`1e4f6f6c…c80a`）已被取代。
- **修掉三个必死的错**（否则 `docker compose up` 一定失败）：
  1. hub 原来写 `build: ../hub`，但 hub/ 下没有 Dockerfile → 改为 `deploy/Dockerfile.hub`
     （debian:bookworm-slim + ca-certificates，只装预编译二进制；config.yaml 不打进镜像，
     运行时挂载）。
  2. compose 原来没传 `OPEN_XIAOAI_TOKEN` → 已补上。
  3. netease-api 原来写 `binarytorch/neteasecloudmusicapi`，Docker Hub 查无此仓库（404）→
     改为官方镜像 `binaryify/netease_cloud_music_api`。
- **镜像核实**（2026-11-17 逐个查 Docker Hub 页面 / 官方文档，见 `NAS_CHECKLIST.md` 末尾的表）：
  存在：rhasspy/wyoming-whisper、rhasspy/wyoming-piper、
  ivdata/snapserver、vimagick/mpd、
  binaryify/netease_cloud_music_api；不存在：binarytorch/neteasecloudmusicapi（404）。
  （eclipse-mosquitto 已删，见下文 2026-11-17 晚 P1 修复；ghcr.io/music-assistant/server
  已随 2026-11-18 融合 B 删除，见上文。）
- **已知问题 / 未核实项**（NAS 上首次 `up` 时重点看）：
  - Hub 跑在 host 网络下，`hub/config.yaml` 里 netease 的 `netease-api:3000`
    这个容器名解析不到；`nas-install.sh` 会在 NAS 副本上改成
    `127.0.0.1:3000`（本机 `hub/config.yaml` 不动）。
    同理 `agents` 里的 `hermes:8642` / `openclaw:18789` 也解析不到，Hermes/OpenClaw
    部署好之前 Hub 只会记连接失败日志，不阻塞启动。
    （2026-11-18 融合 B：`music-assistant:8095` 一行已随 MA 删除，不再改写；
    `hub/config.yaml` 里该键的删除由 Hub 面负责。）
  - （2026-11-18 融合 B 已删除）旧问题：MA 官方要求 host 网络（mDNS/uPnP 发现播放器），
    本 compose 里 MA 是 bridge 网络 + 8095 映射，MA 里看不到 Snapcast 播放器。
    MA 容器已砍，该问题不存在了；Snapcast 改由 Hub 直接驱动。
  - vimagick/mpd 镜像的默认配置是否把 `/music` 输出到 `/tmp/snapfifo/snapfifo` 未核实
    （本机无 Docker，拉不了镜像看默认配置）。若 MPD 库为空或 snapserver 收不到流，
    需要挂自定义 mpd.conf。
  - `hub/config.yaml` 里的 HA 地址已是真实 NAS 地址 `ws://192.168.88.247:8123`
    （2026-11-19 改）；`nas-install.sh` 第 3 步仍会按 `.env` 的 `NAS_IP` 改写
    NAS 上的副本（防旧副本还是 `192.168.1.10`），并有 grep 断言。

## 网易云扫码登录（H，FR-05，融合 B 版）

只写流程，未登录过（关口 7 BLOCKED，需要 bai 的网易云账号扫码）。
旧版（MA 界面 QR Login）已废弃：2026-11-18 融合 B 起，8095 端口与 MA 界面均不存在。

### 链路

```
语音点歌 → Hub → NeteaseCloudMusicApi（:3000）→ 网易云
              → MPD → Snapcast Server → 2 台音箱同步放
```

### 在哪一步扫码

**在 Hub Web 面板里扫码**：

1. 浏览器直接开 `http://192.168.88.247:8081`，进 Hub Web 面板 → **网易云登录**页。
   面板绑 NAS 内网 IP（`hub/config.yaml` 里 `web_listen: 192.168.88.247:8081`，2026-11-19 bai 定局域网直开），局域网内任何设备都能开，不用建 SSH 隧道。
   要输 Web Token 时用 `deploy/.env` 里的 `OPEN_XIAOAI_WEB_TOKEN`（不要把 Token 发给别人或贴进群聊）。
2. 点**生成二维码**（调 `POST /api/netease/login/qr`），页面显示二维码。
3. 手机打开**网易云音乐 App**扫这个二维码，App 内点确认登录。
4. 面板状态变**已登录**（后台轮询 `GET /api/netease/login/qr/check?key=K` 的结果；
   可再调 `GET /api/netease/status` 核对，见 `NAS_CHECKLIST.md` §3）。

底层原理（不用手工操作，了解即可）：Hub 点生成二维码后，后台调 NeteaseCloudMusicApi
的三步接口——`GET /login/qr/key` 取 unikey → `GET /login/qr/create` 生成二维码 →
轮询 `GET /login/qr/check` 等结果（801 等待扫码 / 802 已扫码待确认 / 803 成功 /
800 二维码过期）。二维码本身有有效期（约 2 分钟），过期（800）就点生成二维码重新生成。
API 也自带一个演示页 `public/qrlogin.html`，但本部署的标准路径是 Hub Web 面板，
不要绕过 Hub 直接调 API 登录。

### 扫码后登录态存在哪

- 登录态是一串 Cookie，核心是 `MUSIC_U`，由 **Hub 保存**，持久化在 `hub-data` 卷
  （Hub 数据目录 `/app/hub-data`；具体文件名待 Hub 面确认，见 `MUSIC_FUSION.md`）。
- **NeteaseCloudMusicApi 容器本身不保存登录态**：它是无状态转发，重启容器不丢登录，
  但 `hub-data` 卷丢了就要重扫。
- 验证登录态：面板里看状态为已登录，或
  `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env | cut -d= -f2-); curl -s -H "Authorization: Bearer $WEB_TOKEN" http://192.168.88.247:8081/api/netease/status`
  返回 `logged_in:true`（在 NAS 上跑，或局域网内任何设备上跑）。

### Cookie 过期与重登

- Cookie 约 **1–2 个月过期**（`MUSIC_U` 约 30–60 天，按 30 天做保守估计）。
  过期表现：搜歌/播歌报 301 需要登录，或歌单突然变空。
- 重登方法：回 Hub Web 面板重新扫码一次，不用改任何配置、不重建容器。
  也可以先调 `/login/refresh` 续期，但过期后只能重扫。

### VIP 歌

- **VIP 歌需要 VIP 账号**。网易云只播账号本身有权限的内容，
  绕不过网易云的任何限制。非 VIP 账号只能播免费曲库，VIP 歌会播不了或只给试听片段。
- 未登录时只能搜公开内容、播免费歌；登录后才能看到自己的歌单、收藏和日推。

### 失败时先看什么

| 现象 | 原因 / 做法 |
|---|---|
| 面板点生成二维码没反应 / `/api/netease/*` 返回 404 | Hub 二进制还没接网易云登录路由（融合 B 的 Hub 面工作）：先确认 Hub 是不是最新构建，再按 `MUSIC_FUSION.md`“待 Hub 面确认”核对 |
| 生成二维码点了没出二维码 | 看 Hub 日志和 `curl http://127.0.0.1:3000/login/qr/key` 是否返回 unikey（3000 只绑 127.0.0.1，在 NAS 上跑） |
| 扫码后 App 显示已确认，面板还是未登录 | 二维码过期（800）或轮询被断，重新点生成二维码再扫 |
| 昨天还能播，今天报 301 | Cookie 过期，重扫（约 1–2 个月周期是正常的） |
| 搜得到歌但播 VIP 歌失败 | 账号不是 VIP，或该歌无版权；换免费歌验证链路本身是否通 |

## 诚实说明

- 本部署包仍是**未在真机上跑过**（关口 2 BLOCKED）。第一次 `up` 出错最可能是：
  UGOS Docker 路径、端口占用——把报错贴回来再改。`/volume1/Music` 不存在不再是
  失败原因（music_folder 是可选本地兜底，缺了只警告）。
- Snapcast 参数沿用已写的两房间包（`speaker-snapcast-3room/`，同样未实测），
  包括 fifo 必须是文件（`/tmp/snapfifo/snapfifo`）这个已修过的坑。
- 网易云 VIP 歌需要 VIP 账号；非官方 API 可能失效，Cookie 过期要扫码重登。

## Home Assistant 深度融合

- **Wyoming STT/TTS 注册**：把 NAS 上的 Whisper（:10300）与 Piper
  （:10200）注册进 HA 做语音识别/合成——先在 NAS 上跑探针
  `go run ./cmd/wyoming-probe`，再按步骤加集成。详见
  [`docs/HA_WYOMING.md`](../docs/HA_WYOMING.md)（注意先确认 HA
  容器的网络模式：端口只绑了 `127.0.0.1`，HA 须能连到 NAS 本机回环）。
- **音箱 media_player 实体**：Hub 每 15 秒把注册表里的音箱上报给
  HA（`media_player.open_xiaoai_*`，state/音量/当前曲；默认开，
  `home_assistant.media_player_enabled: false` 可关）。只做状态上报，
  HA 里控制音箱（播放/暂停/音量）尚未接。详见
  [`docs/HA_MEDIA_PLAYER.md`](../docs/HA_MEDIA_PLAYER.md)。
- 两项都只在 stub/假服务上验证过，尚未在真 NAS、真 HA 上走通过；
  待验证清单见各文档末尾。
