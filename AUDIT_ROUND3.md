# open-xiaoai-next 第三轮审计报告（AUDIT_ROUND3.md）

- 审计日期：2026-11-18
- 审计分支：`audit-round3`（从 main `3a9d1d9` 切出，PR #4 刚合入）
- 前两轮报告：`AUDIT_REPORT.md`（2026-11-17，108 项 P0 6 / P1 46 / P2 56）、`AUDIT_ROUND2.md`（2026-11-17 晚，55 项 P0 1 / P1 14 / P2 40）
- 本轮分工报告（证据全文）：`.audit-round3/hub.md`、`.audit-round3/client.md`、`.audit-round3/firmware-deploy.md`、`.audit-round3/kws-sidecar.md`

## 0. 结论先行

- **新问题共 53 项：P0 0 / P1 9 / P2 44。** P0 为零——本轮没有发现部署前必须修的安全/正确性硬伤。
- **最严重的 9 个 P1（部署前修）**：H-1 点歌无总超时（上游一抖静默 ~90s）；C-1 AEC 标称延迟未经验证且代码不回读实际缓冲（可能全程零作用还不报错）；F-1 打包验证三处标记为空转（原厂镜像也能通过）；S-1 边车算的 `rms_peak` 被 Hub 整个丢弃（SC-P2-2 的仲裁目标落空）；F-2 inittab 另两行免密 root shell（tts/0、tty1）；S-2 全新卷直接 `compose up` 会 crash-loop（SC-P2-12 只修一半）；S-3 边车根本没有 TLS，Hub 的指纹 pin 是死配置；C-2 CI 从不编译 `sherpa-kws` feature（P2-20/P2-21 零回归保障）；F-3 P2-34 的 SHA256 pin 永不触发（无写入方）。
- **固件面可以刷**：两份重编镜像内容核实全部通过（P1-36、P2-33、P2-34 三缺口确实在镜像里，证书、shadow、OTA 禁用、libxaudio 均就位），部署面 digest、cap_drop、user、清单校验、MA 清理也全部核实通过。但首刷前建议先修 F-1（否则下次重编的打包验证拦不住回归），并定夺 F-2 与 F-3；F-4 的 root hash 复用只能留待首刷时用串口密码登录实测。
- **融合 B 的 2,699 行新代码首次被审计**：11 条路由全鉴权、cookie 0600 落盘与登出清盘正确、直连 + 本地兜底 + 明说的三级降级闭环、MA 零活残留、music_folder 降级为警告无后遗症——整体接线良好，唯一 P1 是 H-1 的超时预算。
- **P2 轮修复抽查**：Hub 10 项 headline 全部属实；客户端 11 项属实、2 项有缺口（C-6、C-8）；边车 11 项属实、2 项部分落实（SC-P2-2、SC-P2-12）另 1 处小缺口（S-9）；固件/部署 headline 全部属实。没有发现“声称已修实际没修”的项。
- **P2 轮待定项裁决**：gosec 14 项中 6 项生产代码逐项裁决（5 假阳性、1 真阳性低风险 H-8），其余 8 项无任何记录可查（H-10）；P2-29 ProbeModel 结论是删（H-13）；alerts.Watcher 无 Stop 经核实无泄漏；P2-10/P2-21 本地 KWS 残留、P2-42 hub_url 默认、read_only 暂缓均只记录现状，不替 bai 决定。

## 1. 审计范围与方法

### 1.1 范围

- **Hub（Go）**：B 融合新增代码（`hub/internal/music/netease_auth.go`、11 条 `/api/netease/*` 与 `/api/music/*` 路由、Hub 直连网易云路径、music_folder warn-only 启动校验、MA 残留）+ P2 轮 Hub 修复抽查 + gosec 裁决 + P2-29 / alerts.Watcher / P2-10/P2-21 待定项。
- **客户端（Rust）**：`client/src/aec.rs`（Opus 回声消除）+ rust-toolchain.toml / ci.yml 同步 + P2 轮客户端修复抽查 + TestArbiterWindow flake + unsafe 审计。
- **kws-sidecar（Python）**：`kws-sidecar/server.py` 全文 + SC-P2-1~13 逐项复核 + N-P2-10 TLS pin + 与 Hub 的契约。
- **firmware-patch + deploy + CI**：重编镜像内容核实（unsquashfs 解开两份新镜像）+ patch.sh / flash-check.sh / nas-install.sh / compose 加固 + P2-35。
- **编号口径**：H- = Hub 面，C- = 客户端面，F- = 固件/部署面，S- = 边车面；编号与分工报告一一对应，便于追溯。

### 1.2 方法

四面并行审计，每面先读代码、再 grep 交叉验证生产调用点；文档与代码冲突时以代码为准；能实测的都实测（镜像用 unsquashfs 解开逐项核对、digest 与 Docker Hub 实测值逐一比对）；真机才能验证的项一律记 BLOCKED，不写成通过；找不到证据写“未确认”，不猜。

### 1.3 基线测试结果（协调员亲跑，main 3a9d1d9）

| 命令 | 结果 |
|---|---|
| `go test -count=1 ./...`（hub/，用 env.sh） | **270 passed，0 failed**（22 个包全部 ok）；`go vet` 干净 |
| `cargo test`（client/） | **96 passed，0 failed**；`cargo fmt --check` 通过；`cargo clippy --all-targets` 0 warning |
| `cargo test --features sherpa-kws`（client/） | **101 passed，0 failed** |
| `python3 -m pytest firmware-patch/tests/ -q` | **86 passed**（PATH 需加 `~/workspace/tools/squashfs-tools/usr/bin/`，VM 换机后系统无 unsquashfs；不要走 apt，apt update 会卡死） |
| `kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q` | **55 passed** |
| `./scripts/lint.sh` | **全部通过**（shellcheck 未装，只做 bash -n） |

### 1.4 局限

- 无音箱硬件、无 NAS：全部真机项（刷机、NAS 部署、同步播放、真人唤醒率、双机仲裁、打断率、网易云扫码、HA 真联调、AEC 真实延迟校准、P2-35）均未验证，记 BLOCKED。
- Root 密码 hash 与旧镜像的一致性无法比对：旧镜像（SHA `7266aba9…`/`139de363…`）已不在磁盘上，无任何旧 hash 记录可供对照（见 F-4）。
- gosec 本机未安装，按任务要求未重跑；14 项发现中 8 项在仓库内无任何记录，无法裁决（见 H-10）。

## 2. 分级统计

| 分级 | 新问题 | 其中最严重 |
|---|---|---|
| P0 | 0 | — |
| P1 | 9 | H-1 点歌无总超时；C-1 AEC 延迟未验证；F-1 打包验证空转；S-1 rms_peak 被丢弃；F-2 inittab 另两行免密 shell；S-2 全新卷 crash-loop；S-3 边车无 TLS；C-2 CI 不编译 sherpa-kws；F-3 SHA256 pin 死代码 |
| P2 | 44 | H-2 MPD 响应无界读；H-3 SongURL 未验 scheme；H-6 HA Close 双关 panic；S-7 边车镜像未钉 digest；C-10 无 WS Ping/发送超时 |
| 合计 | **53** | — |

## 3. P0 级新问题（0 项）

本轮 P0 为零。上一轮唯一的 P0（P0-N1 客户端双开 ALSA）修复仍在：录音任务用 `open_capture_only`、会话用 `open_playback_only`，main.rs 只用这两个构造器，回归测试在 `client/src/audio.rs:1107–1167`。

## 4. P1 级新问题（9 项，部署前修）

### H-1 点歌路径无总超时预算，上游抖动时最坏静默 ~90s，FR-05「3s 内出声」失效

**结论：** 正常路径很快，但网易云一抖，单次语音点歌可静默阻塞近 90 秒且中途无任何反馈。

**证据：** `hub/internal/router/router.go:803` `Search(ctx, query, 3)`；`:819–824` 对 3 首歌逐首串行 `SongURL` → `PlayURL`；`hub/internal/music/music.go:225` `NewNetease` 客户端超时 15s；`music.go:281` `SongURL` 无重试；`hub/internal/voice/voice.go:83` 整条语音管线总超时 `answerTimeout = 120s`。最坏算术：搜歌 15s + 3×（取地址 15s + MPD 操作数秒）≈ 60–90s，之后才走本地兜底或回「放不了」。其间用户听不到任何进度，设备也无法响应其他指令。

**修复建议：** 给点歌单独设总预算（如 search+SongURL 合计 ≤8s，超时即转本地兜底）；或 3 首歌的 `SongURL` 并行取、最先成功者播放；超时/失败时先播报「网易云有点慢，先试本地」再继续。

### C-1 AEC 标称延迟 80ms 未经验证，且代码不回读实际 ALSA 缓冲大小

**结论：** `AEC_REF_DELAY_FRAMES = 4` 只是理论值，若真实缓冲不是 4 周期，回声会落在 256 抽头窗之外，AEC 静默失效且无任何告警。

**证据：** `client/src/aec.rs:42` `pub const AEC_REF_DELAY_FRAMES: usize = 4;`，模块头注释（aec.rs:27–29）自认“只是 ALSA 缓冲 4 周期的理论值，真实声学延迟需真机校准”。`client/src/audio.rs:458–465` `open_pcm` 用 `set_period_size_near` / `set_buffer_size_near(4*320)` 设缓冲，`_near` 语义是“尽量接近”，随后没有任何回读（无 `buffer_size()` / `period_size()` 读取、无日志、无断言）。硬件若取整为 3 或 5 周期，延迟偏差 20ms 即超出 NLMS 窗口（256 抽头 = 16ms）。另有两处加剧不确定性的因素：采集 `hw:0,0` 与播放 `hw:0,2` 是两个 PCM 设备，延迟以“帧计数”而非墙钟对齐，速率稍有漂移即表现为延迟抖动；`aec.rs` 注释称 NLMS 窗口“覆盖该延迟前后各 16ms”，实为单侧 0~16ms（抽头只覆盖标称延迟之后），容差比注释写的更紧。

**修复建议：** `open_pcm` 设完参数后回读实际 `buffer_size` / `period_size` 并记日志；若与 4 周期不符则启动时明确 warn（或据实推导 `AEC_REF_DELAY_FRAMES`）；真机校准仍是 BLOCKED 项，刷机验证时实测回声延迟。

### F-1 打包后验证三处标记为空转——原厂镜像也能通过（patch.sh §6 / flash-check.sh §4b）

**结论：** 三处“补丁标记”检查无法区分已打补丁与原厂镜像，打包验证形同虚设。

