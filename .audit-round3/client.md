# 第三轮审计：客户端面分工报告

分支已确认：`audit-round3`（基于 main `3a9d1d9`，PR #4 刚合入）。只读代码，未改任何文件、未提交、未跑测试。

## Headline 结论

- **P0：0 项。** 未发现安全/正确性硬伤。P0-N1 的 ALSA 双开修复仍在（录音任务 `open_capture_only`、会话 `open_playback_only`，main.rs 中只用这两个构造器；回归测试在 audio.rs:1107–1167）。
- **P1：2 项。** 一项是 AEC 的标称延迟未经验证且代码连实际缓冲大小都不回读——若真实缓冲不是 4 周期，AEC 会静默失效；一项是 CI 根本不编译 `sherpa-kws` feature，P2-20/P2-21 的修复没有任何 CI 兜底。
- **P2：13 项**（含 1 项待定 P2-42 残留、1 项已知延期 P2-1 备注）。
- P2 轮 headline 修复**逐项核实：11 项属实有效，2 项有缺口**——P2-52 的 ALSA 标记断言只在 musl 脚本落实，默认交付的 gnueabihf 脚本没有；P2-54 的编解码函数本身干净，但 `kws.rs:311` 仍有一处生产 `.expect`。

## P1

### C-1 — AEC 标称延迟 80ms 未经验证，且代码不回读实际 ALSA 缓冲大小

**一句话结论：** `AEC_REF_DELAY_FRAMES = 4` 只是理论值，若真实缓冲不是 4 周期，回声会落在 256 抽头窗之外，AEC 静默失效且无任何告警。

**证据：**
- `client/src/aec.rs:42`：`pub const AEC_REF_DELAY_FRAMES: usize = 4;`，模块头注释（aec.rs:27–29）自认“只是 ALSA 缓冲 4 周期的理论值，真实声学延迟需真机校准”。
- `client/src/audio.rs:458–465`：`open_pcm` 用 `set_period_size_near` / `set_buffer_size_near(4*320)` 设缓冲，**`_near` 语义是“尽量接近”，随后没有任何回读**（无 `buffer_size()` / `period_size()` 读取、无日志、无断言）。硬件若取整为 3 或 5 周期，延迟偏差 20ms 即超出 NLMS 窗口（256 抽头 = 16ms）。
- 另有两处加剧不确定性的因素：采集 `hw:0,0` 与播放 `hw:0,2` 是两个 PCM 设备，延迟以“帧计数”而非墙钟对齐，速率稍有漂移即表现为延迟抖动；`aec.rs` 注释称 NLMS 窗口“覆盖该延迟前后各 16ms”，实为单侧 0~16ms（抽头只覆盖标称延迟之后），容差比注释写的更紧。

**修复建议：** `open_pcm` 设完参数后回读实际 `buffer_size` / `period_size` 并记日志；若与 4 周期不符则启动时明确 warn（或据实推导 `AEC_REF_DELAY_FRAMES`）；真机校准仍是 BLOCKED 项，刷机验证时实测回声延迟。

### C-2 — CI 从不编译/测试 `sherpa-kws` feature，P2-20/P2-21 的修复无 CI 兜底

**一句话结论：** Rust job 只跑默认 feature，`sherpa.rs` 整个文件（含 `decode_ready_loop` 及其单测）与 `kws.rs` 的 sherpa 分支在 CI 里从未被编译。

**证据：**
- `.github/workflows/ci.yml:88–96`：`cargo test --locked`、`cargo fmt --check`、`cargo clippy --all-targets`，均未加 `--features sherpa-kws`。
- `client/src/sherpa.rs:39`：`#![cfg(feature = "sherpa-kws")]`，整个文件门控；`client/src/sherpa.rs:724–751` 的 `decode_ready_loop_caps_stuck_is_ready` 单测、P2-20 的停后端逻辑（kws.rs:180 的 `KWS_FEED_DISABLE_AFTER = 50` 及 feed 内 sherpa 分支）只在 ARM 构建脚本触发的编译中才会被检查。
- 后果：后续改动改坏 sherpa 路径时 CI 仍全绿，只能靠 VM 上的 ARM 构建才发现。

