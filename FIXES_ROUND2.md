# open-xiaoai-next P1 全量修复报告（FIXES_ROUND2.md）

- 修复分支：`fix-p1-round2`（从 main `04b7497` 切出，PR #1 已合）
- 审计依据：`AUDIT_ROUND2.md` §10（优先级表）、§6（96 项延期复查主表）、`.audit-round2/*.md`（分面证据）
- 修复范围：**54 项（P0 1 + P1 53）**——新发现 P1 14 项（Hub 3 + 客户端 3 + 边车 4 + 固件/部署 4）+ P0-N1 1 项 + 首轮仍开 P1 35 项 + 首轮部分修复 4 项（P1-14、P1-27、P1-28、P1-45）一并闭合。P2 不在范围（仅注明的一行级顺带除外）；P2-31 未动。
- 方法：Hub / 客户端 / 边车 / 固件+部署四面并行修，协调员汇总、独立重跑全量测试、修 lint 规则、开 PR。

## 0. 决策（本轮替 bai 做的决定，bai 可以推翻）

| 决策 | 内容 | 依据 |
|---|---|---|
| **P1-40 路线：长期服务端模式** | 唤醒检测长期放 NAS 边车（kws-sidecar），本地 KWS 不再作为生产路径；源码重编 ORT+sherpa（A 路）保留为文档化的未来可选项，本地 KWS 代码不删除 | bai 之前把 P1-40 的 A/B 选择权交给了 Friday（“哪个稳选哪个”），Friday 选了服务端唤醒且已落地验证；官方 ARM `.so` 要求 GLIBC 2.34、音箱最高 2.25，A 路需从源码重编整个 ORT+sherpa（数小时构建、OOM 风险），无迫切收益 |
| **P1-44 形态：注册制** | 客户端在已有 WS 连接上主动注册（`register` + 每 30s `heartbeat`），不在音箱上开新监听口；指纹语义 = mTLS 客户端证书 DER 的 SHA-256（小写 hex），无证书填 `""`；Hub 侧接注册、指纹白名单校验（空表一律拒绝）、删除自动扫描；默认关闭、白名单的安全模型保留 | 审计建议方向（Hub 侧注册/心跳或客户端主动上报）；注册走已有连接，不新增暴露面 |
| **MA_TOKEN 为空即不启用 MA**（行为变更） | 未配 MA Token 时不建 MA 客户端，点歌走 MPD 降级；配好 Token 后重启 Hub 启用 | P1-6：空 Token 时 MA 无认证却照常建客户端是明文密钥问题；降级路径本来就存在 |
| **HA 地址改为 `wss://`** | `hub/config.yaml` 的 `home_assistant.url` 由 `ws://192.168.1.10:8123` 改为 `wss://192.168.1.10:8123` | P1-5：明文 HA 连接；**需 bai 确认 HA 已配 TLS，否则改回 `ws://`**（见 §7） |
| **`music_folder` 缺失时启动即 Fatal** | `ValidateStartup()` 在 music 文件夹不存在或不是目录时 Hub 启动即退出（fail-closed，与 KWS 非法配置启动报错风格一致） | P1-45 收紧的一部分；若 bai 希望改为只警告继续跑，需推翻此决策 |
| **P1-4 部分修复的取舍** | MA 8095 有意保留向局域网发布（扫码登录入口）；hub/snapserver/mpd 保留 host 网络；未加 `user:`/`cap_drop`/`read_only`（hub 需写 NAS root 属主文件、发现扫描需 NET_RAW、与 P1-32 冲突）；digest 未钉死（VM 到 Docker Hub 的 manifest 接口超时，无法可信解析） | 见 §7；digest 待在能访问 Docker Hub 的机器上取 `Docker-Content-Digest` 再填 |

## 1. 逐项修复状态表

### 1.1 硬门（优先级 0）

