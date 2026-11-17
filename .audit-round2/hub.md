# open-xiaoai-next 第二轮审计：Hub（Go）面

- 审计日期：2026-11-17（晚）
- 审计对象：`~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/`，分支 `audit-round2-ci-fix`（从 main `509e57a` 切出）
- 审计范围：Hub 全部 Go 代码（`hub/cmd/hub/`、`hub/internal/*`、`hub/integration_test.go`）+ 与 Hub 接线直接相关的 `hub/config.yaml`、`deploy/docker-compose.yml`、`deploy/Dockerfile.hub`、`deploy/nas-install.sh`、`deploy/.env.example`。客户端（`client/` Rust）、固件补丁脚本（`firmware-patch/scripts/`）不在本轮 Hub 面范围，只在延期复查表中标注“非 Hub 面，未复查”。
- 对照：首轮 `AUDIT_REPORT.md`（2026-11-17 13:19，108 项 P0 6 / P1 46 / P2 56；只修了 A+B 12 项）与 `PLAN.md` v4。
- 角色：只审计、不修代码；未修改任何源代码，未推 GitHub，未动 main 分支，未动 firmware/，未把任何密码写进文件或报告。

## 方法

逐包读代码（`internal/discovery/`、`internal/kws/`、`internal/music/`、`internal/router/`、`internal/web/`、`internal/wyoming/`、`internal/transport/`、`internal/voice/`、`internal/ota/`、`internal/intercom/`、`internal/ha/`、`internal/agent/`、`internal/config/`、`internal/registry/`、`internal/arbitration/`、`internal/reminders/`、`internal/alerts/`、`internal/aec/`、`internal/tlscert/`、`internal/opus/`、`cmd/hub/main.go`），对每项首轮延期项 grep 生产调用点验证（调用方、调用次数、调用条件），文档与代码冲突时以代码为准。另跑了基线测试、`go vet` 与 `-race` 测试（见下）。

## 基线测试结果

命令（`hub/` 内，`source ../env.sh`，Go 在 `~/workspace/tools/go/bin`）：

```
go test -count=1 ./...        # 22 个包全部 ok，exit 0
go test -count=1 -v ./...     # --- PASS 156 个，--- FAIL 0 个，--- SKIP 0 个
go vet ./...                  # 干净，无输出，exit 0
go test -race -count=1 ./...  # FAIL：internal/ha、internal/kws 两个包失败；
                              # 其余 20 个包（含 cmd/hub、transport、voice、web）ok
```

`-race` 失败详情（见新问题 N-P2-1）：11 处 `WARNING: DATA RACE`，**全部栈顶在测试替身代码**（`ha_test.go` 的 fakeHA、`kws_test.go` 的 fakeSidecar 在两个 goroutine 里对同一条 gorilla websocket 连接并发 `WriteJSON`），没有一处栈涉及生产代码文件。生产代码在 `-race` 下未检出竞态；但这两处失败使 `-race` 整体为红，且恰好是首轮 P1-21（HA 客户端无写锁）同一 bug 类在测试替身里的复现。

基线结论：普通测试全绿（156 通过 / 0 失败），与首轮“单元测试全绿、生产接线断裂”的判断一致——本轮发现的所有仍开项同样是接线/生产态问题，包内单测测不出来。

## P0 级新问题

**无。本轮在 Hub 面未发现新的 P0 级问题**（变砖、丢数据、远程执行、认证绕过类）。首轮 3 项 Hub 面 P0（P0-1 Web 鉴权、P0-3 mTLS+弱 Token、P0-4 发现广播 Token）均已修（见延期复查表），未发现绕过修复的新路径。

---

## 新问题

### P1（新，3 项）

#### N-P1-1 Snapcast `call()` 不校验响应 id，异步通知会被当成正常响应
- **位置**：`hub/internal/music/music.go:76-108`（`SnapClient.call`）。
- **证据**：`call()` 发请求后只读一行（`s.r.ReadBytes('\n')`），`json.Unmarshal` 进带 `ID` 字段的 `resp` 结构体后**从不比对 `resp.ID == s.seq`**，无 `Error` 即返回 `Result`。Snapcast 服务端会在客户端连接/断开、音量变化时主动推送无 `id` 的 JSON-RPC 通知；通知进 `call()` 会被当成“ID 0、无错误、空结果”的正常响应返回（`GetGroups()` 随即得到空分组且 `err == nil`），而真正的响应留在缓冲里被下一次 `call()` 读走，造成两次调用结果错配。另 `ReadBytes('\n')` 无长度上限，异常服务端可发无界长行打满内存（与 P2-7 同类）。
- **影响**：点歌后 `applySnapcast()`（`router.go:157-196`）可能静默跳过延迟校准/切流（分组取空且无错），或把上一次调用的响应错当成这一次的；多房间同步延迟校准（目标 ≤10ms）在通知交错时静默失效，且无任何日志。
- **修复建议**：`call()` 循环读行直到读到 `resp.ID == s.seq` 的响应（通知行直接丢弃并可记 debug 日志）；行读改限长（如 `bufio.Reader.ReadSlice` + 上限或 `ReadString` 前判缓冲）；响应 `ID` 不匹配即报错不断开连接。

#### N-P1-2 `DialMPD` 读欢迎行无 deadline，MPD 只建连不发问候时 Hub 启动永久挂起
- **位置**：`hub/internal/music/music.go:433-448`（`DialMPD`）；调用点 `hub/cmd/hub/main.go:401-409`（启动时同步调）、`:437-453`（后台重试 goroutine）。
- **证据**：`DialMPD` 的 `timeout` 参数只传给 `net.DialTimeout`（TCP 建连），随后 `c.r.ReadString('\n')` 读 `"OK MPD ..."` 欢迎行**之前未设任何 `SetReadDeadline`**。MPD 进程存活但卡住/未发问候（或端口被别的只接受不说话的服务占用）时，`ReadString` 永久阻塞。`main.go:401` 是启动流程里的同步调用——Hub 停在启动中途，`:4399`、`Web`、`KWS` 全未起，且无任何日志；后台重试 goroutine 同样会被一次挂起的 `DialMPD` 永久卡住，不再重试。
- **影响**：一次 MPD 异常即 Hub 无法启动（无超时、无日志、无降级）；与首轮 P1-5（HA `Dial` 首次读无超时）同一 bug 类，但 MPD 这一处首轮未点名。
- **修复建议**：读欢迎行前 `SetReadDeadline(now+timeout)`，超时即 `conn.Close()` 并返回错误；`main.go` 启动时对 `DialMPD`/`DialSnap` 包一层整体超时或放 goroutine 里做，避免单个下游拖死启动。

