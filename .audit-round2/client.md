# open-xiaoai-next 第二轮审计 — 客户端（Rust）面

- 审计日期：2026-11-17（晚）
- 审计对象：`~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/client/`（`src/` 全部 6 个模块 + 两个构建脚本 + `kws/vendor-arm/`），分支 `audit-round2-ci-fix`
- 审计员：客户端面（只审计、不修代码；未修改任何源代码，未推 GitHub，未动 main 分支，未动 firmware/ 原厂固件；报告中不含任何密码）
- 对照：首轮 `AUDIT_REPORT.md`（2026-11-17 13:19，108 项 P0 6 / P1 46 / P2 56）、`PLAN.md` v4、Hub 侧现状（仅为给客户端结论提供上下文而 grep，不审计 Hub 代码本身）

## 审计范围

- 客户端全部 Rust 源码：`src/main.rs`（880 行）、`src/transport.rs`（1315 行）、`src/audio.rs`（732 行）、`src/kws.rs`（557 行）、`src/sherpa.rs`（721 行）、`src/config.rs`（302 行），共 4507 行。
- 构建脚本：`build-armv7-gnueabihf.sh`（默认交付，方案 B）、`build-armv7.sh`（musl fallback）。
- 首轮之后的新代码（重点审）：`kws_mode="server"` 默认模式（持续推 Opus 流）、`init_kws` 分流、`Session::new_server`、本地 KWS 代码路径保留状态、vendor-arm 的两个 `.so` 现状。
- Hub 侧仅作 grep 级核对（对讲触发、`start_play` 发送、Ping 发送、`EchoCancel` 调用方），用于判定 P1-15 / P1-16 的客户端可见部分；Hub 代码本身不在本报告审计范围内。
- 排除：`firmware-patch/work/`（原厂固件）未审；真机验证全部 BLOCKED（本 VM 无 `/dev/snd`、无音箱硬件）。

**分支说明**：`git log main..HEAD` 为空、`git diff main --stat -- client/` 为空——本分支与 `main 509e57a` 在 client/ 上无差异。首轮之后客户端的新代码（server 模式、gnueabihf 切换）已在 main 里，本报告审计的就是当前 main 的客户端代码。

## 方法

逐文件读代码、追踪数据流与并发路径、grep 交叉验证调用点；基线测试实跑（见下）；文档与代码冲突时以代码为准；真机才能验证的项一律记 BLOCKED，不写成通过；合成语音测试标合成；找不到证据写“未确认”，不猜。

## 基线测试结果（实跑，2026-11-17 晚）

在 `client/` 内执行，`source env.sh`，`TMPDIR=~/workspace/.cargo-tmp`：

| 命令 | 结果 |
|---|---|
| `cargo test` | **53 passed；0 failed**（首轮报告引用 46 passed；新增的 server-mode 测试等使总数上升） |
| `cargo test --features sherpa-kws` | **57 passed；0 failed**（首轮引用 50 passed） |
| `./build-armv7-gnueabihf.sh` | **exit 0**；全部断言 `ASSERT OK`（ARM EABI5 硬浮点动态链接、解释器 `/lib/ld-linux-armhf.so.3`、sherpa 标记 3 个、GLIBC 最高 2.25、RUNPATH 含 `$ORIGIN/lib`、NEEDED 5 个库均在音箱 rootfs 内、vendor `.so` 含 10 个必需导出符号）；但两处 GLIBC 警告仍在输出里（见 P1-40） |

产物（本次重建）：`target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client`，**2,444,332 字节**，SHA256 `e90c3058cb29cc9faa6fe304cea2c1ad265403c68cf6b3743a5e8f5efc0161ff`。

注意事项（诚实红线）：

- `cargo test --features sherpa-kws` 的 57 通过里，**真实模型端到端测试没有真正执行**：`sherpa_detects_nihaoxiaoli_tts_sample` 与 `sherpa_open_missing_model_reports_clear_error` 因未设 `SHERPA_KWS_TEST_*` 环境变量而打印 `SKIP` 后返回 ok（实测确认）。即：57 通过 ≠ 模型已验证；sherpa 真实检出仍只在首轮的合成语音冒烟里验证过（合成，非真人）。
- 本次重建产物与 MEMORY 记录的上一版产物不一致：上一版 2,435,964 字节、SHA256 `2da754d4…7a00b`；本次 2,444,332 字节、SHA256 `e90c3058…61ff`。源码树与 main 无 diff（见上），同一脚本重建却字节不同——构建不可字节复现，或上一版产物出自不同环境/不同树状态。原因未确认（见新 P2-N8）。
- 全部真机项 BLOCKED：ALSA 真机、真人唤醒率、ARM 上 `.so` 加载、双机仲裁、播放/对讲端到端，均未验证。

## 新问题总览

| 分级 | 数量 |
|---|---|
| **P0（新）** | **1** |
| **P1（新）** | **3** |
| **P2（新）** | **6** |
| 合计新问题 | 10 |

---

## P0 级新问题（置顶）

### P0-N1 录音任务与会话各持一个 `Audio`，两个实例各打开两路 ALSA `hw` 设备：第二路打开大概率 EBUSY，播放可能全灭