| 项 | 状态 |
|---|---|
| **P0-N1** 客户端双开 ALSA | ✅ 修了（客户端）：按需打开——录音任务只开采集（`Audio::open_capture_only`）、会话只开播放（`Audio::open_playback_only`），未请求方向标 `not requested` 并调到即报错；回归测试 3 项。EBUSY 是否在真机发生仍需真机确认（BLOCKED，修法不依赖确认）。 |
| **FW-P1-1** Web Token 未接进部署 | ✅ 修了（部署）：`.env.example` 加 `OPEN_XIAOAI_WEB_TOKEN`（强随机占位、必填、与 `OPEN_XIAOAI_TOKEN` 独立）；compose 的 hub 服务 `environment` 传该变量；`nas-install.sh` §0 强校验（<16 位或常见值拒绝、两 Token 相等拒绝）+ §5 带 Bearer Token 调 `/api/status` 断言 200 且含 `wake_word`；`NAS_CHECKLIST.md` §0.4/§4.1a/§6.1/§7.2/§7.3 的 curl 全部补 Token；故障速查加“`/api/*` 401”行。 |

### 1.2 优先级 1

| 项 | 状态 |
|---|---|
| **P1-16** 播放状态机 latch bug | ✅ 修了（Hub+客户端）：客户端收到帧即播（裸帧不丢弃）、`playing_since` + 2s 看门狗复位、打断后 200ms 抑制在途帧、无设备时节流日志；Hub 侧每次播报先发 `start_play`、播完发 `stop_play`，按音频时长维护 playing，打断判断按真实播放状态。 |
| **P1-N1** `set_volume` 谎报成功 | ✅ 修了（客户端）：真调 ALSA 混音器（`Master`→`PCM`→`Speaker`→`Digital`），按真实结果回 `rpc_result`，成功才更新 `Session.volume`；无设备回 `ok:false`。混音器元素名需真机确认哪个存在（BLOCKED）。 |
| **P1-N2** `start_recording` 空操作 | ✅ 修了（客户端）：`Session.recording` 标志 + `pushing_audio()` 推流门，local 模式起停时补发 `audio_start`/`audio_end`，`Interrupt` 清标志。 |
| **SC-P1-1** 边车重复 device_id 孤儿流 | ✅ 修了（边车）：新连接通过鉴权后，先置旧流 `replaced=True`（旧 handler 见标志即停喂、即使算出结果也不发 wake），再 `await old_ws.close(1000, "replaced by new connection")`（5s 超时兜底）；旧流对象丢弃即清掉排队帧与 spotter 流状态；finally 的 `is stream` 守卫保留，不误删新连接。 |
| **SC-P1-2** 空 keywords.txt fail-open | ✅ 修了（边车）：加载前校验至少一行有效行（非空、非注释、含 `@` 且 token 部分非空），否则抛 `ValueError` → FATAL、`sys.exit(1)`，绝不带病上线。 |
| **P1-44** 发现扫错方向 | ✅ 修了（Hub+客户端）：注册制（见 §0 决策）；客户端 `hello_ok` 后立即发 `register`（`device_id` 取分配值，`room` 未认领填 `""`，`fingerprint` 按 §0 语义），每 30s 发 `heartbeat`；Hub 接 `register`/`heartbeat`（device_id/model/指纹一致性校验，白名单为空一律拒绝 fail closed）、`SetFingerprints` 接线；删除自动扫描（`Scanner.Scan`/`hostsInCIDR`/`arpNeighbors`/`inc`/`Enabled` 全删，main.go 发现 goroutine 与 `newDiscoveryScanner`/`localCIDR`/`discoveryPort` 全删）。 |

### 1.3 优先级 2