**修复建议：** CI rust job 增加一步 `cargo test --features sherpa-kws`（x86_64 宿主机即可；需真实 .so 的端到端测试无环境变量时自动 SKIP，只有 `decode_ready_loop` 单测与结构体尺寸单测会实际跑，正好覆盖 P2-21）。

## P2

### C-3 — AEC 回归测试可信度缺口：两项关键测试是空洞的，回声测试只覆盖甜点

**一句话结论：** `pure_voice` 与 `zero_reference` 测试因全零参考走短路分支，根本没测到自适应路径；回声抑制测试只用了周期正弦 + 恰好 1320 采样延迟。

**证据：**
- `client/src/aec.rs:84–97`：队列不足或参考全零时 `process_frame` 直接 `return *mic`，不进 NLMS。
- `client/src/aec.rs:216–236`（`zero_reference_passes_mic_through_unchanged`）与 `aec.rs:262–279`（`pure_voice_without_echo_is_preserved`）：全程只喂零参考，`taps_max_abs() == 0` 与“RMS 误差 <5%”均由短路保证，**测不到“有非零参考时 NLMS 是否误伤语音”**——而这才是真实风险。
- `client/src/aec.rs:177–215`（`echo_suppression_synthetic_known_echo_plus_ref`）：参考为 300Hz 正弦（每帧恰 6 周期，帧间相位连续）、回声延迟恰为 1320 采样（= 4 帧 + 40 点，正落在抽头窗内）；无延迟失配用例、无非周期信号、无中途静音后恢复用例。
- 注：逐帧验算过延迟队列逻辑——每帧弹出最旧帧恰为 4 帧前的参考，与文档一致；且零参考短路**不会**造成永久失同步（队列每帧把最新环形样本重锚定到回声源，扰动 <1 帧即自愈），所以这只是测试缺口，不是 C-1 之外的实现 bug。

**修复建议：** 补三例：非零参考 + 纯语音（验证 NLMS 不削弱语音）、回声延迟 ±1 帧失配（验证抑制率衰减可预期）、中途喂零参考帧后恢复（验证自愈）。

### C-4 — 零能量守卫阈值 1e-9 对 i16 输入几乎永不触发，真正起作用的是步长钳制

**一句话结论：** P2-22 的两道防线只有第二道（±1.0 钳制）在干活，第一道（1e-9）在 i16 数据上基本是死代码。

**证据：**
- `client/src/aec.rs:47–50`：`AEC_MIN_REF_NORM = 1e-9`，`AEC_MAX_STEP = 1.0`；`aec.rs:131–140`：`if norm >= AEC_MIN_REF_NORM` 才更新，步长钳制 ±1.0。
- 算术：单采样 1 LSB 的平方范数 `(1/32768)² ≈ 9.3e-10 < 1e-9`，两个 1 LSB 采样即 `1.9e-9 > 阈值`——守卫只拦得住“全窗至多一个 1 LSB 采样”的情形；且全零帧早被 `aec.rs:93` 的短路拦下。实际防发散靠的是钳制（`step = (mu*e/(norm+1e-6)).clamp(±1.0)`）。

**修复建议：** 不改行为亦可，但注释应写实（“守卫只拦单采样毛刺，防发散靠钳制”）；若想让守卫真正生效，把阈值改为能量口径的有意义值并配测试。

### C-5 — `ref_tx` 满时静默丢帧，无计数、无日志

**一句话结论：** 播放参考是尽力而为的信号，但丢参考恰好是扰动 AEC 对齐的事件，丢了却完全不可见。

**证据：** `client/src/main.rs:799`：`let _ = ref_tx.try_send(frame);`——`Full` 与 `Closed` 均被忽略；通道容量 16（main.rs:511）。对比同文件的 `audio_tx` 满时有计数 + 5 秒一条节流日志（P2-N5，main.rs:572 起）。

