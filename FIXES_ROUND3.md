# P1 修复轮（第三轮审计）— FIXES_ROUND3.md

> 2026-11-18，分支 `fix-p1-round3`（自 main 3a9d1d9），协调员 Friday。
> 本轮只修 AUDIT_ROUND3.md §9 的 9 个 P1，按 §9 顺序；44 个 P2 不在本轮范围。
> 测试口径：合成语音 / 假设备标合成；真机才能验证的项一律记 BLOCKED。

## 0. 总览

9 项 P1：**修了 8 项，S-3 部分修 1 项**（TLS 实现等 bai 定夺，见 §2）。

| 面 | 项 | 修了 | 部分修 | 未修 BLOCKED |
|---|---|---|---|---|
| Hub | H-1、S-1 | 2 | 0 | 0 |
| 客户端 | C-1、C-2 | 2 | 0 | 0 |
| kws-sidecar | S-2、S-3 | 1（S-2） | 1（S-3） | 0 |
| 固件 / 部署 / CI | F-1、F-2、F-3 | 3 | 0 | 0 |
| 合计 | 9 | 8 | 1 | 0 |

另：镜像因 F-2 重编（§3，两型号新 SHA，root 密码未换）；C-1 回归测试顺带抓到一个真 bug 并修了（§1）；F-7（P2）README 矛盾未动，留给 P2 轮（§6）。

基线（main 3a9d1d9）：go 270、cargo 96/101、pytest 固件 86、边车 55、lint 全绿。
本轮终值（协调员最终重跑，见 §4）：go **277 passed**（22 包全 ok）、cargo **105 / 110**（默认 / sherpa-kws）、pytest 固件 **100**、边车 **66 passed + 1 skipped**、lint.sh **全部通过**、go `-race` kws 包 ok。

## 1. 逐项状态表（按 §9 顺序）

### H-1 点歌总超时预算 — 修了（提交 `98fe64b`）
- `Router` 新增 `MusicBudget` 字段（默认 8s，`defaultMusicBudget`）：点歌的网易云阶段（Search + SongURL）用 `context.WithTimeout` 单独约束，不再沿用语音管线 120s 的总超时；预算一到，在途 HTTP 请求即取消，转本地兜底。
- 新增 `fetchSongURLs`：3 首歌的 SongURL 并行取，结果按原搜歌顺序排列，调用方按顺序优先播放（第一首优先的语义不变；第一首无地址时播第二首，不抢第三首）。MPD `PlayURL` 仍逐首试（本地 MPD，每条命令自带 5s 读写 deadline，不占网易云预算）。
- 新增 `musicSlowFallback`：只有真正超时才播报「网易云有点慢，先试本地」；快速失败（VIP/无版权、搜歌 500）仍走原有明确回复，不误报。Router 只有一个回复通道，播报落在回复句首。
- 回归测试（`hub/internal/router/router_p1r3_test.go`，4 个）：搜歌超时转本地、取地址超时转本地、SongURL 并行（3 次请求发出间隔 <400ms，播第二首、MPD 收到第二首地址、未抢第三首）、默认预算 8s。

