# 音乐融合 B——砍 Music Assistant，Hub 直连网易云

- 日期：2026-11-18；分支 `fuse-netease-direct`（基线 main `3770010`）；决策：bai 已选 B。
- 本文件是部署面的实施记录。**本轮只改 deploy/ + 文档，未动 `hub/` 下任何 Go 代码与测试，未 commit、未 push**（由协调员统一提交）。
- PLAN.md 在仓库外（`~/workspace/speaker-open-xiaoai-rebuild/PLAN.md`），已同步更新，但**未进本 PR**（见 §7）。
- 旧二进制 `deploy/hub-linux-amd64` 仍是融合 B 之前的构建（含 MA 字符串），待 Hub 面重编。

## 0. 结论先行

- 部署面已改完：compose 删 `music-assistant` 服务与 `ma-data` 卷（容器 8 → 7，拉取镜像 6 → 5），
  8095 端口不再发布、不再检查；`nas-install.sh` 删 MA 检查、加 netease-api 健康检查与 Hub
  `/api/netease/status` 登录态检查（未登录只警告）；扫码登录从 MA 界面改到 Hub Web 面板；
  `music_folder` 全面改成“可选本地兜底，缺了只警告”。
- 盘点中两个基线描述被证伪（见 §1.3），实施时已按仓库实际处理，如实记录在下面。
- Hub 面（Go 代码 + 8 条新 Web 路由 + Cookie 落盘 + `music_folder` 改 warn）由另一位工人在同分支并行改，
  **截至本轮末已在工作树落地大部分（未提交）**：11 个 handler 全部注册（`hub/internal/web/web.go:173–183`）、
  Cookie 文件 `hub-data/netease-cookie.json`（0600）、`netease.music_assistant` 字段删除且旧键硬报错、
  `ValidateStartup` 不再因文件夹缺失 Fatal、`MAClient` 已删。细节与证据见 §2.2 与 §5；
  最终清单以 Hub 面汇报与最终 diff 为准。

## 1. 盘点结果（MA 引用全清单，基线 3770010）

### 1.1 部署面——活引用（本轮已全部处理）

