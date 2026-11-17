# open-xiaoai-next 测试报告

- 日期：2026-11-17（CST）
- 范围：Phase 0 环境、Phase 1（Hub 骨架 / firmware-patch / Client）、Phase 2（仲裁 / Snapcast / 网易云）、Phase 3（连续对话 / 打断 / HA / 提醒 FR-14 / 对讲）、Phase 4（Web 面板 / OTA / 紧急呼叫 FR-15 / 摄像头播报 FR-10）
- 原则：每项测试真实执行被测代码；需要真机才能验证的一律记 BLOCKED，绝不写成通过。
- 位置：`~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/`

## Phase 0 — 环境

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| Go 1.23.12 持久安装 | `go version` | `go1.23.12 linux/amd64`（`~/workspace/tools/go`） | 通过 | 无 |
| Rust stable 持久安装 | `cargo --version` / `rustc --version` | `cargo 1.100.0` / `rustc 1.100.0`（`~/workspace/tools/cargo`，需 source `env.sh`） | 通过 | 无 |
| Python + pytest + yaml | `python3 -m pytest --version` / `python3 -c "import yaml"` | pytest 9.2.0；yaml 可用 | 通过 | 无 |
| squashfs-tools | `which mksquashfs unsquashfs` / `dpkg -l squashfs-tools` | 4.6.1 已装（`apt-get update` 自 03:39 卡住，改用本地缓存 .deb `dpkg -i` 装好） | 通过 | `/usr` 重启会丢，需重装 |
| libopus 构建 | 源码自编 libogg 1.3.5 + libopus 1.5.2 + libopusfile 0.12 到 `~/workspace/tools/libopus`；`pkg-config --modversion opus opusfile ogg` | 1.5.2 / 0.12 / 1.3.5；`.pc` 补 `-lm -logg` 后 `go build` 通过 | 通过 | apt 的 libopus-dev 未装上（update 卡住），改走源码自编；NAS 部署时 Hub 镜像里同样要装 libopus |
| 参考仓库只读 clone | `ls references/` | idootop/open-xiaoai、OwnDing/open-xiaoai、coderzc/open-xiaoai-bridge | 通过 | 无 |

## Phase 1 — Hub 骨架（Go）

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| 编译 + vet | `go build ./...` / `go vet ./...`（`hub/`，需 source `env.sh`） | BUILD_OK / VET_OK | 通过 | 无 |
| 单元测试 17 个包 | `go test ./...`（`hub/`） | 75 个测试全过，0 失败（见下表分包明细） | 通过 | 无 |
| 集成：Hub + 双模拟 Client 仲裁 | `go test -run 'TestTwoClientsArbitration\|TestSingleClientWinsDirectly\|TestBadTokenClientRejected' -v .` | 双 Client：RMS -15 的 oh2p 得 wake_win，RMS -30 的 lx06 得 wake_lose；单台直接赢；错误 Token 得 hello_reject | 通过 | 无 |
| 端到端：真 Hub 二进制 + 真 Rust Client | Hub（`tls: false`，`:14399`）+ `target/release/open-xiaoai-client`（token 一致）同时跑，连接期间 curl `/api/devices` 与 `/api/status` | 客户端日志 `authenticated with hub, device_id=lx06-127-0-0-1`；Hub 侧 devices 返回 `{"ID":"lx06-127-0-0-1","Model":"lx06","IP":"127.0.0.1","Online":true,"Version":"0.1.0"}`，status `{"devices":1,"online":1}` | 通过 | 无 |
| Hub 二进制冒烟（ws） | `go build -o /tmp/hub-smoke ./cmd/hub` 后以 `tls: false` 启动，curl `:18080/api/status` 与 `/api/devices` | `:14399`/`:18080` 监听正常；status 返回 `{"devices":1,"online":0,"rooms":["客厅"],"wake_word":"你好小李"}`；HA/STT/TTS 拒绝连接时记日志、不阻塞启动 | 通过 | 无 |
| Hub 二进制冒烟（wss） | 同上以 `tls: true` 启动 + 临时 wss 探针拨号 hello | 证书自签落盘 `hub-data/tls/hub.crt|hub.key`，指纹 `64cc0b6506dccf3ab3e990e00f114d1c7230bedb7abc43fb110d9f56e6a9891e`；正确 Token → `hello_ok`（device_id `lx06-127-0-0-1`），错误 Token → `hello_reject`（bad token） | 通过 | 无 |
| config.yaml / docker-compose.yml 校验 | `python3 -c "import yaml; yaml.safe_load(...)"` | YAML OK | 通过 | 无 |

### `go test ./...` 分包明细（75 个测试）

| 包 | 测试数 | 关键断言 |
|---|---|---|
| hub（集成） | 3 | 双 Client 仲裁只响的一台赢；单台直接赢；错误 Token 拒绝 |
| internal/config | 4 | 真实 config.yaml 加载+校验通过；TLS 默认开；非法型号/空 music_folder 拒绝；Save/Load 往返不丢数据 |
| internal/registry | 3 | Upsert 不丢已认领房间；ByRoom 只返回在线设备；**缺字段 Upsert 合并语义（不抹已有字段、不改在线状态）** |
| internal/arbitration | 3 | 纯函数选 RMS 最大；100ms 窗口真计时关闭、赢家+输家正确 |
| internal/opus | 2 | 440Hz 正弦往返，对齐编解码延迟（~30 采样）后相对 RMS 误差 4.97% < 10%；非 320 点帧报错 |
| internal/tlscert | 2 | 证书生成+复用（指纹不变）；非法 PEM 报错 |
| internal/aec | 3 | 合成回声残余 RMS < 输入 50%；RMSEnergy；ProcessFrame 长度 |
| internal/ota | 4 | SHA256 已知值；非法 sha/version 拒绝；旧版 hello 不确认、新版确认 OK；超时 31s 扫荡 → failed |
| internal/reminders | 6 | NextDaily 今/明判断；一次性触发后删除；每日重排；间隔推进；持久化往返；Start ticker 2s 内触发 |
| internal/intercom | 4 | 半双工忙拒绝；Forward 中继到目标；广播；5s 空闲超时清掉 |
| internal/wyoming | 2 | 假 Wyoming TCP 服务：Describe/Transcribe（“打开客厅灯”）/Synthesize（640 字节 PCM）；PCM 往返 |
| internal/agent | 3 | 假 OpenAI 服务：Chat；SSE 流式（delta“你”“好”）；错误 key 401 |
| internal/ha | 5 | 假 HA WS：auth 握手；get_states；call_service 真改状态；subscribe 后服务端推送事件收到；fire_event；错误 token 拒绝 |
| internal/music | 6 | 假 Snapcast：GetGroups（1 组 2 台，卧室延迟 15ms）+ 延迟/流/静音/音量；假网易云：搜歌/取 URL/歌词；假 MA：Players/点歌/播放暂停 + 错误透传；假 MPD：PlayURL 发 clear/add/play |
| internal/router | 10 | 紧急呼叫；每日/间隔提醒+取消；记忆；HA 开灯（查状态、不重复调已开的灯）；门锁拒绝；MA 点歌；网易云+MPD 降级点歌；Agent 兜底；无 Agent 明说 |
| internal/transport | 5 | hello_ok（device_id `lx06-127-0-0-1`）；错误 Token/未知型号 hello_reject 且关闭；room_claim；服务端 RPC 下发；二进制帧 0x01 前缀 |
| internal/discovery | 3 | hostsInCIDR（/30 展 4 个，/16 截断 1024）；ProbeModel 握手成功/错误 Token 拒绝；Scan 在 127.0.0.2/32 找到 lx06，本机 IP 跳过 |
| internal/voice | 4 | RMS；整条管线（Opus 语音帧 → 800ms 静音结算 → STT → Router → TTS → 50 帧 Opus 回帧）；audio_end 强制结算；纯静音不回答 |
| internal/web | 3 | /api/status、/api/devices；PUT 非法配置 400 且不写文件、合法 200 且落盘；OTA 非法 sha 400、合法 200 且 PushOTA 被调 |

### 测试中发现并修复的实现问题

1. `hub/cmd/hub/main.go`：`httpListenAndServe` 是写坏的桩 → 改为 `http.ListenAndServe`；删除 `tlsCertHolder` 死代码；提醒 Store 建了两次（路由用的和调度用的不是同一个）→ 合并为一个 + 新增 `reminders.SetOnFire`。
2. `hub/internal/reminders/reminders.go`：`Add` 用毫秒时间戳做 ID，同一毫秒连加两条会撞 ID、后一条覆盖前一条 → 改为纳秒时间戳 + 进程内自增序号。
3. `hub/internal/intercom/intercom.go`：`Start` 只查发起方是否忙，目标方忙时也能反向建会话（半双工失效）→ 加 `participantBusy`，发起方/目标方任一在会话中即拒绝。
4. `hub/internal/router/router.go`：每日提醒正则 `每天\s*(\d{1,2})[点:：]` 不允许数字与“点”之间有空格，“每天 8 点提醒我吃药”会漏到 Agent 兜底 → 正则加 `\s*`。
5. `hub/internal/web/web.go`：config PUT 先校验后补默认，缺 `hub.listen` 的 PUT 被 400 → 改为先 `ApplyDefaults` 再 `Validate`（wake_word / music_folder / 型号校验不受影响）。
6. 测试方法修正（实现无问题）：Opus 往返须对齐编解码延迟（~30 采样）再算误差，否则相位差会被算成 103% 误差；Voice 测试须每帧现编新鲜静音包，重复解同一个静音包会复现编码器残留音（RMS ~2210），VAD 永远等不到静音。
7. `hub/internal/registry/registry.go`：`Upsert` 更新路径无条件覆盖全部字段，只保留 Room。后果：`main.go` 的 `OnStatus` 只带 `{ID, Online, Version}`，每次客户端上报状态都会把 hello 时登记的 Model/IP 抹成空；配置播种/自动发现的 Upsert 不带 Online，还会把已连设备标离线；`LastSeen` 也会被零值覆盖。端到端复现确认后修复：Upsert 改为合并语义（只覆盖非空字段、不碰 Online、LastSeen 恒刷新），hello 与 OnStatus 改走 `SetOnline`；新增回归测试 `TestUpsertMergeDoesNotWipe`；端到端重验：连接中 `/api/devices` 正确显示 `Model: lx06`、`IP: 127.0.0.1`、`Online: true`。

## Phase 1 — firmware-patch 流水线（Python + shell）

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| pytest 全套 | `python3 -m pytest tests/ -q`（`firmware-patch/`，我亲自复跑） | **35 passed in 64.34s** | 通过 | 无 |
| download-ota 签名算法 | `pytest tests/test_download_ota.py -q`（含在上） | mock 网络、固定 time，断言 s 值与完整 URL 与 REA_ANALYSIS.md 算法一致；MD5 不符抛错；无真实请求 | 通过 | 无 |
| patch_firmware 补丁 | `pytest tests/test_patch_firmware.py -q`（含在上） | 在真实 rootfs 副本上跑：6 组补丁逐项断言内容；前置条件不满足时抛错且被改文件字节级无变化 | 通过 | 无 |
| shell 脚本 | `pytest tests/test_shell_scripts.py -q`（含在上）+ `bash -n scripts/patch.sh scripts/unpack.sh` | 8 passed；bash -n OK | 通过 | 无 |
| unpack 真实固件 | 测试内用真实 `lx06_1.94.13.bin` 重跑解包 | root.squashfs **37,500,699 字节、Squashfs v4.0 xz、2137 inodes、blocksize 131072**，与 REA_ANALYSIS.md 一致 | 通过 | 无 |
| patch.sh 端到端 | 测试内跑完整流水线 | 输出镜像 **38,453,248 字节 < 41,943,040（40MB），余 3,489,792 字节**；重解后全部补丁标记都在 | 通过 | 无 |
| 补丁镜像持久交付（复核人重跑） | `OTA_BIN=<官方包> ./scripts/unpack.sh lx06 1.94.13` + `./scripts/patch.sh lx06 1.94.13` | `firmware-patch/work/lx06_1.94.13/root-patched.squashfs`，**38,453,248 字节 < 40MB**，SHA256 `cc36b28e7f2e998d5a6a88f8597de85f0cc0021a1d864d4d785234a6bb54f06d`；原包 MD5 仍为 `b202de2bbe149ce884aa500911151b95` | 通过 | 真机刷机验证 |
| firmware/ 原物未动 | 测试内校验 | `lx06_1.94.13.bin` MD5 仍为 `b202de2bbe149ce884aa500911151b95`；`firmware/rootfs` shadow 仍为原 hash | 通过 | 无 |

