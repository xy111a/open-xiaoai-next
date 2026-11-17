# NAS 检查清单（UGREEN DXP6800 Pro + UGOS）

按顺序做，每步做完对一下“预期结果”。做完一步再做下一步。
**状态：全部未在真 NAS 上验证（关口 2 BLOCKED）。** 哪一步的结果和预期不一样，
把命令输出原样贴回来，不要猜着往下走。

前置信息：NAS 内网 IP 记为 `NAS_IP`（如 192.168.1.10）；部署目录 `NAS_DIR=/volume1/docker/open-xiaoai-next`。
可用两种方式执行：跑 `deploy/nas-install.sh`（自动做 0–5 步的检查），或照下面手工做。
脚本本身只在本机做过 `bash -n` 校验，NAS 上实际运行未验证。

## 0. 前置准备

| # | 做什么 | 预期结果 |
|---|---|---|
| 0.1 | NAS 开 SSH（UGOS → 控制面板 → 终端机 / SSH），记下登录账号 | 能从电脑 `ssh root@NAS_IP` 登录 |
| 0.2 | UGOS 确认 Docker 已装（自带 Docker 管理器），`docker --version` + `docker compose version` 都有输出 | 两个命令都有版本号 |
| 0.3 | 在 UGOS 文件管理里建 `/volume1/music`，放几首 MP3/FLAC 测试 | `ls /volume1/music` 能看到音乐文件 |
| 0.4 | 本机：`cp deploy/.env.example deploy/.env`，填 `NAS_IP` / `HASS_TOKEN` / `LLM_API_KEY` / `OPEN_XIAOAI_TOKEN`（后三个不能为空，Hub 没 `OPEN_XIAOAI_TOKEN` 会直接退出） | `.env` 里四项都有值 |
| 0.5 | 准备网易云账号（手机上装好网易云音乐 App）；要听 VIP 歌的话账号本身得是 VIP | 能正常登录网易云 App |

## 1. 传文件到 NAS

| # | 做什么 | 预期结果 |
|---|---|---|
| 1.1 | `NAS_SSH=root@NAS_IP ./deploy/nas-install.sh`（不带 `up`，只传文件+检查） | 末尾汇总 `FAIL=0`；文件在 `$NAS_DIR/deploy/`（docker-compose.yml / Dockerfile.hub / hub-linux-amd64 / snapserver.conf / .env）和 `$NAS_DIR/hub/config.yaml` |
| 1.2 | 脚本第 3 步会自动改 NAS 上的 `config.yaml`：netease 两个地址改成 `127.0.0.1`，kws.url 若是旧容器名 `ws://kws-sidecar:10400` 也改成 `ws://127.0.0.1:10400`，HA 地址改成 `ws://NAS_IP:8123`（原文件备份为 `config.yaml.bak`） | 屏幕打印出的 `music_assistant:` / `api:` / `url:` 三行是 `127.0.0.1` / `NAS_IP`，`kws:` 下的 `url:` 是 `ws://127.0.0.1:10400/v1/kws` |
| 1.3 | 原因（不用改回来）：Hub 跑在 host 网络下，Docker 容器名 `music-assistant` / `netease-api` / `kws-sidecar` 解析不到，必须用 `127.0.0.1`；kws-sidecar 与 Hub 一样走 host 网络（见 `deploy/docker-compose.yml`）；本机 `hub/config.yaml` 没动 | — |

## 2. 拉镜像并启动

| # | 做什么 | 预期结果 |
|---|---|---|
| 2.1 | `NAS_SSH=root@NAS_IP ./deploy/nas-install.sh up`，或手工：`cd $NAS_DIR/deploy && docker compose pull && docker compose up -d` | `pull` 7 个镜像都成功（见镜像核实表；`binaryify/netease_cloud_music_api` 是修正后的官方镜像；hub 与 kws-sidecar 是本地构建，不拉取）；`up -d` 无报错 |
| 2.2 | 等 15 秒（Whisper 首次要下载 small-int8 模型，约 500MB 左右；kws-sidecar 首次还要下载约 5.6MB 唤醒模型到 `kws-data` 卷，`docker compose ps` 里它会晚一点变健康），再 `docker compose ps` | 9 个容器状态全是 `Up`：oxn-hub / oxn-whisper / oxn-piper / oxn-kws-sidecar / oxn-mosquitto / oxn-snapserver / oxn-mpd / oxn-music-assistant / oxn-netease-api |

