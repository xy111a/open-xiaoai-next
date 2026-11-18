# P2 全修 + 收尾 — FIXES_P2.md

> 2026-11-18，分支 `fix-p2-loose-ends`（自 main e9091d1），协调员 Friday。
> 本轮只做修复与收尾，不做审计；第三轮审计是下一轮的事，不要在本轮启动它。
> 工作表 `P2_REMAINING.md` 与四面状态文件（`.p2-status-{hub,client,sidecar,fw}.md`）
> 的内容已全部折入本文件，工作文件随 PR 删除，不进 main。
> 测试口径：合成语音标合成；真机才能验证的项一律记 BLOCKED。

## 0. 总览

剩余 P2 共 **86 项**（首轮 56 + 第二轮新 40，减去 P1 轮顺带修掉的 12 项与
P2-31——P2-31 另见 §4）：**修了 73 项，核实闭合/失效 11 项，未修（BLOCKED）2 项**。

| 面 | 项数 | 修了 | 核实闭合/失效 | 未修 BLOCKED |
|---|---|---|---|---|
| Hub | 34 | 25 | 8 | 1（P2-12） |
| 客户端 | 14 | 14（含 P2-42 残留 1 项） | 0 | 0 |
| kws-sidecar | 13 | 13 | 0 | 0 |
| 固件 / 部署 / CI | 25 | 21 | 3 | 1（P2-35） |
| 合计 | 86 | 73 | 11 | 2 |

另：P2-31 复议（§4）不销项、转为修复，已修；P1-4 收尾（§2）全部定夺；
Opus 回声接线（§3）已接上（客户端）；kws `-race` flake（§6）已复现并修掉两处；
上机包（§5）已重编镜像、四路 flash-check 全绿。

基线（main e9091d1）：go 236、cargo 81/85、pytest 固件 68、边车 25、lint 全绿。
本轮终值：go **270 passed**（22 包全 ok）、cargo **96 / 101**（默认 / sherpa-kws）、
pytest 固件 **86**、边车 **55**、lint.sh **全部通过**、go `-race` kws 包 20 遍 0 FAIL。

## 1. 剩余 P2 逐项状态表

“修了”= 按报告建议修完并配回归测试或写清验证方式；“核实闭合”= 报告写错、
已被顺带修掉或已失效，附证据、未重复修；“未修”= 需真机或需 bai 拍板，见 §7。

### 1.1 Hub 面（34 项）