**证据：** 对原厂 `firmware-patch/work/<型号>/rootfs` 实测——`etc/init.d/dropbear` 原厂第 104 行已有 `mkdir -p /data/etc/dropbear`（在 `keygen()` 内，原厂自带）；`etc/init.d/wireless` 原厂已有 1 处 `echo "download_fail"`（curl 失败分支）；OH2P 原厂 `etc/asound.conf` 已有 2 处 `slave.pcm Capture`。而 `patch.sh:247,252,229` 与 `flash-check.sh:180,196,206` 恰好用这三处 `grep -q` 作“已打补丁”断言。另 `inittab` 的 `ttyS0 login` 检查同理（原厂即有，仅能作回归 guard）。live 镜像本身是正确打过补丁的（直接核查了门控删除与函数头），且 `patch_firmware.py` 的进程内校验是正确的，所以这是纵深防御失效，不是 live 缺陷。

**修复建议：** 改断言为补丁特有标记——dropbear 查三门控已删（`_require_not_in` 同款）；wireless 查 `ota()` 头后 60 字符内同时有 `echo "download_fail"` 与 `exit 3`（照抄 `patch_firmware.py:patch_disable_ota` 的校验）；OH2P asound 查 Capture 块参数或与 `.bak` 的差异。两处脚本同步改，并补回归测试（拿原厂 rootfs 跑 §6 应 FAIL）。

### S-1 `rms_peak` 被 Hub 整个丢弃，SC-P2-2 的仲裁目标落空

**结论：** 边车算了也发了 `rms_peak`，Hub 的解析结构里根本没有这个字段，仲裁继续用会被 trailing 静音稀释的 1s 均值 `rms`。

**证据：** 边车发送：`kws-sidecar/server.py:441`，wake 事件含 `"rms_peak": stream.rms_peak`；`server.py:230-236` 为峰值属性；回归测试 `kws-sidecar/tests/test_sidecar_p2.py:119-141` 断言事件含 `rms_peak` 且 `rms_peak >= 0.25`（pos1 实测峰值 0.311、均值约 0.137，标定合理：峰值帧在检出窗口内，均值确被稀释）。Hub 丢弃：`hub/internal/kws/kws.go:798-812`，`handleMessage` 的 wake 解析结构只有 `device_id/keyword/rms/timestamp_ms` 四个字段；`WakeEvent`（`kws.go:58-64`）同样无 `RMSPeak`；全仓库 grep `rms_peak`/`RMSPeak` 在 Go 侧 **0 命中**。仲裁仍用均值：`hub/cmd/hub/main.go:262-272`，`OnKWSWake` 用 `ev.RMS` 做 `20*log10(rms)` 送仲裁；两台设备检出延迟不同时，稀释程度不同，比较结果会被系统性扭曲。

**修复建议：** Hub 侧三处同步加 `RMSPeak`：`handleMessage` 解析结构、`WakeEvent`、`OnKWSWake` 仲裁换算优先用 `rms_peak`（缺失时回退 `rms`，并记一条 warn）；补一个 Hub 单测断言含 `rms_peak` 的 wake 事件进仲裁用的是峰值。

### F-2 inittab 仍有两处免密 root shell——tts/0 与 tty1 的 `/bin/ash --login`，只守住了 ttyS0

**结论：** P1-36 只修了串口三行中的一行；另两行无任何代码检查。

**证据：** 两镜像 `/etc/inittab` 全文三行控制台——`tts/0::askfirst:/bin/ash --login`、`ttyS0::askfirst:/bin/login`、`tty1::askfirst:/bin/ash --login`；`patch.sh:236` §6 只断言 `ttyS0` 行，`patch_firmware.py:check_login`（:456-457）同样只要求 `ttyS0` 行。内核侧证据（降低定级为 P1 而非 P0 的原因）：解压 LX06 `boot.img` 内核（8,241,160 字节）实测——meson-uart 驱动注册的设备名是 `ttyS`（内核内 `ttyS\x00` 出现 2 次、`tts\x00` 出现 0 次），`tts/0` 在 4.9 内核 + devtmpfs 下大概率是不存在的死项；`tty1` 是虚拟终端，音箱无键盘/显示不可达。但以上均未在真机启动日志上证实。

**修复建议：** 把三行统一改为 `/bin/login`（或在 `patch.sh` §6 增加“全表无免密 shell”断言）；真机首刷后核对启动日志确认 `/dev/tts/0` 不存在。

### S-2 SC-P2-12 只修一半：全新卷 + 直接 `compose up` 会 crash-loop

**结论：** compose 的 `user: "1000:1000"` 和 `nas-install.sh` 的 chown one-shot 都在，但镜像本身对卷属主毫无处理，README 的直接部署路径在全新卷上必失败。

**证据：** 落实的部分：`deploy/docker-compose.yml` kws-sidecar 服务有 `user: "1000:1000"`；`deploy/nas-install.sh:222` 有 `docker compose run --rm --user root --no-deps --entrypoint /bin/chown kws-sidecar -R 1000:1000 /data/kws/models`。缺失的部分：`kws-sidecar/Dockerfile` 全文 grep `mkdir|chown|USER` **0 命中**——没有建 `/data/kws/models`、没有改属主、没有 `USER`。而 compose 注释却写“新卷的首次 copy-up 由 Dockerfile 里的 mkdir + chown 负责”，与事实不符。失败链：全新 `kws-data` 卷以 root 属主挂载 → uid 1000 的 entrypoint 跑 `scripts/download-model.sh` 写不进 `/data/kws/models` → `set -euo pipefail` 下 `cp` 失败退出 → `restart: unless-stopped` 循环重启，唤醒全断（失败是响亮的，不会静默，但 README 写的 `cd ../deploy && docker compose up -d kws-sidecar` 路径即中招）。

**修复建议：** 二选一并写进 compose 注释纠正错误说法：(a) Dockerfile 加 `RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models`（具名卷首次 copy-up 会带走属主）；或 (b) entrypoint 在下载前对不可写的卷目录做一次可读的预检报错。并补一个“不用 nas-install.sh、只 compose up 全新卷能起来”的部署冒烟项。

### S-3 N-P2-10 半边落实：Hub 的指纹 pin 做了，边车根本没有 TLS

**结论：** Hub 侧 pin 实现正确，但边车只监听明文 `ws`、无任何 TLS/证书逻辑，`wss + 指纹` 在真实部署里不可用，token 与 PCM 目前靠回环隔离 + token 裸奔。

**证据：** Hub 侧落实：`hub/internal/kws/kws.go:tlsConfig()`（`InsecureSkipVerify` + `VerifyPeerCertificate` 校验有效期与 SHA-256 指纹）、拨号与健康检查共用同一配置；`kws.New` 校验指纹为 64 位 hex；`hub/internal/config/config.go:276-285` 启动期再校验一次；`hub/cmd/hub/main.go:909` 把 `cfg.KWS.Fingerprint` 传入。边车侧缺失：`kws-sidecar/server.py` 全文无 `ssl`/`SSL`，`serve(..., max_size=1<<20)`（`server.py:571`）无 `ssl` 参数；`deploy/` 与 `nas-install.sh` 里没有任何为边车签发证书的逻辑；`hub/config.yaml:15` 实际配的是 `ws://127.0.0.1:10400/v1/kws`（明文）。现实风险面：compose 把 `KWS_HOST=127.0.0.1`（`docker-compose.yml` kws-sidecar 环境变量），Hub 也连 `127.0.0.1`，流量不出回环，局域网抓包风险不存在；但任何 NAS 本机进程都可直连 `127.0.0.1:10400`，token 以明文 JSON 在 hello 里传输（`server.py:382-383` 只校验不加密），且 `wss://` 一配就连不上（fail-closed，算优点）。

**修复建议：** 部署前二选一并写进决策：(a) 接受“回环 + token”并把“边车不启用 TLS，依赖 127.0.0.1 绑定”写为正式安全假设，同时把 `KWS_HOST` 默认值改为 `127.0.0.1`（见 S-8）；或 (b) 给边车加 TLS（自签证书 + 启动期生成/挂载），让 Hub 的 pin 真正跑通并补端到端测试。不要让 `fingerprint` 配置项长期处于“配了也永远用不上”的状态。

### C-2 CI 从不编译/测试 `sherpa-kws` feature，P2-20/P2-21 的修复无 CI 兜底

**结论：** Rust job 只跑默认 feature，`sherpa.rs` 整个文件（含 `decode_ready_loop` 及其单测）与 `kws.rs` 的 sherpa 分支在 CI 里从未被编译。

**证据：** `.github/workflows/ci.yml:88–96`：`cargo test --locked`、`cargo fmt --check`、`cargo clippy --all-targets`，均未加 `--features sherpa-kws`。`client/src/sherpa.rs:39`：`#![cfg(feature = "sherpa-kws")]`，整个文件门控；`client/src/sherpa.rs:724–751` 的 `decode_ready_loop_caps_stuck_is_ready` 单测、P2-20 的停后端逻辑（kws.rs:180 的 `KWS_FEED_DISABLE_AFTER = 50` 及 feed 内 sherpa 分支）只在 ARM 构建脚本触发的编译中才会被检查。后果：后续改动改坏 sherpa 路径时 CI 仍全绿，只能靠 VM 上的 ARM 构建才发现。

**修复建议：** CI rust job 增加一步 `cargo test --features sherpa-kws`（x86_64 宿主机即可；需真实 .so 的端到端测试无环境变量时自动 SKIP，只有 `decode_ready_loop` 单测与结构体尺寸单测会实际跑，正好覆盖 P2-21）。

### F-3 P2-34 的 SHA256 pin 校验永不触发——没有任何写入方会产生 `/data/init.sh.sha256`

**结论：** 包装器三条校验中 pin 这一条是死代码，实际只剩属主 + 权限两条。

**证据：** 全仓 grep `init.sh.sha256` 只有 `firmware-patch/scripts/patch_firmware.py:65,72,661`、`patch.sh:257` 与测试文件引用；没有任何客户端安装流程、脚本或文档写该文件；且 `/data/init.sh` 本身如何装到音箱上，在仓内也无文档（`firmware-patch/README.md` 只字未提安装流程）。包装器本身写得正确（`ls -ln` 取属主/权限串、第 6/9 位 `w` 拒写、符号链接因 `lrwxrwxrwx` 第 6 位为 `w` 被拒、pin 为空即拒），但 pin 的前置条件永远不成立。

**修复建议：** 二选一——(a) 让客户端安装流程写 pin 文件并把“写 pin”写进安装文档；(b) 把 pin 改为必填（缺 pin 文件即拒绝执行）。并补一份 `/data/init.sh` 安装流程文档。

## 5. P2 级新问题（44 项）

### 5.1 Hub 面（H-2 ~ H-13，12 项）