| 项 | 状态 |
|---|---|
| **P1-11 / P1-12** 60s 读超时误杀 + 重连误删新连接 | ✅ 修了（Hub+客户端）：Hub 主动发 Ping（PingInterval 默认 30s、PongWait 60s）、收到 Pong 续期读 deadline；重连清理按连接身份比对删连接表；客户端回标准 WS Pong（带原 payload）、无读 deadline 不主动杀连接；客户端侧重连已核实干净（每次重连新建 `Session`，旧资源靠所有权 drop）。另修一真竞态：`hello_ok` 原绕开 `Conn.mu` 直接 `WriteJSON`，现走 `c.SendJSON`。 |
| **N-P1-2** `DialMPD` 读 welcome 行无 deadline | ✅ 修了（Hub/music）：读 welcome 行加 deadline，MPD 故障时 Hub 不再永久挂起。 |
| **N-P1-1** Snapcast `call()` 不校验响应 id | ✅ 修了（Hub/music）：`call()` 循环读行直到读到 `resp.ID == seq` 的响应，通知行丢弃；行读 64KB 上限，最多跳 10 条。 |
| **P1-40** 文档纠正 + 路线决策 | ✅ 修了（客户端+协调员）：`client/kws/vendor-arm/SOURCE.md`、`client/src/sherpa.rs`、`client/kws/README.md`、`client/README.md` 里的证伪说法全部清掉（两个 `.so` 都要求 GLIBC 最高 2.34 / GLIBCXX 最高 3.4.29，`readelf -V` 实测；“ORT 只要求 GLIBC_2.4”已被证伪）；构建脚本的 GLIBC 检查模式由只认 2.29 改为覆盖 2.29 及以上的模式，警告里打印实测最高版本（两个真实 `.so` 均触发并报出 `GLIBC_2.34`）；`TEST_REPORT.md:166` 旧行由协调员统一修正（见 TEST_REPORT.md“P1 修复”节）；路线决策见 §0（长期服务端模式）。 |
| **P1-33 + FW-P1-2** 安装脚本覆盖配置 + 虚假 PASS 检查 | ✅ 修了（部署）：NAS 上已有 `config.yaml` 时不覆盖，先做带时间戳备份再跳过 scp，仅首次部署传种子配置；删掉原来备错文件的 `cp … config.yaml.bak`；健康检查全部按 HTTP 状态码判（8080 断言 200+`wake_word`，401 即 FAIL）；端口检查补 4399/1704/1705、去 1883；§3 四处 sed 改写后逐项 grep 断言，不通过即 FAIL。另：种子 `hub/config.yaml` 的 HA 地址已是 `wss://`，原只匹配 `ws://` 的 sed 会静默失效，已改成只替换主机部分、保留原有协议、不降级成明文。 |

### 1.4 新 P1 其余

| 项 | 状态 |
|---|---|
| **N-P1-3** `speakText` 多路并发 Opus 帧交错 | ✅ 修了（Hub）：`playFrames` 统一播放协议（start_play→帧→stop_play，按帧数×20ms 维护 playing，同设备 playLock 串行化；main.go 本会话收尾；测试 `TestPlayFramesSerializesPerDevice`）。 |
| **SC-P1-3** 边车 WS 无鉴权、无连接数上限 | ✅ 修了（边车+部署+Hub）：hello 增加 `token` 字段，`hmac.compare_digest` 常数时间比较，缺失/错误即 error + 1008 关闭；Token 复用 `OPEN_XIAOAI_TOKEN`（与 Hub 规则一致 ≥16 位），未配或 <16 位时边车拒绝启动；Token 值绝不记日志、不回显；连接数上限 `--max-connections` / `KWS_MAX_CONNECTIONS`，默认 16，超限即 error + 1013 关闭；鉴权在取代旧连接之前，未通过鉴权的连接杀不掉合法旧连接；compose 的 kws-sidecar 服务传 `OPEN_XIAOAI_TOKEN`，`KWS_HOST` 改为 `127.0.0.1`；Hub 的 `kws.Options` 加 `Token`，hello 带 `"token"`（`kws.go:535`，源 `OPEN_XIAOAI_TOKEN`），fakeSidecar 加 token 校验。**部署顺序：Hub 与边车必须一起升级，否则升级边车后唤醒立即全断。** |
| **SC-P1-4** `/health` 503 分支不可达 | ✅ 修了（边车）：`/health` 返回真实健康（200 带 `status/model_loaded/keywords/devices/uptime_s/total_wakes`；任一不满足即 503 + `reason`：模型未加载 / 词表为空 / 连续 5 次推理失败，fail-closed，需重启恢复，成功喂帧即清零计数）。 |
| **FW-P1-3** CI 的 Go job 与 lint.sh 的 `go vet` 环境差 | ✅ 修了（部署，注释方案——报告建议的两种修法之一）：ci.yml Go job 加注释，写明本机自编 libopus 1.5.2 + libopusfile 0.12（`~/workspace/tools/libopus`）与 CI apt 版的版本差，新增 cgo 依赖须两边同步。 |
| **FW-P1-4**（即 P1-新-3）Python job 串行掩盖 | ✅ 已随 PR #1 修（python job 已拆为 `python-firmware` + `python-sidecar`）；本轮核实，无需再改。 |

