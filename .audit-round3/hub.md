# 第三轮审计：Hub 面分工报告

分支已确认：`audit-round3`，HEAD `3a9d1d9`（PR #4 刚合入）。只审计，未改任何文件，未提交。

## 计数

- **P0：0 项**——融合 B 的新增代码没有发现部署前必须修的安全/正确性硬伤。
- **P1：1 项**
- **P2：12 项**

另有 gosec 6 项生产发现逐项裁决（3 假阳性、1 真阳性低风险、1 假阳性、1 假阳性，见 §4），以及 10 项声称已修的 P2 headline 抽查全部核实通过（见 §5）。

## P1

### H-1 点歌路径无总超时预算，上游抖动时最坏静默 ~90s，FR-05「3s 内出声」失效

**结论：** 正常路径很快，但网易云一抖，单次语音点歌可静默阻塞近 90 秒且中途无任何反馈。

**证据：** `hub/internal/router/router.go:803` `Search(ctx, query, 3)`；`:819–824` 对 3 首歌逐首串行 `SongURL` → `PlayURL`；`hub/internal/music/music.go:225` `NewNetease` 客户端超时 15s；`music.go:281` `SongURL` 无重试；`hub/internal/voice/voice.go:83` 整条语音管线总超时 `answerTimeout = 120s`。最坏算术：搜歌 15s + 3×（取地址 15s + MPD 操作数秒）≈ 60–90s，之后才走本地兜底或回「放不了」。其间用户听不到任何进度，设备也无法响应其他指令。

**修复建议：** 给点歌单独设总预算（如 search+SongURL 合计 ≤8s，超时即转本地兜底）；或 3 首歌的 `SongURL` 并行取、最先成功者播放；超时/失败时先播报「网易云有点慢，先试本地」再继续。

## P2

### H-2 MPD `query()` 响应行无任何上限——P2-7 只堵了 HTTP 侧，留了 TCP 侧缺口

**结论：** 上游 HTTP 响应已全部 1MB 限长，但 MPD 行协议响应是无界读行，异常/卡住的 MPD 可把 Hub 内存打满。

**证据：** `hub/internal/music/music.go:379` `query()` 内 `for { line, err := c.r.ReadString('\n') ... }`，无行数上限、无单行长度上限、无总字节上限；`Status`/`Queue`/`CurrentSong` 全部经此读。对比同文件 Snapcast 侧 `readLimitedLine` 有 64KB 单行上限（`music.go` `maxSnapLineBytes`），KWS 侧有 `maxInboundBytes`。

**修复建议：** 给 `query()` 加与 Snapcast 同款上限（单行 64KB、总行数或总字节上限），超限即断开重连（看门狗已会重拨）。

### H-3 `SongURL` 返回的 URL 未校验 scheme/host，MPD 侧存在 SSRF 面

**结论：** 上游（netease-api）返回的任意 URL 被原样交给 MPD `add` 播放，未校验是否为可信音频源。

**证据：** `hub/internal/music/music.go:281–305` `SongURL` 只判空即返回；`music.go:407` `PlayURL` 的 `validateMPDURL` 只拦 `\r/\n`/控制字符（防换行注入，P1-7 已做），不拦 `file://`、内网 `http://` 等 scheme。`router.go:819` 直接 `mpd.PlayURL(u)`。netease-api 是 LAN 内容器，一旦其返回被操纵（如上游被投毒或 DNS 劫持），MPD 会代为请求内网地址。

**修复建议：** `PlayURL`/`SongURL` 处加白名单：只许 `http(s)://` 且 host 在网易云 CDN 域名表内（或至少拒绝 `file://`、内网 IP、localhost）；非法 URL 按「无可播地址」走本地兜底。

### H-4 `Status` 把「上游故障」与「登录过期」混成 502，过期时未必返回明确的需重扫

**结论：** 文档承诺「过期后 `/api/netease/status` 明确返回需重扫」，但上游对过期 cookie 若回非 200，端点会 502 而不是 `needs_rescan:true`。

**证据：** `hub/internal/music/netease_auth.go:239` `Status()`：`getJSON` 出错即 `return false, "", "", err`；`hub/internal/web/web.go:583` `neteaseStatus` 见 err 即回 502「查登录状态失败」。只有上游回 200 且 `account.id==0` 时才走 `logged_in:false` 分支。Binaryify 的 `/login/status` 对过期 cookie 的行为未在仓库内证实。