| 位置 | 内容 | 处理 |
|---|---|---|
| `deploy/docker-compose.yml:16` | 头注释：MA 的 8095 仍向局域网发布（扫码入口） | 改写为融合 B 说明 |
| `deploy/docker-compose.yml:179–196` | `music-assistant` 服务整块：`ghcr.io/music-assistant/server:latest`、`oxn-music-assistant`、`8095:8095`、`ma-data:/data` | 整块删除 |
| `deploy/docker-compose.yml` 末尾 | `volumes:` 里的 `ma-data` | 删除 |
| `deploy/docker-compose.yml:173` | mpd 注释“Hub 启动时自动校验，不存在再问” | 改为可选兜底 |
| `deploy/docker-compose.yml:12` | 头注释“`/volume1/music` 不存在”是首次 up 失败主因之一 | 改为只警告 |
| `deploy/nas-install.sh:93` | §1 端口占用循环含 `8095` | 删除 |
| `deploy/nas-install.sh:123` | §3 注释列出容器名 `music-assistant` | 删除该名 |
| `deploy/nas-install.sh:132,136` | §3 sed 改写 `music-assistant:8095` + 断言 `127.0.0.1:8095` | 删除 |
| `deploy/nas-install.sh:140` | §3 打印 `grep -E 'music_assistant:\|…'` | 去掉 `music_assistant:` |
| `deploy/nas-install.sh:165` | §5“8 个容器都 Up（…music-assistant…）” | 改为 7 个，去掉 MA |
| `deploy/nas-install.sh:171` | §5 端口连通循环含 `8095` | 删除 |
| `deploy/nas-install.sh:179–180` | §5 MA Web 检查（`curl :8095` 断言 200） | 删除，改为 `/api/netease/status` 检查 |
| `deploy/nas-install.sh` 末尾 | “下一步：…（MA 界面点 QR Login）” | 改为 Hub Web 面板扫码 |
| `deploy/nas-install.sh:87–90` | `/volume1/music` 不存在记 FAIL | 改为只 WARN（music_folder 可选） |
| `deploy/NAS_CHECKLIST.md:26–27` | §1.2/1.3：netease 两个地址、`music-assistant` 容器名 | 只留 `api:3000`，删 MA 名 |
| `deploy/NAS_CHECKLIST.md:34` | §2.2：8 个容器含 `oxn-music-assistant`；§2.1：pull 6 个镜像 | 改为 7 个 / 5 个 |
| `deploy/NAS_CHECKLIST.md:43` | §3 端口表 8095 行（MA Web，200） | 删除；新增 8080 `/api/netease/status` 行 |
| `deploy/NAS_CHECKLIST.md:64–68` | §5 网易云扫码登录全节（MA 界面 5 步） | 整节重写为 Hub Web 面板扫码 |
| `deploy/NAS_CHECKLIST.md:108` | 故障速查“MA 里看不到 Snapcast 播放器”（mDNS/bridge） | 删除（MA 已不存在）；改为 404 路由排查行 |
| `deploy/NAS_CHECKLIST.md:120` | 镜像核实表 `ghcr.io/music-assistant/server:latest`“存在” | 改为“已删（融合 B）” |
| `deploy/NAS_CHECKLIST.md:17` | §0.3 建 `/volume1/music`（必做） | 改为可选 |
| `deploy/README.md:27` | 服务表 `music-assistant` 行（8095） | 删除 |
| `deploy/README.md:50,54–56,60–62` | G 节：镜像核实含 MA、config 容器名含 `music-assistant:8095`、MA bridge/mDNS 已知问题 | MA 从核实表删除；只留 `netease-api:3000`；mDNS 问题标注已删除 |
| `deploy/README.md:75,82–…` | §网易云扫码登录全节（MA 界面 QR Login、Cookie 存 `ma-data`） | 整节重写（见 §3） |
| `deploy/README.md:26,131` | mpd 行“读 /volume1/music”、诚实说明“`/volume1/music` 不存在”是失败主因 | 改为可选兜底 |
| `deploy/README.md` | 无“部署迁移说明” | 新增“2026-11-18 音乐融合 B”一节（含迁移说明） |
| `README.md:17,28`（仓库根） | “Music Assistant 的宿主”、架构图“网易云 (Music Assistant + NeteaseCloudMusicApi)” | 改为直连表述 |
| `docs/DECISIONS.md:34` | 2026-11-17 条“启动时自动校验，不存在再问”（music_folder） | 旧条不改；顶部新增 2026-11-18 融合 B 条，注明取代关系 |

依赖检查（砍 MA 前已核实）：没有任何服务依赖 MA——`hub` 的 `depends_on` 只有
whisper/piper/kws-sidecar，`snapserver` 只依赖 mpd；没有任何服务在 `environment`/`links` 里
引用 MA；`netease-api` 也无任何服务依赖它（Hub 经 `127.0.0.1:3000` 直连）。

### 1.2 Hub 面——引用（盘点时属 Hub 面；截至本轮末，大部分已由 Hub 工人在工作树处理，见 §2.2）

| 位置 | 内容 | 本轮末状态（工作树，未提交） |
|---|---|---|
| `hub/config.yaml:59`（HEAD 版） | `netease.music_assistant: "http://music-assistant:8095"`（另有 `netease.api`，保留） | 工作树已删该键；注释改为直连说明 + Cookie 路径 |
| `hub/cmd/hub/main.go:448–456`（HEAD 版） | `MA_TOKEN` 为空即不启用 MA、点歌走 MPD 降级；否则 `music.NewMA(...)` | 工作树已无 `MA_TOKEN` / `MAClient` 命中；`MusicFolderWarning` 已接进启动（`main.go:416`） |
| `hub/cmd/hub/main.go:536–538,567,611–613`（HEAD 版） | 启动时查 MA `Players()` 取默认 `QueueID`；`rt` 注入 `MA`；`QueueID` 热更新 | `hub/internal/router/router.go` 改动中（工作树 +361 行），细节以 Hub 面汇报为准 |
| `hub/internal/music/music.go:11–16,319–459`（HEAD 版） | `MAClient`（POST `/api`、Bearer）及 `Players/PlayMedia/PlayTrackByName/Queue*` | 工作树已无 `MAClient` / `NewMA` 命中（已删） |
| `hub/internal/router/router.go:42,200`（HEAD 版） | `Router.MA *music.MAClient`、`DefaultQueueID(ctx, ma)` | 改动中，见上 |
| `hub/internal/config/config.go:88,195`（HEAD 版） | `Netease.MusicAssistant` 字段 + `Validate()` 的下游 URL 校验 | 字段已删；`Load` 增加旧键硬报错（`checkDeprecatedMusicAssistant`，见 §5 Q7） |
| `hub/internal/config/config_test.go:106`、`hub/internal/router/router_p1_26_test.go:195–217` | 含 MA 的测试（假 MA 服务） | 改动状态以 Hub 面汇报与全量测试为准 |
| `hub/internal/web/` | 8 条约定路由（11 个 handler） | **已落地（工作树，未提交）**：`web.go:173–183` 全部注册；新文件 `hub/internal/music/netease_auth.go`（281 行）管扫码登录与 Cookie |