### 1.5 优先级 3（其余 P1）

| 项 | 状态 |
|---|---|
| **P1-3** 传输层无限制 | ✅ 修了（Hub/transport）：`SetReadLimit` 64KB、hello 失败封禁、`CheckOrigin`、deviceID 碰撞顶旧连接、`http.Server` 超时。 |
| **P1-4** Compose 暴露面 | ⚠️ 部分修了（部署）：whisper/piper/netease 只绑 `127.0.0.1`；mosquitto 服务+`depends_on`+卷全部删除；8 个服务加 `TZ=Asia/Shanghai` 与 `deploy.resources.limits`；`Dockerfile.hub` 加 `tzdata`。未做的（见 §0 决策）：MA 8095 保留局域网发布、hub/snapserver/mpd 保留 host 网络、未加 `user:`/`cap_drop`/`read_only`、digest 未钉死。 |
| **P1-5** HA 明文 + 读挂起 | ✅ 修了（Hub）：HA 走 `wss`、Dial 读 deadline、空 token 不发 auth；`config.yaml` 已改 `wss://192.168.1.10:8123`（**需 bai 确认 HA 已配 TLS，否则改回 `ws://`**）。 |
| **P1-6** 下游明文密钥 | ✅ 修了（Hub）：Agent/MA/网易云 TLS 或内网标注；`MA_TOKEN` 为空即不启用 MA、点歌走 MPD 降级（行为变更，见 §0）；`agent.Chat` 错误分支只拼截断摘要。 |
| **P1-7** MPD 换行注入 | ✅ 修了（Hub/music）：`PlayURL` 校验 `\r`/`\n`/控制字符。 |
| **P1-8** 解包无哈希校验（残留） | ✅ 修了（固件）：`unpack.sh`/`patch.sh` 加 `find_exactly_one`（零个或多个即失败）；`unpack.sh` 在 unblob 前清旧产物；`mico_version` 与入参版本比对，不符拒绝。 |
| **P1-9** OTA 验证零调用 | ✅ 修了（Hub）：OTA 验证与回滚进状态机（`PushOTA==nil` 回 500 不假装成功；pushing 超 10 分钟标失败；`MarkRebooted` pushing→verifying；`ConfirmHello` 校验版本；每 10s `Sweep` 定时接线已入 main.go）。 |
| **P1-10** 指纹可选 | ✅ 修了（Hub+客户端）：Hub 侧 `GET /api/fingerprint`（鉴权后返 Hub TLS 证书 SHA-256）；启动时证书提前签发、指纹注入 `webServer.HubFingerprint` 并在启动日志写明“刷机前写进 client.toml 的 hub_ca_fingerprint”；客户端 `tls=true` 时指纹必填且格式合法，否则拒绝启动，`tls=false` 须 `allow_insecure_ws=true` 且 `hub_url` 须为 `ws://`；另补 `hub_url` scheme、`model`、`ram_mb` 校验。 |
| **P1-13** 仲裁忽略时间戳 | ✅ 修了（Hub）：仲裁用时间戳（去重/去陈旧/冷却/赢家失败补位，losers 按 RMS 降序）；`OnWake` 用 `wakeTimestamp`（TsMs>0 用事件时间戳，为零才补 now）。 |
| **P1-14** RMS 单帧值 | ✅ 修了（客户端）：local 模式改 `RmsWindow`（50 帧 = 1s）均值，sherpa 命中时上报窗口均方根。已实测确认 trailing 静音稀释真实存在，根治属 SC-P2-2，未修。 |
| **P1-15** 对讲零触发 | ✅ 修了（Hub+客户端）：Hub 侧 Router 呼叫/对讲/广播/挂断分支、intercom `EndFor` 双向结束、`Sweep` 每 10s 定时接线、`OnDisconnect` 双向 End；客户端侧 `start_recording` 真实现、`start_play` 收到后能播。 |
| **P1-17** 对讲失败落管线 | ✅ 修了（Hub）：`main.go` OnAudioFrame 的 `Forward` 出错即 return，不再走语音管线。 |
| **P1-18** | 已删除 | 2026-11-23 紧急呼叫功能整体删除，本项作废。 |
| **P1-19** | 已删除 | 2026-11-23 紧急呼叫功能整体删除，本项作废。 |
| **P1-20** Wyoming 无锁 | ✅ 修了（Hub/wyoming）：`wyoming.Client` 加写锁。 |
| **P1-21** HA 并发写不安全 | ✅ 修了（Hub/ha）：`call()` 与 `SubscribeEvents` 的 `WriteJSON` 加写锁。 |
| **P1-22** Web PUT 合并语义 + 配置锁 | ✅ 修了（Hub/web/config）：PUT 递归合并、原地更新 `*s.cfg`、与房间认领共用 `cfgMu`；全部 Config 字段补 `json` 标签。已知限制：PUT 合并无法表达删除设备，删设备需直接改文件。 |
| **P1-23** `Config.Save` 临时文件 | ✅ 修了（Hub/config）：`Config.Save` 用 `os.CreateTemp` 唯一临时文件。 |
| **P1-24** 提醒未知 Kind 永触发 | ✅ 修了（Hub/reminders）：`validKind` 白名单、Add 拒绝、Load 丢弃、tick default 分支删除。 |
| **P1-25** 每日提醒越界 | ✅ 修了（Hub/router）：hour 0–23、min 0–59，越界回用户可听的话；顺带修 `dailyRe` 分钟前缺 `\s*`（“每天 8 点 70 分”从未匹配上）。 |
| **P1-27** MA 恢复后 QueueID 不热更新 | ✅ 修了（Hub）：`wyoming.NewDialer` 懒拨号 + main.go 每 30s 看门狗（MA Players/MPD Ping/Snapcast GetGroups/HA GetStates 探活，重拨后 `rt.SetHA` + `watcher.Rebind`）；`alerts.Watcher.Rebind`；`Router.SetHA`/`HA()`；Dialer 失败带“STT/TTS 不可用”前缀。 |
| **P1-28** 语音管线结算异步化 | ✅ 修了（Hub/voice）：结算异步化 + `Discard`（断线丢弃未结算 utterance，`OnDisconnect` 调 `Discard`）。 |
| **P1-29** 同设备 in-flight guard | ✅ 修了（Hub/voice）：上一句未答完新结算丢弃记日志，120s 超时。 |
| **P1-30** `patch_all` 中途失败无回滚 | ✅ 修了（固件）：`patch_firmware.py` 新增原子写 `_write_bytes`（同目录临时文件 + fsync + `os.replace` + 目录 fsync，保留原权限）；`patch_all` 先快照后执行，任一失败尽力恢复 11 个文件原样并重抛。 |
| **P1-34** netease 检查 | ✅ 修了（部署）：netease 检查改为断言 HTTP 200；Checklist 3000 行同步（注明只绑回环、NAS 外打不开是正常的）。 |
| **P1-36** 串口免密 root | ✅ 修了（固件）：串口仍走 `ttyS0::askfirst:/bin/login`，需 root 密码，不再免密进 ash；`patch.sh:139` 断言与 README 同步。权衡：root 密码丢失后串口恢复也没了，只能重刷。 |
| **P1-37** mosquitto 匿名 broker | ✅ 修了（部署）：mosquitto 删除（Hub 代码无任何 MQTT 引用，全仓库 grep 核实；需要时再加带密码配置的版本）。 |
| **P1-38** 唤醒词路由 | ✅ 修了（Hub/router）：按唤醒词选 Agent；未命中按 Agent 名排序确定性取首个；默认大脑排序取首。 |
| **P1-39** 房间认领 | ✅ 修了（Hub/router）：房间认领（“我在这里 / 我在 X”）分支 + main.go `RoomClaim: h.OnRoomClaim`；客户端 `send_room_claim` 仍为死代码（协议 API 保留），hello 已带 room、P1-44 的 register 每连接重申 room，覆盖重连场景。 |
| **P1-41** “你还记得什么”+ 定时动作提醒 | ✅ 修了（Hub/router+reminders）：`KindAction`；顺带修真 bug：`Memory.Set` 用秒级时间戳当 key，同一秒两条“记住”互相覆盖，已改纳秒 key。 |
| **P1-42** 查询只查不调 | ✅ 修了（Hub/router）：动作路径 `GetEntityState` 出错回“查不到状态，先不操作”。 |
| **P1-43** 无限重连 | ✅ 修了（客户端）：退避 100ms 起步翻倍、上限 30s、±50% jitter；鉴权失败只重试 3 次后非零退出；连接期间 Ctrl-C 可中断退避。 |
| **P1-45** 配置校验收紧 | ✅ 修了（Hub+客户端）：DND 格式、wake_word_routes 值须在 agents 里、hub.mpd 内网 host 校验、web_listen 拒 `:8080`/`0.0.0.0`/公网 IP、种子空 IP 不报错；客户端补 model/ram_mb/hub_url 校验。 |
| **P1-46** 时区 | ✅ 修了（部署）：8 个服务加 `TZ=Asia/Shanghai`；`Dockerfile.hub` 装 `tzdata`（否则 TZ 不生效）。 |

