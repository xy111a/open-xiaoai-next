# P2 修复轮（第三轮审计）修复报告（FIXES_P2_ROUND3.md）

- 修复轮：第三轮审计的 44 个 P2（`AUDIT_ROUND3.md` §5）
- 分支：`fix-p2-round3`（从 main `5351ac0` 切出，PR #5/#6 已合）
- 协调员：Friday（4 个工人并行：Hub Go / 客户端 Rust / 固件部署 / 边车 Python）
- 日期：2026-11-18

## 0. 结论先行

- **44 项中 37 项已修，4 项已核实关闭，1 项接受现状，1 项已知延期，1 项待 bai 定夺；无未修项。**
- 优先的 5 项（H-2、H-3、H-6、S-7、C-10）全部已修。
- 镜像不需要重编：本轮没有改动影响镜像内容的补丁逻辑（F-5/F-6 只改构建脚本门控行为；F-4 只做文档；其余是文档/接受项）。当前已交付的两份补丁镜像（LX06 `1a5c2760…d0281`、OH2P `8db02670…9d197`）内容不受影响；新门控在下次重编时才生效。
- 全套测试协调员已亲自重跑：Go 292、Rust 110/115、固件 102、边车 91（+1 跳过）、lint 全过、flash-check 四路全过（见 §2）。

## 1. 逐项状态表

### 1.1 Hub 面（H-2 ~ H-13，12 项，全部已修）

| 项 | 状态 | 改动 |
|---|---|---|
| H-2 MPD query() 无界读 | 已修 | `hub/internal/music/music.go`：`query()` 改走 `readLimitedLine`（单行 64KB，与 Snapcast 同款）；加总行数上限 10,000 行、总字节上限 4MB；超限/读错即断开连接（看门狗重拨）。`readLimitedLine` 错误前缀由 `snapcast:` 改为通用文案。测试 `TestMPDQueryRejectsOversizeLine` / `TestMPDQueryRejectsTooManyLines` / `TestMPDQuerySmallResponseOK` |
| H-3 SongURL 未验 scheme | 已修 | `music.go`：新增 `validatePlayableURL`（SongURL 与 PlayURL 共用）：只许 http(s)，拒绝 file:// 等 scheme、localhost/`.localhost`、任何裸 IP（上游是域名 CDN）、单标签主机名、userinfo；不做 DNS 解析（DNS 重绑定不在本项范围，注释写明）。非法 URL 在 SongURL 按“无可播地址”报错，router 即走本地兜底。测试 `TestValidatePlayableURL`（4 合法/18 非法）/ `TestPlayURLRejectsDisallowedURL` / `TestSongURLRejectsDisallowedURL`。取舍见 §4 |
| H-4 Status 混淆上游故障与登录过期 | 已修（代码）；NAS 实测 BLOCKED | `hub/internal/music/netease_auth.go`：`getJSON` 非 200 时返回带状态码的 `httpStatusError`；`Status()` 遇 401/403 映射为未登录（无 err，Web 层回 `logged_in:false`/`needs_rescan:true`），仅网络错误/5xx 才返回 err（Web 502）。NAS 上用过期 cookie 的实测未做，记 BLOCKED（无 NAS）。测试 `TestNeteaseStatusMaps401403ToLoggedOut`（401/403/500/200-id=0/200-正常账号五档） |
| H-5 register_reject 回显拒绝原因 | 已修 | `hub/internal/transport/transport.go`：注册拒绝与 hello 看齐，统一回 `"rejected"`，四种原因只记服务端日志。测试 `TestRegisterRejectReasonUnifiedP2R3`（四种拒绝全为 `rejected`） |
| H-6 ha.Client.Close() 并发双关 panic | 已修 | `hub/internal/ha/ha.go`：`Close()` 改 `sync.Once` 保护 `close(c.done)`，并发双关不再 panic。测试 `TestConcurrentCloseNoPanic`（5 轮 × 20 goroutine 并发 Close + 顺序再关一次） |
| H-7 Web /api/status 未走共享 CfgLock | 已修 | `hub/internal/web/web.go`：`status()` 改走共享 `s.cfgLock()` 在锁内快照 `WakeWord` 与 `RoomNames()`，再用 `s.mu` 读 `KWSStatus`；`fingerprintAPI` 未动（已在 `s.mu` 下、无写方）。测试 `TestStatusAndConfigPutNoRace`（500ms 内 GET status 与 PUT config 并发，`-race` 下验证） |
| H-8 transport TLS 未设 MinVersion | 已修 | `transport.go`：`tlsConf` 加 `MinVersion: tls.VersionTLS12`。测试 `TestServeTLSMinVersion12P2R3`（TLS 1.1 客户端握手失败、TLS 1.2 正常 hello_ok）；gosec 复扫证实该 G402 已消失 |
| H-9 并发 CheckQR 内存 cookie 与落盘不一致 | 已修 | `netease_auth.go`：`save()` 全程持 `a.mu`（文件写也在锁内），再更新内存。测试 `TestNeteaseSaveConcurrentConsistent`（20 goroutine 并发 save 后文件 cookie == 内存 cookie，且无残留临时文件） |
| H-10 gosec 其余 8 项无记录 | 已修（扫描+裁决落盘） | 见 §5。未加 CI gosec job，CI 决定不变 |
| H-11 /api/music/lyric?id= 负数未校验 | 已修 | `web.go` `musicLyric`：`id < 0` 回 400“id 非法”；`hub/internal/router/router.go` 的 `MusicLyric` 也拦一道，防其他调用方把负数拼进上游。测试 `TestMusicLyricNegativeIDRejected` |
| H-12 /api/netease/login/qr 无创建节流 | 已修 | `web.go`：每 IP 每分钟最多新建 10 个二维码，超限回 429 且不调上游；`qrAttempts` 惰性初始化（`New()` 外的构造也不怕 nil map）。测试 `TestNeteaseLoginQRThrottle`（前 10 次 200、第 11 次 429、另一 IP 不受影响） |
| H-13 P2-29 ProbeModel 死代码 | 已修（删） | `hub/internal/discovery/discovery.go`：删 `ProbeModel`、已无调用方的 `serverNameForCert` 与 `fingerprintAllowed`，并更新包注释；`discovery_test.go` 删 6 处调用所在的 4 个测试 + 明文测试里的 ProbeModel 调用及随之无用的 `stubHandler`/`startFakeHubTLS` 与导入。核实：生产代码无调用方（全仓 grep 仅定义与测试） |