实施中修正的问题（均有测试覆盖）：`download-ota.py` 时间戳由秒级改为毫秒（与 ota.ts / REA 一致）；旧 `unpack.sh` 调 unblob 用 `-x`，unblob 26.6.4 只有 `-e`，已修正；unblob 缺 `sasquatch`/`7z` 时非零退出但产物已正确 carve 出，`unpack.sh` 改为以产物校验为准；容器无 CAP_MKNOD 时 `unsquashfs` 建 `/dev/*` 节点报 `Operation not permitted`（非致命），两处调用加 `-no-exit-code`；`/tmp` 是 512MB tmpfs 装不下多份 rootfs 副本，测试临时目录改到 `firmware-patch/.pytest-tmp`。

## Phase 1 — Client（Rust）

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| `cargo test`（x86_64，我亲自复跑） | `cargo test`（`client/`，需 source `env.sh`，`TMPDIR=~/workspace/.cargo-tmp`） | **32 passed; 0 failed; 1.52s**。覆盖：Opus 往返（1000Hz 正弦 50 帧，解码 RMS 误差 <10%，帧长 320）；Token 握手（本地真 tokio-tungstenite 服务，好 token 通过、错 token 被 `hello_reject` 拒）；wss + 指纹（rcgen 自签证书的真 tokio-rustls 服务：好指纹+好 token 通过，错 token 被拒，错指纹握手失败，非法指纹配置期报错）；断线重连（服务端主动关闭后同端口重启，30s 内重连成功；服务晚起 1s 的重试）；RMS（恒定 16384 → -6.02dB，正弦 → -9.03dB，静音 → -120dB）；config（默认/自定义/部分缺省/非法指纹/缺失文件）；Session 状态机（wake_win 开始推流并发送 audio_start、wake_lose 静默、interrupt 立即停播、rpc 白名单、`run_shell` 拒绝并回 `ok:false`） | 通过 | 无 |
| `cargo build --release`（host，我亲自复跑） | `cargo build --release` | 通过，0 警告；产物 `target/release/open-xiaoai-client` 2,919,768 字节，ELF 64-bit x86-64，stripped | 通过 | 无 |
| ARMv7 交叉编译（我亲自复跑 + 强制重链接验证） | `./build-armv7.sh`（先 `rustup target add armv7-unknown-linux-musleabihf`；`touch src/main.rs` 后重跑） | exit 0（强制重链接 38.14s）；产物 `target/armv7-unknown-linux-musleabihf/release/open-xiaoai-client` 2,412,048 字节，**ELF 32-bit ARM，静态链接，stripped（<5MB 目标）** | 通过 | 真机上机验证（见关口 1） |
| 客户端冒烟（我亲自复跑） | `OPEN_XIAOAI_CLIENT_TOML=/tmp/smoke/client.toml timeout 6 ./target/release/open-xiaoai-client`（指不存在的 Hub） | 正常启动、打 KWS/ALSA BLOCKED 警告、按 100ms→200ms→400ms→800ms→1s 退避重试，不崩溃 | 通过 | 无 |
| 端到端：真 Client + 真 Hub | 见 Phase 1 Hub 表“端到端”行 | 客户端 `authenticated with hub, device_id=lx06-127-0-0-1`；Hub 侧设备在线、型号/IP 正确 | 通过 | 无 |

与任务书的偏差（已核实）：**`audopus` 在 crates.io 上不存在**（`cargo` 报 `no matching package named audopus`）——改用功能等价的纯 Rust crate `opus-rs` 0.1.42（libopus 1.6 移植，无 C 依赖）；`audiopus`/`opus` 均链接系统 libopus，ARMv7 musl 交叉编译会卡住，不用。已写入 Cargo.toml 注释与 README。

交叉编译踩坑（已解决，写进 README 与 `build-armv7.sh`）：musl.cc 的 `arm-linux-musleabihf-cross` 是 32 位 x86 静态二进制，在本 VM 一运行即段错误（exit 139），已删除；改用 zig 0.15.2（`~/workspace/tools/zig-x86_64-linux-0.15.2/zig`）做 ARM C 编译器 + 链接器，包装器 `~/workspace/tools/zig-cc-arm.sh` 过滤 cc-rs 传的 `--target=arm-unknown-linux-musleabihf`，另需 `RUSTFLAGS="-C link-self-contained=no"`；rustls 相关依赖均 `default-features = false` 只开 `ring`（否则 `aws-lc-sys` 数百 MB C 编译堵死交叉编译，`cargo tree -i aws-lc-rs` 已无任何包）。

## Phase 2 — 就近仲裁 + Snapcast + 网易云

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| 仲裁（纯函数+窗口计时） | `go test ./internal/arbitration/ ./...` | 见 Phase 1 分包表：选 RMS 最大、窗口真计时、赢家+输家正确 | 通过 | 真机两台同时唤醒的实测（需 bai 硬件） |
| Snapcast 融进 Hub | `go test ./internal/music/` | 假 Snapcast 服务：分组、延迟校准调用、静音/音量均验证 | 通过 | 真 Snapcast 两台同步播放（需 bai 硬件） |
| 网易云（MA + NeteaseCloudMusicApi） | `go test ./internal/music/ ./internal/router/` | 假 MA/网易云/MPD 服务：搜歌、取 URL、点歌、降级路径均验证 | 通过 | 网易云扫码登录（需 bai 账号） |

## Phase 3 — 语音体验

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| 连续对话 + 打断 | `go test ./internal/voice/` | VAD 800ms 静音结算；audio_end 强制结算；打断逻辑在 `main.go`（播放中收到 wake 即 interrupt）——单元级已覆盖，端到端待真机 | 通过 | 真机打断率（需 bai 硬件） |
| HA 接入 | `go test ./internal/ha/ ./internal/router/` | 假 HA 服务：查状态/调服务/场景（`scene.coming_home`）；先查状态再调，不重复调 | 通过 | 真 HA 联调（需 bai 的 HA） |
| 循环提醒 FR-14 | `go test ./internal/reminders/ ./internal/router/` | 调度、持久化、语音取消均验证 | 通过 | 无（到点 TTS 播报需真机听） |
| 对讲半双工 | `go test ./internal/intercom/` | 忙拒绝、中继、广播、超时清掉均验证 | 通过 | 真机两台对讲（需 bai 硬件） |
| AEC（OwnDing 子带思路移植，单带 NLMS） | `go test ./internal/aec/` | 合成回声残余 < 50% | 通过 | 真机实测回声抑制率；子带升级待真机测量后定 |

## Phase 4 — 产品化

| 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|
| Web 面板 + config 读写 | `go test ./internal/web/` + Hub 冒烟 | /api/status、/api/devices、config GET/PUT（含校验失败拒绝）、OTA POST 均验证；真二进制 `:18080` 可访问 | 通过 | 无 |
| 一键 OTA（失败回滚 boot1） | `go test ./internal/ota/ ./internal/web/` | 状态机 Push→Verifying→OK/Failed、超时扫荡、Web 下发均验证 | 通过 | 真机刷机验证（需 bai 硬件）；回滚本身是设备端 boot1 行为 |
| 对讲全双工 | — | 未实现全双工切换，当前为半双工（见 Phase 3）；全双工需真机验证回声路径 | 未做 | 需真机验证 |
| 紧急呼叫 FR-15（可选） | `go test ./internal/router/` | “救命/打 120/叫救护车/打 110/报警”触发 Notify + 回复；取消路径为语音“取消呼叫”（Router 侧未接取消状态机） | 部分 | 真机验证；取消状态机待补 |
| 摄像头主动播报 FR-10 | — | `main.go` 已接 HA `state_changed` 订阅（leak/smoke/water → 播报，勿扰时段跳过），无自动化测试 | 未测 | 真 HA 摄像头联调（需 bai 硬件） |

## 第二轮补做（2026-11-17，用户问“除了刷机还能先做完哪些”后；复核人独立重跑确认）

| # | 内容 | 测试命令 | 输出摘要 | 结果 | 阻塞项 |
|---|---|---|---|---|---|
| A | OH2P 补丁验证 | `download-ota.py` 下 1.58.6 → `unpack.sh oh2p 1.58.6` → `patch.sh oh2p 1.58.6`；`pytest tests/ -q` | 官方包 36,042,824 字节，MD5 `eb86c0c12ea118fb701433bf009a180c`，与 2026-11-16 验证值一致；镜像 `firmware-patch/work/oh2p_1.58.6/root-patched.squashfs` **30,175,232 字节 < 40MB**，SHA256 `db00add0ad5674ba8efda41b647a5b1b4508a6fb4d54fa554f2526614fff0423`；pytest **36 passed**（35 + 新增 OH2P 回归测试 1 项）；LX06 镜像重跑后 SHA256 仍为 `cc36b28e…54f06d` | 通过 | 真机刷机/开机/SSH/Hub 连接 |
| B | 对讲全双工 | `go test ./...`（hub/） | `SetFullDuplex`/`SetHalfDuplex` 切换保留会话；全双工双向中继；`EchoCancel` 合成回声输入 RMS 2832.0 → 残余 89.9（3.2%）< 50%；全量 go test 88 通过 | 通过 | 真机回声实测；Opus 级回声接线（decode→AEC→re-encode）未做 |
| C | FR-15 取消状态机 | `go test ./...`（hub/） | 触发→取消、触发→60s 超时清理、重复触发不重复 Notify、同设备隔离取消；全量 go test 88 通过 | 通过 | 真实通知链路（HA fire_event → 电话网关/推送）10s 内送达未验证；真机语音取消 |
| D | FR-10 自动化测试 | `go test ./...`（hub/） | 新包 internal/alerts：6 测试——播报文本（类型+位置）、勿扰跳过（含边界）、非安全类不播报、假 HA WS 推真信封事件端到端；全量 go test 88 通过 | 通过 | 真 HA 联调（实际触发、“漏水 5s 内播报”时延） |
| E | ALSA 接线（Client） | `cargo test`；`./build-armv7.sh` | `cargo test` **37 passed**（32 + 新增 ALSA 5 项）；trait 隔离硬件，Fake 设备测试；无硬件返回明确错误、不崩溃；ARMv7 交叉编译通过，产物 ELF 32-bit ARM 静态链接 **2,852,620 字节**（含 musl libasound，仍 <5MB） | 通过 | 真音箱 `hw:0,0` 采集 / `hw:0,2` 播放实测 |
| F | KWS“你好小李”.onnx 模型验证 | Python 实验（sherpa-onnx 2.0.1）+ `cargo test --features sherpa-kws` | 模型 sherpa-onnx-kws-zipformer-wenetspeech-3.3M（Apache 2.0），int8 集 5,708,052 字节；**合成语音正 5/5 检出、负 5/5 静默**（SNR 5dB 加噪仍检出）；Rust feature `sherpa-kws` 接入，`cargo test --features sherpa-kws` **41 passed**；复核人用真实模型 + 真实 .so + 真实 TTS 样本重跑端到端测试通过（非 SKIP）；模型文件未进仓库，下载脚本 client/scripts/download-kws-model.sh + 文档 client/kws/README.md 已写 | 通过（仅合成语音） | ARM 版 `libsherpa-onnx-c-api.so` 待交叉编译；真人唤醒率（1m/3m ≥95%）与 ARM 真机性能 |
| G | NAS 部署包 | `go build -o deploy/hub-linux-amd64`；YAML 校验；`bash -n` | Hub linux/amd64 二进制 11,534,848 字节，冒烟 `--help` 退出 0、`/api/status` 返回 wake_word“你好小李”；docker-compose 镜像逐个核实：**binarytorch/neteasecloudmusicapi 不存在（404）→ 改为官方 binaryify/netease_cloud_music_api**；另修 hub 无 Dockerfile、缺 OPEN_XIAOAI_TOKEN 两个必死错；deploy/nas-install.sh（bash -n OK）+ NAS_CHECKLIST.md 已写 | 通过 | NAS 上实际运行 |
| H | 网易云登录准备 | 文档核实（MA 官方文档） | deploy/README.md 新增扫码登录流程：扫码点在 MA 界面（QR Login）、登录态存 MA 侧 ma-data 卷、Cookie 约 30 天过期重扫、VIP 歌需 VIP 账号；未尝试登录 | 通过 | bai 的网易云账号扫码 |