#### N-P1-3 `speakText` 多路并发时同设备 Opus 帧交错，且无播放态同步
- **位置**：`hub/cmd/hub/main.go:477-515`（`h.speakText` 闭包）；并发调用方：提醒 `onFire`（`:465-476`，`remStore` ticker goroutine）、HA 告警 `watcher` 回调（`:681-690`，HA 读循环 goroutine）、语音降级回复（`OnAudioFrame`/`OnAudioEnd`，transport 读循环）。
- **证据**：`speakText` 每次新建 `opus.Encoder`、循环 `c.SendBinary(f)`，全程**无任何 per-device 互斥**。`Conn.SendBinary` 只保证单帧写不交错（`transport.go:113-122` 持 `Conn.mu`），但两路 `speakText`（如提醒到点恰撞上漏水告警，或两条提醒同秒触发）会逐帧交替发往同一设备，客户端按到达顺序播放即为两段 TTS 绞在一起的杂音。另 `speakText` 从不发 `start_play`/置 `playing`（属 P1-16），且中途 `enc.Encode` 出错即静默 `return`（前半段已发出的帧照常播放，尾音丢失无日志）。
- **影响**：提醒/告警并发时用户听到的是混杂播报；结合 P1-16，`speakText` 的帧在客户端 `playing==false` 时还会被整段静默丢弃——即提醒可能既无声、又在极少数时序下变成杂音。
- **修复建议**：给每设备一把播报锁（`speakText` 按 deviceID 串行化，或经单 worker 队列投递）；统一走与 `onReply` 相同的播放协议（先 `start_play`、播完 `stop_play`、按音频时长维护 `playing`，见 P1-16 修复建议）；`Encode` 中途出错记 error 日志。

### P2（新，12 项）

#### N-P2-1 `-race` 下 2 个测试失败：测试替身并发写同一条 WS 连接
- **位置**：`hub/internal/ha/ha_test.go:49,66,94,107,180`（`fakeHA.pushEvent` 与 `handle` 的响应写并发）；`hub/internal/kws/kws_test.go:49,83,285`（`fakeSidecar.handleWS` 与测试 goroutine 的 `WriteJSON` 并发）。
- **证据**：`go test -race -count=1 ./...`：`FAIL internal/ha`（`TestSubscribeEventsReceivesPush`）、`FAIL internal/kws`（`TestErrorMessageDoesNotBreakStream`），共 11 处 race 警告，栈全部指向测试文件里的 `conn.WriteJSON` 并发调用；gorilla websocket 文档要求同一连接最多一个并发写者。生产代码栈未出现，`cmd/hub`、`transport`、`voice` 等 20 个包在 `-race` 下通过。
- **影响**：测试本身不可靠（`-race` 常红会掩盖真正的生产竞态）；且它正是首轮 P1-21（HA 客户端无写锁）同一 bug 类的活演示——生产 `ha.Client.call()` 同样无写锁（见延期复查 P1-21）。
- **修复建议**：给两个 fake server 的连接加写互斥（或把事件推送串行化到 handler goroutine）；CI 加 `go test -race ./...` 门禁。

#### N-P2-2 `kws.Client.Close()` 与 `SendPCM`/`streamFor` 竞态：Close 后仍可泄漏流 goroutine
- **位置**：`hub/internal/kws/kws.go:380-393`（`Close`）、`:353-371`（`streamFor`）、`:324-351`（`SendPCM`）。
- **证据**：`Close()` 在 `closeOnce` 内：关 `done`、置 `closed=true`、收集现有流、`wg.Wait()`。`SendPCM` 先 `isClosed()` 检查（持 `c.mu` 后释放），再调 `streamFor`（重新持 `c.mu`、新建流、`c.wg.Add(1)`、起 goroutine）。两步之间 `Close()` 可插进来完成 `wg.Wait()` 并返回，随后 `streamFor` 仍 `wg.Add(1)` 并启动一个永不被等的流 goroutine（继续按退避拨号）。`isClosed` 检查与 `streamFor` 的建流不是原子操作。
- **影响**：Hub 退出路径上（`main.go:708-710` 调 `Close()` 后进程即退出，实际影响小）之外，任何复用 `Close()` 后又 `SendPCM` 的调用都会泄漏拨号 goroutine；测试里 `Close` 后误调即复现。
- **修复建议**：`SendPCM`/`streamFor` 在同一把 `c.mu` 内同时做 `closed` 检查与建流（`streamFor` 内判 `closed` 返回 nil 哨兵），或 `Close()` 后 `SendPCM` 直接丢弃前先持锁复查。

#### N-P2-3 KWS 边车重连后，断线期间积压的约 1 秒陈旧 PCM 照常发给边车，可触发幽灵唤醒
- **位置**：`hub/internal/kws/kws.go:324-351`（`SendPCM` 丢最旧帧保新）、`:455-510`（`stream.run` 重连）、`:546-565`（`serve`/`writePump` 直接排空队列）。
- **证据**：队列是 per-stream（不在 per-connection），`serve()` 断线重连后 `writePump` 继续排空同一队列；断线期间 `SendPCM` 仍在入队（满 50 帧即丢最旧，队列恒保持最新约 1 秒音频）。重连成功瞬间，这 1 秒“断线前/断线中”的旧音频被当作实时音频发给边车；边车在其上检出关键词即回 `wake`，`OnKWSWake`（`main.go:169-179`）只按 `timestamp_ms` 算延迟记日志、**不拒绝过期音频上的唤醒**，随即开 20 秒会话。边车的 `timestamp_ms` 是检出时刻而非音频采集时刻，滞后在日志里看不出来。
- **影响**：用户早已停止说话 1 秒后仍可能误开一轮会话；与 P1-13（仲裁无去重/冷却）叠加后，幽灵唤醒可与真实唤醒各赢一次。
- **修复建议**：重连成功（`serve()` 的 `hello` 之后）先清空该流队列再开始发新帧；或给队列帧打采集时间戳，超过 500ms 的帧直接丢弃。

#### N-P2-4 KWS wake 事件的 `device_id` 轻信边车回传值，可把唤醒算到另一台设备头上
- **位置**：`hub/internal/kws/kws.go:610-627`（`handleMessage` 的 `wake` 分支，`firstNonEmpty(m.DeviceID, s.deviceID)`）；`hub/cmd/hub/main.go:169-179`（`OnKWSWake` 直接用 `ev.DeviceID` 仲裁/开会话）。
- **证据**：边车 `wake` 消息里的 `device_id` 被优先采信，流自身的 `s.deviceID`（Hub 在 `hello` 里发过去的）只是回退。边车实现异常/被冒充（边车是无鉴权 `ws://127.0.0.1:10400`，见 N-P2-10 相关）返回别的 `device_id`，Hub 就会给从未唤醒的设备开会话、发 `wake_win`。
- **影响**：低（边车在可信本机）；但这是“标识应以连接为准、不应以对端自报为准”的同类错误，与 P1-44 的型号自报问题同根。
- **修复建议**：`wake` 事件一律用流的 `s.deviceID`，忽略消息里的 `device_id`（不一致时记 warn 日志）。

#### N-P2-5 `kws_status` 最长滞后 30 秒
- **位置**：`hub/internal/kws/kws.go:218-248`（`Start` 的健康检查 ticker，`HealthInterval` 默认 30s）、`:288-316`（`Status()` 的 `Down` 判定）、`hub/cmd/hub/main.go:564-589`（启动时一次 `CheckHealth`，之后只靠 ticker）。
- **证据**：`Down` 由两路决定：`healthChecked && !healthOK`，或 `streams>0 && connected==0`。边车挂掉且**尚无音频流**（`streams==0`，即唤醒从未用过或设备刚断开）时，第二路恒为假，`Down` 只由 30 秒一次的健康检查翻转；边车恢复后，即使流已重连成功（`connected==true`），`healthOK==false` 仍会让 `Down` 保持 true 直到下一个 tick。`main.go` 的 `/api/status`（`web.go:291-309`）直接透传该值。
- **影响**：`kws_down` 报警与恢复指示最长滞后 30 秒；运维在边车重启后 30 秒内看到的仍是旧状态。
- **修复建议**：流断开/重连成功时立即触发一次 `CheckHealth`（或让 `Status()` 在 `connected` 变化时刷新健康判定）；健康间隔可配，默认 30 秒改为 10 秒。