### 1.2 客户端面（C-3 ~ C-15，13 项）

| 项 | 状态 | 改动 |
|---|---|---|
| C-3 AEC 回归测试空洞 | 已修 | `client/src/aec.rs` 测试区补三例：`pure_voice_with_nonzero_reference_is_preserved`（非零参考 + 纯语音）、`echo_delay_off_by_one_frame_degrades_predictably`（±1 帧失配均不抑制，且残余 >2× 对齐残余）、`zero_reference_mid_stream_then_recovers`（中途 10 帧零参考后恢复，尾段抑制回到 <50%）。旧 `pure_voice_without_echo_is_preserved` 改名改写为非零参考版；保留 `zero_reference_passes_mic_through_unchanged`（短路分支本身仍需覆盖）。断言口径见 §4（总 RMS 不能用，改用有符号正交投影 + 总 RMS 双边界） |
| C-4 零能量守卫阈值 1e-9 几乎永不触发 | 已修（注释修正，阈值不动） | `aec.rs:47-56`：`AEC_MIN_REF_NORM` 注释写实——守卫只拦单采样毛刺，防发散靠 `±AEC_MAX_STEP` 钳制；阈值 1e-9 未改（改口径会动行为） |
| C-5 ref_tx 满时静默丢帧 | 已修 | `client/src/main.rs`：`ref_tx` 满时计数 + 5 秒节流 warn（P2-N5 同款），共用新建 `DropTracker`。测试 `drop_tracker_logs_first_drop_then_throttles_until_5s` |
| C-6 gnueabihf 脚本缺 ALSA 标记断言 | 已修 | `client/build-armv7-gnueabihf.sh:137-160`（2b 节）：musl 脚本 2b 节 ALSA 标记断言原样补进；`--no-default-features` 透传时跳过并 WARNING（与 musl `--allow-no-alsa` 语义对齐）。第 6 节 vendor .so 警告/失败逻辑未碰（等 bai 拍板）。四分支逻辑实测验证（有标记 OK / 无标记 FAIL exit 1 / 透传时 WARNING exit 0 / 其他透传参数仍 FAIL） |
| C-7 rust-toolchain.toml 无 targets 列表 | 已修 | 加 `targets = ["armv7-unknown-linux-gnueabihf", "armv7-unknown-linux-musleabihf"]`，注释修正（旧“自动下载”不成立）；本机两 target 已装，无下载 |
| C-8 kws.rs:311 仍有 .expect | 已修 | `client/src/kws.rs:311-325`：`.expect("frame is 320 samples")` 改为 `debug_assert_eq!` + `match` 返回 `Err`；生产代码无 `.expect` 残留（已 grep 核实）。FIXES_P2.md:85 表述同步收窄为“编解码函数不再有任何 `.expect` panic 路径”（见 §3） |
| C-9 播放路径每帧一条 info! 日志 | 已修 | `main.rs` 播放分支：`info!("played {}-sample frame via ALSA")` 降为 `debug!` |
| C-10 无 WS Ping、无发送超时 | 已修 | `client/src/transport.rs`、`main.rs`：全部 `send_*` 走带 `SEND_TIMEOUT = 5s` 的 `ws_send`（超时按断线重连）；新增 `send_ping()`，`run_session` 每 30s 随 heartbeat 发 Ping；`TcpStream` 开 SO_KEEPALIVE（libc setsockopt：30s 空闲/10s 间隔/3 次，无新增依赖）；录音任务 `wake_tx.send(ev).await` 改 `try_send` + 丢事件计数节流日志。测试 `client_sends_ping_and_enables_tcp_keepalive`、`send_times_out_when_peer_stops_reading`（对端停读 60s、循环发 1KB 帧，两次全量套件均稳定在 ~5s 超时报错） |
| C-11 P2-19/P2-N4 接线无测试 | 接受现状 | 录音任务是 `main()` 内 spawn 的闭包、无注入点；抽为可测函数是结构性改动，P2-19/P2-N4 的修复代码本身在，缺的只是录音任务本体接线测试。收益（两条接线测试）小于重构录音任务 + 采集/AEC/编码交织路径的风险，不做 |
| C-12 TestArbiterWindow flake | 已修 | `hub/internal/arbitration/arbitration_test.go`：去掉 `Sleep(50ms)`，两事件背靠背提交仍在同一 100ms 窗口内；生产仲裁逻辑未动 |
| C-13 文档/注释过期 + Audio::open() 可公开双开 | 已修 | `transport.rs`：模块头“上限 1s”→30s；“未配置指纹跳过校验”→指纹必填（P1-10），回退分支仅 validate 被绕过时可达；`connect_with_retry` 注释与报错文案“3 times in a row”→累计 3 次（`auth_failures` 从不归零）。`audio.rs`：已核实生产代码只用 `open_capture_only`/`open_playback_only`（main.rs:531/700），`Audio::open()`、`Audio::open_devices()` 及 `AlsaAudioDevice::open_devices()` 均改为 `#[cfg(test)]`，生产误调即编译失败 |
| C-14 FingerprintVerifier 不校验有效期与 SAN | 已知延期，不新开工作 | P2-1 范围已知延期，下次动 TLS 验证时一并补有效期 + SAN 校验 |
| C-15 P2-42 残留：hub_url 默认写死 | 待 bai 定夺，未改 | `client/src/config.rs:12-14` 默认仍是 `wss://192.168.1.10:4399`；改必填是 breaking change，等 bai 拍板 |