### 第二轮发现并修复的 2 个真实问题
1. `download-ota.py` 的 `main()` 读 `info["currentInfo"]`，但 API 实际返回 `info["data"]["currentInfo"]`，首次运行报 `KeyError`（测试 mock 编成扁平结构把 bug 藏住了）。已改脚本 + 两个 mock 改为真实嵌套结构，断言本身未动；LX06 镜像重跑 SHA 不变。
2. FR-10 旧接线读扁平的 `ev["entity_id"]` / `ev["new_state"]`，但真 HA 的 state_changed 事件是 `event.data.*` 嵌套信封——旧接线在真 HA 下永远不会触发。已改 `ShouldAnnounce` 两种形状都认（嵌套优先、扁平兼容），`SubscribeEvents` 的错误此前被 `_` 丢弃，现在记日志。

### 第二轮复核人重跑结果（2026-11-17）
- `go test -count=1 ./...`：20 个包 ok，**88 通过、0 失败**；`go vet` / `go build` OK。
- `cargo test`：**37 passed**；`cargo test --features sherpa-kws`：**41 passed**；sherpa 端到端测试用真实模型 + 真实 .so + 真实 TTS 样本重跑通过（非 SKIP）。
- `pytest tests/`（firmware-patch）：**36 passed in 65.57s**。
- OH2P 镜像 30,175,232 字节、SHA256 `db00add0…f0423` 已核对；LX06 镜像 SHA256 仍为 `cc36b28e…54f06d`；firmware/lx06_1.94.13.bin MD5 仍为 `b202de2bbe149ce884aa500911151b95`；ARM 产物 2,852,620 字节 ELF 32-bit ARM 静态链接已核对；deploy/hub-linux-amd64 11,534,848 字节 ELF 64-bit 已核对。

## 审计修复（2026-11-17）

审计报告 AUDIT_REPORT.md（108 项：P0 6 / P1 46 / P2 56）之后，前一波 6 个工人修了 12 项 + lint 规则 + CI；本轮（收尾）补齐 main.go 遗留接线、客户端默认 Token、lint 全绿与文档同步。本节逐项写修复内容、测试命令、精确输出、结果与剩余阻塞。

说明：前一波 6 个工人的汇报原文不在本轮上下文里，本节的“修复内容”以代码实测为准（逐项 grep/读代码核实落地点），“测试命令/精确输出”全部是本轮亲自重跑的结果，不是转抄旧汇报。凡在 VM 上无法完整验证的部分，写清验证边界。

### 12 项逐项

