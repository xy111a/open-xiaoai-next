# open-xiaoai-next 第二轮审计报告（AUDIT_ROUND2.md）

- 审计日期：2026-11-17（晚）
- 审计分支：`audit-round2-ci-fix`（从 main `509e57a` 切出）
- 首轮报告：`AUDIT_REPORT.md`（2026-11-17 13:19，108 项 P0 6 / P1 46 / P2 56；只修了 A+B 12 项）
- 本轮分工报告（证据全文）：`.audit-round2/hub.md`、`.audit-round2/client.md`、`.audit-round2/kws-sidecar.md`、`.audit-round2/firmware-deploy-ci.md`

## 0. 结论先行

- **新问题共 55 项：P0 1 / P1 14 / P2 40。** 唯一的 P0 是客户端面双开 ALSA（P0-N1）：真机上播放可能全灭，刷机前必须修。
- **首轮 96 项延期项复查**：已修 1（P1-35，A+B 之外新确认）、部分修复 17、仍开 77、建议销项 1（P2-31）。P1-44、P1-9、P1-15/P1-16、P1-27 四个必查项**全部仍开**。
- **最严重的部署断裂（新 P1）**：`OPEN_XIAOAI_WEB_TOKEN` 从未接进部署——NAS 上的 Hub 以空 Web Token 启动，`/api/*` 全部 401，Web 面板与 OTA 下发入口不可用。P0-1 修的是“未鉴权”，部署物没跟上，Web 从“不安全地可用”变成了“安全地不可用”。
- **CI 首次实跑的两个失败已修**（本轮分支，见 §7）：Go 加 `libopusfile-dev`；固件测试缺 fixture 时显式跳过；另把固件与边车测试拆成两个独立 job（run 44063914126 里固件失败掩盖了边车步骤，边车一次都没跑）。
- **核心判断与首轮一致**：测试全绿（见基线），生产接线仍断裂——对讲（FR-04）与 OTA（FR-11）在生产上仍是零实现；提醒/告警播报大概率无声；每台音箱每 60 秒必断线一次。

## 1. 审计范围与方法

### 1.1 范围

- **Hub（Go）**：`hub/` 全部代码 + `hub/config.yaml` + deploy 相关文件。
- **客户端（Rust）**：`client/src/` 全部 6 个模块（4507 行）+ 两个构建脚本 + `kws/vendor-arm/`。
- **kws-sidecar（Python）**：`kws-sidecar/` 全部代码（首轮之后全新写的，本轮第一次被审计）。
- **firmware-patch + deploy + CI**：`firmware-patch/`、`deploy/`、`.github/workflows/ci.yml`、`scripts/lint.sh`、`.golangci.yml`、`.semgrep/`。
- **首轮之后的新代码（重点）**：(1) P1-40 B：客户端切 gnueabihf；(2) 服务端唤醒：kws-sidecar + `hub/internal/kws/` + 客户端 `kws_mode="server"`；(3) CI 补缺：sidecar `tests/bootstrap.py` + ci.yml sidecar 步骤。

### 1.2 方法

四面并行审计，每面先跑基线测试、再逐文件读代码、grep 交叉验证生产调用点；文档与代码冲突时以代码为准；能实测的都实测（真边车 + 真模型探针、重复 `device_id` 探针、关键词表故障实验、20,000 帧洪泛 RSS 对照）；真机才能验证的项一律记 BLOCKED，不写成通过；找不到证据写“未确认”，不猜。

### 1.3 基线测试结果（协调员亲跑）

| 命令 | 结果 |
|---|---|
| `go test -count=1 ./...`（hub/） | **156 passed，0 failed**（22 个包全部 ok）；`go vet` 干净 |
| `cargo test`（client/） | **53 passed，0 failed** |
| `cargo test --features sherpa-kws`（client/） | **57 passed，0 failed**（含 2 项真实模型端到端测试因未设 `SHERPA_KWS_TEST_*` 而 SKIP，未真正执行） |
| `python3 -m pytest firmware-patch/tests/ -q` | **61 passed**（首次跑 1 failed + 6 errors，原因是本机 `/usr` 里 `unsquashfs` 丢失——VM 易失环境问题，非代码回归；重装 squashfs-tools 4.6.1 后重跑 61 通过） |
| `kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q` | **16 passed** |
| `./scripts/lint.sh` | **全部通过** |
| `go test -race -count=1 ./...`（hub/，附加） | **FAIL**：`internal/ha` 与 `internal/kws` 两个包失败，共 11 处 race 警告，全部栈顶在测试替身代码（fakeHA/fakeSidecar 并发 WriteJSON），生产代码栈未出现（见新 P2 Hub-N-P2-1） |

### 1.4 局限

- 无音箱硬件、无 NAS：全部真机项（刷机、NAS 部署、同步播放、真人唤醒率、双机仲裁、打断率、网易云扫码、HA 真联调、ALSA `hw:0,0`/`hw:0,2`）均未验证，记 BLOCKED。
- `go test -race` 的失败只在测试替身复现，生产竞态未检出——不等于生产无竞态。
- 构建不可字节复现（同一脚本、同一树状态重建产物 SHA/大小与上一版不同，原因未确认），见客户端 P2-N7。
- 客户端面报告内 P2 编号有一处口径差：摘要列 6 项、详述列 N1–N6，另有“构建不可字节复现”只在基线与未确认项里出现——本报告按 7 项 distinct 计入，并在该项注明。

## 2. 分级统计

| 分级 | 新问题 | 其中最严重 |
|---|---|---|
| P0 | 1 | 客户端 P0-N1：双开 ALSA，播放可能全灭 |
| P1 | 14 | FW-P1-1 Web Token 未接进部署；SC-P1-1 孤儿流出 wake；SC-P1-2 空词表 fail-open；N-P1-2 DialMPD 无 deadline 可挂死启动 |
| P2 | 40 | Hub -race 失败、边车 is_ready 无上限循环、flash-check 只警告不失败等 |
| 合计 | **55** | — |

首轮 96 项延期复查统计：**已修 1 / 部分修复 17 / 仍开 77 / 建议销项 1**（详见 §5 总表；另 P1-40 不在 96 项内，见 §6）。

## 3. P0 级新问题（1 项，置顶）

### P0-N1 录音任务与会话各持一个 `Audio`，两个实例各打开两路 ALSA `hw` 设备：第二路打开大概率 EBUSY，播放可能全灭

- **位置**：`client/src/main.rs:316`（录音任务 `Audio::open()`）、`:375`（每个会话 `Audio::open()`）；`client/src/audio.rs:420-424`（`open_devices` 总是同时打开采集与播放）；`:218-260`（两路各开各的，失败只记录、不整体失败）。
- **证据**：录音任务的 `Audio` 在进程生命周期内一直活着，同时打开了采集 `hw:0,0` 和播放 `hw:0,2`（播放这路它永远不用）；每个会话又 `Audio::open()` 一次，再次打开两路。`hw` 设备独占，第二次 `snd_pcm_open` 返回 EBUSY；`open_devices` 把失败只记进 `playback_err`，`Audio::open()` 仍返回 Ok。后果链：会话的 `playback` 为 `None` → 每次 `play_frame` 返回错误 → `run_session` 只 `warn!`，TTS/对讲/提醒音频**全部静默丢弃**，每帧一条 warn 刷屏。
- **为什么是 P0**：如果成立，首关口（刷一台 LX06 + Hub + 同步一首歌/播一句回答）中“播”这一半在真机上是全灭的，且失败是静默的（进程正常跑、正常连 Hub、日志只有 warn）。修复极便宜：只打开需要的方向（录音任务只开采集，会话只开播放），或两路共享一个 `Audio` 实例。
- **不确定性**：`hw:0,2` 是否只有一个子设备、第二次打开是否确实 EBUSY，**需真机确认，BLOCKED**。若有多个子设备，本项降级为 P1。代码结构上的双开是确定的，独占语义是 ALSA `hw` 设备的标准行为。
- **修复建议**：`Audio::open_devices` 拆成按需打开，或把 `Audio` 实例提到重连循环外共享；补测试：两个 `Audio::open()` 共存时第二个实例的 `play_frame` 必须成功。
- **出处**：`.audit-round2/client.md` §P0-N1。

## 4. P1 级新问题（14 项）

### 4.1 Hub 面（3 项）

#### N-P1-1 Snapcast `call()` 不校验响应 id，异步通知会被当成正常响应
- **位置**：`hub/internal/music/music.go:76-108`。
- **证据**：`call()` 发请求后只读一行，`json.Unmarshal` 后**从不比对 `resp.ID == s.seq`**，无 `Error` 即返回 `Result`。Snapcast 服务端会在连接/断开、音量变化时主动推送无 `id` 的 JSON-RPC 通知；通知进 `call()` 会被当成“ID 0、无错误、空结果”的正常响应返回（`GetGroups()` 随即得到空分组且 `err == nil`），真响应留在缓冲里被下一次 `call()` 读走，两次调用结果错配。另 `ReadBytes('\n')` 无长度上限（与 P2-7 同类）。
- **影响**：点歌后 `applySnapcast()` 可能静默跳过延迟校准/切流，或把上一次调用的响应错当成这一次的；多房间同步延迟校准（目标 ≤10ms）在通知交错时静默失效，且无任何日志。
- **修复建议**：`call()` 循环读行直到读到 `resp.ID == s.seq` 的响应（通知行丢弃并可记 debug 日志）；行读改限长；ID 不匹配即报错不断开连接。

#### N-P1-2 `DialMPD` 读欢迎行无 deadline，MPD 只建连不发问候时 Hub 启动永久挂起
- **位置**：`hub/internal/music/music.go:433-448`；调用点 `hub/cmd/hub/main.go:401-409`（启动时同步调）、`:437-453`（后台重试 goroutine）。
- **证据**：`timeout` 参数只传给 `net.DialTimeout`（TCP 建连），随后 `ReadString('\n')` 读 `"OK MPD ..."` 欢迎行**之前未设任何 `SetReadDeadline`**。MPD 进程存活但卡住/未发问候时 `ReadString` 永久阻塞。`main.go:401` 是启动流程里的同步调用——Hub 停在启动中途，`:4399`、Web、KWS 全未起，且无任何日志；后台重试 goroutine 同样会被一次挂起的 `DialMPD` 永久卡住，不再重试。
- **影响**：一次 MPD 异常即 Hub 无法启动（无超时、无日志、无降级）；与首轮 P1-5（HA `Dial` 首次读无超时）同一 bug 类，但 MPD 这一处首轮未点名。
- **修复建议**：读欢迎行前 `SetReadDeadline(now+timeout)`，超时即关连接并返回错误；启动时对 `DialMPD`/`DialSnap` 包一层整体超时或放 goroutine 里做。

#### N-P1-3 `speakText` 多路并发时同设备 Opus 帧交错成杂音，且无播放态同步
- **位置**：`hub/cmd/hub/main.go:477-515`；并发调用方：提醒 `onFire`（`:465-476`）、HA 告警 `watcher` 回调（`:681-690`）、语音降级回复（`OnAudioFrame`/`OnAudioEnd`）。
- **证据**：`speakText` 每次新建 `opus.Encoder`、循环 `c.SendBinary(f)`，全程**无任何 per-device 互斥**。`Conn.SendBinary` 只保证单帧写不交错，但两路 `speakText` 会逐帧交替发往同一设备，客户端按到达顺序播放即为两段 TTS 绞在一起的杂音。另 `speakText` 从不发 `start_play`/置 `playing`（属 P1-16），且中途 `enc.Encode` 出错即静默 `return`（前半段已发出的帧照常播放，尾音丢失无日志）。
- **影响**：提醒/告警并发时用户听到混杂播报；结合 P1-16，`speakText` 的帧在客户端 `playing==false` 时还会被整段静默丢弃——提醒可能既无声、又在极少数时序下变成杂音。
- **修复建议**：每设备一把播报锁（按 deviceID 串行化，或经单 worker 队列投递）；统一走与 `onReply` 相同的播放协议（先 `start_play`、播完 `stop_play`、按音频时长维护 `playing`）；`Encode` 中途出错记 error 日志。

### 4.2 客户端面（3 项）

#### P1-N1 `set_volume` 只改内存数字、从不碰 ALSA 混音器：Hub 以为调了音量，实际响度永远不变，客户端还回 `ok:true`
- **位置**：`client/src/main.rs:181-229`；全仓 grep `Mixer|mixer` 在 `client/src/` **零命中**；音量唯一消费点是 30 秒 `send_status` 上报（`main.rs:503`）。
- **证据**：`set_volume` 分支只写 `self.volume`、记一条 info 日志、返回 `(true, None)`。没有任何代码调用 ALSA 混音器，也没有任何代码把 `self.volume` 传给播放路径。`Audio::play` 不收音量参数。
- **影响**：Hub 发 `set_volume` 后收到 `ok:true`，Web/状态页显示的音量也变了，但音箱实际响度纹丝不动。用户说“小声点”会得到一句“好的”加毫无变化的音量——功能性撒谎，比直接报错更难排查。
- **修复建议**：二选一：(a) 真正接 ALSA 混音器（失败回 `ok:false`）；或 (b) 明确不支持：回 `ok:false, error:"volume control not implemented"`，并把该 RPC 从协议白名单文档里摘掉。不要保持“回 ok 但什么都不做”。

#### P1-N2 `start_recording` / `stop_recording` RPC 是空操作却回 `ok:true`：local 模式下对讲发送端是断的
- **位置**：`client/src/main.rs:193-200`（两个分支都只有一条 `info!` 日志）。
- **证据**：两个分支都不改任何状态、不发任何动作，返回 `(true, None)`。而录音任务向 Hub 推音频帧的唯一门是 `session.streaming`（`main.rs:483-495`），`streaming` 只在 `WakeWin` / `Interrupt` / server 模式建立时改变。于是 local 模式下 Hub 调 `start_recording` 想让发起方开始发对讲音频时，客户端照常不推流——对讲发起方发不出声音。server 模式下 `streaming` 恒为 true，本项被掩盖。
- **影响**：P1-15 的触发接通后，local 模式对讲仍是单向的（只能收不能发），且 Hub 收到 `ok:true` 以为录音已开始。首轮只写了“Hub 无触发入口”，客户端这半边的空操作是新发现的另一半。
- **修复建议**：`start_recording` 应置一个独立的 `recording` 标志（与 `streaming` 分开，或直接置 `streaming = true` 并在 `stop_recording` 时恢复），推流门改为 `streaming || recording`；`stop_recording` 清标志。补测试：收到 `start_recording` 后无 `wake_win` 也能向 Hub 推帧。

#### P1-N3 P1-40 的剩余路径文档是错的：`libonnxruntime.so` 并不“只要求 GLIBC_2.4”，它要求 GLIBC_2.34——方案 B 的“复用官方 ORT”前提已证伪
- **位置**：`client/kws/vendor-arm/SOURCE.md`（“`libonnxruntime.so` 本身只要求 GLIBC_2.4，没问题；卡的是 sherpa 那层”；出路 (b) 写“复用本目录官方 `libonnxruntime.so`”）；`client/src/sherpa.rs` 头注释（仍写 musl 客户端不能 dlopen、走 sidecar 过渡）。
- **证据**（本次实测 `readelf -V`）：`libsherpa-onnx-c-api.so`：GLIBC 最高 **2.34**、GLIBCXX 最高 **3.4.29**；`libonnxruntime.so`：GLIBC 最高 **2.34**（含 2.27/2.28/2.29/2.32/2.33/2.34）、GLIBCXX 最高 **3.4.29**。音箱上限：glibc 2.25、libstdc++ 3.4.22。两个 `.so` 都超标，不只是 sherpa 那层。首轮 AUDIT_REPORT.md 的 P1-40 一节已写明“‘ORT 只要求 GLIBC_2.4’是错的，已实测纠正”，但 `SOURCE.md` 与 `sherpa.rs` 头注释没有同步更新。
- **影响**：按 SOURCE.md 的出路 (b) 去做，会做到一半才发现 ORT 也要从源码重编——工作量评估错误（只重编 sherpa 层 vs 两层全重编）。决策者读的是文档。
- **修复建议**：更新 `SOURCE.md`：ORT 同样要求 GLIBC_2.34 / GLIBCXX_3.4.29，方案 B 的正确表述是“ORT + sherpa 都需从源码重编（或 KWS 继续放 NAS 边车）”；同步修正 `sherpa.rs` 头注释里 musl 与 sidecar 的过时现状。