### 1.3 盘点中被证伪的两处基线描述

1. **`MA_TOKEN` 从未进过部署文件。** 任务基线说“`MA_TOKEN` 读自环境变量”，代码层面属实
   （`hub/cmd/hub/main.go:452` 读环境变量），但全仓 grep 证实：`deploy/.env.example`、
   `deploy/docker-compose.yml`、`deploy/nas-install.sh` 里从来没有 `MA_TOKEN`，
   compose 从未把它传进 Hub 容器。后果：**旧部署里 Hub 的 MA 路径实际一直未启用**
   （`MA_TOKEN` 为空 → `main.go:453` 记日志、点歌走 MPD 降级），砍 MA 不改变已部署的
   实际行为。迁移时 `.env` 要删的只是“如果旧 `.env` 里曾手动加过 `MA_TOKEN`”那一行；
   `.env.example` 本轮无项可删，只加了废弃注释。
2. **8 条 Hub Web API 路由在盘点初一条也不存在，但本轮末已落地（工作树，未提交）。**
   任务给出的约定路由（`GET /api/netease/status`、`POST /api/netease/login/qr`、
   `GET /api/netease/login/qr/check?key=K`、`POST /api/netease/logout`、
   `GET /api/music/status`、`POST /api/music/pause|resume|next|previous`、
   `GET /api/music/queue`、`GET /api/music/lyric?id=N`）在本轮开始时 `hub/internal/web/` 里
   全仓 grep 无命中（只有 `web_test.go:228` 一个 `file://` 校验用例提到 netease）。
   本轮末复核时，Hub 工人已在工作树落地：`web.go:173–183` 注册 11 个 handler、
   新文件 `hub/internal/music/netease_auth.go`（281 行）实现扫码登录与 Cookie 存取。
   本轮部署文档按约定路由写；`nas-install.sh` 的 `/api/netease/status` 检查对 404 只警告、
   不记 FAIL（旧二进制上就是 404）。路由的响应字段见 §5（Q2–Q5 已据工作树确认）。

### 1.4 有意保留、不改的残留

- **历史审计文档**：`AUDIT_REPORT.md`、`AUDIT_ROUND2.md`、`FIXES_ROUND2.md`、
  `TEST_REPORT.md` 旧章节（P1 一节 L485–486 的 `MA_TOKEN` 决策与 P1-4 的“MA 8095 保留局域网发布”）、
  `.audit-round2/*.md`——都是带日期的历史记录，**不改写历史**；TEST_REPORT.md 只追加新节（§6）。
  其中 P1-4 的“MA 8095 保留局域网发布（扫码入口）”已被融合 B 明确推翻，见本文件与 PLAN.md 新增决策条。
- **`.semgrep/rules.yml:121`**：`MA_TOKEN` 仍在密钥检测正则里——**有意保留**，
  删掉会削弱检测；若将来有人误引回 MA_TOKEN，规则仍会告警。
- **`client/Cargo.lock:98`**：`8095` 是一串校验和里的子串，不是端口，与 MA 无关。
- **`deploy/hub-linux-amd64`**：旧二进制含 MA 字符串（grep 命中），待 Hub 面重编后自然消失。
- **deploy 文档里的 MA 字样**：全部是“已砍/已删/已废弃”类迁移注释，不是活引用
  （复核 grep 见 §6）。