| # | 审计项 | 修复内容（落地点已核实） | 测试命令（本轮重跑） | 精确输出 | 结果 | 剩余阻塞 |
|---|---|---|---|---|---|---|
| 1 | P0-1 Web 零鉴权 | `hub/internal/web/web.go`：`/api/*` 全部要求 Bearer（与 `OPEN_XIAOAI_TOKEN` 独立的 `OPEN_XIAOAI_WEB_TOKEN`），未配 Token 时一律 401（fail closed）；恒定时间比较 + 单 IP 失败限流；`NewHTTPServer` 带 `ReadHeaderTimeout/ReadTimeout/WriteTimeout` 与 `MaxBytesReader` 请求体上限；`WebListen` 默认 `127.0.0.1:8080`（`config.go:323-325`，`hub/config.yaml:71` 已改 `127.0.0.1:8080`） | `go test -count=1 ./internal/web/`（`hub/`） | `ok`，10 passed，0 failed | 通过 | NAS 上 Web 实际监听地址未实测（关口 2）；GET `/api/config` 是否脱敏敏感字段未在本轮核对 |
| 2 | P0-2 默认 root 密码 | `firmware-patch/scripts/patch_firmware.py`：删除 `DEFAULT_PASSWORD`，`SSH_PASSWORD` 未设时交互式 `getpass` 读（≥16 位）或直接失败；`md5crypt $1$` 换成 `sha512_crypt`（5000 轮，随机盐）；`main()` 不打印 `login_hash`；`--password` 只从环境变量/`getpass` 读 | `python3 -m pytest tests/ -q`（`firmware-patch/`） | `61 passed in 103.36s` | 通过 | 新密码机制只在 VM 内测过补丁流程，真机刷机后 SSH 密码是否生效待关口 1 |
| 3 | P0-3 Token + mTLS | `hub/internal/transport/transport.go`：新增 `ValidateToken`（空 / <16 位 / 弱值表即错，错误含 `openssl rand -hex 16`）、Token 比较改 SHA-256 后恒定时间比较；`hub/internal/tlscert/tlscert.go`：新增 `LoadOrCreateMTLS`（自有 CA + CA 签发的服务端证书 + 共享客户端证书 CN `open-xiaoai-client`）；`transport.Server.SetClientCAs` + `Serve` 用 `RequireAndVerifyClientCert`；`deploy/nas-install.sh` 预检查拒绝弱 Token；`client/src/transport.rs`：`connect_once` 建连前 `validate_token` 拒绝弱 Token；`client/src/config.rs` 默认 Token 改 `""`（本轮，fail closed）。**本轮补的 main.go 接线**：启动时调 `transport.ValidateToken`（弱 Token 即 Fatal；只能放 main.go，绝不能放 `transport.New/Serve`，集成测试仍用短 Token `"secret"`）；改调 `tlscert.LoadOrCreateMTLS(cfg.Hub.DataDir)`，再 `server.SetClientCAs(pool)` 后 `Serve(ln, &cert)` | `go test -count=1 ./internal/transport/`；`go test -count=1 ./internal/tlscert/`；`cargo test`（`client/`）；真二进制冒烟：`/tmp/mtls-smoke/hub -config …`（`tls: true`，`:14399`）+ `openssl s_client` | transport `ok`，8 passed；tlscert `ok`，6 passed；cargo `46 passed; 0 failed`（含 `connect_once_rejects_weak_token_without_dialing`、`wss_mtls_with_and_without_client_cert`）；冒烟：启动日志 `wss 已启用（mTLS：要求客户端证书），Hub 证书指纹：cbeefcc87f0dcdbbe5f6199d3c0257002ca5d41e474581798eddf8f573585c5b`；无客户端证书握手被拒（`tlsv13 alert certificate required`，Hub 日志 `http: TLS handshake error …: tls: client didn't provide a certificate`）；带 CA 签发客户端证书握手成功（`New, TLSv1.3, Cipher is TLS_AES_128_GCM_SHA256`，`Verify return code: 0 (ok)`）；弱 Token（`CHANGE_ME`）启动即 Fatal：`OPEN_XIAOAI_TOKEN 无效: OPEN_XIAOAI_TOKEN 太短（9 位，至少 16 位）；生成一个：openssl rand -hex 16` | 通过 | 客户端证书随固件下发到两台音箱的路径（`gen-certs.sh` / `patch_firmware.py --cert-dir`）未在真机验证；旧自签 `hub.crt` 被 `LoadOrCreateMTLS` 重新签发覆盖后指纹会变，`client.toml` 的 `hub_ca_fingerprint` 需同步更新；`CheckOrigin` 恒 true 与 `deviceID` 顶替（P1-3）未修 |
| 4 | P0-4 发现广播 Token | `hub/internal/discovery/discovery.go`：扫描默认关闭（`Enabled=false` 时 `Scan` 直接拒绝）；先匿名探测（只建 TCP + TLS 握手取叶子证书指纹，不发应用层数据、不发 Token），指纹在 `ExpectedFingerprints` 白名单内才建 wss 连接发 Token；`hub/cmd/hub/main.go` 的 `newDiscoveryScanner`：只在配置显式开启 + 配了指纹白名单 + TLS 开启时才扫描，端口取 `cfg.Hub.Listen`（不写死 4399） | `go test -count=1 ./internal/discovery/` | `ok`，9 passed，0 failed | 通过 | P1-44 未修：音箱是 WS 客户端、根本不监听 `:4399`，生产扫描仍找不到设备（发现目前只对有 `:4399` TLS 服务的候选有意义）；真机发现实测待关口 1 |
| 5 | P0-5 OTA 明文下载 | `firmware-patch/scripts/download-ota.py`：接口改 `https://api.miwifi.com/…`；`EXPECTED` 白名单为强制门（不在白名单的 `(model, version)` 拒绝下载）；下载链接强制 `https` + host 白名单（`cdn.cnbj1.fds.api.mi-img.com`，`check_download_link`）；SHA256 白名单校验（MD5 只作兼容参考）；带超时的流式下载，先下 `.tmp`、校验通过再 rename，失败删临时文件；`verify_local` 供 `unpack.sh` / `patch.sh` / `flash-check.sh` 调用 | `python3 -m pytest tests/ -q`（`firmware-patch/`，含 `test_download_ota.py`） | `61 passed in 103.36s` | 通过 | 其它版本无白名单锚点（新增版本需人工核实 SHA256 后更新 `EXPECTED` 再下载）；`unpack.sh` / `patch.sh` 的哈希校验（P1-8）未在本轮修 |
| 6 | P0-6 刷机流程 | `firmware-patch/FLASH_RUNBOOK.md`（书面 runbook：只写非当前槽 system 分区、刷前 `flash-check.sh`、读 `boot_part`、备份 boot1、刷后校验、回滚演练）+ `firmware-patch/scripts/flash-check.sh`（干跑核对：官方包 / 原始 rootfs / `mico_version` / 补丁镜像大小与魔数 / SHA256） | `./scripts/flash-check.sh lx06 1.94.13`；`./scripts/flash-check.sh oh2p 1.58.6`（`firmware-patch/`，本轮重跑） | 两型号均 exit 0，`干跑检查通过：VM 上能做的校验都过了`；LX06：`PASS 官方 .bin 白名单 SHA256/MD5/大小`、`PASS 原始 root.squashfs 白名单 SHA256/大小`、`PASS mico_version 含 LX06 1.94.13`、`PASS 补丁镜像大小在 (0, 40MB) 内`、`PASS squashfs 魔数 hsq s`、`PASS unsquashfs -s 可解析`；补丁镜像 SHA256 打印为 `7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3`（LX06） | 通过（VM 干跑部分） | **真机步骤全部 BLOCKED**（读 `boot_part`、备份分区、刷后校验、回滚演练，见 runbook §8）；另注意：`flash-check.sh:48-49` 的 `KNOWN_PATCHED_SHA256` 仍是旧值（LX06 `cc36b28e…`、OH2P `db00add0…`），对新镜像会打一条 `WARN 补丁镜像 SHA256 与记录值不同`（只警告、不拦截，exit 仍为 0）——本轮按任务范围只同步了 runbook，未改该脚本，见下文“本轮收尾工作” |
| 7 | P1-1 Wyoming 读上限 | `hub/internal/wyoming/wyoming.go`：`parsePayloadLength` 校验（非数字 / 负数 / 非整数 / 超 1MB 一律报错）；`readHeaderLine` 限长读；`readEvent`/`writeEvent` 全程 10s 读写 deadline；`Transcribe`/`Synthesize` 总量上限。本轮另修了 3 处 `defer SetReadDeadline/SetWriteDeadline` 未判错（errcheck，`_ =` + 注释，见“本轮收尾工作”） | `go test -count=1 ./internal/wyoming/` | `ok`，7 passed，0 failed（含 `TestReadEventRejectsOversizeHeaderLine`） | 通过 | 无 |
| 8 | P1-2 STT/TTS nil | `hub/internal/voice/voice.go`：`answer()` 开头判 nil，返回明确错误（`STT 不可用（Whisper 未连接），请稍后再试` / `TTS 不可用…`）；`hub/cmd/hub/main.go`：`OnAudioFrame`/`OnAudioEnd`/`speakText` 遇语音服务错误播一句降级回复（“语音服务暂时没连上，请稍后再试”），`speakText` 在 TTS 为 nil 时只记日志、不 panic | `go test -count=1 ./internal/voice/` | `ok`，10 passed，0 failed | 通过 | 单句语音上限（`Frame` 的 `u.pcm` 累积）是否已加未在本轮核对；属 P1-2 同条建议，见“遇到的其他审计项” |
| 9 | P1-26 点歌/Snapcast 接线 | `hub/cmd/hub/main.go`：启动时查 MA `Players()` 取默认 `player_id` 作 `QueueID`（查不到记日志、点歌走 MPD 降级）；`music.DialMPD(cfg.Hub.MPD)` 注入 `MPD`；`music.DialSnap(cfg.Hub.Snapcast)`；MPD/Snapcast 启动时没连上时后台每 30s 重试、连上即经 `rt.SetMPD`/`rt.SetSnap` 补接 | `go test -count=1 ./internal/router/`；`go test -count=1 ./internal/music/` | router `ok`，17 passed；music `ok`，5 passed | 通过 | MA 恢复后 `QueueID` 不热更新（需重启 Hub，见 P1-27）；真机两台同步播放延迟差 ≤10ms 未验证（关口 3） |
| 10 | P1-31 卷挂错位 | `deploy/docker-compose.yml`：卷改挂到 `/app/hub-data`（Hub 数据目录就是 `./hub-data`，`WORKDIR /app`），原来挂 `/app/data` 没人用 | 仅核对 compose 文件（VM 内无 NAS 可验证） | `hub-data:/app/hub-data` 已确认在 compose 里；`hub/config.yaml:56` 的 `data_dir: "./hub-data"` 与之对应 | 通过（文件核对） | **NAS 实测 BLOCKED**（关口 2：容器重建后提醒/记忆/TLS 证书指纹不变，需在 NAS 上验证） |
| 11 | P1-32 config `:ro` | `deploy/docker-compose.yml`：挂载去掉 `:ro`（改可写）；`hub/cmd/hub/main.go` 的 `OnRoomClaim`：`Save` 失败记日志，不再吞掉。本轮另改了 `deploy/Dockerfile.hub` 里“运行时由 docker-compose.yml 挂载（只读）”的过时注释，与现状一致 | 仅核对 compose 文件与 main.go（VM 内无 NAS 可验证） | `../hub/config.yaml:/app/config.yaml`（无 `:ro`）已确认 | 通过（文件核对） | **NAS 实测 BLOCKED**（关口 2：语音认领房间重启后仍在，需在 NAS 上验证）；可写/种子配置分离（P1-22/P1-23 重构范围）未修 |
| 12 | P1-40 ARM 无 KWS | `client/build-armv7.sh`：默认开 `--features sherpa-kws`（`--no-sherpa-kws` 仅供应急构建），构建末尾断言产物含 sherpa 标记（`open-xiaoai-kws-backend=sherpa-onnx-2.0.1` 等 3 个）与 ARM `.so` 的 10 个必需导出符号；`client/src/kws.rs`：feature 开时 `Kws::new` 直接返回 Err——生产路径在 sherpa 后端加载失败后调到这里，Err 经 `?` 向上传播，进程以非零码退出并打印原因，绝不静默跑 RMS；ARM 版 `libsherpa-onnx-c-api.so` 已取（`client/kws/vendor-arm/`，sherpa-onnx 官方 v2.0.1 `linux-arm-gnueabihf-shared`，SHA256 `46d5250538b5bcf97d921204f7fe61eca7bac119ad2dcb1c95b557a314d8bc6b`，`libonnxruntime.so` SHA256 `4636872e9b50f985b7ecaa55a6829dee81bdb96f77010b5bc0a4059e51d872dc`） | `./build-armv7.sh`（`client/`，本轮重跑）；`cargo test --features sherpa-kws`（`client/`） | `./build-armv7.sh` exit 0（`Finished release profile in 39.88s`）；`ASSERT OK: ARM binary contains sherpa-kws markers (feature enabled)`；`ASSERT OK: kws/vendor-arm/libsherpa-onnx-c-api.so exports all 10 required KWS symbols`；产物 `target/armv7-unknown-linux-musleabihf/release/open-xiaoai-client` 2,885,364 字节，`ELF 32-bit LSB executable, ARM, EABI5, statically linked, stripped`；`cargo test --features sherpa-kws`：`50 passed; 0 failed` | **部分通过：构建与断言通过，真机唤醒仍 BLOCKED**（见下） | **P1-40 真实状态——按审计要求已做，但真机唤醒仍 BLOCKED**：官方 ARM `.so` 要求 `GLIBC_2.29`（音箱最高 2.25）且 `GLIBCXX_3.4.29`（音箱最高 3.4.22），`build-armv7.sh` 本轮重跑即打印 `WARNING: kws/vendor-arm/libsherpa-onnx-c-api.so requires GLIBC_2.29, speaker has glibc 2.25 — will NOT load on-device as-is`（GLIBCXX 同理）；且 musl 静态 Client 不支持 `dlopen`（x86_64 同构实测：musl 静态程序 `dlopen` 直接失败 `Dynamic loading not supported`，exit 2）。即：哪怕拿到版本兼容的 `.so`，不改 Client 链接方式也加载不了。出路二选一，待决策：(a) Client 改动态链接 musl（或 static-PIE）+ 交叉编译 musl 版 sherpa/ORT（ORT 没有 musl 预编译，需从源码编，工作量大）；或 (b) Client 改 `armv7-unknown-linux-gnueabihf`（glibc 2.25 目标，zig 可指定 `-target arm-linux-gnueabihf.2.25`）+ 从源码重编 sherpa（复用官方 `libonnxruntime.so`，它只要求 GLIBC_2.4；音箱自带 glibc 2.25 与 `/usr/lib/libasound.so.2`，glibc 路线有现成地基）。证据在 `client/kws/vendor-arm/SOURCE.md` 与 `client/kws/README.md`。在决策落地前，刷进音箱的 Client 在 FR-01 上仍是零能力（只会上报 RMS，永不产生 `wake` 事件），且 `cargo test --features sherpa-kws` 的 50 通过全部是 x86_64 宿主机上的验证，不能当作 ARM 真机唤醒的证据 |

### 本轮收尾工作（2026-11-17，不属于 12 项，是收尾补齐）

| 内容 | 改动 | 测试命令（本轮重跑） | 精确输出 | 结果 |
|---|---|---|---|---|
| main.go 遗留接线（P0-2/P0-3 收尾） | `hub/cmd/hub/main.go`：启动时调 `transport.ValidateToken`（弱 Token 即 Fatal，错误含 `openssl rand -hex 16`；只放 main.go 启动处，不进 `transport.New/Serve`）；改调 `tlscert.LoadOrCreateMTLS(cfg.Hub.DataDir)`，再 `server.SetClientCAs(pool)` 后 `Serve(ln, &cert)` | `go test -count=1 ./...`（`hub/`）；`go vet ./...`；真二进制 mTLS 冒烟（见 P0-3 行） | `go test`：21 个包 ok，**131 passed，0 failed**；`go vet`：`VET_OK`；冒烟见 P0-3 行 | 通过 |
| 客户端默认 Token 与过时注释 | `client/src/config.rs:15`：默认 Token `CHANGE_ME` 改为 `""`（fail closed；`transport.rs` 的 `connect_once` 已在建连前拒绝弱 Token，空值同样拒绝；未动 `transport.rs`）。`client/src/main.rs`：CHANGE_ME 警告处加注释（默认值已不是 CHANGE_ME，只拦显式配错）；KWS 注释改写为现状（feature 开时无后端即 `Kws::new` 返回 Err、非零退出，不回退纯 RMS）。只改注释，未改行为代码 | `cargo test`；`cargo test --features sherpa-kws`（`client/`） | `46 passed; 0 failed`；`50 passed; 0 failed` | 通过 |
| lint 全绿 | `gofmt -w` 修 8 个格式文件（`hub/integration_test.go`、`internal/ha/ha.go`、`internal/music/music.go`、`music_test.go`、`internal/ota/ota.go`、`internal/reminders/reminders.go`、`internal/voice/voice.go`、`voice_test.go`）；`cargo fmt` 修 client 格式漂移；修 5 处小问题（`wyoming.go` 3 处 `defer SetReadDeadline/SetWriteDeadline` 未判错、`wyoming_p1_test.go` 2 处 `writeRaw` 未判错，均 `_ =` + 注释；`ha_test.go:22` 未用字段 `eventCh` 删除——删字段后对齐漂移，顺带 `gofmt -w` 该文件；`router_test.go:276` 无效赋值 `reply` 改为首调不取 `reply`、次调 `:=`）；`firmware-patch/scripts/download-ota.py` 第 130 行与 193 行的 `urlopen` 各加 `# nosemgrep: open-xiaoai-r5-urlfetch-no-allowlist` 标注（文件本身已做对：https + 白名单 + 超时，只差标注）；`scripts/check-wiring.go` 规则 1 加 allowlist（`ota.ConfirmHello`、`ota.MarkRebooted`、`otaMgr.Sweep`、`intercom.Start`、`intercom.Sweep` 5 项，理由“deferred until after first gate（bai 2026-11-17 批复）”，条目保留在检查表里）；`scripts/check-bounds.go` 规则 3 对 `hub/internal/music/music.go` 的 `SnapClient.call()` 无界 `ReadBytes` 加已知例外标注（暂缓审计项，不在本轮 12 项内，只记录、不修） | `scripts/lint.sh`（本轮重跑，基线 exit 1 → 修后 exit 0） | `lint: 全部通过`（gofmt / go vet / golangci-lint / 规则 1 / 规则 2 / 规则 3 / Semgrep / cargo fmt / clippy 全 ok；规则 1 打印 5 条 `DEFERRED` 提醒后 `rule1: OK（45 项接线检查全部通过）`；规则 3 打印 1 条 `KNOWN-EXCEPTION` 后 `rule3: OK`） | 通过 |
| Dockerfile.hub 过时注释 | `deploy/Dockerfile.hub`：`config.yaml 运行时由 docker-compose.yml 挂载（只读）` 改为可写挂载说明（P1-32 已去 `:ro`，Hub 运行时要写它） | 核对 `deploy/docker-compose.yml:29`（`../hub/config.yaml:/app/config.yaml`，无 `:ro`） | 已确认一致 | 通过 |
| 补丁镜像 SHA 同步 | 镜像已重编（前一波 P0-2 修复改了补丁内容），新 SHA 已在磁盘上核实：LX06 38,453,248 字节 SHA256 `7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3`；OH2P 30,179,328 字节 SHA256 `139de363d39d0a5472bf9b00c015effd9a24c379ca5fa2436cc21c43e8706415`；旧 SHA（LX06 `cc36b28e…54f06d`、OH2P `db00add0…f0423`）已被取代。`firmware-patch/FLASH_RUNBOOK.md` §1 与本节已同步新值 | `sha256sum firmware-patch/work/lx06_1.94.13/root-patched.squashfs firmware-patch/work/oh2p_1.58.6/root-patched.squashfs`；`./scripts/flash-check.sh <型号> <版本>` | `sha256sum` 输出与上表一致；`flash-check.sh` 两型号均 exit 0（见 P0-6 行，含旧值 WARN 提醒） | 通过（VM 内）；真机刷机仍 BLOCKED |