### C-1 AEC 延迟按实际缓冲推导 — 修了（提交 `18eea63`）
- `open_pcm` 在 `hw_params` 生效后用 `hw_params_current()` 回读实际 `buffer_size` / `period_size`，记 info 日志；与请求值（1280f/320f）不符时明确 warn，播放方向注明推导出的 AEC 延迟帧数；回读失败不致命，按请求值假设并 warn。新增 `PcmBufferInfo` 与 `Audio::playback_aec_delay_frames()`。
- `aec.rs` 新增 `ref_delay_frames_for_buffer`：延迟帧数 = 实际播放缓冲总帧数 / 320，四舍五入，至少 1 帧（1280→4、960→3、1600→5）。延迟帧数改为 `Aec` 实例状态（`with_delay_frames` / `set_delay_frames` / `delay_frames()`），不再是写死常量；`main.rs` 会话每次打开播放设备后重推导，经共享 `Arc<AtomicUsize>` 写回录音任务，每帧取最新值、变化时热更新并记 info 日志。
- 修正两处误导注释：NLMS 窗是单侧 0~+16ms（不是“前后各 16ms”）；采集 `hw:0,0` 与播放 `hw:0,2` 是两个 PCM 设备，延迟按帧计数对齐而非墙钟。
- **回归测试顺带抓到一个真 bug 并修了**：延迟热下调后，`process_frame` 的 trim 用 `delay+1` 导致队列永远缩不到新长度，队首永远是旧延迟的帧、新延迟永不生效（中途改延迟测试失败，残余 RMS 2656 > 输入 2296）。改为 trim 到 `delay` 帧，稳态不受影响（稳态队列长恰为 `delay`）。
- 回归测试（9 个，白噪声参考——纯正弦周期整除帧长时 NLMS 能靠相位拟合蒙对，测不出失配）：推导值、3 帧延迟抑制回声、延迟错位不抑制、中途改延迟后恢复抑制、setter 钳制、`PcmBufferInfo::period_count`、非 ALSA 设备返回 None、ALSA `null` 插件回读（本 VM 无硬件也能跑通 `open_pcm` 全程）、`describe` 含回读缓冲。
- BLOCKED：真实声学延迟仍需真机校准（缓冲占用只是延迟的一部分，扬声器-麦克风声学传播、驱动内部排队仍未知）；VM 上无法验证硬件取整为 3/5 周期时的 warn/推导分支（只有 `null` 插件的请求值一致路径被测到）。已写进 `aec.rs` 模块头注释。

### S-1 Hub 接 `rms_peak` 进仲裁 — 修了（提交 `f1ed8f6`）
- 三处同步加 `RMSPeak`：`WakeEvent` 新增字段 + `EffectiveRMS()`（>0 用峰值，否则回退均值）；`handleMessage` 的 wake 解析结构新增 `rms_peak` 字段并赋进事件；`OnKWSWake` 改用 `EffectiveRMS()` 换算 dB 送仲裁，`rms_peak <= 0` 时记一条 warn 日志（「缺少 rms_peak，回退用 rms 仲裁」），旧边车行为不变。
- 回归测试：`TestWakeEventRMSPeakParsed`（假边车发 rms 0.137 / rms_peak 0.311，断言事件解析出峰值且 `EffectiveRMS()==0.311`）、`TestWakeEventEffectiveRMSFallback`（缺峰值回退均值）；`TestKWSArbitrationUsesRMSPeak`（lx06-1 均值 0.6/峰值 0.25，oh2p-1 均值 0.2/峰值 0.65；用均值仲裁 lx06 赢、用峰值仲裁 oh2p 赢，断言赢家是 oh2p-1）。旧 `sendWake` 签名未动，回退路径仍由旧测试覆盖。

### F-1 打包验证改成真检查 — 修了（提交 `5fa881a`）
- `patch_firmware.py` 新增 `verify_markers()`，作为 `patch.sh` §6 与 `flash-check.sh` §4b 共用的唯一断言源：dropbear 查三门控已删（enable=0 返回、蓝牙门控、ssh_en 门控；`mkdir` 行只作文件完整性检查，因原厂 keygen 里已有该行）；wireless 查 `ota()` 头后 60 字符内同时有 `echo "download_fail"` 与 `exit 3`（与 `patch_disable_ota` 的校验同款）；asound 查两文件 mico_record/noop 块走 Capture + Capture 块参数 384/6144 且无 480/3840；另含 inittab、shadow、PAM、OTA 三处、libxaudio。
- 实测确认 OH2P 的 `asound.conf` 原厂即 Capture 且 Capture 块参数与补丁后逐字节相同——该文件只能由 `.bak` 证明打过补丁：`flash-check.sh` §4b 把 `.bak` 缺失从警告改为 FAIL，原始 `root.squashfs` 在时逐字节比对 `.bak`（与 `patch.sh` §6 同款）。
- 回归测试：原厂 rootfs（LX06/OH2P）跑断言必 FAIL、原厂 rootfs 打包成镜像跑 flash-check 必 FAIL、门控插回 / OTA 块删除 / inittab 改回 ash 的变异必 FAIL。

