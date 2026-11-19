# NAS 检查清单（UGREEN DXP6800 Pro + UGOS）

按顺序做，每步做完对一下“预期结果”。做完一步再做下一步。
**状态：全部未在真 NAS 上验证（关口 2 BLOCKED）。** 哪一步的结果和预期不一样，
把命令输出原样贴回来，不要猜着往下走。

前置信息：NAS 内网 IP 记为 `NAS_IP`（192.168.88.247）；部署目录 `NAS_DIR=/volume4/docker/open-xiaoai-next`（2026-11-19：NAS 的 Docker 根目录是 /volume4/docker，不是 /volume1/docker）。
可用两种方式执行：跑 `deploy/nas-install.sh`（自动做 0–5 步的检查），或照下面手工做。
脚本本身只在本机做过 `bash -n` 校验，NAS 上实际运行未验证。

## 0. 前置准备

| # | 做什么 | 预期结果 |
|---|---|---|
| 0.1 | NAS 开 SSH（UGOS → 控制面板 → 终端机 / SSH），记下登录账号 | 能从电脑 `ssh huajun@NAS_IP` 登录（2026-11-19：NAS 账号是 huajun，不是 root） |
| 0.2 | UGOS 确认 Docker 已装（自带 Docker 管理器），`docker --version` + `docker compose version` 都有输出 | 两个命令都有版本号 |
| 0.3 | （可选）本地歌兜底：在 UGOS 文件管理里确认 `/volume1/Music`（音乐在 /volume1/Music，不是 /volume1/music，大小写敏感），放几首 MP3/FLAC 测试 | `ls /volume1/Music` 能看到音乐文件；**不建也不阻塞启动**（music_folder 是可选本地兜底，网易云直连是主路） |
| 0.4 | 本机：`cp deploy/.env.example deploy/.env`，填 `NAS_IP` / `HASS_TOKEN` / `LLM_API_KEY` / `OPEN_XIAOAI_TOKEN` / `OPEN_XIAOAI_WEB_TOKEN`（后四个不能为空；Hub 没 `OPEN_XIAOAI_TOKEN` 会直接退出，没 `OPEN_XIAOAI_WEB_TOKEN` 时 Web 面板 /api/* 一律 401；两个 Token 必须不同，各用 `openssl rand -hex 16` 生成） | `.env` 里五项都有值 |
| 0.5 | 准备网易云账号（手机上装好网易云音乐 App）；要听 VIP 歌的话账号本身得是 VIP | 能正常登录网易云 App |

## 1. 传文件到 NAS

| # | 做什么 | 预期结果 |
|---|---|---|
| 1.1 | `NAS_SSH=huajun@NAS_IP NAS_DIR=/volume4/docker/open-xiaoai-next HUB_PORT=4399 ./deploy/nas-install.sh`（不带 `up`，只传文件+检查；HUB_PORT 部署时定，默认 4399=停老 Hub 直接接管，4400=与老 Hub 并行） | 末尾汇总 `FAIL=0`；文件在 `$NAS_DIR/deploy/`（docker-compose.yml / Dockerfile.hub / hub-linux-amd64 / snapserver.conf / .env）、`$NAS_DIR/hub/config.yaml`，以及 `$NAS_DIR/kws-sidecar/`（构建源文件：Dockerfile / server.py / entrypoint.sh / requirements.txt / scripts/——compose 里边车是 `build: ../kws-sidecar`，只传 deploy/ 的话 `up` 会因构建上下文缺失而失败，2026-11-18 P2 轮已补传） |
| 1.2 | 脚本第 3 步会自动改 NAS 上的 `config.yaml`：netease 的 `api` 地址改成 `127.0.0.1:3000`，kws.url 若是旧容器名 `ws://kws-sidecar:10400` 也改成 `ws://127.0.0.1:10400`，HA 地址改成 `NAS_IP:8123`（保持原有 ws/wss 协议，不降级成明文），agents 的 `hermes`/`openclaw` 地址改成 `NAS_IP:8642` / `NAS_IP:18789`（容器名在 host 网络下解析不到，openclaw 只绑 LAN IP），`music_folder` 改成 `/volume1/Music`，`listen` 改成 `:$HUB_PORT`（部署时定的端口，默认 4399）。每处改写都有 grep 断言，任一处没改上脚本即 FAIL（NAS 上已有 config 时不覆盖，先备份为带时间戳的 `config.yaml.bak.YYYYMMDDHHMMSS`；首次部署才传种子配置）。2026-11-18 融合 B：`music_assistant` 地址不再改写——该行已废弃，脚本会直接删掉 NAS 副本里的这一行（新 Hub 读到它会拒绝启动）；种子配置里的该行由 Hub 面删除 | 屏幕打印出的 `api:` / `url:` 行是 `127.0.0.1` / `NAS_IP`，`kws:` 下的 `url:` 是 `ws://127.0.0.1:10400/v1/kws`，`listen:` 是 `:$HUB_PORT`；打印结果里不应再有 `music_assistant:` |
| 1.3 | 原因（不用改回来）：Hub 跑在 host 网络下，Docker 容器名 `netease-api` / `kws-sidecar` / `hermes` / `openclaw` 解析不到，必须用 `127.0.0.1` 或 NAS 的 LAN IP；kws-sidecar 与 Hub 一样走 host 网络（见 `deploy/docker-compose.yml`）；本机 `hub/config.yaml` 没动 | — |

## 2. 拉镜像并启动

| # | 做什么 | 预期结果 |
|---|---|---|
| 2.1 | `NAS_SSH=huajun@NAS_IP NAS_DIR=/volume4/docker/open-xiaoai-next HUB_PORT=4399 ./deploy/nas-install.sh up`，或手工：`cd $NAS_DIR/deploy && docker compose pull && docker compose up -d`（HUB_PORT 与 1.1 保持一致） | `pull` 5 个镜像都成功（见镜像核实表；`binaryify/netease_cloud_music_api` 是修正后的官方镜像；hub 与 kws-sidecar 是本地构建，不拉取；mosquitto、Music Assistant 已删，不再拉取）；`up -d` 无报错 |
| 2.2 | 等 15 秒（Whisper 首次要下载 small-int8 模型，约 500MB 左右；kws-sidecar 首次还要下载约 5.6MB 唤醒模型到 `kws-data` 卷，`docker compose ps` 里它会晚一点变健康），再 `docker compose ps` | 7 个容器状态全是 `Up`：oxn-hub / oxn-whisper / oxn-piper / oxn-kws-sidecar / oxn-snapserver / oxn-mpd / oxn-netease-api |

## 3. 端口检查（NAS 上跑）

| 端口 | 服务 | 检查命令 | 预期结果 |
|---|---|---|---|
| 10300 | whisper（STT） | `timeout 3 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/10300'` | 能连通（Wyoming 是 TCP 协议不是 HTTP，curl 会返回空，这是正常的） |
| 10200 | piper（TTS） | 同上，端口 10200 | 能连通 |
| 10400 | kws-sidecar（服务端唤醒） | `curl -s -m 5 http://127.0.0.1:10400/health` | 返回 `{"status":"ok","model_loaded":true}`（模型未加载好时 503；模型下载失败容器会报错退出，见故障速查） |
| 3000 | NeteaseCloudMusicApi | `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/` | `200`（只绑 127.0.0.1，NAS 外打不开是正常的；以前 `head -c 200` 的检查 404/500 也算通过，已改成断言 200） |
| 8080 | Hub Web 面板 | `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' $NAS_DIR/deploy/.env \| cut -d= -f2-); curl -s -m 5 -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/status` | 返回 JSON，含 `"wake_word":"你好小李"`，如 `{"devices":2,"online":0,...}`（无 Token 时一律 401，不是面板坏了） |
| 8080 | Hub Web 面板（网易云登录态） | `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' $NAS_DIR/deploy/.env \| cut -d= -f2-); curl -s -m 5 -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/netease/status` | 返回 200；已登录时 `logged_in:true`，未登录时只警告、去 §5 扫码（404 说明 Hub 二进制还没接该路由，以 Web 面板显示为准，见 MUSIC_FUSION.md“待 Hub 面确认”） |
| 1704 / 1705 | snapserver（流/控制） | `ss -tln \| grep -E ':1704|:1705'` | 两个端口都在监听 |
| 4399 | Hub wss（音箱连） | `ss -tln \| grep ':<HUB_PORT>'`（HUB_PORT 部署时定，默认 4399；并行时是 4400） | 在监听（TLS 开启时是 wss） |

## 4. 日志检查

| # | 做什么 | 预期结果 |
|---|---|---|
| 4.1 | `docker compose logs --tail=50 hub` | 出现 `open-xiaoai-hub 已启动：唤醒词 "你好小李"，监听 :<HUB_PORT>`（HUB_PORT 部署时定，默认 4399）和 `Web 面板：http://localhost:8080`；STT/TTS 连不上时会记 `Wyoming STT/TTS 连接失败`——刚启动时出现一次是正常的，等 whisper/piper 起好后 Hub 会重连（若一直失败，看 3 的端口） |
| 4.1a | 同上，看 KWS 一行 + `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' $NAS_DIR/deploy/.env \| cut -d= -f2-); curl -s -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/status` | Hub 日志出现 `KWS 边车健康检查通过: ws://127.0.0.1:10400/v1/kws`；`/api/status` 里 `"kws_status":"ok"`（若是 `"down"`，看 10400 的 `/health` 与 `docker compose logs kws-sidecar`；刚重启边车后 `kws_status` 最多滞后一个 30s 健康检查周期才回 `ok`，唤醒本身恢复是即时的） |
| 4.1b | `docker compose logs kws-sidecar \| tail -20` | 出现 `KWS model ready` / 监听 `ws://127.0.0.1:10400/v1/kws`（compose 传 `KWS_HOST=127.0.0.1`；镜像默认也是 `127.0.0.1`，见 kws-sidecar/README.md“安全假设”）（模型首次下载失败会明确报错退出，`kws-data` 卷里文件不全时重起会重新下载；不要手动往卷里拷模型） |
| 4.2 | `docker compose logs --tail=30 hub \| grep '设备注册制'` | 应有 `设备注册制：…` 行（P1-44 注册制：白名单为空时 `register` 一律拒绝；配好指纹后音箱发 `register` 才注册。自动发现扫描已删，不要再查“发现/扫描”——旧写法已废弃）（本机冒烟时无真设备，只验证到日志格式；NAS 上有音箱刷好后才会注册） |
| 4.3 | `docker compose logs --tail=100 \| grep -iE 'fatal|panic'` | 无输出（有则把对应容器日志贴回来） |
| 4.4 | `docker compose logs snapserver \| tail -20`、`docker compose logs mpd \| tail -20` | 无 ERROR/FATAL；snapserver 应加载到 `snapserver.conf` 里 `NAS音乐` 这个流 |

## 5. 网易云扫码登录（Hub Web 面板，融合 B，见 README §网易云扫码登录）

> 2026-11-18 融合 B：Music Assistant 已砍，扫码点从 MA 界面（`NAS_IP:8095`）改到 **Hub Web 面板**。8095 端口已不存在，不要再开它。
> 面板只绑回环（`hub/config.yaml` 里 `web_listen: 127.0.0.1:8080`），浏览器不能直连 `NAS_IP:8080`——先在电脑上建 SSH 隧道再开面板（若 Hub 面改了面板访问方式，以 MUSIC_FUSION.md 为准）。

| # | 做什么 | 预期结果 |
|---|---|---|
| 5.0 | 电脑上建隧道：`ssh -L 8080:127.0.0.1:8080 huajun@NAS_IP`（保持该终端不退出），浏览器开 `http://127.0.0.1:8080` | 看到 Hub Web 面板（要输 Web Token 时，用 `deploy/.env` 里的 `OPEN_XIAOAI_WEB_TOKEN`；不要把 Token 发给别人或贴进群聊） |
| 5.1 | 面板里进**网易云登录**页，点**生成二维码**（调 `POST /api/netease/login/qr`） | 页面显示二维码（有效期约 2 分钟，过期点重新生成） |
| 5.2 | 手机网易云音乐 App 扫码，App 内点确认登录（面板后台轮询 `GET /api/netease/login/qr/check?key=K`） | 面板状态变**已登录**；`curl -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/netease/status` 返回 `logged_in:true` |
| 5.3 | 面板里搜一首歌点播放（先选非 VIP 的），或语音“你好小李，播放周杰伦的青花瓷” | 能出声（Hub → netease-api → MPD → Snapcast → 音箱；音箱未刷好前先在面板确认能播） |
| 5.4 | Cookie 约 1–2 个月过期后 | 搜歌/播歌报需要登录时，回面板重新扫码即可，不用改配置、不重建容器 |

## 6. 首次联调

| # | 做什么 | 预期结果 |
|---|---|---|
| 6.1 | `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env \| cut -d= -f2-); curl -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/devices`（Token 在本机 `deploy/.env` 里；Web 面板只绑 127.0.0.1:8080，须先建好 §5.0 的隧道再调 127.0.0.1:8080，**不要**用 `NAS_IP:8080`——面板不在那个地址监听） | 有音箱刷好并连上后，列表里出现设备且 `Online:true`（没刷机前只有配置里的两台占位，`Online:false` 是正常的） |
| 6.1a | 对音箱说“你好小李”（真人唤醒，VM 联调只验过合成语音） | 只有一台应答；另一台静默；Hub 日志出现 `kws: 设备 … 唤醒 "你好小李"`，`/api/status` 的 `kws_status` 为 `ok`（真人唤醒率 1m/3m ≥95% 仍是 BLOCKED，需真机测） |
| 6.2 | 语音“你好小李，播放周杰伦的青花瓷” | 3 秒内出声（FR-05 验收线）；歌词/暂停/下一首可用 |
| 6.3 | 两台音箱都刷好后放同一首歌 | 同组同步播放，延迟差 ≤10ms（关口 3） |

## 7. P1-31 / P1-32 冒烟：重启后数据与配置不丢（2026-11-17 补）

**状态：未在真 NAS 上验证（BLOCKED，无 NAS 实测）。** 本节只在 NAS 上跑，不要在本机猜结果。

| # | 做什么 | 预期结果 |
|---|---|---|
| 7.1 | `up` 后：`docker compose exec hub ls /app/hub-data`，再 `docker compose logs hub \| grep "证书指纹"` 记下指纹 | `/app/hub-data` 里有证书文件、`reminders.json`、`memory.json`（没有也先记下实际内容）；指纹是一串十六进制 |
| 7.2 | 语音认领一台房间（如“你好小李，我在客厅”），或在 Web 面板改一次配置 | 认领后 `WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env \| cut -d= -f2-); curl -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/devices` 里该设备 `Room` 已变（须先建好 §5.0 的隧道；不要用 `NAS_IP:8080`）；NAS 上 `cat $NAS_DIR/hub/config.yaml \| grep -A2 <设备ID>` 能看到新房间 |
| 7.3 | `docker compose restart hub`，等 10 秒，再查一次指纹与房间：`docker compose logs hub \| grep "证书指纹"`、`WEB_TOKEN=$(grep '^OPEN_XIAOAI_WEB_TOKEN=' deploy/.env \| cut -d= -f2-); curl -H "Authorization: Bearer $WEB_TOKEN" http://127.0.0.1:8080/api/devices`（同上，须先建好 §5.0 的隧道） | **指纹与 7.1 记下的一致**（变了说明证书丢了重生成，音箱指纹 pin 会失效）；房间认领还在；`/app/hub-data` 内容还在 |
| 7.4 | 若 7.3 任一项不对 | 停下，把 `docker compose logs hub` 与 `docker compose config` 的输出原样贴回来，不要猜着往下走 |

| 现象 | 含义 |
|---|---|
| 重启后指纹变了 / 提醒没了 | hub-data 卷挂错位置（P1-31 复发），先查 `docker compose config \| grep -A3 hub-data` |
| 认领房间后重启就丢 / Web 改配置报 500 | config.yaml 仍是只读挂载（P1-32 复发），先查 `docker compose config \| grep config.yaml` |

## 8. S-2 冒烟：全新卷直接 `compose up`（2026-11-18 补）

**状态：未在真 NAS 上验证（BLOCKED，无 NAS 实测；本机无 docker，
只审过 Dockerfile 与 entrypoint 逻辑）。** 本节只在 NAS 上跑，
不要在本机猜结果。目的：证明不走 `nas-install.sh` 的 chown
one-shot、只 `compose up` 全新卷也能起来（SC-P2-12 半修的回归项）。

| # | 做什么 | 预期结果 |
|---|---|---|
| 8.1 | 先停边车并删掉它的卷（**只删 kws-data，别用 `down -v`**，那会连 hub-data 一起删）：`docker compose stop kws-sidecar; docker compose rm -f kws-sidecar; VOL=$(docker volume ls -q \| grep kws-data); docker volume rm $VOL`（卷名前缀是 compose 项目名，先 `docker volume ls \| grep kws-data` 核对，不要硬写 `open-xiaoai-next_kws-data`） | 卷已删；`docker volume ls \| grep kws-data` 无输出 |
| 8.2 | 不跑 `nas-install.sh`，直接：`cd $NAS_DIR/deploy && docker compose up -d kws-sidecar`，等 60 秒（首次要下载约 5.6MB 模型） | `up` 无报错；`docker compose ps kws-sidecar` 是 `Up`，不是 `Restarting` |
| 8.3 | `docker compose logs kws-sidecar \| tail -20` 与 `curl -s -m 5 http://127.0.0.1:10400/health` | 日志出现 `KWS model ready` 与监听 `ws://127.0.0.1:10400/v1/kws`；`/health` 返回 `{"status":"ok","model_loaded":true}` |
| 8.4 | 若 8.2/8.3 任一项不对 | 看日志里的 FATAL 行：`is not writable` 说明卷属主仍是 root——按 FATAL 里给的命令跑 chown one-shot（`docker compose run --rm --user root --no-deps --entrypoint /bin/chown kws-sidecar -R 1000:1000 /data/kws/models`）再重起；`does not exist and cannot be created` 说明卷挂载本身有问题，先查 `docker compose config \| grep -A2 kws-data`。把日志原样贴回来，不要猜着往下走 |

## 故障速查

| 现象 | 先查 |
|---|---|
| `docker compose up` 报 `build ... Dockerfile not found` 之类 | 确认 `deploy/Dockerfile.hub` 和 `deploy/hub-linux-amd64` 都传到了 NAS 的 deploy 目录（2026-11-17 已修过一次：原来 `build: ../hub` 但 hub/ 下没 Dockerfile） |
| `up` 报 `pull ... not found` | 看是哪个镜像；`binarytorch/neteasecloudmusicapi` 已证实不存在（Docker Hub 404），正确的是 `binaryify/netease_cloud_music_api`（compose 已改） |
| Hub 容器一起就退，日志 `OPEN_XIAOAI_TOKEN 未设置` | `.env` 里 `OPEN_XIAOAI_TOKEN` 为空，填好重 `up` |
| `curl /api/*` 返回 401 | `.env` 里 `OPEN_XIAOAI_WEB_TOKEN` 未填、太弱或与容器里的不一致：先在 NAS 上 `docker compose config \| grep WEB_TOKEN` 看传没传进去，再核对 `.env` 里的值（两个 Token 必须不同）；改完重 `up` |
| Hub 日志一直 `Wyoming STT/TTS 连接失败` | `docker compose ps` 看 whisper/piper 是否 Up；`curl` 10300/10200 端口；首次启动等够 1 分钟（下载模型） |
| Hub 日志 `唤醒服务暂不可用` 或 `/api/status` 里 `kws_status: down` | 先 `curl http://127.0.0.1:10400/health`：不通/503 → `docker compose logs kws-sidecar` 看模型下载是否失败（失败会明确报错退出；`kws-data` 卷文件不全时重起会重新下载）；通且 `model_loaded:true` 但 Hub 仍报不可用 → 看 Hub 日志里的边车地址是不是 `ws://127.0.0.1:10400/v1/kws`（写成容器名 `kws-sidecar` 时 host 网络解析不到，见 §1.2） |
| `docker compose up` 时 kws-sidecar 反复重启 | `docker compose logs kws-sidecar`：模型下载失败（网络/SHA 校验）会直接退出；卷目录写不进时 entrypoint 会先报可读的 `FATAL: ... is not writable`（S-2 预检，含修复命令），不要只看最后的 `cp` 报错；NAS 出网有问题时先确认能访问 `github.com`（模型在 GitHub releases 上） |
| 10400 端口被占 | kws-sidecar 走 host 网络，10400 被占就起不来：`ss -tlnp \| grep 10400` 查占用者，先停掉再 `up` |
| 搜不到网易云歌 / 报 301 需要登录 | Cookie 过期（约 1–2 个月），回 Hub Web 面板重新扫码（§5）；或在 NAS 上 `curl http://127.0.0.1:3000/login/status` 看登录态（3000 只绑 127.0.0.1，NAS 外打不开是正常的） |
| 面板点生成二维码没反应 / `/api/netease/*` 返回 404 | Hub 二进制还没接网易云登录路由（融合 B 的 Hub 面工作）：先确认 Hub 是不是最新构建（`docker compose logs hub` 看启动行），再按 MUSIC_FUSION.md“待 Hub 面确认”核对 |
| MPD 库是空的 / snapserver 没收到流 | 未核实项：vimagick/mpd 镜像默认配置是否把 `/music` 写进 fifo。先看 `docker compose logs mpd`；如需自定义 mpd.conf，挂一个写好 fifo 输出的配置再重起 |
| Hub 日志 `agents hermes/openclaw 连接失败` | hermes 与 openclaw 都在 NAS 上跑（容器 `hermes` / `openclaw`，2026-11-19 已确认在运行）。先看 `hub/config.yaml` 里的 agents 地址是不是 NAS 的 LAN IP（`http://192.168.88.247:8642/v1` / `http://192.168.88.247:18789/v1`）——写成容器名 `hermes`/`openclaw` 时 host 网络解析不到；再确认两个容器在跑（`docker ps \| grep -E 'hermes\|openclaw'`），hermes 在 127.0.0.1:8642 可达，openclaw 只绑 192.168.88.247:18789 |

## 镜像核实表（2026-11-17，Docker Hub / 官方文档逐个查过）

| 镜像 | 结果 | 依据 |
|---|---|---|
| rhasspy/wyoming-whisper:latest | 存在 | Docker Hub 页面可打开，latest 标签，455.1 MB，2 个月前更新 |
| rhasspy/wyoming-piper:latest | 存在 | Docker Hub 页面可打开 |
| ivdata/snapserver:latest | 存在 | Docker Hub 页面可打开 |
| vimagick/mpd:latest | 存在 | Docker Hub 页面可打开 |
| ghcr.io/music-assistant/server:latest | **已删（2026-11-18 融合 B）** | Music Assistant 容器已砍，compose 里不再拉取；旧记录：MA 官方文档曾明确给出该镜像及 compose 示例 |
| binarytorch/neteasecloudmusicapi:latest | **不存在** | Docker Hub 返回 404；compose 已改为官方镜像 |
| binaryify/netease_cloud_music_api:latest（替代） | 存在 | Docker Hub 页面可打开，官方 `docker pull binaryify/netease_cloud_music_api` |
| kws-sidecar（本地构建，`python:3.12-slim` + sherpa-onnx） | 已修 | `deploy/docker-compose.yml` 的 kws-sidecar 服务：与 Hub 一样走 host 网络，Hub 经 `ws://127.0.0.1:10400/v1/kws` 连接；模型不 bake 进镜像，首次启动由 entrypoint 下载到 `kws-data` 卷（约 5.6MB，SHA256 见 `kws-sidecar/scripts/download-model.sh`），下载失败容器报错退出 |
| hub（本地构建） | 已修 | 原 `build: ../hub` 无 Dockerfile；现用 `deploy/Dockerfile.hub` + 预编译 `deploy/hub-linux-amd64`（12,093,544 字节，SHA256 `973a3b1fa2d34acab7a1feaa5c6fb146bc01167735b4c50ba408d1acd0213091`，2026-11-18 P2 轮收尾从分支尖 afcdcf0 重编，清单 `deploy/hub-linux-amd64.sha256`；旧值 `95535eee…ead7de` / 12,093,552 字节、`1e4f6f6c…c80a` / 11,747,288 字节已取代） |