#### N-P2-6 `config.Validate` 漏 `hub.mpd`；`web_listen` 显式写 `:8080` 不拦截
- **位置**：`hub/internal/config/config.go:242-252`（只校验 `hub.stt`/`hub.tts`/`hub.snapcast` 的 host:port + 内网 host，`hub.mpd` 不在列表里）；`:152-160`（`validateListenAddr` 只校验可解析，不校验绑定面）；`hub/internal/web/web.go:312-336`（PUT 只拦 `tls` 降级，不拦 `web_listen` 改回全接口）。
- **证据**：`hub.mpd: "公网IP:6600"` 可通过启动校验，之后 `DialMPD` 直连公网（MPD 协议无鉴权）。`config.yaml` 当前是 `web_listen: "127.0.0.1:8080"`，但显式改成 `":8080"` 或经 Web PUT 改成 `":8080"` 时，P0-1 的“默认只绑回环”不生效——`ApplyDefaults` 只在空值时补默认，不校验显式值。
- **影响**：配置漂移可悄悄把 Web 面板与 MPD 暴露到局域网全接口；与 P0-1 的修复意图相悖。
- **修复建议**：`Validate()` 把 `hub.mpd` 加入 host:port + 内网 host 校验；`web_listen` 要求 host 为回环或明确内网 IP，拒绝空 host（`:8080`）与 `0.0.0.0`，或至少在非回环时启动即 warn。

#### N-P2-7 Web 鉴权失败表 `authFailures` 永不清理
- **位置**：`hub/internal/web/web.go:184-233`（`requireAuth`）。
- **证据**：失败时按 IP 建 `authAttempt` 条目；成功时 `delete` 该 IP；但失败条目本身（即使 `failures` 切片已被窗口过滤为空、`blockedUntil` 已过期）**永不删除**，map 只增不减。局域网扫描器换 IP 段刷一遍即可让表无限增长。
- **影响**：极小（条目很小）；但属于与 P2-28 同类的长期运行泄漏，7×24h 服务应避免。
- **修复建议**：定期清理（失败为空且 `blockedUntil` 已过的条目删除），或失败记录改用带 TTL 的结构。

#### N-P2-8 `nas-install.sh` 的 Hub `/api/status` 检查在 P0-1 修完后恒为 PASS
- **位置**：`deploy/nas-install.sh:149-151`（`curl -s -m 5 http://127.0.0.1:8080/api/status`，无 `-f`、无 Bearer Token）。
- **证据**：P0-1 修复后 `/api/*` 未带 `OPEN_XIAOAI_WEB_TOKEN` 一律 401；但 `curl -s` 在 HTTP 401 时退出码仍为 0（无 `-f`），`&& ok "Hub Web 面板正常"` 恒打印。检查文案还称“应含 wake_word 你好小李”，实际从未校验响应体。部署检查在 Web 面板鉴权/未启动两种情况下都报 PASS。
- **影响**：部署假阳性；与 P1-34（网易云检查恒 PASS，仍开）同类。
- **修复建议**：`curl -fsS -H "Authorization: Bearer $OPEN_XIAOAI_WEB_TOKEN"` 并 `grep -q wake_word` 断言响应体；或改查无需鉴权的存活探针。

#### N-P2-9 `transport` 的 `hello_ok` 写无 deadline；`Serve` 的 `http.Server` 仍无超时
- **位置**：`hub/internal/transport/transport.go:173-191`（`Serve`：`http.Server{Handler: mux}`，无 `ReadTimeout`/`ReadHeaderTimeout`/`WriteTimeout`/`MaxHeaderBytes`）；`:228-232`（hello 成功后 `WriteJSON(hello_ok)`，无写 deadline；此前 10 秒读 deadline 只管读）。
- **证据**：`hello_ok` 写出时对端若只建连不读（慢速/黑洞客户端），该 `handleWS` goroutine 永久阻塞在写上；`Serve` 的 `http.Server` 无超时与首字节读限流（P1-3 已点名，仍开），这里是同一问题的剩余细节。
- **影响**：低（每连接一个 goroutine，堆积缓慢）；但与 `Conn.SendJSON`（有 5 秒写 deadline，`transport.go:104-110`）的防护不一致，属于漏网点。
- **修复建议**：`hello_ok` 写前 `SetWriteDeadline(now+5s)`；`Serve` 的 `http.Server` 加 `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout`（与 Web 面板的 `NewHTTPServer` 同款）。

#### N-P2-10 KWS 边车走 `wss` + 自签证书时 Dial 与健康检查双双失败，无配置可解
- **位置**：`hub/internal/kws/kws.go:502-513`（`dial()`：`websocket.Dialer{HandshakeTimeout}`，无 `TLSClientConfig`）；`:251-281`（`CheckHealth`：`&http.Client{Timeout: 3s}`，默认 Transport）。
- **证据**：`ValidateURL` 允许 `wss`，但拨号与健康检查都用默认 TLS 配置（系统根证书校验）。边车若用自签证书（与 Hub 自身的 mTLS 做法一致），WS 握手与 `/health` 均校验失败；`kws.New` 本身不报错（只校验地址不拨号），Hub 启动后表现为“唤醒服务暂不可用”且无任何指纹 pin 配置项可配。当前默认 `ws://127.0.0.1:10400` 不受影响。
- **影响**：`wss` 边车目前是不可用配置项；配了即静默降级。
- **修复建议**：`Options` 加 `TLSServerName`/`Fingerprint` 或 `InsecureSkipVerify` 的显式测试开关（二选一，默认校验）；`CheckHealth` 与 `dial()` 共用同一 TLS 配置。

#### N-P2-11 点歌时 MA 失败静默降级，无日志
- **位置**：`hub/internal/router/router.go:310-320`（`music()`：`ma.PlayTrackByName` 出错即落入降级分支，`err` 被丢弃）。
- **证据**：MA 分支 `if err := ma.PlayTrackByName(...); err == nil { ... }`，失败时无任何 `log.Printf`，直接走网易云 + MPD 降级。MA 挂掉/鉴权失败/无此歌三种情况在用户侧与日志里完全不可区分，排障只能靠猜。
- **影响**：MA 长期故障不可见；点歌成功率下降无告警。
- **修复建议**：MA 失败记一条 warn 日志（含 query 与错误摘要，截断上游响应体，见 P2-7）。

#### N-P2-12 KWS 重连退避无 jitter，两台设备流可同步重连脉冲
- **位置**：`hub/internal/kws/kws.go:480-501`（`nextBackoff` 纯翻倍，500ms→30s，无随机化）。
- **证据**：两台设备的流在边车同一次重启后同时进入退避序列，退避值完全相同（同一起点、同一翻倍），重连脉冲同步化；与首轮 P1-43（客户端重连无 jitter）同一问题在 Hub 侧的翻版。
- **影响**：低（仅 2 台设备，脉冲幅度小）；边车重启瞬间双流同时拨号。
- **修复建议**：退避加 ±25%~50% jitter。

---

## 必查项逐项结论