- **位置**：`client/src/main.rs:316`（录音任务 `Audio::open()`）与 `client/src/main.rs:375`（每个会话 `Audio::open()`）；`client/src/audio.rs:420-424`（`Audio::open_devices` 在 `alsa` feature 下总是同时打开采集与播放）；`client/src/audio.rs:218-260`（`AlsaAudioDevice::open_devices` 两路各开各的，失败只记录、不整体失败）。
- **证据**：
  - 录音任务的 `Audio` 实例在进程生命周期内一直活着（`main.rs:309-355` 的 `tokio::spawn`），它调用 `Audio::open()` → `open_devices("hw:0,0", "hw:0,2")`，同时打开了采集 `hw:0,0` 和播放 `hw:0,2`——播放这路它永远不用。
  - 每个会话（每次重连）又 `Audio::open()` 一次（`main.rs:375`），再次打开 `hw:0,0` 与 `hw:0,2`。
  - `hw` 设备是独占的：同一 PCM 子设备已被打开时，第二次 `snd_pcm_open` 返回 EBUSY。`open_devices` 把失败只记进 `playback_err`（`audio.rs:231-238`），`Audio::open()` 仍返回 Ok（构造器永不因缺硬件失败）。
  - 后果链：会话的 `Audio` 的 `playback` 为 `None` → 每次 `play_frame` 都返回 `"ALSA playback unavailable: device hw:0,2 open failed: ..."`（`audio.rs:347-353`）→ `run_session` 的 `RawFrame::Audio` 分支只 `warn!("playback frame failed: {e}")`（`main.rs:447-461`），TTS/对讲/提醒音频**全部静默丢弃**，且每帧一条 warn 刷屏。
  - 同理采集路：会话的 `Audio` 的 `capture` 也会 EBUSY，但会话的实例本来就不用采集，所以采集本身不受影响；受害的是播放。
- **为什么是 P0**：如果成立，首关口（刷一台 LX06 + Hub + 同步一首歌/播一句回答）中“播”这一半在真机上是全灭的，且失败是静默的（进程正常跑、正常连 Hub、日志只有 warn）。修复极便宜：`Audio` 只打开需要的方向（录音任务只开采集，会话只开播放），或两路共享一个 `Audio` 实例。
- **不确定性（诚实红线）**：`hw:0,2` 是否只有一个子设备、第二次打开是否确实 EBUSY，**需真机确认，BLOCKED**。若该设备有多个子设备、第二次打开拿到子设备 1，则本项降级为 P1（资源浪费 + 抢占风险）。在真机确认前不要把本项当作“已证实全灭”，但也不要当作不存在——代码结构上的双开是确定的，独占语义是 ALSA `hw` 设备的标准行为。
- **修复建议**：`Audio::open_devices` 拆成按需打开（`open_capture_only` / `open_playback_only`），或把 `Audio` 实例提到重连循环外由录音任务与会话共享（共享需注意 `Audio` 不是 `Sync`，用 `Mutex` 或消息传递）；并补一个测试：两个 `Audio::open()` 实例共存时，第二个实例的 `play_frame` 必须成功（Fake 设备测结构，真机测 EBUSY）。

---

## P1 级新问题

### P1-N1 `set_volume` 只改内存数字、从不碰 ALSA 混音器：Hub 以为调了音量，实际响度永远不变，客户端还回 `ok:true`

- **位置**：`client/src/main.rs:181-229`（`handle_rpc` 的 `"set_volume"` 分支只做 `self.volume = v`）；全仓 grep `Mixer|mixer` 在 `client/src/` **零命中**；音量唯一消费点是 30 秒 `send_status` 上报（`main.rs:503`）。
- **证据**：`set_volume` 分支（`main.rs:213-224`）取 `params.volume` 后只写 `self.volume`、记一条 info 日志、返回 `(true, None)`。没有任何代码调用 ALSA 混音器（`alsa::mixer`）、也没有任何代码把 `self.volume` 传给播放路径。`Audio::play`（`audio.rs`）不收音量参数。
- **影响**：Hub 发 `set_volume` 后收到 `ok:true`，Web/状态页显示的音量也变了，但音箱实际响度纹丝不动。用户说“小声点”会得到一句“好的”加毫无变化的音量——功能性撒谎，比直接报错更难排查。
- **修复建议**：二选一：(a) 真正接 ALSA 混音器（打开 `hw:0` 的 Master/PCM 混音元素，按 0–100 映射，失败回 `ok:false`）；或 (b) 明确不支持：在 `set_volume` 回 `ok:false, error:"volume control not implemented"`，并把该 RPC 从协议白名单文档里摘掉。不要保持“回 ok 但什么都不做”。

### P1-N2 `start_recording` / `stop_recording` RPC 是空操作却回 `ok:true`：local 模式下对讲发送端是断的

- **位置**：`client/src/main.rs:193-200`（`"start_recording"` 分支只有一条 `info!` 日志；`"stop_recording"` 同理，只记日志）。
- **证据**：两个分支都不改任何状态、不发任何动作，返回 `(true, None)`。而录音任务向 Hub 推音频帧的唯一门是 `session.streaming`（`main.rs:483-495`），`streaming` 只在 `WakeWin` / `Interrupt` / server 模式建立时改变。于是 local 模式下：Hub 调 `start_recording` 想让发起方开始发对讲音频时，客户端照常不推流（除非恰好在 20 秒连续对话会话内）——对讲发起方发不出声音。server 模式下 `streaming` 恒为 true，本项被掩盖（对讲能发是靠持续推流，不是靠这个 RPC）。
- **影响**：P1-15 的触发接通后，local 模式对讲仍是单向的（只能收不能发），且 Hub 收到 `ok:true` 以为录音已开始。首轮报告只写了“Hub 无触发入口”，客户端这半边的空操作是新发现的另一半。
- **修复建议**：`start_recording` 应置一个独立的 `recording` 标志（与 `streaming` 分开，或直接置 `streaming = true` 并在 `stop_recording` 时恢复），推流门改为 `streaming || recording`；`stop_recording` 清标志。补测试：收到 `start_recording` 后无 `wake_win` 也能向 Hub 推帧。