| 编号 | 状态 | 一句话 |
|---|---|---|
| P2-1 | 修了 | 自签证书复用前校验有效期（过期/30 天内过期即备份重签，不再 Fatal）；新证书 2 年、SAN 含局域网 IPv4 + 主机名；证书/私钥 0600。CA 仍 10 年；真机指纹同步见 §7。 |
| P2-2 | 修了 | hello 拒绝原因统一回 `"rejected"`，具体原因只记服务端日志；已核实客户端只看类型不解析 reason。 |
| P2-3 | 修了 | 新增 `ValidateRoomName`（非空、≤16 rune、中文/字母/数字/常用标点，拒控制字符）与 `SanitizeField`（超 64 字符自由文本返回 ""）；非法房间名当未上报，语音认领非法名回复提示。 |
| P2-4 | 修了 | `config.Save` 临时文件 0600；reminders/memory 落盘唯一临时文件 + 0600；证书/私钥/CA 0600；cookie 目录 0700；启动时数据目录 0700 并收紧已有文件到 0600。 |
| P2-5 | 核实闭合 | Web PUT 与房间认领共用 `cfgMu`、同一 `*config.Config` 指针原地更新、`CreateTemp` 唯一临时文件——证据在 `main.go`/`web.go`/`config.go`，未重修。 |
| P2-7 | 修了 | `agent.Chat` 成功响应改 `io.LimitReader` 1MB 限长解码；`netease_auth.getJSON` 改 1MB 限长；Web PUT/POST 已有 `MaxBytesReader`；MA 的无上限 `ReadAll` 已随融合 B 移除。 |
| P2-12 | 未修 | 紧急呼叫二次确认——需 bai 拍板，见 §7。未改 `router.emergency` 代码。 |
| P2-13 | 修了 | `Start` 加守卫（重复 Start 是 no-op，`Stop` 后可再次 `Start`，`Stop` 可重复调）；main 关停流程补 `remStore.Stop()`。 |
| P2-14 | 修了 | `Cancel` 改签名 `(bool, error)`、`CancelByText` 改 `(int, error)`，落盘失败返回 error；`Add` 落盘失败置 `dirty` 并返回错误；调用处落盘失败时回复“取消失败，保存没存住，请稍后再试一次。” |
| P2-15 | 核实闭合 + 加固 | P1-41 已改纳秒 key；本轮又补原子自增序号兜底同一纳秒（key = 纳秒时间戳 + `memSeq`）。 |
| P2-17 | 修了残留 | `Transcribe`/`Synthesize` 已有 120s 整体超时（P1 轮）；本轮修未知事件死循环：读循环对未知事件类型计数，连续 10 个即报错。 |
| P2-25 | 修了 | `InDoNotDisturb`：空串仍 false（未配）；非空但格式非法时返回 true（fail-closed）并记 warn。 |
| P2-26 | 修了 | `KindFor` 要求 domain 为 `binary_sensor`，对象名按 `_` 分词后含 leak/smoke/water 词元，且含 heater/battery/temperature/humidity/tamper 任一词元即排除。 |
| P2-27 | 修了 | `ByRoom` 改确定性（按设备 ID 排序取第一台）；新增 `ByRoomAll`；提醒到点改全房间播报；`config.yaml` 两个空 IP 占位已删除；种子注册按 `DeviceIDFor` 归一化。 |
| P2-28 | 核实闭合 | `main.go` 的 `OnDisconnect` 已删 `decoders[deviceID]`（P1-28），未重修。 |
| P2-44 | 修了 | `tick` 只在有增删改或 `dirty` 时写盘；落盘失败记 error 并置 `dirty` 下次重试；`Load` 损坏时改名备份 `path + ".corrupt.<时间戳>"` 并返回错误；`Stop` 后可再次 `Start`；`onFire` 每条提醒起独立 goroutine；`Load()` 的 `_ =` 改为记日志。 |
| P2-46 | 修了 | `ChatStream` 不再用 `bufio.Scanner`（1MB 上限），改 `bufio.Reader.ReadLine` 分段读行（上限 4MB）；读错或行超限时返回已拼接的部分内容 + 错误。 |
| P2-47 | 核实失效 | 自动发现 `Scan` 已删除，`localCIDR` 在 main.go 中已无踪影；`discovery` 包现只剩手动指纹验证。 |
| P2-48 | 修了 | TTS 尾音：编码循环前把尾部不足一帧的采样补零到整帧；合成采样率：`Synthesize` 校验 `audio-start` 的 `rate` 字段，存在且 ≠16000 即报错；同设备并发跑两次回答已有 `inFlight` 守卫（P1-29），生产不可达，未重修。 |
| P2-49 | 修了 | 新增 `allowedVoiceDomain`（light/switch/climate/cover）；“我回来了”执行前调 `GetSceneEntityIDs` 逐个校验 domain，含高风险实体拒绝执行，查不到场景内容时 fail-closed 拒绝；家电分支共用该白名单。 |
| P2-51 | 修了 | 新增 `registry.ClearRoom`（显式清空房间）、`FillRoomIfEmpty`（hello/register 的 room 只在当前无房间时填充）；hello 与 `handleRegister` 不再经 `Upsert` 覆盖认领；种子键归一化。 |
| P2-53 | 修了 | `sweepSessions`、看门狗（30s）、OTA/对讲/紧急扫荡（10s）的 `time.Tick` 全部改 `NewTicker` + `done` 通道退出；wss/ws 服务 goroutine 的 `log.Fatalf` 改走 `serveErr` 通道；信号处理补全关停顺序（`close(done)` → `remStore.Stop()` → `kwsClient.Close()` → STT/TTS `Close()` → MPD/Snap `Close()` → `haClient.Close()` → Web `Shutdown` 5s 超时）。 |
| P2-56 | 修了残留 | `index`：非 `/` 路径返回 `http.NotFound`；GET 配置暴露内部拓扑一项经核实已有鉴权与字段收敛，无新增改动。 |
| N-P2-2 | 修了 | `SendPCM` 在同一把 `c.mu` 内做 closed 检查 + 建流（拆 `streamForLocked`，持锁调用）；`Start` 的 `wg.Add(1)` 加 closed 守卫；删 `isClosed()`。协调员收尾：`streamFor` 包装函数本身已是死代码（无调用方），一并删除，lint 由此全绿。 |
| N-P2-3 | 修了 | `serve()` 重连成功后先排空队列，计入丢帧数并记日志；首次连接不排空。协调员收尾：排空移到发 hello 之前（见 §6 flake 一节），否则 hello 后新到的实时帧会被误排。 |
| N-P2-4 | 修了 | `handleMessage` 的 wake 分支一律用流的 `s.deviceID`；回传值非空且不一致时记 warn 日志。删 `firstNonEmpty`。 |
| N-P2-5 | 修了 | 默认 `HealthInterval` 30s → 10s；`Start` 启动时立即做一次健康检查；流连接成功与断开时立即异步触发一次 `CheckHealth`。 |
| N-P2-8 | 修了（固件面） | `nas-install.sh` §5 改为注册制，查 Hub 日志 `设备注册制` 行（启动时必打，P1-44），不再查“自动发现/扫描/discover”。 |
| N-P2-10 | 修了 | `config.KWS` 增加 `fingerprint` 字段（`Validate` 在 KWS 启用时校验 64 位 hex）；`Client.tlsConfig()`：wss + 指纹非空时返回 InsecureSkipVerify + `VerifyPeerCertificate`（比对叶子证书 SHA-256 指纹并校验有效期）；main 接线 `TLSFingerprint: cfg.KWS.Fingerprint`。 |
| N-P2-12 | 修了 | `nextBackoff` 加 ±25% jitter（Go 1.23 全局 rand 已自动播种），jitter 后仍钳制在 max 内。 |
| N-P2-1 | 核实闭合 | `go test -race ./internal/ha/ ./internal/kws/` 全部 ok；测试替身并发写已在 PR #1 修掉。另见 §6 flake 收尾（两个新 flake 均已修）。 |
| N-P2-6 | 核实闭合 | `config.Validate` 已含 `hub.mpd`，且有 `validateWebListen` 拒绝 `:8080`/`0.0.0.0`（P1 轮已修）。 |
| N-P2-7 | 核实闭合 | `web.requireAuth` 内已有过期条目清理循环（P1 轮已修）。 |
| N-P2-9 | 核实闭合 | `transport` 的 `hello_ok` 写前已设 5s 写 deadline；`Serve` 的 `http.Server` 已有 ReadHeaderTimeout/ReadTimeout/WriteTimeout/IdleTimeout/MaxHeaderBytes（P1 轮已修）。 |

### 1.2 客户端面（14 项）