**修复建议：** 参照 P2-N5 加丢帧计数 + 节流日志。

### C-6 — P2-52 的 ALSA 标记断言缺失于默认交付脚本（gnueabihf）

**一句话结论：** musl 脚本有双重保护（缺 libasound 即 ASSERT FAIL + 产物标记断言），默认交付的 gnueabihf 脚本只有前者，没有后者。

**证据：**
- `client/build-armv7.sh:63–65`（缺 musl libasound 即 exit 1）与 `build-armv7.sh:2b` 节（产物必须含 `open-xiaoai-audio-backend=alsa` 标记，否则 ASSERT FAIL）。
- `client/build-armv7-gnueabihf.sh`：全篇无 `open-xiaoai-audio-backend=alsa` 检查；它只断言 sherpa 标记、GLIBC 版本、rpath、NEEDED 库。该脚本把其余参数原样透传给 cargo（`CARGO_ARGS`），即 `./build-armv7-gnueabihf.sh --no-default-features` 会静默产出无 ALSA 的默认交付二进制且脚本不拦。

**修复建议：** 把 musl 脚本 2b 节的标记断言原样补进 gnueabihf 脚本（有 `--no-default-features` 透传时跳过并 WARNING，与 musl 脚本的 `--allow-no-alsa` 语义对齐）。

### C-7 — `rust-toolchain.toml` 无 targets 列表，注释称“自动下载 ARM target”不成立

**一句话结论：** 工具链版本本身钉对了（1.100.0，ci.yml:75 同步安装 + rustfmt/clippy），但自动装 target 的说法是错的。

**证据：**
- `client/rust-toolchain.toml:16`：只有 `[toolchain] channel = "1.100.0"`，无 `targets = [...]`；其注释（第 9 行）称“首次构建会自动下载 1.100.0 + 两个 ARM target”——rustup 只自动装文件里列出的 target，未列出的不装。
- 两构建脚本均无 `rustup target add`，只有 `client/README.md:164,202` 写了手动 `rustup target add armv7-...`。新机器上首次构建会报 target 未安装，报错不直指原因。
- 其余引用点一致：ci.yml:68–76、两构建脚本注释、FIXES_P2.md:91 均写 1.100.0，无版本漂移。

**修复建议：** 在 `rust-toolchain.toml` 加 `targets = ["armv7-unknown-linux-gnueabihf", "armv7-unknown-linux-musleabihf"]`（一处改动即覆盖两脚本），或至少修正注释。

### C-8 — 生产代码仍有一处 `.expect`，与 P2-54 声称的“不再有任何 .expect panic 路径”不符

**一句话结论：** 编解码自由函数本身已干净，但 sherpa 门控的生产路径里还有一处 `.expect`。

**证据：**
- `client/src/audio.rs` 的 `encode_pcm_frame` / `decode_opus_frame`：`Result<OpusCodec>` 线程本地 + `try_borrow_mut`，失败返回 Err，确认无 expect/unwrap——P2-54 就这两个函数而言属实。
- `client/src/kws.rs:311`（`#[cfg(feature = "sherpa-kws")]` 的 `feed` 内）：`frame.as_slice().try_into().expect("frame is 320 samples")`。逻辑上不可达（frame 恰由 `drain(..FRAME_SAMPLES)` 切出），风险极低，但 FIXES_P2.md:85 的表述是绝对化的。

**修复建议：** 改为 `debug_assert!` + `Err` 返回（或 `match`），并收窄 FIXES_P2.md:85 的表述为“编解码函数无 expect 路径”。

### C-9 — 播放路径每帧一条 `info!` 日志（50 条/秒）

**一句话结论：** 每次播出一帧都打 info 日志，长期运行会刷屏。

**证据：** `client/src/main.rs:801`：`info!("played {}-sample frame via ALSA", pcm.len());`，位于 `RawFrame::Audio` 分支，每 20ms 一帧即 50 条/秒。对比同文件的播放失败日志有 5 秒节流（main.rs 内 `playback_err_count` / `last_playback_err_log`）。