### 1.3 固件/部署面（F-4 ~ F-11，8 项）

| 项 | 状态 | 改动 |
|---|---|---|
| F-4 root hash 移植未脚本化 | 已核实关闭（审计前提已过期） | 机制已在 P1 轮落地：`firmware-patch/scripts/patch_firmware.py:384` `_reuse_root_hash()`（`PATCH_REUSE_ROOT_HASH_FILE` 沿用旧 `$6$` hash，不读密码、不生成新盐）、测试 `test_patch_firmware.py:258-286`、流程见 `firmware-patch/FLASH_RUNBOOK.md:41`。本次核实：两份已交付补丁镜像的 shadow root 行与本地 gitignore 备份 `firmware-patch/work/.root-hashes-backup` 逐字节一致（只比对相等性，未打印 hash）。另在 README root 密码行补了指向 runbook 的一句话。残留：首刷后串口 + bai 已有密码实测登录仍是 BLOCKED（需真机） |
| F-5 PATCH_NO_CERTS=0 也跳过证书 | 已修 | `firmware-patch/scripts/patch.sh:76`：`[ -n "${PATCH_NO_CERTS:-}" ]` → `[ "${PATCH_NO_CERTS:-}" = "1" ]`。手工验证：`=0` / `=yes` / 未设在证书目录缺失时均 fail“证书目录不存在”，`=1` 才跳过。回归测试在 `test_patch_sh_requires_cert_dir` 内加 `=0` / `=yes` 用例 |
| F-6 inode 硬门 fail-open | 已修 | `patch.sh:203-205`（§6）：读不到即 `fail`（删半成品镜像）；`unpack.sh:117-121` 同款改为 exit 1。新增 `_unsquashfs_no_inodes_shim` + 两个回归测试（shim 在 `-s` 时去掉 inode 行、其余透传真实 unsquashfs）。实测机制仍通过：LX06 补丁镜像 `unsquashfs -s` 2144 inodes、find 计数 2144；OH2P 2105/2105（原厂对照 LX06 2137、OH2P 2098）。flash-check.sh 经查无 inode 门控，不存在同款 fail-open，未动 |
| F-7 README:47 旧 rc.local 直行 | 已修 | `firmware-patch/README.md`“启动”行同步为包装器行 `[ -x /etc/open-xiaoai/init.sh ] && /etc/open-xiaoai/init.sh >/dev/null 2>&1 &`，与 `patch_firmware.py:55-56` `RC_LOCAL_WRAPPER_LINE` 逐字一致，并注明旧直行已废弃 |
| F-8 两台音箱共用 mTLS 证书 | 已修（仅文档澄清） | `firmware-patch/scripts/gen-certs.sh` 头注释 + `firmware-patch/README.md` mTLS 行：写明共用身份含义——吊销/轮换无隔离，吊销或 `--force` 重建同时影响两台，Hub 无法只禁其中一台。决定：不做按设备独立签发。理由：那是较大改动——需改 patch.sh 的证书选取（按型号分目录）、Hub 客户端身份映射，并重刷两台镜像；无测试覆盖；家用 + 局域网 + mTLS 前提下，`--force` 全量轮换已能覆盖吊销场景，风险可接受 |
| F-9 boot.img ramdisk /init 免密兜底 shell | 已核实关闭（接受现状并记录） | `firmware-patch/README.md` 新增“已知接受（审计 F-9 / F-10，不修）”一节：仅 `switch_root` 失败路径可达，boot.img 不在补丁范围（补丁只改 root.squashfs；grep 确认 patch 脚本无任何 boot.img 处理），未改 boot.img |
| F-10 每日 03:00 cron 跑 /bin/ota slient | 已核实关闭（接受现状并记录） | 同上记入 README。实测核实：两份补丁镜像 `etc/crontabs/root` 均有该行；`bin/ota` 的 `check()` 只调只读的 `matool_check_upgrade`，`upgrade()` 开头 `return 1` 已死（`patch_firmware.py:patch_disable_ota` 保证）。未删 cron 行：删行改镜像内容、触发重编，对 P2 不值得；审计本身也列“接受现状”为合法选项 |
| F-11 FIXES_P2.md:130 的 P2-39 实测数过期 | 已修（文档，协调员改） | FIXES_P2.md:130 改为：“补丁镜像 2141/2141（2026-11-17 旧重编值；2026-11-18 P1 修复轮重编后实测 LX06 2144/2144、OH2P 2105/2105，`unsquashfs -s` 与 find 计数一致）” |