### 4.3 kws-sidecar 面（4 项，均有实证）

#### SC-P1-1 重复 `device_id` 时旧连接不关闭，孤儿流继续出 wake
- **位置**：`kws-sidecar/server.py:175-177`（重连分支）、`:217-218`（finally 守卫）；`README.md`“并发与状态”一节。
- **证据**：重连时只执行 `self.devices[device_id] = stream`（新流覆盖字典项）并记 warning，**没有任何 `await old_ws.close()`**；旧 handler 的局部变量 `stream` 仍指向旧流，继续喂音频、继续发 wake。探针实证（合成 pos1 样本）：连接 1 喂前半句 → 连接 2 同 `device_id` 连上并收到 `ready` → 连接 1 继续喂后半句 + 50 帧静音 → **连接 1 收到 wake（rms 0.136），连接 2 收到 0 条消息**。服务端日志只有 `device dup-dev reconnected while old stream alive; dropping old stream`，但旧流实际未停。
- **影响**：Hub 断线重连窗口内同一设备两条连接同时向 Hub 发 wake；Hub 仲裁无去重/无冷却（首轮 P1-13 未修），可能开两个仲裁窗口、各赢一次，或同一声唤醒触发两次会话；孤儿流持续占用 CPU；日志措辞误导排障。
- **修复建议**：`Server.devices` 改存 `(stream, ws)`；新连接建立时先 `await 旧 ws.close(code=1000, reason="replaced by new connection")` 再替换；或反过来拒绝新连接直到旧连接关闭。二选一，并补测试：旧连接在新连接建立后喂完整关键词应**收不到** wake（且连接已关闭），新连接喂完整关键词恰好一次 wake。

#### SC-P1-2 空 `keywords.txt` 时边车带病上线：`/health` 照常 200 ok，但永远检不出唤醒
- **位置**：`kws-sidecar/server.py:58-83`（`load_spotter` 只校验关键词文件**存在**，不校验内容）；`:120,128`（`model_loaded` 只反映 spotter 非空）。
- **证据**：实测三组（同一模型目录，10402 端口）：空文件（0 字节）时进程**不退出**，`GET /health` → 200 `{"status":"ok","model_loaded":true}`，关键词表零行，`get_result` 恒为空——边车永不唤醒，但 Hub 健康检查全绿。错误 token 行（`n i h ao`）时 sherpa 抛错，进程 exit 255，fail-loud（好）。正确 token 行正常启动。
- **影响**：部署/运维手误（`download-model.sh` 写关键词失败、卷挂载空文件、误清空）会让 FR-01 在一切检查都通过的情况下静默死亡；排障时 `/health` 反而证明“边车正常”，误导方向。
- **修复建议**：`load_spotter` 加载后校验关键词文件至少含一行非空、含 `@` 的有效行；空表/全注释时 `sys.exit(1)` 并打印 FATAL；`/health` 增加 `keywords` 计数，让健康检查能区分“模型在、无词表”。

#### SC-P1-3 边车在局域网无鉴权、无连接数上限
- **位置**：`kws-sidecar/server.py:267`（`serve()` 未配任何鉴权）；`deploy/docker-compose.yml` kws-sidecar 段（`network_mode: host`、`KWS_HOST=0.0.0.0`）；`Dockerfile`（`EXPOSE 10400`）。
- **证据**：代码层面确认——`handler` 对 hello 只校验格式，不校验任何 Token/证书；`Server.devices` 无数量上限；`serve` 未设连接数限制。探针 7 次匿名连接全部只被协议格式拦下，无一被身份拦下。部署形态上 10400 不只监听回环，而是 host 网络下的 `0.0.0.0`。
- **影响**：局域网内任何主机可匿名建连、开任意多个 `device_id` 流、持续喂音频；解码在单事件循环内串行执行，流一多即打满 CPU，真音箱的唤醒延迟随之恶化直至不可用。不能直接向 Hub 注入 wake（wake 只回发到攻击者自己的 socket，Hub 只认自己建的那条连接），所以定 P1 而非 P0；但这与 Hub 4399 的 mTLS + 强 Token 形成明显反差——唤醒面是整条语音链里唯一零鉴权的入口。
- **修复建议**：按威胁从低到高任选：(a) 最低成本——监听改 `127.0.0.1`（边车与 Hub 同 NAS、同 host 网络，无需 `0.0.0.0`；健康检查本来就走 127.0.0.1）；(b) 共享密钥 hello（与 Hub 的 Token 复用同一机密，hello 增加 `token` 字段校验，失败即 1008 关闭）；(c) 连接数上限。建议至少做 (a)，(b) 作为纵深。

#### SC-P1-4 `/health` 的 503 分支不可达，健康语义只剩“进程活着”
- **位置**：`kws-sidecar/server.py:118-143`（`process_request`）、`:120`、`:263`（加载失败 `sys.exit(1)` 在 `Server` 构造之前）。
- **证据**：`model_loaded` 在 `Server.__init__` 里被置为 `spotter is not None`；而 `amain` 里模型加载失败时直接 `sys.exit(1)`，根本到不了 `Server(...)`。因此运行中的进程 `model_loaded` 恒为 True，503 分支是死代码。README 与模块 docstring 写的“模型未加载好时 503”在现有启动流程下**永远不会发生**。结合 SC-P1-2：空关键词表时 /health 同样是 200。
- **影响**：Hub 与 NAS 部署脚本把 `/health` 当作“唤醒可用”的判据，实际它只能证明“进程在监听”，模型/词表/推理能力一概不证明；故障模式（空词表、推理持续异常）全部漏过健康检查。
- **修复建议**：二选一：(a) 启动时先起 HTTP 再异步加载模型，加载中返回 503（改动大）；或 (b) 收缩语义——文档改写为“/health 只证明进程存活”，并在 /health 增加 `keywords` 数量与一次启动自检结果，同时由 Hub 侧定期做轻量推理探针。至少要修文档，消除错误承诺。

### 4.4 firmware-patch / deploy / CI 面（4 项）

#### FW-P1-1 P0-1 修复后，`OPEN_XIAOAI_WEB_TOKEN` 从未接进部署：NAS 上的 Web 面板注定 401，OTA 下发入口随之不可用
- **位置**：`deploy/docker-compose.yml`（hub 服务 `environment`）、`deploy/.env.example`、`deploy/nas-install.sh`；服务端 `hub/internal/web/web.go:57-59,82-83`。
- **证据**（审计员 grep 实测，协调员复核确认）：全仓库 `grep -rn "WEB_TOKEN" deploy/ hub/config.yaml hub/cmd/hub/main.go` **零命中**；`deploy/.env.example` 只列 `NAS_IP / MUSIC_FOLDER / HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN`，无 `OPEN_XIAOAI_WEB_TOKEN`；`docker-compose.yml` 的 hub `environment` 只传 `HASS_TOKEN / LLM_API_KEY / OPEN_XIAOAI_TOKEN` 三项，未传 Web Token。服务端行为：`web.go:82-83` 从环境变量读 token，为空时 `/api/*` 一律 401（fail closed）。
- **影响**：NAS 部署的 Hub 永远以空 Web Token 启动 → `/api/status`、`/api/devices`、`/api/config`、`/api/ota` 全部 401 → Web 面板不可用，FR-11 的一键 OTA 下发（走 `POST /api/ota`）在 NAS 上无入口。P0-1 修的是“未鉴权”，修完后部署物没跟上，Web 功能从“不安全地可用”变成了“安全地不可用”。更糟的是检查本身也被拖下水：`nas-install.sh` §5 用无 token 的 `curl /api/status` 判“Hub Web 面板正常”——curl 对 401 也 exit 0，检查恒为 PASS，是虚假检查（见 FW-P1-2）；`NAS_CHECKLIST.md` §3 预期 8080 返回含 `"wake_word":"你好小李"` 的 JSON，§6.1、§7.2、§7.3 的无 token `curl /api/devices` 在新版 Hub 上实际返回 401 JSON，清单与代码已脱节。
- **修复建议**：四处同步补：(a) `.env.example` 加 `OPEN_XIAOAI_WEB_TOKEN`（强随机 ≥16 位，与 `OPEN_XIAOAI_TOKEN` 独立）；(b) compose 的 hub `environment` 加 `- OPEN_XIAOAI_WEB_TOKEN=${OPEN_XIAOAI_WEB_TOKEN}`；(c) `nas-install.sh` §0 预检查加 Web Token 的存在与强度校验（与 `OPEN_XIAOAI_TOKEN` 同规则），§5 的 8080 检查改为带 `Authorization: Bearer $OPEN_XIAOAI_WEB_TOKEN` 并断言 HTTP 200；(d) `NAS_CHECKLIST.md` §3/§6.1/§7.2/§7.3 的 curl 命令补 token 用法。**本轮未修，只报告**（超出批准的“两处 CI 修复”范围）。

#### FW-P1-2 `nas-install.sh` 的多处检查是虚假 PASS（401 也算通过；端口检查漏 4399；`sed` 改 HA 地址无校验）
- **位置**：`deploy/nas-install.sh` §1（端口循环）、§3（`sed` 改 config）、§5（逐项检查）。
- **证据**：§5 的 `curl -s -m 5 http://127.0.0.1:8080/api/status` 与 `curl -s -m 5 http://127.0.0.1:3000/` 均只看 curl 退出码，不看 HTTP 状态码——401 / 404 / 500 照样 exit 0、照样 `ok`。与 FW-P1-1 叠加：8080 检查在新版 Hub 上必然 401 却必然 PASS。§5 的 MA 检查用了 `-w '%{http_code}'` 并 `grep -q 200`，是正确的写法——说明 8080/3000 两处是遗漏，不是全脚本的风格问题。§1 的端口占用循环是 `for p in 10300 10200 10400 1883 8095 3000 8080`——**漏了 4399**（Hub 主端口，P2-24 首轮已指出“未查 4399 端口”，仍未修）。4399 被占时 Hub 启动即绑定失败、容器 crash-loop，脚本却在 §1 报全部空闲。§3 的 `sed -i 's#ws://192.168.1.10:8123#ws://$NAS_IP:8123#'` 无任何事后校验：若 NAS 上的 config.yaml 里 HA 地址已不是 `192.168.1.10`，sed 静默无操作，随后的 `grep` 只打印、不比对，脚本照常 `ok`。
- **影响**：部署脚本的核心价值是“FAIL 即停、PASS 即真”；虚假 PASS 会让操作员在 Web 401、4399 被占、HA 地址未改的情况下一路走到 §6 网易云扫码，再回头排查重得多。
- **修复建议**：8080/3000 检查改用 `-w '%{http_code}'` 断言 200（8080 须带 Web Token，见 FW-P1-1）；§1 与 §5 的端口表补 4399（占用检查）与 10400/8080（连通检查），与 checklist §3 对齐；§3 的 sed 后加 `grep -q "ws://$NAS_IP:8123"` 断言，失败即 FAIL。**本轮未修，只报告。**

#### FW-P1-3 CI 的 Go job 与 lint.sh 的 `go vet` 环境差是结构性的，不只缺一个包
- **位置**：`.github/workflows/ci.yml:28-31`（Go job 安装行）、`scripts/lint.sh`（`go vet` 步骤）、`hub/`（`go.mod` 依赖 `gopkg.in/hraban/opus.v2`）。
- **证据**：CI 只装 `libopus-dev`；本机 `env.sh` 设 `PKG_CONFIG_PATH` 指向源码自编的 libopus 1.5.2 + libopusfile 0.12。本次是 `opusfile.pc` 缺失（已随本轮 CI 修复加 `libopusfile-dev` 止血）；即使补上，CI 与本机仍有版本差（apt 的 libopus 1.4.x/1.5.x vs 自编 1.5.2），`go test ./internal/opus` 的往返误差断言（相对 RMS 误差 <10%）理论上可能在 CI 抖动。
- **影响**：每次新增 cgo 系统依赖都会先在本机通过、再在 CI 失败，修一次、漏一次。
- **修复建议**：后续把 CI 的 Go 系统依赖钉成与本机一致的版本（或在 ci.yml 注释里写明“本机用 `~/workspace/tools/libopus` 自编，CI 用 apt，新增 cgo 依赖须两边同步”）。

#### FW-P1-4 Python job 把固件测试与 kws-sidecar 测试串在一个 job 里，前者失败会掩盖后者
- **位置**：`.github/workflows/ci.yml` 原 python job（两个 `pytest` 步骤顺序执行）。
- **证据**：run 44063914126 日志全文无 `kws-sidecar` 字样；kws-sidecar 测试本身是自给自足的（`tests/bootstrap.py`，与固件测试无任何依赖关系），却被固件失败连带跳过。CI 红了之后无法从本次运行判断边车是否也坏了；修固件的人与修边车的人互相阻塞在同一个 job 结论里。
- **状态**：**已随本轮 CI 修复处理**——python job 已拆成 `python-firmware` 与 `python-sidecar` 两个独立 job（见 §7），各自结论互不掩盖。

## 5. P2 级新问题（40 项）

### 5.1 Hub 面（12 项）

#### N-P2-1 `-race` 下 2 个测试失败：测试替身并发写同一条 WS 连接
- **位置**：`hub/internal/ha/ha_test.go:49,66,94,107,180`；`hub/internal/kws/kws_test.go:49,83,285`。
- **证据**：`go test -race -count=1 ./...`：`FAIL internal/ha`（`TestSubscribeEventsReceivesPush`）、`FAIL internal/kws`（`TestErrorMessageDoesNotBreakStream`），共 11 处 race 警告，栈全部指向测试文件里的 `conn.WriteJSON` 并发调用；gorilla websocket 文档要求同一连接最多一个并发写者。生产代码栈未出现，`cmd/hub`、`transport`、`voice` 等 20 个包在 `-race` 下通过。
- **影响**：测试本身不可靠（`-race` 常红会掩盖真正的生产竞态）；且它正是首轮 P1-21（HA 客户端无写锁）同一 bug 类的活演示——生产 `ha.Client.call()` 同样无写锁。
- **修复建议**：给两个 fake server 的连接加写互斥（或把事件推送串行化到 handler goroutine）；CI 加 `go test -race ./...` 门禁。

#### N-P2-2 `kws.Client.Close()` 与 `SendPCM`/`streamFor` 竞态：Close 后仍可泄漏流 goroutine
- **位置**：`hub/internal/kws/kws.go:380-393`（`Close`）、`:353-371`（`streamFor`）、`:324-351`（`SendPCM`）。
- **证据**：`Close()` 在 `closeOnce` 内关 `done`、置 `closed=true`、收集现有流、`wg.Wait()`。`SendPCM` 先 `isClosed()` 检查（持 `c.mu` 后释放），再调 `streamFor`（重新持锁、新建流、`wg.Add(1)`、起 goroutine）。两步之间 `Close()` 可插进来完成 `wg.Wait()` 并返回，随后 `streamFor` 仍 `wg.Add(1)` 并启动一个永不被等的流 goroutine（继续按退避拨号）。
- **影响**：Hub 退出路径上实际影响小（`main.go:708-710` 调 `Close()` 后进程即退出），但任何复用 `Close()` 后又 `SendPCM` 的调用都会泄漏拨号 goroutine；测试里 `Close` 后误调即复现。
- **修复建议**：`SendPCM`/`streamFor` 在同一把 `c.mu` 内同时做 `closed` 检查与建流，或 `Close()` 后 `SendPCM` 直接丢弃前先持锁复查。