## 2. 改动清单

### 2.1 部署面（本轮已改，未 commit、未 push）

| 文件 | 改了什么 |
|---|---|
| `deploy/docker-compose.yml` | 删 `music-assistant` 服务整块与 `ma-data` 卷；头注释加融合 B 说明；mpd 注释改为可选兜底；`/volume1/music` 不再列为 up 失败主因。服务 8 → 7：hub / whisper / piper / kws-sidecar / snapserver / mpd / netease-api |
| `deploy/.env.example` | 无 `MA_TOKEN` 可删；`MUSIC_FOLDER` 标注为可选本地兜底；加注释说明 `MA_TOKEN` 已废弃、旧 `.env` 如有该行请删 |
| `deploy/nas-install.sh` | 删 8095 端口占用/连通检查与 MA Web 检查；容器数断言 8 → 7；§3 不再改写 8095 地址，改为删掉 NAS 副本里已废弃的 `music_assistant:` 行并断言已删（新 Hub 读到该行会拒绝启动）；`/volume1/music` 不存在改只 WARN；新增 Hub `/api/netease/status` 登录态检查（200 + `logged_in:true` 判已登录，未登录/404 只 WARN 不 FAIL；401 才 FAIL；不打印响应体）；`_web_tok` 赋值前移（原在 `/api/status` 检查前才赋值，新检查在前，`set -u` 下会炸——已修）；末尾下一步改为 Hub Web 面板扫码 |
| `deploy/NAS_CHECKLIST.md` | §0.3 建音乐文件夹改为可选；§1.2/1.3 只留 `api:3000`；§2.1 pull 6 → 5 个镜像、§2.2 容器 8 → 7；§3 删 8095 行、加 `/api/netease/status` 行；§5 整节重写为 Hub Web 面板扫码（含 SSH 隧道步骤）；故障速查删 MA 播放器行、301 行改为 Hub 版、加 404 路由排查行；镜像表 MA 行改为已删 |
| `deploy/README.md` | 服务表删 MA 行，mpd 行标注可选兜底，netease-api 行改 Cookie 由 Hub 存 hub-data、约 1–2 个月；新增“2026-11-18 音乐融合 B”一节（含部署迁移说明）；G 节同步（核实表删 MA、只留 `netease-api:3000`、mDNS 问题标注已删除）；§网易云扫码登录整节重写为 Hub Web 面板流程；诚实说明去掉 `/volume1/music` 不存在这一失败主因 |
| `README.md`（仓库根） | NAS 行与架构图的两处 MA 表述改为直连 |
| `docs/DECISIONS.md` | 顶部新增 2026-11-18 融合 B 决策条（旧 2026-11-17 条不改，注明取代关系） |
| `MUSIC_FUSION.md`（本文件，新建） | 盘点、改动清单、迁移说明、取舍、待 Hub 面确认 |
| `TEST_REPORT.md` | 末尾追加“音乐融合 B”一节（见 §6；最终全量数字留空位，待协调员填入） |
| `PLAN.md`（仓库外） | FR-05、架构图、§4.2 选型表、§4.3、Phase 2 行、部署行更新；决策日志追加第 13 条；**未进本 PR** |

### 2.2 Hub 面（另一位工人进行中；以下为本轮末工作树现状，未提交，以 Hub 面汇报与最终 diff 为准）

- 删 `MAClient`（`hub/internal/music/music.go`）：工作树已无 `MAClient` / `NewMA` 命中；
  点歌主路改为 Hub → netease-api → MPD（现成降级路径升为主路）；
- 删 `main.go` 的 `MA_TOKEN` 读取与 MA 客户端创建：工作树已无 `MA_TOKEN` 命中；
- 删 `hub/config.yaml` 的 `netease.music_assistant` 与 `config.go` 的对应字段：
  工作树种子配置已删该键；`Load` 增加 `checkDeprecatedMusicAssistant`——旧配置里还留着该键时
  **明确报错、拒绝启动**（`config.go:423–444`，不是静默忽略）。因此 `nas-install.sh` 第 3 步
  会删掉 NAS 副本里的该行并断言已删（部署面已做，见 §2.1）；