## 3. 端口检查（NAS 上跑）

| 端口 | 服务 | 检查命令 | 预期结果 |
|---|---|---|---|
| 10300 | whisper（STT） | `timeout 3 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/10300'` | 能连通（Wyoming 是 TCP 协议不是 HTTP，curl 会返回空，这是正常的） |
| 10200 | piper（TTS） | 同上，端口 10200 | 能连通 |
| 10400 | kws-sidecar（服务端唤醒） | `curl -s -m 5 http://127.0.0.1:10400/health` | 返回 `{"status":"ok","model_loaded":true}`（模型未加载好时 503；模型下载失败容器会报错退出，见故障速查） |
| 1883 | mosquitto（MQTT） | 同上，端口 1883 | 能连通 |
| 8095 | Music Assistant Web | `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8095` | `200`；浏览器开 `http://NAS_IP:8095` 能看到 MA 界面 |
| 3000 | NeteaseCloudMusicApi | `curl -s -m 5 http://127.0.0.1:3000/ \| head -c 200` | 返回内容（API 文档页），不是 connection refused |
| 8080 | Hub Web 面板 | `curl -s -m 5 http://127.0.0.1:8080/api/status` | 返回 JSON，含 `"wake_word":"你好小李"`，如 `{"devices":2,"online":0,...}` |
| 1704 / 1705 | snapserver（流/控制） | `ss -tln \| grep -E ':1704|:1705'` | 两个端口都在监听 |
| 4399 | Hub wss（音箱连） | `ss -tln \| grep ':4399'` | 在监听（TLS 开启时是 wss） |

## 4. 日志检查

| # | 做什么 | 预期结果 |
|---|---|---|
| 4.1 | `docker compose logs --tail=50 hub` | 出现 `open-xiaoai-hub 已启动：唤醒词 "你好小李"，监听 :4399` 和 `Web 面板：http://localhost:8080`；STT/TTS 连不上时会记 `Wyoming STT/TTS 连接失败`——刚启动时出现一次是正常的，等 whisper/piper 起好后 Hub 会重连（若一直失败，看 3 的端口） |
| 4.1a | 同上，看 KWS 一行 + `curl -s http://127.0.0.1:8080/api/status` | Hub 日志出现 `KWS 边车健康检查通过: ws://127.0.0.1:10400/v1/kws`；`/api/status` 里 `"kws_status":"ok"`（若是 `"down"`，看 10400 的 `/health` 与 `docker compose logs kws-sidecar`；刚重启边车后 `kws_status` 最多滞后一个 30s 健康检查周期才回 `ok`，唤醒本身恢复是即时的） |
| 4.1b | `docker compose logs kws-sidecar \| tail -20` | 出现 `KWS model ready` / 监听 `ws://0.0.0.0:10400/v1/kws`（模型首次下载失败会明确报错退出，`kws-data` 卷里文件不全时重起会重新下载；不要手动往卷里拷模型） |
| 4.2 | `docker compose logs --tail=30 hub \| grep -i 发现` | 应有自动发现扫描相关行（本机冒烟时无真设备，只验证到日志格式；NAS 上有音箱刷好后才会扫到设备） |
| 4.3 | `docker compose logs --tail=100 \| grep -iE 'fatal|panic'` | 无输出（有则把对应容器日志贴回来） |
| 4.4 | `docker compose logs snapserver \| tail -20`、`docker compose logs mpd \| tail -20` | 无 ERROR/FATAL；snapserver 应加载到 `snapserver.conf` 里 `NAS音乐` 这个流 |

## 5. 网易云扫码登录（MA 界面，见 README §网易云扫码登录）

| # | 做什么 | 预期结果 |
|---|---|---|
| 5.1 | 浏览器开 `http://NAS_IP:8095` → 设置 → 音乐源 → 添加音乐源 → NetEase Cloud Music | 出现 NetEase 源配置页 |
| 5.2 | API base URL 填 `http://netease-api:3000`（MA 和 netease-api 在同一个 compose 网络里，容器名能解析） | 保存成功 |
| 5.3 | 点 **QR Login**，新页面弹出二维码 | 页面显示二维码（有效期约 2 分钟，过期点重新生成） |
| 5.4 | 手机网易云音乐 App 扫码，App 内点确认登录 | MA 页面显示已登录，网易云歌单/收藏出现在 MA 曲库里 |
| 5.5 | 在 MA 里搜一首歌点播放（先选非 VIP 的） | 能出声（经 Snapcast 到音箱；音箱未刷好前先在 MA 界面确认能播） |