| 编号 | 状态 | 一句话 |
|---|---|---|
| P2-19 | 修了 | 录音任务只在 `pushing_audio()` 为真时编码 Opus（`run_session` 用 `Arc<AtomicBool>` 同步会话状态；server 模式恒真，local 模式无仲裁会话/对讲录音时跳过编码，只喂 KWS）；重连等待期间先清标志。 |
| P2-20 | 修了 | `Kws::feed` 推理失败改走 `FailureThrottle`（1 秒一条 + 累计），连续失败 50 次后停 sherpa 后端并报一次 error，退回只算 RMS。 |
| P2-21 | 修了 | `sherpa.rs` 的 `is_ready` 排空循环改为 `decode_ready_loop`（上限 100 次，超限复位流并返回 Err）；核实：server 模式根本不走此路径（`init_kws` 返回 `Ok(None)`），本修复只对 local 模式生效。 |
| P2-22 | 修了 | Go `NLMS.Process` 加零能量守卫（参考窗能量 < 1e-9 时跳过自适应更新、只输出残差）+ 步长钳制 ±1.0；客户端新 `aec.rs` 用同一守卫（1e-9 / ±1.0）+ 全零参考帧短路直通。 |
| P2-40 | 修了 | 录音任务 ALSA 打开/采集失败不再退出：按 100ms 起步翻倍、上限 5s 的退避重开设备（只有采到一帧才清零退避计数，持续失败时日志 cadence 衰减到 5 秒一条）；只有主循环退出（通道关闭）时任务才结束。 |
| P2-42 | 修了（残留 1 项） | `load_from_path` 缺失/不可读时明确报错退出（写清路径与补救方法），不再静默回退默认配置。残留：`hub_url` 默认仍是写死的 `wss://192.168.1.10:4399`——未改：Token 默认已空、指纹必填，错配的默认 Hub 无法鉴权，危险部分已被 fail-closed 兜住；彻底去掉需改配置格式（hub_url 必填），属 breaking change，留待 bai 拍板（见 §7）。 |
| P2-52 | 修了 | `build-armv7.sh`：musl libasound 缺失时默认 `ASSERT FAIL` exit 1，不再静默产出无 ALSA 应急版；只有显式 `--allow-no-alsa` 才降级；构建后断言产物含 ALSA 标记（新增 `#[used] ALSA_BACKEND_MARKER`）。失败路径已实测。 |
| P2-54 | 修了 | `encode_pcm_frame` / `decode_opus_frame` 改返回 `Result`：线程本地存 `Result<OpusCodec>`（初始化失败返回 Err），`try_borrow_mut`（重入返回 Err），不再有任何 `.expect` panic 路径。 |
| P2-N1 | 修了 | `interrupt` 的 `StopPlayback` 动作真调 `Audio::stop_playback()`（ALSA `snd_pcm_drop` 丢弃约 80ms 尾音 + `prepare` 复位）；`AudioDevice` 加 `stop_playback`（默认明确 Err；Fake 设备清空已播缓冲并计数）。 |
| P2-N2 | 修了 | 入站 WS 配置收紧为 64KB（`max_message_size` 与 `max_frame_size` 均为 64KB；协议最大帧是 <1.3KB 的 Opus 帧与 <1KB 的文本消息），TLS 与明文两路都传显式配置；超限即 tungstenite 断线，主循环按断线重连。 |
| P2-N4 | 修了 | 录音任务编码失败不再静默吞掉：节流 warn（1 秒一条 + 累计），连续失败 50 次后记 error 并 `Audio::rebuild_codec()` 重建编解码器。 |
| P2-N5 | 修了 | `audio_tx.send().await` 改 `try_send`：通道满时丢当前帧并计数（5 秒一条节流日志），永不反压堵采集（否则 ALSA 采集缓冲 overrun → XRUN → 录音任务死）。 |
| P2-N6 | 修了 | `parse_hub_text` 与 `connect_once` 的 hello_ok 解析严格化：`hello_ok` 缺 `device_id` 或为空 → Err 断线；`rpc` 缺 `id`/`method` 或为空 → Err 丢弃（不回空 id 的 rpc_result）。 |
| P2-N7 | 修了 | 钉死工具链（`client/rust-toolchain.toml`：rustc 1.100.0）+ 两构建脚本加 `--remap-path-prefix` 与 `SOURCE_DATE_EPOCH`（取 HEAD 提交时间）；宿主机与 ARM 各两次 clean 构建 SHA256 完全相同（宿主机 `67881031…f927`、ARM `a1b42662…86`，2,459,460 字节）。 |

### 1.3 kws-sidecar 面（13 项）

| 编号 | 状态 | 一句话 |
|---|---|---|
| SC-P2-1 | 修了 | `DeviceStream.feed` 单帧最多 decode 32 次（`MAX_DECODE_PER_FRAME`），超限记 error 并 `reset_stream` 该设备流；假 spotter 回归测试验证上限生效。 |
| SC-P2-2 | 修了 | wake 事件新增 `rms_peak`（窗口内单帧 RMS 峰值），`rms` 原字段保留（协议兼容）；pos1 实测峰值帧 RMS 0.311、均值约 0.137，回归测试断言 `rms_peak >= 0.25` 且 `rms_peak > rms`。 |
| SC-P2-3 | 修了 | hello 10s 超时单独捕获 `asyncio.TimeoutError`：记 warning、发 `{"type":"error","message":"hello timeout"}`、以 1008 关闭；回归测试断言消息、关闭码与服务端日志无 traceback。 |
| SC-P2-4 | 修了 | `finally` 里对未被取代的流补喂 25 帧（500ms）静音做最后一次检出，检出只记 `wake lost on disconnect` 日志；探针证实 pos1 纯音频喂完不检出、补 9 帧静音才检出，flush 25 帧确定检出。 |
| SC-P2-5 | 修了 | hello 校验 `device_id` 为 1–64 字符 `[A-Za-z0-9_.-]`，非法即 error + 1002 关闭；报错路径日志用 `_safe_device_id` 单行化。**与报告建议有出入**：报告建议 `[A-Za-z0-9_-]`，实际多了 `.`——既有测试的 device_id 含点，按报告字符集实现后全量测试 11 项失败；点不影响日志单行性。回归测试 `test_p2_5_dotted_device_id_still_accepted` 已钉住该行为。 |
| SC-P2-6 | 修了 | `amain` 启动期校验 `0 < threshold <= 1`、`score >= 0`，越界 FATAL 退出；与实验验证值（0.25/1.0）不同时打 warning。 |
| SC-P2-7 | 修了 | `wake_count` 接入两处：`/health` 新增 `device_wakes`（每设备累计唤醒次数）；wake 日志行新增 `wake_no` 与 `frames_since_wake`（上次检出后喂入的帧数，每帧 20ms，检出延迟的近似度量）。 |
| SC-P2-8 | 修了 | `VOICE_SHA256` pin 两个语音文件（实测 `~/workspace/.kws-exp/tts/` 现有文件）；`_download` 下载后验 SHA，不符删文件重报；`_ensure_voice` 对已存在文件也验 SHA，不符删掉重下。 |
| SC-P2-9 | 修了 | `_verify_samples` 改读 `KWS_THRESHOLD` / `KWS_SCORE` 环境变量（默认 0.25/1.0），与 `server.py` 同源；非法值直接报错。 |
| SC-P2-10 | 修了 | 重抽前把原样本备份为 `<name>.bak-<YYYYMMDD-HHMMSS>` 再覆盖；`_verify_samples` 与模块 docstring 已注明该行为。本轮测试未在共享目录产生任何 `.bak` 文件。 |
| SC-P2-11 | 修了 | 按重抽次数计数（`MAX_REROLLS = 10`，即最多 11 次检测）；旧 `MAX_GENERATION_ATTEMPTS`（10 次检测 / 9 次重抽）已删除；日志改为 `re-roll {k}/10`；文档同步为“最多 10 次重抽（11 次检测）”。 |
| SC-P2-12 | 修了（固件面） | compose 加 `user: "1000:1000"` + 卷属主 one-shot（`nas-install.sh` §4，`chown -R 1000:1000 /data/kws/models`）；限额已有（512M/1.0）。边车工人核实。 |
| SC-P2-13 | 修了 | `curl -sSL` 改为 `curl -sSLf`；SHA 校验失败的报错打印文件大小并说明下载本身已成功（HTTP 错误已被 --fail 拦下），避免误判为上游重新打包。 |