**修复建议：** 降为 `debug!` 或节流。

### C-10 — 无客户端 WS Ping、无发送超时；半死连接可 wedged 整个会话与录音任务

**一句话结论：** Hub 只停读不发 FIN/RST 时，`run_session` 的 `send_*` 会无限挂起，录音任务还会因阻塞的 wake 发送一并停摆。

**证据：**
- `client/src/transport.rs`：客户端从不发 Ping（只回 Pong，`next_raw` 内）；`run_session`（main.rs:735 起）的 `send_audio_frame` / `send_status` / `send_heartbeat` / `send_wake` 均无超时；`TcpStream` 未设 keepalive。
- 传导链：`send_*` 挂起 → `run_session` 的 `select!` 停住 → `audio_rx`（容量 32）满后录音任务丢帧尚可自保，但 `wake_tx.send(ev).await`（main.rs 录音任务内，阻塞式，与 P2-N5 的 `try_send` 不对称）会把录音任务整个堵住——采集、AEC、编码全停。正常断线（FIN/RST）不受影响，已有重连测试覆盖（transport.rs:1264 起）。

**修复建议：** 发送加超时（如 `tokio::time::timeout`，超时按断线重连）；或客户端每 30s 随 heartbeat 发 WS Ping；`wake_tx` 改 `try_send` 与音频通道一致。

### C-11 — P2-19 录音任务侧跳编码、P2-N4 重建编解码器接线无测试

**一句话结论：** 修复代码都在，但能证明它们的测试只覆盖了节流器单体与会话层，录音任务本体不可测。

**证据：**
- P2-19：`if !pushing_capture.load(...) { continue; }`（main.rs:566）在录音任务内跳过编码；e2e 测试 `local_mode_start_recording_pushes_audio_frames`（main.rs:1936）只验证了 `run_session` 层面的帧门控，未验证“未推流时不编码”。
- P2-N4：`encode_throttle.should_recover()` → `rebuild_codec()`（main.rs 录音任务内）；单测只有 `FailureThrottle` 本体（audio.rs:868 起）与 `rebuild_codec_keeps_audio_usable`，两者之间的接线无测试。
- 根因：录音任务是 `main()` 内 spawn 的闭包，无注入点。

**修复建议：** 接受现状或将录音任务抽为可测函数（注入 Fake Audio + 通道），后者是结构性改动，留待 P2 收尾时评估。

### C-12 — TestArbiterWindow flake：测试侧时序假设过紧（Hub 侧 Go 测试）

**一句话结论：** 50ms 睡眠对 100ms 窗口只剩 50ms 裕量，CI 负载下睡眠超调即漏窗、断言赢家错误；生产仲裁逻辑本身无问题。

**证据：**
- `hub/internal/arbitration/arbitration_test.go:34–54`：`New(100ms)` 后 `Submit` 事件 1，`time.Sleep(50ms)`，再 `Submit` 事件 2，断言赢家为事件 2。
- `hub/internal/arbitration/arbitration.go` 的 `Submit`：首条事件即 `time.AfterFunc(window, close)`；`time.Sleep` 只保证不少于 50ms、不保证不多于——负载下调度延迟 >50ms 时窗口已在事件 2 到达前关闭，第一次 `close` 即以仅事件 1 裁决，测试在赢家断言处失败。这与 FIXES_P2.md:283 记录的“1/5 FAIL”及 TEST_REPORT.md:194“4 次以上重跑未复现”一致（调度相关、偶发）。
- 定级：测试侧，P2；生产侧 `Arbiter` 的窗口/去重/冷却逻辑读过，无对应缺陷。

**修复建议（测试侧）：** 去掉 `Sleep(50ms)`（两事件背靠背提交仍在同一窗口内，测试意图不变，flake 根除）；或把窗口放大到 500ms、睡眠保持 100ms（5 倍裕量）；彻底做法是给 `Arbiter` 注入可 mock 的时钟。

### C-13 — 文档/注释过期一捆 + `Audio::open()` 仍可公开双开