### P1-N3 P1-40 的剩余路径文档是错的：`libonnxruntime.so` 并不“只要求 GLIBC_2.4”，它要求 GLIBC_2.34——方案 B 的“复用官方 ORT”前提已证伪

- **位置**：`client/kws/vendor-arm/SOURCE.md`（“`libonnxruntime.so` 本身只要求 GLIBC_2.4，没问题；卡的是 sherpa 那层”；出路 (b) 写“复用本目录官方 `libonnxruntime.so`”）；`client/src/sherpa.rs` 头注释（仍写“当前 ARM Client 是 musl 静态链接，`dlopen` 本身就不被支持”“真机唤醒走过渡路线：参考 kws 二进制 sidecar”——与已落地的 gnueabihf 切换和 server 模式都已脱节）。
- **证据**（本次实测 `readelf -V`）：
  - `libsherpa-onnx-c-api.so`：GLIBC 最高 **2.34**、GLIBCXX 最高 **3.4.29**。
  - `libonnxruntime.so`：GLIBC 最高 **2.34**（含 2.27/2.28/2.29/2.32/2.33/2.34）、GLIBCXX 最高 **3.4.29**。
  - 音箱上限：glibc 2.25、libstdc++ 3.4.22。两个 `.so` 都超标，不只是 sherpa 那层。
  - 首轮 AUDIT_REPORT.md 的 P1-40 一节已写明“‘ORT 只要求 GLIBC_2.4’是错的，已实测纠正”，但 `SOURCE.md` 与 `sherpa.rs` 头注释**没有同步更新**，仍保留被证伪的前提。
- **影响**：按 SOURCE.md 的出路 (b) 去做，会做到一半才发现 ORT 也要从源码重编——工作量评估错误（只重编 sherpa 层 vs 两层全重编）。文档与代码/实测冲突时以实测为准，但决策者读的是文档。
- **修复建议**：更新 `SOURCE.md`：ORT 同样要求 GLIBC_2.34 / GLIBCXX_3.4.29，方案 B 的正确表述是“ORT + sherpa 都需从源码重编（或 KWS 继续放 NAS 边车）”；同步修正 `sherpa.rs` 头注释里 musl 与 sidecar 的过时现状（sidecar 路线已在 `kws.rs` 头注释里标明作废，两处说法应统一）。

---

## P2 级新问题

### P2-N1 `interrupt` 只清 `playing` 标志、不停 ALSA：ALSA 缓冲里已写入的 80ms 尾音仍会放完

- **位置**：`client/src/main.rs:524-527`（`SessionAction::StopPlayback` 只 `info!`，不碰 `audio`）；`client/src/main.rs:144-152`（`HubMsg::Interrupt` 只置 `playing = false`）。
- **证据**：`apply_action` 的 `StopPlayback` 分支没有任何停止播放的动作；`run_session` 里 `playing=false` 只是让后续收到的音频帧被丢弃（`main.rs:447` 的门）。已写入 ALSA 的缓冲（4 周期 × 20ms = 80ms，`audio.rs` 的 `ALSA_BUFFER_PERIODS = 4`）会继续放完；且 `play_frame` 是同步写，正在写的一帧（20ms）不会被打断。
- **影响**：打断后仍有约 20–100ms 尾音。对“打断成功率 ≥90%”的体感影响小，但 `StopPlayback` 这个名字承诺的是“立即停播”，实际是“下一帧起不再喂”。另：没有任何 `drop`/`flush`/`prepare` 调用能清空缓冲。
- **修复建议**：`StopPlayback` 执行时调 `pcm.drop()`（丢弃缓冲）+ `prepare()`，或把 `Audio` 加一个 `stop_playback()` 方法（ALSA `drop` + `prepare`）；`NoAudioDevice`/Fake 也实现为空操作。补测试：interrupt 后 Fake 设备的 `played` 不再增长且缓冲被清空。

### P2-N2 客户端对入站 WS 帧无大小限制：被攻陷/异常的 Hub 可用超大帧打满 256MB 音箱内存

- **位置**：`client/src/transport.rs:476-502`（`connect_once` 调 `connect_async_tls_with_config(url, None, false, …)` 与 `connect_async(url)`——配置参数传 `None`，即 tungstenite 默认值）。
- **证据**：tungstenite 默认 `max_message_size` 为 64MB、`max_frame_size` 为 16MB，客户端未改小。`next_raw`（`transport.rs:419-448`）对文本帧直接 `parse_hub_text`（整条 JSON 进内存）、对二进制帧直接 `opus.to_vec()` 全量复制。Hub 是受信端，正常情况下无害；但 Hub 一旦被攻陷或出 bug 发超大帧，256MB 音箱无任何防线（对比：首轮 P1-3 只给 Hub 侧提了 `SetReadLimit`，客户端这半边同样没有）。
- **影响**：单条 64MB 文本帧即可让客户端 OOM 被杀；且 `parse_hub_text` 的 `serde_json::from_str` 在超大输入上还会放大内存。
- **修复建议**：`connect_async_tls_with_config` 传入显式 `WebSocketConfig { max_message_size: Some(64 * 1024), max_frame_size: Some(64 * 1024), ..Default::default() }`（音频帧 <1.3KB、文本消息 <1KB，64KB 绰绰有余）；超限即断线重连并记 error 日志。