**修复建议：** `Status()` 里把上游 401/403 也映射为「未登录」（不返回 err），仅网络错误/5xx 才 502；并在 NAS 上用过期 cookie 实测一次确认。

### H-5 `register_reject` 仍回显具体拒绝原因——P2-2 只统一了 hello，注册留了口子

**结论：** 与 P2-2 的统一原则不一致，注册失败会告诉对端是哪一项校验没过。

**证据：** `hub/internal/transport/transport.go:678–680` `reject` 把 `reason` 原样发回：`device_id 与连接不一致` / `model 与 hello 不一致` / `未配置指纹白名单` / `fingerprint 不在白名单内`。对比同文件 `:483–487` hello 拒绝已统一回 `"rejected"`，具体原因只记日志。

**修复建议：** 注册拒绝与 hello 看齐，统一回 `"rejected"`，四种原因只记服务端日志。

### H-6 `ha.Client.Close()` 并发双关可 panic——关停路径与看门狗存在竞态窗口

**结论：** 关停与看门狗可能同时 `Close()` 同一个 HA 客户端，`close(c.done)` 双关即 panic。

**证据：** `hub/internal/ha/ha.go:127–135` `Close()` 用 `select { case <-c.done: default: close(c.done) }`——两个 goroutine 可同时走 `default` 分支双关 `done`。`hub/cmd/hub/main.go:724` 看门狗探活失败时 `_ = haClient.Close()`；`:1055` 关停先 `close(done)` 但**不等看门狗 goroutine 退出**，`:1065` 又 `_ = haClient.Close()`。窗口虽窄（需 HA 恰在关停瞬间探活失败），但 `close of closed channel` 是真 panic。

**修复建议：** `Close()` 内加 `sync.Once` 或互斥锁保护 `close(c.done)`；或关停时先等看门狗退出（`sync.WaitGroup`）再关客户端。

### H-7 Web `/api/status` 读配置未走共享 `CfgLock`，与 PUT 的 `*s.cfg` 写存在数据竞争

**结论：** GET `/api/status` 与 PUT `/api/config` 并发时是对同一 `Config` 结构体的无锁读写，`-race` 可检出。

**证据：** `hub/internal/web/web.go:353` `status()` 在 `s.mu` 下读 `cfg.WakeWord`/`cfg.RoomNames()`；同文件 `configAPI` PUT 在 `cfgLock()`（共享锁）下执行 `*s.cfg = cfg`；`hub/cmd/hub/main.go:980` `webServer.CfgLock = cfgMu` 已接线。`status` 未用 `cfgLock()`，与 P1-22 的共享锁设计脱节。

**修复建议：** `status()`（及 `fingerprintAPI` 对 `HubFingerprint` 的读已在 `s.mu` 下、无写方，可不动）改走 `s.cfgLock()` 读配置；或把 `status` 的配置快照也纳入共享锁。

### H-8 transport TLS 未设 `MinVersion`——G402 真阳性（低风险）

**结论：** wss 服务端接受 TLS 1.0/1.1（Go 默认），是 gosec G402 的真阳性；但在 mTLS + 局域网前提下实际风险低。

**证据：** `hub/internal/transport/transport.go:438` `tlsConf := &tls.Config{Certificates: ...}`，只设 `Certificates`/`ClientAuth`/`ClientCAs`，无 `MinVersion`。Go 1.23 默认服务端最低仍为 TLS 1.0。

**修复建议：** 加 `MinVersion: tls.VersionTLS12`（客户端是自家 Rust 代码，支持 TLS 1.2+，无兼容问题）。

### H-9 并发 `CheckQR` 成功时，内存 cookie 与落盘文件可能不一致

**结论：** 两个不同二维码同时扫码成功并行保存时，文件写与内存写分两步、无统一锁，交错后文件与内存可能各执一个 cookie。