#### H-2 MPD `query()` 响应行无任何上限——P2-7 只堵了 HTTP 侧，留了 TCP 侧缺口

**结论：** 上游 HTTP 响应已全部 1MB 限长，但 MPD 行协议响应是无界读行，异常/卡住的 MPD 可把 Hub 内存打满。

**证据：** `hub/internal/music/music.go:379` `query()` 内 `for { line, err := c.r.ReadString('\n') ... }`，无行数上限、无单行长度上限、无总字节上限；`Status`/`Queue`/`CurrentSong` 全部经此读。对比同文件 Snapcast 侧 `readLimitedLine` 有 64KB 单行上限（`music.go` `maxSnapLineBytes`），KWS 侧有 `maxInboundBytes`。

**修复建议：** 给 `query()` 加与 Snapcast 同款上限（单行 64KB、总行数或总字节上限），超限即断开重连（看门狗已会重拨）。

#### H-3 `SongURL` 返回的 URL 未校验 scheme/host，MPD 侧存在 SSRF 面

**结论：** 上游（netease-api）返回的任意 URL 被原样交给 MPD `add` 播放，未校验是否为可信音频源。

**证据：** `hub/internal/music/music.go:281–305` `SongURL` 只判空即返回；`music.go:407` `PlayURL` 的 `validateMPDURL` 只拦 `\r/\n`/控制字符（防换行注入，P1-7 已做），不拦 `file://`、内网 `http://` 等 scheme。`router.go:819` 直接 `mpd.PlayURL(u)`。netease-api 是 LAN 内容器，一旦其返回被操纵（如上游被投毒或 DNS 劫持），MPD 会代为请求内网地址。

**修复建议：** `PlayURL`/`SongURL` 处加白名单：只许 `http(s)://` 且 host 在网易云 CDN 域名表内（或至少拒绝 `file://`、内网 IP、localhost）；非法 URL 按「无可播地址」走本地兜底。

#### H-4 `Status` 把「上游故障」与「登录过期」混成 502，过期时未必返回明确的需重扫

**结论：** 文档承诺「过期后 `/api/netease/status` 明确返回需重扫」，但上游对过期 cookie 若回非 200，端点会 502 而不是 `needs_rescan:true`。

**证据：** `hub/internal/music/netease_auth.go:239` `Status()`：`getJSON` 出错即 `return false, "", "", err`；`hub/internal/web/web.go:583` `neteaseStatus` 见 err 即回 502「查登录状态失败」。只有上游回 200 且 `account.id==0` 时才走 `logged_in:false` 分支。Binaryify 的 `/login/status` 对过期 cookie 的行为未在仓库内证实。

**修复建议：** `Status()` 里把上游 401/403 也映射为「未登录」（不返回 err），仅网络错误/5xx 才 502；并在 NAS 上用过期 cookie 实测一次确认。

#### H-5 `register_reject` 仍回显具体拒绝原因——P2-2 只统一了 hello，注册留了口子

**结论：** 与 P2-2 的统一原则不一致，注册失败会告诉对端是哪一项校验没过。

**证据：** `hub/internal/transport/transport.go:678–680` `reject` 把 `reason` 原样发回：`device_id 与连接不一致` / `model 与 hello 不一致` / `未配置指纹白名单` / `fingerprint 不在白名单内`。对比同文件 `:483–487` hello 拒绝已统一回 `"rejected"`，具体原因只记日志。

**修复建议：** 注册拒绝与 hello 看齐，统一回 `"rejected"`，四种原因只记服务端日志。

#### H-6 `ha.Client.Close()` 并发双关可 panic——关停路径与看门狗存在竞态窗口

**结论：** 关停与看门狗可能同时 `Close()` 同一个 HA 客户端，`close(c.done)` 双关即 panic。

**证据：** `hub/internal/ha/ha.go:127–135` `Close()` 用 `select { case <-c.done: default: close(c.done) }`——两个 goroutine 可同时走 `default` 分支双关 `done`。`hub/cmd/hub/main.go:724` 看门狗探活失败时 `_ = haClient.Close()`；`:1055` 关停先 `close(done)` 但**不等看门狗 goroutine 退出**，`:1065` 又 `_ = haClient.Close()`。窗口虽窄（需 HA 恰在关停瞬间探活失败），但 `close of closed channel` 是真 panic。

**修复建议：** `Close()` 内加 `sync.Once` 或互斥锁保护 `close(c.done)`；或关停时先等看门狗退出（`sync.WaitGroup`）再关客户端。

#### H-7 Web `/api/status` 读配置未走共享 `CfgLock`，与 PUT 的 `*s.cfg` 写存在数据竞争

**结论：** GET `/api/status` 与 PUT `/api/config` 并发时是对同一 `Config` 结构体的无锁读写，`-race` 可检出。

**证据：** `hub/internal/web/web.go:353` `status()` 在 `s.mu` 下读 `cfg.WakeWord`/`cfg.RoomNames()`；同文件 `configAPI` PUT 在 `cfgLock()`（共享锁）下执行 `*s.cfg = cfg`；`hub/cmd/hub/main.go:980` `webServer.CfgLock = cfgMu` 已接线。`status` 未用 `cfgLock()`，与 P1-22 的共享锁设计脱节。

**修复建议：** `status()`（及 `fingerprintAPI` 对 `HubFingerprint` 的读已在 `s.mu` 下、无写方，可不动）改走 `s.cfgLock()` 读配置；或把 `status` 的配置快照也纳入共享锁。

#### H-8 transport TLS 未设 `MinVersion`——G402 真阳性（低风险）

**结论：** wss 服务端接受 TLS 1.0/1.1（Go 默认），是 gosec G402 的真阳性；但在 mTLS + 局域网前提下实际风险低。

**证据：** `hub/internal/transport/transport.go:438` `tlsConf := &tls.Config{Certificates: ...}`，只设 `Certificates`/`ClientAuth`/`ClientCAs`，无 `MinVersion`。Go 1.23 默认服务端最低仍为 TLS 1.0。

**修复建议：** 加 `MinVersion: tls.VersionTLS12`（客户端是自家 Rust 代码，支持 TLS 1.2+，无兼容问题）。

#### H-9 并发 `CheckQR` 成功时，内存 cookie 与落盘文件可能不一致

**结论：** 两个不同二维码同时扫码成功并行保存时，文件写与内存写分两步、无统一锁，交错后文件与内存可能各执一个 cookie。

**证据：** `hub/internal/music/netease_auth.go:91` `save()`：先写临时文件+改名（无锁），再 `a.mu.Lock()` 更新内存 `cookie/savedAt`；`:212` `CheckQR` 的 803 分支调 `save()`，`CreateQR`/`CheckQR` 本身不持锁。交错顺序「A 写盘、B 写盘、B 写内存、A 写内存」→ 盘上是 B、内存是 A，重启前 VIP 取地址用的 cookie 与盘上不一致。

**修复建议：** `save()` 全程持 `a.mu`（文件写也在锁内），或把「写盘+更新内存」做成一个原子操作；并发扫码本就是边缘场景，锁内写盘的代价可忽略。

#### H-10 gosec 其余 8 项发现无任何记录可查——审计链断裂

**结论：** 「14 项发现」只在 `FIXES_P2.md:301` 列出 6 项生产代码项，剩余 8 项在仓库内无 gosec 报告、无清单、无裁决，无法验证「都在测试代码」的说法。

**证据：** 全仓 `grep gosec` 仅命中散文描述（`.golangci.yml` 未启用 gosec）；无 gosec 输出文件；本机 `gosec` 未安装，按任务要求未重跑、直接审代码。

**修复建议：** 补跑一次 `gosec ./hub/...`（或在 CI 加只报告不卡门的 gosec job），把 14 项完整清单与裁决落盘；在此之前「8 项在测试代码」只能算未证实的主张。

#### H-11 `/api/music/lyric?id=` 负数未校验，直通上游

**结论：** `id=-5` 会拼进 `/lyric?id=-5` 发给上游，靠上游报错兜底。

**证据：** `hub/internal/web/web.go:779` `musicLyric` 只 `strconv.ParseInt`，未判 `id < 0`；`hub/internal/router/router.go:971` `MusicLyric` 只把 `id==0` 解释为当前曲，负数直接进 `netease.Lyric`。

**修复建议：** `id < 0` 时回 400「id 非法」。

#### H-12 `/api/netease/login/qr` 无创建节流，可无限生成 unikey

**结论：** 持 Web Token 者可循环 POST 建二维码，上游 unikey 无 Hub 侧限流。

**证据：** `hub/internal/web/web.go:607` `neteaseLoginQR` 无任何频次限制；`requireAuth` 的限流只针对鉴权失败（`web.go` `maxAuthFailures=5`），鉴权成功后不限流。

**修复建议：** 对该端点加简单节流（如每 IP 每分钟 ≤10 次新建）或复用一次未过期的 key；低优先级。

#### H-13 P2-29：`ProbeModel` 仍是死代码，`Version: "probe"` 无生产调用方——结论：删

**结论：** 删。P1-44 已删自动扫描后，生产代码无任何调用方，只剩测试在调；留着它，`Version: "probe"` 会在将来某次误接线时覆盖真实版本号进注册表。

**证据：** 全仓 `grep ProbeModel(`：仅定义 `hub/internal/discovery/discovery.go:153` 与 `discovery_test.go` 的 6 处测试调用；`discovery.go:191` 写死 `"version": "probe"`。`FIXES_P2.md:321` §7 第 9 项亦确认「无生产调用方」。

**修复建议：** 删 `ProbeModel` 及其测试；如将来恢复自动发现，按真实 hello 的 `Version` 字段重写。

### 5.2 客户端面（C-3 ~ C-15，13 项）

#### C-3 AEC 回归测试可信度缺口：两项关键测试是空洞的，回声测试只覆盖甜点

**结论：** `pure_voice` 与 `zero_reference` 测试因全零参考走短路分支，根本没测到自适应路径；回声抑制测试只用了周期正弦 + 恰好 1320 采样延迟。

**证据：** `client/src/aec.rs:84–97`：队列不足或参考全零时 `process_frame` 直接 `return *mic`，不进 NLMS。`client/src/aec.rs:216–236`（`zero_reference_passes_mic_through_unchanged`）与 `aec.rs:262–279`（`pure_voice_without_echo_is_preserved`）：全程只喂零参考，`taps_max_abs() == 0` 与“RMS 误差 <5%”均由短路保证，**测不到“有非零参考时 NLMS 是否误伤语音”**——而这才是真实风险。`client/src/aec.rs:177–215`（`echo_suppression_synthetic_known_echo_plus_ref`）：参考为 300Hz 正弦（每帧恰 6 周期，帧间相位连续）、回声延迟恰为 1320 采样（= 4 帧 + 40 点，正落在抽头窗内）；无延迟失配用例、无非周期信号、无中途静音后恢复用例。注：逐帧验算过延迟队列逻辑——每帧弹出最旧帧恰为 4 帧前的参考，与文档一致；且零参考短路**不会**造成永久失同步（队列每帧把最新环形样本重锚定到回声源，扰动 <1 帧即自愈），所以这只是测试缺口，不是 C-1 之外的实现 bug。