### P2-N3 `tls=false` 配 `wss://` URL 时悄悄走系统根证书 TLS：指纹被绕过，与“tls=false = 纯 ws”自述矛盾

- **位置**：`client/src/transport.rs:460-475`（`connect_once` 只在 `tls=true` 时把 `ws://` 升级为 `wss://`；`tls=false` 时原样用 `cfg.hub_url` 调 `connect_async`）。
- **证据**：`tls=false` + `hub_url = "wss://…"` 时，URL 原样进 `connect_async`，而 tokio-tungstenite 按 scheme 决定是否 TLS——`wss://` 会握手 TLS，用的是 `rustls-tls-native-roots` 的系统根证书，**不走** `FingerprintVerifier`，也不走 `build_tls_config`。`Config::validate`（`config.rs:159-169`）只校验 `kws_mode`，不校验 `tls` 与 URL scheme 的一致性。
- **影响**：操作员以为关了 TLS（或以为指纹在保护），实际走了另一套 TLS 验证（自签 Hub 证书会握手失败，报错信息还指向系统根证书，误导排查）；反向组合（`tls=true` + `ws://`）有升级逻辑，唯独这向没处理。
- **修复建议**：`validate()` 里校验：`tls=false` 时 `hub_url` 必须以 `ws://` 开头，否则启动报错；或 `connect_once` 里 `tls=false` 且 URL 为 `wss://` 时明确报错而非静默走另一条路。

### P2-N4 Opus 编码失败被静默吞掉：编码器一旦出问题，客户端无声无息地断流

- **位置**：`client/src/main.rs:339`（`if let Ok(opus) = audio.encode_pcm(&pcm)`，Err 分支无任何处理、无日志）。
- **证据**：捕获任务里编码失败时 `if let Ok` 不匹配即直接跳过该帧，不记日志、不计数。编码器是长期持有的有状态对象（`audio.rs` 的 `OpusCodec`），一旦进入错误状态（如内部状态损坏），每帧都失败、每帧都静默丢弃，Hub 侧只看到流断了、客户端日志里什么都没有。
- **影响**：可恢复的偶发错误与持续性编码器故障无法区分；排障时无任何线索。
- **修复建议**：Err 分支记一条节流 warn（沿用 P2-20 的节流思路，1 秒一条 + 计数）；连续失败 N 次（如 50 次）后记 error 并重建 `OpusCodec`（或让录音任务按 P2-40 的退避重启）。

### P2-N5 录音任务无背压丢弃策略：`audio_tx`（容量 32，约 640ms）满时反压会堵住采集，引发 ALSA overrun

- **位置**：`client/src/main.rs:312`（`mpsc::channel::<Vec<u8>>(32)`）；`main.rs:330-343`（捕获任务：`capture_pcm` → `encode_pcm` → `audio_tx.send(opus).await`，全在同一个任务里串行）。
- **证据**：`audio_tx.send().await` 在通道满时挂起等待，而 `capture_pcm` 是阻塞读（约 20ms 一帧，`audio.rs`），发送挂起期间不读 ALSA——ALSA 采集缓冲（80ms）很快 overrun，下一次读到 XRUN（`audio.rs` 的 capture XRUN 处理最多重试 2 次后返回错误，录音任务随即退出，见 P2-40）。即：一次 WS 抖动/Hub 慢消费会通过背压链把录音任务杀死。
- **影响**：Hub 端一次 GC/慢处理（>640ms）即可让音箱永久失聪（录音任务退出不重启），且日志里只看到 XRUN，看不到真正的背压原因。
- **修复建议**：发送改 `try_send`，满时丢最旧帧（或丢当前帧并计数），永不阻塞采集；或把编码与发送拆到不同任务，中间用有界丢弃队列。并在日志里记 `dropped_frames` 计数。

### P2-N6 `hello_ok` / `rpc` 解析过于宽容：缺字段时静默接受空值

- **位置**：`client/src/transport.rs:347-380`（`parse_hub_text`：`hello_ok` 的 `device_id` 缺失时 `unwrap_or("")`；`rpc` 的 `id`/`method` 缺失时同样 `unwrap_or("")`）。
- **证据**：`connect_once`（`transport.rs:531-544`）接受 `device_id == ""` 的 `hello_ok`，`Transport.device_id = Some("")`，会话照常建立；之后 `send_rpc_result` 会带着 `id: ""` 回复，Hub 侧无法匹配。`HubMsg::Rpc` 的 `method == ""` 会走 `handle_rpc` 的 `other` 分支回 `unknown rpc method: `（空方法名），日志无有效信息。
- **影响**：协议错配/Hub bug 时客户端不报错、不拒绝，静默跑在一个半握手状态；排障时 `device_id=""` 的会话难以定位。
- **修复建议**：`hello_ok` 缺 `device_id` 或为空时 `connect_once` 直接报错断线；`rpc` 缺 `id`/`method` 时记 warn 并丢弃（不回 `rpc_result`，或回 `ok:false` 并注明缺字段）。