### P1-44 自动发现方向问题：仍开（生产扫描永远找不到设备；安全模型已修，方向未修）
- **方向未修**：`discovery.go:47-89` 的 `ProbeModel` 仍假设音箱在 `:4399` 上监听并做 WS 握手；客户端只有出站 `connect_async`，无任何监听 socket（首轮证据未变）。`main.go:647-677` 的启动扫描仍走同一路径，只是多了 `ScanEnabled`/`DiscoveryFingerprints`/`TLS` 三道门。生产扫描对每个 IP 都是连接被拒，`Scan` 永远返回空；且 `Scan` 在 ctx 超时时仍返回 `(found, nil)`（`discovery.go:300-314`），调用方无法区分“扫完没找到”与“扫的过程被打断”。
- **安全模型已修（P0-4 的部分）**：扫描默认关闭（`Enabled=false` 时 `Scan` 拒绝，`discovery.go:259-261`）；先匿名探测（`probeLeafCert` 只建 TCP + TLS 握手、不发应用层数据、不发 Token，`:138-161`）；指纹在白名单内才建 wss 发 Token（`ProbeModel`，`:181-247`）；端口取 `cfg.Hub.Listen`（`main.go:621-645` 的 `discoveryPort` + `newDiscoveryScanner`）；`main.go` 只在显式开启 + 配了指纹白名单 + TLS 开启时才扫描。测试 `go test ./internal/discovery/` 通过。
- **仍开的次级问题**：
  - 型号误标：`Scan` 仍先试 `lx06` 再试 `oh2p`（`discovery.go:288-297`），而 `transport.go:211-214` 只校验 model 在 `lx06/oh2p` 之内、不校验真实型号——第一个探针（lx06）对任何一台真音箱都会“成功”，型号恒被标为 lx06。
  - 探针污染注册表（P2-29 仍开）：`ProbeModel` 的 hello 里仍写 `version: "probe"`（`discovery.go:222`），经 `transport.handleWS` 的 `Upsert` 会覆盖设备的真实版本号（`registry.go:44-62` 非空即覆盖），直到下一次真实 hello/status 才纠正；探针连接关闭时 defer 还会把设备标离线（`transport.go:240-246`）。
  - 网段推导仍武断（P2-47 仍开）：`localCIDR()`（`main.go:680-699`）取第一个非回环接口 IP 并强制 `/24`，无视真实掩码；`Scan` 仍为每个候选 IP 起一个 goroutine（信号量 32 只限探针并发，不限 goroutine 数）。
- **结论**：发现的安全重做是实的，但方向性错误未动——Hub 应以 hello 注册为唯一事实来源，或改 mDNS/UDP 发现；当前扫描在生产上是“安全的空转”。

### P1-9 OTA 验证与回滚仍从未被生产代码调用：仍开
- **证据**：全仓 grep（除测试与 `ota.go` 本身）：`ConfirmHello`、`MarkRebooted`、`MarkDownloading`、`otaMgr.Sweep` 在生产代码中**零调用**。`main.go` 全文无调用；`transport.handleWS` 的 hello 成功处（`transport.go:213-238`）未调 `ConfirmHello`、未把版本号与 OTA 任务比对；`main.go` 无任何定时器调 `Sweep`；`otaMgr` 只在 `web.go:132` 的 `Push()` 与 `main.go:619-629` 的 `PushOTA` 下发处出现。`ota.go:92-131` 的 `MarkRebooted`/`ConfirmHello`/`Sweep` 实现本身未变（30 秒超时、`VerifyTimeout` 默认 30s 未放宽，P2-38 仍开）。
- **次级问题仍开**：`web.go:360-393` 的 `otaAPI` 在 `PushOTA == nil` 时仍返回 200 + job（只在非 nil 且下发失败时才 500）；无查询任务状态的 GET 接口，操作员无法在 Web 上看到任务状态。
- **结论**：OTA 下发后任务永远停在 `pushing`，失败的升级 Hub 永远发现不了；FR-11 端到端仍是零实现。另客户端 `HubMsg::Ota` 只记日志是非 Hub 面，本轮不复查。

### P1-15 对讲仍无生产触发入口；`playing` latch bug 仍在：均仍开
- **触发入口仍开**：全仓 grep：`intercom.Start`、`StartBroadcast`、`SetFullDuplex`、`SetHalfDuplex`、`EchoCancel` 在生产代码中**零调用**；`router.go` 全文件无“呼叫/对讲/广播”分支；`main.go` 只调到 `Forward`（`OnAudioFrame`，`:108-126`）与 `End`（`OnAudioEnd`、`:194-195`，`OnDisconnect`、`:255`）。语音说“呼叫卧室”仍落到 Agent 闲聊。
- **超时清理仍开**：`intercom.Sweep` 在生产代码中**零调用**；`main.go` 只有 `sweepSessions` 的 goroutine（`:271-282`），无 `intercom.Sweep` 的定时器。目标方断线时 `OnDisconnect` 只 `End(断线方)`（即只清以该设备为发起方的会话），断线方是目标时会话永不结束，`participantBusy` 永久拒绝该发起方的新呼叫。
- **P1-17 仍开**（同函数内）：`main.go:108-126` 的 `Forward` 失败（`err != nil`，如目标刚离线）时不 return，继续走语音管线；若该设备恰在 20 秒会话内，对讲音频会被当成语音命令转写并执行。
- **`playing` latch bug 仍在（P1-16 全部三点）**：
  1. `main.go:490-508` 的 `onReply`：置 `playing=true` → 发 `start_play` RPC → 循环 `SendBinary` → **发完网络帧的瞬间即置 `playing=false`**，从不发 `stop_play`；客户端的 `playing` 在第一次回答后永久为 true（客户端面，未复查）。
  2. `main.go:477-515` 的 `speakText`：直接 `SendBinary`，**不发 `start_play`、不置 `playing`**；对讲中继（`main.go:108-126` 的 `Forward` 成功后直接 `SendBinary`，广播展开同理）同样只发裸音频帧。客户端 `playing==false` 时这些帧会被静默丢弃（客户端行为，引首轮报告；Hub 侧发帧路径未变）。
  3. 打断判断恒 false：`OnWake`（`main.go:79-94`）读 `playing` 判断“播放中收到唤醒 = 打断”，但 `playing` 只在发帧的毫秒级窗口内为 true，真实播放期间几乎恒为 false，打断逻辑名存实亡。另 `onReply` 在帧列表为空（TTS 返回 <320 点，见 P2-48）时仍发 `start_play`。
- **结论**：FR-04 对讲在生产上仍不可用；提醒/告警播报（`speakText`）仍大概率无声；打断仍基本收不到。

### P1-27 MA 恢复后 QueueID 不热更新：仍开
- **证据**：`main.go:383-395` 注释明说“MA 恢复后需重启 Hub，见 P1-27”；`router.SetQueueID`（`router.go:130-136`）存在但**生产代码零调用**；MPD/Snap 有 30 秒后台重试 goroutine（`main.go:437-468`，仅当启动时 Dial 失败才建），MA 没有任何重试 goroutine；HA（`main.go:243-251`）、Wyoming STT/TTS（`:274-289`）同样一次 Dial、失败即 nil/缺失，全仓无 `reconnect`/`redial`/`health` 相关生产代码。
- **部分修复**：MPD/Snap 启动时没连上时后台每 30 秒重试、连上即经 `rt.SetMPD`/`rt.SetSnap` 补接（P1-26 的一部分，已修）。
- **仍开的缺口**：MA 恢复后 QueueID 不热更新（需重启 Hub）；MPD/Snap **中途断线**无探活、无重连（重试 goroutine 只在启动失败时存在，连上后即 `return` 退出）；HA 断线后 `haClient` 成僵尸（读循环退出即永久失效，`ha.go:102-135` 的 `readLoop` 读错即 `return`，无重连、无通知，`watcher` 订阅随之永久丢失）；Wyoming 断线后 STT/TTS 保持 nil（P1-2 的降级回复只能播“语音服务暂时没连上”，永不自愈）。
- **结论**：下游重连仍是“启动时一次 + MPD/Snap 初次失败重试”的局部状态，P1-27 核心未修。

