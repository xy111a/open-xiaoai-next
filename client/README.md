# Client（音箱端薄客户端，Rust）

跑在 LX06 / OH2P 上，ARMv7 32-bit，glibc 2.25 动态链接。只做五件事：录音、放音、本地 KWS、
上报唤醒事件、执行 Hub 的 RPC。**不做业务逻辑**：不跑 ASR、不跑 LLM、不管房间、不管音乐同步。

> **构建目标（P1-40 方案 B，2026-11-17 决策）**：默认交付是
> `armv7-unknown-linux-gnueabihf`（glibc 2.25，zig 作链接器），见
> `build-armv7-gnueabihf.sh`。旧的 `build-armv7.sh`（musl 静态链接）仅作
> **应急 fallback** 保留——静态 musl 程序不能 `dlopen`，刷进去的 Client
> 永远唤醒不了（见 `kws/vendor-arm/SOURCE.md`）。

## Phase 1 实现状态（2026-11-17）

| 模块 | 状态 |
|---|---|
| `src/audio.rs` | ✅ Opus 编解码真实实现（`opus-rs` 纯 Rust，16kHz/单声道/20ms/VOIP/16kbps）；ALSA 采集/播放已接线（任务 E，2026-11-17：`AudioDevice` trait + 真实 `AlsaAudioDevice` + Fake/NoDevice；无硬件时返回明确错误；真机实测仍 BLOCKED） |
| `src/transport.rs` | ✅ wss + Token 鉴权 + 指纹校验 + 指数退避重连（100ms 起步，上限 1s）；支持 `tls: false` 纯 ws 测试模式 |
| `src/kws.rs` | ✅ RMS 计算与帧处理结构真实；sherpa-onnx 后端已接（任务 F，2026-11-17：feature `sherpa-kws`，见 `kws/README.md`；默认不开，无模型时恒不谎报唤醒） |
| `src/config.rs` | ✅ `tls`、`hub_ca_fingerprint`、`room` 字段；缺字段走默认值，旧 client.toml 兼容；`kws_mode`（`"server"` 默认 / `"local"`，非法值启动报错，见下） |
| `src/main.rs` | ✅ Session 状态机：KWS 命中 → 发 wake → 等 wake_win/wake_lose → 赢了才推 audio_start + 音频帧；收到 interrupt 立即停播；rpc 白名单执行（`run_shell` 不在白名单，拒绝）；server 模式：会话一建立即发 audio_start、持续推流，wake_lose/interrupt 不停流 |

## 构建

```bash
source ~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/env.sh
# /tmp 是 512MB tmpfs 且常满（他处 pytest 残留），cc 临时文件改走磁盘目录：
export TMPDIR=~/workspace/.cargo-tmp

cargo build --release            # 宿主机 x86_64
./build-armv7-gnueabihf.sh      # ARMv7 gnueabihf 交叉（默认交付，见下）
./build-armv7.sh                 # ARMv7 musl 交叉（仅应急 fallback，不再是默认）
```

## ALSA 接线（任务 E，2026-11-17）

- **结构**：`AudioDevice` trait（`capture()` / `play()` / `describe()`）隔离硬件。
  真实设备 `AlsaAudioDevice`（`alsa` 0.12 crate，feature `alsa`，默认开）：
  采集 `hw:0,0`、播放 `hw:0,2`，S16LE / 16kHz / 单声道 / 20ms 周期（320 点），
  缓冲 4 周期；XRUN（EPIPE）时 `prepare()` 后各重试一次。
  测试用 `FakeAudioDevice`（同一 trait，队列吐帧、记录播放帧）；
  无硬件或未编译 `alsa` feature 时用 `NoAudioDevice`，采集/播放一律返回
  带设备名的明确错误，绝不返回静默/假数据。
- **无硬件行为**：`Audio::open()` 仍成功（构造器不因缺硬件失败）；录音任务
  记日志退出，主循环继续跑传输、不崩溃（冒烟已验证）；`play_frame` 无硬件时
  返回明确错误，记日志丢帧。两路独立打开：采集打不开不影响播放，反之亦然。