---

## 必查项逐项结论

### 1. 播放状态机 `playing` latch bug —— 仍在（P1-16 全貌未动）

- **结论：仍在。** `client/src/main.rs:447` 的 `RawFrame::Audio` 分支仍是 `if session.playing { play }`，else 分支**连日志都没有**——裸音频帧在 `playing == false` 时被静默丢弃（与首轮 `main.rs:358-375` 同款行为，行号后移）。
- `play_text` RPC 仍置 `playing = true` 且无任何过期/复位（`main.rs:201-211`）；`stop_play` 是唯一的复位路径（`main.rs:190-192`），而 Hub 侧核对（grep `hub/cmd/hub/main.go`）：`onReply`（语音回答）发 `start_play` 后循环发帧、**仍不发 `stop_play`**（`main.go:476` 发 `start_play`，随后立即清 Hub 自己的 `playing` 标志）；`speakText`（提醒/告警，`main.go:520-536`）与对讲中继（`main.go:109-122` 广播、`main.go:375-380` 的 `intercom.New` relay）**仍是裸 `SendBinary`，全程不发 `start_play`**。
- 因此三条 P1-16 的子结论全部仍成立：(a) 对讲/提醒/告警的音频帧到客户端时 `playing` 多半为 false，被静默丢弃；(b) 一次正常回答后客户端 `playing` 永久 latch 为 true（之后 `status` 上报永远 `playing:true`，且之前丢弃的对讲帧会“恰好”能播，造成时有时无的假象）；(c) Hub 的 `playing` 在发完网络帧瞬间即清零，`OnWake` 的“播放中打断”判断在真实播放期间几乎恒为 false。
- 唯一变化：`interrupt` 在 server 模式下只停播不停流（`main.rs:144-152`），语义正确；但这不修复 latch。

### 2. P1-15 对讲：触发入口、start_play、超时清理 —— 仍缺失；Opus 级回声接线 —— 仍为零调用

- **触发入口**：Hub 全仓 grep，`intercom.Start` / `StartBroadcast` 在生产代码里**零调用**（只有 `intercom.go` 里的定义与注释）；`router.go` 无“呼叫/对讲”分支。客户端侧：`start_recording` RPC 是空操作（见新 P1-N2），`send_room_claim` 仍 `#[allow(dead_code)]`、仍无调用方（`transport.rs:516`，`main.rs` 全文件无调用）——P1-39 同样仍在。
- **超时清理**：`intercom.Sweep` 在生产代码里**零调用**（grep `hub/` 仅定义）；`main.go` 无定时调用。目标方断线/会话泄漏问题仍在。
- **Opus 级回声（EchoCancel）**：`hub/internal/intercom/intercom.go:136` 的 `EchoCancel` 在生产代码里**零调用**（grep 全仓仅定义一处）；Hub 的 `OnAudioFrame` 在对讲路径里不做任何回声消除。客户端侧无任何 AEC 代码（也不应有——回声消除按设计在 Hub 侧）。结论：仍为零接线。

### 3. `kws_mode="server"` —— 已落地且行为与文档一致；local 模式 fail-loud 回归仍在

- **无本地后端不退出**：`init_kws` 在 server 模式直接 `Ok(None)`（`main.rs:241-244`），不要求模型文件、不要求 `.so`；测试 `init_kws_server_mode_needs_no_backend` 通过（实跑）。
- **持续推流**：`Session::new_server()` 置 `streaming = true`（`main.rs:95-104`）；`run_session` 开头先发 `audio_start` 再推帧（`main.rs:398-406`）；`wake_win` 不重复发 `audio_start`、`wake_lose` 不停流、`interrupt` 只停播不停流（`main.rs:120-152`）。端到端测试 `server_mode_streams_continuously_without_wake_win` 通过（fake Hub + 合成 1000Hz 正弦波 Opus 帧，10/10 帧到达，`audio_start` 在首帧之前；**合成音频，非真人语音**）。
- **断线重连**：重连后新建 `Session::new_server()`、`run_session` 重新发 `audio_start`（`main.rs:366-406`），流自动恢复。但重连期间的音频帧丢失（无补发），且重连循环本身的问题见 P1-43（仍在）。
- **带宽与背压**：16kbps 目标码率（`audio.rs` 的 `TARGET_BITRATE_BPS = 16000`）、50 帧/秒、持续推流——单台约 16kbps、两台约 32kbps，约 350MB/天（LAN 内）。无静音抑制、无 VAD 门控（设计如此：检出在边车做）。背压问题见新 P2-N5（`audio_tx` 容量 32，满则堵采集）。
- **local 模式 fail-loud 回归仍在**：feature 开时 `Kws::new` 直接 Err（`kws.rs`），`init_kws` 的回退路径调它后经 `?` 传播、进程非零退出（`main.rs:247-258`）；测试 `init_kws_local_mode_refuses_without_backend_when_sherpa_feature_on` 与 `kws_new_refuses_rms_only_when_sherpa_feature_on` 均通过（实跑，`--features sherpa-kws` 57 通过的一部分）。feature 不开时 local 模式仍是纯 RMS（老行为，测试通过）。
- **本地 KWS 代码路径保留状态**：`sherpa.rs`、`kws.rs`（`Kws::new_with_sherpa`、`feed` 的 sherpa 分支）、feature 门控原样保留；`KwsLogMonitor`（sidecar 日志尾随）代码保留但已在 `kws.rs` 头注释标明**作废**（server KWS 落地后不再接线进 `main.rs`）。`build-armv7-gnueabihf.sh` 默认仍开 `--features sherpa-kws`。