**修复建议：** 补三例：非零参考 + 纯语音（验证 NLMS 不削弱语音）、回声延迟 ±1 帧失配（验证抑制率衰减可预期）、中途喂零参考帧后恢复（验证自愈）。

#### C-4 零能量守卫阈值 1e-9 对 i16 输入几乎永不触发，真正起作用的是步长钳制

**结论：** P2-22 的两道防线只有第二道（±1.0 钳制）在干活，第一道（1e-9）在 i16 数据上基本是死代码。

**证据：** `client/src/aec.rs:47–50`：`AEC_MIN_REF_NORM = 1e-9`，`AEC_MAX_STEP = 1.0`；`aec.rs:131–140`：`if norm >= AEC_MIN_REF_NORM` 才更新，步长钳制 ±1.0。算术：单采样 1 LSB 的平方范数 `(1/32768)² ≈ 9.3e-10 < 1e-9`，两个 1 LSB 采样即 `1.9e-9 > 阈值`——守卫只拦得住“全窗至多一个 1 LSB 采样”的情形；且全零帧早被 `aec.rs:93` 的短路拦下。实际防发散靠的是钳制（`step = (mu*e/(norm+1e-6)).clamp(±1.0)`）。

**修复建议：** 不改行为亦可，但注释应写实（“守卫只拦单采样毛刺，防发散靠钳制”）；若想让守卫真正生效，把阈值改为能量口径的有意义值并配测试。

#### C-5 `ref_tx` 满时静默丢帧，无计数、无日志

**结论：** 播放参考是尽力而为的信号，但丢参考恰好是扰动 AEC 对齐的事件，丢了却完全不可见。

**证据：** `client/src/main.rs:799`：`let _ = ref_tx.try_send(frame);`——`Full` 与 `Closed` 均被忽略；通道容量 16（main.rs:511）。对比同文件的 `audio_tx` 满时有计数 + 5 秒一条节流日志（P2-N5，main.rs:572 起）。

**修复建议：** 参照 P2-N5 加丢帧计数 + 节流日志。

#### C-6 P2-52 的 ALSA 标记断言缺失于默认交付脚本（gnueabihf）

**结论：** musl 脚本有双重保护（缺 libasound 即 ASSERT FAIL + 产物标记断言），默认交付的 gnueabihf 脚本只有前者，没有后者。

**证据：** `client/build-armv7.sh:63–65`（缺 musl libasound 即 exit 1）与 `build-armv7.sh:2b` 节（产物必须含 `open-xiaoai-audio-backend=alsa` 标记，否则 ASSERT FAIL）。`client/build-armv7-gnueabihf.sh`：全篇无 `open-xiaoai-audio-backend=alsa` 检查；它只断言 sherpa 标记、GLIBC 版本、rpath、NEEDED 库。该脚本把其余参数原样透传给 cargo（`CARGO_ARGS`），即 `./build-armv7-gnueabihf.sh --no-default-features` 会静默产出无 ALSA 的默认交付二进制且脚本不拦。

**修复建议：** 把 musl 脚本 2b 节的标记断言原样补进 gnueabihf 脚本（有 `--no-default-features` 透传时跳过并 WARNING，与 musl 脚本的 `--allow-no-alsa` 语义对齐）。

#### C-7 `rust-toolchain.toml` 无 targets 列表，注释称“自动下载 ARM target”不成立

**结论：** 工具链版本本身钉对了（1.100.0，ci.yml:75 同步安装 + rustfmt/clippy），但自动装 target 的说法是错的。

**证据：** `client/rust-toolchain.toml:16`：只有 `[toolchain] channel = "1.100.0"`，无 `targets = [...]`；其注释（第 9 行）称“首次构建会自动下载 1.100.0 + 两个 ARM target”——rustup 只自动装文件里列出的 target，未列出的不装。两构建脚本均无 `rustup target add`，只有 `client/README.md:164,202` 写了手动 `rustup target add armv7-...`。新机器上首次构建会报 target 未安装，报错不直指原因。其余引用点一致：ci.yml:68–76、两构建脚本注释、FIXES_P2.md:91 均写 1.100.0，无版本漂移。

**修复建议：** 在 `rust-toolchain.toml` 加 `targets = ["armv7-unknown-linux-gnueabihf", "armv7-unknown-linux-musleabihf"]`（一处改动即覆盖两脚本），或至少修正注释。

#### C-8 生产代码仍有一处 `.expect`，与 P2-54 声称的“不再有任何 .expect panic 路径”不符

**结论：** 编解码自由函数本身已干净，但 sherpa 门控的生产路径里还有一处 `.expect`。

**证据：** `client/src/audio.rs` 的 `encode_pcm_frame` / `decode_opus_frame`：`Result<OpusCodec>` 线程本地 + `try_borrow_mut`，失败返回 Err，确认无 expect/unwrap——P2-54 就这两个函数而言属实。`client/src/kws.rs:311`（`#[cfg(feature = "sherpa-kws")]` 的 `feed` 内）：`frame.as_slice().try_into().expect("frame is 320 samples")`。逻辑上不可达（frame 恰由 `drain(..FRAME_SAMPLES)` 切出），风险极低，但 FIXES_P2.md:85 的表述是绝对化的。

**修复建议：** 改为 `debug_assert!` + `Err` 返回（或 `match`），并收窄 FIXES_P2.md:85 的表述为“编解码函数无 expect 路径”。

#### C-9 播放路径每帧一条 `info!` 日志（50 条/秒）

**结论：** 每次播出一帧都打 info 日志，长期运行会刷屏。

**证据：** `client/src/main.rs:801`：`info!("played {}-sample frame via ALSA", pcm.len());`，位于 `RawFrame::Audio` 分支，每 20ms 一帧即 50 条/秒。对比同文件的播放失败日志有 5 秒节流（main.rs 内 `playback_err_count` / `last_playback_err_log`）。

**修复建议：** 降为 `debug!` 或节流。

#### C-10 无客户端 WS Ping、无发送超时；半死连接可 wedged 整个会话与录音任务

**结论：** Hub 只停读不发 FIN/RST 时，`run_session` 的 `send_*` 会无限挂起，录音任务还会因阻塞的 wake 发送一并停摆。

**证据：** `client/src/transport.rs`：客户端从不发 Ping（只回 Pong，`next_raw` 内）；`run_session`（main.rs:735 起）的 `send_audio_frame` / `send_status` / `send_heartbeat` / `send_wake` 均无超时；`TcpStream` 未设 keepalive。传导链：`send_*` 挂起 → `run_session` 的 `select!` 停住 → `audio_rx`（容量 32）满后录音任务丢帧尚可自保，但 `wake_tx.send(ev).await`（main.rs 录音任务内，阻塞式，与 P2-N5 的 `try_send` 不对称）会把录音任务整个堵住——采集、AEC、编码全停。正常断线（FIN/RST）不受影响，已有重连测试覆盖（transport.rs:1264 起）。

**修复建议：** 发送加超时（如 `tokio::time::timeout`，超时按断线重连）；或客户端每 30s 随 heartbeat 发 WS Ping；`wake_tx` 改 `try_send` 与音频通道一致。

#### C-11 P2-19 录音任务侧跳编码、P2-N4 重建编解码器接线无测试

**结论：** 修复代码都在，但能证明它们的测试只覆盖了节流器单体与会话层，录音任务本体不可测。

**证据：** P2-19：`if !pushing_capture.load(...) { continue; }`（main.rs:566）在录音任务内跳过编码；e2e 测试 `local_mode_start_recording_pushes_audio_frames`（main.rs:1936）只验证了 `run_session` 层面的帧门控，未验证“未推流时不编码”。P2-N4：`encode_throttle.should_recover()` → `rebuild_codec()`（main.rs 录音任务内）；单测只有 `FailureThrottle` 本体（audio.rs:868 起）与 `rebuild_codec_keeps_audio_usable`，两者之间的接线无测试。根因：录音任务是 `main()` 内 spawn 的闭包，无注入点。

**修复建议：** 接受现状或将录音任务抽为可测函数（注入 Fake Audio + 通道），后者是结构性改动，留待 P2 收尾时评估。

#### C-12 TestArbiterWindow flake：测试侧时序假设过紧（Hub 侧 Go 测试）

**结论：** 50ms 睡眠对 100ms 窗口只剩 50ms 裕量，CI 负载下睡眠超调即漏窗、断言赢家错误；生产仲裁逻辑本身无问题。

**证据：** `hub/internal/arbitration/arbitration_test.go:34–54`：`New(100ms)` 后 `Submit` 事件 1，`time.Sleep(50ms)`，再 `Submit` 事件 2，断言赢家为事件 2。`hub/internal/arbitration/arbitration.go` 的 `Submit`：首条事件即 `time.AfterFunc(window, close)`；`time.Sleep` 只保证不少于 50ms、不保证不多于——负载下调度延迟 >50ms 时窗口已在事件 2 到达前关闭，第一次 `close` 即以仅事件 1 裁决，测试在赢家断言处失败。这与 FIXES_P2.md:283 记录的“1/5 FAIL”及 TEST_REPORT.md:194“4 次以上重跑未复现”一致（调度相关、偶发）。定级：测试侧，P2；生产侧 `Arbiter` 的窗口/去重/冷却逻辑读过，无对应缺陷。

**修复建议（测试侧）：** 去掉 `Sleep(50ms)`（两事件背靠背提交仍在同一窗口内，测试意图不变，flake 根除）；或把窗口放大到 500ms、睡眠保持 100ms（5 倍裕量）；彻底做法是给 `Arbiter` 注入可 mock 的时钟。

#### C-13 文档/注释过期一捆 + `Audio::open()` 仍可公开双开

**结论：** 三处文字与代码不符；防 P0-N1 回归只靠注释，结构上无强制。