### F-2 inittab 三行统一 `/bin/login` — 修了（提交 `cf2a4e4`），已重编镜像
- `patch_login` 把 `tts/0` 与 `tty1` 的免密 ash 改成 `/bin/login`；`check_login` 接受原厂态与已打补丁态；两脚本同步要求三行 login、全表无 ash。
- **tts/0 死项结论（已独立复现内核证据）：** LX06 `boot.img` 内核解压后 8,241,160 字节、Linux 4.9.61，`ttyS\x00` 出现 2 次、`tts\x00` 出现 0 次，meson-uart 只注册 `ttyS` 设备名；内核里 3 处裸 `tts` 全在 `tts_enable` 属性名内，无 `tts/` 路径；原厂 rootfs 无任何 `tts` 设备节点来源（`dev/` 为空，走 devtmpfs）。所以 `/dev/tts/0` 不会生成，该行是死项；`tty1` 是无键盘/显示的音箱不可达的虚拟终端。但纵深防御仍统一改掉。真机启动日志核对仍待首刷（BLOCKED，见 §5）。
- **重编流程（先记 hash 再重编）：** 重编前把两份旧镜像的 shadow root hash 记到本地 gitignored 文件 `firmware-patch/work/.root-hashes-backup`（已用 `git check-ignore` 确认忽略，work 与 release 两份镜像的 hash 一致）；新增 `PATCH_REUSE_ROOT_HASH_FILE` 机制（`patch_firmware.py:_reuse_root_hash`，非法 hash 即拒），两型号重编均沿用旧 hash，重编后逐字节比对验证与备份一致，root 密码未换。hash 值只存在该忽略文件里，报告、提交、PR 均不含 hash 值。详见 §3。

### S-2 全新卷 compose up 不 crash-loop — 修了（提交 `664fc69`）
- 审计建议 (a)(b) 二选一，两个都做了，各 cover 一种卷状态：(a) `kws-sidecar/Dockerfile` 加 `RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models`，且放在 `VOLUME` 声明之前（之后改会被丢弃）——全新具名卷首次挂载时 copy-up 带走 1000:1000 属主，不走 `nas-install.sh` 也能直接 `compose up`；(b) `kws-sidecar/entrypoint.sh` 在下载前加可写预检：目录不存在时尝试 `mkdir -p`，失败或不可写时给可读 FATAL（含 `ls -ld` 与 chown 修复命令）再退出，不再走到 `download-model.sh` 深处的 `cp` 上失败——覆盖 (a) 够不着的情形：已存在的 root 属主旧卷（copy-up 对它无效）。
- `deploy/docker-compose.yml` 两处注释已纠正：新卷属主来自 Dockerfile 的 mkdir + chown（copy-up），`nas-install.sh §4` 的 chown one-shot 只负责已存在的旧卷迁移。`deploy/NAS_CHECKLIST.md` 新增 §8 全新卷冒烟项（只删 `kws-data` 卷、明确禁 `down -v`、不跑 `nas-install.sh` 直接 `up`、预期 `KWS model ready` + `/health` 200），并在 4.1b 与故障速查里同步了新 FATAL 文案。
- 附带说明：失实的 copy-up 说法原文在 `nas-install.sh:217`（不在本轮可改范围，没改它）——但 Dockerfile 修复让这句话从假话变成了真话；`nas-install.sh` 的执行顺序是先 `build` 再 chown one-shot，新卷在 one-shot 那一步建卷时即 copy-up，所以 one-shot 在新卷上仍是空操作，与注释一致。
- 回归测试（`kws-sidecar/tests/test_sidecar_p1r3_s2.py`，7 项）：Dockerfile mkdir+chown 在 VOLUME 之前、entrypoint 预检快失败 / 好路径直达启动、compose 注释与检查单 §8 接线。
- BLOCKED：真实 `docker compose up` 全新卷实测——本机无 docker，做不了；Dockerfile 与 entrypoint 逻辑已审过（copy-up 语义、RUN 在 VOLUME 之前、预检在下载之前、root 下 `-w` 恒真故预检对 root 无害），NAS 首次部署时必须按 `NAS_CHECKLIST.md` §8 实测。