**证据：** `hub/internal/music/netease_auth.go:91` `save()`：先写临时文件+改名（无锁），再 `a.mu.Lock()` 更新内存 `cookie/savedAt`；`:212` `CheckQR` 的 803 分支调 `save()`，`CreateQR`/`CheckQR` 本身不持锁。交错顺序「A 写盘、B 写盘、B 写内存、A 写内存」→ 盘上是 B、内存是 A，重启前 VIP 取地址用的 cookie 与盘上不一致。

**修复建议：** `save()` 全程持 `a.mu`（文件写也在锁内），或把「写盘+更新内存」做成一个原子操作；并发扫码本就是边缘场景，锁内写盘的代价可忽略。

### H-10 gosec 其余 8 项发现无任何记录可查——审计链断裂

**结论：** 「14 项发现」只在 `FIXES_P2.md:301` 列出 6 项生产代码项，剩余 8 项在仓库内无 gosec 报告、无清单、无裁决，无法验证「都在测试代码」的说法。

**证据：** 全仓 `grep gosec` 仅命中散文描述（`.golangci.yml` 未启用 gosec，见下）；无 gosec 输出文件；本机 `gosec` 未安装，按任务要求未重跑、直接审代码。

**修复建议：** 补跑一次 `gosec ./hub/...`（或在 CI 加只报告不卡门的 gosec job），把 14 项完整清单与裁决落盘；在此之前「8 项在测试代码」只能算未证实的主张。

### H-11 `/api/music/lyric?id=` 负数未校验，直通上游

**结论：** `id=-5` 会拼进 `/lyric?id=-5` 发给上游，靠上游报错兜底。

**证据：** `hub/internal/web/web.go:779` `musicLyric` 只 `strconv.ParseInt`，未判 `id < 0`；`hub/internal/router/router.go:971` `MusicLyric` 只把 `id==0` 解释为当前曲，负数直接进 `netease.Lyric`。

**修复建议：** `id < 0` 时回 400「id 非法」。

### H-12 `/api/netease/login/qr` 无创建节流，可无限生成 unikey

**结论：** 持 Web Token 者可循环 POST 建二维码，上游 unikey 无 Hub 侧限流。

**证据：** `hub/internal/web/web.go:607` `neteaseLoginQR` 无任何频次限制；`requireAuth` 的限流只针对鉴权失败（`web.go` `maxAuthFailures=5`），鉴权成功后不限流。

**修复建议：** 对该端点加简单节流（如每 IP 每分钟 ≤10 次新建）或复用一次未过期的 key；低优先级。

### H-13 P2-29：`ProbeModel` 仍是死代码，`Version: "probe"` 无生产调用方——结论：删

**结论：** 删。P1-44 已删自动扫描后，生产代码无任何调用方，只剩测试在调；留着它，`Version: "probe"` 会在将来某次误接线时覆盖真实版本号进注册表。

**证据：** 全仓 `grep ProbeModel(`：仅定义 `hub/internal/discovery/discovery.go:153` 与 `discovery_test.go` 的 6 处测试调用；`discovery.go:191` 写死 `"version": "probe"`。`FIXES_P2.md:321` §7 第 9 项亦确认「无生产调用方」。

**修复建议：** 删 `ProbeModel` 及其测试；如将来恢复自动发现，按真实 hello 的 `Version` 字段重写。

## §4 gosec 14 项逐项裁决（6 项生产代码）

| # | 发现 | 裁决 | 证据 |
|---|---|---|---|
| 1 | G115 `wyoming.go:325` `int16(Uint16)` | **假阳性** | PCM 字节流转采样值，位重解释是有意的，值域完整保留 |
| 2 | G115 `wyoming.go:334` `uint16(int16)` | **假阳性** | 同上，逆向重解释，编码/解码对称 |
| 3 | G115 `kws.go:418` `uint16(int16)` | **假阳性** | Opus 解码出的 int16 采样转小端字节，位操作正确 |
| 4 | G402 `transport.go` TLS MinVersion 未设 | **真阳性（低风险）** | 见 H-8；LAN + mTLS，修法一行 |
| 5 | G402 `kws.go:273` `InsecureSkipVerify: true` | **假阳性** | 指纹 pin 本身就是校验：`VerifyPeerCertificate` 验 SHA-256 指纹 + 证书有效期（`kws.go:261–293`），wss 无指纹时回落系统根证书 |
| 6 | G404 `kws.go:627` `math/rand` jitter | **假阳性** | 仅用于重连退避 ±25% 抖动（`nextBackoff`），非安全用途；`go.mod: go 1.23`，顶层 `math/rand` 已自动播种 |