- 新接 8 条 Web 路由（11 个 handler）：`web.go:173–183` 全部注册；新文件
  `hub/internal/music/netease_auth.go`（281 行）实现扫码登录与 Cookie 存取；
- Cookie 落盘到 `hub-data` 卷：`<data_dir>/netease-cookie.json`（0600，原子写临时文件后改名），
  过期时间优先取 cookie 的 `Expires=`、其次 `Max-Age=`（相对保存时间），都没有按 1–2 个月估算；
- `music_folder` 改为可选兜底：`ValidateStartup()` 不再因文件夹缺失 Fatal（`config.go:322–332`，
  仅空值仍报错）；新增 `MusicFolderWarning()`（`config.go:335–344`），`main.go:416` 在启动时打出警告；
  `Validate()` 的空值拒绝（`config.go:185–186`）仍在——空值仍是配置错误，但经 compose 默认值兜底，
  部署路径上 Hub 收到的一定是非空路径；
- 重编 `deploy/hub-linux-amd64`：仍待做（旧二进制含 MA 字符串、没有新路由）。

## 3. 迁移说明（给 bai / 给协调员汇总用）

### 3.1 `.env` 要删的项

- **`MA_TOKEN`**：旧 `.env` 里如果曾手动加过这一行，删掉它。注意：仓库模板与 compose 从未传过它，
  多数部署里这一行根本不存在；Hub 也不再读它。
- **`music_assistant` 相关的配置项**：不在 `.env` 里，在 `hub/config.yaml` 的
  `netease.music_assistant` 一行——种子配置里的该行已由 Hub 面删除；NAS 上旧副本里的该行
  由 `nas-install.sh` 第 3 步自动删掉并断言（新 Hub 读到它会直接拒绝启动，所以不要留着）。

### 3.2 首次部署扫码步骤（改去 Hub Web 面板）

1. 电脑上建 SSH 隧道：`ssh -L 8080:127.0.0.1:8080 root@NAS_IP`（保持终端不退出）。
   面板只绑回环（`web_listen: 127.0.0.1:8080`），浏览器不能直连 `NAS_IP:8080`。
2. 浏览器开 `http://127.0.0.1:8080` → Hub Web 面板 → **网易云登录**页（Web Token 用
   `deploy/.env` 里的 `OPEN_XIAOAI_WEB_TOKEN`；不要把 Token 发给别人或贴进群聊）。
3. 点**生成二维码**（`POST /api/netease/login/qr`）→ 手机网易云音乐 App 扫码 → App 内点确认登录
   （面板后台轮询 `GET /api/netease/login/qr/check?key=K`）。
4. 面板状态变**已登录**；也可调 `GET /api/netease/status` 核对（应为 `logged_in:true`，
   另返回昵称与过期时间；未登录时 `needs_rescan:true`）。
5. 面板搜一首非 VIP 歌点播放，或语音“你好小李，播放周杰伦的青花瓷”，确认出声。

### 3.3 其他迁移点

- **Cookie 约 1–2 个月过期需重扫**：过期表现是搜歌/播歌报 301 或歌单变空；回面板重扫即可，
  不用改配置、不重建容器。
- **`music_folder` 现在是可选本地兜底**：`/volume1/music` 不建、不放歌也不阻塞启动；
  想放本地歌时再建（MPD 读它作降级/本地播放）。compose 里该路径的挂载仍保留，
  不存在时 Docker 会自动建一个空目录，无害。
- **旧 MA 数据清理（可选）**：新链路能播歌后，`docker volume ls | grep ma-data` 确认卷名再删
  （如 `docker volume rm <卷名>`）。删了 MA 的登录态就作废——没关系，Hub 侧登录是独立的。
  **不要**用 `docker compose down -v`，那会连 `hub-data` 等其他卷一起删掉。
- **容器/镜像数**：容器 8 → 7，`pull` 镜像 6 → 5；8095 端口释放，不再占用、不再检查。

## 4. 已知取舍