**证据：** `client/src/transport.rs:1–8` 模块头仍写“断线后指数退避重连：首 100ms，上限 1s，30s 内恢复”——实际 `BACKOFF_MAX_MS = 30_000`（transport.rs:60），上限是 30s；同文件头又写“未配置指纹时跳过校验并打警告”，与 P1-10 的指纹必填（config.rs validate）矛盾（回退分支仍在 `build_tls_config`，仅当 validate 被绕过时才可达）。`client/src/transport.rs` 的 `connect_with_retry` 注释称鉴权失败“只重试 3 次”/“3 times in a row”，实际 `auth_failures` 是累计计数、非连续失败不归零（transport.rs 内 `connect_with_retry`）。`client/src/audio.rs` 的 `Audio::open()` / `open_devices()` 仍为 pub 且双开两个方向，防回归只靠文档“禁止生产路径用本函数”与 audio.rs 的构造器行为测试；main.rs 目前用的是对的，但一次误改即可重引入 P0-N1。

**修复建议：** 顺手改注释；`Audio::open()` 改为 `#[cfg(test)]`（生产与测试目前都只在测试里用它）。

#### C-14 `FingerprintVerifier` 不校验证书有效期与 SAN（已知延期项，备注）

**结论：** 指纹匹配即放行，不过期、不验 SAN；过期证书只要指纹对就仍被接受。

**证据：** `client/src/transport.rs` 的 `verify_server_cert` 忽略 `_server_name` 与 `_now`，只比对 `fingerprint_matches`；签名本身经 `verify_tls12/13_signature` 用 webpki 验过。config.rs 注释已言明该项属 P2-1 范围、不在本轮。

**修复建议：** 记为已知延期（P2-1），下次动 TLS 验证时一并补有效期 + SAN 校验；不新开工作。

#### C-15 P2-42 残留：`hub_url` 默认仍是写死的 `wss://192.168.1.10:4399`（待定，需 bai 拍板）

**结论：** 缺失文件已明确报错（P2-42 前半属实），但字段级默认仍在，旧 client.toml 省略 `hub_url` 即静默连向写死地址。

**证据：** `client/src/config.rs:12–14`：`default_hub_url()` 仍为 `wss://192.168.1.10:4399`；`config.rs` 的 `load_from_path` 缺失文件时明确报错（P2-42 前半已落实，测试 `missing_file_is_clear_error_not_silent_default` 在 config.rs 内）。风险已被部分兜住：Token 默认已空、指纹必填，错配的默认 Hub 无法鉴权（FIXES_P2.md:296 同款表述）。

**状态：** 非新发现，FIXES_P2.md:83 已如实标注残留。彻底去掉需 `hub_url` 必填（breaking change），**等 bai 拍板**，不擅自改。

### 5.3 固件/部署面（F-4 ~ F-11，8 项）

#### F-4 root hash“沿用旧镜像”的说法无法核实，且移植步骤未进脚本

**结论：** 文档宣称的 hash 复用无任何可验证锚点，重编时靠手工操作。

**证据：** `TEST_REPORT.md:597-598` 称重编用一次性随机密码走 `SSH_PASSWORD_FILE`，随后“把两份新镜像的 shadow root 行换回旧镜像的 `$6$` hash（逐字节比对验证一致）”；但 `patch.sh`/`patch_firmware.py` 内无任何移植 hash 的代码路径（`patch_firmware.py:sha512_crypt` 每次都用 `secrets` 新随机盐，见 `:284-296`），且旧镜像已不在。只观察到两份新镜像的盐互不相同（各自独立生成或各自沿用各自旧 hash 均可解释，无法区分）。

**修复建议：** 把移植步骤脚本化（或至少把所用命令记录进 runbook）；首刷后用串口 + bai 已有密码实测登录一次（真机项）。

#### F-5 `PATCH_NO_CERTS` 任意非空值都跳过证书，包括 `PATCH_NO_CERTS=0`

**证据：** `patch.sh:80` 用 `[ -n "${PATCH_NO_CERTS:-}" ]` 判断。

**修复建议：** 只认 `=1`（`[ "${PATCH_NO_CERTS:-}" = "1" ]`）。

#### F-6 P2-39 inode 硬门 fail-open——`unsquashfs -s` 读不到 inode 数时只警告跳过

**证据：** `patch.sh:230-234` 与 `unpack.sh` 同款（`EXPECT_INODES` 为空时 `echo 警告…跳过数量硬门`）。机制本身已实测通过（LX06 2144/2144、OH2P 2105/2105，`find` 计数与 `unsquashfs -s` 一致）。

**修复建议：** 读不到即 FAIL（或至少 `--strict` 下 FAIL）。

#### F-7 `firmware-patch/README.md:47` 仍写旧 rc.local 直行，与镜像实际不符

**证据：** 该行写 `etc/rc.local 增加 [ -f "/data/init.sh" ] && sh /data/init.sh …`，实际镜像是包装器行 `[ -x /etc/open-xiaoai/init.sh ] && /etc/open-xiaoai/init.sh …`。

**修复建议：** 同步为包装器行。

#### F-8 两台音箱共用同一份 mTLS 客户端证书/私钥

**结论：** 家用可接受，但吊销无隔离。

**证据：** 两镜像内 `ca.crt`/`client.crt`/`client.key` 的 SHA256 完全相同。

**修复建议：** 如需按设备吊销，`gen-certs.sh` 改为按设备签发独立 client 证书；至少在文档里写明共用身份的含义。

#### F-9 原厂 `boot.img` ramdisk 内 `/init` 仍有免密兜底 shell（`exec sh`），补丁范围外

**证据：** 解压 LX06 `boot.img` ramdisk（2,802,688 字节）含 `"Failed to switch root, dropping to a shell"` + `exec sh`；squashfs 内 `/init` 是同一脚本的副本。仅在 `switch_root` 失败的启动失败路径可达，且 boot.img 不在本次补丁范围内。

**修复建议：** 暂接受并记录；要修需改 boot.img（签名/刷机流程另议）。

#### F-10 每日 03:00 cron 仍跑 `/bin/ota slient` 向小米查升级

**结论：** 仅查询、不致刷机，但每日联网行为仍在。

**证据：** 两镜像 `/etc/crontabs/root` 有 `* 3 * * * /bin/ota slient # check ota`；`bin/ota` 的 `check()` 只调 `matool_check_upgrade`（只读查询），`upgrade()` 头 `return 1` 已死。

**修复建议：** 接受现状，或在 `patch_firmware.py` 里删该 cron 行并记录。

#### F-11 `FIXES_P2.md:130` 的 P2-39 实测数已过期（历史记录，非 live 断言）

**证据：** 该行写“补丁镜像 2141/2141”，现 LX06 实测 2144 inodes（`unsquashfs -s` 与 `find` 均为 2144）。

**修复建议：** 注上“2026-11-17 旧重编值”或更新数字。

### 5.4 边车面（S-4 ~ S-14，11 项）

#### S-4 无单帧大小校验：协议定 640B，实际放行到 1MB 切片喂入

**结论：** 二进制帧只拦奇数长度（`server.py` 音频循环），1MB 以内的超长帧会被切成 1562 个 20ms 块连续喂入，`is_ready` 恒真时最坏 1562×32 次 decode 会堵住单事件循环、全设备停摆。

**证据：** `server.py:571` `max_size=1 << 20`；音频循环 `for i in range(0, len(pcm), FRAME_SAMPLES)` 无长度上限；`DeviceStream.feed`（`server.py:187-193`）每块最多 32 次 decode。

**修复建议：** 二进制帧长度非 640 字节即记 error + `continue`（或按 640 整数倍放行并记 warn），把 `max_size` 收紧到如 64KB。

#### S-5 连续错误计数是全局的，且 `_fatal_error` 粘住永不恢复

**结论：** 一台设备的解码异常会计入全服务计数，5 次即 `/health` 503 且必须重启恢复，另一台正常设备也被连坐。

**证据：** `server.py:267-274` `note_feed_error()` 全局 `self._consecutive_stream_errors`，达 5 置 `_fatal_error`；`health_reason()`（`server.py:260-261`）一旦置起即 503，无任何清零路径；`note_feed_success()` 只清计数不清 `_fatal_error`。

**修复建议：** 计数按 `device_id` 分桶；或 `_fatal_error` 在连续 N 次成功后自动清除（带日志），避免一次瞬时故障永久拉下健康检查。

#### S-6 单帧喂入异常直接掐断整条连接，而不是像 SC-P2-1 那样复位流

**结论：** `feed()` 抛任何异常都会 `note_feed_error()` 后 `raise`，整条设备连接被撕掉、Hub 重连并排空队列；与 SC-P2-1“超限复位自愈”的哲学不一致。

**证据：** `server.py:423-427` 音频循环内 `except Exception: self.note_feed_error(); raise`。

**修复建议：** 捕获后记 error、`reset_stream` 该设备流并 `continue`，仅连续多次失败才断连接（与 S-5 的分桶计数联动）。

#### S-7 Dockerfile 基础镜像未钉 digest，违反项目自己的 P1-4 标准

**结论：** `python:3.12-slim` 既无 digest 也无 patch 版本，compose 里其余镜像全部钉了 digest，只有边车是例外；且 `apt-get install curl bzip2` 未钉版本、镜像内无 `USER`。

**证据：** `kws-sidecar/Dockerfile:5` `FROM python:3.12-slim`；`deploy/docker-compose.yml` 注释称 P1-4 已钉 digest（whisper/piper/netease-api/snapserver/mpd 均有 `@sha256:`，边车是 `build:` 无 digest）；`requirements.txt` 全部 `==` 钉住（sherpa-onnx==2.0.1、websockets==17.4、numpy==2.5.5 等），反衬出基础镜像是唯一漂移点。

**修复建议：** 按 P1-4 同样做法钉 `python:3.12-slim@sha256:<...>`，并记录更新流程；apt 包能钉则钉；镜像内加 `USER 1000:1000`（与 compose 一致，独立 `docker run` 也不跑 root）。

#### S-8 `KWS_HOST` 默认 `0.0.0.0`，脱离 compose 独立跑即暴露到局域网

**结论：** 安全默认值应是回环，当前默认是全接口；compose 靠环境变量覆盖才安全。

**证据：** `kws-sidecar/Dockerfile` ENV `KWS_HOST=0.0.0.0`；`server.py:497` `--host` 默认同样 `0.0.0.0`；只有 `deploy/docker-compose.yml` 显式设 `KWS_HOST=127.0.0.1` 兜底。

**修复建议：** 两处默认值都改为 `127.0.0.1`，需局域网暴露时再显式改。

#### S-9 SC-P2-8 有缺口：部分缺失时，已存在语音文件不验 SHA

**结论：** 已存在文件验 SHA 只在“两个语音文件都已存在”时走，缺一个文件时现存那个直接跳过校验用到底。