### 本轮重跑总表（2026-11-17，收尾后）

| 命令 | 精确输出 | 结果 |
|---|---|---|
| `go test -count=1 ./...`（`hub/`） | 21 个包 ok，**131 passed，0 failed**（分包：transport 8、tlscert 6、web 10、wyoming 7、voice 10、discovery 9、router 17、intercom 8、ota 4、music 5、config 8、ha 5、agent 3、集成 3，余为 aec/alerts/arbitration/opus/registry/reminders/cmd/hub） | 通过 |
| `go vet ./...`（`hub/`） | `VET_OK`，无输出报错 | 通过 |
| `cargo test`（`client/`） | `test result: ok. 46 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 1.61s` | 通过 |
| `cargo test --features sherpa-kws`（`client/`） | `test result: ok. 50 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 1.61s` | 通过 |
| `python3 -m pytest tests/ -q`（`firmware-patch/`） | `61 passed in 103.36s (0:01:43)` | 通过 |
| `./build-armv7.sh`（`client/`） | exit 0；`ASSERT OK: ARM binary contains sherpa-kws markers (feature enabled)`；`ASSERT OK: kws/vendor-arm/libsherpa-onnx-c-api.so exports all 10 required KWS symbols`；产物 2,885,364 字节 ELF 32-bit ARM 静态链接 stripped；另打印 `WARNING: … requires GLIBC_2.29, speaker has glibc 2.25 — will NOT load on-device as-is`（GLIBCXX_3.4.29 同理），见 P1-40 行 | 通过（断言部分）；真机唤醒 BLOCKED |
| `scripts/lint.sh` | exit 0，`lint: 全部通过` | 通过 |

说明：`TestArbiterWindow`（arbitration 包）的时序抖动在本轮重跑中**没有复现**（`go test -count=1 ./...` 全量跑了 4 次以上，131 通过、0 失败），只记录、不弱化测试。`go test` 总数从第二轮的 88 涨到 131，是前一波审计修复新增的测试（Web 鉴权、Token 校验、mTLS、发现、Wyoming 边界等），不是旧测试改名。

## P1-40 方案 B — 客户端切换到 gnueabihf（2026-11-17，本节只写客户端切换）

决策：客户端从 `armv7-unknown-linux-musleabihf`（musl 静态链接，不能 dlopen）
切换到 `armv7-unknown-linux-gnueabihf`（glibc 2.25，zig 作链接器）。本节只记
客户端切换的构建与验证；`.so` 从源码重编是另一路的工作，本节不替它写结论。

| 内容 | 测试命令 | 精确输出 | 结果 |
|---|---|---|---|
| 目标安装 | `rustup target add armv7-unknown-linux-gnueabihf` | 已装：`armv7-unknown-linux-gnueabihf`（与 musleabihf、x86_64 并存） | 通过 |
| zig 包装器冒烟 | `zig-cc-arm-gnueabihf.sh` 编最小 C 程序链接 rootfs 的 `libasound.so.2` | `ELF 32-bit LSB executable, ARM, EABI5`，解释器 `/lib/ld-linux-armhf.so.3`，NEEDED `libasound.so.2` + `libc.so.6`，最高 GLIBC_2.4 | 通过 |
| gnueabihf 构建 | `./build-armv7-gnueabihf.sh`（`client/`，强制重链接重跑，40.63s） | exit 0；产物 `target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client` **2,435,964 字节**，SHA256 `2da754d48160906b65bb289925dd3dacdb83b16721f7690540e48f80dd37a00b`，`ELF 32-bit LSB pie executable, ARM, EABI5`，动态链接，解释器 `/lib/ld-linux-armhf.so.3`，stripped | 通过 |
| 构建末尾断言 | 同上（脚本内断言，失败即非零退出） | `ASSERT OK: ARM EABI5 hard-float dynamic binary`；`ASSERT OK: ARM binary contains sherpa-kws markers`；`ASSERT OK: max GLIBC version required is GLIBC_2.25`；`ASSERT OK: requires no GLIBCXX versioned symbols`；`ASSERT OK: RPATH/RUNPATH contains $ORIGIN/lib`；NEEDED 5 个库逐一 `found in speaker rootfs`；`ASSERT OK: libsherpa-onnx-c-api.so exports all 10 required KWS symbols`；另打印官方 `.so` 要求 GLIBC_2.29 / GLIBCXX_3.4.29 的 WARNING（见 BLOCKED） | 通过 |
| readelf 核查 | `readelf -h/-d/-V/--dyn-syms`（`client/`，本轮亲自跑） | ELF 头 `Type: DYN (PIE)`，`Machine: ARM`，`Flags: 0x5000400, Version5 EABI, hard-float ABI`；`RUNPATH $ORIGIN/lib`；NEEDED：`libasound.so.2`、`libm.so.6`、`libpthread.so.0`、`libc.so.6`、`libdl.so.2`，全部在 rootfs `/lib` 或 `/usr/lib` 里找得到；Version needs 最高 GLIBC_2.25，唯一的 2.25 符号是 WEAK 的 `getrandom`（glibc 2.25 自带，能解析），其余 ≤2.18；无 GLIBCXX 版本符号 | 通过 |
| 宿主机测试重跑 | `cargo test`（`client/`） | `test result: ok. 46 passed; 0 failed; finished in 1.64s`，与审计修复后基线 46 持平 | 通过 |
| 宿主机测试重跑 | `cargo test --features sherpa-kws`（`client/`） | `test result: ok. 50 passed; 0 failed; finished in 1.56s`，与基线 50 持平；含 fail-loud 回归 `kws_new_refuses_rms_only_when_sherpa_feature_on`（feature 开时 `Kws::new` 无后端即 Err，非零退出，不静默跑 RMS） | 通过 |

切换中修正的 1 个真实问题：首版断言用 `readelf -A` 的 `Tag_ABI_VFP_args`
判硬浮点，构建 exit 1——实测新产物 ELF 头 Flags 已是 `0x5000400, Version5 EABI,
hard-float ABI`（与 musl 产物、参考 kws 二进制、rootfs libasound 完全一致），
只是 LLVM 不出 `Tag_ABI_VFP_args` 这一属性。断言已改判 ELF 头 Flags，
`readelf -A` 不能当硬浮点判据（已写入 `client/README.md`）。

ALSA 接法（切换部分）：gnueabihf 产物动态链接音箱自带的 `libasound.so.2`
（链接期用 rootfs 抽出的 `libasound.so.2.0.0`，头文件用
`~/workspace/tools/libasound-arm/install/include/`，架构无关）；
`build-armv7-gnueabihf.sh` 每次运行重建 staging 目录 `.link-gnueabihf/`
（已进 `.gitignore`），alsa-sys 经定向 `.pc` 只认该 staging，不认宿主机
glibc 版；未从源码编 ALSA，未走 musl 静态库。部署形态：rpath `$ORIGIN/lib`，
部署清单见 `client/README.md`“部署清单”一节（以该节为准）。

未动：`firmware/` 原厂固件与已交付的补丁镜像均未碰（客户端二进制本来就
没有烤进镜像，刷机后另行拷贝到 `/data`，切换不需要重编镜像）；未推 GitHub；
musl 脚本 `build-armv7.sh` 保留作 fallback，仅文件头与 README 标注其为
musl 应急版，不再是默认交付。

BLOCKED（客户端切换侧）：真机唤醒率一律 BLOCKED（无音箱硬件、无 qemu-arm，
宿主机 46/50 通过不能当作 ARM 真机证据）；官方 ARM `.so` 仍要求 GLIBC_2.29 /
GLIBCXX_3.4.29，音箱只有 2.25 / 3.4.22，`.so` 从源码重编落定前，刷上去的
Client 会因 KWS 后端缺失非零退出（P1-40 要求的 fail-loud 行为）；ALSA
`hw:0,0` / `hw:0,2` 真机读写仍待刷机后验证。

### P1-40 方案 B — .so 勘测与证伪（2026-11-17）

**前提纠正（协调员实测）**：任务简报称“官方 libonnxruntime.so 只要求 GLIBC_2.4”是错的。`readelf -V`（只看 **Version needs** 段）：
- 官方 v2.0.1 `libsherpa-onnx-c-api.so`：GLIBC 最高 **2.34**（libc.so.6：2.32/2.34/2.33；libm.so.6：2.27/2.29）、GLIBCXX 最高 **3.4.29**。
- 同捆 `libonnxruntime.so`：GLIBC 最高 **2.34**（2.28/2.34/2.32/2.33/2.29/2.27）、GLIBCXX 最高 **3.4.29**。
- 音箱：glibc 2.25、libstdc++ 最高 GLIBCXX_3.4.22。**两个官方 .so 都无法在音箱上加载。**

**旧版勘测（结论：无可用版本）**：勘测记录 `~/workspace/.kws-exp/survey/SURVEY_REPORT.md`，下载物在 `~/workspace/.kws-exp/survey/`（约 155MB）。

| 候选 | 最高 GLIBC | 结论 |
|---|---|---|
| v1.10.34（最后一个有 linux-arm-gnueabihf-shared 的版本） | 2.29 | ❌ |
| v1.9.29（c-api 本身只要 2.4，但 core.so 与 ORT 要 2.29） | 2.29 | ❌ |
| v1.8.10（包内根本没有 KWS C API） | 2.29 | ❌ |