- **HA 里没了现成的音乐 `media_player`**：以前是 MA 向 HA 提供音乐播放器实体，砍 MA 后 HA 侧
  没有音乐入口。语音点歌与 Hub Web 控制不受影响。以后要不要由 Hub 自己向 HA 暴露播放状态，
  只提方案、不做：方向是复用 Hub 已有的 HA WebSocket 连接（`wss://NAS:8123`），由 Hub 向 HA
  注册/更新一个 `media_player` 实体（上报播放状态 + 接受播放/暂停/切歌），或走 HA 的 Wyoming
  语音通道；两种都未设计、未排期，需 bai 定。
- **网易云接口漂移仍由 `netease-api` 上游扛**：`binaryify/netease_cloud_music_api` 是非官方 API，
  随时可能失效；Hub 直连没有改变这一风险，只是少了一层转发。
- **少一层不等于少一处故障点**：登录态从 MA 的 `ma-data` 卷改到 Hub 的 `hub-data` 卷，
  卷丢了照样要重扫；`hub-data` 卷的备份重要性因此更高（证书、提醒、记忆、登录态都在里面）。
- **LX06 flac 限制（§5 Q10，已决）**：旧文档说“LX06 不支持 flac，MA 里强制转 mp3/aac”。融合 B 后不需要 Hub 转码：MPD 解码（mp3/flac 都能解）→ PCM 进 snapfifo（48kHz）→ snapserver 编 Opus → 音箱只收 Opus，根本见不到 flac。MA 的强制转码在这条链路上是多余的。待 NAS 实测确认 MPD 播网易云 URL 流无声不断。
- **歌单/收藏浏览（§5 Q11，未做，留作后续）**：本轮路由只有点歌/队列/歌词，没有歌单/收藏浏览接口；FR-05 的“歌单、收藏”验收口径因此收窄为“语音点歌 + 队列 + 歌词”。是否补歌单浏览，由 Friday 与 bai 另定，不在本 PR 做。

## 5. 待 Hub 面确认

| 编号 | 问题 | 部署面的现状/影响 |
|---|---|---|
| Q1 | Cookie 在 `hub-data` 卷里的具体文件名（与路径） | 文档只写了“存 hub-data 卷”，文件名未写；影响备份与故障排查 |
| Q2 | `GET /api/netease/status` 的响应字段名（`logged_in`？） | `nas-install.sh` 按 `"logged_in":true/false` 做最佳努力匹配，未命中时只报“接口可达”；字段名定了之后脚本可收紧 |
| Q3 | `POST /api/netease/login/qr` 的响应形态（`key` 字段名？二维码是 URL 还是 base64？有效期？） | 文档按约定路由写，未写响应细节 |
| Q4 | `GET /api/netease/login/qr/check?key=K` 的返回码语义（801/802/803/800 是否沿用 netease-api 原语义） | 文档沿用了旧 MA 版的三步接口语义，未经 Hub 面确认 |
| Q5 | `GET /api/music/status`、`POST /api/music/pause\|resume\|next\|previous`、`GET /api/music/queue`、`GET /api/music/lyric?id=N` 的请求/响应形态 | 文档未写细节；语音点歌与 Web 控制的验收依赖这些路由 |
| Q6 | Hub Web 面板访问方式：`web_listen` 是否仍是 `127.0.0.1:8080`？bai 是走 SSH 隧道开面板，还是面板要改绑？ | 本轮文档（checklist §5、README）按 SSH 隧道写；若 Hub 面改了访问方式，文档要同步改 |
| Q7 | `hub/config.yaml` 的 `netease.music_assistant` 删除 + `config.go` 字段/校验删除 | 已核实删键不阻塞启动校验（`validateDownstreamURL` 空值放行）；删除本身归 Hub 面 |
| Q8 | `main.go` 的 `MA_TOKEN` 环境变量读取删除；旧 `.env` 如有该行是否忽略即可 | 部署面已在迁移说明里写“删掉那一行” |
| Q9 | `music_folder` 改 warn：`Validate()` 空值拒绝与 `ValidateStartup()` 不存在 Fatal（`config.go:183–184,323–335`，`main.go:411`）+ 测试 `TestValidateStartupMusicFolder` 同步 | 部署面的检查与文档已改成只警告；Hub 不改，NAS 上 Hub 启动仍会 Fatal，部署面的改动等于没生效 |
| Q10 | LX06 flac 限制：MA 的强制转码去掉后，MPD 解码 + Snapcast 是否覆盖，有无无声风险 | **已决（协调员汇总）**：不需要 Hub 转码。MPD 解码 → snapfifo 48kHz PCM → snapserver 编 Opus，音箱只收 Opus；待 NAS 实测确认 MPD 播网易云 URL 流 |
| Q11 | 歌单/收藏浏览是否进 Web 面板（还是只保留点歌/队列/歌词） | **未做，留作后续**：本轮无歌单/收藏浏览接口；FR-05 验收口径收窄为点歌/队列/歌词；是否补由 Friday 与 bai 另定，不在本 PR 做 |
| Q12 | `deploy/hub-linux-amd64` 何时重编（含新路由的 Hub 二进制） | **已决**：协调员汇总时已重编（12,018,496 字节，含 11 条新路由，`music-assistant:8095`/`MA_TOKEN` 零命中） |
| Q13 | HA 音乐 `media_player` 缺失是否接受 | **已决：接受**。bai 选 B 时已知该代价（语音点歌与 Hub Web 控制不受影响）；以后补的方案见 §4，不在本轮做 |