### 1.4 边车面（S-4 ~ S-14，11 项）

| 项 | 状态 | 改动 |
|---|---|---|
| S-4 无单帧大小校验 | 已修 | `kws-sidecar/server.py`：`max_size` 1MB→64KB（`MAX_WS_MESSAGE_SIZE`）；二进制帧非 640 字节即 error+丢弃，不再切块喂入 |
| S-5 连续错误计数是全局的，_fatal_error 粘住永不恢复 | 已修 | `server.py`：错误计数按 `device_id` 分桶（`_stream_errors`，无参调用仍兼容旧测试）；`_fatal_error` 置起后连续 5 次成功喂入自动清除并记日志；断开清该设备分桶。设计选择：分桶计数 + fatal 自动清除两者都做了（审计的“或”方案中更彻底的组合，未改 `/health` 的 fail-closed 语义） |
| S-6 单帧喂入异常直接掐断整条连接 | 已修 | `server.py`：单帧异常记 error、复位该设备流（新增 `DeviceStream.reset_stream()`）并 `continue`；仅该设备分桶连续失败达 5 次才断连接（与 S-5 联动） |
| S-7 基础镜像未钉 digest | 已修 | `kws-sidecar/Dockerfile`：钉 `python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b`（Docker Hub API 查得，协调员已用同一 API 独立复核一致，`last_updated: 2026-11-08`；x86_64 对应镜像 digest `sha256:05a994d2…` 也记在注释里）；apt 钉版本（`curl=7.88.1-10+deb12u15`、`bzip2=1.0.8-5+b1`，2026-11-18 查 bookworm Packages）；加 `USER 1000:1000`（mkdir/chown 之后、ENTRYPOINT 之前，与 compose 一致）。更新流程写进 Dockerfile 注释。构建验证待 NAS 实测（本机无 docker） |
| S-8 KWS_HOST 默认 0.0.0.0 | 已核实关闭（P1 轮已修） | 三处复核均为 `127.0.0.1`（Dockerfile ENV、server.py `--host` 默认、compose）；全文 grep `0.0.0.0` 零命中，未改代码；另加守护测试防回归 |
| S-9 SC-P2-8 缺口：部分缺失时已存在语音文件不验 SHA | 已修 | `kws-sidecar/tests/bootstrap.py:_ensure_voice()`：下载循环前先对已存在文件逐个验 SHA（复用 `_voice_sha_ok`），坏的删掉重下；与全存在路径一致 |
| S-10 flush 检出不计入 total_wakes | 已修 | `server.py`：flush 检出时 `total_wakes`+1 并单记 `lost_wakes`；`device_wakes` 改为累计值（`_device_wakes_total` + 当前连接计数），断开后保留；`/health` 新增 `lost_wakes` |
| S-11 被取代的旧连接关闭超时后滞留 | 已修 | `server.py`：取代时 close 5 秒超时后显式 `old_task.cancel()`（登记 `_handler_tasks`，旧 handler 的 finally 释放计数）；`/health` 新增 `connections` / `authenticated_connections`，区分半开与已认证连接。测试证实取消后连接计数在 10 秒内回到 1、新连接可正常喂音频 |
| S-12 KWS_SCORE 无上界，参数无校验 | 已修 | `server.py`：score 上界 [0,10]（超限 FATAL）；`_env_float`/`_env_int` 解析非法环境变量时打印 FATAL 后 `sys.exit(1)`，无 traceback；num_threads 限 1~8、port 限 1~65535 |
| S-13 wake_no / frames_since_wake 未进事件 | 已修 | `server.py`：wake 事件新增 `wake_no`（= `stream.wake_count`）与 `frames_since_wake`（= `frames_at_last_wake`）；server.py 模块协议文档与 README 同步更新；Hub 侧容错解析、无需改动 |
| S-14 指纹归一化两处不一致 | 已修 | `hub/internal/config/config.go`：新增导出 `NormalizeFingerprintHex`；`hub/internal/kws/kws.go`：`normalizeFingerprintHex` 改为委托 `config.NormalizeFingerprintHex`（并删因此无用的 strings 导入）。两处共用同一归一化函数（去冒号/空格/连字符，转小写）；报错文案改为“64 位 hex（大小写不限）”。测试：config `TestNormalizeFingerprintHex` / `TestValidateKWSFingerprintHyphenAndCase`、kws `TestNormalizeFingerprintHexSharedWithConfig` / `TestNewAcceptsHyphenatedFingerprint` |