### 1.4 固件 / 部署 / CI 面（25 项）

| 编号 | 状态 | 一句话 |
|---|---|---|
| P2-6 | 修了 | `patch.sh` §6 扩展验证（shadow `$6$` + 0600、PAM、OTA 三处禁用、rc.local 包装器行、asound.bak、包装器文件、dropbear 可执行）。 |
| P2-8 | 修了 | `SSH_PASSWORD_FILE` 优先（读文件、去行尾换行；读失败抛错；空文件落长度校验失败）；错误信息改为生成式示例。 |
| P2-9 | 修了 | patch.sh 三处、unpack.sh、flash-check.sh 均改为严格正则 `^[0-9]+\.[0-9]+\.[0-9]+$`；`1/94/13` 等均被拒绝（有测试）。 |
| P2-10 | 核实 | server 模式下生产不走本地 KWS，残留归客户端面；固件面无可修项。 |
| P2-23 | 修了 | 新增 `validate_ota_params`（model/version/channel/sn 四个正则），`build_ota_url` 与 `expected()` 入口调用；超时已核实存在（`urlopen timeout=30` 两处）。 |
| P2-24 | 修了 | nas-install.sh 开头校验 NAS_IP/NAS_DIR/NAS_SSH 三项输入；§5 `_web_tok` 改为远端读 token（`remote_curl_api`，带引号 heredoc + `$1/$2` 传参），stub 验证含 `$` `;` `"` `'` `(` `)` `&` 的 token 完整无损到达 Authorization 头。 |
| P2-29 | 核实失效 | 自动扫描已删（P1-44 注册制），ProbeModel 无生产调用方；代码层缺陷仍在（ProbeModel 仍用 `Version: "probe"` 覆盖真实版本号），未修——无调用方，修它不改变任何生产行为；是否删除死代码由第三轮审计定。 |
| P2-30 | 修了 | §5 加 38MB 预警阈值（95%）+ 比原厂大 2MB 以上警告（基线 +952,549 字节）；`PATCH_XZ_BCJ` 环境变量 opt-in（默认关闭）；不把 `-Xbcj arm` 设为默认（音箱内核是否支持 BCJ 未证实，猜错会无法挂载 rootfs；实测省约 1MB，当前 38.45MB 距 40MB 上限有 3.49MB 余量）。 |
| P2-32 | 修了 | `check_ssh` 接受原厂态与已打补丁态；`patch_ssh` / `patch_disable_ota` / `patch_libxaudio` 幂等；`patch_all` 两次执行测试通过。修出一个 latent bug：wireless 分支已禁判断漏了开头的 `\n`，重跑会重复插两块 `download_fail`（已修正）。 |
| P2-33 | 修了 | 首次改动前把原厂内容存为 `<文件>.bak`（只在 .bak 不存在时写，绝不覆盖）；两 .bak 路径入 `_PATCHED_FILES` 以便回滚清理。 |
| P2-34 | 修了 | `/etc/open-xiaoai/init.sh` 包装器（/data/init.sh 存在 + 属主 uid 0 + 权限串第 6/9 位非 w + 有 .sha256 时 SHA256 第一字段一致才 `exec sh`，否则拒绝并记 `/data/open-xiaoai-init.log`）；rc.local 改为包装器行，旧直行迁移时替换。 |
| P2-35 | 未修 | OTA 禁用残留路径只能真机核实，见 §7。 |
| P2-36 | 修了 | 与 P1-4 收尾一并做（见 §2）：Dockerfile.hub 加 `appuser`（uid 1000）+ `USER appuser` + HEALTHCHECK + `.dockerignore`。 |
| P2-37 | 修了 | 与 P1-4 收尾一并做（见 §2）：compose 7 个服务全加 `cap_drop: [ALL]`，hub + kws-sidecar 加 `user: "1000:1000"`。 |
| P2-38 | 修了（文档） | runbook 新增 §1.5——绝不复用原厂 `bin/flash.sh` 推补丁镜像；`VerifyTimeout` 30 秒（`hub/internal/ota/ota.go:65`，PushTimeout 10 分钟是另一项），设备须 30 秒内回 hello 否则任务标失败，误判先看设备日志再决定是否重推。 |
| P2-39 | 修了 | inode 计数硬门（期望数 = `unsquashfs -s` 的 inodes − stderr 里 device 创建失败行数）+ stderr 过滤（仅允许建 device 失败与 xz 选项行，其余含 error/failed 的行即 FAIL）；patch.sh §6 与 unpack.sh 同款。实测：原厂 root.squashfs 2137 inodes / 解出 2136、stderr 唯一良性行是建 /dev/console 失败；补丁镜像 2141/2141、stderr 为空。 |
| P2-55 | 修了 | nas-install.sh §0 加 Hub 二进制清单校验（`deploy/hub-linux-amd64.sha256` 存在时比对 SHA256 与大小，不一致 FAIL；不存在时 WARN）；§2 用 tar 传 kws-sidecar 构建源文件（Dockerfile/server.py/entrypoint.sh/requirements.txt/scripts/，tests/ 与 .venv 不传）。 |
| FW-P2-1 | 修了 | ci.yml Go job 加 `gofmt -l hub scripts`（失败即红）；golangci-lint 本地与 CI 同为 v1.64.8，写进注释；CI 不直接跑 lint.sh 的结构性差距已记（lint.sh 依赖 `~/workspace/tools` 绝对路径）。 |
| FW-P2-2 | 核实闭合 | pytest 已钉版本、固件 e2e 跳过原因与本地补跑命令注释已在 ci.yml（P1 轮已修）。 |
| FW-P2-3 | 修了 | TEST_REPORT.md L166 两处证伪说法加删除线（`GLIBC_2.29` → `GLIBC_2.34`；“它只要求 GLIBC_2.4”删除线 + “已被证伪，见上文勘误”）。 |
| FW-P2-4 | 修了 | L75/L126/L130/L132/L143/L166/L117/L118 各加取代关系标注（旧数保留，新值 + 指针）；L143/L402 与 README §G、NAS_CHECKLIST 镜像核实表的 Hub 值已随 P2 轮重编更新为 12,093,552 字节 / `95535eee…ead7de`；P2 轮结束时新增一节最终重跑。 |
| FW-P2-5 | 修了 | flash-check.sh 加 `--strict`（SHA 不一致即 FAIL，默认仍 WARN）；加 §4b 内容验证（dropbear mkdir、inittab /bin/login、asound Capture、shadow `$6$`、PAM、OTA 三处、rc.local 行、lx06 的 libxaudio 无 hw:0,3）；§4 大小加 38MB 预警；.bak/包装器缺失只 WARN（旧版镜像，下次重编会带上）。协调员收尾：§4b 新增证书核对（ca.crt/client.crt 存在、client.key 0600，缺失即 FAIL）。 |
| FW-P2-6 | 修了 | patch.sh 头注释改为“目标槽以 FLASH_RUNBOOK.md §3 决策表为准（按实测 boot_part 定），不要默认 boot0”。 |
| FW-P2-7 | 修了 | §4.1b `ws://0.0.0.0:10400` → `ws://127.0.0.1:10400`（compose 传 `KWS_HOST=127.0.0.1`，server.py 日志实测）；§4.2 自动发现改为注册制期望（查 `设备注册制` 行）；§1.1 加 kws-sidecar 源码发货；容器数 8→7 已在融合 B 时同步，无残留。 |
| FW-P2-8 | 修了 4 处 + 评估 1 处 | semgrep 规则 3 Rust `vec![x; n]` 缺口：新建 `scripts/check-vec-len.sh`（awk 按花括号深度跳过 `#[cfg(test)]` 模块，只查生产代码；变量长度 FAIL，常量长度放行），lint.sh `run_rust` 与 ci.yml rust job 都调它。实测生产代码无违规；`client/src/aec.rs:181` 有一处 `vec![0i16; total_samples]`，但在 `#[test]` 内且 `total_samples` 是两个常量之积、有界，属测试模块，不查——协调员确认：测试内常量长度 vec! 无风险，不改。check-wiring.go 的 DEFERRED allowlist 已空，补摘掉条件注释；check-bounds.go 的 KNOWN-EXCEPTION 旁加摘掉条件；bash -n：lint.sh 加 `shell` 步骤，ci.yml 加同款独立 `shell` job。gosec：评估完成，不启用（见 §7）。 |