#### N-P2-3 KWS 边车重连后，断线期间积压的约 1 秒陈旧 PCM 照常发给边车，可触发幽灵唤醒
- **位置**：`hub/internal/kws/kws.go:324-351`（`SendPCM` 丢最旧帧保新）、`:455-510`（`stream.run` 重连）、`:546-565`（`serve`/`writePump` 直接排空队列）。
- **证据**：队列是 per-stream（不在 per-connection），`serve()` 断线重连后 `writePump` 继续排空同一队列；断线期间 `SendPCM` 仍在入队（满 50 帧即丢最旧，队列恒保持最新约 1 秒音频）。重连成功瞬间，这 1 秒“断线前/断线中”的旧音频被当作实时音频发给边车；边车在其上检出关键词即回 `wake`，`OnKWSWake`（`main.go:169-179`）只按 `timestamp_ms` 算延迟记日志、**不拒绝过期音频上的唤醒**，随即开 20 秒会话。边车的 `timestamp_ms` 是检出时刻而非音频采集时刻，滞后在日志里看不出来。
- **影响**：用户早已停止说话 1 秒后仍可能误开一轮会话；与 P1-13（仲裁无去重/冷却）叠加后，幽灵唤醒可与真实唤醒各赢一次。
- **修复建议**：重连成功（`serve()` 的 `hello` 之后）先清空该流队列再开始发新帧；或给队列帧打采集时间戳，超过 500ms 的帧直接丢弃。

#### N-P2-4 KWS wake 事件的 `device_id` 轻信边车回传值，可把唤醒算到另一台设备头上
- **位置**：`hub/internal/kws/kws.go:610-627`（`handleMessage` 的 `wake` 分支，`firstNonEmpty(m.DeviceID, s.deviceID)`）；`hub/cmd/hub/main.go:169-179`。
- **证据**：边车 `wake` 消息里的 `device_id` 被优先采信，流自身的 `s.deviceID`（Hub 在 `hello` 里发过去的）只是回退。边车实现异常/被冒充返回别的 `device_id`，Hub 就会给从未唤醒的设备开会话、发 `wake_win`。
- **影响**：低（边车在可信本机）；但这是“标识应以连接为准、不应以对端自报为准”的同类错误，与 P1-44 的型号自报问题同根。
- **修复建议**：`wake` 事件一律用流的 `s.deviceID`，忽略消息里的 `device_id`（不一致时记 warn 日志）。

#### N-P2-5 `kws_status` 最长滞后 30 秒
- **位置**：`hub/internal/kws/kws.go:218-248`（健康检查 ticker，`HealthInterval` 默认 30s）、`:288-316`（`Status()` 的 `Down` 判定）；`hub/cmd/hub/main.go:564-589`。
- **证据**：`Down` 由两路决定：`healthChecked && !healthOK`，或 `streams>0 && connected==0`。边车挂掉且**尚无音频流**（`streams==0`）时，第二路恒为假，`Down` 只由 30 秒一次的健康检查翻转；边车恢复后，即使流已重连成功，`healthOK==false` 仍会让 `Down` 保持 true 直到下一个 tick。`/api/status` 直接透传该值。
- **影响**：`kws_down` 报警与恢复指示最长滞后 30 秒；运维在边车重启后 30 秒内看到的仍是旧状态。
- **修复建议**：流断开/重连成功时立即触发一次 `CheckHealth`；健康间隔可配，默认 30 秒改为 10 秒。

#### N-P2-6 `config.Validate` 漏 `hub.mpd`；`web_listen` 显式写 `:8080` 不拦截
- **位置**：`hub/internal/config/config.go:242-252`（只校验 `hub.stt`/`hub.tts`/`hub.snapcast`，`hub.mpd` 不在列表里）；`:152-160`（`validateListenAddr` 只校验可解析，不校验绑定面）；`hub/internal/web/web.go:312-336`（PUT 只拦 `tls` 降级，不拦 `web_listen` 改回全接口）。
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
- **修复建议**：MA 失败记一条 warn 日志（含 query 与错误摘要，截断上游响应体）。

#### N-P2-12 KWS 重连退避无 jitter，两台设备流可同步重连脉冲
- **位置**：`hub/internal/kws/kws.go:480-501`（`nextBackoff` 纯翻倍，500ms→30s，无随机化）。
- **证据**：两台设备的流在边车同一次重启后同时进入退避序列，退避值完全相同（同一起点、同一翻倍），重连脉冲同步化；与首轮 P1-43（客户端重连无 jitter）同一问题在 Hub 侧的翻版。
- **影响**：低（仅 2 台设备，脉冲幅度小）；边车重启瞬间双流同时拨号。
- **修复建议**：退避加 ±25%~50% jitter。

### 5.2 客户端面（7 项）

#### P2-N1 `interrupt` 只清 `playing` 标志、不停 ALSA：ALSA 缓冲里已写入的 80ms 尾音仍会放完
- **位置**：`client/src/main.rs:524-527`（`SessionAction::StopPlayback` 只 `info!`，不碰 `audio`）；`:144-152`（`HubMsg::Interrupt` 只置 `playing = false`）。
- **证据**：`StopPlayback` 分支没有任何停止播放的动作；`playing=false` 只是让后续收到的音频帧被丢弃。已写入 ALSA 的缓冲（4 周期 × 20ms = 80ms）会继续放完；且 `play_frame` 是同步写，正在写的一帧（20ms）不会被打断。
- **影响**：打断后仍有约 20–100ms 尾音。对“打断成功率 ≥90%”的体感影响小，但 `StopPlayback` 这个名字承诺的是“立即停播”，实际是“下一帧起不再喂”。
- **修复建议**：`StopPlayback` 执行时调 `pcm.drop()`（丢弃缓冲）+ `prepare()`，或给 `Audio` 加 `stop_playback()` 方法；补测试：interrupt 后 Fake 设备的 `played` 不再增长且缓冲被清空。

#### P2-N2 客户端对入站 WS 帧无大小限制：被攻陷/异常的 Hub 可用超大帧打满 256MB 音箱内存
- **位置**：`client/src/transport.rs:476-502`（`connect_once` 调 `connect_async_tls_with_config(url, None, false, …)` 与 `connect_async(url)`——配置参数传 `None`，即 tungstenite 默认值）。
- **证据**：tungstenite 默认 `max_message_size` 为 64MB、`max_frame_size` 为 16MB，客户端未改小。`next_raw`（`transport.rs:419-448`）对文本帧直接 `parse_hub_text`（整条 JSON 进内存）、对二进制帧直接 `opus.to_vec()` 全量复制。Hub 是受信端，正常情况下无害；但 Hub 一旦被攻陷或出 bug 发超大帧，256MB 音箱无任何防线（对比：首轮 P1-3 只给 Hub 侧提了 `SetReadLimit`，客户端这半边同样没有）。
- **影响**：单条 64MB 文本帧即可让客户端 OOM 被杀；且 `parse_hub_text` 的 `serde_json::from_str` 在超大输入上还会放大内存。
- **修复建议**：`connect_async_tls_with_config` 传入显式 `WebSocketConfig { max_message_size: Some(64 * 1024), max_frame_size: Some(64 * 1024), ..Default::default() }`（音频帧 <1.3KB、文本消息 <1KB，64KB 绰绰有余）；超限即断线重连并记 error 日志。

#### P2-N3 `tls=false` 配 `wss://` URL 时悄悄走系统根证书 TLS：指纹被绕过，与“tls=false = 纯 ws”自述矛盾
- **位置**：`client/src/transport.rs:460-475`（`connect_once` 只在 `tls=true` 时把 `ws://` 升级为 `wss://`；`tls=false` 时原样用 `cfg.hub_url` 调 `connect_async`）。
- **证据**：`tls=false` + `hub_url = "wss://…"` 时，URL 原样进 `connect_async`，而 tokio-tungstenite 按 scheme 决定是否 TLS——`wss://` 会握手 TLS，用的是系统根证书，**不走** `FingerprintVerifier`，也不走 `build_tls_config`。`Config::validate`（`config.rs:159-169`）只校验 `kws_mode`，不校验 `tls` 与 URL scheme 的一致性。
- **影响**：操作员以为关了 TLS（或以为指纹在保护），实际走了另一套 TLS 验证（自签 Hub 证书会握手失败，报错信息还指向系统根证书，误导排查）；反向组合（`tls=true` + `ws://`）有升级逻辑，唯独这向没处理。
- **修复建议**：`validate()` 里校验：`tls=false` 时 `hub_url` 必须以 `ws://` 开头，否则启动报错；或 `connect_once` 里 `tls=false` 且 URL 为 `wss://` 时明确报错而非静默走另一条路。

#### P2-N4 Opus 编码失败被静默吞掉：编码器一旦出问题，客户端无声无息地断流
- **位置**：`client/src/main.rs:339`（`if let Ok(opus) = audio.encode_pcm(&pcm)`，Err 分支无任何处理、无日志）。
- **证据**：捕获任务里编码失败时 `if let Ok` 不匹配即直接跳过该帧，不记日志、不计数。编码器是长期持有的有状态对象，一旦进入错误状态，每帧都失败、每帧都静默丢弃，Hub 侧只看到流断了、客户端日志里什么都没有。
- **影响**：可恢复的偶发错误与持续性编码器故障无法区分；排障时无任何线索。
- **修复建议**：Err 分支记一条节流 warn（1 秒一条 + 计数）；连续失败 N 次（如 50 次）后记 error 并重建 `OpusCodec`。

#### P2-N5 录音任务无背压丢弃策略：`audio_tx`（容量 32，约 640ms）满时反压会堵住采集，引发 ALSA overrun
- **位置**：`client/src/main.rs:312`（`mpsc::channel::<Vec<u8>>(32)`）；`:330-343`（捕获任务：`capture_pcm` → `encode_pcm` → `audio_tx.send(opus).await`，全在同一个任务里串行）。
- **证据**：`audio_tx.send().await` 在通道满时挂起等待，而 `capture_pcm` 是阻塞读（约 20ms 一帧），发送挂起期间不读 ALSA——ALSA 采集缓冲（80ms）很快 overrun，下一次读到 XRUN（capture XRUN 处理最多重试 2 次后返回错误，录音任务随即退出，见 P2-40）。即：一次 WS 抖动/Hub 慢消费会通过背压链把录音任务杀死。
- **影响**：Hub 端一次 GC/慢处理（>640ms）即可让音箱永久失聪（录音任务退出不重启），且日志里只看到 XRUN，看不到真正的背压原因。
- **修复建议**：发送改 `try_send`，满时丢最旧帧（或丢当前帧并计数），永不阻塞采集；或把编码与发送拆到不同任务，中间用有界丢弃队列。并在日志里记 `dropped_frames` 计数。

#### P2-N6 `hello_ok` / `rpc` 解析过于宽容：缺字段时静默接受空值
- **位置**：`client/src/transport.rs:347-380`（`parse_hub_text`：`hello_ok` 的 `device_id` 缺失时 `unwrap_or("")`；`rpc` 的 `id`/`method` 缺失时同样 `unwrap_or("")`）。
- **证据**：`connect_once`（`transport.rs:531-544`）接受 `device_id == ""` 的 `hello_ok`，`Transport.device_id = Some("")`，会话照常建立；之后 `send_rpc_result` 会带着 `id: ""` 回复，Hub 侧无法匹配。`HubMsg::Rpc` 的 `method == ""` 会走 `handle_rpc` 的 `other` 分支回 `unknown rpc method: `（空方法名），日志无有效信息。
- **影响**：协议错配/Hub bug 时客户端不报错、不拒绝，静默跑在一个半握手状态；排障时 `device_id=""` 的会话难以定位。
- **修复建议**：`hello_ok` 缺 `device_id` 或为空时 `connect_once` 直接报错断线；`rpc` 缺 `id`/`method` 时记 warn 并丢弃（不回 `rpc_result`，或回 `ok:false` 并注明缺字段）。

#### P2-N7 构建不可字节复现：同一脚本、同一树状态重建产物 SHA/大小与上一版不同
- **位置**：`client/build-armv7-gnueabihf.sh`；产物 `client/target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client`。
- **证据**：本轮重建产物 2,444,332 字节、SHA256 `e90c3058…61ff`；上一版 2,435,964 字节、SHA256 `2da754d4…7a00b`。源码树与 main 无 diff（`git log main..HEAD` 为空、`git diff main --stat -- client/` 为空），同一脚本重建却字节不同。
- **影响**：无法用 SHA 断言“产物就是某次提交编出来的”；`flash-check.sh` 之外的产物核对不可靠；排障时无法区分“代码变了”与“环境变了”。
- **修复建议**：查明差异源（时间戳、随机种子、rustc 版本、链接顺序），钉死可复现构建（`SOURCE_DATE_EPOCH`、固定工具链版本）；在原因未确认前不要用 SHA 作为唯一核对手段。
- **注**：客户端面报告摘要列 P2 6 项、详述列 N1–N6，另有本项只在基线与未确认项里出现——本报告按 distinct 7 项计入。

### 5.3 kws-sidecar 面（13 项）

#### SC-P2-1 `is_ready` 排空循环无上限（首轮 P2-21 的模式迁移到了边车）
- **位置**：`kws-sidecar/server.py:100-101`（`DeviceStream.feed` 内 `while self.spotter.is_ready(self.stream): decode_stream`）。
- **证据**：代码确认无迭代上限、无超时；`feed` 是同步调用，跑在单事件循环里。首轮 P2-21（客户端同款循环）当时标“疑似”；本轮在边车面同样**未观察到卡住实例**（探针 571 帧 + 20,000 帧均正常排空），但模式原样迁移且 blast radius 变大——边车循环卡住会堵住**全部设备**的 handler，而客户端只堵一台音箱。
- **影响**：若某路流的 `is_ready` 因模型/音频异常恒为真，整个边车事件循环永久阻塞，所有设备唤醒停摆，且无日志、无超时、无自愈。
- **修复建议**：循环加硬上限（如单帧最多 decode 32 次，超限记 error 并 `reset_stream` 该设备流）；或把 decode 移到线程池。补一个“构造持续 ready 的假 spotter”的单测验证上限生效。

#### SC-P2-2 wake 事件的 rms 是 1s 均值，会被 trailing 静音稀释（首轮 P1-14 的残留形态）
- **位置**：`kws-sidecar/server.py:91`（`rms_frames = deque(maxlen=50)`）、`:107-114`（`rms` 取 50 帧 RMS 的均方根）、`:94-105`（每帧都 append，含 trailing 静音帧）。
- **证据**：代码确认 rms 窗口是“最近 50 帧（含检出前喂入的 trailing 静音）”的均值，不是词内峰值。TEST_REPORT 场景 0 的实测区间下界就是 `0.000`（窗口喂满静音时 rms 归零）；探针中 pos1 在 10 帧 trailing 后检出、rms 0.136，而窗口里已有 200ms 静音。检出延迟越长，窗口里静音占比越高，rms 越小；两台设备检出延迟不同时，稀释程度不同，仲裁比较的不再是“谁喊得响”，而是“谁检出得早”。
- **影响**：仲裁公平性漂移；极端时两台都报接近 0 的 rms，胜负由浮点噪声决定。首轮 P1-14（单帧取值）已通过“1s 窗口”缓解，但未根治。
- **修复建议**：rms 改取窗口内峰值（或窗口内前 50% 分位），或在检出时回溯取关键词时间窗（检出时刻前约 0.8s）内的帧算 rms；至少在 wake 事件里同时报 `rms_peak` 供仲裁选用。补测试：词后喂 50 帧静音再检出，断言 rms 仍接近词内水平。