- **gnueabihf libasound（默认交付，P1-40 方案 B）**：ARMv7 动态链接直接用音箱自带的
  `libasound.so.2`。链接期用 rootfs 里抽出的 `libasound.so.2.0.0`
  （`firmware/rootfs/usr/lib/`，只读参考，不改 rootfs）；rootfs 里没有 ALSA
  头文件，头文件用 `~/workspace/tools/libasound-arm/install/include/`
  （alsa-lib 1.2.14 构建树，架构无关）。`build-armv7-gnueabihf.sh` 每次运行
  自行重建 staging 目录 `.link-gnueabihf/`（lib + `alsa.pc`，已进
  `.gitignore`，不进仓库），并经 `PKG_CONFIG_PATH_armv7_unknown_linux_gnueabihf`
  让 alsa-sys 只认这一个 `.pc`（不认宿主机 glibc 版）；链接时 `-lasound`，
  不从源码编 ALSA、不走 musl 静态库。产物 `NEEDED libasound.so.2`，
  在音箱上由系统自带的 `/usr/lib/libasound.so.2` 提供。
- **musl libasound（仅应急 fallback）**：ARMv7 静态链接不能用宿主机 glibc 的 libasound。
  本机已自编 musl 版：`~/workspace/tools/libasound-arm/`
  （alsa-lib 1.2.14，zig cc `-target arm-linux-musleabihf`，
  `--disable-shared --enable-static --disable-python`，产物
  `install/lib/libasound.a` 1,328,688 字节 + `install/lib/pkgconfig/alsa.pc`）。
  `build-armv7.sh` 已接好（`PKG_CONFIG_ALLOW_CROSS=1` +
  `PKG_CONFIG_PATH_armv7_unknown_linux_musleabihf` + `ALSA_STATIC=1`，
  静态链进二进制）；若该目录缺失，脚本退化为 `--no-default-features`
  编一个无 ALSA 的应急版并打印 WARNING，不让构建直接炸掉。
  ARM 产物含真实 libasound 符号（`strings` 可见 `snd_pcm_open/readi/writei`、
  `hw:0,0`、`hw:0,2`），2,851,468 字节，仍 <5MB。
- **仍 BLOCKED**：真机实测。`cargo test` 53 通过、`cargo test --features sherpa-kws`
  57 通过（2026-11-17 服务端唤醒模式后，见“测试”一节）；`./build-armv7-gnueabihf.sh`
  通过（ELF 32-bit ARM EABI5 硬浮点动态链接，见下）。

## 服务端唤醒模式（kws_mode，2026-11-17）

决策：唤醒检测移出音箱、放 NAS 端边车服务（kws-sidecar）。音箱只负责
持续推 Opus 音频流，唤醒词检出、仲裁、对话都在 Hub/边车侧做。
`client.toml` 里 `kws_mode` 控制：

| | `server`（默认） | `local` |
|---|---|---|
| 唤醒检测 | Hub/边车（kws-sidecar） | 音箱本地 sherpa-onnx |
| 本地模型 / `.so` | **不需要** | 需要（见部署清单） |
| 无后端时 | 正常启动、持续推流 | feature 开：**非零退出**（P1-40 fail-loud）；feature 不开：纯 RMS、不谎报唤醒 |
| 推流 | 会话一建立即发 `audio_start`，**持续推**，不等 `wake_win`；`wake_lose` / `interrupt` 不停流（`interrupt` 只停播） | `wake_win` 后才推 `audio_start` + 音频帧；`wake_lose` 停流 |
| 启动日志 | `kws_mode=server` | `kws_mode=local` |
| 适用 | 默认部署（.so 未落定也能跑） | 未来 A 路（ARM .so 从源码重编落定后）/ 离线场景 |

实现（只改 client/，hub/ 与 kws-sidecar/ 未动）：`config.rs` 增加
`kws_mode`（默认 `"server"`，非法值启动即报错，`validate()`）；
`main.rs` 增加 `init_kws()`（server 返回 `Ok(None)`，不初始化本地
KWS；local 保持原有行为）与 `Session::new_server()`（建立即推流）；
录音任务 server 模式跳过 `Kws::feed`、只编码推流；`run_session` 开头
server 模式先发 `audio_start`，Ctrl-C 时尽力补发 `audio_end`。
本地 KWS 代码路径（`src/sherpa.rs`、`src/kws.rs`、feature 门控）保留
不动，供未来 A 路复用；回归测试
`kws_new_refuses_rms_only_when_sherpa_feature_on` 仍通过（local 模式）。

### 带宽估算（持续 Opus 流，16kHz 单声道，20ms/帧，50 帧/秒）

编码器目标码率 16kbps（`audio.rs` 的 `TARGET_BITRATE_BPS = 16000`，
即 2.0 KB/s 净荷）。VM 上用真实 `OpusCodec` 对**合成音频**实测
（非真人语音，仅用于标定码率，见下表）：