## 2. 全量测试重跑输出（协调员亲跑，2026-11-18）

| 命令 | 结果 |
|---|---|
| `go test -count=1 ./...`（hub/，用 env.sh） | **22 包全 ok、292 passed、0 failed**（审计基线 270；P1 轮 277；本轮新增 19 个回归测试，H-13 删 4 个测试，277+19-4=292） |
| `go vet ./...` / `gofmt -l .` | 干净 / 无输出 |
| `go test -count=1 -race ./internal/{music,ha,web,transport,kws,arbitration,config,router,discovery}/` | **全部 ok，无 race**（含 H-7 的 status/PUT 并发测试与 H-9 并发 save 测试） |
| `cargo test --locked`（client/） | **110 passed，0 failed**（P1 轮 105 + 本轮新增 5） |
| `cargo test --locked --features sherpa-kws`（client/） | **115 passed，0 failed**（P1 轮 110 + 本轮新增 5；C-8 改动在该 feature 下编译并通过） |
| `cargo fmt --check` / `cargo clippy --locked --all-targets` | 通过 / **0 warning** |
| `cargo check --locked` | 通过（确认 `Audio::open()` 改 `#[cfg(test)]` 后生产构建仍通过） |
| `python3 -m pytest firmware-patch/tests/ -q` | **102 passed**（P1 轮 100 + 本轮新增 2 个 F-6 测试；351 秒） |
| `kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q` | **91 passed，1 skipped**（P1 轮 66 + 本轮新增 25；跳过的 1 项按设计 skip） |
| `./scripts/lint.sh` | **全部通过**（shellcheck 未装，只做 bash -n） |
| `flash-check.sh` 四路（两型号 × 默认/`--strict`） | **全部 exit 0** |