#### SC-P2-3 hello 10s 超时走通用 except：全量 traceback + 误导性 "internal error"
- **位置**：`kws-sidecar/server.py:145`（`wait_for(ws.recv(), timeout=10)`）、`:~248-256`（通用 `except Exception`）。
- **证据**：探针：建连后 10s 不发 hello，客户端收到 `{"type":"error","message":"internal error"}`，随后连接关闭；服务端日志打了一段完整 traceback。超时是**预期内**的协议行为，却被当作内部异常处理。
- **影响**：日志噪声（每次半开连接/端口扫描都刷一条 traceback，淹没真异常）；客户端收到的 "internal error" 误导排障。
- **修复建议**：单独捕获 `asyncio.TimeoutError`：记一条 INFO/WARNING，发 `{"type":"error","message":"hello timeout"}` 后以 1008 关闭，不打 traceback。

#### SC-P2-4 连接断开时不 flush，尾部关键词随流丢弃
- **位置**：`kws-sidecar/server.py:217-218`（finally 只删字典项，无排空）。
- **证据**：代码确认——断开即 `del self.devices[device_id]`，不给流补 trailing 静音、不做最后一次 `decode`/`get_result`。探针证实唤醒需要关键词后约 200ms（10 帧）trailing 音频才出结果；若设备在关键词尾音处恰好断线，这句唤醒必然丢失。
- **影响**：低概率丢唤醒；正常使用下麦克风持续送静音，trailing 总会到来，所以影响有限，定 P2。
- **修复建议**：finally 里对断开的流补喂约 10-25 帧静音并做一次 `get_result`（若检出则记日志；wake 事件已无连接可发，至少留一条 `wake lost on disconnect` 日志供排障）。或在文档里明确写清“关键词尾 + 断线 = 丢唤醒”的边界。

#### SC-P2-5 `device_id` 无长度/内容校验，原样进日志
- **位置**：`kws-sidecar/server.py:160-170` 附近（hello 校验只判“非空字符串”）。
- **证据**：代码确认——`device_id` 只要是字符串、非空即通过，无长度上限、无字符集限制；随后日志原样输出。Hub 侧的 `device_id` 来自可信配置，当前风险低。
- **影响**：超长 `device_id` 可进字典与日志；含 `\n` 的 `device_id` 可伪造日志行（与首轮 P2-3 同款问题在 Hub 侧的 room 名上已提过，边车侧是同类残留）。
- **修复建议**：校验长度（如 ≤64）与字符集（`[A-Za-z0-9_-]`），非法即 error + 1002 关闭；日志输出前做单行化（去 `\r`/`\n`）。

#### SC-P2-6 `KWS_THRESHOLD` / `KWS_SCORE` 无范围校验
- **位置**：`kws-sidecar/server.py:build_arg_parser`（`--threshold` / `--score` 只 `float(...)`，无范围判断）。
- **证据**：代码确认——`KWS_THRESHOLD=5.0` 或 `-1` 都会原样传给 `KeywordSpotter`，sherpa 是否 clamp 未验证；阈值配错时边车要么永不唤醒、要么狂误触发，且启动日志只打印数值、不告警。
- **影响**：配置手误静默改变唤醒行为，无启动期拦截。
- **修复建议**：启动时校验 `0 < threshold <= 1`（按 sherpa 语义定界）、`score >= 0`，越界即 FATAL 退出；或至少在越出实验验证值（0.25/1.0）时打一条 WARNING。

#### SC-P2-7 可观测性缺口：`wake_count` 是死指标，无指标端点、无延迟统计
- **位置**：`kws-sidecar/server.py:92`（`self.wake_count += 1`）、`:107-114`、`/health` 只返回两个字段。
- **证据**：代码确认——`wake_count` 自增后**没有任何读取方**（不进日志、不进 /health、无指标导出）；/health 只有 `status`/`model_loaded`；无每设备帧数、无检出延迟、无丢帧/错误计数。排障只能靠 grep 日志里的 `wake:` 行。
- **影响**：NAS 上长期运行后无法回答“昨天误触发几次、平均检出延迟多少、哪台设备流异常”。
- **修复建议**：/health 扩展为 `{"status","model_loaded","keywords","devices","total_wakes","uptime_s"}`；或至少在 `wake` 日志行里加上“自该设备连接以来的第 N 次”与“关键词结束到检出的帧数”。

#### SC-P2-8 bootstrap：Piper 语音文件只 pin 了版本 tag，未做 SHA 校验
- **位置**：`kws-sidecar/tests/bootstrap.py:62-67`（`VOICE_BASE` 指向 HF `rhasspy/piper-voices` `v1.0.0`）、`:_download`（`urllib.request.urlopen` 直写文件，无 hash 校验）。
- **证据**：代码确认——模型下载有 SHA256 pin，但两个语音文件下载后**不校验任何摘要**；`_download` 失败时删残留文件是好的，但内容损坏/被替换时要到 piper 合成失败才暴露，报错指向 piper 而非下载。
- **影响**：供应链完整性不对称；HF tag 被重新指向或传输损坏时，测试样本悄悄变质（最坏情况：语音损坏导致样本校验反复重抽 10 次才报错，浪费 CI 时间）。
- **修复建议**：给两个语音文件 pin SHA256（实测现有 `.kws-exp/tts/` 里的文件算一次），下载后校验，不符即删文件重报；或至少记录已下载文件的 SHA 到日志。

#### SC-P2-9 bootstrap：样本校验阈值写死，与服务端运行阈值可漂移
- **位置**：`kws-sidecar/tests/bootstrap.py:104-105`（`VERIFY_THRESHOLD = 0.25`、`VERIFY_SCORE = 1.0`）。
- **证据**：代码确认——校验用的阈值是写死的常量，只在注释里说“取边车默认值”；而服务端阈值由 `KWS_THRESHOLD`/`KWS_SCORE` 环境变量决定。若部署改了阈值，测试校验的仍是旧阈值：可能出现“样本校验通过、生产阈值下检不出”或反之。
- **影响**：测试与生产的判定标准悄悄分叉；目前默认值一致，所以是潜在项。
- **修复建议**：`_verify_samples` 从环境变量读阈值（默认 0.25/1.0），与 `server.py` 同源；或在 README 里写明“改阈值必须同步改 bootstrap 常量”。

#### SC-P2-10 bootstrap：默认资产根是共享实验目录，坏样本重抽会改写它
- **位置**：`kws-sidecar/tests/bootstrap.py:assets_root`（默认 `~/workspace/.kws-exp`）、`_verify_samples` + `_generate_missing`（不合格样本就地重抽覆盖）。
- **证据**：代码确认——`KWS_TEST_ASSETS` 未设时，`resolve_assets()` 直接操作 `~/workspace/.kws-exp/samples/`；`_verify_samples` 对校验不过的样本调用 `_synthesize_sample(..., path)` 原地覆盖。本地跑测试本应是只读验证，却可能改写实验目录的参考样本（TEST_REPORT 称 pos3 曾命中一次坏抽样并重抽——那次重抽就发生在共享目录里）。
- **影响**：测试副作用污染实验基线；多人/多任务共用 `.kws-exp` 时样本会被悄悄替换，复现性受损。
- **修复建议**：默认根目录不存在“写”需求时，先校验、仅当确实缺失才生成；重抽前把原文件备份（如 `pos3.wav.bak-<timestamp>`）或改写进 `KWS_TEST_ASSETS` 指向的隔离目录；文档注明“本地默认路径下测试可能改写 `.kws-exp/samples`”。

#### SC-P2-11 bootstrap：10 次上限实际是 10 次检测 / 9 次重抽，文档说“重抽最多 10 次”差一
- **位置**：`kws-sidecar/tests/bootstrap.py:93`（`MAX_GENERATION_ATTEMPTS = 10`）、`:345`、`:356-357`。
- **证据**：代码走读——`attempts` 从 1 开始计检测次数：首次检测失败（attempts=1）→ 重抽 → 第 2 次检测……第 10 次检测失败即 raise。实际重抽次数上限是 **9 次**，不是文档写的“重抽最多 10 次”。另：日志里的 `attempt {attempts+1}` 在第 1 次失败时打印 `attempt 2/10`，计数口径与“第几次重抽”不一致，易误读。
- **影响**：纯计数口径问题，不影响“10 次不合格就报错”的兜底语义；但文档与代码差一，排障时数重抽次数会对不上。
- **修复建议**：统一口径：把循环改成“最多 10 次重抽”（即 11 次检测）或把文档改为“最多 10 次检测（9 次重抽）”；日志改为 `re-roll {k}/10` 的重抽计数。

#### SC-P2-12 容器以 root 运行、无资源上限
- **位置**：`kws-sidecar/Dockerfile`（无 `USER` 指令，默认 root）；`deploy/docker-compose.yml` kws-sidecar 服务段（无 `mem_limit`/`cpus`，全文件 grep 也无任何 `mem_limit`）。
- **证据**：文件核对确认。边车是纯推理服务，不需要 root；单事件循环 + 无界设备流（见 SC-P1-3）时，一个失控进程可吃掉 NAS 全部 CPU/内存。
- **影响**：与首轮 P1-4 的容器加固要求同类（当时针对 hub/Wyoming 等服务）；边车面是其中一块未补的拼图。风险低于 Hub（边车不写配置、不调 HA），定 P2。
- **修复建议**：Dockerfile 加 `USER nobody`（或建专用 `kws` 用户；注意 `/data/kws/models` 卷权限）；compose 加 `mem_limit: 512m`、`cpus: "1.0"`（按 NAS i5 实测再调）。

#### SC-P2-13 `download-model.sh` 的 curl 无 `--fail`，HTTP 错误靠 SHA 兜底、报错误导
- **位置**：`kws-sidecar/scripts/download-model.sh`（`curl -sSL --retry 2 -o …`，无 `--fail`）。
- **证据**：脚本走读——无 `--fail` 时 HTTP 404/403 也会写出一个（错误页面）文件并 exit 0，随后 `sha256sum -c` 失败才退出。最终结果仍是 fail-loud（SHA 兜住了），但用户看到的报错是 “SHA256 校验失败”，会误以为是上游重新打包，而真实原因是下载根本没拿到包。
- **影响**：排障误导；CI 里下载失败时多花一次排查。
- **修复建议**：curl 加 `--fail`（`curl -sSLf`），并在 SHA 校验失败的报错里同时打印 HTTP 状态与文件大小。

### 5.4 firmware-patch / deploy / CI 面（8 项）

#### FW-P2-1 CI 与 lint.sh 互相缺项，本地全绿 ≠ CI 全绿
- **证据对照**（均实测读文件）：lint.sh 有 `gofmt -l hub scripts`，CI 的 Go job 没有 gofmt 检查——一个未格式化的 Go 改动可通过 CI，却在本地 lint 失败。lint.sh 不跑任何测试（自述“由 ci.yml 跑”），CI 不跑 `scripts/lint.sh` 本身——`check-wiring.go` 等规则 1/2/3 脚本在 CI 里是直接 `go run` 的，与 lint.sh 的调用一致，这部分没漂移；但 `golangci-lint` 在 CI 里由 `golangci-lint-action@v6`（version v1.64.8）跑，在 lint.sh 里由本机 `~/workspace/tools/bin/golangci-lint` 跑，版本未钉死一致。CI 的 Go job 里 `golangci-lint-action` 在 run 44063914126 留了一条警告：`Restore cache failed: Dependencies file is not found in /home/runner/work/open-xiaoai-next/open-xiaoai-next. Supported file pattern: go.sum`——`go.sum` 实际在 `hub/go.sum`，action 默认在仓库根找，缓存未命中（只影响速度，不影响结论）。CI 的 Rust job 跑 `cargo test --locked` + fmt + clippy；lint.sh 的 Rust 部分只跑 fmt + clippy（无 test）——两者互补但无单一入口能一次跑全。
- **影响**：低。两边各漏一角，迟早有人在其中一边翻车。
- **修复建议**：CI 增加 `gofmt -l` 步骤（或直接跑 `scripts/lint.sh go`）；给 `golangci-lint-action` 显式配缓存路径或接受该警告；长远可让 CI 直接跑 `scripts/lint.sh`（需先解决 lint.sh 对 `~/workspace/tools` 绝对路径的依赖）。

#### FW-P2-2 CI 的 Python job 只装 `pytest`，未声明固件测试的其它前置
- **位置**：`.github/workflows/ci.yml:63-67`（现 python-firmware job）。
- **证据**：固件测试实际还需要 `unblob`、`unsquashfs`、`mksquashfs`、`file` 等系统工具（`test_shell_scripts.py` 的 `unpacked_work` fixture 跑 `unpack.sh`，后者调 `unblob` 与 `unsquashfs`；`patch.sh` 调 `mksquashfs`）。CI 的 ubuntu-latest 镜像默认没有这些工具，且本轮跳过方案实施后，这些测试在 CI 里会被跳过而非执行——跳过是正确选择，但 ci.yml 里没有任何注释说明“固件端到端测试在 CI 里不跑、在哪跑”（应在有固件材料的本机跑）。另外 `python -m pip install --quiet pytest` 未钉版本，也未装 `pyyaml`（当前固件测试只用标准库，暂时不需要；若后续测试读 YAML 会静默失败）。
- **影响**：低。主要是可维护性：后来者会在 CI 里看到一片 skipped 而不知道去哪补跑。
- **修复建议**：在 ci.yml 的 pytest 步骤上方加注释，写明固件端到端测试的跳过原因与本地补跑命令（`python3 -m pytest firmware-patch/tests/ -q`，需固件材料与 squashfs-tools）；`pytest` 钉一个版本范围。

#### FW-P2-3 `TEST_REPORT.md` 的 P1-40 行（L166）仍留着已被证伪的“ORT 只要求 GLIBC_2.4”说法
- **位置**：`TEST_REPORT.md:166`（“审计修复”节 P1-40 行，方案 (b) 括号内）：“复用官方 `libonnxruntime.so`，它只要求 GLIBC_2.4”。
- **证据**：同一文件 L235-236（“P1-40 方案 B — .so 勘测与证伪”节，“前提纠正”）已用 `readelf -V` 证伪——同捆 `libonnxruntime.so` 的 GLIBC 最高需求是 **2.34**、GLIBCXX 最高 3.4.29。旧说法与纠正相隔 70 行，读前半部分的人会按错误前提做决策（例如以为方案 B 只差重编 sherpa C API）。
- **影响**：低（文档）。但它是“已被证伪却仍留着”的典型文档滞后。
- **修复建议**：在 L166 该句后加删除线/勘误注，或直接改为“（该前提已被证伪：官方 libonnxruntime.so 要求 GLIBC 2.34，见本文件‘前提纠正’节）”。**本轮未修，只报告。**