### 服务端唤醒接线（首轮之后的新代码）：可用，但有 5 处可验证的缺口
接线链路（已逐段读代码验证通路存在）：`OnAudioFrame`（`main.go:97-152`）每帧解码一次 → `kws.SendPCM`（`kws.go:324-351`，非阻塞、队列满丢最旧）→ 边车每设备一路 WS 流（`stream.run`/`serve`，`kws.go:455-565`）→ `wake` 事件进缓冲 32 的 `Events` channel（满时丢事件并记日志，`kws.go:610-627`）→ `main.go:575-579` 的 goroutine 消费 → `OnKWSWake`（`:169-179`，线性 rms 换算 `20*log10(rms)`，`rms<=0` 记 -100dB）→ `OnWake`（`:79-94`，打断判断 + 仲裁 `Submit`）→ 250ms 仲裁窗口 → 赢家开 20 秒会话 + 发 `wake_win`，输家发 `wake_lose`（`:542-556`）。测试覆盖：`kws_test.go` 11 个测试、`kws_wiring_test.go` 6 个测试（含双设备仲裁、断线重连、边车不可用降级、断开清理、RMS 换算），均通过（合成 PCM + 假边车，非真人唤醒率）。

逐项结论：

- **边车断线/重连退避**：有，指数退避 500ms→30s（`kws.go:480-501`），拨号失败与 serve 断开都走同一退避；`serve` 内写泵每 30 秒发 ping、读泵 90 秒 deadline + Pong 续期（`:546-601`），下行 JSON 限 64KB（`maxInboundBytes`）。缺口：退避无 jitter（N-P2-12）；重连后不清空陈旧队列（N-P2-3）。
- **背压丢帧**：有，每设备队列默认 50 帧（约 1 秒），满时丢最旧一帧并计数、首丢与每满 100 帧记一次日志（`kws.go:324-351`）；`SendPCM` 永不阻塞调用方。缺口：丢帧计数只在 `Status().DroppedFrames` 里暴露，`main.go` 不记 metrics、不告警。
- **WebSocket 流处理**：每设备独立流（边车靠连接区分设备，二进制帧不带 device_id，不可复用）；`serve` 里写泵/读泵双 goroutine，任一退出即关连接、等两泵都停后重连（`kws.go:514-544`）。缺口：`serve` 返回 `<-errCh` 取先到的错误，可能取到另一泵的 nil（只影响日志）；`readPump` 对非文本帧静默 `continue`（边车发二进制即无声丢弃）。
- **多设备隔离**：有，`streams` map 按 deviceID 隔离（`kws.go:353-371`），`RemoveDevice` 在 `OnDisconnect` 调（`main.go:256-260`），测试 `TestKWSOnDisconnectCleansStream` 覆盖。缺口：wake 事件的 `device_id` 轻信边车回传（N-P2-4）；`streamFor` 与 `Close` 的竞态（N-P2-2）。
- **`kws_status` 滞后 30s**：属实（N-P2-5）。健康检查 ticker 默认 30 秒（`kws.go:218-248`），`Status().Down` 在无流时只靠健康检查翻转；边车恢复后即使流已重连，`healthOK==false` 也会让 `down` 保持到下一个 tick。`main.go:564-589` 启动时做一次同步 `CheckHealth`（3 秒超时），之后全靠 ticker；`/api/status` 的 `kws_status`/`kws_down`（`web.go:291-309`）直接透传。
- **配置缺失时的 fail 行为**：fail closed，且分三档（`config.go:303-321` 的 `Validate` + `main.go:564-589`）：`enabled=true` 但 url/keywords 缺配或配错 → `kws.New` 报错 → `log.Fatalf` 启动失败并明说原因；`enabled=true` 且边车启动时不可用 → 记日志警告、继续启动（`kwsClient` 已建，流按需懒建，`kws.go:196-214` 的 `New` 只校验地址不拨号），`/api/status` 报告 `kws_down`；`enabled=false` 但配了 url → 记警告、不连边车；`enabled=false` 且未配 url → 记日志、唤醒不可用。`ValidateURL`（`kws.go:139-165`）只许 ws/wss + 内网 host、无 userinfo、必须带路径。
- **wake 事件到仲裁再到管线的完整链路**：通（见上）。但两处断裂点仍在链路上游：仲裁本身的 P1-13 未修（`OnWake` 用 `time.Now()` 覆盖时间戳，`main.go:87-91`；`OnKWSWake` 传的 `TsMs` 只用于记延迟日志，不进仲裁；同一声唤醒迟到 250ms 以上会开第二窗口各赢一次；赢家在窗口关闭到发 `wake_win` 之间断线时无人补位）；`OnKWSWake` 与客户端本地 `wake` 消息是双通道、无去重（同一声唤醒可提交两次，见 N-P2-13 的相关说明）。

### Hub 面其余首轮延期项逐项复查

每项一句话状态 + 证据。统计口径：Hub 面延期项 = 首轮 P0/P1/P2 中触及 `hub/` 或 Hub 部署物的项（共 76 项；纯客户端 12 项、纯固件补丁 20 项不在本轮复查范围，见表后说明）。