其余 8 项：无记录，见 H-10。另：`.golangci.yml` 启用的 linter 仍是 `govet/staticcheck/gofmt/goimports/ineffassign/unused/errcheck`，gosec 未启用——与 `FIXES_P2.md` §7 第 8 项一致。

## §5 P2 轮 Hub 修复抽查——10 项全部核实通过

| 项 | 结论 | 证据 |
|---|---|---|
| P2-1 证书复用有效期校验 | ✅ 在代码里且有效 | `tlscert.go:47` `renewBefore=30d`；`:111–118` `certNeedsRenew` 覆盖已过期/30 天内过期/未生效；`:158` 备份后重签；证书/私钥/CA 均为 0600（`:192–195`、`:284–288`、`:383–387`） |
| P2-2 hello 拒绝原因统一 | ✅（仅 hello） | `transport.go:483–487` 统一回 `"rejected"`，原因只记日志；但注册拒绝未统一，见 H-5 |
| P2-4 权限收紧 0600/0700 | ✅ | `main.go:463–471` 数据目录 0700、4 个文件启动时收紧 0600；`config.go:560` `Save` 临时文件 0600；`reminders.go:153` 落盘 0600；`netease_auth.go:91` cookie 目录 0700、文件 0600 |
| P2-7 上游 1MB 限长 | ✅（HTTP 侧） | `agent.go:108,113` 错误体与成功体均 `LimitReader` 1MB；`netease_auth.go` `getJSON` 1MB；Web `MaxBytesReader` 1MB；但 MPD 侧无上限，见 H-2 |
| P2-13 Start 守卫 | ✅ | `reminders.go:259–290` 重复 `Start` no-op、`Stop` 可重复调、可再 `Start`；`main.go:1056` 关停补 `remStore.Stop()` |
| P2-44 tick 落盘 | ✅ | `reminders.go:332–339` 只在 `changed \|\| dirty` 时写盘，失败置 dirty 下次重试；`:104` 损坏文件改名备份 `.corrupt.<时间戳>` |
| P2-46 ChatStream 4MB 分段读 | ✅ | `agent.go:57–60` `maxSSELineBytes = 4<<20`；`:190–207` `readSSELine` 用 `ReadLine` 分段拼接、超限报错（当前行片段丢弃，调用方保留此前已拼接部分，与声称一致） |
| P2-49 场景实体校验 | ✅ | `router.go:673–676` `allowedVoiceDomain` 白名单（light/switch/climate/cover）；`:714–728` 执行前调 `ha.go:259` `GetSceneEntityIDs` 逐个校验，查不到场景内容时 fail-closed 拒绝；家电分支 `:1106–1108` 共用白名单 |
| N-P2-3 重连排空移到 hello 之前 | ✅ | `kws.go:660–680` `everConnected` 时先 `drainQueue()` 计丢帧记日志，再发 hello；注释写明顺序原因 |
| N-P2-5 HealthInterval 10s | ✅ | `kws.go:80–82,122–123` 默认 10s；`:298–312` `Start` 立即查一次；连接成功/断开即触发一次健康检查 |

## §6 融合 B（PR #3）核查结论