#### FW-P2-4 `TEST_REPORT.md` 多处旧数未随重编更新，与 runbook / checklist 脱节
- **位置与证据**：
  - L143（“第二轮复核人重跑结果”）：LX06 镜像 SHA 仍写 `cc36b28e…54f06d`、OH2P 镜像 `db00add0…f0423`、OH2P 镜像 30,175,232 字节、ARM 产物 2,852,620 字节、`deploy/hub-linux-amd64` 11,534,848 字节——均已被取代：新 SHA 为 LX06 `7266aba9…d97b3`（38,453,248 字节）、OH2P `139de363…6415`（30,179,328 字节）；ARM 默认交付已是 gnueabihf 2,435,964 字节；hub 二进制已是 11,747,288 字节、SHA `1e4f6f6c…c80a`（`NAS_CHECKLIST.md` 镜像核实表已更新，TEST_REPORT 未同步）。
  - L117（Phase 4 表“一键 OTA”行）：“状态机 Push→Verifying→OK/Failed、超时扫荡、Web 下发均验证 | 通过 | 真机刷机验证；回滚本身是设备端 boot1 行为”——与首轮审计 P1-9 的结论冲突：`ConfirmHello` / `MarkRebooted` / `Sweep` 在生产代码里零调用，OTA 任务永久停在 `pushing`，回滚闭环是零实现而非“只差真机验证”。该行把“包内单测通过”写成了接近“功能通过”。
  - Phase 4 表“对讲全双工 | 未做”与第二轮 B 行“对讲全双工已做（`go test` 88 通过）”自相矛盾——前者是旧表未更新，后者是新行；以 B 行为准，但读者无法自行判断。
  - 计数滞后：多处仍写 `go test 88 通过 / cargo test 37 / pytest 36`，而“审计修复”节协调员重跑已是 `go test 131 / cargo test 46 / pytest 61`。旧数本身是 dated 记录可以理解，但 P1-40 行与 L143 的 SHA 是会被直接拿去刷机的数，必须改。
- **影响**：低到中（文档）。SHA 旧值若被误用，`flash-check.sh` 只打 WARN 不拦截（见 FW-P2-5），操作员可能按旧 SHA 去核对新镜像而误判。
- **修复建议**：L143 与 L166 的旧 SHA/旧说法改为新值并标注取代关系；L117 的 OTA 行改为“包内状态机单测通过；生产接线零调用（P1-9），回滚闭环未实现”；“对讲全双工”旧行标注已被第二轮 B 行取代。**本轮未修，只报告。**

#### FW-P2-5 `flash-check.sh` 的补丁镜像 SHA 不一致只警告、不失败，且不校验镜像内的补丁内容
- **位置**：`firmware-patch/scripts/flash-check.sh`（`KNOWN_PATCHED_SHA256` 比对分支）、`firmware-patch/scripts/patch.sh` §6（打包后验证）。
- **证据**：flash-check.sh：`PSHA` 与 `KNOWN_PATCHED_SHA256` 不一致时只 `warn`，`FAIL` 保持 0，脚本 exit 0。设计意图是“镜像重编后值会变”，runbook §1 也写“以重编后 flash-check.sh 打印的值为准”。但这意味着：一次静默重编（例如另一位修复工人改了 `patch_firmware.py` 后重编，或产物被替换）只产生一行 WARN，操作员在 §0.2 的“SHA 必须与表格一致”与脚本的“不一致只警告”之间得不到强制门。flash-check.sh §4 只验补丁镜像的存在、大小 <40MB、魔数 `hsqs`、`unsquashfs -s` 可解析——**不验镜像里的补丁是否都在**（不解开看 shadow/PAM/dropbear/asound）。`patch.sh` §6 的解开验证也只查 11 个文件存在 + 3 处 grep，不查 shadow 的 `$6$`、PAM 的 pam_unix、OTA 禁用、`/data/init.sh` 启动行、证书文件、文件权限（见首轮 P2-6，仍未修）。
- **影响**：低到中。正常流程下 `patch.sh` §6 已挡掉大部分坏镜像；flash-check 的 WARN-only 是最后一道人为核对，不是自动门。风险场景是“重编后未更新 runbook 表格 + 操作员没看 WARN 就刷”。
- **修复建议**（二选一）：(a) flash-check 加 `--strict` 模式（SHA 不一致即 FAIL，刷机时默认用 strict，重编后由重编人同步更新 runbook 表格与脚本内白名单再刷）；或 (b) 保持 WARN，但在 runbook §8 清单里把“补丁镜像 SHA 与 §1 表格一致，或已记明重编来源”列为必勾项并要求附 flash-check 输出。另把 `patch.sh` §6 的验证扩展到 shadow/PAM/OTA 禁用（P2-6 的原建议）。**本轮未修，只报告。**

#### FW-P2-6 `patch.sh` 头注释仍写“刷 boot0，保留 boot1 回滚”，与 runbook 的“以实测 boot_part 为准”冲突
- **位置**：`firmware-patch/scripts/patch.sh:8`（注释）：“A/B 分区：system0 / system1；刷 boot0（补丁系统），boot1 保留原厂系统用于回滚”；对照 `FLASH_RUNBOOK.md` §0.3 与 §3 决策表（boot_part 实测为 boot0 时目标是 boot1/system1；未实测时不刷，例外才按默认写 system0/boot0 并记明）。
- **影响**：低（文档）。`patch.sh` 本身不写分区（只产镜像），注释不会直接导致误刷；但它是操作员会读到的第二份“槽位说法”，与 runbook 的决策表并列时会造成混淆。
- **修复建议**：把该行注释改为“目标槽以 FLASH_RUNBOOK.md §3 决策表为准（按实测 boot_part 定），不要默认 boot0”。**本轮未修，只报告。**

#### FW-P2-7 `nas-install.sh` 与 `NAS_CHECKLIST.md` 的端口表、容器数、检查口径三处不一致
- **证据**（均实测读文件）：容器数：脚本 §5 写“9 个容器都应为 Up”，checklist §2.2 也写 9 个——一致，但脚本 §5 只 `docker compose ps` 打印、不数数，`ok` 是无条件打印的（`$SSH "… ps …" && ok "见上：9 个容器都应为 Up…"`——ps 成功即 ok，不校验行数/状态）。端口表：脚本 §1 占用检查（10300/10200/10400/1883/8095/3000/8080）与 §5 连通检查（10300/10200/1883/8095/3000）与 checklist §3（10300/10200/10400/1883/8095/3000/8080/1704/1705/4399）三张表各不相同（见 FW-P1-2）。网易云检查：首轮 P1-34 称“网易云检查恒为 PASS”——本轮复核：脚本 §5 的 netease 检查是 `$SSH "curl -s -m 5 http://127.0.0.1:3000/ | head -c 200; echo" && ok "NeteaseCloudMusicApi 有响应（见上）"`——curl 只要能连上（哪怕返回 404/500）即 ok，且 `head -c 200` 的输出不校验内容；与 8080 检查同类问题，仍是弱检查（首轮 P1-34 未修）。
- **影响**：低。主要是检查口径不统一，逐项对着清单手工核对时才发现。
- **修复建议**：脚本 §5 的 ps 检查改为数 Up 的容器数（`docker compose ps --format '{{.Status}}' | grep -c '^Up'` 应为 9）；三张端口表统一为 checklist §3 的完整表；netease 检查断言 HTTP 200 或响应含预期字段。**本轮未修，只报告。**

#### FW-P2-8 lint 规则覆盖面缺口（5 处，均实测）
- **位置**：`scripts/lint.sh`、`.golangci.yml`、`.semgrep/rules.yml`、`.github/workflows/ci.yml`。
- **证据**：
  1. **lint.sh 不覆盖 Python 测试与 shell 脚本 lint**：注释自述“测试（go test / cargo test / pytest）不在此脚本里”，且无 `shellcheck` / `bash -n` 步骤——shell 脚本的语法只靠 `test_shell_scripts.py::test_shell_scripts_syntax`（pytest 内 `bash -n`）间接覆盖，CI 里 pytest 失败时连语法检查也被跳过。
  2. **Semgrep 规则 3 的 Rust 分支自认缺口**：`.semgrep/rules.yml` 注释写明 `vec![x; n]` 未覆盖（“实测 semgrep 1.185.0 的 Rust 宏模式无法可靠表达”，“仓库现状无变量长度的 `vec![x; n]`，先不覆盖”）——现状无违规，但规则本身有已知洞。
  3. **规则 1 的 allowlist 是“打印 DEFERRED 即通过”**：`check-wiring.go` 对 P1-9 / P1-15 的 5 项接线缺失打印 `DEFERRED … deferred until after first gate` 后返回通过（本轮 lint 输出实测）。机制本身是 bai 批复的暂缓安排，没错；但风险是 allowlist 没有过期提醒——第一关口后若忘记摘掉，lint 会继续全绿。
  4. **规则 3 Go 部分有 1 条 KNOWN-EXCEPTION 且无过期条件**：`SnapClient.call()` 的 `ReadBytes` 无长度上限（lint 输出实测）。与第 3 条同类风险：例外永久化。
  5. **`.golangci.yml` 未开 `gosec` / `noctx` 等**：当前启用 `govet / staticcheck / gofmt / goimports / ineffassign / unused / errcheck`。`errcheck` 已在本轮补过 3 处 `_ =`，但 `gosec`（弱加密/硬编码凭据等）未启用——P0-2 类问题（硬编码默认密码）若重现，golangci 不会报警；不过 semgrep 规则 4（Token/密码禁默认值）覆盖了同类形状，属于分工而非完全缺失。
- **影响**：低。lint 本轮全绿是真实的；以上是“规则本身的已知洞”，不是“规则被绕过”。
- **修复建议**：lint.sh 加 `bash -n`（或 shellcheck，若可用）步骤；给规则 1 allowlist 与规则 3 KNOWN-EXCEPTION 加摘掉条件与日期；评估 `gosec` 是否启用（与 semgrep 规则 4 去重后决定）。**本轮未修，只报告。**

## 6. 首轮 96 项延期项逐项复查（主表）

统计口径：首轮 108 项 − A+B 已修 12 项（P0 6 项 + P1-1、P1-2、P1-26、P1-31、P1-32、P1-40）= 96 项延期。状态定义：**已修** = 标题所述问题已闭合；**部分修复** = 核心子项已修、仍有明确残留；**仍开** = 标题所述问题未闭合（即使有连带缓解，也在备注里写明）；**建议销项** = 本轮代码上无法复现首轮说法。分类以本表为准；与各分面报告的局部口径有出入处见表后注。