| 编号 | 状态 | 一句话证据 |
|---|---|---|
| P0-1 Web 零鉴权 | **已修** | `/api/*` 加 Bearer 鉴权（独立 `OPEN_XIAOAI_WEB_TOKEN`，未配则 401 fail closed）+ 恒定时间比较 + 单 IP 限流；`NewHTTPServer` 带超时与 1MB 体上限；`WebListen` 默认 `127.0.0.1:8080`；GET config 脱敏。`go test ./internal/web/` 通过。 |
| P0-3 单 Token 无 mTLS | **已修** | `ValidateToken`（空/<16 位/弱值即 Fatal）+ `LoadOrCreateMTLS` + `RequireAndVerifyClientCert`，`main.go` 已接线；`tokenEqual` 改 SHA-256 后恒定时间比较。剩余：`CheckOrigin` 恒 true 与 deviceID 顶替属 P1-3，未修。 |
| P0-4 发现广播 Token | **已修** | 扫描默认关闭；先匿名探测取证书指纹、白名单内才发 Token；端口取 `cfg.Hub.Listen`；`main.go` 三道门。剩余：P1-44 方向问题未修。 |
| P1-1 Wyoming 帧校验 | **已修** | `parsePayloadLength` 校验 + 头行限长 64KB + 读写 10s deadline + `Transcribe`/`Synthesize` 120s 整体超时与总量上限。 |
| P1-2 STT/TTS nil panic | **已修** | `answer()` 开头判 nil 返回明确错误；单句上限 30 秒（480000 采样点）+ 总并发 4 路；`main.go` 降级播报。 |
| P1-3 传输层无限制 | **仍开** | 无 `SetReadLimit`、无速率限制/失败封禁、`CheckOrigin` 恒 true、deviceID 仍为 model+IP、同 IP 新连接踢旧连接；`Serve` 的 `http.Server` 仍无超时。 |
| P1-4 Compose 暴露面 | **仍开** | whisper/piper/mosquitto/MA/网易云端口仍发布到局域网，mosquitto 仍无配置/密码文件，镜像仍全是 `:latest` 无 digest，hub 容器仍 host 网络 + root 运行、无 `USER`/资源限制。 |
| P1-5 HA 明文 + 读挂起 | **仍开** | `config.yaml` 仍为 `ws://192.168.1.10:8123`；`Dial` 两次 `ReadMessage` 仍无 deadline；空 token 仍会发一次 auth 再失败。 |
| P1-6 下游明文密钥 | **仍开** | Agent/MA/网易云仍走明文 HTTP；`MA_TOKEN` 为空即无认证；`agent.Chat` 错误分支仍把上游响应体全文拼进错误（P2-7 同）。 |
| P1-7 MPD 换行注入 | **仍开** | `PlayURL`（`music.go:463-468`）仍对 URL 不做 `\r`/`\n`/控制字符校验，直接 `add %s`。 |
| P1-9 OTA 验证零调用 | **仍开** | 见必查项：生产零调用；`otaAPI` 在 `PushOTA==nil` 时仍回 200。 |
| P1-11 60s 读超时误杀 | **仍开** | Hub 侧仍无任何 Ping 发送（全仓 grep `WriteControl`/`PingMessage` 零命中）；读 deadline 仍一次性设 60 秒、读循环里永不续期；注释“靠 ping/pong 保活”与代码自相矛盾。 |
| P1-12 重连误删新连接 | **仍开** | `transport.go:240-246` 的 defer 仍按 deviceID 无条件 `delete` + `SetOnline(false)`，无连接身份比对。 |
| P1-13 仲裁忽略时间戳 | **仍开** | `Arbitrate`/`Arbiter` 仍不用 `Timestamp`；无冷却期、无去重、无赢家失败补位；`OnWake` 仍用 `time.Now()` 覆盖。 |
| P1-15 对讲零触发 | **仍开** | 见必查项：`Start`/`Sweep` 生产零调用；`OnDisconnect` 只单向 `End`。 |
| P1-16 播放状态机错 | **仍开** | 见必查项：三点全在（立即复位 + 无 `stop_play`；`speakText`/对讲裸帧；打断恒 false）。 |
| P1-17 对讲失败落管线 | **仍开** | `main.go:108-126` 的 `Forward` 出错仍不 return，继续走语音管线。 |
| P1-18 紧急呼叫谎报 | **仍开** | `router.go:282-285` 的 `_ = r.deps.Notify` 后仍无条件回“已经帮你呼叫急救并通知了家人”；重复触发仍不重试；`emergencyCall` 仍无 `notified` 字段。 |
| P1-19 “报警”误触发 | **仍开** | `emergencyRe`（`router.go:251`）仍含裸 `报警`，无词边界、无否定排除。 |
| P1-20 Wyoming 无锁 | **仍开** | `wyoming.Client`（`wyoming.go:32-36`）仍无 `sync.Mutex`；两台设备 + 提醒 goroutine 并发调同一 STT/TTS 连接时读写交错。 |
| P1-21 HA 并发写不安全 | **仍开** | `ha.go:146-175` 的 `call()` 与 `SubscribeEvents` 的 `WriteJSON` 仍不持写锁；本轮 `-race` 的测试替身竞态即此 bug 类的演示。 |
| P1-22 Web PUT 假热更新 | **仍开** | PUT 仍全量替换（`web.go:312-347`，缺字段即清空、无合并）；运行中 Agent/HA/Wyoming/Router 仍不重建；`OnRoomClaim` 与 PUT 各写各的指针与同一 `.tmp` 文件；JSON 字段仍为 Go PascalCase、与 YAML snake_case 不一致。 |
| P1-23 Devices map 无锁 | **仍开** | `OnRoomClaim`（`main.go:213-233`）仍无锁写 `h.cfg.Devices` 并 `Save`；Web GET/PUT 并发读写同一 map；`Save` 仍固定用 `path+".tmp"`。 |
| P1-24 未知 Kind 每秒触发 | **仍开** | `reminders.go:202-237` 的 `tick` 的 switch 仍无 `default` 分支；未知 Kind 既不删也不重排，`Next` 恒 ≤ now，每秒触发一次 `onFire` 永不停；`Add`/`Load` 仍不校验 Kind。 |
| P1-25 每日提醒越界无声 | **仍开** | `router.go:217-232` 的 hour/min 仍无范围校验；`每天 25 点` 仍经 `NextDaily` 报错 → `Route` 返回 error → 只记一条日志，用户全程无声、提醒未建。 |
| P1-26 点歌接线断 | **已修** | 启动时查 MA `Players()` 取默认 `player_id` 作 `QueueID`；`DialMPD`/`DialSnap` 已注入；启动失败后台 30 秒重试、`SetMPD`/`SetSnap` 补接。 |
| P1-27 下游无重连 | **部分** | MPD/Snap 启动失败重试已加；但 MA 恢复后 QueueID 不热更新、HA/Wyoming/MPD/Snap 中途断线仍永不重连。见必查项。 |
| P1-28 语音无上限/断线不清 | **部分** | 单句 30 秒上限已加；但 `OnDisconnect` 仍不清理 Pipeline 残留 utterance、不删 `decoders`（`main.go:253-261` 只删会话 + `intercom.End` + `kws.RemoveDevice`）。 |
| P1-29 回答同步阻塞 | **仍开** | `answer` 仍在 transport 读循环里同步跑（STT→Router→TTS→编码），`context.Background()` 无超时；`ChatStream` 生产零调用；下游失败仍只记日志（P1-2 降级回复只覆盖 STT/TTS 不可用与管线繁忙两类）。 |
| P1-31 compose 卷挂错 | **已修** | 卷已改挂 `hub-data:/app/hub-data`（文件核对）。剩余：NAS 实测 BLOCKED。 |
| P1-32 config 只读挂载 | **已修** | 挂载已去 `:ro`；`OnRoomClaim` 的 `Save` 失败已记日志。剩余：NAS 实测 BLOCKED；可写/种子分离未修（P1-22/P1-23 范围）。 |
| P1-33 安装脚本覆盖配置 | **仍开** | `nas-install.sh:95-96` 仍 scp 全量覆盖 NAS 端 `config.yaml`；`:104` 的 `.bak` 仍是覆盖**之后**从新文件复制的，备的不是旧文件。 |
| P1-34 网易云检查恒 PASS | **仍开** | `nas-install.sh:141-142` 的检查仍以 `curl … | head -c 200; echo` 结尾，`echo` 恒返回 0，ssh 恒返回 0，`ok` 恒打印；仍无 `-f`。 |
| P1-35 安装脚本放行弱 Token | **已修** | 预检查已拒绝 <16 位与 `CHANGE_ME` 等弱值（`nas-install.sh:50-62`），`FAIL>0` 即退出（`:69`）。 |
| P1-37 mosquitto 匿名 | **仍开** | compose 的 mosquitto 仍映射 `1883`、无配置卷、无密码文件；Hub 代码仍无 MQTT 引用。是否默认匿名仍需 NAS 核实（同首轮）。 |
| P1-38 多 Agent 路由假 | **仍开** | `main.go:251-262` 启动时选定一次；回退分支仍为 Go map 随机迭代 `break`；`OnWake` 与 `Route` 仍不看 `WakeWord`。 |
| P1-39 房间认领端到端断 | **仍开** | `router.go` 仍无“我在这里/我在 X”解析；`room_claim` 仍无生产发送方（客户端面）；服务端 `OnRoomClaim` 仍只有测试调用。 |
| P1-41 记忆只写不读 | **仍开** | `Memory.All` 生产零调用；“2 小时后关灯”类定时动作仍无分支（`afterRe` 只匹配“提醒我”）。 |
| P1-42 查状态失败照调 | **仍开** | `router.go:380-393` 的 `GetEntityState` 出错仍跳过比较、直接 `CallService`；查询类命令（“灯开着吗”“多少度”）仍无分支。 |
| P1-44 发现扫错方向 | **仍开** | 见必查项。 |
| P1-45 配置校验缺 | **部分** | `Validate` 已大幅补强（下游 URL scheme/host 白名单、STT/TTS/Snapcast host:port、`scan_cidr`、KWS URL/keywords）；但仍缺：`do_not_disturb` 格式、`wake_word_routes` 的值须在 `agents` 里存在、`music_folder` 存在性启动检查、`hub.mpd` 校验（见 N-P2-6）、种子设备空 IP。 |
| P1-46 部署物无时区 | **仍开** | compose 与 Dockerfile 仍无 `TZ`/`tzdata`/`/etc/localtime` 挂载（grep `TZ` 零命中）；容器默认 UTC，提醒与勿扰仍整体错 8 小时。 |
| P2-1 证书 10 年有效 | **仍开** | `tlscert.go:82,174,268` 三处 `NotAfter` 仍为 10 年；SAN 仍只有回环地址；指纹校验仍忽略有效期与主机名。 |
| P2-2 Token 比较时序 | **部分** | 比较已改 SHA-256 后恒定时间比较（`transport.go:156-160` 的 `tokenEqual`）；但 hello 拒绝原因仍可区分枚举（`first message must be hello` / `bad token` / `unsupported model` 各回不同 `reason`）。 |
| P2-3 字段无长度校验 | **仍开** | `OnRoomClaim` 只 `TrimSpace`、判空，无长度/字符集限制；`Hello` 的 `Model`/`Room`/`Version` 原样进注册表与 `/api/devices`；房间名含 `\n` 可伪造日志行（`main.go:233` 的 `log.Printf`）。 |
| P2-4 数据文件 0644 | **仍开** | `config.Save`、`reminders.saveLocked`、`memory.saveLocked` 仍为 `0644`；`MkdirAll(DataDir, 0755)` 仍为 `0755`。 |
| P2-5 PUT 与认领互覆盖 | **仍开** | 见 P1-22：两个指针、同一 `.tmp` 文件、无锁；`OnRoomClaim` 写回的是 PUT 换指针前的旧 `Config` 对象，会把 PUT 的修改 whole-file 覆盖丢。 |
| P2-7 上游响应无上限 | **仍开** | MA `command` 的 `io.ReadAll`（`music.go:312`）仍无上限；Agent 错误分支仍回显上游响应体全文（`agent.go:86,124`）；网易云 `Decode` 仍无 `MaxBytesReader`。Web 侧 PUT/POST 已加 1MB 上限（已修的一半）。 |
| P2-11 HA 回调同步执行 | **仍开** | `ha.go:104-145` 的 `readLoop` 里事件 handler 仍同步执行；告警 TTS（数秒）仍会堵住 HA 结果分发；回调 panic 仍会杀死 `readLoop`。 |
| P2-12 紧急呼叫无二次确认 | **仍开** | 任何语音（含电视/录音/广播）说“救命/打120”即触发，无声纹/二次确认。 |
| P2-13 提醒可重复 Start | **仍开** | `reminders.Start` 仍无“已启动”守卫；`Stop` 在 `main.go` 全文仍无调用。 |
| P2-14 提醒落盘错误吞掉 | **仍开** | `tick`/`Cancel`/`CancelByText` 的 `_ = s.saveLocked()` 仍在（`reminders.go:133,148,230`）。 |
| P2-15 记忆 key 秒级碰撞 | **仍开** | `memory.go:44-52` 仍用 `time.Now().Format(time.RFC3339)`（秒级）做 key，同一秒内两次“记住”仍互相覆盖。 |
| P2-16 HA 写失败泄漏 pending | **仍开** | `ha.go:165-195` 的 `call()` 写失败仍直接 `return`，未删 `c.pending[id]`。 |
| P2-17 Wyoming 无超时/死循环 | **部分** | `Transcribe`/`Synthesize` 已加 120 秒整体超时与总量上限；但未知事件类型仍静默循环（无计数、无上限），只靠整体超时兜底。 |
| P2-22 AEC 零能量步长爆炸 |
| P2-24 nas-install 检查水分 | **部分** | 1705/1704 端口检查已补；但 4399 仍漏、ps 仍无计数、sed 仍无校验、curl -f 只修了 MA 检查（网易云仍是 P1-34）。 |
 **仍开** | `aec.go:58-90` 的 `step = mu*e/(norm+eps)` 仍为 `eps=1e-6`、无能量阈值门；参考信号全零时步长仍爆炸。`EchoCancel` 生产零调用，故仍为 P2。 |