### 4. P1-40 现状 —— 审计要求的构建侧已修，但真机唤醒仍 BLOCKED；文档与现状不一致

- **ARM 二进制含 sherpa 标记**：是。本次实跑 `./build-armv7-gnueabihf.sh` exit 0，`ASSERT OK: ARM binary contains sherpa-kws markers (feature enabled)`；产物 2,444,332 字节、ELF 32-bit ARM EABI5 硬浮点动态链接、解释器 `/lib/ld-linux-armhf.so.3`、GLIBC 最高 2.25、RUNPATH 含 `$ORIGIN/lib`、NEEDED 均在音箱 rootfs 内。
- **vendor `.so` 的 GLIBC 警告仍在构建输出里**：是。本次输出仍有两行 WARNING（`libsherpa-onnx-c-api.so requires GLIBC_2.29` 与 `GLIBCXX_3.4.29`）。且本次实测纠正了一点：`libonnxruntime.so` 同样要求 GLIBC 最高 2.34 / GLIBCXX 3.4.29（见新 P1-N3）——两个 `.so` 都不能在音箱（2.25 / 3.4.22）上加载。
- **文档与现状不一致**：`kws/vendor-arm/SOURCE.md` 仍称 ORT“只要求 GLIBC_2.4”且出路 (b) 写“复用官方 `libonnxruntime.so`”；`sherpa.rs` 头注释仍写 musl 客户端不能 dlopen、走 sidecar 过渡。两者都与已落地的 gnueabihf 切换 + server 模式脱节（见新 P1-N3）。
- **结论**：P1-40 的“构建不断言、静默跑 RMS”已修（标记断言 + fail-loud + gnueabihf 动态链接）；但“音箱上本地 KWS 能跑”仍是 **BLOCKED**——本地唤醒在真机上仍是零能力，当前唯一能工作的唤醒路径是 server 模式（NAS 边车）。构建脚本对不可用的 `.so` 只警告不失败（exit 0），绿构建 ≠ 可部署的本地 KWS。

### 5. ALSA 采集/播放接线（hw:0,0 / hw:0,2）、XRUN 处理

- **接线**：`audio.rs` 的 `CAPTURE_DEVICE_DEFAULT = "hw:0,0"`、`PLAYBACK_DEVICE_DEFAULT = "hw:0,2"`，S16LE / 16kHz / 单声道 / 20ms 周期 / 4 周期缓冲（`audio.rs:228-258` 的 `open_pcm`，含 `set_rate_near` 后校验 `rate == 16000`，否则明确报错）。两路独立打开、失败只记录不整体失败；`Audio::open()` 永不因缺硬件失败，`capture_pcm`/`play_frame` 返回带设备名的明确错误（测试 `alsa_open_without_hardware_falls_back_to_clear_error` 通过）。
- **XRUN 处理**：采集与播放的 `EPIPE` 均 `prepare()` 后重试，最多 2 次（`audio.rs` 的 capture/play 循环，`xruns > 2` 即报错）。与首轮一致，未变。
- **新问题**：双 `Audio` 实例双开问题见 P0-N1（本节接线本身没错，错在 `main.rs` 打开了两次）；另 `capture_pcm` 是阻塞读（约 20ms），在独立 tokio 任务里跑，不阻塞主循环（设计正确）。
- **真机实测仍 BLOCKED**：本 VM 无 `/dev/snd`，`hw:0,0`/`hw:0,2` 是否存在、采样率/周期是否被接受、真实 XRUN 行为，均未验证。

### 6. 客户端面其余首轮延期项逐项复查

复查范围：首轮报告里属于客户端面的延期项（Hub/固件/部署侧的项不在本表，见表后说明）。每项一句话状态 + 证据。