| 编号 | 状态 | 一句话证据 |
|---|---|---|
| P1-3 传输层无限制 | 仍开 | 无 `SetReadLimit`、无速率限制/失败封禁、`CheckOrigin` 恒 true、deviceID 仍为 model+IP、同 IP 新连接踢旧连接；`Serve` 的 `http.Server` 仍无超时。 |
| P1-4 Compose 暴露面 | 仍开 | whisper/piper/mosquitto/MA/网易云端口仍发布到局域网，mosquitto 仍无配置/密码文件，镜像仍全 `:latest` 无 digest，hub 容器仍 host 网络 + root 运行、无 USER/资源限制。 |
| P1-5 HA 明文 + 读挂起 | 仍开 | `config.yaml` 仍为 `ws://192.168.1.10:8123`；`Dial` 两次 `ReadMessage` 仍无 deadline；空 token 仍会发一次 auth 再失败。 |
| P1-6 下游明文密钥 | 仍开 | Agent/MA/网易云仍走明文 HTTP；`MA_TOKEN` 为空即无认证；`agent.Chat` 错误分支仍把上游响应体全文拼进错误。 |
| P1-7 MPD 换行注入 | 仍开 | `PlayURL`（`music.go:463-468`）仍对 URL 不做 `\r`/`\n`/控制字符校验，直接 `add %s`。 |
| P1-8 解包无哈希校验 | 仍开（核心已修） | 入口已调 `verify-bin`/`verify-sqfs` 白名单校验（P0-5 连带修复）；但 `unpack.sh:52-55` 与 `patch.sh:62-67` 仍 `find … \| head -1` 任选，旧解包产物不清理、`mico_version` 不比对入参版本。 |
| P1-9 OTA 验证零调用 | 仍开 | 见 §4.4：`ConfirmHello`/`MarkRebooted`/`Sweep` 生产零调用；`otaAPI` 在 `PushOTA==nil` 时仍回 200。 |
| P1-10 指纹可选 | 仍开 | `hub_ca_fingerprint` 仍 `Option`、无指纹时只 warn 走系统根证书（`transport.rs:321-340`）；`tls=false` 只 warn 照常启动。 |
| P1-11 60s 读超时误杀 | 仍开 | Hub 与客户端均无任何 Ping 发送（全仓 grep `WriteControl`/`PingMessage`：4399 链路零命中）；读 deadline 仍一次性 60 秒、永不续期。 |
| P1-12 重连误删新连接 | 仍开 | `transport.go:240-246` 的 defer 仍按 deviceID 无条件 `delete` + `SetOnline(false)`，无连接身份比对。 |
| P1-13 仲裁忽略时间戳 | 仍开 | `Arbitrate`/`Arbiter` 仍不用 `Timestamp`；无冷却期、无去重、无赢家失败补位；`OnWake` 仍用 `time.Now()` 覆盖。 |
| P1-14 RMS 单帧值 | 部分修复 | 服务端边车改取最近 1s（50 帧）RMS 均值，单帧静音值问题已消除；但本地 local 模式仍取单帧值（`kws.rs:207`），且均值会被 trailing 静音稀释（见 SC-P2-2）。 |
| P1-15 对讲零触发 | 仍开 | 见 §4.4：`Start`/`Sweep` 生产零调用；`OnDisconnect` 只单向 `End`；客户端 `start_recording` 还是空操作（P1-N2）。 |
| P1-16 播放状态机错 | 仍开 | 见 §4.4：三点全在（立即复位 + 无 `stop_play`；`speakText`/对讲裸帧；打断恒 false）。 |
| P1-17 对讲失败落管线 | 仍开 | `main.go:108-126` 的 `Forward` 出错仍不 return，继续走语音管线。 |
| P1-18 紧急呼叫谎报 | 仍开 | `router.go:282-285` 的 `_ = r.deps.Notify` 后仍无条件回“已经帮你呼叫急救并通知了家人”；重复触发仍不重试；`emergencyCall` 仍无 `notified` 字段。 |
| P1-19 “报警”误触发 | 仍开 | `emergencyRe`（`router.go:251`）仍含裸 `报警`，无词边界、无否定排除。 |
| P1-20 Wyoming 无锁 | 仍开 | `wyoming.Client`（`wyoming.go:32-36`）仍无 `sync.Mutex`；两台设备 + 提醒 goroutine 并发调同一 STT/TTS 连接时读写交错。 |
| P1-21 HA 并发写不安全 | 仍开 | `ha.go:146-175` 的 `call()` 与 `SubscribeEvents` 的 `WriteJSON` 仍不持写锁；本轮 `-race` 的测试替身竞态即此 bug 类的演示（N-P2-1）。 |
| P1-22 Web PUT 假热更新 | 仍开 | PUT 仍全量替换（`web.go:312-347`，缺字段即清空、无合并）；运行中 Agent/HA/Wyoming/Router 仍不重建；`OnRoomClaim` 与 PUT 各写各的指针与同一 `.tmp` 文件；JSON 字段仍为 Go PascalCase、与 YAML snake_case 不一致。 |
| P1-23 Devices map 无锁 | 仍开 | `OnRoomClaim`（`main.go:213-233`）仍无锁写 `h.cfg.Devices` 并 `Save`；Web GET/PUT 并发读写同一 map；`Save` 仍固定用 `path+".tmp"`。 |
| P1-24 未知 Kind 每秒触发 | 仍开 | `reminders.go:202-237` 的 `tick` 的 switch 仍无 `default` 分支；未知 Kind 既不删也不重排，`Next` 恒 ≤ now，每秒触发一次 `onFire` 永不停；`Add`/`Load` 仍不校验 Kind。 |
| P1-25 每日提醒越界无声 | 仍开 | `router.go:217-232` 的 hour/min 仍无范围校验；`每天 25 点` 仍经 `NextDaily` 报错 → `Route` 返回 error → 只记一条日志，用户全程无声、提醒未建。 |
| P1-27 下游无重连 | 部分修复 | MPD/Snap 启动失败时后台 30 秒重试已加；但 MA 恢复后 QueueID 不热更新（`main.go:383-395` 注释自认需重启）、HA/Wyoming/MPD/Snap 中途断线仍永不重连。 |
| P1-28 语音无上限/断线不清 | 部分修复 | 单句 30 秒上限已加（480000 采样点）；但 `OnDisconnect` 仍不清理 Pipeline 残留 utterance、不删 `decoders`（`main.go:253-261`）。 |
| P1-29 回答同步阻塞 | 仍开 | `answer` 仍在 transport 读循环里同步跑（STT→Router→TTS→编码），`context.Background()` 无超时；`ChatStream` 生产零调用；下游失败仍只记日志。 |
| P1-30 patch_all 无回滚 | 仍开 | `_write` 仍直写无临时文件 + rename；`patch_all`（`patch_firmware.py:690-720`）中途 `PatchError` 即退出，已写项无回滚。缓解：`patch.sh` 每次从干净解包复制、失败删半成品镜像。 |
| P1-33 安装脚本覆盖配置 | 仍开 | `nas-install.sh:95-96` 仍 scp 全量覆盖 NAS 端 `config.yaml`；`:104` 的 `.bak` 仍是覆盖**之后**从新文件复制的，备的不是旧文件。 |
| P1-34 网易云检查恒 PASS | 仍开 | `nas-install.sh:141-142` 仍以 `curl … \| head -c 200; echo` 结尾，`echo` 恒返回 0，`ok` 恒打印；仍无 `-f`。 |
| P1-35 安装脚本放行弱 Token | 已修 | 预检查已拒绝 <16 位与 `CHANGE_ME` 等弱值（`nas-install.sh:50-62`），`FAIL>0` 即退出（`:69`）。A+B 12 项之外唯一被顺带修掉的延期项。 |
| P1-36 串口免密 root | 仍开 | `patch_firmware.py:386` 仍把 inittab 改为 `ttyS0::askfirst:/bin/ash --login`；拆机串口即 root，无需密码。 |
| P1-37 mosquitto 匿名 | 仍开 | compose 仍发布 `1883:1883`、无任何 mosquitto 配置卷；Hub 代码仍无 MQTT 引用。是否默认匿名仍需 NAS 核实（同首轮）。 |
| P1-38 多 Agent 路由假 | 仍开 | `main.go:251-262` 启动时选定一次；回退分支仍为 Go map 随机迭代 `break`；`OnWake` 与 `Route` 仍不看 `WakeWord`。 |
| P1-39 房间认领端到端断 | 仍开 | `router.go` 仍无“我在这里/我在 X”解析；`room_claim` 仍无生产发送方（客户端 `send_room_claim` 仍 `#[allow(dead_code)]`）；服务端 `OnRoomClaim` 仍只有测试调用。 |
| P1-41 记忆只写不读 | 仍开 | `Memory.All` 生产零调用；“2 小时后关灯”类定时动作仍无分支（`afterRe` 只匹配“提醒我”）。 |
| P1-42 查状态失败照调 | 仍开 | `router.go:380-393` 的 `GetEntityState` 出错仍跳过比较、直接 `CallService`；查询类命令（“灯开着吗”“多少度”）仍无分支。 |
| P1-43 客户端无限重连 | 仍开 | `connect_with_retry`（`transport.rs:710`）仍无限循环、退避上限 1s、无 jitter、不区分 `hello_reject`（强但错的 Token 也 1 次/秒永重试）；主循环在重连期间不监听 Ctrl-C。 |
| P1-44 发现扫错方向 | 仍开 | 见 §4.4：音箱是 WS 客户端、不监听 :4399，生产扫描仍找不到设备；安全模型已修（默认关闭、匿名探测、指纹白名单），方向未修。 |
| P1-45 配置校验缺 | 部分修复 | `Validate` 已大幅补强（下游 URL scheme/host 白名单、STT/TTS/Snapcast host:port、`scan_cidr`、KWS URL/keywords）；但仍缺：`do_not_disturb` 格式、`wake_word_routes` 的值须在 `agents` 里存在、`music_folder` 存在性启动检查、`hub.mpd` 校验（见 N-P2-6）、种子设备空 IP。 |
| P1-46 部署物无时区 | 仍开 | compose 与 Dockerfile 仍无 `TZ`/`tzdata`/`/etc/localtime` 挂载（grep `TZ` 零命中）；容器默认 UTC，提醒与勿扰仍整体错 8 小时。 |
| P2-1 证书 10 年有效 | 仍开 | `tlscert.go:82,174,268` 三处 `NotAfter` 仍为 10 年；SAN 仍只有回环地址；指纹校验仍忽略有效期与主机名（客户端 `transport.rs:117-128` 同）。 |
| P2-2 Token 比较时序 | 部分修复 | 比较已改 SHA-256 后恒定时间比较（`transport.go:156-160` 的 `tokenEqual`）；但 hello 拒绝原因仍可区分枚举（`first message must be hello` / `bad token` / `unsupported model` 各回不同 `reason`）。 |
| P2-3 字段无长度校验 | 仍开 | `OnRoomClaim` 只 `TrimSpace`、判空，无长度/字符集限制；`Hello` 的 `Model`/`Room`/`Version` 原样进注册表与 `/api/devices`；房间名含 `\n` 可伪造日志行（`main.go:233`）。 |
| P2-4 数据文件 0644 | 仍开 | `config.Save`、`reminders.saveLocked`、`memory.saveLocked` 仍为 `0644`；`MkdirAll(DataDir, 0755)` 仍为 `0755`。 |
| P2-5 PUT 与认领互覆盖 | 仍开 | 见 P1-22：两个指针、同一 `.tmp` 文件、无锁；`OnRoomClaim` 写回的是 PUT 换指针前的旧 `Config` 对象，会把 PUT 的修改 whole-file 覆盖丢。 |
| P2-6 patch.sh 验证不完整 | 仍开 | §6 只查 11 个文件存在 + 3 处 grep，不查 shadow/PAM/禁 OTA 与文件权限；验证用 `unsquashfs -d … -f -no-exit-code` 会掩盖解包错误（P2-39）。 |
| P2-7 上游响应无上限 | 仍开 | MA `command` 的 `io.ReadAll`（`music.go:312`）仍无上限；Agent 错误分支仍回显上游响应体全文（`agent.go:86,124`）；网易云 `Decode` 仍无 `MaxBytesReader`。（Web 侧 PUT/POST 已加 1MB 上限，是已修的一半。） |
| P2-8 密码进参数/日志 | 部分修复 | `patch_firmware.py` 已无 `--password` 参数，密码只走环境变量或 `getpass`；残留：`patch.sh` 仍用环境变量 `SSH_PASSWORD` 传密码（`/proc/<pid>/environ` 同用户可见），注释与 README 示例行仍出现 `SSH_PASSWORD=<密码>` 字样。 |
| P2-9 版本号正则未转义 | 仍开 | `unpack.sh:24-27`、`patch.sh:37-40`、`flash-check.sh:30-33` 三处共用 `case … in [0-9]*.[0-9]*.[0-9]*)`，`.` 未转义可匹配 `/`；缓解：`unpack.sh`/`patch.sh` 建目录前先跑 `verify-bin` 白名单校验，不在白名单的版本到不了写操作。 |
| P2-10 .so 路径任意指定 | 部分修复 | 32 位结构体尺寸已在测试里钉死（`sherpa.rs` 的 `c_api_struct_sizes_match_c_header` 增加 32-bit 分支：OnlineModelConfig=68、KeywordSpotterConfig=104、KeywordResult=28）；但该分支只在 32-bit 目标下执行，x86_64 实跑从未覆盖，ARM 上仍未确认；`kws_lib` 仍任意路径 dlopen，无白名单、无 SHA256 校验。 |
| P2-11 HA 回调同步执行 | 仍开 | `ha.go:104-145` 的 `readLoop` 里事件 handler 仍同步执行；告警 TTS（数秒）仍会堵住 HA 结果分发；回调 panic 仍会杀死 `readLoop`。 |
| P2-12 紧急呼叫无二次确认 | 仍开 | 任何语音（含电视/录音/广播）说“救命/打120”即触发，无声纹/二次确认。 |
| P2-13 提醒可重复 Start | 仍开 | `reminders.Start` 仍无“已启动”守卫；`Stop` 在 `main.go` 全文仍无调用。 |
| P2-14 提醒落盘错误吞掉 | 仍开 | `tick`/`Cancel`/`CancelByText` 的 `_ = s.saveLocked()` 仍在（`reminders.go:133,148,230`）。 |
| P2-15 记忆 key 秒级碰撞 | 仍开 | `memory.go:44-52` 仍用 `time.Now().Format(time.RFC3339)`（秒级）做 key，同一秒内两次“记住”仍互相覆盖。 |
| P2-16 HA 写失败泄漏 pending | 仍开 | `ha.go:165-195` 的 `call()` 写失败仍直接 `return`，未删 `c.pending[id]`。 |
| P2-17 Wyoming 无超时/死循环 | 部分修复 | `Transcribe`/`Synthesize` 已加 120 秒整体超时与总量上限；但未知事件类型仍静默循环（无计数、无上限），只靠整体超时兜底。 |
| P2-18 set_volume 非法值重置 | 仍开 | `main.rs:217` 仍是 `.and_then(\|x\| x.as_u64()).unwrap_or(50)`（另见新 P1-N1：即使合法值也不落硬件）。 |
| P2-19 非推流仍每帧编码 | 仍开 | 捕获任务 `main.rs:339` 无条件 `encode_pcm`，`run_session` 非 streaming 时照常 drain 后丢弃（local 模式）；server 模式下 streaming 恒 true，本项不适用。 |
| P2-20 KWS 失败每帧 warn | 仍开 | `kws.rs:225` 仍是 `tracing::warn!("sherpa feed failed: {e}")`，无节流。 |
| P2-21 is_ready 循环无上限 | 仍开 | `sherpa.rs:462` 仍是 `while (self.api.is_ready)(...) != 0 { decode }`，无迭代上限、无超时（边车面同款循环见 SC-P2-1）。 |
| P2-22 AEC 零能量步长爆炸 | 仍开 | `aec.go:58-90` 的 `step = mu*e/(norm+eps)` 仍为 `eps=1e-6`、无能量阈值门；参考信号全零时步长仍爆炸。`EchoCancel` 生产零调用，故仍为 P2。 |
| P2-23 download-ota 无校验/超时 | 部分修复 | 下载超时已加（`fetch_ota_info` 的 `urlopen(url, timeout=30)` 与流式下载）；但 `build_ota_url(model, version, …)` 仍不校验 model/version 格式，残留主要是 CLI 参数校验的纵深问题。 |
| P2-24 nas-install 检查水分 | 部分修复 | 1705/1704 端口检查已补；但 4399 仍漏、ps 仍无计数、sed 仍无校验、curl -f 只修了 MA 检查（网易云仍是 P1-34）。 |
| P2-25 勿扰格式非法静默 | 仍开 | `InDoNotDisturb` 解析失败仍返回 `false`（按“不在勿扰”处理）；`Validate` 仍不校验 `DoNotDisturb` 格式。 |
| P2-26 安全判定子串匹配 | 仍开 | `KindFor`（`alerts.go:52-63`）仍只看 entity_id 含 `leak`/`smoke`/`water` 子串，不校验 domain 是否为 `binary_sensor`。 |
| P2-27 ByRoom 随机/幽灵行 | 仍开 | `ByRoom` 仍遍历 map 取第一个在线（Go map 随机顺序）；`config.yaml` 的 `livingroom-speaker`/`bedroom-speaker` 空 IP 占位仍在，与真实 ID（`model-IP`）永远对不上。 |
| P2-28 decoders 永不回收 | 仍开 | `decoderFor` 只增不减；`OnDisconnect` 仍不删（见 P1-28）。 |
| P2-29 探针覆盖版本号 | 仍开 | `ProbeModel` 仍发 `version: "probe"`；`Upsert` 仍非空即覆盖。见 P1-44。 |
| P2-30 mksquashfs 参数不一致 | 仍开 | `patch.sh:118-121` 只取 `file` 读出的 `-comp` 与 `-b`，其余参数写死；与原厂参数是否完全一致未在仓库内留证据；只有 `>= 40MB` 硬失败，无预警阈值。 |
| P2-31 patch.sh file 解析死代码 | 建议销项 | 本轮代码上未复现：`patch.sh:104-112` 的 `[ -n "$COMPRESSION" ] \|\| fail` 与 `[ -n "$BLOCKSIZE" ] \|\| fail` 均有实际分支，`COMPRESSION`/`BLOCKSIZE` 在 `mksquashfs` 调用里真实使用。首轮的“死代码”说法在本轮代码上不成立，建议销项（需首轮审计员确认当时所指是否为另一处已改掉的代码）。 |
| P2-32 patch_firmware 非幂等 | 部分修复 | `check_login` 已接受 `$6$`（已打补丁态）；但 `check_ssh` 仍要求原厂 `enable=0` 行仍在，对已打过补丁的 rootfs 重跑会在 `patch_ssh` 处 `PatchError`；`patch_all` 仍无回滚。 |
| P2-33 asound 改动无备份 | 仍开 | `patch_asound`（`patch_firmware.py:530-560`）原地 `_write` 两份 asound 配置，无 `.bak`；恢复只能靠 `unpack.sh` 从官方 `.bin` 重解。 |
| P2-34 /data/init.sh 无校验钩子 | 仍开 | `patch_start`（`patch_firmware.py:492-512`）在 `rc.local` 里加 `[ -f "/data/init.sh" ] && sh /data/init.sh >/dev/null 2>&1 &`；`/data/init.sh` 本身不在补丁里创建、无签名/哈希校验。 |
| P2-35 OTA 禁用残留路径 | 仍开 | `patch_disable_ota` 改 `bin/ota`、`bin/flash.sh`、`etc/init.d/wireless`（`patch_firmware.py:440-490`），仓库内无证据表明存在其他 OTA 入口；是否有残留路径只能真机核实（BLOCKED）。 |
| P2-36 Dockerfile 无 USER/HEALTHCHECK | 仍开 | 仍 `FROM debian:bookworm-slim` 无 digest、无 `USER`、无 `HEALTHCHECK`、无 `.dockerignore`，二进制无校验。 |
| P2-37 compose 无资源限制 | 仍开 | 全 compose 无任何 `mem_limit / cpus / user:`；hub 以 root 跑在 host 网络下（与 P1-4 同根）。 |
| P2-38 OTA 30s 超时偏短 | 仍开 | OTA 30 秒超时是 `hub/internal/ota` 的常量，runbook 未提及；`flash.sh` 复用风险是厂商代码风险，首刷不走该路径（runbook §1 已写明补丁镜像无小米签名、走不通原厂 OTA 路径）。 |
| P2-39 unsquashfs 掩盖解包错误 | 部分修复 | `unpack.sh` 与 `patch.sh` 仍用 `-no-exit-code`，但其后均有产物校验（`unpack.sh` 查关键文件；`patch.sh` §6 解开验证 11 个文件 + 3 处 grep）。残留风险只剩“关键文件都在、但其他文件损坏”的部分解包，定级维持 P2 偏低。 |
| P2-40 录音任务死一次永久失聪 | 仍开 | `main.rs:316-348`：`Audio::open()` 或 `capture_pcm()` 任一出错即 warn 后 `return`，任务退出、不重试；主循环只把 `wake_alive`/`audio_alive` 置 false 继续跑传输（server 模式下还会继续发 `audio_start`、报健康状态，实际零帧——静默失聪）。 |
| P2-41 重连丢音量与会话 | 仍开 | 每次重连 `Session::new()`/`new_server()`，`volume` 硬编码 50（`main.rs:84,97`），`Session` 在重连循环内创建、不跨会话保留。 |
| P2-42 配置缺省危险默认值 | 部分修复 | Token 默认已改为空串（fail closed，`config.rs`），缺 Token 时建连前即报错；但配置文件缺失/读失败仍静默回退默认配置（`config.rs:181-185` 的 `Err(_) => Ok(Config::default())`，含权限不足），`hub_url` 默认仍是写死的 `wss://192.168.1.10:4399`；`validate()` 仍只校验 `kws_mode`。 |
| P2-43 日志含语音文本 | 仍开 | 客户端 `main.rs:210` 仍是 `info!("rpc play_text: {text}")`，全文进日志；Hub `main.go:487` 的 `speakText` 仍把提醒/告警全文记日志，`router.go:281` 的紧急消息含触发原文。 |
| P2-44 提醒存储细节 | 仍开 | `tick` 仍每秒全量写盘（无论有无变化）；`Load` 损坏仍静默丢全部提醒（`main.go:267-269` 的 `_ = remStore.Load()` 无日志）；`Stop` 后无法再 `Start`（`stopCh` 已关闭）；`onFire` 仍同步执行。 |
| P2-45 提醒输入细节 | 仍开 | `每 0 分钟` 仍立即触发一次随后被删除；`CancelByText` 仍要求全文精确相等；仍无“改时间”入口。 |
| P2-46 ChatStream 行缓冲 | 仍开 | `agent.go:102-158` 的 scanner 上限仍为 1MB，超长行仍丢弃整段已生成回答（`sc.Err()` 后 `return "", err`，`full` 被丢弃）。 |
| P2-47 发现 goroutine 风暴 | 部分修复 | `Scan` 对非法 CIDR/未开启已返回真实错误（不再恒 nil 的一部分）；但仍为每个 IP 起一个 goroutine，ctx 超时时仍返回 `(found, nil)`，`localCIDR` 仍强制 `/24`。 |
| P2-48 语音管线细节 | 部分修复 | 尾部不足 320 点的采样仍丢弃、合成采样率仍不校验；但“同设备并发跑两次回答”一半在生产上**不可达**（每设备只有一条 transport 读循环串行调 `FramePCM`/`End`，首轮的并发前提不成立——以代码为准的纠正）。 |
| P2-49 场景绕过 allowlist | 仍开 | `sceneMemory` 的“我回来了”仍直接 `CallService("scene","turn_on","scene.coming_home")`，不校验场景内实体 domain。 |
| P2-50 点歌不拆分歌手 | 仍开 | `music()` 仍把“周杰伦的青花瓷”整个串当歌名传给 MA，`artist` 参数恒为 `""`。 |
| P2-51 注册表房间语义 | 仍开 | `Upsert` 仍无法清空房间；hello 的 `Room` 每次重连仍覆盖语音认领的房间；种子键与真实 ID 仍对不上。 |
| P2-52 构建脚本静默应急版 | 仍开 | `build-armv7.sh` 在 musl libasound 缺失时仍只打印 WARNING 后 `--no-default-features` 继续编、exit 0；gnueabihf 脚本对不可用的 vendor `.so` 同样只警告不失败（exit 0），绿构建 ≠ 本地 KWS 可用。 |
| P2-53 主循环收尾 | 仍开 | `sweepSessions` 仍用 `time.Tick`（`main.go:271`，永不 `Stop`）；wss/ws 服务 goroutine 里仍 `log.Fatalf`；退出信号处理仍只关 KWS 与 HA，不调 `remStore.Stop()`、不关 Wyoming/MPD/Snap/Web。另 MPD/Snap 重试 goroutine 也用 `time.Tick`（`main.go:440,454`）。 |
| P2-54 公开自由函数 panic | 仍开 | `audio.rs:107,118,128` 的 `encode_pcm_frame`/`decode_opus_frame` 与线程本地 `CODEC` 初始化仍用 `.expect(...)`；目前仅测试调用，但签名仍是 panic 语义。 |
| P2-55 部署物缺失 | 部分修复 | `.env.example` 已补（占位值 + 注释）；但二进制 mtime 校验仍无（只查存在）、sed 替换仍无断言；`.env.example` 还缺 `OPEN_XIAOAI_WEB_TOKEN`（见 FW-P1-1）。 |
| P2-56 Web 面板细节 | 部分修复 | `/api/*` 已加鉴权、GET config 已脱敏；但 `index` 处理器仍匹配所有未命中路径并返回 200 HTML（无 404），仍会掩盖调用方拼错路径。 |