| P2-25 勿扰格式非法静默 | **仍开** | `InDoNotDisturb` 解析失败仍返回 `false`（按“不在勿扰”处理）；`Validate` 仍不校验 `DoNotDisturb` 格式。 |
| P2-26 安全判定子串匹配 | **仍开** | `KindFor`（`alerts.go:52-63`）仍只看 entity_id 含 `leak`/`smoke`/`water` 子串，不校验 domain 是否为 `binary_sensor`。 |
| P2-27 ByRoom 随机/幽灵行 | **仍开** | `ByRoom` 仍遍历 map 取第一个在线（Go map 随机顺序）；`config.yaml` 的 `livingroom-speaker`/`bedroom-speaker` 空 IP 占位仍在，与真实 ID（`model-IP`）永远对不上。 |
| P2-28 decoders 永不回收 | **仍开** | `decoderFor` 只增不减；`OnDisconnect` 仍不删（见 P1-28）。 |
| P2-29 探针覆盖版本号 |
| P2-36 Dockerfile 无 USER/HEALTHCHECK | **仍开** | 仍 `FROM debian:bookworm-slim` 无 digest、无 `USER`、无 `HEALTHCHECK`、无 `.dockerignore`，二进制无校验。 |
| P2-37 compose 无资源限制 | **仍开** | 仍无 `deploy.resources`、无 `user:`、无内存/CPU 上限。 |
 **仍开** | `ProbeModel` 仍发 `version: "probe"`；`Upsert` 仍非空即覆盖。见 P1-44。 |