| 合成音频 | 平均包长 | 净荷速率/台 |
|---|---|---|
| 连续 1000Hz 正弦波（满码率参考） | 40.7 B/帧 | **约 2.0 KB/s** |
| 间歇 440Hz（50% 占空比，近似有人说话时断时续） | 24.5 B/帧 | 约 1.2 KB/s |
| 静音 | 8.0 B/帧 | 约 0.4 KB/s |

- **每台约 2.0 KB/s**（Opus 净荷，满码率上限；叠加 1 字节 `0x01` 帧头
  50 B/s 后约 2.1 KB/s）。安静时 VBR 自动降到约 0.4 KB/s。
- **2 台合计约 4.1 KB/s**（满码率上限）。
- **每日总量（满码率上限）**：每台约 175 MB/天，2 台合计约 **350 MB/天**；
  按间歇语音约 1.2 KB/s 算，2 台合计约 210 MB/天；全天静音约 70 MB/天
  （2 台合计）。以上是应用层净荷。
- 局域网线上传输上限：每 20ms 帧是一个独立 WebSocket 二进制消息，
  帧头 2 B + TCP/IP 约 40 B/段（若每帧单独成段、不合并）最多再加约
  2.1 KB/s/台，即线上传输上限约 4.2 KB/s/台、2 台约 8.4 KB/s、
  约 725 MB/天；实际值取决于 TCP 合并情况，只会比这个上限低。
  全部流量都在家庭局域网内（音箱 ↔ NAS），不走外网、不产生公网流量。
- 注意：server 模式是 24 小时持续推流（即使没人说话也有静音帧），
  上面按天算的量就是常驻成本；local 模式平时不推流，只有唤醒后才推，
  带宽可忽略，但要本地模型/.so 才能工作。

### 部署清单变化（相对 local 模式）

- `client.toml` 增加一行 `kws_mode = "server"`（不写也默认 server）。
- server 模式部署**不再必需** `lib/libsherpa-onnx-c-api.so`、
  `lib/libonnxruntime.so` 与 `kws/<模型文件>`（见下表）；但 ARM 构建
  默认仍开 `--features sherpa-kws`（构建脚本不断言变化），切回 local
  模式时无需重编二进制，改配置即可。
- 其余部署项（Token、指纹/mTLS 证书、二进制）不变；Hub 侧须有边车在跑，
  否则持续推流无人检出唤醒（客户端不报错、不退出，只推流）。

## 测试

```bash
source ~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/env.sh
export TMPDIR=~/workspace/.cargo-tmp
cargo test
```

37 个测试全过（2026-11-17 任务 E 后：32 → 37，新增 5 个 ALSA 测试），全部执行真实被测代码，无 mock 造假；另有 feature `sherpa-kws` 开时 41 个（+4 个 sherpa 测试，见 `kws/README.md`）。
审计修复后基线（2026-11-17，TEST_REPORT.md“审计修复”一节）：`cargo test` **46 通过**、
`cargo test --features sherpa-kws` **50 通过**（含 fail-loud 回归
`kws_new_refuses_rms_only_when_sherpa_feature_on`：feature 开时 `Kws::new`
无后端即返回 Err，进程非零退出，不静默跑 RMS）。
服务端唤醒模式后（2026-11-17，见上节）：`cargo test` **53 通过**、
`cargo test --features sherpa-kws` **57 通过**（新增 7 个：`kws_mode`
解析/校验 2 个、server/local 会话分流 2 个、`init_kws` 分流 2 个、
fake Hub 端到端持续推流 1 个；local 模式 fail-loud 回归仍在）。