统计：54 项中 **53 项已修**（含 FW-P1-4 早随 PR #1 修），**1 项部分修**（P1-4，见 §0 决策与上表）。

## 2. P2 顺带（一行级、挡 P1 的路，已注明；其余 P2 未动）

- Hub：N-P2-9（`hello_ok` 写前 5s 写 deadline）、P2-16（`call()` 写失败删 `pending[id]`）、P2-45（“每 0 分钟”拒绝）、P2-50（`splitArtistTrack` 拆歌手/歌名）、P2-11（MA 失败记 warn 日志）、P2-43（TTS 失败日志只记文本长度）、N-P2-6（web_listen 校验）、N-P2-7（`requireAuth` 清理过期 `authFailures`）。
- 客户端：P2-18（非法音量不再静默重置为 50）、P2-41（新会话继承上次成功音量）、P2-N3（`tls=false` + `wss://` 拒绝启动）。
- 固件/部署：FW-P2-2（ci.yml `python-firmware` job 注释补本地补跑命令，`pip install pytest` 钉为 `'pytest>=8,<9'`）。
- 未修的 P2（明确记录）：SC-P2-1（边车 `is_ready` 无上限循环）、SC-P2-2（rms 均值 trailing 稀释）、客户端 P2-N1（`StopPlayback` 的 ALSA 缓冲尾音 `pcm.drop()+prepare()`）。P2-31 未动（等首轮审计员确认）。