### S-3 边车 TLS — 部分修（提交 `272f2d6`），TLS 实现未做，等 bai 定夺
- 只做了不依赖决定的两部分（见 §2）：(1) `KWS_HOST` 默认值 `0.0.0.0` → `127.0.0.1`，共三处：`kws-sidecar/Dockerfile` 的 ENV、`kws-sidecar/server.py` 的 `--host` 默认（另加 `help` 说明）；核实了绑定逻辑：`serve(handler, args.host, args.port, …)` 只绑这一个地址，无其他监听；显式配 `KWS_HOST=0.0.0.0` 或 `--host` 仍可覆盖（opt-in）。(2) “边车不启用 TLS、依赖 127.0.0.1 绑定”已写为正式安全假设：`kws-sidecar/README.md` 新增“安全假设”一节（假设内容、三层一致、风险边界——任何 NAS 本机进程可直连、token 明文 hello、Hub 的指纹 pin 目前配了也用不上、`wss://` 一配就连不上是 fail-closed、待定决定与失效条件），`server.py` 模块 docstring 与 compose 注释同步引用；`NAS_CHECKLIST.md` 4.1b 的旧说法（“0.0.0.0 是镜像默认”）已同步改掉。
- 回归测试（`kws-sidecar/tests/test_sidecar_p1r3_s3.py`，5 项）：host 默认回环及显式覆盖、server 无 TLS 且假设已文档化、`/proc/net/tcp` 实测只在 127.0.0.1:10409 上 LISTEN。测试中的一个坑：最初用“从本机非回环 IP 连 10409 应被拒”验证绑定，在沙箱里失败——沙箱网关会代答 TCP（连上了但没返回合法 HTTP），结论不可靠；已改成读 `/proc/net/tcp` 查 LISTEN 地址断言，通过。

### C-2 CI 编译 sherpa-kws feature — 修了（提交 `d8250ec`）
- `.github/workflows/ci.yml` 的 rust job 在 `cargo test --locked` 后新增一步 `cargo test --locked --features sherpa-kws`（与默认 feature 并跑，x86_64 宿主机即可）。YAML 语法已用 `python3 -c yaml.safe_load` 验证通过；本地 `cargo test --features sherpa-kws` 110 通过。

### F-3 SHA256 pin 改必填 — 修了（提交 `08e2fc0`），选方案 (b)
- 包装器改为：`/data/init.sh` 存在而 pin 文件缺失即拒绝执行并记日志（`sha256 pin missing`），pin 不一致同样拒绝；两脚本同步要求包装器含 pin 必填分支。
- `firmware-patch/README.md` 新增“/data/init.sh 安装流程”：钩子内容模板（启动 `/data/open-xiaoai/client`，路径引自 `client/README.md`）、scp + 写 pin + 属主/权限步骤、包装器三条校验规则、更新与排障（改钩子须同步重写 pin；Hub OTA 升级客户端二进制时 pin 不用重写）。

## 2. S-3 未决部分（需 bai 拍板）

S-3 本轮是**部分修**：回环绑定与安全假设文档已做（见 §1），TLS 实现本身未做。缺的决定：bai 在以下两者之间二选一——

- (a) 接受“回环 + token”安全假设（边车不启用 TLS，依赖 127.0.0.1 绑定；`KWS_HOST` 默认已改回环，假设已写入文档）；
- (b) 给边车加 TLS（自签证书 + 启动期生成/挂载，让 Hub 的指纹 pin 真正跑通 + 端到端测试）。

选 (b) 时本轮写进文档的安全假设即失效，另起一项做 TLS。另：S-3 的决定与 P1-40 路由确认（长期服务端模式）相互独立，不互相阻塞。