**一句话结论：** 三处文字与代码不符；防 P0-N1 回归只靠注释，结构上无强制。

**证据：**
- `client/src/transport.rs:1–8` 模块头仍写“断线后指数退避重连：首 100ms，上限 1s，30s 内恢复”——实际 `BACKOFF_MAX_MS = 30_000`（transport.rs:60），上限是 30s；同文件头又写“未配置指纹时跳过校验并打警告”，与 P1-10 的指纹必填（config.rs validate）矛盾（回退分支仍在 `build_tls_config`，仅当 validate 被绕过时才可达）。
- `client/src/transport.rs` 的 `connect_with_retry` 注释称鉴权失败“只重试 3 次”/“3 times in a row”，实际 `auth_failures` 是累计计数、非连续失败不归零（transport.rs 内 `connect_with_retry`）。
- `client/src/audio.rs` 的 `Audio::open()` / `open_devices()` 仍为 pub 且双开两个方向，防回归只靠文档“禁止生产路径用本函数”与 audio.rs 的构造器行为测试；main.rs 目前用的是对的，但一次误改即可重引入 P0-N1。

**修复建议：** 顺手改注释；`Audio::open()` 改为 `#[cfg(test)]`（生产与测试目前都只在测试里用它）。

### C-14 — `FingerprintVerifier` 不校验证书有效期与 SAN（已知延期项，备注）

**一句话结论：** 指纹匹配即放行，不过期、不验 SAN；过期证书只要指纹对就仍被接受。

**证据：** `client/src/transport.rs` 的 `verify_server_cert` 忽略 `_server_name` 与 `_now`，只比对 `fingerprint_matches`；签名本身经 `verify_tls12/13_signature` 用 webpki 验过。config.rs 注释已言明该项属 P2-1 范围、不在本轮。

**修复建议：** 记为已知延期（P2-1），下次动 TLS 验证时一并补有效期 + SAN 校验；不新开工作。

### C-15 — P2-42 残留：`hub_url` 默认仍是写死的 `wss://192.168.1.10:4399`（待定，需 bai 拍板）

**一句话结论：** 缺失文件已明确报错（P2-42 前半属实），但字段级默认仍在，旧 client.toml 省略 `hub_url` 即静默连向写死地址。

**证据：**
- `client/src/config.rs:12–14`：`default_hub_url()` 仍为 `wss://192.168.1.10:4399`；`config.rs` 的 `load_from_path` 缺失文件时明确报错（P2-42 前半已落实，测试 `missing_file_is_clear_error_not_silent_default` 在 config.rs 内）。
- 风险已被部分兜住：Token 默认已空、指纹必填，错配的默认 Hub 无法鉴权（FIXES_P2.md:296 同款表述）。

**状态：** 非新发现，FIXES_P2.md:83 已如实标注残留。彻底去掉需 `hub_url` 必填（breaking change），**等 bai 拍板**，不擅自改。

## 逐项核实结果（P2 轮 headline，声称 vs 代码）