## 6. 本轮校验（部署面，2026-11-18）

- `python3 -c 'import yaml; yaml.safe_load(open("deploy/docker-compose.yml"))'` 结构校验通过：
  7 个服务 `hub / whisper / piper / kws-sidecar / snapserver / mpd / netease-api`；
  6 个卷 `hub-data / whisper-data / piper-data / kws-data / snapfifo / mpd-data`（无 `ma-data`）；
  `depends_on`：hub → whisper/piper/kws-sidecar，snapserver → mpd，其余无依赖——
  无任何服务依赖 MA，砍 MA 不断链。
- `bash -n deploy/nas-install.sh` 通过。
- `docker compose config` **未做**：本机无 docker（`command -v docker` 无输出），以 `python3 -c 'import yaml'` 解析代替；
  NAS 上首次 `up` 前请跑一次 `docker compose config` 核对。
- grep 复核（`git grep` + `grep -r`，2026-11-18）：
  - deploy/ + 仓库根 README + `docs/DECISIONS.md`：无活 MA 引用；剩余 MA/`8095` 字样全部是
    “已砍/已删/已废弃”类迁移注释（compose 头注释、checklist 镜像表“已删”行、README 迁移节、
    nas-install.sh 的删除说明），无服务、无配置项、无检查项还在用 MA 或 8095。
  - `MA_TOKEN` 在 deploy/ 里只出现在迁移注释与 README 迁移步骤里，无活引用；
    compose 的 `environment` 里确认没有 `MA_TOKEN`。
  - 全仓剩余 `music-assistant` / `MA_TOKEN` / `8095`：历史审计文档（不改写历史）、
    `.semgrep/rules.yml:121`（有意保留的密钥检测）、Hub Go 代码与 `hub/config.yaml`
    的 HEAD 版本（工作树已处理，见 §1.2）、`client/Cargo.lock` 校验和巧合、
    `deploy/hub-linux-amd64`（协调员汇总时已重编，见 §5 Q12）。`MusicAssistant` / `MAClient` 的残留
    在工作树里已清零（`music.go` / `main.go` 均无命中），以 Hub 面最终 diff 为准。
  - deploy/ 里“不存在再问 / 必须存在 / Fatal”说法已清零（仅剩 nas-install.sh 一处注释说明
    Hub 侧 Fatal 待改，见 Q9）。
- Go 测试：**本轮未跑**（任务要求不跑 Go 全量）；最终全量数字见 TEST_REPORT.md 新增一节的留空位，
  待协调员填入。

## 7. 未进 PR 的说明

- 本轮**未 commit、未 push**（按任务要求）；改动文件见 §2.1，由协调员统一提交。
- PLAN.md 在仓库外，已同步更新（FR-05、架构图、§4.2、§4.3、Phase 2 行、决策日志第 13 条），
  但它不在本仓库里，**未进本 PR**，需协调员同步。
- 未动 `firmware/`；未重新引入 MA 或任何新容器；未写任何密码、token、cookie 进文件、报告或日志。