## 3. 协调员收尾：lint 规则更新（P1 改动后规则本身过时）

P1 改动删了自动扫描、把 `wyoming.Dial` 改为 `NewDialer` 懒拨号之后，`scripts/lint.sh` 失败（规则 1 三项 + 规则 2 五处 + golangci-lint 两项），不是代码回归，是规则没跟上：

- `scripts/check-wiring.go`：删 `discovery.New` 与 `discovery Scan 调用` 两条（P1-44 已删自动扫描）；`wyoming.Dial` 改为 `wyoming.NewDialer` 接线检查；`intercom.Start 触发` 的生产正则由 `intercom\.Start\(` 改为 `Intercom\.Start\(`（Router 经 `r.deps.Intercom` 接口调用）；`deferredWiring` 的 5 个旧条目全部摘掉（P1-9/P1-15 接线已落地）。
- golangci-lint：删 `main_test.go` 未使用的 `boolPtr`；`router.go:382` 的 S1008 简化为 `return !strings.HasSuffix(text, "报警了")`。
- 规则 2 nil 守卫：`wyoming.go` 的 `Dialer.Transcribe`/`Synthesize` 在两次 `get()` 后各加 `c == nil` / `c2 == nil` 显式守卫；`main.go` 的 `speakText` 闭包开头加 `tts == nil` 守卫（`tts` 是 `NewDialer` 构造、恒非 nil，守卫只为满足规则 2 的显式要求）。