## 2. P1-4 收尾（首轮 P1-4 在 P1 轮只部分修了）

| 项 | 结论 | 为什么这样定、代价是什么 |
|---|---|---|
| Hub host 网络 | **保留 host 网络** | kws-sidecar 与 Hub 都走 host 网络、寻址用 127.0.0.1:10400；snapserver/mpd host 网络是播放链要求；改桥接要动寻址，收益小于回归风险。MA 8095 部分已随融合 B 砍 MA 而失效，不用做。代价：无新增隔离，容器共享宿主机网络命名空间（与之前一致）。 |
| user: | **Hub 与边车走 uid 1000，其余服务不加** | Hub 走 Dockerfile 建 `appuser`（uid 1000）+ `USER appuser`；nas-install.sh §2 传完 config.yaml 后 `chown 1000:1000`（幂等）；§4 up 前先 `docker compose build hub kws-sidecar`，再加卷属主 one-shot（`docker compose run --rm --user root --no-deps --entrypoint /bin/chown hub -R 1000:1000 /app/hub-data` 与 kws-sidecar 的 `/data/kws/models`），覆盖已存在卷的迁移场景。kws-sidecar 在 compose 加 `user: "1000:1000"` + 同款 one-shot。注意：Hub 的 `Config.Save` 用 `os.CreateTemp` 写 `/app`，所以 Dockerfile 里 `/app` 本身也要 chown 给 appuser，否则语音认领房间与 Web 改配置会失败；`mkdir -p /app/hub-data` 并 chown 利用具名卷首次挂载的 copy-up。代价：其余 5 个服务仍以 root 跑（有 cap_drop 兜底），因上游镜像 root-owned 数据目录，改 user 需逐镜像验证。 |
| cap_drop | **7 个服务都加 `cap_drop: [ALL]`** | 失败模式是响亮的启动失败，会被 nas-install §5 检查捕获。 |
| read_only | **不设，暂缓** | Hub 的 config 保存用 `os.CreateTemp` 写 `/app`，做成需要 /app 的 tmpfs 编排且无法在无 NAS 时验证；记暂缓并写清理由。 |
| digest | **全部钉死** | 2026-11-18 实测 Docker Hub token + manifest 接口均通（上轮协调员称 VM 到 Docker Hub 的 manifest 接口超时，本次未复现）。已钉：compose 里 5 个拉取镜像 + Dockerfile.hub 的 `FROM debian:bookworm-slim@sha256:734a72b3…`。kws-sidecar 基础镜像 python:3.12-slim 的 digest 不由固件面钉（Dockerfile 属边车工人）。apt 包不钉版本（bookworm 稳定版，钉死版本号会在快照轮换时断构建，基础镜像 digest 才是钉死底座的一层）。取值日期 2026-11-18 已写进 compose/Dockerfile 注释。 |

digest 取值（2026-11-18 实测，`Docker-Content-Digest` 响应头）：

| 镜像 | digest |
|---|---|
| debian:bookworm-slim | `sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc`（OCI index，含 linux/amd64） |
| rhasspy/wyoming-whisper:latest | `sha256:3cc01f646c8b149fe47b2a5e8bfaf31866e1bc18faffadb26c9b8be58cbe5703`（index，含 amd64） |
| rhasspy/wyoming-piper:latest | `sha256:333622ab9e4bcd00c80a37219b90a1a9d6aa3fd1ad34e0a6e3bddd36ee999efd`（index，含 amd64） |
| ivdata/snapserver:latest | `sha256:20840a2b507b13e167d7e817d9f310fce7271f3abb0b4457c12804cd367509df`（单 manifest，config 架构 amd64；blob 取回需 `curl -L` 跟 307） |
| vimagick/mpd:latest | `sha256:6f8dbbb84cd6fe55d6a6eb8e6da65932f6a93000e56c230f09b15a507e127e7c`（单 manifest，amd64） |
| binaryify/netease_cloud_music_api:latest | `sha256:ed9fc80e16a4813acac61f57399726a533cec402d4eb230c3adb9e023850bb03`（index，含 amd64） |

## 3. Opus 级回声接线（任务三）

来源：TEST_REPORT.md:292——`EchoCancel` 是 PCM 级接口，main.go 对讲目前转发 Opus 帧，
decode→AEC→re-encode + 播放参考对齐这一跳还没接。