## 3. 镜像重编（F-2 触发）

| 型号 | 大小 | 新 SHA256 |
|---|---|---|
| LX06 | 38,457,344 字节 | `1a5c27603d7ab8064841f3e970258ef39ae754c2847a46fc0529168c250d0281` |
| OH2P | 30,179,328 字节 | `8db02670f0c81bda5059e5a0fe2cad5b6045f4afe252a310353c7ac3acf9d197` |

旧 SHA（LX06 `7dafb950…05e21`、OH2P `fe2bab99…3b5c926`）已作废；大小与旧镜像相同。上机包 `~/workspace/speaker-open-xiaoai-rebuild/release/` 已换新镜像，包内 `RELEASE.md` 与 `FLASH_RUNBOOK.md` 已与仓库版同步（包内文件不在 git 内）。

**hash 复用验证（F-4 教训的闭环）：** 重编前两份旧镜像的 shadow root hash 已记到本地 gitignored 文件 `firmware-patch/work/.root-hashes-backup`（`git check-ignore` 确认忽略；`git ls-files firmware-patch/work/` 为空，无任何 work/ 文件被跟踪）；重编经 `PATCH_REUSE_ROOT_HASH_FILE` 机制沿用旧 hash，重编后逐字节比对验证与备份一致——**root 密码未换**，bai 手里已有的密码继续有效。hash 值只存在该忽略文件里，本报告、提交、PR 均不含 hash 值。

## 4. 全量测试重跑输出（协调员最终重跑，2026-11-18）

- `go test -count=1 ./...`：**22 包全 ok、277 passed**、0 FAIL；`go vet` 干净；`gofmt` 干净；`go test -race ./internal/kws/` ok。
- `cargo test --locked`：**105 passed**；`cargo test --locked --features sherpa-kws`：**110 passed**；`cargo fmt --check` 通过；`cargo clippy --all-targets` 0 warning。
- `pytest firmware-patch/tests/`：**100 passed**（基线 86 + 14 新增：verify_markers 原厂/变异、inittab 三行、hash 复用、包装器四场景运行时、原厂镜像 flash-check）。
- `pytest kws-sidecar/tests/`：**66 passed，1 skipped**（基线 55 + 12 新增：S-2 文件 7 项、S-3 文件 5 项；跳过的 1 项是“不可写目录预检”，因本机以 root 跑测试、root 绕过文件权限，按设计 skip）。
- `bash scripts/lint.sh`：**全部通过**（初跑时 semgrep 规则 3 拦住 C-1 新增测试 helper `noise_signal` 里的 `Vec::with_capacity(total_samples)`；协调员改成 `Vec::new()` + push，测试含义不变，重跑后全绿，见 §6）。
- `flash-check.sh` 四路（两型号 × 默认/`--strict`）：**全部 exit 0，无 FAIL、无 WARN**，SHA 与记录值一致。

## 5. BLOCKED 清单

1. **S-3 TLS 实现**（需 bai 拍板）：见 §2，选 (a) 接受回环假设或 (b) 给边车加 TLS。
2. **真实声学延迟校准**（C-1 残留，需真机）：缓冲占用只是延迟的一部分，扬声器-麦克风声学传播、驱动内部排队仍未知；刷机验证时实测回声延迟。VM 上无法验证硬件取整为 3/5 周期时的 warn/推导分支。
3. **S-2 真实 compose up 全新卷实测**（需 NAS）：本机无 docker；NAS 首次部署时按 `NAS_CHECKLIST.md` §8 实测。
4. **F-2 真机启动日志核对**（需真机）：确认 `/dev/tts/0` 不存在（与 P2-35 同源）。
5. **真机项**（与前几轮相同，全部仍 BLOCKED）：刷机、NAS 部署、同步播放、真人唤醒率、双机仲裁、打断率、网易云扫码、HA 真联调、AEC 真实延迟校准、P2-35。
6. **ARM 交叉编译未实跑**（C-1 遗留）：`cargo check --target armv7-unknown-linux-gnueabihf` 在本机 shell 里因 `arm-linux-gnueabihf-gcc` 不在 PATH 而失败（cc-rs 找编译器阶段，与代码无关，交叉工具链由构建脚本提供）。类型层面已确认无风险：`alsa::pcm::Frames` 即 `snd_pcm_sframes_t = c_long`，32-bit 上是 i32，`as i64` 是拓宽转换，正确。