**证据：** `kws-sidecar/tests/bootstrap.py:_ensure_voice()`——`if all(...)` 为真才验 SHA、坏了删掉重下；否则直接进下载循环且只下载 `not dest.is_file()` 的文件。`_download()` 本身下载后验 SHA 是对的。

**修复建议：** 下载循环前先对已存在的文件逐个验 SHA（复用 `_voice_sha_ok`），坏的删掉重下，与全存在路径一致。

#### S-10 flush 检出的唤醒不计入 `total_wakes`，断开设备的计数随字典消失

**结论：** 断开时补喂检出的唤醒只记日志，`/health` 的 `total_wakes` 会漏计；`device_wakes` 只含当前连接设备，断开即清零。

**证据：** `server.py:475-483` finally 块调 `flush_on_disconnect()`，检出只记 `wake lost on disconnect`；`flush_on_disconnect`（`server.py:204-223`）内 `feed()` 的 `wake_count += 1` 发生在流上，但 `self.total_wakes += 1` 只在正常 wake 发送处（约 `server.py:445`）；`process_request` 的 `device_wakes`（`server.py:289-291`）直接取 `self.devices` 字典。

**修复建议：** flush 检出时 `total_wakes` 也 +1（或单独记 `lost_wakes`）；`device_wakes` 改为保留最近断开设备的累计值（或注明“仅当前连接”）。

#### S-11 被取代的旧连接关闭超时后，旧 handler 可能滞留并占着连接数名额

**结论：** `replaced` 标志能防幽灵唤醒，但旧连接 `close()` 5 秒超时后旧 handler 仍挂在 `async for` 里，`self.connections` 计数不释放，长期可挤占 16 个名额。

**证据：** `server.py` 新连接取代旧连接处 `await asyncio.wait_for(old_ws.close(...), timeout=5)`，超时只记日志继续；旧 handler 的 `finally` 才 `self.connections -= 1`，而旧 handler 要等连接真正出错/关闭才退出。

**修复建议：** 取代时直接 `await old_ws.close()` 后再无条件清理，或给旧 handler 加显式取消/超时关闭；连接数统计里区分半开连接与已认证连接。

#### S-12 `KWS_SCORE` 无上界，`num_threads`/端口等参数无校验

**结论：** 阈值只拦了下界，`score` 无上界（过大可能抬高误触发倾向）；非法环境变量在解析默认值时直接抛 traceback 而非干净 FATAL。

**证据：** `server.py:amain` 校验 `0 < threshold <= 1`、`score >= 0`，无上界；`build_arg_parser`（`server.py:497-515`）里 `float(os.environ.get(...))` 在非法值时抛 `ValueError` traceback；`--num-threads`/`--port` 无范围校验（极端值依赖 sherpa/`serve()` 自身报错）。

**修复建议：** `score` 加合理上界（如 ≤ 10，超出 FATAL 并注明）；环境变量解析失败时捕获并打印 FATAL 后 `sys.exit(1)`；`num_threads` 限 1~8、`port` 限 1~65535。

#### S-13 `wake_no` / `frames_since_wake` 只有日志有，线上的 wake 事件没有

**结论：** 这两个字段未进 wire 事件，Hub 无法按事件观测检出延迟；任务简报里把它们当事件字段与实现不符，需澄清而非默认已实现。

**证据：** 边车 wake 事件（`server.py:436-445`）只有 `type/device_id/keyword/rms/rms_peak/timestamp_ms`；`wake_no`/`frames_since_wake` 只在 `LOG.info`（`server.py:450-454`）；协议文档（`server.py:22-30`、`README.md`）同样只定义了 7 字段事件。

**修复建议：** 若 Hub 需要这两个值做排障/监控，把 `wake_no` 与 `frames_since_wake` 加进事件（Hub 侧容错解析即可）；若不需要，在协议文档里明确写“仅日志，不上事件”，避免后人误以为 Hub 能拿到。

#### S-14 指纹归一化两处不一致：`config.go` 不认 `-`，`kws.go` 认

**结论：** 带 `-` 分隔的指纹会被 config 启动校验拒绝，却是 `kws.New` 能接受的格式；另 config 报错文案说“小写 hex”但实际先 `ToLower`，大写也能过。

**证据：** `hub/internal/config/config.go:277` 只 strip `:` 和空格；`hub/internal/kws/kws.go:normalizeFingerprintHex` 还 strip `-`。

**修复建议：** 两处共用同一个归一化函数；报错文案改为“64 位 hex（大小写不限）”。

## 6. P2 轮待定项逐项裁决

### 6.1 gosec 14 项发现

6 项生产代码逐项裁决如下（本机 gosec 未安装，按任务要求未重跑，直接审代码）：

| # | 发现 | 裁决 | 证据 |
|---|---|---|---|
| 1 | G115 `wyoming.go:325` `int16(Uint16)` | **假阳性** | PCM 字节流转采样值，位重解释是有意的，值域完整保留 |
| 2 | G115 `wyoming.go:334` `uint16(int16)` | **假阳性** | 同上，逆向重解释，编码/解码对称 |
| 3 | G115 `kws.go:418` `uint16(int16)` | **假阳性** | Opus 解码出的 int16 采样转小端字节，位操作正确 |
| 4 | G402 `transport.go` TLS MinVersion 未设 | **真阳性（低风险）** | 见 H-8；LAN + mTLS，修法一行 |
| 5 | G402 `kws.go:273` `InsecureSkipVerify: true` | **假阳性** | 指纹 pin 本身就是校验：`VerifyPeerCertificate` 验 SHA-256 指纹 + 证书有效期（`kws.go:261–293`），wss 无指纹时回落系统根证书 |
| 6 | G404 `kws.go:627` `math/rand` jitter | **假阳性** | 仅用于重连退避 ±25% 抖动（`nextBackoff`），非安全用途；`go.mod: go 1.23`，顶层 `math/rand` 已自动播种 |

其余 8 项：仓库内无任何记录（见 H-10），**无法裁决**。另：`.golangci.yml` 启用的 linter 仍是 `govet/staticcheck/gofmt/goimports/ineffassign/unused/errcheck`，gosec 未启用——与 `FIXES_P2.md` §7 第 8 项一致。**结论：gosec 不启用（维持 P2 轮决定），但应补跑一次完整扫描把 14 项清单落盘，否则“8 项在测试代码”永远只是未证实的主张。**

### 6.2 P2-29 ProbeModel 死代码——结论：删（见 H-13）

`Version: "probe"` 无生产调用方，只剩测试在调。删 `ProbeModel` 及其测试；如将来恢复自动发现，按真实 hello 的 `Version` 字段重写。

### 6.3 alerts.Watcher 无 Stop——核实无泄漏，现状可接受

`main.go:1025` `Start()` 只调一次；看门狗重连后 `Rebind` 到新客户端（`:717`），旧订阅随旧连接关闭而失效；关停时 `main.go:1065` `haClient.Close()` 断开连接，`ha.go:133` `readLoop` 随 `ReadMessage` 出错退出，goroutine 不泄漏。无 `Stop` 在当前接线下可接受；唯一附带问题是 H-6 的双关竞态。

### 6.4 P2-10 / P2-21 本地 KWS 残留——待 P1-40 路由确认后定（只记录，不替 bai 决定）

Hub 侧无本地 KWS 代码（`hub/internal/kws` 全部是 server 侧边车客户端）。残留全部在客户端（Rust）：`client/src/main.rs:422–466` `init_kws` 仍按 `kws_mode` 分流（默认 `server`，`local` 分支保留）；`client/src/kws.rs`、`sherpa.rs`（feature `sherpa-kws`）原样保留；P2-10 的 `.so` 任意路径 dlopen、无白名单/SHA 校验，以及 32-bit 结构体尺寸（68/104/28）未在 ARM 上实测确认；P2-21 的 sherpa `is_ready` 无上限循环仍开。server 模式下生产不走本地 KWS。

### 6.5 TestArbiterWindow timing flake——测试侧，见 C-12

生产仲裁逻辑无问题，测试的 50ms 睡眠对 100ms 窗口裕量不足。修复建议在测试侧（去掉 Sleep 或放大窗口）。

### 6.6 read_only 暂缓——已定，只记录

现状不变，容器文件系统仍可写（P1-4 决定），审计不重定。Hub 的 `Config.Save` 用 `os.CreateTemp` 写 `/app`，做成需要 /app 的 tmpfs 编排且无法在无 NAS 时验证。

### 6.7 root 密码 hash 复用——核实限度（见 F-4）

旧镜像已不在磁盘上，无任何旧 hash 记录可供对照，复用说法无法核实。两份新镜像的盐互不相同（各自独立生成或各自沿用各自旧 hash 均可解释，无法区分）。首刷后用串口 + bai 已有密码实测登录一次（真机项）。

### 6.8 P2-35 OTA 禁用残留路径——BLOCKED（真机）

三处禁用点均在镜像内验证；`boardupgrade.sh` 只被 `flash.sh` source，无其他调用方；`upgrade()` 内 `flash.sh` 调用在 `return 1` 之后不可达；`wireless` 的 `ota()` 经 `EXTRA_COMMANDS` 对外暴露但头行即 `exit 3`。残留真机核实三项：(a) `flash.sh` 主流程顶层的 `return 1` 在设备 busybox ash 下是否真退出脚本（若不退出会继续往下走 `upgrade_verify_image`/`board_system_upgrade`——原厂代码通篇用同一写法，大概率没问题，但 VM 内无法证明）；(b) 刷后 cron 的 `slient` 检查是否真不下载/不触发；(c) 串口密码登录 + `/dev/tts/0` 是否存在（与 F-2 同源）。

## 7. P2 轮修复抽查结果

### 7.1 Hub 面：10 项 headline 全部核实通过