- Opus 编解码往返（1000Hz 正弦波 50 帧，解码后 RMS 误差 <10%，帧长 320；平均包长落在 10~100 字节/帧）
- Token 鉴权握手（本地真实 tokio-tungstenite 服务：正确 token 通过、错误 token 被 `hello_reject` 拒绝）
- wss 鉴权 + 指纹校验（rcgen 自签证书的真实 tokio-rustls 服务：正确指纹+正确 token 通过；错 token 被拒；错指纹握手失败；非法指纹格式配置期报错）
- 断线重连（服务端主动关连接后同端口重启，`connect_with_retry` 在 30s 内重连成功；另有服务晚起 1s 的重试测试）
- RMS 计算（恒定 16384 → -6.02dB；正弦波 → -9.03dB；静音 → -120dB 下限，不返回 -inf）
- config 解析（默认 + 自定义 toml + 部分字段缺省 + 非法指纹识别 + 缺失文件回退默认）
- Session 状态机（wake_win → audio_start；wake_lose → 静默；interrupt → 立即停播；rpc play_text/set_volume；未知 rpc 方法拒绝）
- ALSA（任务 E）：Fake 设备 capture→Opus 编码→play 解码写出往返（RMS 误差 <10%）；NoDevice 的 capture/play 返回带 ALSA 字样的明确错误；真实 ALSA 代码打开不存在的设备（hw:99,99/hw:99,98）返回带设备名的明确错误；本 VM 无声卡时 `Audio::open()` 仍成功、capture 报 `hw:0,0` 明确错误
- KWS（任务 F，`cargo test --features sherpa-kws`）：C API 结构体尺寸与头文件探针对照（64-bit）；缺失 .so / 缺失模型目录报明确错误；真实模型 + 真实 .so + 真实 Piper TTS 样本端到端：`pos1.wav` 检出“你好小李”，`neg3.wav` 静默（需 `SHERPA_KWS_TEST_*` 环境变量指夹具，未设时打印 SKIP）

## ARMv7 交叉编译（默认：gnueabihf，P1-40 方案 B）

```bash
rustup target add armv7-unknown-linux-gnueabihf
./build-armv7-gnueabihf.sh
# 产物：target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client
# 已验证（2026-11-17 方案 B 切换）：ELF 32-bit ARM EABI5 硬浮点动态链接，
# 解释器 /lib/ld-linux-armhf.so.3，stripped，2,435,964 字节（<5MB 目标），
# SHA256 2da754d48160906b65bb289925dd3dacdb83b16721f7690540e48f80dd37a00b
# 服务端唤醒模式后重编（2026-11-17）：2,444,332 字节，
# SHA256 e33dd0e575f15c1d61ae33a2d2679ef88284ff136535b268d1ca7f48e20ebd19
# （构建脚本全部断言通过，见上节部署清单）
```

工具链是 **zig 0.15.2**（`~/workspace/tools/zig-x86_64-linux-0.15.2/zig`），
做 ARM C 编译器 + 链接器；包装器 `~/workspace/tools/zig-cc-arm-gnueabihf.sh`
负责过滤 cc-rs 传的 `--target=*`（zig 0.15 拒绝该三元组），并强制
`-target arm-linux-gnueabihf.2.25`（glibc 上限锁死音箱的 2.25，超了构建断言
直接失败）。另需 `RUSTFLAGS="-C link-self-contained=no"`，否则 rustc 自带
CRT 与 zig 的 glibc crt1.o 重复定义 `_start`，链接失败。
`./build-armv7-gnueabihf.sh` 已把上述环境变量全部包好，直接跑即可。

构建末尾断言（失败即非零退出，不许将就）：

- 产物是 ARM EABI5 硬浮点动态链接，解释器 `/lib/ld-linux-armhf.so.3`
  （ELF 头 Flags `0x5000400, Version5 EABI, hard-float ABI`；注意
  `readelf -A` 不出 `Tag_ABI_VFP_args` 是 LLVM 的正常表现，不能拿它当
  硬浮点判据——2026-11-17 切换时曾因此误判，已改判 ELF 头 Flags）；
- 含 sherpa-kws 3 个标记（`open-xiaoai-kws-backend=sherpa-onnx-2.0.1` 等，
  与 musl 脚本同款）；
- Version needs 里 GLIBC 最高 ≤2.25（实测最高恰为 2.25：唯一的 2.25 符号是
  WEAK 的 `getrandom`，glibc 2.25 自带，能解析）、GLIBCXX 无版本符号；
- `RUNPATH $ORIGIN/lib`（部署包 `lib/` 下的 .so 靠它找到）；
- `NEEDED` 逐一对照音箱 rootfs：`libasound.so.2`、`libm.so.6`、
  `libpthread.so.0`、`libc.so.6`、`libdl.so.2` 全部在 rootfs 的 `/lib` 或
  `/usr/lib` 里找得到，缺失的进部署包或上报；
- vendor ARM `.so` 若已取到，核对 10 个必需导出符号（与 musl 脚本同款）。

### musl 应急版（fallback，不再是默认）

```bash
rustup target add armv7-unknown-linux-musleabihf
./build-armv7.sh
# 产物：target/armv7-unknown-linux-musleabihf/release/open-xiaoai-client
# 静态链接 musl。只在 gnueabihf 路线不可用时用；静态 musl 程序不能 dlopen，
# 刷进去的 Client 永远唤醒不了，KWS 在真机上仍是 BLOCKED。
```