| 项 | 状态 | 证据（一句话） |
|---|---|---|
| P0-3 客户端部分（弱 Token / mTLS / 默认 Token） | **已修** | `validate_token` 建连前拒弱 Token（`transport.rs`）、`load_client_identity` + `build_tls_config` 带客户端证书、`default_token()` 为空串（`config.rs`）；测试 `weak_token_rules`、`wss_mtls_with_and_without_client_cert` 通过（实跑） |
| P1-10 指纹可选、`tls=false` 合法 | **仍开** | `hub_ca_fingerprint` 仍 `Option`、无指纹时只 warn 走系统根证书（`transport.rs:321-340`）；`tls=false` 只 warn 照常启动（`main.rs`）；`CheckOrigin` 之外的新 P2-N3 亦属此簇 |
| P1-11 60s 读超时杀连接、无 Ping 保活 | **仍开** | 客户端从不主动发 Ping，只回 Pong（`transport.rs` 的 `next_raw`/`next_msg`）；Hub 侧 grep：`transport.go` 只有 `SetPongHandler` + 60s deadline，无任何发 Ping 的代码（唯一 `PingMessage` 在 `hub/internal/kws/kws.go:578`，是边车链路，不是 4399 链路） |
| P1-14 RMS 取命中帧单帧值 | **仍开** | `kws.rs:207` 每帧覆盖 `last_rms_db`，命中时 `kws.rs:217` 直接取该值上报；无峰值窗口/回溯（注：server 模式下本地 KWS 不跑，本项只影响 local 模式） |
| P1-15 对讲零接线 | **仍开** | 见必查项 §2：触发、Sweep、EchoCancel 均为零调用；客户端 `start_recording` 还是空操作（新 P1-N2） |
| P1-16 播放状态机 latch | **仍开** | 见必查项 §1：`playing` 仍 latch、裸帧仍静默丢弃、Hub 三路仍无 `stop_play`/仍裸发帧 |
| P1-39 房间认领断 | **仍开** | `send_room_claim` 仍 `#[allow(dead_code)]`（`transport.rs:516`），`main.rs` 全文件无调用；Router 侧解析属 Hub 面 |
| P1-40 ARM 二进制无 KWS | **部分修复** | 构建侧已修（标记断言、默认开 feature、gnueabihf 动态链接、fail-loud 保留）；但 vendor 两个 `.so` 均要求 GLIBC 2.34，音箱上本地 KWS 仍 BLOCKED，唯一工作路径是 server 模式；见必查项 §4 |
| P1-43 无限重连 | **仍开** | `connect_with_retry`（`transport.rs:710`）仍无限循环、退避上限 1s、无 jitter、不区分 `hello_reject`（强但错的 Token 也会 1 次/秒永重试）；`main.rs` 主循环在 `connect_with_retry` 期间不监听 Ctrl-C |
| P2-1 指纹校验忽略有效期与主机名 | **仍开** | `FingerprintVerifier::verify_server_cert` 仍忽略 `_server_name` 与 `_now`（`transport.rs:117-128`），只比指纹；过期证书指纹匹配即放行 |
| P2-10 `.so` 路径任意指定、32 位结构体未锚定 | **部分修复** | 32 位结构体尺寸已在测试里钉死（`sherpa.rs` 的 `c_api_struct_sizes_match_c_header` 增加 32-bit 分支：OnlineModelConfig=68、KeywordSpotterConfig=104、KeywordResult=28，含 offsets）；但该分支只在 32-bit 目标下执行，x86_64 实跑从未覆盖，ARM 上仍**未确认**；`kws_lib` 仍任意路径 dlopen，无白名单、无 SHA256 校验 |
| P2-18 `set_volume` 非法值静默重置为 50 | **仍开** | `main.rs:217` 仍是 `.and_then(|x| x.as_u64()).unwrap_or(50)`（另见新 P1-N1：即使合法值也不落硬件） |
| P2-19 非推流时仍每帧编码 Opus | **仍开** | 捕获任务 `main.rs:339` 无条件 `encode_pcm`，`run_session` 非 streaming 时照常 drain 后丢弃（local 模式）；server 模式下 streaming 恒 true，本项不适用 |
| P2-20 KWS 推理失败每帧打 warn | **仍开** | `kws.rs:225` 仍是 `tracing::warn!("sherpa feed failed: {e}")`，无节流 |
| P2-21 `is_ready` 循环无上限 | **仍开** | `sherpa.rs:462` 仍是 `while (self.api.is_ready)(...) != 0 { decode }`，无迭代上限、无超时 |
| P2-40 录音任务死一次就永久失聪 | **仍开** | `main.rs:316-348`：`Audio::open()` 或 `capture_pcm()` 任一出错即 warn 后 `return`，任务退出、不重试；主循环只把 `wake_alive`/`audio_alive` 置 false 继续跑传输（server 模式下还会继续发 `audio_start`、报健康状态，实际零帧——静默失聪） |
| P2-41 每次重连丢音量与会话状态 | **仍开** | 每次重连 `Session::new()`/`new_server()`，`volume` 硬编码 50（`main.rs:84,97`），`Session` 在重连循环内创建、不跨会话保留 |
| P2-42 配置缺省是静默危险默认值 | **部分修复** | Token 默认已改为空串（fail closed，`config.rs`），缺 Token 时建连前即报错；但配置文件缺失/读失败仍静默回退默认配置（`config.rs:181-185` 的 `Err(_) => Ok(Config::default())`，含权限不足），`hub_url` 默认仍是写死的 `wss://192.168.1.10:4399`；`validate()` 仍只校验 `kws_mode`，`model`/`ram_mb`/`hub_url` 无任何校验；无指纹分支的日志仍写“skipping fingerprint check”（实际并未跳过校验，自签证书会握手失败，误导排查） |
| P2-43 日志里出现用户语音文本 | **仍开** | `main.rs:210` 仍是 `info!("rpc play_text: {text}")`，全文进日志 |
| P2-52 musl 构建脚本静默产出无 ALSA 无 KWS 应急版 | **仍开** | `build-armv7.sh` 在 musl libasound 缺失时仍只打印 WARNING 后 `--no-default-features` 继续编、exit 0；另：gnueabihf 脚本对不可用的 vendor `.so` 同样只警告不失败（exit 0），绿构建 ≠ 本地 KWS 可用 |
| P2-54 公开自由函数失败即 panic | **仍开** | `audio.rs:107,118,128` 的 `encode_pcm_frame`/`decode_opus_frame` 与线程本地 `CODEC` 初始化仍用 `.expect(...)`；目前仅测试调用，但签名仍是 panic 语义 |

**复查统计（客户端面延期项）**：共复查 21 项（P0 1 项 + P1 8 项 + P2 12 项）——**已修 1**（P0-3 客户端部分）、**部分修复 3**（P1-40、P2-10、P2-42）、**仍开 17**。另：过渡 sidecar 路线（`KwsLogMonitor`）**已作废** 1 项（代码保留、`#[allow(dead_code)]`、不再接线进 `main.rs`，见 `kws.rs` 头注释；不是首轮审计项，单列）。