| 项 | 结论 | 证据 |
|---|---|---|
| P2-19 只在 pushing 时编码 | ✅ 属实 | main.rs:566（录音任务跳编码）+ run_session 每次 Hub 消息后同步标志；重连等待期先清标志 |
| P2-20 KWS 失败 50 次停后端 | ✅ 属实 | kws.rs:180（`KWS_FEED_DISABLE_AFTER = 50`）、feed 内节流 + 停后端 + error（sherpa 门控）；但见 C-2：CI 不编译此路径 |
| P2-21 is_ready 排空改 decode_ready_loop | ✅ 属实 | sherpa.rs:529–548（上限 100，超限复位流并 Err）、单测 sherpa.rs:724–751；server 模式不走此路径（init_kws 返回 None） |
| P2-22 NLMS 守卫 1e-9 / ±1.0 | ✅ 字面属实，见 C-4 | aec.rs:47–52、131–140；`mu = 0.5`、256 抽头 = 16ms 与 Hub 侧同口径 |
| P2-40 ALSA 失败退避重开 | ✅ 属实 | main.rs:96（100ms 起步翻倍、上限 5s）、录音任务重开循环、单测 main.rs:2094 |
| P2-42 缺文件明确报错 | ✅ 部分（残留已知，见 C-15） | config.rs `load_from_path` |
| P2-52 musl libasound 缺失 ASSERT FAIL | ⚠️ 仅 musl 脚本属实，见 C-6 | build-armv7.sh:63–65 + 标记断言；gnueabihf 脚本无标记断言 |
| P2-54 编解码函数返回 Result 无 expect | ⚠️ 函数本身属实，见 C-8 | audio.rs 自由函数干净；kws.rs:311 仍有 `.expect` |
| P2-N1 interrupt 真调 stop_playback | ✅ 属实 | main.rs `apply_action` StopPlayback 分支 + e2e 测试 main.rs:2124（stop 恰调 1 次、在途帧被抑制） |
| P2-N2 入站 WS 帧 64KB | ✅ 属实 | transport.rs:41（`MAX_WS_MESSAGE_BYTES = 64*1024`）、ws_config 两路配置、超限断线测试 transport.rs:1157 起 |
| P2-N4 编码失败节流 + 50 次重建 | ✅ 代码属实，接线无测试（见 C-11） | main.rs 录音任务内节流 + `rebuild_codec()` |
| P2-N5 audio_tx 改 try_send | ✅ 属实 | main.rs:572，满时丢帧计数 + 5 秒节流日志 |
| P2-N6 hello_ok/rpc 解析严格化 | ✅ 属实 | transport.rs `parse_hub_text` 与 `connect_once` 握手路径双份校验 |
| P2-N7 工具链钉 1.100.0 + CI 同步 | ✅ 基本属实，见 C-7 | rust-toolchain.toml:16、ci.yml:75（1.100.0 + rustfmt/clippy）；缺 targets 列表 |

**AEC 专项核实：** NLMS 实现正确（`step = mu·e/(norm+1e-6)` 钳制 ±1.0，抽头 256 @16kHz = 16ms 自洽）；延迟队列逻辑正确（每帧弹最旧帧 = 恰 4 帧前，80ms）；`ref_tx` 容量 16、满时 `try_send` 丢帧不阻塞，Aec 由录音任务独占、无 Mutex、无死锁/数据竞争；接线正确——每采一帧先 `process_frame` 再喂 KWS 与 Opus 编码（main.rs 录音任务内），零参考直通，server/local 双模式均生效，唯一播放入口 `RawFrame::Audio → play_frame` 均喂 `ref_tx`（main.rs:799）。回声测试的判定口径与 Hub 侧一致，但覆盖面见 C-3。

**unsafe 审计（sherpa.rs 为唯一含 unsafe 的客户端文件）：** FFI 结构体尺寸 64-bit 有 C 探针锚定测试，32-bit 手算值（OnlineModelConfig 68 / KeywordSpotterConfig 104 / KeywordResult 56）重算无误；`Symbol` 转 `'static` 靠 `_lib` 字段最后 drop 的顺序保证；`create_spotter` / `create_stream` 均判空，失败时已建 spotter 会销毁；`get_result` 判空且 `destroy_result` 恒调；`Drop` 先毁 stream 再毁 spotter；`unsafe impl Send` 依赖 `&mut` 串行调用，与单录音任务独占一致。**unsafe 未发现新问题。**

## 计数

- **P0：0 项**
- **P1：2 项**（C-1、 C-2）
- **P2：13 项**（C-3～C-15，其中 C-15 为待定残留、 C-14 为已知延期备注）
- 未发现“声称已修实际没修”的项；两处为“修了但有缺口”（C-6、C-8）。

**给协调员的建议：** 部署前至少处理 C-1（回读缓冲大小 / 校准延迟，否则 AEC 可能全程零作用还不报错）和 C-2（CI 加 `--features sherpa-kws` 一步，否则 P2-20/P2-21 零回归保障）；其余 P2 可进下一批。其余均可在真机验证时一并确认。