## 6. 协调员取舍摘要

| 决定 | 取舍 |
|---|---|
| H-1 总预算 8s + 并行取地址 | 预算定 8s（审计建议值）：劣化场景下 8s 即转本地兜底，FR-05 的 3s 目标仍需正常路径兑现，8s 是“不静默”的上限而非“3s 出声”的保证；并行取地址保持第一首优先语义不变 |
| H-1 超时播报只在真正超时时说 | 代价是快速失败仍走原有回复；收益是不误报“有点慢”，用户听到慢就是真慢 |
| C-1 warn + 推导两档都做 | 审计给的是“或”，两档都做了：回读日志是证据，推导是纠正；代价是多一处实例状态，收益是缓冲取整为 3/5 周期时 AEC 不再静默错位 |
| C-1 trim 改 `delay` 帧 | 回归测试抓到的真 bug：延迟热下调后旧 trim 让新延迟永不生效；改为 trim 到 `delay` 帧，稳态不受影响 |
| S-1 缺失峰值回退均值 + warn | 代价是旧边车仍用均值仲裁；收益是协议兼容，缺失时有日志可查 |
| F-1 verify_markers 单一断言源 | patch.sh §6 与 flash-check §4b 共用同一函数；代价是改动面跨两脚本，收益是以后只改一处 |
| F-1 OH2P asound 靠 `.bak` 证明 | 原厂与补丁后 asound 参数逐字节相同，标记断言无法区分；`.bak` 缺失从警告改为 FAIL，原始 root.squashfs 在时逐字节比对 |
| F-2 三行统一改掉（哪怕 tts/0 是死项） | 代价是重编镜像、SHA 全变；收益是纵深防御完整，不留“已知免密 shell” |
| F-2 先记 hash 再重编 | 代价是多一个 gitignored 备份文件与复用机制；收益是 F-4 的教训闭环：root 密码可验证未换 |
| F-3 选方案 (b) pin 必填 | 代价是旧安装流程（无 pin）会被拒绝；收益是不依赖任何人记得写 pin，缺 pin 即 fail-closed；安装文档同步补齐 |
| S-2 (a)(b) 两个都做 | 审计建议二选一；(a) 只 cover 全新卷，(b) 才 cover 已存在的 root 属主旧卷；两个都做才算修完 |
| S-3 只做不依赖决定的部分 | 不替 bai 定 TLS；回环绑定 + 安全假设文档两方案都兼容，选 (b) 时假设失效、另起一项 |
| 协调员补修 lint（semgrep 规则 3） | C-1 新增测试 helper 用 `Vec::with_capacity(变量)` 被 lint 拦住；改成 `Vec::new()` + push，测试含义不变，不动生产代码 |
| 44 个 P2 不动 | 按任务范围；其中 F-7（README rc.local 直行与新包装器文档矛盾）已记为观察项，留给 P2 轮 |

## 7. 观察项（留给下一批，不在本轮修）

- **F-7（P2，未动）：** `firmware-patch/README.md:47` 仍写旧的 rc.local 直行，与本轮新增的 F-3 安装文档（走包装器）在同一文件内矛盾。按任务要求没碰，留给 P2 轮修。
- **混合 hunk 跨提交（两处附带行）：** flash-check §4b 的包装器 pin 断言（3 行）随 F-1 提交先行落地；`_patch_all_apply` 的 `result["start"]` 文案同步随 F-2 提交落地。功能归属不受影响。
- **ARM 交叉编译未实跑：** 见 §5 第 6 项。