**不属于客户端面的首轮项**（本报告未复查，只列出以免误以为遗漏）：P0-1、P0-2、P0-4、P0-5、P0-6、P1-1、P1-2、P1-3、P1-4、P1-5、P1-6、P1-7、P1-8、P1-9、P1-12、P1-13、P1-17、P1-18、P1-19、P1-20、P1-21、P1-22、P1-23、P1-24、P1-25、P1-26、P1-27、P1-28、P1-29、P1-30、P1-33、P1-34、P1-35、P1-36、P1-37、P1-38、P1-41、P1-42、P1-44、P1-45、P1-46 及 P2-2~P2-9、P2-11~P2-17、P2-22~P2-39、P2-44~P2-51、P2-53、P2-55、P2-56（Hub / 固件 / 部署侧，需对应面的审计员复查）。

## 审过且没问题的清单（客户端面，本轮确认）

- **RPC 白名单**：`handle_rpc`（`main.rs:181-229`）只接受 6 个方法，未知方法（含 `run_shell`）回 `ok:false`；测试 `rpc_unknown_method_reports_error` 通过。
- **弱 Token 拒绝**：`validate_token` 在建连前拒绝（`transport.rs`），测试 `connect_once_rejects_weak_token_without_dialing` 通过；强但错的 Token 仍无限重试是 P1-43 的范围，不是本项。
- **mTLS 客户端身份**：有证书带证书、无证书只 warn 不降级（读失败直接报错）；测试 `wss_mtls_with_and_without_client_cert` 通过。剩余：证书下发到音箱的路径待真机验证。
- **KWS 诚实性**：无模型/无后端时 `feed` 恒返回 None、不谎报唤醒（测试 `feed_never_false_wakes_without_model` 通过）；静音 RMS 返回 -120 下限而非 -inf。
- **Sherpa FFI 结构**：`Api` 的 `_lib` 最后声明（按声明顺序 drop，符号先失效）；`Drop` 先毁 stream 再毁 spotter；`_paths` 与 spotter 同寿命；显式 `.so` 路径失败不回退默认名；`feed_frame` 每次 `destroy_result`。32 位结构体尺寸已在测试里钉死（但 x86_64 上不执行，见 P2-10）。
- **Opus 编解码边界**：空包拒绝、帧头 `0x01` 校验、20ms/320 点强制、码率显式 16kbps；往返测试 RMS 误差 <10% 通过。
- **ALSA 参数校验**：`set_rate_near` 后校验 16kHz，否则明确报错；XRUN `prepare` 重试最多 2 次；短读循环读满 320。
- **传输鉴权与握手**：hello 先验 Token、5s 握手超时、`hello_reject` 关闭并报错；`FingerprintVerifier` 仍用 webpki 按公钥验签名，只跳过信任链（但忽略有效期/主机名，见 P2-1）。
- **server 模式状态机**：`wake_win`/`wake_lose`/`interrupt` 在 server 模式下的语义（不重复发 `audio_start`、不停流、只停播）均有测试覆盖并通过；Ctrl-C 时尽力补发 `audio_end`。
- **配置解析**：`kws_mode` 非法值启动即报错（测试通过）；指纹归一化（去冒号/空白、转小写、64 hex 校验）与畸形指纹检测均有测试覆盖。

## 未确认项（找不到证据，不猜）

- P0-N1 的 EBUSY 是否在真机上发生：需真机确认 `hw:0,2` 子设备数与二次打开行为，BLOCKED。
- ARM 上 32 位结构体布局（P2-10 的 32-bit 分支断言值 68/104/28）：x86_64 实跑不执行，ARM 交叉测试未跑，BLOCKED。
- 构建不可字节复现的原因（P2-N8）：同一脚本、同一树状态重建产物 SHA/大小与上一版不同，原因未确认。
- 真人唤醒率、双机仲裁真实表现、ALSA 真机行为、60 秒断线在真机上的实际节律：全部 BLOCKED。
- Hub 侧 P1-15/P1-16 的修复计划：本报告只核对了当前 Hub 代码的现状（仍缺失），不预测 Hub 审计员的结论。

## 建议修复顺序（仅建议，不执行）

1. **P0-N1**（双开 ALSA）：按需打开或共享 `Audio` 实例——刷机前必须修，否则首关口的“播”可能是全灭。
2. **P1-16**（仍开）：Hub 每次播报先发 `start_play`、播完发 `stop_play`，客户端给 `playing` 加按音频总时长过期；或客户端收到音频帧时若未 `playing` 视为播放开始。否则对讲/提醒即使接通触发也仍无声。
3. **P1-N1 + P1-N2**（撒谎的 RPC 回复）：`set_volume` 接真混音器或回 `ok:false`；`start_recording` 真正置推流标志。
4. **P1-40 收尾**：修正 `SOURCE.md` / `sherpa.rs` 文档（ORT 也要重编），并决定本地 KWS 路线（源码重编 ORT+sherpa vs 长期 server 模式）；在决策落地前不要把 local 模式当作可用路径。
5. 其余仍开的 17 项按首轮分级处理；P1-43（无限重连）与 P2-40（录音任务不重启）建议优先于 P2 级文档项。