musl 包装器是 `~/workspace/tools/zig-cc-arm.sh`
（`-target arm-linux-musleabihf`）。

踩过的坑（已解决，不用再试）：

- musl.cc 的 `arm-linux-musleabihf-cross` 工具链是 **32 位 x86 静态二进制**，
  在本 VM 上一运行就段错误（exit 139，疑似无 32 位仿真），已删除；
  不要再用它。
- rustls 0.23 / tokio-rustls 默认特性会拉入 `aws-lc-rs` → `aws-lc-sys`
  （数百 MB 的 C 编译，还堵死交叉编译）。本项目所有 rustls 相关依赖
  均已 `default-features = false` 只开 `ring`；`cargo tree -i aws-lc-rs`
  应无任何包。

## 部署到音箱

```bash
scp target/armv7-unknown-linux-gnueabihf/release/open-xiaoai-client root@<音箱IP>:/data/open-xiaoai/client
ssh root@<音箱IP> 'echo "wss://<NAS_IP>:4399" > /data/open-xiaoai/server.txt'
# 随后由 Hub OTA 统一升级，不用再手动 scp
```

### 部署清单（/data/open-xiaoai/，以本节为准；deploy/README.md 不重复写）

| 路径 | 内容 | 状态 |
|---|---|---|
| `client` | 客户端二进制（gnueabihf 默认产物，见上） | ✅ 已构建（2,444,332 字节，SHA256 e33dd0e575f15c1d61ae33a2d2679ef88284ff136535b268d1ca7f48e20ebd19，服务端唤醒模式后重编） |
| `lib/libsherpa-onnx-c-api.so` | sherpa-onnx C API（KWS 后端，`$ORIGIN/lib` rpath 指向这里） | ⏳ 待定：**server 模式不需要**（默认部署可不装）；local 模式才需要。待 P1-40 的 .so 落定后补（官方预编译版要求 GLIBC_2.29，音箱只有 2.25，**不能直接用**；从源码重编是另一路的工作） |
| `lib/libonnxruntime.so` | ONNX Runtime（sherpa 的依赖，只要求 GLIBC_2.4） | ⏳ 待定：**server 模式不需要**；local 模式才需要，待 P1-40 的 .so 落定后补 |
| `kws/<模型文件>` | kws-zipformer 模型（见 `kws/README.md` 下载脚本，模型文件不进仓库） | ⏳ **server 模式不需要**；local 模式部署时按脚本下载后拷贝 |
| `client.toml` | 配置（见下示例） | 部署时写 |

注意：客户端二进制**没有**烤进已交付的补丁镜像（镜像里只有
`/etc/open-xiaoai/tls/` 证书），是刷机后另行拷贝到 `/data` 的，所以
**切换构建目标不需要重编镜像**，不要碰镜像。`lib/` 下的 .so 目前一个都不
在部署包里——在 .so 落定之前，`kws_mode = "local"` 的 Client 启动后会因
KWS 后端缺失**非零退出**（P1-40 要求的 fail-loud 行为，不静默跑 RMS）；
默认的 `kws_mode = "server"` 不受影响，没有 .so 也能正常启动、持续推流
（唤醒检测在 Hub/边车侧做）。

client.toml 示例（`/data/open-xiaoai/client.toml`；宿主机调试可用
`OPEN_XIAOAI_CLIENT_TOML` 环境变量改路径）：

```toml
hub_url = "wss://192.168.1.10:4399"
token = "CHANGE_ME"               # 必须与 Hub 的 OPEN_XIAOAI_TOKEN 一致
wake_word = "你好小李"
kws_mode = "server"               # server（默认）= 唤醒在 NAS 边车做，持续推流，
                                  # 不要求本地模型/.so；local = 音箱本地 sherpa KWS；
                                  # 其他值启动即报错
kws_model = "/data/open-xiaoai/kws/nihaoxiaoli.onnx"  # local 模式用，server 模式可不配
model = "lx06"                    # lx06 / oh2p
ram_mb = 256
tls = true                        # false = 纯 ws，仅本地测试
hub_ca_fingerprint = "aa:bb:cc:..."  # Hub 自签证书 SHA-256 指纹（可带冒号）；
                                  # 不配则跳过校验并打警告（仅测试环境可接受）
room = "客厅"                     # 可选，已认领的房间名
```

## 模块