**复查统计（96 项延期）**：

| 状态 | 数量 | 明细 |
|---|---|---|
| 已修 | 1 | P1-35 |
| 部分修复 | 17 | P1：P1-14、P1-27、P1-28、P1-45（4）；P2：P2-2、P2-8、P2-10、P2-17、P2-23、P2-24、P2-32、P2-39、P2-42、P2-47、P2-48、P2-55、P2-56（13） |
| 仍开 | 77 | P1 35 项（P1-3、P1-4、P1-5、P1-6、P1-7、P1-8、P1-9、P1-10、P1-11、P1-12、P1-13、P1-15、P1-16、P1-17、P1-18、P1-19、P1-20、P1-21、P1-22、P1-23、P1-24、P1-25、P1-29、P1-30、P1-33、P1-34、P1-36、P1-37、P1-38、P1-39、P1-41、P1-42、P1-43、P1-44、P1-46）；P2 42 项（P2-1、P2-3、P2-4、P2-5、P2-6、P2-7、P2-9、P2-11、P2-12、P2-13、P2-14、P2-15、P2-16、P2-18、P2-19、P2-20、P2-21、P2-22、P2-25、P2-26、P2-27、P2-28、P2-29、P2-30、P2-33、P2-34、P2-35、P2-36、P2-37、P2-38、P2-40、P2-41、P2-43、P2-44、P2-45、P2-46、P2-49、P2-50、P2-51、P2-52、P2-53、P2-54） |
| 建议销项 | 1 | P2-31 |
| **合计** | **96** | 1 + 17 + 77 + 1 = 96，与首轮延期数一致 |

**架构变化作废项（不在 96 项内，单列）**：

| 项 | 状态 | 说明 |
|---|---|---|
| P1-40（A+B 已修项，非延期） | 架构替代，非原地修复 | 服务端 KWS 已落地：唤醒移出音箱，ARM `.so` 的 glibc 问题不再是生产关口。构建侧已修（标记断言、默认开 feature、gnueabihf 动态链接、fail-loud）；但本地 KWS 代码保留为未来 A 路可选项，客户端默认 `kws_mode="server"`。文档仍留着已被证伪的“ORT 只要求 GLIBC_2.4”说法（见 §8）。 |
| KwsLogMonitor（过渡 sidecar 路线，非审计项） | 已作废 | TEST_REPORT 与 `client/kws/README.md` 已写明作废原因（服务端 KWS 后不再需要）；代码保留但 `kws.rs` 头注释标明作废，不再接线进 `main.rs`。 |

**口径注**：
- P1-14：客户端面报告按 local 模式计“仍开”（`kws.rs:207` 仍取单帧值），边车面报告按 server 模式计“部分缓解”；主表按“服务端 1s 均值已落地、local 模式仍开”计**部分修复**。
- P1-8：核心入口校验已随 P0-5 修复，但标题的另一半（`find | head -1` 任选 + 旧产物不清）仍在；主表按“标题问题未完全闭合”计**仍开**，备注写明核心已修。若按子项比例也可计部分修复，统计口径以本表为准。
- P2-48 的“同设备并发跑两次回答”一半：Hub 面审计员按代码纠正为生产上不可达（每设备只有一条 transport 读循环串行调 `FramePCM`/`End`），首轮的并发前提不成立；该子项不计入“已修”，计入部分修复的备注。

## 7. A+B 12 项修复的本轮复核

首轮审计后经 bai 批准修复的 12 项（全部 P0 6 项 + 接线类 P1 6 项），本轮按面复核代码与测试，结论如下。另附 A+B 之外被顺带修掉的 1 项（P1-35）与 P1-40 的架构替代说明。

| 项 | 本轮复核 | 结论 |
|---|---|---|
| P0-1 Web 零鉴权 | Hub 面代码复核 + `go test ./internal/web/` 通过 | 已修。`/api/*` 加 Bearer 鉴权（独立 `OPEN_XIAOAI_WEB_TOKEN`，未配则 401 fail closed）+ 恒定时间比较 + 单 IP 限流；`NewHTTPServer` 带超时与 1MB 体上限；`WebListen` 默认 `127.0.0.1:8080`；GET config 脱敏。**残留**：Web Token 从未接进部署（NAS 上的 Hub 以空 Web Token 启动，Web 面板注定 401，见 FW-P1-1）；`web_listen` 显式写 `:8080` 不拦截（见 N-P2-6）。 |
| P0-2 默认 root 密码 | 固件面代码复核 + `test_patch_firmware.py` 全过 | 已修。`patch_firmware.py` 已无 `DEFAULT_PASSWORD`；密码只从 `SSH_PASSWORD` 环境变量或交互式 `getpass` 读（≥16 位，未设且非交互时直接失败退出）；hash 为 sha512crypt（`$6$`，5000 轮，随机 16 字符盐）；`main()` 不打印 hash。 |
| P0-3 单 Token 无 mTLS | Hub 面 + 客户端面代码复核 + 测试通过 | 已修。服务端 `ValidateToken`（空/<16 位/弱值即 Fatal）+ `LoadOrCreateMTLS` + `RequireAndVerifyClientCert`，`main.go` 已接线；`tokenEqual` 改 SHA-256 后恒定时间比较；客户端 `validate_token` 建连前拒弱 Token、`load_client_identity` + `build_tls_config` 带客户端证书、`default_token()` 为空串。**残留**：`CheckOrigin` 恒 true 与 deviceID 顶替属 P1-3，未修。 |
| P0-4 发现广播 Token | Hub 面代码复核 + `go test ./internal/discovery/` 通过 | 已修。扫描默认关闭；先匿名探测取证书指纹、白名单内才发 Token；端口取 `cfg.Hub.Listen`；`main.go` 三道门。**残留**：P1-44 方向问题未修（生产扫描仍找不到设备）。 |
| P0-5 OTA 明文 HTTP + 自指 MD5 | 固件面代码复核 + `test_download_ota.py` 全过 | 已修。`download-ota.py:45` 的 API 已是 `https://api.miwifi.com/…`；`EXPECTED` 白名单为强制门；下载链接强制 `https` + host 白名单；`verify-bin`/`verify-sqfs` 子命令已提供，且 `unpack.sh:40` 与 `patch.sh:74-83` 的入口校验均以它为唯一源。 |
| P0-6 刷机流程 | 固件面文档复核 + `flash-check.sh` 三方一致 | 已修（文档与干跑部分；真机部分仍 BLOCKED）。`FLASH_RUNBOOK.md` 已写全：只写非当前槽、刷前 `flash-check.sh`、读 `boot_part`、备份 boot1、刷后校验、回滚演练（§6.2 要求首刷当天必须做一次回滚演练）。真机步骤仍 BLOCKED，无真机证据。 |
| P1-1 Wyoming 帧校验 | Hub 面代码复核 | 已修。`parsePayloadLength` 校验 + 头行限长 64KB + 读写 10s deadline + `Transcribe`/`Synthesize` 120s 整体超时与总量上限。 |
| P1-2 STT/TTS nil panic | Hub 面代码复核 | 已修。`answer()` 开头判 nil 返回明确错误；单句上限 30 秒（480000 采样点）+ 总并发 4 路；`main.go` 降级播报。 |
| P1-26 点歌接线断 | Hub 面代码复核 | 已修。启动时查 MA `Players()` 取默认 `player_id` 作 `QueueID`；`DialMPD`/`DialSnap` 已注入；启动失败后台 30 秒重试、`SetMPD`/`SetSnap` 补接。**残留**：MA 恢复后 QueueID 不热更新（属 P1-27，未修）；Snapcast 响应 id 不校验（见 N-P1-1）。 |
| P1-31 compose 卷挂错 | Hub 面 + 固件面文件核对 | 已修。卷已改挂 `hub-data:/app/hub-data`（`docker-compose.yml:36`）；Hub 的 `DataDir` 默认 `./hub-data`，容器 `WORKDIR /app`，三方一致。**剩余**：NAS 实测 BLOCKED。 |
| P1-32 config 只读挂载 | Hub 面 + 固件面文件核对 | 已修。挂载已去 `:ro`；`OnRoomClaim` 的 `Save` 失败已记日志。**剩余**：NAS 实测 BLOCKED；可写/种子分离未修（P1-22/P1-23 范围）。 |
| P1-40 ARM 二进制无 KWS | 客户端面代码复核 + 边车面架构确认 | **构建侧已修，本地唤醒仍 BLOCKED**（见 §6 架构变化作废项）。构建侧：标记断言、默认开 `--features sherpa-kws`、gnueabihf 动态链接、fail-loud 保留；但 vendor 两个 `.so` 均要求 GLIBC 2.34，音箱上本地 KWS 仍是零能力，当前唯一能工作的唤醒路径是 server 模式（NAS 边车）。构建脚本对不可用的 `.so` 只警告不失败（exit 0），绿构建 ≠ 可部署的本地 KWS。 |
| P1-35（A+B 之外，顺带修掉） | Hub 面 + 固件面代码复核 | 已修。`nas-install.sh:50-62` 预检查已拒绝 <16 位与 `CHANGE_ME` 等弱值，`FAIL>0` 即退出（`:69`）。 |

**复核小结**：12 项中 11 项的修复在代码层面属实（P1-40 为构建侧属实、本地唤醒仍 BLOCKED 的特殊形态）。但其中 3 项的修复**只修了代码，没修部署/文档的配套**：P0-1（Web Token 未进部署，见 FW-P1-1）、P0-4（P1-44 方向未修）、P1-40（文档仍留着已被证伪的 GLIBC_2.4 说法，见 §8）。另：P0-6 的真机部分、P1-31/P1-32 的 NAS 实测部分，仍 BLOCKED。

## 8. 文档滞后（本轮扫出的，全部未修，只报告）