## 3. 仍 BLOCKED 的清单

1. **H-4 实测**（需 NAS）：`Status()` 的 401/403→未登录映射已修并有单测，但 NAS 上用过期 cookie 的实测未做；Binaryify `/login/status` 对过期 cookie 的行为仍以实测为准。
2. **F-4 串口登录实测**（需真机）：首刷后用串口 + bai 已有密码登录一次，核实 root hash 复用（镜像 shadow 与 gitignore 备份已逐字节比对一致，但“密码能登录”只能真机证实）。
3. **S-7 Dockerfile 构建验证**（需 NAS/docker）：本机无 docker，钉 digest + apt 版本 + `USER 1000:1000` 的 Dockerfile 未实际构建过；NAS 上重编边车镜像时跑 `docker compose up -d kws-sidecar` + `/health` 冒烟确认。
4. **真机项**（与前几轮相同，全部仍 BLOCKED）：刷机、NAS 部署、同步播放、真人唤醒率、双机仲裁、打断率、网易云扫码、HA 真联调、AEC 真实延迟校准、P2-35。
5. **待 bai 定夺**（本轮未碰）：C-15（P2-42）hub_url 改必填；P2-12 已删除（2026-11-23，不再定夺）；gnueabihf 构建遇到不兼容 vendor .so 是警告还是失败；S-3 边车 TLS 方案；HA 是否配 TLS；P1-40 长期服务端模式认不认。
6. **已知延期**：C-14（P2-1）指纹不校验有效期与 SAN，下次动 TLS 验证时一并补。

## 4. 取舍说明与观察项