修后 `./scripts/lint.sh` 全部通过（规则 1：42 项接线检查全部通过）。

## 4. 新发现、未修（协调员复核确认属实，留待定夺）

**`patch_firmware.py:patch_login` 的第一次 `re.subn` 结果被丢弃**（固件工人发现，未擅自改）：

```python
text = _read(shadow)
text, n = re.subn(r"^root:\$[156]\$[^:]+:", f"root:{new_hash}:", text, count=1, flags=re.MULTILINE)  # 能处理 $1$/$5$/$6$
shadow = rootfs / "etc/shadow"
text = _read(shadow)          # ← 重读文件，第一次替换的结果被丢弃
text, n = re.subn(r"^root:\$1\$[^:]+:", f"root:{new_hash}:", text, count=1, flags=re.MULTILINE)  # 只处理 $1$
if n != 1:
    raise PatchError(f"root 密码行替换失败：{shadow}")
```

后果：shadow 里 root 行是 `$6$`（sha512crypt，正是新补丁镜像用的格式）时，第二次 subn 找不到 `$1$`，`n=0`，直接抛错——**已打过补丁的 rootfs 重跑 `patch_login` 必失败**。建议修法（未执行）：删掉第二次 `_read` 与重复 subn，只保留第一次（`$[156]$` 全覆盖）并校验 `n==1`，约 4 行，补一个 `$6$` root 行的回归测试。是否修、由 Friday 定夺。

**【2026-11-17 22:30 Friday 复核后已修】** 修了，但建议的窄修法不够：Friday 按建议只修 shadow 部分后，新回归测试（同一 rootfs 连打两次 `patch_login`）仍失败——`check_login` 的 PAM 前置检查只认原厂状态，已打过补丁的 rootfs 在前置检查就抛错；且 PAM 替换用子串 `replace`，重跑会在已注释的行上叠出 `# # ` 双重注释。所以实际修了三处：(1) shadow：删掉第二次 `_read` + `$1$` 专属 subn，只保留 `$[156]$` 全覆盖并校验 `n==1`；(2) `check_login`：PAM 前置接受原厂与已打补丁两种状态（libmico 行注释与否、pam_unix 行启用与否），其他状态仍拒绝；(3) `patch_login` 的 PAM 段：行首锚定替换，只注释未注释的行、只启用仍被注释的 pam_unix 行。回归测试 `test_patch_login_twice_on_sha512_shadow`：修后通过；回滚修复重跑该测试失败（双向验证）。固件 pytest 由 67 → 68 通过。

## 5. 测试总表（协调员独立重跑，非只信工人汇报）