- **11 条路由鉴权：全部通过。** `web.go:Handler()` 中 4 条 `/api/netease/*` 与 7 条 `/api/music/*` 全部包在 `requireAuth` 内；未配置 `OPEN_XIAOAI_WEB_TOKEN` 时一律 401（fail closed）；Bearer 比较为 SHA-256 后恒定时间比较。无漏鉴权路由。
- **参数校验与错误泄露：基本通过。** `lyric` 的 id 只缺负数校验（H-11）；`qr/check` 的 key 判空但不限长；错误信息均只含上游状态码/摘要，无内部细节外泄。`PUT /api/config` 仍会显式拒绝已废弃的 `netease.music_assistant`（`web.go` configAPI），不静默丢弃。
- **netease_auth.go：通过，附 H-9。** cookie 落盘 0600（临时文件+改名，目录 0700）；`Logout` 清内存并删文件（`netease_auth.go`）；过期处理走 `/login/status` + `account.id==0` 判定，`expires_at` 从 `Expires=`/`Max-Age=` 解析；`CreateQR`/`CheckQR` 无共享 key 状态、无竞态，但并发保存有 H-9 的小窗口；`main.go:505–513` 启动加载 cookie 失败只记日志、不阻塞启动，`web.go:993–995` 的 `Netease`/`NeteaseAuth` 接线完整，扫码成功后 `SetCookie` 即时生效（`:627` 后）。
- **Hub 直连路径：通过，附 H-1/H-3。** search→SongURL→MPD.PlayURL→Snapcast 链路完整，降级顺序明确（直连→本地曲库→明说放不了，`router.go:746–747`）；上游响应 1MB 限长、15s 超时；VIP 失败时逐首试下 3 首、再本地兜底、最后明确回复，不静默失败；`validateDownstreamURL`（`config.go:387–413`）对 `netease.api` 做 scheme 白名单 + 内网 host 校验，Web 改配置无法把 cookie 外发到公网域名。
- **music_folder warn-only：通过。** `config.go:353` `ValidateStartup` 仅在空值时报错；`:362` `MusicFolderWarning` 不存在/非目录只警告；`main.go:451` 只打日志；`router.go:886` `localMusicAvailable()` 每次 `os.Stat` 判断，无任何假设目录存在而 panic/静默失败的路径。
- **MA 残留：无活引用。** 全仓 `git grep` 命中只剩四类：历史审计文档（不改写历史）、`.semgrep/rules.yml:121`（有意保留的密钥检测正则）、`deploy/.env.example:11–13` 与 `deploy/nas-install.sh:182–199` 的迁移说明/删键逻辑、`MUSIC_FUSION.md` 等迁移文档。Go 代码、`hub/config.yaml`、compose 的 `environment` 里零命中；`8095`/`music-assistant` 活引用为零。

## §7 待定项（Hub 面，只记录现状，不替 bai 决定）

- **P2-10 / P2-21 本地 KWS 残留：** Hub 侧无本地 KWS 代码（`hub/internal/kws` 全部是 server 侧边车客户端）。残留全部在客户端（Rust）：`client/src/main.rs:422–466` `init_kws` 仍按 `kws_mode` 分流（默认 `server`，`local` 分支保留）；`client/src/kws.rs`、`sherpa.rs`（feature `sherpa-kws`）原样保留；P2-10 的 `.so` 任意路径 dlopen、无白名单/SHA 校验，以及 32-bit 结构体尺寸（68/104/28）未在 ARM 上实测确认；P2-21 的 sherpa `is_ready` 无上限循环仍开（见 `.audit-round2/client.md:201`）。**待 P1-40 路由确认后定。**
- **alerts.Watcher 无 Stop：核实无泄漏。** `main.go:1025` `Start()` 只调一次；看门狗重连后 `Rebind` 到新客户端（`:717`），旧订阅随旧连接关闭而失效；关停时 `main.go:1065` `haClient.Close()` 断开连接，`ha.go:133` `readLoop` 随 `ReadMessage` 出错退出，goroutine 不泄漏。无 `Stop` 在当前接线下可接受；唯一附带问题是 H-6 的双关竞态。
- **read_only 暂缓：** 现状不变，容器文件系统仍可写（P1-4 决定），审计不重定。

## Headline 结论

Hub 面整体状态良好：融合 B 的 2,699 行新代码接线完整——11 条路由全鉴权、cookie 0600 落盘与登出清盘正确、直连+本地兜底+明说的三级降级闭环、MA 零活残留、music_folder 降级为警告无后遗症；P2 轮 10 项 headline 修复抽查全部属实。

唯一 P1 是点歌路径缺少总超时预算（H-1）：上游一抖，单次点歌可静默拖 ~90 秒，FR-05 的 3s 目标在劣化场景下完全失效，建议修掉再部署。其余 12 项 P2 以 H-2（MPD 响应无界读）、H-3（SongURL 未验 scheme）、H-6（HA Close 双关 panic 竞态）为先；H-8（TLS MinVersion）是一行修复的真阳性；gosec 6 项生产发现中 5 项是假阳性（有代码证据），另 8 项因无记录暂无法裁决（H-10）。