**结论：接在客户端，不接 Hub 侧。**

- Hub 侧拿不到可对齐的参考信号。证据：`hub/cmd/hub/main.go` `OnAudioFrame` 对讲帧是
  Opus 原样转发（`intercom.Forward`），`EchoCancel`（PCM 级）在生产代码里零调用；
  Hub 发给设备的播放（`speakText`/`onReply`）是现编现发的 Opus 帧，Hub 从不保存发出的
  PCM，也不掌握“哪一帧在何时被设备实际放出”——网上抖动 + 客户端 ALSA 播放缓冲
  （4 周期 ≈ 80ms）+ 声学传播延迟都无法在 Hub 侧对齐，错位超过 NLMS 256 抽头窗
  （16ms）后滤波器无法收敛。Hub 的 `EchoCancel` 保持原样（未改 `hub/internal/intercom/`，
  未改 `main.go` 的转发处）。
- 客户端同时握有两路信号的同一时钟域：`play_frame` 解出的播放 PCM（放出什么自己知道）
  与 `capture_pcm` 采到的麦克风 PCM（同一 ALSA 设备、同一 16kHz 时钟）。
- 接法（新文件 `client/src/aec.rs`，单频带 NLMS，与 Hub 侧 `aec.New(256)` 同口径，
  mu 0.5，另加 P2-22 守卫）：`run_session` 每次成功播出一帧后，把解出的 PCM 经
  `ref_tx` 通道（容量 16 帧，满则丢，尽力而为）发给录音任务的 `Aec`；录音任务每采一帧
  先过 `Aec::process_frame`（播放参考经 4 帧延迟队列对齐 ALSA 缓冲延迟，再进 NLMS），
  消回声后的 PCM 再喂本地 KWS 与 Opus 编码。Hub 侧对讲转发与语音管线一行不用改。
- 回归测试（合成信号，`aec.rs` 内）：已知回声（参考延迟 4 帧 + 40 点、衰减 0.5）+
  微弱语音，适应 0.5s 后残余 RMS < 输入 50%（与 Hub 侧 `aec.TestEchoSuppressionSynthetic`
  同判定口径，实测残余远低于语音本底判定线）；零参考直通（输出逐采样等于输入、
  系数保持 0）；1 LSB 参考不爆炸；纯语音通过时 RMS 误差 <5%。
- 真机回声消除率（dB）与 `AEC_REF_DELAY_FRAMES = 4`（80ms）的真实声学延迟校准记 BLOCKED（见 §7）。

## 4. P2-31 复议（任务四）

**结论：不销项，转为修复，已修。** 第二轮报告 §6 建议销项 P2-31，协调员亲自复议后否决。

证据：用 `set -euo pipefail` 下的 exact construct 实测——grep 无匹配时 ERR trap
触发、直接退出，根本走不到 `[ -n "$COMPRESSION" ] || fail`（"REACHED-CHECK" 从未打印）。
即：检查只在成功路径上执行（此时 trivially 通过），失败分支不可达。

修复（固件面）：两处命令替换加 `|| true` 兜底；trap 与 `fail()` 顺带清理 SQFS_ROOT；
补回归测试（无压缩信息的 file 输出 → 报明确错误而非静默 trap 退出）。
`patch.sh` §3 的两条管道与 §6 的 `.bak` 检查（见 §5 重编注）都受同一教训约束：
set -e 下不要用 `grep … && fail`，用 if。

## 5. 上机包（任务五）

位置：`~/workspace/speaker-open-xiaoai-rebuild/release/`（仓库外，20 个文件，78MB）。
索引：`docs/RELEASE.md`（进 PR；只写路径、SHA256、核对命令与顺序，不含任何密码/token/证书/密钥）。

| # | 东西 | 包内路径 | 大小 / SHA256 |
|---|---|---|---|
| 1 | LX06 补丁镜像（2026-11-18 重编） | `images/lx06_1.94.13-root-patched.squashfs` | 38,457,344 字节，`7dafb9507bdc4894715f2978ffaf5fd6a11af18ad61ed268f48c6309f0a05e21` |
| 2 | OH2P 补丁镜像（2026-11-18 重编） | `images/oh2p_1.58.6-root-patched.squashfs` | 30,179,328 字节，`fe2bab99eb15e423b689c24bace6fec1b665b1073dc12ec1c9fa2e4433b5c926` |
| 3 | Hub 二进制 | `deploy/hub-linux-amd64` | 12,093,544 字节，`973a3b1fa2d34acab7a1feaa5c6fb146bc01167735b4c50ba408d1acd0213091`（2026-11-18 从分支尖 afcdcf0 的干净 HEAD worktree 重编；清单 `deploy/hub-linux-amd64.sha256`） |
| 4 | 部署包 | `deploy/`（docker-compose.yml / Dockerfile.hub / .dockerignore / snapserver.conf / .env.example / nas-install.sh / NAS_CHECKLIST.md / README.md） | — |
| 5 | kws-sidecar 构建源 | `kws-sidecar/`（Dockerfile / server.py / entrypoint.sh / requirements.txt / scripts/） | — |
| 6 | 刷机 runbook | `FLASH_RUNBOOK.md` | — |
| 7 | NAS 检查单 | `deploy/NAS_CHECKLIST.md` | — |
| 8 | 回滚检查单 | `ROLLBACK_CHECKLIST.md`（本包内，runbook §6.2 的单页版） | — |

### 5.1 镜像 2026-11-18 重编（协调员定夺）

固件面工人按任务“本轮不重编”把镜像过期三缺口（P1-36 inittab、P2-33 .bak、P2-34 包装器）
留作定夺项；协调员定夺为**重编**（RELEASE.md §0 二选一里的 (a)）。理由：缺口 1 是
物理接触串口即可进 root shell（P1-36），接受它再刷等于明知故犯；上机包的用途就是
“刷机那天不用现找东西”，一个自带 FAIL 的包不是可用的包。重编不改任何已定决策，
不产生新密码（见下）。

- 重编方法：`patch.sh lx06 1.94.13` + `patch.sh oh2p 1.58.6`（现版脚本），密码用一次性
  随机值走 `SSH_PASSWORD_FILE`（文件在 `/tmp`，用完即删，不进任何日志/报告）；随后把两份
  新镜像的 shadow root 行**换回旧镜像的 `$6$` hash**（逐字节比对验证一致），再用与
  `patch.sh` 完全相同的参数重打包（`mksquashfs -comp xz -b 131072 -noappend -all-root
  -always-use-fragments -no-xattrs -no-exports`）。结果：**root 密码未换**，
  bai 手里已有的密码继续有效；未生成、未记录、未传递任何新密码。