| 文件 | 职责 |
|---|---|
| `src/main.rs` | 启动、配置加载、录音任务 + 会话循环、Session 状态机 |
| `src/config.rs` | `/data/open-xiaoai/client.toml`，换唤醒词改这里 |
| `src/audio.rs` | Opus 编解码（`opus-rs`）+ 0x01 音频帧封装；ALSA 已接线（`AudioDevice` trait + `AlsaAudioDevice`，见上） |
| `src/kws.rs` | RMS 计算 + 20ms 帧处理 + WakeEvent 结构；sherpa-onnx 后端（feature `sherpa-kws`，`src/sherpa.rs` 动态加载 C API，见 `kws/README.md`） |
| `src/transport.rs` | wss/ws + Token 鉴权 + 指纹校验，断线指数退避重连 |
| `build-armv7-gnueabihf.sh` | ARMv7 gnueabihf 交叉编译一键脚本（默认交付，zig 工具链，末尾断言 GLIBC ≤2.25 / NEEDED 对照 rootfs / rpath `$ORIGIN/lib`） |
| `build-armv7.sh` | ARMv7 musl 交叉编译一键脚本（仅应急 fallback，静态链接，不能 dlopen） |

## 参考实现（当 Spec，不直接抄）

- OwnDing/open-xiaoai `client-2026.10.09`：连续对话 + 打断 + 子带 AEC 已在同款硬件跑通
- coderzc/open-xiaoai-bridge：服务端 Token 鉴权（`OPEN_XIAOAI_TOKEN`）
- 原项目 idootop/open-xiaoai：`/data/open-xiaoai/client` 路径与 WebSocket 4399 端口约定

## BLOCKED 清单（需 bai 的硬件 / 文件，非代码问题）

1. **KWS 模型文件（模型已验证，ARM 部署仍 BLOCKED）**：sherpa-onnx WenetSpeech
   kws-zipformer 模型已在 VM 上用 Piper 合成语音冒烟通过（5/5 正样本检出、
   5/5 负样本静默，int8 与 fp32 相同；详见 `kws/README.md`），Rust 侧已用
   feature `sherpa-kws` 接入（`src/sherpa.rs`，默认不开）。缺的是 ARM 版
   `libsherpa-onnx-c-api.so`（需从源码交叉编译，见 `kws/README.md` §ARM 部署）
   与真机 + 真人唤醒率验证（FR-01）。模型下载见
   `scripts/download-kws-model.sh`（官方 GitHub releases + SHA256 校验，
   模型文件不进仓库）。
2. **ALSA 采集/播放（代码已接线，真机实测仍 BLOCKED）**：`AudioDevice` trait +
   真实 `AlsaAudioDevice`（`alsa` crate，gnueabihf 产物动态链接音箱自带的
   `/usr/lib/libasound.so.2`，`NEEDED libasound.so.2` 已在 rootfs 核对到；
   musl 应急版是静态链入 libasound.a，`strings` 可验 `snd_pcm_open/readi/writei`）已实现；宿主机 `cargo test`
   53 通过、冒烟（无硬件时明确报错、主循环继续跑、不崩溃）已验证。
   但本机无 /dev/snd、无音箱硬件，`hw:0,0` 采集 / `hw:0,2` 播放的真实
   读写、XRUN 恢复、音量/延迟都未经真机验证，须等刷机后的 LX06/OH2P。
3. **ARM 产物未在真机上跑过**：交叉二进制已产出（gnueabihf 默认：ELF 32-bit ARM
   EABI5 硬浮点动态链接 2,444,332 字节（服务端唤醒模式后重编）；musl 应急版：静态链接 2,885,364 字节），
   但本机无 qemu-arm、无音箱硬件，无法执行验证；上机第一步仍是 Phase 0 定好的：
   刷一台 LX06 + 起 Hub + 同步一首歌。

## 与任务书的偏差（已核实，非擅自改动）

- 任务书要求用 `audopus` crate：**该 crate 在 crates.io 上不存在**
  （索引 `au/do/audopus` 查无此键，`cargo add` 报 `no matching package named audopus`；
  另有 `audiopus`，但它链接系统 libopus，不符合“纯 Rust”要求）。
  改用功能等价的纯 Rust crate **`opus-rs` 0.1.42**（libopus 1.6 的 Rust 移植，
  无 C 依赖，`OpusEncoder::new(16000, 1, Application::Voip)`；码率默认 64kbps，
  已显式改为 16kbps；解码只出 f32，本项目转回 i16）。