| 项 | 结论 | 证据 |
|---|---|---|
| P2-1 证书复用有效期校验 | ✅ 在代码里且有效 | `tlscert.go:47` `renewBefore=30d`；`:111–118` `certNeedsRenew` 覆盖已过期/30 天内过期/未生效；`:158` 备份后重签；证书/私钥/CA 均为 0600 |
| P2-2 hello 拒绝原因统一 | ✅（仅 hello） | `transport.go:483–487` 统一回 `"rejected"`，原因只记日志；但注册拒绝未统一，见 H-5 |
| P2-4 权限收紧 0600/0700 | ✅ | `main.go:463–471` 数据目录 0700、4 个文件启动时收紧 0600；`config.go:560` `Save` 临时文件 0600；`reminders.go:153` 落盘 0600；`netease_auth.go:91` cookie 目录 0700、文件 0600 |
| P2-7 上游 1MB 限长 | ✅（HTTP 侧） | `agent.go:108,113` 错误体与成功体均 `LimitReader` 1MB；`netease_auth.go` `getJSON` 1MB；Web `MaxBytesReader` 1MB；但 MPD 侧无上限，见 H-2 |
| P2-13 Start 守卫 | ✅ | `reminders.go:259–290` 重复 `Start` no-op、`Stop` 可重复调、可再 `Start`；`main.go:1056` 关停补 `remStore.Stop()` |
| P2-44 tick 落盘 | ✅ | `reminders.go:332–339` 只在 `changed \|\| dirty` 时写盘，失败置 dirty 下次重试；`:104` 损坏文件改名备份 `.corrupt.<时间戳>` |
| P2-46 ChatStream 4MB 分段读 | ✅ | `agent.go:57–60` `maxSSELineBytes = 4<<20`；`:190–207` `readSSELine` 用 `ReadLine` 分段拼接、超限报错 |
| P2-49 场景实体校验 | ✅ | `router.go:673–676` `allowedVoiceDomain` 白名单；`:714–728` 执行前调 `ha.go:259` `GetSceneEntityIDs` 逐个校验，查不到场景内容时 fail-closed 拒绝 |
| N-P2-3 重连排空移到 hello 之前 | ✅ | `kws.go:660–680` `everConnected` 时先 `drainQueue()` 计丢帧记日志，再发 hello；注释写明顺序原因 |
| N-P2-5 HealthInterval 10s | ✅ | `kws.go:80–82,122–123` 默认 10s；`:298–312` `Start` 立即查一次；连接成功/断开即触发一次健康检查 |

### 7.2 客户端面：11 项属实，2 项有缺口

| 项 | 结论 | 证据 |
|---|---|---|
| P2-19 只在 pushing 时编码 | ✅ 属实 | main.rs:566（录音任务跳编码）+ run_session 每次 Hub 消息后同步标志 |
| P2-20 KWS 失败 50 次停后端 | ✅ 属实 | kws.rs:180（`KWS_FEED_DISABLE_AFTER = 50`）、feed 内节流 + 停后端；但见 C-2：CI 不编译此路径 |
| P2-21 is_ready 排空改 decode_ready_loop | ✅ 属实 | sherpa.rs:529–548（上限 100，超限复位流并 Err）、单测 sherpa.rs:724–751；server 模式不走此路径 |
| P2-22 NLMS 守卫 1e-9 / ±1.0 | ✅ 字面属实，见 C-4 | aec.rs:47–52、131–140；`mu = 0.5`、256 抽头 = 16ms 与 Hub 侧同口径 |
| P2-40 ALSA 失败退避重开 | ✅ 属实 | main.rs:96（100ms 起步翻倍、上限 5s）、录音任务重开循环、单测 main.rs:2094 |
| P2-42 缺文件明确报错 | ✅ 部分（残留已知，见 C-15） | config.rs `load_from_path` |
| P2-52 musl libasound 缺失 ASSERT FAIL | ⚠️ 仅 musl 脚本属实，见 C-6 | build-armv7.sh:63–65 + 标记断言；gnueabihf 脚本无标记断言 |
| P2-54 编解码函数返回 Result 无 expect | ⚠️ 函数本身属实，见 C-8 | audio.rs 自由函数干净；kws.rs:311 仍有 `.expect` |
| P2-N1 interrupt 真调 stop_playback | ✅ 属实 | main.rs `apply_action` StopPlayback 分支 + e2e 测试 main.rs:2124 |
| P2-N2 入站 WS 帧 64KB | ✅ 属实 | transport.rs:41（`MAX_WS_MESSAGE_BYTES = 64*1024`）、超限断线测试 transport.rs:1157 起 |
| P2-N4 编码失败节流 + 50 次重建 | ✅ 代码属实，接线无测试（见 C-11） | main.rs 录音任务内节流 + `rebuild_codec()` |
| P2-N5 audio_tx 改 try_send | ✅ 属实 | main.rs:572，满时丢帧计数 + 5 秒节流日志 |
| P2-N6 hello_ok/rpc 解析严格化 | ✅ 属实 | transport.rs `parse_hub_text` 与 `connect_once` 握手路径双份校验 |
| P2-N7 工具链钉 1.100.0 + CI 同步 | ✅ 基本属实，见 C-7 | rust-toolchain.toml:16、ci.yml:75；缺 targets 列表 |

**AEC 专项核实：** NLMS 实现正确（`step = mu·e/(norm+1e-6)` 钳制 ±1.0，抽头 256 @16kHz = 16ms 自洽）；延迟队列逻辑正确（每帧弹最旧帧 = 恰 4 帧前，80ms）；`ref_tx` 容量 16、满时 `try_send` 丢帧不阻塞，Aec 由录音任务独占、无 Mutex、无死锁/数据竞争；接线正确——每采一帧先 `process_frame` 再喂 KWS 与 Opus 编码（main.rs 录音任务内），零参考直通，server/local 双模式均生效，唯一播放入口 `RawFrame::Audio → play_frame` 均喂 `ref_tx`（main.rs:799）。回声测试的判定口径与 Hub 侧一致，但覆盖面见 C-3。

**unsafe 审计（sherpa.rs 为唯一含 unsafe 的客户端文件）：** FFI 结构体尺寸 64-bit 有 C 探针锚定测试，32-bit 手算值（OnlineModelConfig 68 / KeywordSpotterConfig 104 / KeywordResult 56）重算无误；`Symbol` 转 `'static` 靠 `_lib` 字段最后 drop 的顺序保证；`create_spotter` / `create_stream` 均判空，失败时已建 spotter 会销毁；`get_result` 判空且 `destroy_result` 恒调；`Drop` 先毁 stream 再毁 spotter；`unsafe impl Send` 依赖 `&mut` 串行调用，与单录音任务独占一致。**unsafe 未发现新问题。**

### 7.3 边车面：SC-P2-1~13 逐项复核

| 项 | 结论 | 证据 |
|---|---|---|
| SC-P2-1 单帧 decode 上限 32 + 超限复位 | ✅ 真修 | `server.py:69,187-193`；测试 `test_p2_1` |
| SC-P2-2 wake 事件 `rms_peak` | ⚠️ 边车真修、Hub 侧丢弃（见 S-1） | `server.py:230-236,441`；测试 `test_p2_2`（pos1 峰值 0.311/均值 0.137 标定合理） |
| SC-P2-3 hello 超时单独捕获、error + 1008 | ✅ 真修 | `server.py:328-337`（`wait_for(..., timeout=10)`，`except asyncio.TimeoutError` 单独分支）；测试 `test_p2_3` |
| SC-P2-4 断开补 25 帧静音最后检出 | ✅ 真修 | `server.py:72,204-223` + finally `server.py:475-483`（被取代的流不 flush，条件正确）；测试 `test_p2_4` |
| SC-P2-5 device_id 1–64 字符含 `.` | ✅ 自洽、无注入风险 | `server.py:76` 正则 + `server.py:363` 校验；仅作字典键/日志/回显，无路径或 shell 拼接；含点是测试钉住的既有协议，与审计报告建议的 `[A-Za-z0-9_-]` 出入属有意的协议兼容 |
| SC-P2-6 阈值越界 FATAL | ✅ 真修 | `server.py:amain` 阈值 `(0,1]`、分数 `>=0` 越界 `sys.exit(1)`；测试 `test_p2_6`（上界缺口见 S-12） |
| SC-P2-7 /health `device_wakes` + wake 日志 `wake_no`/`frames_since_wake` | ✅ 真修 | `server.py:289-291`、`server.py:450-454`；测试 `test_p2_7`（计数口径缺口见 S-10） |
| SC-P2-8 语音文件 SHA256 pin（下载/已存在都验） | ⚠️ 基本真修、一处缺口（见 S-9） | `bootstrap.py:VOICE_SHA256`、`_download` 下载后验 SHA、`_ensure_voice` 已存在验 SHA；测试 `test_p2_8` |
| SC-P2-9 样本校验阈值读环境变量、与服务端同源 | ✅ 真修 | `bootstrap.py:_verify_thresholds` 读 `KWS_THRESHOLD`/`KWS_SCORE`，范围与 `server.py` 一致；测试 `test_p2_9` |
| SC-P2-10 重抽前备份 `.bak-<时间戳>` | ✅ 真修 | `bootstrap.py:_backup_existing` + `_verify_samples` 调用点；测试 `test_p2_10`（注：时间戳秒级精度，同一秒内两次重抽会覆盖备份，实测重抽间隔远大于 1 秒，风险可忽略） |
| SC-P2-11 按重抽次数计、最多 10 次重抽/11 次检测 | ✅ 真修 | `bootstrap.py:MAX_REROLLS=10`，`rerolls` 计数器；测试 `test_p2_11` 两例 |
| SC-P2-12 compose `user: "1000:1000"` + 卷属主 one-shot | ⚠️ 半修（见 S-2） | compose `user` 与 `nas-install.sh:222` chown 落实；但 Dockerfile 无 mkdir/chown/USER，compose 注释说法失实 |
| SC-P2-13 curl `--fail` + SHA 报错带大小 | ✅ 真修 | `download-model.sh:curl -sSLf --retry 2`，SHA 失败打印文件大小并说明下载已成功；测试 `test_p2_13` 两例 |

**其余边车审计面（无新发现，简述结论）：** 鉴权与握手——hello 前喂不了音频（二进制首帧即 `error + 1002` 关闭）；token 缺失/错误 `error + 1008`（`server.py:382-383`），`hmac.compare_digest` 常数时间比较，token 不进日志；未配/短 token 启动即 FATAL；鉴权在取代旧连接之前，未鉴权连接杀不掉合法旧连接；连接上限 16，超限 `error + 1013`。Opus/损坏帧——边车收的是 Hub 解码后的 PCM，Opus 解码失败在 Hub 侧；边车侧奇数长度帧记 error 后 `continue` 不致命，空帧/短尾块可安全喂入，垃圾字节只会被当作 int16 采样，不会卡死或崩溃（超大帧缺口见 S-4）。并发与资源——每 `device_id` 独立 `DeviceStream`，单事件循环串行解码，无共享可变状态串话；断开即 `del self.devices[device_id]` 丢弃流状态；模型缺失/加载失败进程非零退出，不静默降级；Hub 侧 `kws_status` 联动正确。依赖与构建——`requirements.txt` 全部 `==` 钉版本；`entrypoint.sh` 引用均加引号、`set -euo pipefail`、模型缺失下载失败即退出，未发现注入/权限问题（`download-model.sh` 先验 SHA 再解包，`tar --no-same-owner`）。Hub 契约容错——Hub 用 `encoding/json` 解 wake 事件，未知字段忽略、缺失 `rms` 时为 0 → `-100dB`（该设备仲裁必输，不崩溃）；`device_id` 一律以流自身 ID 为准，不轻信边车回传；`keyword` 不在配置表时忽略；`timestamp_ms` 为 0 时回退 Hub 收到时间。