- 同步位置：`FLASH_RUNBOOK.md` §1.1（含过期注改写）、`flash-check.sh`
  `KNOWN_PATCHED_SHA256`（另把三处写死的“2026-11-17 实测值”字样改为不带日期的“记录值”）、
  `docs/RELEASE.md` §0–§2、`release/images/`。旧 SHA（LX06 `7266aba9…d97b3`、
  OH2P `139de363…6415`）已作废。
- 重编中修掉的两个管线缺口（都是重编时实测触发的，不是顺带扩范围）：
  1. `patch.sh` 从不传 `--cert-dir`：2026-11-17 的首版镜像是绕过 `patch.sh`、直接调
     `patch_firmware.py --cert-dir` 打的；直接跑 `patch.sh` 会静默产出无 mTLS 证书的镜像。
     现 `patch.sh` 默认用 `work/certs` 装入（`CERT_DIR` 可覆盖），目录不存在直接失败，
     仅测试可设 `PATCH_NO_CERTS=1` 跳过；`flash-check.sh` §4b 新增证书核对
     （ca.crt/client.crt 存在、client.key 0600，缺失即 FAIL）。
     回归测试：`test_patch_sh_requires_cert_dir`；e2e 测试改设 `PATCH_NO_CERTS=1`。
  2. `patch.sh` §6 验 `.bak` 用 "slave.pcm Capture" 标记法，OH2P 原厂 asound.conf 里就有
     该行（mico_record/noop 出厂即走 Capture），重编 OH2P 时误杀致 `patch.sh` exit 1。
     现改为与原始 root.squashfs 逐字节比对（“备份即原厂”的本来定义，与型号无关）。
     附带发现：OH2P 解包出的 `work/oh2p_1.58.6/rootfs` 是 2026-11-17 被就地打过补丁的
     （asound.conf 已含补丁内容），先用 `unpack.sh oh2p 1.58.6` 重新解出原厂 rootfs 才重编；
     LX06 的 rootfs 是原厂态，未受影响。
     回归测试：`test_oh2p_factory_asound_has_capture_marker_and_bak_matches`。
- OH2P 官方回滚包：`work/oh2p_1.58.6.bin` 已拷入 `firmware/oh2p_1.58.6.bin`，
  SHA256 `1f351d879c06ff3bd7bd4b4c68e2c8fa45eee1a82be5839e57899ae8f9b7c27b`、
  36,042,824 字节，与白名单一致；`firmware/` 里现 LX06/OH2P 官方包齐全。

## 6. 全量测试重跑输出（协调员最终重跑，2026-11-18）

- `go test -count=1 ./...`（hub/）：22 包全部 ok、**270 passed**、0 FAIL；
  `go vet ./...` 干净；`gofmt -l cmd/ internal/` 干净。
  （注：Hub 面工人写的“25 个包”是误数——`go test ./...` 的 ok 行共 22 个：
  根包 + cmd/hub + 20 个 internal 包。）
- `cargo test`（client/）：**96 passed**、0 failed；`cargo test --features sherpa-kws`：
  **101 passed**、0 failed；`cargo fmt --check` 干净；`cargo clippy --all-targets` 0 warning。
- `python3 -m pytest firmware-patch/tests/ -q`：**86 passed**（基线 68；本轮新增 18 项：
  固件面 16 项 + 协调员 2 项——`test_patch_sh_requires_cert_dir`、
  `test_oh2p_factory_asound_has_capture_marker_and_bak_matches`；
  另 `test_flash_check_reports_stale_shipped_image` 已按其 docstring 预定改回“通过”期望，
  更名为 `test_flash_check_passes_on_shipped_image`）。
- `kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q`：**55 passed**（基线 25 + 30 新增）。
- `bash scripts/lint.sh`：**全部通过**（规则 1 接线 40 项、规则 2 nil 守卫、规则 3 bounds、
  semgrep、Rust vec! 检查、cargo fmt/clippy、bash -n、golangci-lint）。
- `go test -race -count=1 ./internal/kws/`：**20 遍 0 FAIL**。
  - Hub 面工人复现：16 遍 4 次 FAIL，全是 TestHelloTokenEnforced（kws_test.go:281）——
    不是数据竞争，是测试断言与客户端状态翻转的时序竞态（serve() 写完 hello 即置
    connected=true，不等边车回 ready；假边车先把 rejected 计数 +1 再发 error+1008）。
    协调员定夺：**修测试侧**——TestHelloTokenEnforced 改轮询等 Connected 归零（3s 超时），
    生产不动（握手关闭期间的瞬时 Connected=1 是异步关闭的固有行为，且状态终会归零；
    生产侧“收到 ready 才置位”会改变 Connected 语义，还会掩盖“流活着但未就绪”的状态）。
    修复后 61 遍 1 次 FAIL（首批第 8 遍，测试名未捕获），随后 51 连过。
  - 协调员最终重跑 20 遍又抓到第二个 flake：TestReconnectAfterDrop（1 次 FAIL）——
    根因是 N-P2-3 的重连排空放在发 hello 之后，测试收到 hello 后发的实时帧若落在排空之前
    会被误当陈旧帧丢掉。修复：**生产侧把排空移到发 hello 之前**（hello 是对端可观测的
    “已重连”信号，排空必须先于它；排空语义不变，只排断线期积压帧，首次连接仍不排空）。
    修复后 20 遍 0 FAIL。
  - 观察项：TestArbiterWindow 既存 timing flake（Hub 面工人记录，1/5 FAIL，arbitration 未动），
    与本轮改动无关，不修。
- `flash-check.sh` 四路（lx06 / oh2p × 默认 / `--strict`）：**全部 exit 0**，无 WARN。
- Hub 二进制冒烟（重编后，见 §5）：`--help` exit 0；弱 Token 启动即 Fatal exit 1；
  设强 Token 后启动，`/api/status` 无 Token 返回 401、带 Web Token 返回 200
  （`kws_status: down` 是本机无边车的预期结果）；`ldd` 只依赖 libc/libm。

## 7. BLOCKED 清单