## 6. 首次联调

| # | 做什么 | 预期结果 |
|---|---|---|
| 6.1 | `curl http://NAS_IP:8080/api/devices` | 有音箱刷好并连上后，列表里出现设备且 `Online:true`（没刷机前只有配置里的两台占位，`Online:false` 是正常的） |
| 6.1a | 对音箱说“你好小李”（真人唤醒，VM 联调只验过合成语音） | 只有一台应答；另一台静默；Hub 日志出现 `kws: 设备 … 唤醒 "你好小李"`，`/api/status` 的 `kws_status` 为 `ok`（真人唤醒率 1m/3m ≥95% 仍是 BLOCKED，需真机测） |
| 6.2 | 语音“你好小李，播放周杰伦的青花瓷” | 3 秒内出声（FR-05 验收线）；歌词/暂停/下一首可用 |
| 6.3 | 两台音箱都刷好后放同一首歌 | 同组同步播放，延迟差 ≤10ms（关口 3） |

## 7. P1-31 / P1-32 冒烟：重启后数据与配置不丢（2026-11-17 补）

**状态：未在真 NAS 上验证（BLOCKED，无 NAS 实测）。** 本节只在 NAS 上跑，不要在本机猜结果。

| # | 做什么 | 预期结果 |
|---|---|---|
| 7.1 | `up` 后：`docker compose exec hub ls /app/hub-data`，再 `docker compose logs hub \| grep "证书指纹"` 记下指纹 | `/app/hub-data` 里有证书文件、`reminders.json`、`memory.json`（没有也先记下实际内容）；指纹是一串十六进制 |
| 7.2 | 语音认领一台房间（如“你好小李，我在客厅”），或在 Web 面板改一次配置 | 认领后 `curl http://NAS_IP:8080/api/devices` 里该设备 `Room` 已变；NAS 上 `cat $NAS_DIR/hub/config.yaml \| grep -A2 <设备ID>` 能看到新房间 |
| 7.3 | `docker compose restart hub`，等 10 秒，再查一次指纹与房间：`docker compose logs hub \| grep "证书指纹"`、`curl http://NAS_IP:8080/api/devices` | **指纹与 7.1 记下的一致**（变了说明证书丢了重生成，音箱指纹 pin 会失效）；房间认领还在；`/app/hub-data` 内容还在 |
| 7.4 | 若 7.3 任一项不对 | 停下，把 `docker compose logs hub` 与 `docker compose config` 的输出原样贴回来，不要猜着往下走 |

| 现象 | 含义 |
|---|---|
| 重启后指纹变了 / 提醒没了 | hub-data 卷挂错位置（P1-31 复发），先查 `docker compose config \| grep -A3 hub-data` |
| 认领房间后重启就丢 / Web 改配置报 500 | config.yaml 仍是只读挂载（P1-32 复发），先查 `docker compose config \| grep config.yaml` |

## 故障速查