### 7.4 固件/部署面：headline 全部核实通过

- patch.sh：证书默认装入、无目录直接失败 ✓（`patch.sh:74-86`）；P2-9 严格正则 ✓（三脚本同款）；P2-31 两处 `|| true` ✓（`:173-174`）；`trap ERR` + `fail()` 清理 `OUT`/`VERIFY_DIR`/`SQFS_ROOT` ✓；P2-30 38MB 预警 + 超原厂 2MB 警告 + `PATCH_XZ_BCJ` opt-in 默认关 ✓；P2-32 幂等 ✓（含 wireless latent bug 修复，镜像内 `ota()` 只有一个插入块）；P2-8 `SSH_PASSWORD_FILE` 优先 ✓（`patch_firmware.py:342-346`）；P2-6 §6 其余断言 ✓（除 F-1 的三处空转外）。
- download-ota.py P2-23 参数白名单 ✓；白名单 hash 实测通过（oh2p `.bin`、`lx06`/`oh2p` 原始 root.squashfs）✓。unpack.sh P2-9/P2-39 ✓。
- 部署：compose 5 个拉取镜像 + `Dockerfile.hub` 的 `debian:bookworm-slim@sha256:734a72b3…` 共 6 个 digest 全钉死，且与 Docker Hub 2026-11-18 实测 `latest` digest 逐一比对一致（whisper `3cc01f64…`、piper `333622ab…`、snapserver `20840a2b…`、mpd `6f8dbbb8…`、netease-api `ed9fc80e…`、debian `734a72b3…`，注释取值日期 2026-11-18 ✓）；`cap_drop: [ALL]` 7/7 服务 ✓；`user: "1000:1000"` 仅 Hub 与边车，其余 5 个仍 root 的原因与 cap_drop 补偿已写在 compose 头注释 ✓；`Dockerfile.hub` 有 appuser + HEALTHCHECK，`deploy/.dockerignore` 排除 `.env`/`*.md`/`*.sha256` ✓；`read_only` 暂缓现状已在注释记录 ✓。
- nas-install.sh：§0 Hub 清单校验 ✓（清单与二进制实测一致：12,093,544 字节 / `973a3b1f…3091`）；§5 容器数 7 检查 + 注册制日志行检查 ✓（缺失仅 WARN）；NAS_IP/NAS_DIR/NAS_SSH 三项输入正则 ✓；`remote_curl_api` 带引号 heredoc + NAS 端读 token ✓；P2-55 kws-sidecar 构建源发货 ✓（Dockerfile/server.py/entrypoint.sh/requirements.txt/scripts 齐全）。
- MA 残留：仅历史注释与文档说明，无服务/卷/端口/`MA_TOKEN` 的 live 引用 ✓。
- FW-P2-1 gofmt 进 CI ✓（`ci.yml:39-42`）；FW-P2-3 TEST_REPORT 勘误删除线 ✓（L166 `~~GLIBC_2.29~~` → 2.34 +“已被证伪”）；FW-P2-4 文档数值同步 ✓（Hub 12,093,544/`973a3b1f`、LX06 38,457,344/`7dafb950`、OH2P 30,179,328/`fe2bab99` 与实物一致，旧值均有取代标注）；FW-P2-6 patch.sh 头注释 ✓；FW-P2-7 checklist 同步 ✓（§4.1b `ws://127.0.0.1:10400`、§4.2 注册制、§1.1 边车源码发货、7 容器）；FW-P2-8 ✓（`scripts/check-vec-len.sh`、CI shell job、`check-wiring.go` 的 DEFERRED allowlist 已空且摘掉条件已写、`check-bounds.go` 已加摘掉条件）。
- P2-38 runbook §1.5 ✓（“绝不复用原厂 `bin/flash.sh` 推补丁镜像”），`VerifyTimeout` 30 秒与 `hub/internal/ota/ota.go:65` 一致 ✓。

### 7.5 融合 B（PR #3）核查结论

- **11 条路由鉴权：全部通过。** `web.go:Handler()` 中 4 条 `/api/netease/*` 与 7 条 `/api/music/*` 全部包在 `requireAuth` 内；未配置 `OPEN_XIAOAI_WEB_TOKEN` 时一律 401（fail closed）；Bearer 比较为 SHA-256 后恒定时间比较。无漏鉴权路由。
- **参数校验与错误泄露：基本通过。** `lyric` 的 id 只缺负数校验（H-11）；`qr/check` 的 key 判空但不限长；错误信息均只含上游状态码/摘要，无内部细节外泄。`PUT /api/config` 仍会显式拒绝已废弃的 `netease.music_assistant`（`web.go` configAPI），不静默丢弃。
- **netease_auth.go：通过，附 H-9。** cookie 落盘 0600（临时文件+改名，目录 0700）；`Logout` 清内存并删文件；过期处理走 `/login/status` + `account.id==0` 判定，`expires_at` 从 `Expires=`/`Max-Age=` 解析；`CreateQR`/`CheckQR` 无共享 key 状态、无竞态，但并发保存有 H-9 的小窗口；`main.go:505–513` 启动加载 cookie 失败只记日志、不阻塞启动，`web.go:993–995` 的 `Netease`/`NeteaseAuth` 接线完整，扫码成功后 `SetCookie` 即时生效。
- **Hub 直连路径：通过，附 H-1/H-3。** search→SongURL→MPD.PlayURL→Snapcast 链路完整，降级顺序明确（直连→本地曲库→明说放不了，`router.go:746–747`）；上游响应 1MB 限长、15s 超时；VIP 失败时逐首试下 3 首、再本地兜底、最后明确回复，不静默失败；`validateDownstreamURL`（`config.go:387–413`）对 `netease.api` 做 scheme 白名单 + 内网 host 校验，Web 改配置无法把 cookie 外发到公网域名。
- **music_folder warn-only：通过。** `config.go:353` `ValidateStartup` 仅在空值时报错；`:362` `MusicFolderWarning` 不存在/非目录只警告；`main.go:451` 只打日志；`router.go:886` `localMusicAvailable()` 每次 `os.Stat` 判断，无任何假设目录存在而 panic/静默失败的路径。
- **MA 残留：无活引用。** 全仓 `git grep` 命中只剩四类：历史审计文档（不改写历史）、`.semgrep/rules.yml:121`（有意保留的密钥检测正则）、`deploy/.env.example:11–13` 与 `deploy/nas-install.sh:182–199` 的迁移说明/删键逻辑、`MUSIC_FUSION.md` 等迁移文档。Go 代码、`hub/config.yaml`、compose 的 `environment` 里零命中；`8095`/`music-assistant` 活引用为零。

## 8. 重编镜像内容核实（固件面实测）

- LX06 `lx06_1.94.13-root-patched.squashfs`：38,457,344 字节，SHA256 `7dafb950…05e21` ✓；OH2P：30,179,328 字节，SHA256 `fe2bab99…5c926` ✓。与 `work/` 产物、`FLASH_RUNBOOK.md` §1 表、`flash-check.sh` 内 `KNOWN_PATCHED_SHA256` 三处一致；`flash-check.sh --strict` 两型号均 exit 0。
- P1-36：`ttyS0::askfirst:/bin/login` 在镜像内 ✓；PAM `libmico` 已注释、`pam_unix` 已启用 ✓；shadow `$6$` + 0600 ✓；dropbear 三门控已删 ✓；OTA 三处禁用在镜像内验证 ✓（`bin/ota` 两函数头 `return 1`、`flash.sh` `upgrade_param_check` 头 `return 1`、`wireless` `ota()` 头 `echo "download_fail"` + `exit 3`）；LX06 `libxaudio_engine.so` 内 `hw:0,3` 计数 0、`noop` 计数 1 ✓。
- P2-33：`etc/asound.conf.bak` 与 `etc/asound.conf.dts.bak` 经 `cmp` 与原厂 `work/<型号>/rootfs` 逐字节相同，两型号 ✓。
- P2-34：`/etc/open-xiaoai/init.sh` 在两镜像内存在，755、root 属主，两镜像 SHA256 相同，`rc.local:4` 接线 ✓。
- mTLS：`ca.crt`/`client.crt` 存在，`client.key` 0600，两镜像内三文件 SHA256 完全相同、与 `work/certs` 一致，有效期至 2036 ✓。
- **核实限度**：root 密码 hash 与旧镜像的一致性**无法比对**——旧镜像（SHA `7266aba9…`/`139de363…`）已不在磁盘上，无任何旧 hash 记录可供对照。只观察到两份新镜像的盐互不相同（见 F-4）。

## 9. 部署前必修清单（P1，9 项）

按影响排序，建议按此顺序修：

1. **H-1** 点歌总超时预算（FR-05 3s 目标在劣化场景下失效）
2. **C-1** AEC 回读实际缓冲并 warn/推导延迟（否则 AEC 可能全程零作用）
3. **S-1** Hub 接 `rms_peak` 进仲裁（否则 SC-P2-2 的修复白做）
4. **F-1** 打包验证改补丁特有标记 + 回归测试（否则下次重编拦不住回归）
5. **F-2** inittab 三行统一 `/bin/login`（或加全表断言）+ 首刷核对启动日志
6. **S-2** Dockerfile 加 `mkdir + chown`（或 entrypoint 预检）+ 纠正 compose 注释
7. **S-3** 定决策：接受“回环 + token”安全假设，或给边车加 TLS
8. **C-2** CI 加 `cargo test --features sherpa-kws` 一步
9. **F-3** pin 改为必填，或让安装流程写 pin + 补安装文档

其余 44 项 P2 可进下一批；其中 H-2（MPD 无界读）、H-3（SongURL 未验 scheme）、H-6（HA Close 双关 panic）、S-7（边车镜像未钉 digest）、C-10（无 WS Ping/发送超时）建议优先。

## 10. 局限与未验证项

- 无音箱硬件、无 NAS：刷机、NAS 部署、同步播放、真人唤醒率、双机仲裁、打断率、网易云扫码、HA 真联调、AEC 真实延迟校准、P2-35 均未验证，记 BLOCKED。
- `go test -race` 本轮未重跑；上一轮的 race 失败在测试替身，生产竞态未检出——不等于生产无竞态（H-6、H-7 是本轮新发现的两个生产侧竞态证据，说明 race 检查值得补跑）。
- gosec 14 项中 8 项无记录，无法裁决（H-10）。
- 本轮只审计、不修代码；报告中所有修复建议均未实施，待下一轮修。