- **H-3 未用严格 CDN 白名单**：审计建议“host 在网易云 CDN 域名表内（或至少拒绝 file://、内网 IP、localhost）”，本轮按“或”的备选方案实现——拒绝 file:// 等 scheme、localhost、任何裸 IP、单标签主机名、userinfo。严格表会把既有测试夹具 `http://music.example.com/...`（music/router 多处在用）判非法，且上游可能用其他网易云域名；如要更严，改为仅允 `*.music.126.net` / `*.music.163.com` 是一处改动。DNS 重绑定不在本项范围，注释已写明。
- **C-3 断言口径的实测结论**（记在测试注释里）：总 RMS 不能用——NLMS 稳态失调（mu=0.5）会给输出叠加噪声，实测总 RMS 10941 vs 输入 8485（+29%）；音调幅度 `|12000+Y|` 也不能用——随机相位噪声相量有 Jensen 上偏（实测 10 帧均值 13815，+15%）。最终用有符号正交投影（sin 分量 ±10%、cos 分量 <10%，100 帧、尾段 50 帧平均）+ 总 RMS 双边界（0.8×~1.5×）。
- **C-3 附带发现（未改代码）**：纯音参考下 NLMS 会互调抵消语音——300Hz 正弦参考 + 1000Hz 纯语音时，时变抽头互调出 1000Hz 分量反向抵消约 36% 语音。这是纯音合成信号的假象，真实播放信号（音乐/语音）为宽带，不触发；但说明 NLMS 在强纯音参考下有误伤语音的理论风险，真机打断场景值得留意。
- **C-11 接受现状**：抽录音任务为可测函数是结构性改动，收益（两条接线测试）小于重构风险，不做。
- **F-8 只做文档澄清**：按设备独立签发证书需改 patch.sh 证书选取、Hub 客户端身份映射并重刷两台镜像，无测试覆盖；家用 + 局域网 + mTLS 前提下 `--force` 全量轮换已能覆盖吊销场景。
- **S-5 组合方案**：分桶计数 + fatal 自动清除两者都做了，比审计的“或”方案更彻底，未改 `/health` 的 fail-closed 语义。
- **H-7 测试插曲（已修，测试侧）**：`TestStatusAndConfigPutNoRace` 初版曾挂住 90s+，原因是测试本身的 bug——用 `time.After(500ms)` 通道作停止信号，它只投递一次，两个 goroutine 抢，输的那个永远等不到。已改为 deadline 循环，测试现 0.5s 通过；生产代码无此问题。
- **S-11 测试侧调整**：websockets 17 连接对象无 `.open` 属性（改用后续合法帧喂入作存活证据）；S-11 里 `open_client` 的 5 秒等 ready 超时与服务端 5 秒关旧连接超时撞车（该用例改用 15 秒手动握手）。
- **C-6 四分支实测**：有标记 OK / 无标记 FAIL exit 1 / 透传时 WARNING exit 0 / 其他透传参数仍 FAIL。

## 5. H-10 gosec 补跑（新发现一项，待定）

- 安装：`go install github.com/securego/gosec/v2/cmd/gosec@latest` 得 v2.30.0（自动切 go1.26.11 工具链）。两次波折记在文档 §1：首次 240s 超时仍在下载；第二次编译报 `no space left on device`——根因是 Go 临时目录默认走 `/tmp`（本机 tmpfs 仅 512M），改 `GOTMPDIR=$HOME/workspace/.tmp/gotmp` 后成功。
- 结果（`hub/...`，2026-11-18）：**生产代码 29 项**（G104×12、G706×6、G115×5、G304×3、G123×1、G404×1、G402×1），裁决见 `docs/gosec-20261118.md` §2：28 项假阳性/低风险，0 项待修；**旧“14 项发现”用当前 v2.30.0 无法复现**（旧版无 G104/G706/G123 等规则，仓库又无当时原始输出），“8 项在测试代码”作为旧主张仍无法证实——但本次 `-tests` 扫描给出可核查的新数：**测试文件 45 项**（G104×17、G304×7、G115×6、G306×5、G112×3、G402×2、G705×2、G101×1、G204×1、G301×1），全部测试代码固有、无生产影响；其中 G402×2 是本轮新增的 H-8 测试（必须用 TLS 1.1 客户端拨号证明服务端拒绝，是有意的）。
- 旧 6 项生产发现现状：5 项仍在（3×G115、G402 kws、G404，裁决不变）；1 项已修——G402 transport MinVersion（H-8）本次扫描已消失。
- **新发现（待定，本轮未修）**：**G123 `kws.go:281`**（`VerifyPeerCertificate` 未配 `VerifyConnection`，会话恢复可能跳过 pin）——低风险待定（恢复复用的是首次握手已 pin 校验的会话，且对端是回环边车），是否加 `VerifyConnection`/禁会话票据需定夺。本轮未修：它动的是 TLS pin 校验行为，而 S-3 的 TLS 决策还在等 bai，不宜在 S-3 未定前擅自改。
- 落盘：`docs/gosec-20261118.md`（裁决全文）、`docs/gosec-20261118.json`（生产代码原始输出）、`docs/gosec-20261118-tests.json`（测试代码原始输出）。未加 CI gosec job。

## 6. 交付物

- 报告：`FIXES_P2_ROUND3.md`（本文件）
- PR：待开（分支 `fix-p2-round3`，基于 main `5351ac0`）
- 改动统计：27 个修改文件 + 11 个新增文件（7 个 Hub 测试文件、1 个边车测试文件、3 个 gosec 文档），+1189/−368 行（不含本报告与 FIXES_P2.md 的两处文档修正）
- 镜像：未重编（见 §0）