| 现象 | 先查 |
|---|---|
| `docker compose up` 报 `build ... Dockerfile not found` 之类 | 确认 `deploy/Dockerfile.hub` 和 `deploy/hub-linux-amd64` 都传到了 NAS 的 deploy 目录（2026-11-17 已修过一次：原来 `build: ../hub` 但 hub/ 下没 Dockerfile） |
| `up` 报 `pull ... not found` | 看是哪个镜像；`binarytorch/neteasecloudmusicapi` 已证实不存在（Docker Hub 404），正确的是 `binaryify/netease_cloud_music_api`（compose 已改） |
| Hub 容器一起就退，日志 `OPEN_XIAOAI_TOKEN 未设置` | `.env` 里 `OPEN_XIAOAI_TOKEN` 为空，填好重 `up` |
| Hub 日志一直 `Wyoming STT/TTS 连接失败` | `docker compose ps` 看 whisper/piper 是否 Up；`curl` 10300/10200 端口；首次启动等够 1 分钟（下载模型） |
| Hub 日志 `唤醒服务暂不可用` 或 `/api/status` 里 `kws_status: down` | 先 `curl http://127.0.0.1:10400/health`：不通/503 → `docker compose logs kws-sidecar` 看模型下载是否失败（失败会明确报错退出；`kws-data` 卷文件不全时重起会重新下载）；通且 `model_loaded:true` 但 Hub 仍报不可用 → 看 Hub 日志里的边车地址是不是 `ws://127.0.0.1:10400/v1/kws`（写成容器名 `kws-sidecar` 时 host 网络解析不到，见 §1.2） |
| `docker compose up` 时 kws-sidecar 反复重启 | `docker compose logs kws-sidecar`：模型下载失败（网络/SHA 校验）会直接退出；NAS 出网有问题时先确认能访问 `github.com`（模型在 GitHub releases 上） |
| 10400 端口被占 | kws-sidecar 走 host 网络，10400 被占就起不来：`ss -tlnp \| grep 10400` 查占用者，先停掉再 `up` |
| MA 里搜不到网易云歌 / 报 301 需要登录 | Cookie 过期（约 1 个月），在 MA 里重新点 QR Login 扫码；或 `curl http://NAS_IP:3000/login/status` 看登录态 |
| MA 里看不到 Snapcast 播放器 | MA 默认要求 host 网络做 mDNS 发现，本 compose 里 MA 是 bridge 网络 + 8095 映射。先在 MA 设置 → 播放器提供者里手动加 Snapcast，地址填 `NAS_IP:1705`；还不行就把 MA 改成 `network_mode: host`（去掉 8095 端口映射）再试 |
| MPD 库是空的 / snapserver 没收到流 | 未核实项：vimagick/mpd 镜像默认配置是否把 `/music` 写进 fifo。先看 `docker compose logs mpd`；如需自定义 mpd.conf，挂一个写好 fifo 输出的配置再重起 |
| `mosquitto` 起了但 Hub 没连它 | 已知现状：Hub 当前代码里没有 MQTT 引用，mosquitto 只是预留（设备注册表 + HA 发现），不起作用不影响 Hub 跑 |
| Hub 日志 `agents hermes/openclaw 连接失败` | 正常：这两个 Agent 服务还没部署，Hub 记日志继续跑；配好 Hermes/OpenClaw 后再看 |

## 镜像核实表（2026-11-17，Docker Hub / 官方文档逐个查过）

| 镜像 | 结果 | 依据 |
|---|---|---|
| rhasspy/wyoming-whisper:latest | 存在 | Docker Hub 页面可打开，latest 标签，455.1 MB，2 个月前更新 |
| rhasspy/wyoming-piper:latest | 存在 | Docker Hub 页面可打开 |
| eclipse-mosquitto:latest | 存在 | Docker 官方镜像，latest = 2.1.2-alpine |
| ivdata/snapserver:latest | 存在 | Docker Hub 页面可打开 |
| vimagick/mpd:latest | 存在 | Docker Hub 页面可打开 |
| ghcr.io/music-assistant/server:latest | 存在 | MA 官方文档明确给出该镜像及 compose 示例 |
| binarytorch/neteasecloudmusicapi:latest | **不存在** | Docker Hub 返回 404；compose 已改为官方镜像 |
| binaryify/netease_cloud_music_api:latest（替代） | 存在 | Docker Hub 页面可打开，官方 `docker pull binaryify/netease_cloud_music_api` |
| kws-sidecar（本地构建，`python:3.12-slim` + sherpa-onnx） | 已修 | `deploy/docker-compose.yml` 的 kws-sidecar 服务：与 Hub 一样走 host 网络，Hub 经 `ws://127.0.0.1:10400/v1/kws` 连接；模型不 bake 进镜像，首次启动由 entrypoint 下载到 `kws-data` 卷（约 5.6MB，SHA256 见 `kws-sidecar/scripts/download-model.sh`），下载失败容器报错退出 |
| hub（本地构建） | 已修 | 原 `build: ../hub` 无 Dockerfile；现用 `deploy/Dockerfile.hub` + 预编译 `deploy/hub-linux-amd64`（11,747,288 字节，SHA256 `1e4f6f6cc814597ad2fbc1de763207a06cc4bc0ec1d50552a156f68df54bc80a`，2026-11-17 服务端 KWS 接线后重编；旧值 `f12c09e0…` 已取代） |