| # | 项 | 原因 / 建议 |
|---|---|---|
| 1 | P2-12 紧急呼叫二次确认 | 需 bai 拍板。加确认会延迟真实紧急呼叫的通知（用户说一次“救命”后可能无法再说话确认），是安全性权衡，只有 bai 能接受；确认方式（语音确认措辞、窗口、重复求救是否算确认）也是产品决策。建议方案：首次触发 → 问“确认要呼叫急救吗？请说‘确认呼叫’”，60s 窗口内“确认/是的/确认呼叫”或再次触发求救词即确认执行，“取消”撤销，超时无确认则不通知。FR-15 触发语义（触发词本身）保持不变。 |
| 2 | P2-35 OTA 禁用残留路径 | 只能真机核实；VM 上能做的是 patch.sh §6 验证三处已知禁用点（已做，见 §1.4 P2-6）。 |
| 3 | P2-42 残留：hub_url 默认 `wss://192.168.1.10:4399` | 需 bai 拍板。彻底去掉需改配置格式（hub_url 必填），属 breaking change：旧 client.toml 缺该字段即启动失败。未改的理由：Token 默认已空、指纹必填，错配的默认 Hub 无法鉴权，危险部分已被 fail-closed 兜住。 |
| 4 | gnueabihf 脚本对不兼容 vendor `.so` 只警告不失败 | 需 bai 拍板（或第三轮审计定）。未改：该 `.so` 已知不可用是 P1-40 遗留状态，改失败会断掉当前能跑通的默认 server 模式构建。 |
| 5 | HA TLS | 悬而未决（P1 轮遗留）：config 已改 `wss://192.168.1.10:8123`，需 bai 确认他的 HA 是否配了 TLS，否则改回 `ws://`。 |
| 6 | P1-40 路由确认 | 悬而未决（P1 轮遗留）：长期 server 模式（按 bai 早先“哪个稳选哪个”的委托已定），待 bai 确认。 |
| 7 | 真机项 | 刷机、NAS 部署、HA 联调、真人唤醒率（1m/3m ≥95% 目标）、“你好”+1s 停顿+“小李”是否算误唤醒、双机仲裁、打断率、Opus 回声消除率（dB）与 `AEC_REF_DELAY_FRAMES = 4` 的真实声学延迟校准、P2-N1 的 `snd_pcm_drop` 真机效果、P2-40 的 ALSA 启动瞬间退避恢复、SC-P2-2 的 `rms_peak` 仲裁实际效果、SC-P2-4 的断开 flush 真机触发率、SC-P2-1 的 `is_ready` 上限真机触发、Dockerfile.hub 首次构建（本机无 docker，NAS 上首次构建为验证关口）、P2-1 的真机指纹同步——均未验证。 |
| 8 | gosec 不启用 | 评估完成（14 findings，6 项生产代码需 Hub 面逐项裁决：G115×3 wyoming.go×2 + kws.go×1 的 int16/uint16 转换；G402×2 transport.go TLS MinVersion 未设、kws.go InsecureSkipVerify；G404×1 kws.go math/rand jitter），启用会立即红 CI；不在本轮 scope，第三轮审计时再定。 |
| 9 | P2-29 代码层残留 | ProbeModel 仍用 `Version: "probe"` 覆盖真实版本号，无生产调用方（P1-44 已删自动扫描）；是否删除死代码由第三轮审计定。 |
| 10 | `alerts.Watcher` 无 `Stop` | 观察项（非 BLOCKED）：关停时靠 `haClient.Close()` 断开 SSE 让监视随之结束；如需显式停止语义，第三轮审计时再定。 |
| 11 | P2-10 / P2-21 本地 KWS 残留 | server 模式下生产不走本地 KWS；本地 KWS 代码保留作未来选项（P1-40 路由确认后定）。 |

## 8. 协调员取舍摘要（替 bai 做的决定）

| 决定 | 取舍 |
|---|---|
| 镜像重编（§5.1） | 选重编，不接受“串口免密”缺口豁免。代价：SHA 全部变了，4 处文档同步更新；收益：上机包自带 flash-check 全绿，刷机那天不用先定夺。 |
| root 密码不换 | 重编沿用旧镜像的 `$6$` hash，不生成新密码。代价：无（bai 手里的密码继续有效）；收益：不用走密码交付流程，不产生新秘密。 |
| kws flake 修测试侧（TestHelloTokenEnforced） | 不改生产语义（Connected=“hello 已发”改为“边车已就绪”会掩盖“流活着但未就绪”的状态）。代价：测试多一次轮询；收益：生产行为零变化。 |
| kws flake 修生产侧（TestReconnectAfterDrop） | 排空移到发 hello 之前。代价：改了一行生产代码的顺序（语义不变——排空的仍是断线期积压帧）；收益：hello 成为真正的“可安全发帧”信号，测试与生产都不再竞态。 |
| P1-4：host 网络保留 | 见 §2。代价：无新增隔离；收益：零回归风险（kws-sidecar/Hub/snapserver/mpd 的寻址都不动）。 |
| P1-4：user: 只给 Hub 与边车 | 见 §2。代价：其余 5 个服务仍 root（cap_drop 兜底）；收益：不逐镜像验证上游数据目录属主。 |
| P1-4：read_only 暂缓 | 见 §2。代价：容器文件系统仍可写；收益：不做无法在无 NAS 时验证的 tmpfs 编排。 |
| P1-4：digest 全部钉死 | 见 §2。代价：上游发新版时需手动更新 digest；收益：部署可复现，不会被 latest 偷换。 |
| P2-31 不销项 | 见 §4。代价：多一次修复 + 回归测试；收益：失败分支真正可达，静默退出的坑被堵死。 |
| Opus 回声接客户端 | 见 §3。代价：Hub 侧 `EchoCancel` 仍是生产零调用（留待以后有参考对齐方案时再议）；收益：回声消除发生在唯一握有双路同钟信号的地方，逻辑已验证。 |
| SC-P2-5 字符集含 `.` | 与报告建议 `[A-Za-z0-9_-]` 有出入（见 §1.3）。代价：device_id 允许点；收益：不破坏既有协议（既有测试的 device_id 含点），回归测试已钉住。 |
| gosec 不启用 | 见 §7 第 8 项。代价：14 项发现暂不处理；收益：CI 不立即变红，6 项生产发现留待第三轮审计逐项裁决。 |
| patch.sh 默认装证书 | 见 §5.1。代价：无证书目录时 patch.sh 直接失败（需先跑 gen-certs.sh）；收益：再也不会静默产出无 mTLS 证书的镜像。 |