| 命令 | 输出 |
|---|---|
| `go test -count=1 ./...`（hub/） | 22 包全部 ok，**223 passed**（基线 156，新增 67） |
| `go vet ./...` | 干净 |
| `go test -count=1 -race ./...`（hub/） | 22 包全部 ok，**无 FAIL**（Hub 工人首遍全量 -race 时 kws 包 flake 一次，未定位到具体用例；协调员重跑未复现，记为未复现、需观察） |
| `cargo test`（client/） | **81 passed**（基线 53，+28） |
| `cargo test --features sherpa-kws`（client/） | **85 passed**（基线 57，+28） |
| `cargo fmt --check` / `cargo clippy --all-targets`（两个 feature 组合） | fmt OK；clippy 0 warnings |
| `python3 -m pytest firmware-patch/tests/ -q` | **67 passed**（基线 61，+6：P1-30 回滚/原子写 2 项、P1-8 4 项） |
| `kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q` | **25 passed**（基线 16，+9：重复 device_id 1、空词表 2、坏 Token 2、未配 Token 2、连接上限 1、健康 503 1） |
| `./scripts/lint.sh` | **全部通过**（修前 FAIL：规则 1 三项 + 规则 2 五处 + golangci 两项，见 §3） |
| e2e（客户端，真实 `run_session` + fake Hub，合成正弦波 Opus） | `set_volume` 真结果、裸音频帧无需 `start_play` 即播出、local 模式 `start_recording` 推流门：3 项 × 各 3 次，**9/9 通过** |

## 6. BLOCKED 清单（真机 / NAS 才能验证）

1. P0-N1 的 EBUSY 是否在真机发生（需真机确认 `hw:0,2` 子设备数与二次打开行为；修法不依赖确认）。
2. P1-N1 混音器元素名（`Master`/`PCM`/`Speaker`/`Digital` 候选，真机上哪个存在需实测；本 VM 无 `/dev/snd`）。
3. 刷一台 LX06（首关口）、Hub 上 NAS、两台同步播一首歌 ≤10ms、真人唤醒率 1m/3m ≥95%、真机双机仲裁、真机打断率、网易云 QR 登录、OH2P 刷机端到端、真实 HA 集成、真机 ALSA 采集/播放（与审计 §9 的 B1–B18 对应）。
4. NAS 实测：nas-install.sh 全流程、`docker compose config`（VM 无 docker）、8 容器 Up、8080 带 Token 检查、netease `/` 返回 200、mosquitto 删除后无残留引用；“已有 config 不覆盖”分支只在假 ssh 下走过首次部署分支。
5. P1-36 串口密码登录（刷机后串口输 root 密码能否进）；P1-30 回滚只在 VM 的 rootfs 副本上验证过。
6. digest 钉死（VM 到 Docker Hub 的 manifest 接口超时，无法可信解析；待在能访问 Docker Hub 的机器上取各镜像 latest 的 `Docker-Content-Digest` 再填）。
7. P1-10 收紧的部署前提：刷机前必须把 Hub 指纹写进 `client.toml`，否则客户端启动即报错退出；指纹下发步骤（`GET /api/fingerprint`）需进刷机 runbook。

## 7. 需要 bai 确认的决策点（不是 BLOCKED，但修完需要他拍板）

1. **HA 是否已配 TLS**：`hub/config.yaml` 的 HA 地址已改为 `wss://192.168.1.10:8123`；若 bai 的 HA 未配 TLS，须改回 `ws://`。
2. **`music_folder` 缺失时启动即 Fatal**：若希望改为只警告继续跑，需推翻此决策。
3. **P1-40 路线**（见 §0）：长期服务端模式；若 bai 要改回本地 KWS（源码重编 ORT+sherpa），需另立项。
4. **§4 的 `patch_login` latent bug**：修不修（建议修法已写明，约 4 行 + 1 个回归测试）。
5. **PR 合并**：本分支 PR（见下）待 bai 定夺；按惯例合并不由协调员自行决定。