| P2-43 日志含语音文本 | **仍开** | `main.go:487` 的 `speakText` 仍把提醒/告警全文记日志；`router.go:281` 的紧急消息含触发原文。 |
| P2-44 提醒存储细节 | **仍开** | `tick` 仍每秒全量写盘（无论有无变化）；`Load` 损坏仍静默丢全部提醒（`main.go:267-269` 的 `_ = remStore.Load()` 无日志）；`Stop` 后无法再 `Start`（`stopCh` 已关闭）；`onFire` 仍同步执行。 |
| P2-45 提醒输入细节 | **仍开** | `每 0 分钟` 仍立即触发一次随后被删除；`CancelByText` 仍要求全文精确相等；仍无“改时间”入口。 |
| P2-46 ChatStream 行缓冲 | **仍开** | `agent.go:102-158` 的 scanner 上限仍为 1MB，超长行仍丢弃整段已生成回答（`sc.Err()` 后 `return "", err`，`full` 被丢弃）。 |
| P2-47 发现 goroutine 风暴 | **部分** | `Scan` 对非法 CIDR/未开启已返回真实错误（不再恒 nil 的一部分）；但仍为每个 IP 起一个 goroutine，ctx 超时时仍返回 `(found, nil)`，`localCIDR` 仍强制 `/24`。 |
| P2-48 语音管线细节 | **部分** | 尾部不足 320 点的采样仍丢弃、合成采样率仍不校验；但“同设备并发跑两次回答”一半在生产上**不可达**（每设备只有一条 transport 读循环串行调 `FramePCM`/`End`，首轮的并发前提不成立——以代码为准的纠正，见方法说明）。 |
| P2-49 场景绕过 allowlist | **仍开** | `sceneMemory` 的“我回来了”仍直接 `CallService("scene","turn_on","scene.coming_home")`，不校验场景内实体 domain。 |
| P2-50 点歌不拆分歌手 | **仍开** | `music()` 仍把“周杰伦的青花瓷”整个串当歌名传给 MA，`artist` 参数恒为 `""`。 |
| P2-51 注册表房间语义 | **仍开** | `Upsert` 仍无法清空房间；hello 的 `Room` 每次重连仍覆盖语音认领的房间；种子键与真实 ID 仍对不上。 |
| P2-53 主循环收尾 |
| P2-55 部署物缺失 | **部分** | `.env.example` 已补（占位值 + 注释）；但二进制 mtime 校验仍无（只查存在）、sed 替换仍无断言。 |
 **仍开** | `sweepSessions` 仍用 `time.Tick`（`main.go:271`，永不 `Stop`）；wss/ws 服务 goroutine 里仍 `log.Fatalf`；退出信号处理仍只关 KWS 与 HA，不调 `remStore.Stop()`、不关 Wyoming/MPD/Snap/Web。另 MPD/Snap 重试 goroutine 也用 `time.Tick`（`main.go:440,454`）。 |
| P2-56 Web 面板细节 | **部分** | `/api/*` 已加鉴权、GET config 已脱敏；但 `index` 处理器仍匹配所有未命中路径并返回 200 HTML（无 404），仍会掩盖调用方拼错路径。 |

**延期复查统计（Hub 面，共 76 项）**：

| 状态 | 数量 | 明细 |
|---|---|---|
| 已修 | 9 | P0-1、P0-3、P0-4、P1-1、P1-2、P1-26、P1-31、P1-32、P1-35 |
| 部分修复（核心动了，子项仍开） | 10 | P1-27、P1-28、P1-45、P2-2、P2-17、P2-47、P2-48、P2-56、P2-24、P2-55 |
| 仍开 | 57 | 其余全部（含 P1-3、P1-9、P1-11、P1-12、P1-13、P1-15、P1-16、P1-17、P1-18、P1-19、P1-20、P1-21、P1-22、P1-23、P1-24、P1-25、P1-29、P1-38、P1-39、P1-41、P1-42、P1-44、P1-46 等全部 P1 核心接线项） |
| 已作废 | 0 | 无一项因设计变更而作废——延期项全部仍是有效问题 |

未复查（非 Hub 面，共 32 项）：纯客户端 12 项（P1-10、P1-14、P1-40、P1-43；P2-18、P2-19、P2-20、P2-21、P2-40、P2-41、P2-42、P2-54）、纯固件补丁 20 项（P0-2、P0-5、P0-6，首轮注记为已修但本轮未复验；P1-8、P1-30、P1-36；P2-6、P2-8、P2-9、P2-10、P2-23、P2-30、P2-31、P2-32、P2-33、P2-34、P2-35、P2-38、P2-39、P2-52）。P1-37（mosquitto）按部署物计入 Hub 面统计，状态仍开。76 + 32 = 108，与首轮总数一致。

## 诚实性说明与 BLOCKED 项

- **未做真机验证的项（BLOCKED，不写成通过）**：60 秒断线在真机上的表现与重连节律（P1-11/P1-12 的生产触发频率）；服务端 KWS 的真人唤醒率与双设备仲裁胜率（测试只用合成 PCM + 假边车）；NAS 上容器重建后提醒/记忆/TLS 指纹是否不变（P1-31）、语音认领房间重启后是否仍在（P1-32）；OTA 回滚真机行为（P1-9）；HA/MA/MPD/Snapcast 在 NAS 上的真实连通性与延迟（P1-26 的 ≤10ms 同步目标）；mosquitto 是否默认匿名（P1-37）；时区错 8 小时的 NAS 实测（P1-46）。
- **未确认的项**：P2-48“同设备并发两次回答”经代码核查为生产不可达（已在表中纠正首轮的前提假设）；其余各项均有代码或测试输出证据，未猜测。
- **首轮报告的 3 项 A+B 之外“已修”注记**：P0-1、P0-3、P0-4、P1-1、P1-2、P1-26、P1-31、P1-32、P1-35 在本轮代码复查中确认属实（P1-35 为部署脚本修复，不在 A+B 12 项内但代码已改）；P1-26 的修复含 MPD/Snap 后台重试，属实。
- **测试覆盖缺口**：`go test` 156 通过但 `go test -race` 2 个包失败（见 N-P2-1）；`hub/cmd/hub` 无生产 `main()` 的集成测试（只有接线 harness 测试）；OTA 状态机、对讲触发、播放状态机、HA/Wyoming 重连均无生产路径测试——包内单测通过与生产接线断裂并存，是本轮与首轮一致的核心结论。

## 最严重的 3 项（摘要）

1. **P1-11 + P1-12 仍开（首轮 P1，未修）**：Hub 从不发 Ping、客户端从不发 Ping，60 秒读 deadline 一次性生效，每台音箱每 60 秒必断线一次；重连时旧连接的 defer 按 deviceID 无条件删除，会误删刚注册的新连接并把设备标离线——设备“假在线、收不到回复”，直到下一次重连。这是生产态语音链路最高频的故障。
2. **P1-15 / P1-16 / P1-9 接线断裂群仍开（首轮 P1，未修）**：对讲无触发入口且超时清理从不运行（FR-04 未实现）；`playing` 在发完帧瞬间复位、从不发 `stop_play`，`speakText` 与对讲只发裸帧（提醒/告警大概率无声、打断恒 false）；OTA 验证闭环生产零调用，任务永卡 `pushing`（FR-11 未实现）。
3. **新 N-P1-1 + N-P1-2**：Snapcast `call()` 不校验响应 id，服务端异步通知会被当成正常响应、真响应错配到下一次调用，点歌后的延迟校准/切流可静默失效；`DialMPD` 读欢迎行无 deadline，MPD 异常时 Hub 启动永久挂起且无日志。