- 2.27–2.29 来自基础 libm/libc 符号（`exp/log/pow@2.29、expf/logf@2.27、fcntl@2.28`），任何用 glibc ≥2.27 的 sysroot 编出的构建都会带上，追更老版本也没用。
- GLIBCXX 不是瓶颈：三个候选的全部 .so 在 Version needs 段里没有任何 `GLIBCXX_*` 条目。
- 微软从未发布过 armv7 的 ORT 预编译包（v1.12.1–v1.16.3 逐一核查，只有 aarch64/x64）；sherpa 捆的 ORT 是它自己交叉编译的。
- **B 原义证伪**：“重编 sherpa C API + 复用官方 libonnxruntime.so”不成立；旧版预编译路线也全军覆没。

**qemu**：qemu-arm-static 8.2.2 已装通（`apt-get update && apt-get install -y qemu-user-static`，另装 armhf cross sysroot 库），跑 v1.10.34 的 ARM 版 `sherpa-onnx --help` 正常 exit 0。但该 sysroot 的 glibc 是 2.39，**只能证明 qemu harness 能跑，不能证明音箱兼容**；真兼容测试需要 glibc 2.25 的 sysroot。

**模型纠正**：KWS transducer 是 encoder/decoder/**joiner** 三件套（不止 encoder/decoder），opset 均为 13（2020 年版本，ORT ≥1.10 都支持），格式上极大概率兼容旧版 C API，但“能加载”未经实测，不能写成已证实。

**zig 探针（协调员亲测，源码重编路径的关键证据）**：`zig cc -target arm-linux-gnueabihf.2.25` 编一个用 `exp`/`log` 的 C 程序，产物最高 GLIBC 需求只有 **2.4**——zig 能产出 glibc 2.25 兼容的代码。但：音箱 rootfs **不能直接当 sysroot**（`/usr/include` 只有 Xiaomi 的 `mico` 目录，无 stdio.h/C++ 头文件/crt）；apt 的 armhf gcc 13.2.0 其 sysroot 是 glibc 2.39，无用；ORT 官方构建文档全文未提 zig，C++ 经 zig 走 CMake 未经验证。

**剩余两条路（待 Friday 复议决定，不擅自开编）**：
- (A) **源码重编 ORT + sherpa**：用 zig 2.25 目标（C 层已验证，C++/CMake 未验证；zig 自带 libc++ 静态链接可避开 GLIBCXX 版本问题，但未经验证）；还需把 sherpa 也重编（它默认从 HF 拉预编译 ORT，需改指自定义构建）；构建数小时，1.5 CPU/6GB 下 OOM 风险高。
- (B) **KWS 移出音箱**：唤醒放 NAS/Hub 端、音箱只做采音；或评估更轻的 KWS 引擎（未评估，仅方向提示）。

**协调员独立重跑（2026-11-17）**：`go test -count=1` **131 passed**；`cargo test` **46 passed**、`cargo test --features sherpa-kws` **50 passed**；`pytest` **61 passed**；`scripts/lint.sh` **全部通过**；新 gnueabihf 二进制 SHA256 与 readelf 复核与本节客户端部分一致（2,435,964 字节，GLIBC 最高 2.25，硬浮点，RUNPATH `$ORIGIN/lib`）；勘测记录已读，readelf 证据与上表一致。

**勘误（本节）**：(1) flash-check.sh 的 `KNOWN_PATCHED_SHA256` **已是新值**（LX06 `7266aba9…d97b3`、OH2P `139de363…6415`），关口 1 里“仍是旧值会打 WARN”已过时；(2) 客户端切换侧 BLOCKED 里“无 qemu-arm”已过时——qemu-arm-static 8.2.2 已装（/usr 易失，重启需重装）。

### 暂缓项（已批复，不在本轮范围）

- **P1-9 OTA 验证与回滚接线**：`ota.ConfirmHello` / `ota.MarkRebooted` / `otaMgr.Sweep` 在生产代码里仍零调用（OTA 任务会永久停在 `pushing`）；`check-wiring.go` 已加 allowlist（deferred until after first gate，bai 2026-11-17 批复），条目保留在检查表里，第一关口后再接线、再摘掉。客户端 `HubMsg::Ota` 仍只记日志，未实现“新版本启动后 30s 内重新 hello + 失败回滚 boot1”。
- **P1-15 对讲触发接线**：`intercom.Start` / `intercom.Sweep` 在生产代码里仍零调用（Router 无“呼叫/对讲”分支，`main.go` 无定时 `Sweep`）；同上 allowlist 暂缓。包内逻辑（半双工/全双工/`EchoCancel`）单测通过，但 FR-04 在生产上仍不可用。
- **P1-9/P1-15 之外的其余 96 项审计发现**（P1 46 项中除 P1-1/P1-2/P1-26/P1-31/P1-32/P1-40 外的 40 项、P2 56 项）未修，保持 AUDIT_REPORT.md 原样。

## 硬件关口检查清单（第一关口在最前）

| # | 关口 | 需要 | 状态 |
|---|---|---|---|
| 1 | **刷一台 LX06**：补丁镜像（`firmware-patch/work/lx06_1.94.13/root-patched.squashfs`，38,453,248 字节，SHA256 `7266aba99b769947bd3ce937c23f0dc0443ad52883f04e4992eed45c0d7d97b3`）刷入 boot0，保留 boot1 原厂回滚；刷后能开机、SSH 能进、Hub 能连上 | bai 的 LX06 + 拆机/刷机操作 | BLOCKED（**关口 1 前置条件变化**：P0 6 项已修完——Web 鉴权、root 强密码、Token + mTLS、发现不再广播 Token、OTA 走 https + 白名单、刷机 runbook 已写；两份新镜像已就绪（LX06 新 SHA 见本行，OH2P 见关口 8；旧 SHA LX06 `cc36b28e…54f06d`、OH2P `db00add0…f0423` 已被取代）；`flash-check.sh` 干跑两型号均通过（2026-11-17 联调时重跑：两型号均 `PASS 补丁镜像 SHA256 与 2026-11-17 实测值一致`，无 WARN——脚本内的 `KNOWN_PATCHED_SHA256` 已是新值，旧注已修正）；但仍需真机刷机——刷前必须用 `flash-check.sh` 核对新 SHA。另服务端 KWS 已落地（见 TEST_REPORT.md“服务端唤醒”一节），P1-40 真机唤醒仍 BLOCKED，见关口 11） |
| 2 | **Hub 在 NAS 上跑起来**：docker-compose 在 UGREEN DXP6800 Pro 上起 Hub + Wyoming（STT/TTS）+ Snapcast + MA + NeteaseCloudMusicApi | bai 的 NAS + docker | BLOCKED（部署包已就绪：deploy/hub-linux-amd64、Dockerfile.hub、nas-install.sh、NAS_CHECKLIST.md；镜像问题已修） |
| 3 | **两台同步放一首歌**：LX06 + OH2P 同组，Snapcast 同步播放网易云一首歌，延迟差 ≤10ms | 两台音箱都刷好 + NAS 服务全起 | BLOCKED |
| 4 | 真音箱 KWS：“你好小李”唤醒，本地出 wake 事件 | 刷好的音箱 + KWS 模型文件 | BLOCKED |
| 5 | 两台同时唤醒仲裁实测：近处响的赢、只回一台 | 两台刷好的音箱 | BLOCKED |
| 6 | 真机打断率：播放中唤醒打断，统计成功率 | 刷好的音箱 + 音乐播放中 | BLOCKED |
| 7 | 网易云扫码登录（MA / NeteaseCloudMusicApi 需要登录态才能播 VIP/完整曲库） | bai 的网易云账号扫码 | BLOCKED（流程文档已写好：deploy/README.md“网易云扫码登录”一节） |
| 8 | OH2P 端到端补丁验证：镜像已就绪（`firmware-patch/work/oh2p_1.58.6/root-patched.squashfs`，30,179,328 字节，SHA256 `139de363d39d0a5472bf9b00c015effd9a24c379ca5fa2436cc21c43e8706415`；旧 SHA `db00add0…f0423` 已被取代），剩上机刷机、开机、SSH、Hub 连接 | bai 的 OH2P + 刷机操作 | BLOCKED（仅剩真机部分；VM 内验证已通过，`flash-check.sh` 干跑 exit 0） |
| 9 | HA 真联调：查状态/调服务/场景/摄像头事件 | bai 的 Home Assistant | BLOCKED |
| 10 | ARMv7 交叉编译 | **已通过（新默认）**：`./build-armv7-gnueabihf.sh` exit 0，产物 ELF 32-bit ARM PIE 动态链接 2,435,964 字节（SHA256 `2da754d4…7a00b`），GLIBC 最高 2.25、硬浮点、RUNPATH `$ORIGIN/lib`，NEEDED 全部在音箱 rootfs 内（见“P1-40 方案 B”一节）；musl 版 `build-armv7.sh`（2,885,364 字节）保留作 fallback，不再是默认交付；未在真机跑过 | 已验证通过（VM 内），真机运行待关口 1 |
| 11 | KWS `.onnx` 模型文件 | 模型已验证：sherpa-onnx-kws-zipformer-wenetspeech-3.3M（Apache 2.0），合成语音正 5/5 检出、负 5/5 静默；Rust feature `sherpa-kws` 已接入；下载脚本 client/scripts/download-kws-model.sh + 文档 client/kws/README.md 已写；客户端已切 gnueabihf（产物已验，GLIBC ≤2.25，见“P1-40 方案 B”一节） | **服务端 KWS 已落地（2026-11-17，B 路已选定并实现，见本文件“服务端唤醒”一节）**：唤醒移出音箱、放 NAS 端 `kws-sidecar` 边车，Hub 解码 Opus 后转发 PCM；真边车 + 真 Hub 二进制端到端联调通过（合成语音正样本出 wake_win 进管线回答、负样本静默、双设备只赢一台、边车断线重连恢复；rms 语义已核对）。客户端 `kws_mode="server"` 默认持续推 Opus 流、无后端不退出（cargo 53/57 通过）；A 路（源码重编 ORT+sherpa）保留为未来可选项，本地 KWS 代码不删除；`KwsLogMonitor` 旁路方案已作废（见 `client/kws/README.md`）。**仍 BLOCKED**：真人唤醒率 1m/3m ≥95%、真机性能、真机两台仲裁实测——宿主机与合成语音的证据不能当作真机证据 |
| 12 | ALSA 采集/播放接线 | 已接线：`alsa` crate + trait 隔离（Fake 设备测试），无硬件返回明确错误、不崩溃；gnueabihf 产物动态链接音箱自带的 `libasound.so.2`（链接期用 rootfs 抽出的 `libasound.so.2.0.0`） | BLOCKED（需音箱硬件实测 `hw:0,0` 采集 / `hw:0,2` 播放、XRUN 恢复） |

## 未做 / 已知缺口（第二轮后更新）

- 对讲全双工已实现（第二轮 B）；FR-15 取消状态机已接（第二轮 C）；FR-10 自动化测试已补（第二轮 D）。剩余缺口：Opus 级回声接线未做（`EchoCancel` 是 PCM 级接口，main.go 对讲目前转发 Opus 帧，decode→AEC→re-encode + 播放参考对齐这一跳还没接）；FR-15 真实通知链路（HA fire_event → 电话网关/推送）10s 内送达未验证。
- `deploy/docker-compose.yml` 已修三个必死错（hub 无 Dockerfile、缺 OPEN_XIAOAI_TOKEN、netease-api 镜像 404），但仍未在 NAS 上验证过（见关口 2）；vimagick/mpd 默认配置是否输出 fifo 未核实；MA bridge 网络能否看到 Snapcast 播放器未核实（清单里有降级方案）。
- 补丁镜像比原 root.squashfs 大 952,549 字节（38,453,248 vs 37,500,699），仍在 40MB 内；原因未深究（疑似 mksquashfs 默认 xz 字典与原厂打包参数差异），不影响刷机。
- `hub/cmd/hub` 已有接线测试（`kws_wiring_test.go`：假边车 + 合成 PCM 流 → wake → 仲裁 → 管线回答、双设备仲裁、断线重连、KWS 挂了降级不断线、rms 换算；`main_test.go`），main.go 接线逻辑不再只有冒烟覆盖。
- KWS：**B 路已落地为服务端 KWS（2026-11-17，见本文件“服务端唤醒”一节）**：官方两个 ARM `.so` 均要求 GLIBC 2.34 / GLIBCXX 3.4.29（“ORT 只要求 GLIBC_2.4”是错的），旧版全部要求 GLIBC 2.29，无可用版本，B 原义（重编 sherpa + 复用官方 ORT）证伪后改走 B 的另一半——唤醒移出音箱、放 NAS 端边车；A 路（源码重编 ORT+sherpa）保留为未来可选项，本地 KWS 代码不删除；`KwsLogMonitor` 旁路方案已作废。真人唤醒率未测；Porcupine / openWakeWord 两条替代路线只写了流程文档，未注册账号、未录音。

## 服务端唤醒（2026-11-17，FR-01 从“音箱端”改为“NAS 端”）

FR-01 的唤醒词检测从音箱端改为 NAS 端：音箱只采音、持续推 Opus 流，
Hub 解码后把 16kHz PCM 发给 NAS 上的 `kws-sidecar` 边车做唤醒检测，
命中后走原有 250ms RMS 仲裁进语音管线。**仍是本地、仍离线**：音频不
出局域网，只有 LLM 文本出网（NFR-01 不变）；唤醒词、仲裁、路由逻辑不变。

### 为什么改（架构变化的理由）

- 复用已验证组件：边车复用实验 `~/workspace/.kws-exp/run_kws_test.py`
  已验证的 sherpa-onnx 2.0.1 + WenetSpeech zipformer 模型与“你好小李”
  token 行，不用再证一条新链路。
- 终结 ARM `.so` 问题：官方两个 ARM `.so` 均要求 GLIBC 2.34，旧版全部
  要求 GLIBC 2.29，音箱只有 2.25，无可用预编译版本；唤醒移出音箱后，
  `.so` 的 glibc 版本不再是关口。
- 128MB 机型同一条路：唤醒不再占音箱内存，不带 DTS 的 128MB LX06
  以后添置也能走同一条路（PLAN.md §2.1 的“128MB 精简 KWS”不再需要）。
- 局域网内本地离线：边车与 Hub 同在 NAS 上，音频不出家门；换词仍是
  换模型文件 + 改 `config.yaml`，不用重刷机。
- A 路保留为未来可选项：源码重编 ORT+sherpa（zig 2.25 目标）仍可日后
  做，本地 KWS 代码（`client/src/sherpa.rs`、`kws.rs` 的 sherpa 后端、
  `kws-models/` 下载脚本）**不删除**；`KwsLogMonitor` 尾随日志的旁路
  方案就此作废（服务端 KWS 后不再需要），作废原因已写进
  `client/kws/README.md`“过渡路线”一节与 `client/src/kws.rs` 文件头。

### 架构改动（三路，均在联调前已实现，本轮只联调验证）

| 路 | 落点 | 内容 |
|---|---|---|
| 边车 | `kws-sidecar/`（Python + sherpa-onnx） | WS `ws://127.0.0.1:10400/v1/kws`，HTTP `GET /health`（模型未好 503、加载失败进程非零退出）；每 `device_id` 独立 KeywordSpotter 流，连接断开即丢弃流状态；wake 事件 `{"type":"wake","device_id","keyword":"你好小李","rms":0.42,"timestamp_ms":...}`，其中 `rms` 是线性值 0~1（最近 1s 窗口）；模型不 bake 进镜像，首次启动由 entrypoint 下载到 `kws-data` 卷 |
| Hub | `hub/internal/kws/kws.go` + `hub/cmd/hub/main.go` | Opus 只解一次，解出的 PCM 同时喂边车与（会话期内）语音管线；每设备一路独立 WS 流，断线退避重连（500ms→30s）；wake 走 `OnKWSWake` → 同一条 `OnWake` → 250ms 仲裁 → `wake_win`/`wake_lose` → 赢家开 20s 会话；边车不可用时降级（日志 + `/api/status` 报告 `kws_down`，音频流重连后自动恢复），不阻塞启动；设备断开即清理其 KWS 流 |
| 客户端 | `client/`（`kws_mode="server"` 默认） | 持续推 Opus 流，无后端不退出（不回退纯 RMS、不非零退出）；`cargo test` 含 `server_mode_streams_continuously_without_wake_win` |

契约（已定，不许改）：Hub→边车首条 `{"type":"hello","device_id":"...","sample_rate":16000}`，
二进制 PCM int16 LE 16kHz（20ms = 640 字节）；边车→Hub `ready` / `wake` / `error`。

### 寻址统一（部署寻址不一致，已修并实测）

问题：Hub 在 compose 里是 `network_mode: host`，而 `hub/config.yaml` 的 kws url
原先写的是 `ws://kws-sidecar:10400/v1/kws`——host 网络下 compose 域名解析不到，
Hub 启动即报“唤醒服务暂不可用”。统一方案：**边车也走 host 网络 + url 用
`ws://127.0.0.1:10400/v1/kws`**（改动：`hub/config.yaml`、`deploy/docker-compose.yml`
的 kws-sidecar 服务改 `network_mode: host`、去掉 `ports` 映射，`hub/internal/kws/kws.go`、
`hub/internal/config/config.go` 的示例地址、`kws-sidecar/server.py` 与 `README.md`
的协议地址同步；`config_kws_test.go` 的默认地址断言同步更新；`nas-install.sh`
第 3 步加 sed，旧副本的 `ws://kws-sidecar:10400` 也会被改过来）。论证：边车只暴露
一个端口、无需 mDNS/组播发现，host 形态与 hub/snapserver/mpd 一致；127.0.0.1
在 NAS 与 VM 测试里同一写法。实测：VM 上真 Hub 二进制启动日志
`KWS 边车健康检查通过: ws://127.0.0.1:10400/v1/kws`，`/api/status`
`{"kws_status":"ok","kws_down":false}`。

### 端到端联调（真边车 + 真 Hub 二进制，2026-11-17）

方法：临时 harness（`hub/cmd/hub/e2e_kws_real_test.go`，联调完已删除，不进常驻
测试）：真实边车子进程（加载 `~/workspace/.kws-exp` 的真实 WenetSpeech 模型，
端口 10400）+ 真实 Hub 二进制（`tls: false`，`:14399`，kws 指 127.0.0.1:10400，
假 Wyoming STT/TTS + 假 Agent 替身）+ 脚本化 Opus 推流假音箱（复用
`hub/internal/opus` 编码器）。**全部音频均为 Piper TTS 合成样本
（`~/workspace/.kws-exp/samples`），合成语音，非真人唤醒率。** 完整通过：
`--- PASS: TestE2ERealServerKWS (63.40s)`。

| # | 场景 | 证据 | 结果 |
|---|---|---|---|
| 0 | rms 语义核对（直连边车探针） | 响版（gain=1.0）wake rms=0.137（-17.3dB），轻版（gain=0.5）wake rms=0.069（-23.2dB）；事件 rms 落在同口径复算区间带 [0.000, 0.169] 内（上界纯语音窗口，下界全喂完后窗口全静音）；响版 > 轻版 | 通过：边车 rms 是线性值 0~1（最近 1s 窗口），Hub 侧 `20*log10(rms)` 换算后仲裁只比大小，语义一致 |
| A | 正样本 → 仲裁赢 → 进管线 → 回答 | 合成 pos1“你好小李”→ `wake_win`，耗时 696ms（含 250ms 仲裁窗口；另一次跑 317ms）；赢家会话期内再说一句 → `start_play` RPC + **50 帧 Opus 回答音频**；假 STT/TTS/Agent 各调用 1 次；唤醒路径 Hub 日志 0 丢帧 | 通过 |
| B | 负样本静默 | neg1~neg5 + neg_noise2s 各自独立流（每文件新连接）全部静默，无 `wake_win`/`wake_lose` | 通过 |
| C | 双设备同时唤醒 | lx06 响版 `wake_win`，oh2p 轻版 `wake_lose`；600ms 内无多余裁决，只赢一台 | 通过 |
| D | 边车断线/重连后恢复 | 边车停 → 40s 内观测到 `kws_status` 变 `down`，Hub 进程未退出、`/api/status` 仍 200；边车重启 → `/health` 恢复，合成唤醒再次 `wake_win`；`kws_status` 最多滞后一个 30s 健康检查周期才回 `ok`（唤醒恢复本身即时，状态显示滞后，见下文观察项） | 通过 |

联调中发现并已修的 2 个真实问题（都是 harness/配置问题，不是 Hub 的 bug）：

1. **测试配置 `tls` 默认 true**：`config.yaml` 里不写 `tls` 时 `ApplyDefaults` 会补成
   true，Hub 走 wss + mTLS，假音箱纯 ws 拨号报 `bad handshake`；测试配置必须显式写
   `tls: false`。生产配置 `tls: true` 不受影响。
2. **尽力推流打满 KWS 队列丢关键词**：harness 不 pacing、112 帧瞬间打满 50 帧队列，
   丢最旧帧把关键词开头丢掉，上轮 A 因此 20s 无裁决（Hub 日志“累计丢 1 帧”+ 边车
   无 wake）；改 5ms/帧 pacing + 先推 10 帧静音 prime（让 Hub→边车流完成拨号+hello）
   后稳定通过。真音箱按 20ms/帧实时推流，拨号期间队列不会满，不是 Hub 的 bug；
   但 50 帧队列 + 丢最旧帧的策略在突发推流下会丢关键词，已记在 harness 注释里。

联调中发现的 1 个模型行为（已证实，不是误报，记作观察项）：**跨句拼接**——neg1
“你好”+ 1s 静音 + neg2“小李”串在一路流里会被流式 transducer 拼成一次“你好小李”
（rms 0.132；对照：反向 neg2+neg1 静默，neg1+neg1 静默；6 个负样本串联时也是这
一处触发，rms 0.132 与单测一致）。单样本无误触发；但“停顿 1s 再喊‘小李’”在真人
场景下是否算误唤醒，需真人测试验证（BLOCKED）。

### 部署更新（本轮同步）

- `deploy/nas-install.sh`：预检端口加 10400；第 3 步加 kws.url 的 sed（旧容器名→
  127.0.0.1）；`ps` 检查 8→9 个容器（含 oxn-kws-sidecar，首次启动下载约 5.6MB
  模型会多等一阵）；第 5 步加 `curl http://127.0.0.1:10400/health`（应为
  `{"status":"ok","model_loaded":true}`）与 Hub 日志 `KWS 边车健康检查通过` 检查。
- `deploy/NAS_CHECKLIST.md`：§1.2/1.3（kws 寻址原因）、§2.1（pull 7 个镜像 + 2 个
  本地构建）、§2.2（9 个容器）、§3 加 10400/`/health` 行、§4.1a（`kws_status`
  与健康检查日志）/4.1b（边车日志与模型下载）、§6.1a（真人说“你好小李”只一台应答）、
  故障速查加 3 行（`唤醒服务暂不可用`/`kws-sidecar 反复重启`/10400 被占）、镜像表加
  kws-sidecar 行并更新 hub 二进制 SHA（见下）。
- `firmware-patch/scripts/flash-check.sh`：**SHA 本就已是新值，不用改**——2026-11-17
  联调时重跑两型号均 exit 0，`PASS 补丁镜像 SHA256 与 2026-11-17 实测值一致`，无 WARN
  （LX06 `7266aba9…d97b3`、OH2P `139de363…6415`）；只修正了 `FLASH_RUNBOOK.md`
  里“脚本仍是旧值会打 WARN”的过时注。

### 重编部署二进制（Hub 代码变了）

`go build -o deploy/hub-linux-amd64 ./cmd/hub`：**11,747,288 字节**，ELF 64-bit x86-64，
SHA256 `1e4f6f6cc814597ad2fbc1de763207a06cc4bc0ec1d50552a156f68df54bc80a`（旧值
`f12c09e0…`、11,534,848 字节已取代）。冒烟：`--help` exit 0；弱 Token
（`CHANGE_ME`、短 Token）启动即 Fatal（`OPEN_XIAOAI_TOKEN 无效…至少 16 位`）exit 1。

### 全部重跑输出（2026-11-17，全绿）

| 命令 | 精确输出 | 结果 |
|---|---|---|
| `go test -count=1 ./...`（`hub/`） | 21 个包 ok，**156 passed，0 failed**（基线 156，持平） | 通过 |
| `cargo test`（`client/`） | `test result: ok. 53 passed; 0 failed`（基线 53，持平；含 `server_mode_streams_continuously_without_wake_win`） | 通过 |
| `cargo test --features sherpa-kws`（`client/`） | `test result: ok. 57 passed; 0 failed`（基线 57，持平） | 通过 |
| `python3 -m pytest tests/ -q`（`firmware-patch/`） | `61 passed in 101.44s`（基线 61，持平） | 通过 |
| `.venv/bin/python -m pytest tests/ -q`（`kws-sidecar/`） | `16 passed in 14.67s`（基线 16，持平） | 通过 |
| `scripts/lint.sh` | `lint: 全部通过`（exit 0；clippy 仅警告，不拦） | 通过 |
| `./build-armv7-gnueabihf.sh`（`client/`） | exit 0，全部 ASSERT OK（GLIBC 最高 2.25、硬浮点、RUNPATH `$ORIGIN/lib`、NEEDED 5 个库全在音箱 rootfs 内、`.so` 10 个导出符号）；产物 **2,444,332 字节**，ELF 32-bit ARM PIE stripped，SHA256 `e90c3058cb29cc9faa6fe304cea2c1ad265403c68cf6b3743a5e8f5efc0161ff`，连续两次构建 SHA 一致；与 P1-40 时记录的 2,435,964 字节（`2da754d4…`）不同，是因为之后客户端加了 server-mode 代码（cargo 46→53），不是本次改动造成；官方 `.so` 仍要求 GLIBC_2.29 / GLIBCXX_3.4.29 的 WARNING 照旧（服务端 KWS 后不再是关口） | 通过（断言部分）；真机唤醒 BLOCKED |

### 测试自给自足 + CI 接入（2026-11-17，本轮之后补）

问题：边车 16 个测试此前只能本地跑——`tests/conftest.py` 写死
`~/workspace/.kws-exp` 的模型与样本路径，系统 Python 下连
`websockets` 都没有；CI 的 python job 也只跑 firmware-patch 的
测试。修复（`tests/bootstrap.py`，conftest 在导入时调
`resolve_assets()`；功能代码与测试断言未动）：

- 资产根目录默认仍是 `.kws-exp`（已有全部资产时只做存在性检查，
  一个字节也不重新下载）；`KWS_TEST_ASSETS` 指向另一根目录时
  （CI 用 `/tmp/kws-test-assets`），在该目录内按同样结构补齐。
- 模型缺失 → 调 `scripts/download-model.sh` 下载（SHA256 已 pin，
  同时把 `keywords.txt` 写进模型目录）；关键词表缺失 → 写
  2026-11-17 实验验证过的 token 行；样本缺失 → 下载 Piper 语音
  （HF rhasspy/piper-voices v1.0.0，zh_CN-huayan-medium）后现生成，
  再 ffmpeg 重采样到 16kHz；噪声样本用 numpy 固定种子生成。
- 任何一步失败都抛 `RuntimeError`（写清缺什么、从哪下），不静默
  跳过；此前 fixture 里的两处 `pytest.skip` 已删掉。
- CI：`.github/workflows/ci.yml` 的 python job 增加一步（装
  `requirements.txt` + `requirements-dev.txt` + `piper-tts`、
  `apt install ffmpeg`，`KWS_TEST_ASSETS=/tmp/kws-test-assets` 跑
  `python -m pytest kws-sidecar/tests/ -q`）。

实测发现（已修）：Piper 合成是非确定性的，同一文本两次生成是
不同的音频；pos3（“你好小李，今天天气怎么样”）约 1/7 的抽样检
不出（关键词本身的渲染没问题，前缀 1.3s 单独喂能检出，是逗号后
prosody/停顿的差异让 transducer 的关键词路径丢了；另 6 次同文本
抽样 6/6 检出）。bootstrap 因此在生成后用离线 spotter 逐个校验
（与测试同样的流式喂法：20ms 块 + 1s trailing 静音；正样本必须
检出、负样本必须静默），不合格的重抽，最多 10 次，10 次仍不合
格就明确报错——干净状态验证时确实命中一次坏抽样（pos3 生成后
40310 帧，校验不过，重抽为 36224 帧后通过）。

验证（两遍，均为 16 passed）：① 干净状态——全新 venv（只装
`requirements.txt` + `requirements-dev.txt` + `piper-tts`）、空
`KWS_TEST_ASSETS`：bootstrap 下载模型 + 语音、生成 11 个样本、
重抽 1 次，`16 passed`；② 本地默认路径（`.kws-exp` + 现有
`.venv`）：`16 passed`，无下载。

### BLOCKED 清单（本轮之后仍阻塞的）

- **真人唤醒率（1m/3m ≥95%）**：全部证据都是 Piper TTS 合成语音；真人唤醒率未测，需真机。
- **真机性能**：边车在 NAS 上的推理延迟/RTF、双设备并发、长期运行内存，均未在 NAS 上测过（VM 联调只证明链路通，不证明 NAS 上的性能）；NAS 部署本身仍是关口 2 BLOCKED。
- **真机两台仲裁实测**：VM 里是假音箱 + 增益差（gain 1.0 vs 0.5）；真机上两台同时喊、谁赢谁输，需刷机后实测。
- **跨句拼接是否算误唤醒**：见上文观察项——“你好”停 1s 再喊“小李”会被拼成唤醒；真人场景下的误唤醒率需真机验证。
- **kws_status 显示滞后**：边车恢复后 `/api/status` 的 `kws_status` 最多滞后一个 30s 健康检查周期才回 `ok`（唤醒本身恢复即时）；只是状态显示问题，未改行为（改健康检查间隔是可选优化，未做）。
- 其余关口（刷机、NAS 部署、HA 联调、网易云扫码等）见“硬件关口检查清单”，状态不变。

## CI 修复（2026-11-17，第二轮审计，分支 `audit-round2-ci-fix`，PR #1）

CI 首次实跑（run 44063914126）：Rust ✓、Semgrep ✓；Go ✗、Python ✗。

- **Go 失败原因**：`go vet` 编译 `gopkg.in/hraban/opus.v2` 时报 `Package opusfile was not found in the pkg-config search path`；`opusfile.pc` 由 `libopusfile-dev` 提供，ci.yml 只装了 `libopus-dev`。**修复**：apt 安装行加上 `libopusfile-dev`。
- **Python 失败原因**：firmware-patch 的测试依赖仓库外的固件 rootfs（本地在 `speaker-open-xiaoai-rebuild/firmware/rootfs/`，9.7GB，不进仓库），CI 环境里没有，setup 直接 FileNotFoundError；且固件步骤先失败、job 中止，kws-sidecar 测试一步从未执行。**修复**：依赖 fixture 的测试在缺失时 `pytest.skip` 带明确理由（不删测试、不静默跳过、不把 rootfs 塞进仓库）；原 `python` job 拆为 `python-firmware` + `python-sidecar` 两个 job，边车测试独立跑、独立出结果。
- **本地预验证**：CI 条件模拟（干净 checkout，无固件材料）24 passed / 37 skipped，0 failed；本地有 fixture 时 61 passed；kws-sidecar 16 passed；YAML 校验通过。
- **PR 首跑结果（run 44084093818）**：Go ✓、Rust ✓、Python（固件）✓、Semgrep ✓；**Python（kws-sidecar）✗**——边车 job 冷启动失败：bootstrap 调 `download-model.sh` 下载模型（SHA 校验通过），随后 `sherpa-onnx-cli text2token` 生成 keywords.txt 时报 `ModuleNotFoundError: No module named 'click'`。根因：`sherpa-onnx==2.0.1` 只声明依赖 `sherpa-onnx-core`，不声明 `click`，但其 `sherpa-onnx-cli` 在 import 时就要 click；本地此前只跑过暖缓存（`~/workspace/.kws-exp` 已有模型 + keywords.txt，`download-model.sh` 从未执行），冷启动路径从未被实际跑过。**修复**：`kws-sidecar/requirements.txt` 加 `click==8.5.1`（带注释）。同一行同时修掉 NAS 首次部署的同款隐患：容器 entrypoint 在模型缺失时也调 `download-model.sh`，缺 click 同样会 FATAL 退出。
- **click 修复的冷验证（第一次，走了 fallback 分支）**：全新 venv（只装 requirements + requirements-dev + piper-tts）+ 空 `KWS_TEST_ASSETS`：模型下载、11 个样本生成，`16 passed in 15.24s`。但事后复核发现这次验证并不忠实：pytest 是用 venv 的 python 绝对路径跑的，venv 的 bin 不在 PATH 里，`command -v sherpa-onnx-cli` 找不到，`download-model.sh` 走了“写已验证 token 行”的 fallback 分支，text2token 根本没跑——所以它没能发现缺 `sentencepiece`。
- **PR 第二跑结果（run 44085140541）**：Go ✓、Rust ✓、Python（固件）✓、Semgrep ✓；**Python（kws-sidecar）仍 ✗（35s）**——click 已修好（CLI 跑起来了），但 text2token 接下来又缺 `sentencepiece`（`sherpa_onnx/utils.py:41`）。根因同款：`sherpa-onnx-cli text2token`（`--tokens-type ppinyin`）还要 `sentencepiece` 与 `pypinyin`，三者都不在 sherpa-onnx 的声明依赖里；`download-model.sh` 的注释（L31-32）与报错提示（L89）其实写明了要装这三样，只是 requirements 里一个都没写。**修复**：requirements.txt 再加 `sentencepiece==0.2.3`、`pypinyin==0.55.0`。
- **真正的冷验证（第二次，text2token 实跑）**：同一全新 venv 补装 sentencepiece + pypinyin，`PATH` 加上 venv 的 bin（与 CI 一致，`sherpa-onnx-cli` 在 PATH 上，强制走 text2token 分支）+ 空 `KWS_TEST_ASSETS`：模型下载、text2token 生成 keywords.txt、11 个样本生成，`16 passed in 14.89s`。`download-model.sh` 有 `set -euo pipefail`，text2token 失败即脚本 exit 1、bootstrap 抛错，所以 16 passed 即证明 text2token 真跑通了。
- **最终 CI 结果**：（重跑后填写；以 GitHub Actions 实跑为准。）