| 位置 | 问题 | 影响 |
|---|---|---|
| `TEST_REPORT.md:166` | P1-40 行仍留着已被证伪的“ORT 只要求 GLIBC_2.4”说法（方案 (b) 括号内：“复用官方 `libonnxruntime.so`，它只要求 GLIBC_2.4”）。同一文件 L235-236 的“前提纠正”节已用 `readelf -V` 证伪（同捆 `libonnxruntime.so` 最高需求 GLIBC 2.34、GLIBCXX 3.4.29）。 | 读前半部分的人会按错误前提做决策（例如以为方案 B 只差重编 sherpa C API）。见 FW-P2-3。 |
| `TEST_REPORT.md:143` | “第二轮复核人重跑结果”：LX06 镜像 SHA 仍写 `cc36b28e…54f06d`、OH2P 镜像 `db00add0…f0423`、OH2P 镜像 30,175,232 字节、ARM 产物 2,852,620 字节、`deploy/hub-linux-amd64` 11,534,848 字节——均已被取代：新 SHA 为 LX06 `7266aba9…d97b3`（38,453,248 字节）、OH2P `139de363…6415`（30,179,328 字节）；ARM 默认交付已是 gnueabihf 2,435,964 字节；hub 二进制已是 11,747,288 字节、SHA `1e4f6f6c…c80a`（`NAS_CHECKLIST.md` 镜像核实表已更新，TEST_REPORT 未同步）。 | SHA 旧值若被误用，`flash-check.sh` 只打 WARN 不拦截（见 FW-P2-5），操作员可能按旧 SHA 去核对新镜像而误判。见 FW-P2-4。 |
| `TEST_REPORT.md:117` | Phase 4 表“一键 OTA”行：“状态机 Push→Verifying→OK/Failed、超时扫荡、Web 下发均验证 \| 通过 \| 真机刷机验证；回滚本身是设备端 boot1 行为”——与 P1-9 的结论冲突：`ConfirmHello`/`MarkRebooted`/`Sweep` 在生产代码里零调用，OTA 任务永久停在 `pushing`，回滚闭环是零实现而非“只差真机验证”。该行把“包内单测通过”写成了接近“功能通过”。 | 见 FW-P2-4。 |
| `TEST_REPORT.md` Phase 4 表 | “对讲全双工 | 未做”与第二轮 B 行“对讲全双工已做（`go test` 88 通过）”自相矛盾——前者是旧表未更新，后者是新行；以 B 行为准，但读者无法自行判断。 | 见 FW-P2-4。 |
| `TEST_REPORT.md` 多处计数 | 多处仍写 `go test 88 通过 / cargo test 37 / pytest 36`，而“审计修复”节协调员重跑已是 `go test 131 / cargo test 46 / pytest 61`。旧数本身是 dated 记录可以理解，但 P1-40 行与 L143 的 SHA 是会被直接拿去刷机的数，必须改。 | 见 FW-P2-4。 |
| `client/kws/SOURCE.md`、`client/src/kws/sherpa.rs` | 仍写 ORT “只要求 GLIBC_2.4”（与 TEST_REPORT.md:166 同款证伪说法）；`sherpa.rs` 头注释里 musl 与 sidecar 的过时现状未同步（sidecar 路线已在 `kws.rs` 头注释里标明作废，两处说法应统一）。 | 见 P1-N3。 |
| `firmware-patch/scripts/patch.sh:8` | 头注释仍写“刷 boot0，保留 boot1 回滚”，与 `FLASH_RUNBOOK.md` §0.3 与 §3 决策表（boot_part 实测为 boot0 时目标是 boot1/system1；未实测时不刷）冲突。 | 见 FW-P2-6。`patch.sh` 本身不写分区，注释不会直接导致误刷；但它是操作员会读到的第二份“槽位说法”。 |
| `deploy/nas-install.sh` / `NAS_CHECKLIST.md` | 端口表、容器数、检查口径三处不一致：脚本 §5 只 `docker compose ps` 打印、不数数，`ok` 无条件打印；脚本 §1/§5 与 checklist §3 的端口表各不相同；网易云检查仍不校验响应内容（P1-34 同类）。 | 见 FW-P2-7。 |

## 9. BLOCKED 清单（只能上真机/真人验证，本轮未验证、不猜结论）

| # | 事项 | 需要什么 | 备注 |
|---|---|---|---|
| B1 | P0-N1：ALSA 双开是否真 EBUSY | 真机确认 `hw:0,2` 子设备数与二次打开行为 | 刷机前必须先修 P0-N1，但 EBUSY 本身是否发生仍需真机确认；修法（按需打开或共享 Audio 实例）不依赖确认结果。 |
| B2 | 刷一台 LX06（首关口） | bai 拆机刷机 + 回滚演练 | 新镜像 + FLASH_RUNBOOK.md 已就绪；但 P0-N1 必须先修。 |
| B3 | Hub 上 NAS（docker-compose） | NAS + bai 操作 | 部署包已就绪，未在 UGOS 上实测；FW-P1-1（Web Token 接线）应先修，否则 Web 面板注定 401。 |
| B4 | 两台音箱同步播一首歌 ≤10ms | B2 + B3 完成 | 需真机测同步误差。 |
| B5 | 真机 KWS 唤醒 + 真人唤醒率（1m/3m ≥95% 目标） | 真人实测 | 合成语音结果（pos 5/5、neg 5/5、pos3 抽样敏感 ~1/7 失败）不能代替真人唤醒率。 |
| B6 | 真机双机仲裁 | 真人 + 两台音箱 | 含“你好”+1s 停顿+“小李”拼成唤醒是否算误触发（需真人测试定性）。 |
| B7 | 真机打断率 | 真人实测 | P1-16 未修，打断恒 false 的问题在真机上表现为打断无效。 |
| B8 | 网易云 QR 登录 | bai 的网易云账号扫码 | 流程已写进 `deploy/README.md`。 |
| B9 | OH2P 刷机端到端 | bai 操作 | 镜像已就绪（新 SHA `139de363…6415`）。 |
| B10 | 真实 HA 集成 | HA 实例 + bai 的设备 | 含 P1-5（明文 ws）、P1-21（并发写）等未修项的真机表现。 |
| B11 | 真机 ALSA `hw:0,0` 采集 / `hw:0,2` 播放 | 真机 | 与 B1 同根；P0-N1 的修复效果需真机验证。 |
| B12 | ARM 上 32 位结构体布局（P2-10 的 32-bit 分支断言值 68/104/28） | ARM 交叉测试 | x86_64 实跑不执行该分支，ARM 上仍未确认。 |
| B13 | P0-6 真机步骤（读 `boot_part`、备份分区、刷后校验、回滚演练） | 真机 | 文档与干跑部分已修，真机部分无证据。 |
| B14 | P1-31/P1-32 的 NAS 实测（hub-data 卷、config 可写） | NAS | 代码层面已修，NAS 上未验证。 |
| B15 | P1-37 mosquitto 是否默认匿名 | NAS 实测 | 仓库里无据可查，取决于镜像默认配置。 |
| B16 | P2-35 OTA 禁用残留路径 | 真机核实 | 仓库内无证据表明存在其他 OTA 入口。 |
| B17 | 边车静音超时 reset 阈值（1.5–2s 建议值） | 真人测试 | 说话慢、词间停顿 >1s 的真唤醒会被切断的风险，阈值需实测。 |
| B18 | 构建不可字节复现的原因（P2-N7） | 环境比对 | 同一脚本、同一树状态重建产物 SHA/大小与上一版不同，原因未确认。 |

## 10. 修复优先级（建议，不执行）

按“刷机前必须 → NAS 部署前必须 → 首关口后 → 其余”的顺序。P0-N1 与 FW-P1-1 是两个硬门：前者决定首关口的“播”是不是全灭，后者决定 NAS 部署后的 Web 面板是不是注定 401。

| 优先级 | 项 | 理由 |
|---|---|---|
| 0（刷机前必须） | **P0-N1**：ALSA 双开 EBUSY（客户端录音任务与会话各开一次 `hw:0,0` + `hw:0,2`） | 唯一新 P0；播放可能在真机上全灭。修法便宜（按需打开或共享一个 Audio 实例），EBUSY 确认可并行做。 |
| 0（NAS 部署前必须） | **FW-P1-1**：`OPEN_XIAOAI_WEB_TOKEN` 接进部署（`.env.example` + compose `environment` + `nas-install.sh` 预检查与带 token 检查 + checklist curl 补 token） | 否则 NAS 上的 Web 面板注定 401，FR-11 一键 OTA 下发无入口；且 `nas-install.sh` 的 8080 检查是虚假 PASS。 |
| 1 | **P1-16**：播放状态机 latch bug（三点全在） | 对讲/提醒/TTS 帧静默丢弃、打断恒 false；不修则即使 P1-15 接通触发也仍无声。 |
| 1 | **P1-N1 + P1-N2**：撒谎的 RPC 回复（`set_volume` 只改内存数、`start_recording` 空操作） | 客户端对 Hub 谎报成功；local 模式对讲发送侧是断的。 |
| 1 | **SC-P1-1 + SC-P1-2**：边车重复 device_id 孤儿流 + 空 keywords.txt fail-open | 孤儿流持续误唤醒；空关键词文件时边车 `/health` 200、永不唤醒。 |
| 1 | **P1-44**：发现扫错方向 | 生产扫描永远找不到设备；安全模型已修，只差方向（改为 Hub 侧注册/心跳或客户端主动上报）。 |
| 2 | **P1-11 / P1-12**：60s 读超时误杀 + 重连误删新连接 | 双机长连接稳定性的基础；与 P0-N1 同属“连接层”问题。 |
| 2 | **N-P1-2**：`DialMPD` 读 welcome 行无 deadline | MPD 故障时 Hub 启动永久挂起、无日志。 |
| 2 | **N-P1-1**：Snapcast `call()` 不校验响应 id | 异步通知会被误当响应，结果错配。 |
| 2 | **P1-40 文档纠正 + 本地 KWS 路线决策**（`SOURCE.md`/`sherpa.rs`/`TEST_REPORT.md:166` 的 GLIBC_2.4 证伪说法） | 文档滞后会误导后续决策；本地 KWS 路线（源码重编 ORT+sherpa vs 长期 server 模式）需 bai 定。 |
| 2 | **P1-33 + FW-P1-2**：安装脚本覆盖配置（`.bak` 备错文件）+ 多处虚假 PASS 检查 | 每次重部署丢数据 + 部署检查不可信。 |
| 3 | 其余 P1（P1-3、P1-4、P1-5、P1-6、P1-7、P1-9、P1-10、P1-13、P1-15、P1-17、P1-18、P1-19、P1-20、P1-21、P1-22、P1-23、P1-24、P1-25、P1-27、P1-28、P1-29、P1-30、P1-34、P1-36、P1-37、P1-38、P1-39、P1-41、P1-42、P1-43、P1-45、P1-46） | 按首轮定级与首关口依赖排：P1-9/P1-15 与 P1-16 同属“接线未完成”簇；P1-3/P1-5/P1-6 属传输安全簇；其余按功能面排。 |
| 3 | 其余 P2（新发现 40 项中的剩余 + 延期 77 项中的 P2） | 在 P1 清完或与 P1 同批处理；其中 N-P2-1（`-race` 测试替身竞态）与 SC-P2-1（边车 `is_ready` 无上限循环）建议优先于一般 P2。 |
| — | **P2-31** | 建议销项（需首轮审计员确认当时所指是否为另一处已改掉的代码）。 |

---

*报告结束。本报告只报告、不修代码（CI 的两处修复除外，见附录 A）。审计发现的修复决策待 bai 看报告后定。*

## 附录 A：CI 修复（本轮，分支 `audit-round2-ci-fix`）

本附录记录任务批准的两处 CI 修复（审计发现本身不修，这两处是明确批准的例外）。CI 结果以 GitHub Actions 实跑为准，本地结果只作预验证，不冒充 CI 结果。

### A.1 Go job：apt 安装行加 `libopusfile-dev`

- **文件**：`.github/workflows/ci.yml`（Go job“安装 libopus”步骤）。
- **改动前**：`sudo apt-get update && sudo apt-get install -y libopus-dev pkg-config`
- **改动后**：`sudo apt-get update && sudo apt-get install -y libopus-dev libopusfile-dev pkg-config`
- **根因（独立复核 run 44063914126 的 Go job 日志）**：`go vet` 在编译 `gopkg.in/hraban/opus.v2` 时报 `Package opusfile was not found in the pkg-config search path`；`opusfile.pc` 由 `libopusfile-dev` 提供；日志无其他根因（`opus.pc` 本身已找到，`libopus-dev` 正常安装）。
- **包名核实**：`apt-cache policy libopusfile-dev` 在 Ubuntu 24.04 上有候选 `0.12-4build3`；`libopus-dev` 候选 `1.4-1build1`。

### A.2 Python job：固件测试在缺 fixture 时显式跳过，并拆分为两个 job

- **文件**：`firmware-patch/tests/conftest.py`、`firmware-patch/tests/test_patch_firmware.py`、`firmware-patch/tests/test_shell_scripts.py`、`.github/workflows/ci.yml`。
- **做法（按任务要求选跳过方案，不删测试、不静默跳过、不把 9.7GB rootfs 塞进仓库、不让 CI 现场下载解固件）**：
  - `conftest.py` 新增模块级标志与辅助函数：`HAS_FIRMWARE_ROOTFS` / `HAS_FIRMWARE_BIN`（文件存在性检查）与 `require_firmware_rootfs()` / `require_firmware_bin()`（缺失时 `pytest.skip("…")`，理由写明缺失路径与“固件材料不在仓库里，本地跑需 speaker-open-xiaoai-rebuild/firmware/”）。另新增 `HAS_PATCHED_WORK`（`firmware-patch/work/<型号>_<版本>/root-patched.squashfs` 是否存在）与 `require_patched_work()`，覆盖依赖 `work/` 已交付产物的测试。
  - `test_patch_firmware.py`：模块级 `patched_rootfs` fixture 与所有直接调 `copy_rootfs()` 的测试在入口先调 `require_firmware_rootfs()`；不依赖 fixture 的纯逻辑测试（`sha512_crypt` 等）不动，CI 里继续跑。
  - `test_shell_scripts.py`：模块级 `unpacked_work` fixture 与 `test_unpack_with_ota_bin_env`、`test_unpack_rejects_tampered_bin`、`test_patch_rejects_tampered_bin`、`test_patch_rejects_tampered_sqfs_when_bin_absent` 等入口先调 `require_firmware_bin()`；`test_flash_check_ok_on_real_work` 与 `test_flash_check_rejects_tampered_image` 入口先调 `require_patched_work()`。
  - 跳过是显式的：pytest 输出里显示为 `SKIPPED` 并带理由（`pytest -q` 的 `s` 计数与 `-rs` 可见），不是静默跳过。
- **Job 拆分（协调员决定）**：原 `python` job 拆为 `python-firmware` 与 `python-sidecar` 两个 job。原因：run 44063914126 里固件步骤先失败、job 中止，kws-sidecar 测试一步从未执行（grep 0 行），串行结构掩盖了边车结果。拆分后边车测试在 CI 里独立跑、独立出结果；固件测试跳过时仍显示 24 passed / 37 skipped，不再拖垮边车 job。YAML 已校验通过。
- **kws-sidecar 测试步骤**：在 `python-sidecar` job 里（bootstrap 自给自足，CI 内下载模型/生成样本），与固件步骤无依赖。

### A.3 本地预验证（2026-11-17 晚，全部实跑；不是 CI 结果）

- YAML 校验通过：`python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"` 打印 `YAML OK`。
- **CI 条件模拟**（干净 checkout，无固件材料、无 work/）：把仓库复制到 `/tmp/ci-sim/open-xiaoai-next`（排除 `firmware-patch/work/`），跑 `python3 -m pytest firmware-patch/tests/ -q -rs`，结果 **24 passed, 37 skipped in 0.94s**——与 run 44063914126 的 `24 passed, 24 failed, 1 skipped, 12 errors` 逐项对应（24 failed + 12 errors + 1 skipped = 37 skipped），0 failed、0 errors；`-rs` 输出里每条 skip 都带明确理由。
- **本地有 fixture 时照常执行**：仓库原位（固件材料齐全）跑 `python3 -m pytest firmware-patch/tests/ -q -rs`，结果 **61 passed in 134.65s**（0 failed、0 errors、0 skipped）。
- **kws-sidecar 测试仍过**：`kws-sidecar/.venv/bin/python -m pytest tests/ -q` 结果 **16 passed in 14.26s**（默认复用 `~/workspace/.kws-exp` 已验证资产，未重新下载模型）。
- **未验证**：GitHub Actions 上的实际运行（须等 PR 开通后 CI 跑）；`go vet` 在装 `libopusfile-dev` 后的 CI 环境里是否通过，只能以 CI 实跑为准，本地 `go vet` 全绿用的是自编 libopus，不能冒充 CI 结果。

### A.4 PR 与 CI 结果

- 分支：`audit-round2-ci-fix`（从 main 509e57a 切出）。
- 提交：`e12e395`（CI 修复：Go job 加 `libopusfile-dev`；固件测试在缺 fixture 时显式跳过）、`40d30fc`（第二轮审计：firmware-patch / deploy / CI 面报告）。
- PR 链接与 CI 结果：（待 PR 开通、CI 跑完后填写；以 GitHub Actions 实跑为准。）
