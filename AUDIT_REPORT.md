# open-xiaoai-next 代码审计报告

- 审计日期：2026-11-17
- 审计对象：`~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/`（client/、hub/、firmware-patch/scripts/ + tests/、deploy/），对照 `~/workspace/speaker-open-xiaoai-rebuild/PLAN.md` v4（FR-01~FR-15、NFR-01~NFR-07）与 `open-xiaoai-next/TEST_REPORT.md`
- 审计方法：只读审计，**未修改任何源代码**；4 个审计员并行分工（安全、正确性、固件补丁与部署、健壮性与质量），逐文件读代码、追踪数据流与并发路径、grep 交叉验证调用点；本报告由协调员去重合并，**8 项关键结论已由协调员 grep/读代码抽查核实**（Web 无鉴权、默认口令、ARM 构建无 sherpa、main.go 接线缺失、60s 读超时、卷挂错位、发现广播 Token）。
- 审计范围排除：`firmware-patch/work/`（官方固件解包内容）未审；未运行 `go test`/`cargo test`/`pytest`（测试结论引用 TEST_REPORT.md 的 88/37/36 通过数）。

## 结论总览

| 分级 | 数量（去重后） | 含义 |
|---|---|---|
| **P0** | **6** | 刷机/上线前必须修：变砖、丢数据、远程执行、认证绕过 |
| **P1** | **46** | 功能性错误：功能在宣称场景下会错 |
| **P2** | **56** | 健壮性/可维护性 |
| 合计 | **108** | 原始发现 154 项，去重合并后 108 项 |

**最严重的结论：代码在 VM 上的单元测试全绿，但生产接线存在系统性断裂。** 按 FR 对照，FR-01/03/04/05/11/12/13 在生产态是**未实现**，FR-02/06/07/08/09/10/14/15 是**部分实现**，NFR-02/04/05 **不满足**。也就是说：包内逻辑大多写对了，但 `hub/cmd/hub/main.go` 的接线把其中多条掐断了——点歌、Snapcast、对讲、多 Agent 路由、房间认领、OTA 回滚在生产部署下全部不可用或不存在。**在修完 P0 并接通 P1 的接线类问题之前，不建议刷机上线；刷机后首要风险不是功能不好用，而是 Web 零鉴权 + 默认 root 口令的组合。**

## 与 TEST_REPORT.md 的关系：比报告所述更严重的 3 项

1. **OTA 回滚是零实现，不是“待真机验证”。** TEST_REPORT 写“回滚本身是设备端 boot1 行为，只差真机验证”。实际：客户端 `HubMsg::Ota` 只记日志（`client/src/main.rs:128-132`）；Hub 的 `MarkRebooted`/`ConfirmHello`/`Sweep` 在生产代码中零调用（`hub/cmd/hub/main.go` 全文无调用）；任务会永久卡在 `pushing` 并阻塞后续重试。FR-11 目前是**零实现**。
2. **ARM 交付二进制根本不含 KWS。** TEST_REPORT 宣称“KWS 已验证、ARM 产物已交付”。实际：`client/build-armv7.sh` 全程无 `--features sherpa-kws`，且 ARM 版 `libsherpa-onnx-c-api.so` 本就未交叉编译。刷进音箱的 Client 在 FR-01 上是零能力——永不产生 `wake` 事件，整条语音链路无从开始，且失败是静默的（只有一条 warn）。
3. **main.go 的问题不只是“无单测”。** TEST_REPORT 只写“hub/cmd/hub 无测试文件”。实际接线本身有多个生产态错误：QueueID 写死 `""`、MPD 从未注入、Snapcast 从未 Dial、Agent 启动选定一次、房间认领无发送方、STT/TTS 失败留 nil、HA/Wyoming 无重连。

其余发现均为 TEST_REPORT 未提及的新发现，不重复其已写明的已知缺口（Opus 级回声接线、FR-15 真实通知链路、NAS 未实测、KWS ARM .so、ALSA 真机、MPD fifo、MA 组网、真人唤醒率等）。

---

## P0 — 刷机/上线前必须修（6 项）

### P0-1 Web 管理面板（:8080）零鉴权：局域网任何人可读配置、改配置、推 OTA
- **修复状态（2026-11-17，已修复）**：`/api/*` 加 Bearer 鉴权（与 `OPEN_XIAOAI_TOKEN` 独立的 `OPEN_XIAOAI_WEB_TOKEN`，未配 Token 时一律 401 fail closed）+ 恒定时间比较 + 单 IP 失败限流；`NewHTTPServer` 带读写超时与请求体上限；`WebListen` 默认 `127.0.0.1:8080`。证据：`go test -count=1 ./internal/web/` 10 passed（本轮重跑）。剩余：NAS 上 Web 实际监听地址未实测；GET `/api/config` 是否脱敏敏感字段未在本轮核对。
- **文件:行号**：`hub/internal/web/web.go:33-44`（`Handler()` 注册 `/`、`/api/status`、`/api/devices`、`/api/config`、`/api/ota`，全程无鉴权中间件）；`hub/internal/web/web.go:85-115`（`configAPI` GET/PUT）、`117-144`（`otaAPI` POST）；`hub/cmd/hub/main.go:442-443`（`http.ListenAndServe(cfg.Hub.WebListen, …)`，无 TLS、无超时）；`hub/config.yaml:56`（`web_listen: ":8080"`，监听全部接口）；`deploy/docker-compose.yml:21`（hub `network_mode: host`）。
- **问题**：注释称“只做内网管理界面，不对外暴露”，但代码上没有任何鉴权、来源限制或仅回环绑定。同一局域网的任何设备无需 Token 即可 GET 全部配置、PUT 改写 `config.yaml`、POST 推 OTA。`web_test.go` 的测试也全程无鉴权用例，说明缺失是设计性的。
- **为什么重要**：一条未鉴权 PUT 即可：(a) `hub.tls=false` 把 4399 从 wss 降级为明文 ws；(b) `home_assistant.url` 指到攻击者服务器，Hub 会把 `HASS_TOKEN` 当 `access_token` 发过去（`hub/internal/ha/ha.go:67`）；(c) `agents[].base_url` 指到攻击者服务器，Hub 会把 `LLM_API_KEY` 当 Bearer 发过去并把用户语音文本送过去（`hub/internal/agent/agent.go:77-78`）；(d) `hub.stt/tts` 指到攻击者 Wyoming 服务，全部语音 PCM 与 TTS 文本外泄。改动落盘持久化，下次重启即生效。`Validate()` 只校验 4 项（`config.go:112-130`），不校验 `hub.tls`、`home_assistant.url`、`agents[].base_url`、`netease.*`、`scan_cidr`、`web_listen`。OTA 接口只校验三字段非空与 sha256 为 64 位 hex，不校验 URL scheme/host（`hub/internal/ota/ota.go:65-90`），`http://`/`file://` 均可下发；当前客户端尚忽略 OTA 消息（`client/src/main.rs:128-131`），故暂为 latent，但 Hub 侧下发链路已就绪，一旦接通客户端 OTA 即成未鉴权远程刷机入口。
- **建议修复**：上线前二选一且做全：(a) `WebListen` 默认改为 `127.0.0.1:8080`，远程管理走 SSH 隧道；或 (b) 全部 `/api/*` 加与 `OPEN_XIAOAI_TOKEN` 同级（最好独立）的 Bearer Token 鉴权（恒定时间比较、失败记日志并限流），GET `/api/config` 不得返回敏感字段。收紧 `Validate()`：下游地址只允许 `http(s)://` + 内网地址白名单，拒绝公网 IP/域名与 `file://`；`hub.tls` 不允许经 Web 改为 false。`ota.Push()` 增加 URL 白名单（只允许配置的 OTA 仓库 host、只允许 `https://`）。给 `http.Server` 加 `ReadHeaderTimeout/ReadTimeout/WriteTimeout` 与请求体上限。修复后补测试：无 Token 的 PUT/POST 必须 401。

### P0-2 音箱 root 密码是公开的默认值 `open-xiaoai`，弱 md5crypt + 固定盐，SSH 被改成始终开启
- **修复状态（2026-11-17，已修复）**：删除 `DEFAULT_PASSWORD`，只从环境变量 `SSH_PASSWORD` 或交互式 `getpass` 读 ≥16 位密码；`md5crypt $1$` 换成 `sha512crypt`（5000 轮，随机盐）；`main()` 不打印 `login_hash`。证据：`python3 -m pytest tests/ -q` 61 passed（本轮重跑）。剩余：真机刷机后 SSH 密码是否生效待关口 1 验证。
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:37-38`（`DEFAULT_PASSWORD = "open-xiaoai"`，`PASSWORD_SALT = "open-xiaoai"`，盐截断后仅 `open-xia`）；`:157-204`（`md5_crypt()`：`$1$` MD5crypt，仅 1000 轮）；`:205-206`（`_password()`：环境变量 `SSH_PASSWORD` 未设即用默认值）；`:287-294`（`patch_login()` 把 hash 写进 `/etc/shadow` 的 root 行）；`:221-262`（`patch_ssh()`：删掉 `enable=0` 返回、蓝牙模式门控、`/data/ssh_en` 门控，dropbear 始终启动）；`:559,587-588`（`patch_all()` 返回 `login_hash`，`main()` 把它 `print` 到 stdout）；`firmware-patch/README.md`“root 密码”参数表公开写明默认口令与 hash；`firmware-patch/scripts/patch.sh` 全文无 `SSH_PASSWORD` 未设置时的警告或拒绝。
- **问题**：PLAN NFR-02 要求“SSH 改默认密码”，实际实现是把默认密码写死在公开仓库代码里：任何人读到源码即知道每台未改密码音箱的 root 密码；盐固定且公开，hash 可预先算好、离线暴力破解成本极低；dropbear 又被改成无条件启动、root 密码登录。`login_hash` 还被打印到终端/日志，进一步扩大暴露面。
- **为什么重要**：刷机后只要不主动设 `SSH_PASSWORD`，局域网任何人 `ssh root@<音箱IP>` + `open-xiaoai` 即 root；一台音箱失陷即可长期监听麦克风、冒充设备连 Hub。且与 Hub Token、TLS 无关。
- **建议修复**：刷机前必须修：(a) 删除 `DEFAULT_PASSWORD`，`SSH_PASSWORD` 未设时 `patch_firmware.py` 直接失败退出，强制操作员现场设强随机密码（≥16 位），永不把默认值写进仓库/文档；(b) 把 `md5crypt $1$` 换成 `yescrypt`/`sha512crypt`（若固件 crypt 支持）或至少 `sha256crypt`，盐每次随机生成；(c) `main()` 不打印 `login_hash`，`--password` 改为只从环境变量/交互式 `getpass` 读，禁止命令行传密码；(d) 考虑 dropbear 只允许 key 登录，或刷机后首次启动强制改密；(e) 已刷出的镜像若用默认密码，必须立即改密。

### P0-3 4399 只有单 Token 鉴权，无 mTLS；`CHANGE_ME` 默认 Token 可直接上线
- **修复状态（2026-11-17，已修复）**：`transport.ValidateToken`（空 / <16 位 / 弱值表即错，错误含 `openssl rand -hex 16`）+ `tlscert.LoadOrCreateMTLS`（自有 CA + CA 签发的服务端证书 + 共享客户端证书）+ `transport.SetClientCAs`（`RequireAndVerifyClientCert`），`main.go` 已接线（启动时 Fatal 弱 Token、mTLS 服务）；`nas-install.sh` 预检查拒绝弱 Token；客户端 `connect_once` 建连前拒弱 Token，默认 Token 改 `""`（fail closed）。证据：`go test ./internal/transport/` 8 passed、`./internal/tlscert/` 6 passed、`cargo test` 46 passed；真二进制冒烟：无客户端证书握手被拒（`tls: client didn't provide a certificate`），带 CA 签发证书握手成功（TLSv1.3，Verify return code 0），弱 Token 启动即 Fatal（本轮重跑）。剩余：客户端证书随固件下发到两台音箱的路径待真机验证；旧自签证书被覆盖后指纹需同步更新 `client.toml`；`CheckOrigin` 恒 true 与 deviceID 顶替（P1-3）未修。
- **文件:行号**：PLAN `NFR-02`（“Client→Hub mTLS + Token；4399 不裸奔”）；`hub/internal/transport/transport.go:150-151,206`（`New(token…)` 注明“token 为空表示不鉴权（只允许测试用）”，`handleWS` 仅 `hello.Token != s.token` 比较，非恒定时间）；`hub/internal/transport/transport.go:173-182`（`Serve()` 只有服务端证书，无 `ClientAuth`）；`client/src/transport.rs:187,203`（两种分支均为 `.with_no_client_auth()`）；`hub/cmd/hub/main.go:222-225`（启动只拒绝空 Token，`CHANGE_ME` 照常通过）；`client/src/config.rs:14-15`（默认 Token `CHANGE_ME`）；`deploy/.env.example:15`（`OPEN_XIAOAI_TOKEN=CHANGE_ME`）；`deploy/nas-install.sh:45-52`（预检查只查三 Token“非空”，`CHANGE_ME` 能通过）。
- **问题**：三层落差：(1) PLAN 要 mTLS，实际双向均为 `with_no_client_auth`——Hub 不验证客户端证书，客户端只验服务端指纹（且指纹可选，见 P1-10），Token 是唯一认证因素；Token 一旦泄露（见 P0-4）或被猜中，攻击者即可冒充任意音箱、收发语音、触发 HA/提醒/紧急呼叫；(2) 默认值 `CHANGE_ME` 是公开的：仓库、`.env.example`、客户端默认配置三处一致，操作员若照抄示例部署，Token 等于公开。
- **为什么重要**：4399 是全屋语音与家电控制的入口，单因素 + 公开默认值 = 未鉴权即有控制权。
- **建议修复**：(a) 启动时拒绝弱 Token：长度 <16、等于 `CHANGE_ME`/`secret`/`password` 等常见值即 Fatal，并给出生成命令（`openssl rand -hex 16`）；`nas-install.sh` 同样拦截；(b) 按 PLAN 补 mTLS：Hub 生成客户端证书并随固件/OTA 下发，服务端 `tls.RequireAndVerifyClientCert`，客户端带证书连接；做不到 mTLS 之前，至少把 Token 当作机密轮换并在文档中明确“当前只有 Token，无 mTLS，与 PLAN 有差距”；(c) Token 比较改 `crypto/subtle.ConstantTimeCompare`。

### P0-4 自动发现向全网段广播真实 Token：局域网任意 :4399 监听者可白嫖 Token
- **修复状态（2026-11-17，已修复）**：扫描默认关闭（`Enabled=false` 时 `Scan` 直接拒绝）；先匿名探测（只建 TCP + TLS 握手取叶子证书指纹，不发应用层数据、不发 Token），指纹在白名单内才建 wss 连接发 Token；端口取 `cfg.Hub.Listen`，不写死 4399；`main.go` 只在显式开启 + 配了指纹白名单 + TLS 开启时才扫描。证据：`go test ./internal/discovery/` 9 passed（本轮重跑）。剩余：P1-44 未修（音箱是 WS 客户端、不监听 `:4399`，生产扫描仍找不到设备）；真机发现实测待定。
- **文件:行号**：`hub/internal/discovery/discovery.go:24-26`（`tlsConfigInsecure = tls.Config{InsecureSkipVerify: true}`）；`:47-90`（`ProbeModel()`：对每个候选 IP 先建 WS 连接，立即 `WriteJSON({"type":"hello","token": s.Token, …})`，不做任何对端身份校验）；`:140-175`（`Scan()`：ARP 邻居 + 网段展开，逐个试 `lx06`/`oh2p`）；`hub/cmd/hub/main.go:460`（启动时 `discovery.New(4399, token)`，`Scanner.TLS` 从未置 true——即使 Hub 是 wss，扫描也走明文 ws）；`:456-477`（启动自动扫一次，`ScanCIDR` 默认本机 /24）；`New(4399, token)` 写死端口，忽略 `cfg.Hub.Listen`。
- **问题**：Hub 启动即把真实 `OPEN_XIAOAI_TOKEN` 发给网段内**每一个** IP 的 :4399 端口，且先发 Token、后看对方是不是真音箱。局域网内任何人在 :4399 起一个假 WS 服务（或一台被入侵的设备恰好占了该端口）即可直接收到 Token；拿到 Token 就能以音箱身份连 Hub（P0-3）。`InsecureSkipVerify` 与 `TLS=false` 还让扫描全程明文，Token 可被抓包。另：扫描大概率扫不到真设备（ws 打 wss 会失败），属于“既不安全又不好用”。
- **为什么重要**：Token 是唯一认证因素（P0-3），而发现流程把它当作可广播的探测载荷，等于把家门钥匙挨家挨户递过去问“你是音箱吗”。
- **建议修复**：重做发现协议：(a) 先做无 Token 的匿名探测（如只发 `{"type":"probe"}` 或 TCP 指纹），对端返回可验证的设备标识/证书指纹后，**只对指纹匹配的候选者**发 Token 做 hello；或改用 mDNS + 设备证书 pin；(b) 扫描默认关闭，需在配置里显式开启并限定 `scan_cidr`；(c) 删 `InsecureSkipVerify`，扫描走 wss + 指纹校验，端口取 `cfg.Hub.Listen` 而非写死 4399；(d) 扫描失败只记日志，不把 Token 写进任何日志/错误信息。

### P0-5 官方 OTA 经明文 HTTP 下载、信任服务端自带的 MD5：新版本可被中间人替换固件
- **修复状态（2026-11-17，已修复）**：接口改 `https://api.miwifi.com/…`；`EXPECTED` 白名单为强制门（不在白名单的 `(model, version)` 拒绝下载）；下载链接强制 `https` + host 白名单；SHA256 白名单校验（MD5 只作兼容参考）；带超时的流式下载，先下 `.tmp`、校验通过再 rename，失败删临时文件。证据：`python3 -m pytest tests/ -q` 61 passed（本轮重跑）。剩余：其它版本无白名单锚点（新增版本需人工核实 SHA256 后更新 `EXPECTED`）；P1-8（`unpack.sh`/`patch.sh` 哈希校验）未修。
- **文件:行号**：`firmware-patch/scripts/download-ota.py:29`（`API = "http://api.miwifi.com/…"`，明文 HTTP）；`:60-61`（`fetch_ota_info()` 用 `urllib.request.urlopen(url, timeout=30)` 取 JSON，含 `link/hash/size`）；`:86-99`（`main()`：`urlretrieve(link, out)` 下载，`verify_file(out, md5, size)` 只比对**服务端同一条 HTTP 响应里给的 md5**）；`:31-34,94-99`（仅 `lx06 1.94.13` 与 `oh2p 1.58.6` 有硬编码 `EXPECTED` 二次比对，其它任意版本无此保护）；`:90`（`urlretrieve` 直写最终路径，下载中断或 MD5 失败后截断的 `.bin` 仍留在 `work/`）；`firmware-patch/scripts/unpack.sh:47-58`、`patch.sh:61-64`（对 `.bin`/`root.squashfs` 不做任何哈希校验，`find | head -1` 任选其一）。
- **问题**：信任链是自指的：MD5 由同一条未加密信道上的同一台服务端提供，中间人可同时替换 `link`（指向恶意固件）与 `hash`（与恶意固件匹配），`verify_file()` 会放行。MD5 本身还是弱哈希（碰撞攻击成熟），且 `urlretrieve()` 对 `link` 的 scheme/host 无白名单——服务端返回 `file://`/`ftp://`/内网 URL 也会被跟随下载（`urlretrieve` 无超时）。`EXPECTED` 只保护两个已验证版本；一旦操作员下载其它版本（如 `1.94.14`），即无任何独立锚点。之后 `unpack.sh`/`patch.sh` 对 `.bin` 不再做任何哈希校验，恶意固件会一路进补丁、进刷机。
- **为什么重要**：这是刷机供应链的源头污染：一次成功的中间人替换 = 持久化 rootkit 固件刷进音箱，且后续所有本地校验都会认为“MD5 已通过”。
- **建议修复**：(a) 接口改 `https://`（若小米接口不支持 HTTPS，则必须把 `EXPECTED` 白名单作为强制门：不在白名单的 `(model, version)` 拒绝下载，新增版本需人工核实 MD5/SHA256 后更新白名单再下）；(b) 把 MD5 升级为 SHA256 白名单（MD5 只作兼容参考）；(c) `link` 强制白名单 host（小米官方域名）与 `https://` scheme，拒绝 `file://` 等；(d) `urlretrieve` 改带超时的流式下载并校验下载后大小，先下到 `.tmp`，校验通过再 rename，失败删临时文件；(e) `unpack.sh` 入口增加对 `.bin` 的白名单哈希校验，校验失败拒绝解包。

### P0-6 没有刷机流程：boot1 回滚无任何保障，首刷写错分区即变砖或丢回滚
- **修复状态（2026-11-17，已修复）**：补书面 runbook（`firmware-patch/FLASH_RUNBOOK.md`：只写非当前槽 system 分区、刷前 `flash-check.sh`、读 `boot_part`、备份 boot1、刷后校验、回滚演练）+ `flash-check.sh` 干跑核对。证据：`./scripts/flash-check.sh lx06 1.94.13` 与 `oh2p 1.58.6` 均 exit 0，`干跑检查通过`（本轮重跑）。剩余：真机步骤全部 BLOCKED（读 `boot_part`、备份分区、刷后校验、回滚演练）。
- **文件:行号**：`firmware-patch/README.md:11`（刷机只有一句话“LX06 拆机走 Micro USB（Amlogic Flash Tool / macOS flash-tool）”）；全仓库除 `work/` 外 grep `Amlogic|flash-tool|nandwrite|mtd|boot_part|fw_env` 仅此一行——没有任何刷机命令、脚本、分区目标、备份步骤。原厂 A/B 保护（已核对的官方固件 `bin/boardupgrade.sh:224-259` 写非当前槽、`:203-213` 翻转 `boot_part`）只在走 `flash.sh` 的升级路径生效。
- **问题**：交付物只有一个 `root-patched.squashfs`，没有“写哪个分区、写之前如何备份/校验 boot1、写之后如何验证”的任何步骤。手动用烧录工具写错 MTD（如写到当前 system 分区、写到 bootloader 分区）会直接毁掉回滚或变砖。`TEST_REPORT.md` 关口 1 只写了目标（“刷入 boot0，保留 boot1 原厂回滚”），没有可执行流程。
- **为什么重要**：首刷是全项目最高风险操作；一旦写错不可逆。
- **建议修复**：刷机前必须补书面 runbook：刷前用 `fw_env` 读 `boot_part`、备份 boot1（kernel + rootfs）镜像并校验、明确写非当前槽的命令、刷后校验分区内容与 `boot_part`、做一次回滚演练；工具与命令要在文档里钉死，禁止口头操作。

---

## P1 — 功能性错误（46 项）

### P1-1 Wyoming `readEvent()` 对 `payload_length` 不做任何校验：类型断言 panic、负数 panic、超大值 OOM
- **修复状态（2026-11-17，已修复）**：`parsePayloadLength` 校验（非数字 / 负数 / 非整数 / 超 1MB 一律报错）；`readHeaderLine` 限长读；`readEvent`/`writeEvent` 全程 10s 读写 deadline；`Transcribe`/`Synthesize` 总量上限。本轮另修了 3 处 `defer SetReadDeadline/SetWriteDeadline` 未判错（errcheck，`_ =` + 注释）。证据：`go test ./internal/wyoming/` 7 passed（本轮重跑，含 `TestReadEventRejectsOversizeHeaderLine`）。
- **文件:行号**：`hub/internal/wyoming/wyoming.go:39-56`（`readEvent()`：`n := int(l.(float64))` 后 `payload = make([]byte, n)`，再 `io.ReadFull`）；`:23-27`（`Dial()` 只有连接超时，连接后 `readEvent` 无任何读写 deadline）；`:44-45`（`ReadBytes('\n')` 无长度上限）。
- **为什么重要**：`payload_length` 不是 JSON 数字时类型断言直接 panic；负数时 `make` panic；极大值（如 `1e9`）时一次性分配，Hub OOM。调用链上没有任何 `recover()`，一次 panic 即 Hub 进程退出（容器 `restart: unless-stopped` 会重启，但会丢全部会话/提醒调度状态）。Wyoming 是明文 TCP、无鉴权，局域网攻击者冒充/劫持 STT/TTS 服务即可稳定触发；即使无攻击者，异常的 Piper/Whisper 响应同样能触发。
- **建议修复**：类型断言用 `, ok` 模式、拒绝非数字/负数/非整数、设上限（如音频 chunk ≤ 1MB，超过即断开并报错）；`readEvent` 全程设读写 deadline（如 10s），`ReadBytes` 改用限长读取；`Transcribe/Synthesize` 循环加总字节上限与整体超时；主循环顶层加 `recover` 兜底。

### P1-2 STT/TTS 为 nil 时首句语音直接 panic；单句语音无上限累积直至 OOM
- **修复状态（2026-11-17，已修复 nil panic 部分）**：`voice.answer()` 开头判 nil，返回明确错误（`STT 不可用（Whisper 未连接），请稍后再试` / `TTS 不可用…`）；`main.go` 的 `OnAudioFrame`/`OnAudioEnd`/`speakText` 遇语音服务错误播一句降级回复，`speakText` 在 TTS 为 nil 时只记日志、不 panic。证据：`go test ./internal/voice/` 10 passed（本轮重跑）。剩余：单句语音上限（`Frame` 的 `u.pcm` 无上限累积）是否已加未在本轮核对。
- **文件:行号**：`hub/internal/voice/voice.go:133-160`（`answer()`：`p.stt.Transcribe(...)`（:138）与 `p.tts.Synthesize(...)`（:155）均无 nil 检查）；`hub/internal/voice/voice.go:83-110`（`Frame()`：`u.pcm = append(u.pcm, pcm...)`（:95），只有 `speech && silenceMs>=800ms` 才结算）；`hub/cmd/hub/main.go:272-285`（Wyoming 拨号失败只记日志，`stt/tts` 保持 nil 接口，`Pipeline` 照常创建并接入 `OnAudioFrame`）。
- **为什么重要**：STT 或 TTS 任一拨号失败（NAS 上 Whisper/Piper 未起、端口被占——部署常见情况），Hub 照常启动、照常接受唤醒与音频，第一个说完话的用户即触发 nil 接口调用 panic，Hub 崩溃；即使 STT/TTS 正常，只要说话方持续出声（长语音、音乐/电视声被当成语音、恶意设备持续推流），`u.pcm` 无任何时长/字节上限地增长直至 OOM。`silenceMs` 每帧只 +20ms，且只在 `rms<=阈值` 时累计，持续语音可无限延长。
- **建议修复**：`answer()` 开头判 nil 并返回明确错误（“STT/TTS 不可用”，由 `OnAudioFrame` 播报而非 panic）；`main.go` 在 STT/TTS 为 nil 时应让语音管线走降级回复；`Frame()` 加单句上限（如 30s / 约 960KB PCM，超限即强制结算或丢弃并记日志）；`Pipeline` 加总并发上限。

### P1-3 4399 传输层无帧大小/连接数/速率限制：Token 可暴力破解，已鉴权设备可打满 Hub，设备身份可被顶替
- **文件:行号**：`hub/internal/transport/transport.go:135-136`（`upgrader.CheckOrigin` 恒 true）；`:174-182`（`Serve()`：`http.Server{Handler: mux}`，无 `ReadTimeout/ReadHeaderTimeout/MaxHeaderBytes`，无 `SetReadLimit`）；`:192,247`（`ws.ReadMessage()` 直接读，无大小上限）；`:206`（hello 失败只回 `hello_reject` 即关连接，无延迟、无计数、无封禁）；`:214-232`（`deviceID = model + "-" + IP`，同 IP 的新连接直接 `old.Close()` 踢掉旧连接）。
- **为什么重要**：无 `SetReadLimit`：gorilla 默认无上限，单条超大文本/二进制帧可把 Hub 内存打满；无速率限制：Token 只有 hello 一道关，且失败无延迟/锁定，若 Token 是短/弱口令（P0-3 的 `CHANGE_ME` 类），局域网可高速暴力破解；`CheckOrigin: true` 允许任意网页跨域发起 WS 握手；`deviceID` 仅由型号+源 IP 派生：同一 NAT/IP 后的两台设备会互相踢下线，持 Token 的攻击者从同一出口 IP 连接即可踢掉真音箱并顶替其身份收语音、收 TTS。
- **建议修复**：`SetReadLimit`（如文本 64KB、二进制音频帧 64KB）；hello 失败加指数延迟与单 IP 计数（短时多次失败即临时封禁该 IP）；`deviceID` 改为 hello 里设备提供的稳定唯一 ID（如 MAC/序列号 + Token 绑定）并做冲突时拒绝新连接而非静默踢旧连接；`CheckOrigin` 收紧为仅允许空/局域网 Origin；`Serve` 加超时。

### P1-4 Compose 把无鉴权的 Wyoming/MQTT/MA/网易云 API 全量发布到局域网，且全部用 `:latest`、无 digest
- **文件:行号**：`deploy/docker-compose.yml:36-41`（whisper `rhasspy/wyoming-whisper:latest`，`"10300:10300"`）；`:47-52`（piper `:latest`，`"10200:10200"`）；`:57-64`（mosquitto `eclipse-mosquitto:latest`，`"1883:1883"`，**未挂载任何配置文件/密码文件**）；`:68-71,79-82`（snapserver/mpd `network_mode: host`）；`:91-95`（music-assistant `ghcr.io/music-assistant/server:latest`，`"8095:8095"`）；`:102-106`（netease-api `binaryify/netease_cloud_music_api:latest`，`"3000:3000"`）；`deploy/Dockerfile.hub:8,18`（`FROM debian:bookworm-slim` 无 digest、无 `USER`，容器以 root 运行，`EXPOSE 4399 8080`）。
- **为什么重要**：Wyoming 协议本身无鉴权，:10300/:10200 发布到局域网后任何人可用免费 STT/TTS，还可冒充 Wyoming 服务端攻击 Hub（P1-1）；mosquitto 未挂配置：是否允许匿名取决于镜像默认配置，仓库里无任何密码/ACL 文件可供核查（疑似，需实测确认），若默认匿名，局域网任何人可订阅/发布 MQTT；MA 与网易云 API 同样无鉴权发布，任何人可调 MA 播歌、调网易云接口；全部镜像 `:latest` 且无 digest：每次 `docker compose pull`（`nas-install.sh` 会执行）都可能拉到不同版本，供应链投毒/破坏性更新无法发现；hub 容器 host 网络 + root 运行，容器逃逸面与宿主网络完全打通。
- **建议修复**：Wyoming/MQTT/MA/网易云只监听 `127.0.0.1` 或放进内部 Docker 网络，不向局域网发布端口；mosquitto 挂载显式配置并开密码认证（或不用就删掉，见 P1-37）；全部镜像钉死 digest（`image:…@sha256:…`）；hub 容器加 `user: "1000:1000"`、`read_only: true`（除数据卷外）、`cap_drop: [ALL]`，去掉不必要的 host 网络；加 `mem_limit`/`cpus` 资源上限。

### P1-5 HA 经明文 `ws://` 连接，`HASS_TOKEN` 明文传输；`Dial()` 首次读消息无超时可永久挂起
- **文件:行号**：`hub/config.yaml:30-31`（`home_assistant.url: "ws://192.168.1.10:8123"`）；`hub/internal/ha/ha.go:47-67`（`Dial()`：`websocket.Dialer{HandshakeTimeout: timeout}` 建连后直接 `conn.ReadMessage()` 等 `auth_required`，无读 deadline；随即 `WriteJSON({"type":"auth","access_token": token})`）；`:63-86`（不校验 token 是否为空，空 token 也会发一次 auth 再失败）。
- **为什么重要**：HA 的长期访问令牌是“拿到即完全控制 HA”的凭证，却走明文 `ws://` 在局域网传输，可被抓包；`config.Validate()` 不校验该 URL 的 scheme。另 `Dial()` 在握手后的两次 `ReadMessage()` 均无 deadline：对端只建连不发消息时 `Dial` 永久挂起，调用方 `main.go:238` 会卡在启动流程里（5s 的 `HandshakeTimeout` 只管握手，不管之后）。
- **建议修复**：HA URL 默认改 `wss://`（HA 需配证书或走本地可信网络时明确标注风险），`Validate()` 拒绝非 `ws(s)://…/api/websocket` 的地址；`Dial()` 启动前判空 token，握手后每次 `ReadMessage` 前设 deadline（如 5s）；考虑 HA 连接失败后后台重试而非启动时同步阻塞（见 P1-27）。

### P1-6 LLM / MA / 网易云密钥与请求全走明文 HTTP，无 URL 校验
- **文件:行号**：`hub/internal/agent/agent.go:57-85`（`New()` 存 `APIKey`，`Chat()` 对 `c.BaseURL+"/chat/completions"` 发 POST，`Authorization: Bearer <APIKey>`（:77-78），`http.Client{Timeout: 120s}`，无 TLS 校验定制、无 URL 校验）；`:115-116`（`ChatStream()` 同样）；`hub/internal/music/music.go:272-305`（`MAClient.command()`：POST `baseURL+"/api"`，有 token 才加 Bearer，`NewMA()` 注明“token 为空表示无认证”（:281-282））；`:186-260`（`NeteaseClient`：`Search/SongURL/Lyric` 均为明文 GET，无鉴权）；`hub/config.yaml:36-39,44-45`（Agent 与网易云默认均为 `http://` 内网地址）。
- **为什么重要**：LLM 密钥以 Bearer 明文跑在局域网 HTTP 上，抓包即得；且 `BaseURL` 来自 `config.yaml`（可经 P0-1 的 Web PUT 改写），攻击者改一个地址即可同时收走密钥与全部对话文本。MA 的 token 为空即无认证是默认值（`main.go:247` 的 `MA_TOKEN` 未设即空），网易云 API 全程无鉴权。`agent.Chat()` 错误返回里还把对端响应体原文拼进错误信息（:86,124，`io.ReadAll` 无上限），可能把上游错误里的敏感内容带进 Hub 日志。
- **建议修复**：内网 Agent/MA 一律改 `https://`（自签证书 + 指纹 pin，与 4399 同款做法）或走 Docker 内部网络 + 回环；`Validate()` 对 `base_url` 做 scheme/host 白名单校验；`MA_TOKEN` 为空时记警告并拒绝把 MA 当作可信端；错误信息不回显上游响应体全文（截断/脱敏，见 P2-7）。

### P1-7 MPD `PlayURL()` 对网易云返回的 URL 不做换行过滤：可注入任意 MPD 命令
- **文件:行号**：`hub/internal/music/music.go:438-468`（`MPDClient.cmd()`：`fmt.Fprintf(c.conn, format+"\n", args...)`，按行读到 `OK/ACK` 为止；`PlayURL(url)`：`c.cmd("add %s", url)`（:465））；`:216-246`（`SongURL()`：取网易云 API 返回的 `data[0].url` 原样返回，无任何校验）；`hub/internal/router/router.go:300-330`（`music()`：`SongURL` 结果直接进 `MPD.PlayURL(u)`）。
- **为什么重要**：MPD 是行协议，`add %s` 的 `%s` 参数虽不是 format 串（`%` 安全），但 URL 里的 `\n`/`\r` 不会被转义：若网易云 API 被冒充/劫持（它是明文 HTTP，见 P1-6），或某首歌的 URL 本身含换行，攻击者可在 `add <url>\n<任意 MPD 命令>\n` 后注入命令（如 `password`、`clear`、`play` 之外的管理命令，取决于 MPD 的 ACL）。`cmd()` 的读写 deadline 只有 5s，无命令白名单。
- **建议修复**：`PlayURL()` 校验 URL：只允许 `http(s)://`、拒绝含 `\r`/`\n`/控制字符的 URL；`cmd()` 对参数做白名单转义或改用 MPD 的参数化写法；网易云 API 改 HTTPS/内网可信源（P1-6）。

### P1-8 `unpack.sh` / `patch.sh` 对 `.bin` / `root.squashfs` 不做任何哈希校验，`find … | head -1` 任选其一；陈旧解包产物可被复用
- **文件:行号**：`firmware-patch/scripts/unpack.sh:30`（只判 `.bin` 文件存在）；`:36-60`（`"$UNBLOB" -e "$OUT-extract"` 失败只警告继续；`find "$OUT-extract" -name 'root.squashfs' | head -1` 取产物；全程不删旧 `$OUT-extract`/`$OUT`，不校验 `.bin` 的 MD5/大小；`mico_version` 只 `cat` 打印，不比对）；`:69`（`unsquashfs -f` 不清理目标目录）；`firmware-patch/scripts/patch.sh:61-64`（`ORIG_SQFS` 先取 `$DIR/root.squashfs`，否则 `find "$DIR"-extract -name 'root.squashfs' | head -1`）；`:73-91`（`file` 读压缩方式/blocksize 后直接 `mksquashfs`）。
- **为什么重要**：`download-ota.py` 的 MD5 校验只在下载当时做一次，`unpack.sh` 完全可以对任意 `.bin`（`OTA_BIN` 环境变量任意指定）直接解包、打补丁、打包，中间无任何“这个包是不是官方包”的再校验。若 `.bin` 在下载后被替换/损坏，或 unblob 从一个被篡改的包里 carve 出多个 `root.squashfs`，`head -1` 的挑选是任意的（字典序/遍历序），可能选中诱饵文件。`$OUT-extract` 不清理，旧的 `root.squashfs`/`mico_version` 会留在原地被 `find | head -1` 捡走；`unsquashfs -f` 不删除新包里已不存在的旧文件，旧版文件可残留进 rootfs。两处都不核对 `mico_version` 与入参 `$VERSION` 是否一致。
- **建议修复**：`unpack.sh` 入口对 `.bin` 做白名单 SHA256/MD5 校验（与 `download-ota.py` 的 `EXPECTED` 同源），失败拒绝解包；开头 `rm -rf "$OUT-extract" "$OUT/rootfs"`；`find` 改为“恰好一个，多个即失败”；unblob 非零退出时除已知的缺提取器场景外应失败（白名单化警告原因）；`patch.sh` 打包前校验原始 `root.squashfs` 的哈希与大小，并记录进产物校验报告；核对 `mico_version` 包含入参 `$VERSION`，不符即失败。

### P1-9 OTA 的验证与回滚在生产代码中从未被调用：失败的 OTA 永远停在 `pushing`，回滚无人验证
- **文件:行号**：`hub/internal/ota/ota.go:92-131`（`MarkRebooted()`、`ConfirmHello()`、`Sweep()` 均已实现：30s 内新版本未 hello 即 `failed`；`MarkDownloading` 是空函数体）；`hub/cmd/hub/main.go` 全文（grep：`ConfirmHello`、`MarkRebooted`、`Sweep`、`MarkDownloading` 在 `main.go` 与 `transport` 中**零调用**；`otaMgr` 只在 `web.go:132` 的 `Push()` 与 `main.go:435` 的 `PushOTA` 下发处出现）；`hub/internal/transport/transport.go:186-232`（`handleWS` 的 hello 成功后未调 `ConfirmHello`，未把版本号与 OTA 任务比对）；`client/src/main.rs:128-132`（`HubMsg::Ota` 只记日志 `"ota available (Phase 4, ignored)"`）。
- **为什么重要**：与 TEST_REPORT 的差异：TEST_REPORT 只说“OTA 失败自动回滚 boot1”与“FR-11 一键 OTA”待真机验证，未指出 Hub 侧的验证闭环在生产代码里根本没接线。现状是：OTA 下发后，无论设备是否重启、是否用新版本 hello、是否超时，任务永远停在 `StatusPushing`；`Sweep()` 无定时器调用，`ConfirmHello()` 无 hello 钩子，设备端也未实现“30s 内重新 hello”。一旦客户端 OTA 接通，失败的升级不会被 Hub 发现，操作员在 Web 面板看到的永远是 `pushing`，无法区分“进行中”与“已变砖”。`otaAPI` 在 `PushOTA` 为 nil 时不应返回 200（当前 `web.go:135-139` 只在非 nil 时才下发，却照常返回成功）。
- **建议修复**：在 `transport.handleWS` 的 hello 成功处调 `otaMgr.ConfirmHello(deviceID, hello.Version)`；`main.go` 起定时器每秒调 `otaMgr.Sweep(time.Now())` 并记日志/告警；客户端 OTA 接通时实现“新版本启动后 30s 内重新 hello + 失败回滚 boot1”的闭环测试；Web 面板暴露任务状态查询接口；或先把 FR-11 标注为未实现并从 Phase 4 验收中摘出。

### P1-10 客户端 TLS 指纹是可选的，且 `tls=false` 可在生产配置里合法存在：降级与中间人只有一句 warn
- **文件:行号**：`client/src/config.rs:73-80`（`tls: bool` 默认 true，但可配 false；`hub_ca_fingerprint: Option<String>`，未配即 `None`）；`client/src/transport.rs:171-204`（`build_tls_config()`：有合法指纹才用 `FingerprintVerifier`，无指纹时回退到系统根证书 + 一句 `warn!`，自签 Hub 证书会验不过；指纹格式非法才报错）；`client/src/main.rs:203-207`（`tls=false` 只 `warn!`，`token == "CHANGE_ME"` 只 `warn!`，两种情况照常启动并无限重连）；`client/src/transport.rs:462-476`（`tls=true` 但 URL 是 `ws://` 时自动升级为 `wss://`，反向——`tls=false` 时不做任何阻止）；`client/src/transport.rs:119-128`（`FingerprintVerifier::verify_server_cert()` 忽略 `_server_name` 与 `_now`，只比指纹）。
- **为什么重要**：安全默认值是反的：生产最关键的 pin（Hub 自签证书指纹）是可选字段，不配只警告；`tls=false` 是合法配置值，不配指纹的客户端在生产网络里要么连不上（自签证书不在系统根里），要么被引导去关 TLS 走明文。指纹的下发路径也不存在：Hub 启动时把指纹打到日志（`main.go:418`），操作员需手工抄进每台音箱的 `client.toml`，无 OTA/自动下发，容易抄错或干脆不配。
- **建议修复**：发布构建把 `hub_ca_fingerprint` 改为必填（缺失即拒绝启动并报错），`tls=false` 只允许 `debug` 构建/显式 `--insecure-test` 标志；Hub 提供指纹的安全下发路径（首次配对时经已认证通道下发，或刷机时写进镜像）；`FingerprintVerifier` 应同时校验有效期与 SAN（当前忽略 `_now`/`_server_name`）。

### P1-11 读超时 60 秒后杀死所有 Client 连接，且无任何 Ping 保活
- **文件:行号**：`hub/internal/transport/transport.go:229-234`（hello 成功后先清掉读超时，又一次性设 `SetReadDeadline(now+60s)`，之后读循环里再也不续期；注释写“靠 ping/pong 保活”）；`client/src/transport.rs:402-440`（Client 只回 Pong，从不主动发 Ping；`next_raw`/`next_msg` 里无 Ping 发送）。
- **为什么重要**：Go 的 deadline 是绝对时间点，不是“每次读后顺延”。服务端从不发 Ping（无 ping 循环），Rust 客户端也从不发 Ping。于是只要连接存活满 60 秒，下一次 `ReadMessage` 即超时，连接被断开——与客户端是否在发 30 秒状态上报无关（状态帧不续 deadline，只有 Pong 才会，而 Pong 永远不会来）。每台音箱每 60 秒断线重连一次，`OnDisconnect` 删会话、注册表反复上下线；60 秒节律的抖动会系统性污染仲裁、连续对话与对讲。`TEST_REPORT.md` 的端到端只跑了数秒，未覆盖 60 秒边界。
- **建议修复**：二选一：（a）服务端每 20 秒发 Ping，收到 Pong 续 60 秒 deadline（现有 `SetPongHandler` 已写好续期逻辑，只缺发 Ping 的 goroutine）；（b）读循环每次成功读到消息后 `SetReadDeadline(now+60s)`。并补一个“连接存活 > 90 秒”的集成测试。

### P1-12 旧连接的 deferred 清理按 deviceID 无条件删除，会误删刚重连的新连接
- **文件:行号**：`hub/internal/transport/transport.go:213-244`（`handleWS`：同 deviceID 新连接先 `old.Close()`，defer 里 `delete(s.conns, deviceID)` + `SetOnline(deviceID, false)`）。
- **为什么重要**：defer 按 deviceID 删表，不校验“被删的是否还是我这条连接”。旧连接的读循环退出时，若新连接已注册（重连退避最快 100ms，见 `client/src/transport.rs` 的 `backoff_delay`），旧 defer 会删掉新连接的表项并把设备标离线；新连接的读循环仍在跑、消息仍进 Handler，但 `server.Conn(deviceID)` 查不到它，`wake_win`/`wake_lose`/TTS 帧全部发不出去，设备永久“假在线、收不到回复”，直到下一次重连。与 P1-11 的 60 秒断线叠加后，此竞态会被高频触发；同型号 + 同 NAT 出口 IP 的两台设备、自动发现探针也会触发同一竞态。失败模式是静默的：Hub 日志无任何异常，设备侧只表现为“唤醒后没人理”。
- **建议修复**：`Conn` 加唯一连接 ID（或存指针比对），defer 里只当 `s.conns[deviceID] == c` 时才删；`SetOnline(false)` 同理只当被删的是当前连接才执行；或把“关闭旧连接”改为“拒绝新连接”并由客户端退避重试。

### P1-13 仲裁只看“到达时间”，忽略客户端时间戳、无冷却期：两台先后唤醒会各赢一次
- **文件:行号**：`hub/internal/arbitration/arbitration.go:33-90`（`Arbitrate`/`Arbiter` 全程不用 `WakeEvent.Timestamp`）；`hub/cmd/hub/main.go:76-94`（`OnWake` 用 `time.Now()` 覆盖时间戳，`Wake.TsMs` 解析后即丢弃）。
- **为什么重要**：窗口从第一条到达的 wake 才开始计时；第二台若因 KWS 检出延迟、网络抖动晚到 250ms 以上，会开启第二个窗口并独自获胜。于是“两台都回”这一 PLAN 明确列为唯一失败模式的情况会在真实双唤醒下稳定出现——不是小概率。`Timestamp`/`TsMs` 字段存在却全程不用，等于仲裁完全没有“同一声唤醒”的概念。测试只覆盖了 50ms 内到达的两条事件，未覆盖跨窗口到达。另：同一台设备一次唤醒上报两次（如 KWS 抖动）会在 `pending` 里占两席；赢家在窗口关闭到发 `wake_win` 之间断线时，无人补位（输家已收到 `wake_lose`），整轮唤醒无人应答。
- **建议修复**：仲裁器维护“上次裁决时间 + 冷却期”（如裁决后 1 秒内到达的 wake 视为同一声唤醒的迟到副本，直接判 lose 或丢弃）；或以客户端 `TsMs`（需先做时钟偏差容忍，如 ±500ms）聚类同一声唤醒；窗口内按 `deviceID` 去重（保留最大 RMS）；赢家发送失败时把次优者提升为赢家并补发。选其一并补跨窗口到达的测试。

### P1-14 KWS 上报的 RMS 是命中帧的单帧值，近处设备可能报静音值而输掉仲裁
- **文件:行号**：`client/src/kws.rs:141-175`（`feed` 每帧覆盖 `last_rms_db`，命中时 `WakeEvent.rms_db = self.last_rms_db`）。
- **为什么重要**：`WakeEvent` 的 RMS 取的是“检出命中的那一帧”的 RMS，不是唤醒词期间的峰值/均值。sherpa 检出常发生在关键词结束后的若干帧（`sherpa.rs` 的 `is_ready`/`decode` 循环 + trailing blanks），此时麦克风已是词尾静音，RMS 可跌到 -120dB 下限。于是离得近、喊得响的设备可能上报 -120dB，而远处设备若检出时刻恰在词内，反而报 -20dB 获胜。仲裁的唯一依据就是这个 RMS；上报值系统性失真会让“近处赢”变成随机。代码层面可确认（单帧取值），真机上检出延迟几帧是否足以跌到静音需实测，故时延部分标“疑似，需真机确认”，但“应取词内峰值而非命中帧单帧值”的修复方向是确定的。
- **建议修复**：`Kws` 维护最近 N 帧（如 500ms 窗口）的 RMS 峰值，命中时上报该峰值；或在 `feed` 里命中后回溯 `pending` 帧重算。补单测：词后喂 10 帧静音再命中，断言上报 RMS 仍为词内峰值。

### P1-15 对讲在生产代码里零接线：无任何触发入口，且超时清理从不运行
- **文件:行号**：`hub/internal/intercom/intercom.go:69-226`（`Start`/`StartBroadcast`/`SetFullDuplex`/`EchoCancel`/`Sweep`）；`hub/cmd/hub/main.go` 全文无任何调用（仅 `Forward`/`End` 被 `OnAudioFrame`/`OnAudioEnd` 调到）；`hub/internal/router/router.go` 全文件无“呼叫/对讲/广播”分支（`grep 呼叫` 仅 FR-15 紧急呼叫）；`hub/cmd/hub/main.go:183-189`（`OnDisconnect` 只 `End(deviceID)`，即只清以该设备为发起方的会话）。
- **为什么重要**：与 `TEST_REPORT.md` 的表述有实质差异：报告写“对讲全双工已实现、半双工已测”，实际是包内单测通过、生产接线为零——FR-04（点对点呼叫 + 全屋广播）当前不可用。没有任何 Router 分支、RPC 方法、Web API 能调用 `Start`/`StartBroadcast`——语音说“呼叫卧室”会落到 Agent 闲聊，`intercom.Manager` 永远建不出会话。`Sweep`（5 秒空闲超时）在 `main.go` 里没有 goroutine 调用，`LastFrame` 的更新因此毫无作用。另：目标方断线时 `OnDisconnect` 只 `End(断线方)`，若断线方是目标而非发起方，会话永不结束；`participantBusy` 会永久拒绝该发起方的新呼叫（无 Sweep 兜底）；目标方断线、Client 崩溃未发 `audio_end`、网络丢帧超过 5s 空闲时，会话永远留在 `sessions` 里，双方都无法再发起对讲，只能重启 Hub。
- **建议修复**：先接通最小触发路径（Router 增加“呼叫/对讲 + 房间”分支调 `Start`，`audio_end` 调 `End`），并在 `main.go` 起一个每秒 `intercom.Sweep` 的 goroutine（与 `sweepSessions` 并列）；`OnDisconnect` 改为按“发起方或目标方”双向结束会话。

### P1-16 播放状态机是错的：对讲/提醒/告警的音频帧会被 Client 静默丢弃；正常回答的 `playing` 会永久 latch
- **文件:行号**：`hub/cmd/hub/main.go:96-113`（对讲 `Forward` 成功后直接 `SendBinary`，广播展开后直接 `SendBinary`，全程不发 RPC）；`hub/cmd/hub/main.go:330-340`（`onReply`：置 `playing=true` → 发 `start_play` RPC → 循环 `SendBinary` → 立即置 `playing=false`，**不等设备实际播完**，且从不发 `stop_play`）；`:358-390`（`speakText`：直接 `SendBinary`，**不发 `start_play`、不置 `playing`**）；`client/src/main.rs:358-375`（`RawFrame::Audio` 分支：`if session.playing { play }`，else 分支连日志都没有）；`client/src/main.rs:405-425`（同上）。
- **为什么重要**：Hub 的 TTS 回复路径会先发 `start_play` RPC 再发音频帧，但对讲中继/广播只发裸音频帧。Client 收到帧时 `playing` 为 false，直接丢弃。于是即使 P1-15 的触发接通，对讲语音到目标音箱后也是静默丢弃。`speakText`（FR-14 提醒、FR-10 告警共用）发的帧同样会被整段丢弃——提醒/漏水播报大概率无声，除非之前某次正常回答把 `playing` latch 成了 true（`play_text` RPC 会把 `playing` 置 true 且无音频跟随，还会让后续误到的对讲帧“恰好”能播，造成时有时无的假象，极难排查）。正常回答路径从不发 `stop_play`，Client 的 `playing` 在第一次回答后永久为 true，之后 `status` 上报永远 `playing:true`。Hub 的 `playing` 在发完网络帧的瞬间即清零，所以 `OnWake` 里“播放中收到唤醒 = 打断”的判断在真实播放期间几乎恒为 false，打断逻辑名存实亡。
- **建议修复**：统一播放协议：Hub 每次播报都先发 `start_play`、播完发 `stop_play`（或按 Opus 帧时长在 Hub 侧 pacing 后再清 `playing`）；Client 收到音频帧时若未 `playing` 应视为播放开始（或 Hub 保证先发 RPC）；给 `playing` 加“按音频总时长过期”的清理；补端到端测试：speakText → Client 实际 `play_frame` 被调用；对讲首帧前向目标发 `start_play`、会话结束发 `stop_play`；或 Client 增加独立的“对讲播放态”（收到对讲帧即播，不依赖 `playing`）。

### P1-17 对讲中继失败时音频落入语音管线
- **文件:行号**：`hub/cmd/hub/main.go:96-113`（`if to, isBroadcast, err := h.intercom.Forward(...); err == nil { if to != "" || isBroadcast { return } }`——只有 `err == nil` 且命中目标才 return）。
- **为什么重要**：`relay` 失败（目标刚离线，`intercom.New` 注入的 relay 返回 `设备 %s 不在线`）时 `err != nil`，代码不 return，继续走“仲裁赢了/会话期内”的语音管线；若该设备恰在 20 秒连续对话会话内，对讲音频会被当成语音命令转写并执行。对讲失败时用户听到的是“音箱把对讲内容当命令执行了”（如对讲里说“关灯”真的关了灯），属于错误路由。
- **建议修复**：`Forward` 返回 `(to, isBroadcast, err)` 后先判断“是否在对讲会话中”（`to != "" || isBroadcast || err != nil 且会话存在`）就 return；中继失败记日志并（可选）向发起方播报“对方不在线”。

### P1-18 FR-15 紧急呼叫 Notify 失败被吞掉，用户仍被告知“已通知家人”；60 秒内重复触发不再重试
- **文件:行号**：`hub/internal/router/router.go:184-192`（`_ = r.deps.Notify(ctx, msg)`，随后无条件返回 `"已经帮你呼叫急救并通知了家人，请保持通话。…"`；重复触发分支直接 return“已经在呼叫急救了”）；`hub/cmd/hub/main.go:322-328`（`Notify` 本就是 `haClient.FireEvent`，HA 未连接时返回 `HA 未连接，无法通知`）。
- **为什么重要**：`Notify`（生产是 `haClient.FireEvent`）的错误被 `_ =` 丢弃，随后无条件返回“已经帮你呼叫急救并通知了家人”。HA 未连接/断线时（见 P1-27，HA 断线是常态），用户听到的是虚假确认。且第一次 Notify 失败后 60 秒内再次喊“救命”会走“重复触发不重复 Notify”分支，永远不再补发通知。紧急呼叫是安全功能，虚假确认比不确认更危险：用户会停止自救/等待根本不会来的救援。PLAN 要求“触发后 10 秒内拨出或推送送达”，当前代码对送达与否完全不观测。
- **建议修复**：`Notify` 错误必须向上传播并改播报为“呼叫失败，HA 未连接，请直接打电话”，并重试一次；重复触发时若上次 Notify 失败应重试（记录上次 Notify 是否成功）；`emergencyCall` 增加 `notified bool` 字段；`Notify` 成功也应只说“已尝试通知”，除非 HA 侧有送达回执。

### P1-19 FR-15 “报警”子串误触发
- **文件:行号**：`hub/internal/router/router.go:155-156`（`emergencyRe = 救命|打\s*120|叫救护车|打\s*110|报警`）。
- **为什么重要**：纯子串匹配，无词边界、无否定判断。日常语音“烟雾报警器响了”“漏水报警了怎么办”“不要报警”都会触发紧急呼叫 + Notify。与 P1-18 叠加：误触发后用户还会被告知“已经呼叫急救”。误触发会骚扰家人/触发 HA 自动化，几次之后用户将不再信任紧急呼叫，真正求救时也可能被当作误触取消。
- **建议修复**：收紧正则：`报警` 要求独立成词或与“打/叫/帮我”搭配（如锚定形态），并把“报警器/报警了”类陈述句与“别/不要报警”等否定句排除；补否定测试用例。另：紧急呼叫应加二次语音确认（“确认要呼叫急救吗”）或声纹/按键确认，见 P2-12。

### P1-20 Wyoming 客户端无锁，同一连接被多设备 goroutine 与提醒/播报并发共用
- **文件:行号**：`hub/internal/wyoming/wyoming.go:20-24`（`Client` 只有 `conn` + `reader`，无 `sync.Mutex`）；`:100-190`（`Transcribe`/`Synthesize` 都是“写一串事件 → 循环读事件”的多步序列，无锁）；`hub/cmd/hub/main.go:274-289`（STT/TTS 各一个共享 client）、`main.go:342-390`（`speakText` 与 Pipeline 共用同一个 `tts`）。
- **为什么重要**：每条设备连接各有一个 transport 读 goroutine，`OnAudioFrame` → `Pipeline.Frame` → `answer` → `Transcribe`/`Synthesize` 是并发的；提醒到点（`reminders.Store` 的 ticker goroutine）与 FR-10 播报（HA 事件读循环）又会并发调同一个 TTS client 的 `Synthesize`。`writeEvent`/`readEvent` 对同一 TCP 连接无任何串行化，两个 `Synthesize` 的请求头/音频负载会交错写入，响应也会错配到另一个调用方。双设备同时说话、或提醒到点恰撞上对话时，STT 串话、TTS 播错内容或直接报协议错，且错误随机出现、难以复现。
- **建议修复**：给 `wyoming.Client` 加 `sync.Mutex`，`Transcribe`/`Synthesize`/`Describe` 全程持锁（读事件循环也在锁内）；或每路调用方持独立连接（STT/TTS 各建连接池）。同时给 `Transcribe`/`Synthesize` 加整体超时。

### P1-21 HA 客户端多路并发 `WriteJSON` 不安全（gorilla websocket 只允许一个并发写者）
- **文件:行号**：`hub/internal/ha/ha.go:146-175`（`call()`：注册 `pending` 后释放 `mu`，再直接 `c.conn.WriteJSON(m)`，写期间不持锁）；调用方：两台设备的语音管线可并发进 `router.homeAssistant` → `GetEntityState`/`CallService`（`hub/internal/router/router.go:341-398`）。
- **为什么重要**：gorilla websocket 文档要求同一连接最多一个并发写者；两路 `call()` 并发写会交错帧、写出损坏的 JSON，甚至触发内部 race。`SnapClient.call`（`music.go:76`）、`MPDClient.cmd`（`music.go:438`）都正确持锁，只有 HA 与 Wyoming 没锁。
- **建议修复**：给 `ha.Client` 加写互斥锁（`call()` 与 `SubscribeEvents` 的 `WriteJSON` 都走同一把锁），或把写操作串行化到单一写 goroutine。

### P1-22 Web PUT 全量替换且不合并：省略的字段会被清空；运行中子系统不重建，配置与行为漂移
- **文件:行号**：`hub/internal/web/web.go:85-116`（PUT 解码进全新的 `config.Config`，`ApplyDefaults` 只补 Hub 字段与空 map，随后 `Save` + `s.cfg = &cfg`）；`hub/cmd/hub/main.go:278-325`（Agent/HA/Wyoming/Router 均在启动时一次性构建，之后不再读 `cfg`）；`hub/cmd/hub/main.go:139-156`（`OnRoomClaim` 直接改 `h.cfg.Devices` 并 `h.cfg.Save(h.cfgPath)`，无锁）；`web_test.go:65-72`（PUT 的 JSON 字段名是 Go 结构体的 PascalCase（`WakeWord`），与 `config.yaml` 的 snake_case（`wake_word`）不一致——按 YAML 习惯写小写字段会被静默丢弃）。
- **为什么重要**：两处错误。（a）PUT 省略 `devices`/`agents`/`ha_entities`/`wake_word_routes` 任一即被清空为 `{}`——而 `TEST_REPORT.md` 写的是“PUT 可以只改关心的字段”，实际行为相反，合法的部分更新会静默销毁整库配置；小写字段名还会被静默丢弃，随后 `Validate` 报 `wake_word 为空`或更糟：缺失的 `Agents`/`WakeWordRoutes`/`HAEntities`/`Devices` 被悄悄清空。（b）即使 PUT 完整，运行中的 Agent 客户端、HA 连接、Wyoming 连接、Router 的 `entities` 仍是启动时的旧对象，GET 返回新配置、行为用旧配置；`OnRoomClaim` 还继续写 `h.cfg` 指向的旧 `Config` 对象，房间认领在 PUT 后丢失。（c）`OnRoomClaim` 与 Web PUT 各自写同一文件、无互斥、无合并：认领房间后 `Save` 的是 main 启动时的旧 `cfg` 对象，会把 PUT 刚写入的修改 whole-file 覆盖丢（反之亦然，后写者胜）。
- **建议修复**：PUT 改为“读现有配置 → 按请求字段合并（map 字段逐键合并、缺省不碰）→ 校验 → 保存”；或明确改为“必须全量 PUT”，缺字段直接 400。给 `Config` 加 `json` tag 与 YAML 保持一致的命名。下游重建（至少 Agent/HA/Wyoming/Router entities）要么实现热重载，要么 PUT 后返回“需重启 Hub 生效”并由 `nas-install.sh`/compose 触发重启。保存与认领共用一把写锁 + “读-改-写”合并。

### P1-23 `config.Devices` map 多 goroutine 无锁读写，可致致命崩溃与保存竞态
- **文件:行号**：`hub/cmd/hub/main.go:140-155`（`OnRoomClaim` 在 transport goroutine 里写 `h.cfg.Devices[deviceID]` 并 `_ = h.cfg.Save`）；`hub/internal/web/web.go:68-116`（`configAPI` GET 编码 `s.cfg`、`PUT` 调 `Save`，`s.cfg` 与 `h.cfg` 是同一指针）；`hub/internal/config/config.go:189-198`（`Save()` 固定用 `path+".tmp"` 再 `Rename`）。
- **为什么重要**：同一 `map` 被 transport goroutine 写、Web GET 迭代编码、Web PUT 全量替换并写文件，三路无任何锁。Go 运行时的 map 并发读写是致命错误（`concurrent map writes` / `concurrent map iteration and map write`），直接 crash 进程，不是普通 data race。另两路 `Save` 用同一个 `path + ".tmp"`，先写完的一方会被后 rename 的一方覆盖，房间认领或配置修改会静默丢失；`OnRoomClaim` 的 `Save` 错误被 `_ =` 丢弃，落盘失败无感知。
- **建议修复**：给 `Config` 的 `Devices` 访问加 `sync.RWMutex`（或把房间认领改为经 web 层同一个带锁的 `UpdateDevice` 方法）；`Save` 也纳入同一把锁并用唯一临时文件名（如 `path+".tmp."+pid`）并检查错误；`OnRoomClaim` 写失败要记日志并回失败给设备。配置读写集中到一个带锁的 `Store`（单指针 + `sync.Mutex`）。

### P1-24 提醒遇到未知 Kind 时每秒重复触发、永不停止
- **文件:行号**：`hub/internal/reminders/reminders.go:202-237`（`tick` 的 switch 无 `default` 分支）；`:116-127`（`Add` 不校验 `Kind`）。
- **为什么重要**：`tick` 对 `KindOnce` 删除、`KindDaily` 重算、`KindInterval` 推进；未知 Kind（如损坏/手改的 `reminders.json` 里 `kind: "weekly"`、或旧版本写入的空串）既不删除也不重排，`Next` 永远 ≤ now，于是每秒（ticker 周期）触发一次 `onFire` → 每秒 TTS 播报一次，永不停，直到人工删文件或重启后再次加载又继续。`Add` 不校验 Kind，脏数据可从任何写入方进入。
- **建议修复**：`Add` 与 `Load` 校验 Kind，非 `once/daily/interval` 直接拒绝/跳过并记日志；`tick` 加 `default: delete + log`（未知类型按一次性处理，删掉并告警）。

### P1-25 每日提醒的小时/分钟不校验，越界输入导致全程无声、提醒未建
- **文件:行号**：`hub/internal/router/router.go:217-232`（`hour, _ := strconv.Atoi(m[1])`，`min` 同理，无范围检查；`at := fmt.Sprintf("%02d:%02d", hour, min)`；`NextDaily` 报错时 `return "", true, err`）；`hub/internal/reminders/reminders.go:113-124`（`Add` 本身不校验 `Kind`/`Every`/`At`）。
- **为什么重要**：`每天 25 点提醒我吃药` → `at="25:00"` → `NextDaily` 的 `time.Parse("15:04")` 报错 → `Route` 返回 error → `Pipeline.answer` 返回 error → `OnAudioFrame` 只记一条日志，用户全程听不到任何回复，提醒也没建。`每天 8 点 70 分` 同理。STT 误识别很容易产出越界数字（“8 点 60 分”），这条路径并不罕见。错误处理链断在用户侧：本应是“一句话纠正”，实际是“静默失败”。
- **建议修复**：`reminder()` 里校验 `0<=hour<=23`、`0<=min<=59`，越界时返回中文纠正（“时间不对，小时要在 0 到 23 之间”）而不是 error；`intervalRe` 的 `mins` 校验 `0 < mins ≤ 1440`（`每 0 分钟`会立即触发一次随后被删除）；同时 `Pipeline.answer` 的 Router/STT/TTS 错误应转为一句播报（“刚才没听清/服务没连上”），而不是只记日志；`CancelByText` 改为子串/归一化匹配，并增加“把 X 提醒改到 Y 点”分支。

### P1-26 语音点歌与 Snapcast 的生产接线是断的：Router 的 QueueID 恒为空且 MPD 从未注入；Snapcast 从未 Dial
- **修复状态（2026-11-17，已修复）**：`main.go` 启动时查 MA `Players()` 取默认 `player_id` 作 `QueueID`（查不到记日志、点歌走 MPD 降级）；`music.DialMPD(cfg.Hub.MPD)` 注入 `MPD`；`music.DialSnap(cfg.Hub.Snapcast)`；MPD/Snapcast 启动时没连上时后台每 30s 重试、连上即经 `rt.SetMPD`/`rt.SetSnap` 补接。证据：`go test ./internal/router/` 17 passed、`./internal/music/` 5 passed（本轮重跑）。剩余：MA 恢复后 `QueueID` 不热更新（需重启 Hub，见 P1-27）；真机两台同步播放延迟差 ≤10ms 未验证。
- **文件:行号**：`hub/cmd/hub/main.go:300-325`（`router.New(router.Deps{..., QueueID: "", ...}`，`MPD` 字段根本没传）；`hub/internal/router/router.go:300-330`（`music()`：`MA != nil && QueueID != ""` 才走 MA；否则走 Netease + `MPD`，`MPD == nil` 时返回“音乐服务没接好（播放器未配置）”）；`hub/internal/music/music.go:62-75`（`DialSnap`）；`hub/cmd/hub/main.go` 全文无 `DialSnap` 调用，`cfg.Hub.Snapcast`（`hub/config.yaml: snapcast: localhost:1705`）无人读取。
- **为什么重要**：`QueueID: ""` 使 MA 分支恒为假；`MPD` 字段恒为 nil 使降级分支恒失败。于是“播放周杰伦的青花瓷”在生产上**永远**返回“音乐服务没接好（播放器未配置）”，与 `config.yaml` 里配好的 Music Assistant 地址、`music.go` 里完整实现的 MA/MPD 客户端无关——纯接线错误。包内单测因手动注入了 `QueueID`/`MPD` 而全部通过，掩盖了生产接线缺失。Snapcast 分组、延迟校准、静音/音量（`music.go:110-160`）全部是死代码。`deploy/docker-compose.yml` 起了 snapserver + mpd，但 Hub 与之零交互；两房间同步播放的延迟校准（目标 ≤10ms）没有任何生产路径会执行。与 `TEST_REPORT.md` 的“MA 桥接网络未核实”不同：这里不是“接了但没验证”，是“根本没接”。FR-05（P0 需求，“3s 内出声”）与 FR-03 在生产部署下 100% 失败。
- **建议修复**：`main.go` 在启动时查 MA `Players()` 取默认 `player_id` 作 `QueueID`（查不到则记日志并使点歌明确报“MA 未就绪”），并 `music.DialMPD(cfg.Hub.…)` 注入 `MPD`（MPD 地址需先补进 `config.yaml`，当前连配置项都没有）；启动时 `DialSnap(cfg.Hub.Snapcast)`，失败记日志但继续（后台重试，见 P1-27）；点歌成功后按配置的分组/延迟调用 `SetClientLatency`/`SetGroupStream`；或明确把 Snapcast 管理权交给 MA 并删除 Hub 内的 Snapcast 客户端与配置项，二选一，不要留死代码；补一个“用 main 同款 Deps 构造 Router”的集成测试。

### P1-27 HA / Wyoming 断线永不重连；读循环出错静默退出，haClient 成僵尸
- **文件:行号**：`hub/cmd/hub/main.go:236-245`（`ha.Dial` 失败记“HA 连接失败（稍后重试，不阻塞启动）”，但全文无任何重试代码）；`:274-282`（Wyoming STT/TTS 同样一次 Dial，失败即 nil）；`hub/internal/ha/ha.go:102-135`（`readLoop` 读错即 `return`，无重连、无通知）；全仓库无 `reconnect`/`redial`/`health` 相关代码（`grep -rn reconnect hub/` 无结果，除 Client 外）。
- **为什么重要**：注释承诺的“稍后重试”不存在。HA 重启/网络抖动一次，`haClient` 即永久失效：之后每次家电命令先 `GetStates` 失败（错误被 `router.go` 的 `err == nil` 判断吞掉），再 `CallService` 等 10 秒超时才报错；FR-10 的 `Watcher` 订阅也随读循环退出而永久失效，且 `main.go` 只在启动时订阅一次。HA 是家电、场景、紧急通知、摄像头播报四条线的共同上游，断一次等于四条线全断且永不自愈；用户侧表现为“突然什么家电都不听使唤了”，只能重启 Hub。NAS 上容器重启、HA 升级是最常见的运维事件，每次都要人工重启 Hub 才能恢复语音，且失败是静默的。
- **建议修复**：给 `ha.Client` 加带退避的重连（断线后每 5→30 秒重 Dial，成功后重订阅已有 handlers 并恢复 `pending`）；Wyoming STT/TTS 用“懒连接 + 失败重试”包装，或在 `answer()`/`speakText` 失败时触发一次重拨；或 `main.go` 起一个监督 goroutine 定期探活、失败则重建 client 并重新接 `Watcher` 与 Router 的 `HA` 引用（注意 Router 持有的是启动时传入的指针，重建需同步更新）；Web `/api/status` 暴露各下游健康状态。

### P1-28 语音 utterance 无长度上限且断线不清理：长语音无限累积，断线后旧音频混入下一句
- **文件:行号**：`hub/internal/voice/voice.go:76-110`（`Frame` 无条件 `u.pcm = append(u.pcm, pcm...)`，结束条件只有 800ms 静音或 `End`）；`hub/cmd/hub/main.go:183-188`（`OnDisconnect` 只删会话 + `intercom.End`，不碰 Pipeline）；`hub/cmd/hub/main.go:62-75`（`decoders` map 按设备建后**永不删除**）。
- **为什么重要**：持续人声/噪声（RMS > 200）且无 800ms 静音时，`pcm` 无限增长：一次长独白或持续底噪即可吃掉大量内存，随后整段发给 STT（超长音频转写慢、费用高、还可能超 Wyoming/LLM 限制）。电视声/持续说话使 `speech==true` 且 `silenceMs` 归零，`pcm` 可无限增长（每 20ms 一帧，1 分钟约 1.9MB PCM，10 分钟约 19MB/设备，且 `answer()` 还要再复制一份字节流）。设备断线（或 60 秒超时断线，见 P1-11）时 utterance 残留在 `Pipeline.utterances` 里；同 deviceID 重连后下一句话会把旧 PCM 前缀带上，转写结果混入上一段语音的尾巴；断线重连会不断新建解码器（每个持有 libopus 状态）而不释放旧的。直接违反 NFR-05（7×24h 不漏内存）。
- **建议修复**：`Frame` 加上限（如 30s/60s，超限强制 `answer` 并记日志）；`Pipeline` 加 `Discard(deviceID)` 方法，`OnDisconnect` 调用；`sweepSessions` 清会话时顺带清对应 utterance；`OnDisconnect` 删 `decoders[deviceID]`（需给 `opus.Decoder` 补 `Close` 或直接丢弃）；定期扫荡超期未结算语音。

### P1-29 `Pipeline.answer` 同步跑在 transport 读循环里，打断与 audio_end 会被 STT/Agent/TTS 拖住数秒；下游失败全部静默
- **文件:行号**：`hub/internal/voice/voice.go:130-179`（`answer` 串行做 STT → Router → TTS → 编码，全程无 goroutine、无超时）；`hub/cmd/hub/main.go:96-130`（`OnAudioFrame` 同步调 `pipeline.Frame`，即 transport 读循环 goroutine 内执行）；`hub/internal/router/router.go:148,226,232,243,254,273,315,324,327,395`（各分支 `return "", true, err`）；`hub/internal/agent/agent.go:102-158`（`ChatStream` 全仓库除测试外零调用，Router 用的是非流式 `Agent.Chat`，`router.go:145-150`）。
- **为什么重要**：一句话的完整处理（STT 数秒 + Agent 数秒 + TTS 合成）在设备连接的唯一读 goroutine 里同步执行，期间该连接的后续消息（新的 wake、audio_end、interrupt）都在 TCP/websocket 缓冲里排队。用户在播放中喊唤醒词打断，Hub 要等整轮回答跑完才读到 interrupt；`audio_end` 被延迟则 `End` 晚调，下一句的结算也被拖后。`context.Background()`（`main.go:122-128`）还意味着没有任何一环能被取消。任何下游失败（网易云搜不到、HA 调服务失败、Agent 报错、TTS 失败）都只变成 `main.go:127` 的一条 `log.Printf("voice frame: …")`，用户端完全静默——连一句“没听清/服务出问题了”都没有。直接损害 Phase 3 的核心指标“打断成功率 ≥90%”与“唤醒到首字 ≤1.5s”；FR-08 要求“流式回答、首字 ≤800ms、长回答边生成边播”，但语音是“等全文 → 等全量 TTS → 一次性发全部 Opus 帧”，首字延迟 = LLM 全文 + TTS 全文，必然超 1.5s（NFR-04）。
- **建议修复**：`OnAudioFrame` 里 `answer` 改为异步（每设备一个有界 worker，`Frame` 只入队、不阻塞读循环）；或至少把 `answer` 放到独立 goroutine 并用带超时的 context（STT/Agent/TTS 各设上限）；打断路径（wake → interrupt）与音频处理路径分离，interrupt 优先处理；下游失败时合成一句简短错误提示再返回（至少让用户听见失败）；Router 改用 `ChatStream` 边生成边合成边播（或先播首句再续播），并给 STT/LLM/TTS 各设分段超时。

### P1-30 patch_all 中途失败无回滚；`_write` 是非原子写
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:91-92`（`_write` 直接 `write_text`）；`:552-570`（`patch_all` 顺序调 `patch_ssh → patch_login → patch_disable_ota → patch_start → patch_asound → patch_libxaudio`，无快照/回滚）；`:302/:307/:319`（`patch_login` 先后写 shadow、inittab、pam）；`firmware-patch/scripts/patch.sh:53-60`（trap 只删 `OUT`，不清 `SQFS_ROOT`）。
- **为什么重要**：`preflight` 只保证“前置条件不满足时不写文件”（`TEST_REPORT.md` 已验证的只是这一种失败）。一旦在 apply 阶段失败（如 pam 子表达式失败、磁盘满、进程被杀），已写的文件不会还原，rootfs 副本处于半补丁状态；`_write` 是原地截断再写，无 tmp+rename、无 fsync，写一半断电会留下截断的 shadow/dropbear（无法启动或无法 SSH）。半补丁镜像若被误刷，设备可能无法启动或 SSH 行为异常；且失败现场（半补丁的 `squashfs-root`）会留在磁盘上，容易被误认为可用。
- **建议修复**：每个文件写临时文件 + rename + fsync；`patch_all` 出错时从快照恢复或直接删除工作副本并明确报错“需重跑 patch.sh”；把“apply 阶段失败清理”写进测试；`patch.sh` 的 trap 顺带清理 `SQFS_ROOT`。

### P1-31 compose 的 hub-data 卷挂错了位置，Hub 状态实际落在容器可写层
- **修复状态（2026-11-17，已修复）**：compose 卷改挂到 `/app/hub-data`（Hub 数据目录就是 `./hub-data`，`WORKDIR /app`）。证据：文件核对（`hub-data:/app/hub-data`，本轮确认）。剩余：NAS 实测 BLOCKED（容器重建后提醒/记忆/TLS 证书指纹不变，需在 NAS 上验证）。
- **文件:行号**：`deploy/docker-compose.yml:28`（`hub-data:/app/data`）；`hub/config.yaml:56`（`data_dir: "./hub-data"`）；`deploy/Dockerfile.hub:17`（`WORKDIR /app`）、`:20`（`CMD ["-config", "/app/config.yaml"]`）；`hub/cmd/hub/main.go:226`（`MkdirAll(cfg.Hub.DataDir)`）、`:266/:268`（`reminders.json`、`memory.json` 在 `DataDir` 下）、`:414`（TLS 证书 `LoadOrCreate(cfg.Hub.DataDir)`）。
- **为什么重要**：容器内 Hub 工作目录是 `/app`，数据实际写到 `/app/hub-data`，而命名卷挂在 `/app/data`——卷挂了但没人用。容器重建（`down`/`up`、重编镜像）后提醒、记忆、TLS 证书全丢；证书再生会改变指纹，客户端 wss 指纹 pin 随即失效，音箱连不上 Hub，直到人工更新指纹。静默丢数据 + 断连；与“7×24h 稳定”目标冲突。
- **建议修复**：二选一：compose 把卷改挂到 `/app/hub-data`，或把 `config.yaml` 的 `data_dir` 改为 `/app/data`；并加一条冒烟检查（`up` 后重启容器，确认提醒与证书指纹不变）。

### P1-32 config.yaml 以 `:ro` 挂载，但 Hub 运行时要写它
- **修复状态（2026-11-17，已修复）**：compose 挂载去掉 `:ro`（改可写）；`main.go` 的 `OnRoomClaim`：`Save` 失败记日志，不再吞掉；`deploy/Dockerfile.hub` 里“挂载（只读）”的过时注释已同步改掉（本轮）。证据：文件核对（`../hub/config.yaml:/app/config.yaml`，无 `:ro`，本轮确认）。剩余：NAS 实测 BLOCKED（语音认领房间重启后仍在，需在 NAS 上验证）；可写/种子配置分离（P1-22/P1-23 重构范围）未修。
- **文件:行号**：`deploy/docker-compose.yml:27`（`../hub/config.yaml:/app/config.yaml:ro`）；`hub/cmd/hub/main.go:147-151`（`OnRoomClaim` 后 `_ = h.cfg.Save(h.cfgPath)`，错误被忽略）；`hub/internal/web/web.go:103`（config PUT 时 `Save`，失败返 500）。
- **为什么重要**：语音认领房间、Web 改配置都要写 `config.yaml`；只读挂载下 `Save` 必然失败。房间认领在内存里生效、重启后丢失（且 `main.go:151` 吞掉了错误，无日志）；Web PUT 直接 500。`TEST_REPORT.md` 只在宿主机上测过 config PUT，未在容器挂载下测过。核心功能（语音认领房间持久化）在 NAS 部署形态下实际不可用，且失败是静默的。
- **建议修复**：挂载去掉 `:ro`（改 rw），或把 Hub 的可写配置与只读种子配置分离；`OnRoomClaim` 的 `Save` 错误必须记日志。

### P1-33 nas-install.sh 每次覆盖 NAS 端 config.yaml，且 .bak 不是旧文件备份
- **文件:行号**：`deploy/nas-install.sh:72-88`（第 2 步 scp 全量覆盖 `deploy/` 与 `hub/config.yaml`；第 3 步 `cp $NAS_DIR/hub/config.yaml $NAS_DIR/hub/config.yaml.bak` 后 sed）。
- **为什么重要**：NAS 端 `config.yaml` 是 Hub 的运行态文件（语音认领的房间、发现的 IP、Web 改动都写在这里，`main.go:151`、`web.go:103`）；每次重跑脚本都用本机旧副本全量覆盖，NAS 端改动无备份丢失——`.bak` 是在覆盖之后才从“刚传上去的文件”复制的，备的是新文件不是旧文件；第二次跑还会把已 sed 改过的文件再备一次，原始文件彻底丢失。
- **建议修复**：scp 前先把 NAS 端 `config.yaml` 拉回本机备份（或远端 `cp` 带时间戳）；只在 NAS 端文件不存在时才上传；`.bak` 只在不存在时创建或加时间戳；改用合并而非覆盖。

### P1-34 nas-install.sh 网易云检查恒为 PASS
- **文件:行号**：`deploy/nas-install.sh:121`（`$SSH "curl -s -m 5 http://127.0.0.1:3000/ | head -c 200; echo" && ok …`）。
- **为什么重要**：远端命令以 `echo` 结尾，`echo` 恒返回 0，ssh 恒返回 0，`ok` 恒打印——即使 netease-api 容器没起、端口不通，检查也显示 PASS。`curl` 本身没加 `-f`，HTTP 500 也算成功。部署检查的意义被架空；网易云链路断了要到点歌时才发现。
- **建议修复**：去掉结尾的 `echo`，用 `curl -fsS` 并断言 HTTP 200（或对 `/login/status` 做 JSON 断言）。

### P1-35 nas-install.sh 接受 CHANGE_ME 占位 token
- **文件:行号**：`deploy/nas-install.sh:45-50`（预检查只判非空）；`deploy/.env.example:15`（`OPEN_XIAOAI_TOKEN=CHANGE_ME`）。
- **为什么重要**：用户照抄 `.env.example` 忘改 token 时，检查显示“已填”通过；Hub 与音箱将用公开写在仓库示例里的字符串 `CHANGE_ME` 做鉴权，等于没有鉴权。结合 P0-1 的无鉴权 Web 面板，管理面基本不设防。
- **建议修复**：预检查拒绝 `CHANGE_ME`、空值与过短 token；`.env.example` 改用空值加注释。

### P1-36 串口被改成免密 root shell
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:287-319`（`patch_login`；inittab 的 `ttyS0::askfirst:/bin/login` 改为 `ttyS0::askfirst:/bin/ash --login`）。
- **为什么重要**：原行经过 `/bin/login` + PAM 认证；改后串口 askfirst 直接起 root 的 ash，不问口令。LX06 调试口拆机可达；OH2P 是底部 Type-C 免拆可达，物理接触 + 一根线即 root。能短暂接触设备的人无需口令拿到 root；与“SSH 改默认密码”的安全叙事相悖。
- **建议修复**：评估是否真需要串口免密调试；若只为救砖，改为保留 `/bin/login`（PAM 已切到 pam_unix，root 口令可用），或在首次启动成功联网后由 `/data/init.sh` 把 inittab 改回 login。

### P1-37 mosquitto 1883 疑似匿名开放，且 Hub 根本不用 MQTT（疑似，需 NAS 核实）
- **文件:行号**：`deploy/docker-compose.yml:58-64`（`eclipse-mosquitto:latest`，映射 `1883`，无配置卷、无启动参数、无密码文件）；hub 代码内无 MQTT 引用（`deploy/NAS_CHECKLIST.md` 故障速查亦承认“Hub 当前代码里没有 MQTT 引用”）；`hub/go.mod:20`（`// TODO Phase 1: MQTT 客户端`）。
- **为什么重要**：官方镜像默认配置疑似允许匿名接入（需在 NAS 上 `docker exec` 进容器 `cat /mosquitto/config/mosquitto.conf` 核实）；一个无人使用的 broker 白白暴露在局域网 1883 上。若匿名属实，局域网内任何人可读写 MQTT 主题（设备发现/注册表预留的通道）；且是无收益的暴露面。
- **建议修复**：核实后二选一：不用就从 compose 里删掉 mosquitto；要用就挂自定义 `mosquitto.conf`（`allow_anonymous false` + `password_file`）。

### P1-38 FR-12 多 Agent 路由是假的：Agent 在启动时按默认唤醒词选定一次，之后永不切换
- **文件:行号**：`hub/cmd/hub/main.go:251-262`（`if name, ok := cfg.WakeWordRoutes[cfg.WakeWord]` 选一个 `agentClient`，否则取 `cfg.Agents` map 迭代的第一个并 `break`）；`hub/internal/router/router.go:119-152`（`Route` 只有单一 `deps.Agent`，全程不看 `deviceID` 的唤醒词）；`hub/internal/arbitration/arbitration.go:17-24`（`WakeEvent` 带 `WakeWord`，但 `main.go:76-94` 的 `OnWake` 与仲裁回调都不使用它）。
- **为什么重要**：“你好小李”→Hermes、“你好龙虾”→OpenClaw 的路由从未发生：Client 上报的 `wake_word` 在 Hub 入口即被丢弃；Router 只有一个 Agent。更糟的是回退分支 `for _, a := range cfg.Agents { …; break }` 依赖 Go map 随机迭代顺序——配置里 `wake_word_routes` 缺失默认唤醒词时，每次启动选中的 Agent 是随机的。FR-12（P1）宣称实现但实际未实现；随机 Agent 还会让同一句话在不同启动后得到不同风格/能力的回答。
- **建议修复**：把 `WakeWord` 存进仲裁赢家的会话（或直接由 `OnWake` 按 `WakeWordRoutes` 选 Agent 注入当次 `Route` 调用）；Router 持 `map[string]*agent.Client`，按唤醒词选；回退分支改为确定性选择（按 `cfg.WakeWord` 或排序后的第一个 key），并在路由缺失时启动即报错而非随机挑。

### P1-39 房间认领端到端是断的：Client 从不发 `room_claim`，Router 也不解析“我在客厅”
- **文件:行号**：`client/src/transport.rs:361-365`（`send_room_claim` 存在但标记 `#[allow(dead_code)]`，`main.rs` 全文件无调用）；`hub/internal/router/router.go` 全文件无 `我在` 解析（`grep 我在` 零命中）；`hub/internal/transport/transport.go:274-278` 与 `hub/cmd/hub/main.go:139-156`（`OnRoomClaim` 服务端已实现，但除测试外无发送方）。
- **为什么重要**：PLAN §8 定好的认领方式（“你好小李，我在客厅”→ Hub 记住房间）没有任何生产路径：语音文本进了 Router 后没有房间解析分支，最终落到 Agent 闲聊；`room_claim` 协议消息也没有任何发送方。房间名只能靠改 `client.toml` 的 `room` 字段随 hello 上报——而这条路还会带来 P2-51 的回退问题。房间是 FR-06（“关这屋的灯”）、FR-04 对讲路由、FR-14 分房间提醒的前提；认领断了，这些功能只能靠手填 IP/房间，回到 PLAN 明确要去掉的手工方式。
- **建议修复**：在 Router 增加“我在这里/我在 X / 这是 X”解析分支，命中后调注册表 `SetRoom` + 写回 config（复用现有 `OnRoomClaim` 的持久化逻辑）；或在 Client 侧 KWS 命中后支持语音认领消息；并补端到端测试（文本 → 房间落盘 → 下次同房间命令生效）。

### P1-40 ARM 交付二进制根本不含 KWS：`build-armv7.sh` 没开 `sherpa-kws` feature，默认 Client 永远无法唤醒
- **修复状态（2026-11-17，已修复审计要求、但真机唤醒仍 BLOCKED）**：`build-armv7.sh` 默认开 `--features sherpa-kws`（`--no-sherpa-kws` 仅供应急构建），构建末尾断言产物含 sherpa 标记与 ARM `.so` 的 10 个必需导出符号；feature 开时 `Kws::new` 直接返回 Err——生产路径在 sherpa 后端加载失败后调到这里，Err 经 `?` 向上传播，进程以非零码退出并打印原因，绝不静默跑 RMS；ARM 版 `libsherpa-onnx-c-api.so` 已取（sherpa-onnx 官方 v2.0.1 `linux-arm-gnueabihf-shared`，SHA256 `46d5250538b5bcf97d921204f7fe61eca7bac119ad2dcb1c95b557a314d8bc6b`）。证据：`./build-armv7.sh` exit 0（`ASSERT OK: ARM binary contains sherpa-kws markers`、`ASSERT OK: … exports all 10 required KWS symbols`，产物 2,885,364 字节 ELF 32-bit ARM 静态链接 stripped，本轮重跑）；`cargo test --features sherpa-kws` 50 passed（x86_64 宿主机，本轮重跑）。**BLOCKED 细节**：官方 ARM `.so` 要求 `GLIBC_2.29`（音箱最高 2.25）且 `GLIBCXX_3.4.29`（音箱最高 3.4.22），`build-armv7.sh` 本轮重跑即打印 `WARNING: … requires GLIBC_2.29, speaker has glibc 2.25 — will NOT load on-device as-is`（GLIBCXX 同理）；且 musl 静态 Client 不支持 `dlopen`（x86_64 同构实测：musl 静态程序 `dlopen` 直接失败 `Dynamic loading not supported`，exit 2）。即：哪怕拿到版本兼容的 `.so`，不改 Client 链接方式也加载不了。出路二选一，待决策：(a) Client 改动态链接 musl（或 static-PIE）+ 交叉编译 musl 版 sherpa/ORT（ORT 没有 musl 预编译，需从源码编，工作量大）；或 (b) Client 改 `armv7-unknown-linux-gnueabihf`（glibc 2.25 目标，zig 可指定 `-target arm-linux-gnueabihf.2.25`）+ 从源码重编 sherpa（复用官方 `libonnxruntime.so`，它只要求 GLIBC_2.4）。在决策落地前，刷进音箱的 Client 在 FR-01 上仍是零能力（只会上报 RMS，永不产生 `wake` 事件），且 `cargo test --features sherpa-kws` 的 50 通过全部是 x86_64 宿主机上的验证，不能当作 ARM 真机唤醒的证据。
- **实施状态（2026-11-17，方案 B，bai 授权 Friday 定、Friday 选 B）**：客户端已从 musl 切换到 `armv7-unknown-linux-gnueabihf`（新脚本 `client/build-armv7-gnueabihf.sh`，zig 链接器 `-target arm-linux-gnueabihf.2.25`，默认 sherpa-kws，musl 脚本保留作明确标注的 fallback；产物 2,435,964 字节，SHA256 `2da754d4…7a00b`，GLIBC 最高 2.25、硬浮点、RUNPATH `$ORIGIN/lib`，NEEDED 全在音箱 rootfs 内；ALSA 动态链接音箱自带的 `libasound.so.2`；fail-loud 保留）。但 **.so 原义证伪**：官方两个 ARM `.so` 均要求 GLIBC 2.34 / GLIBCXX 3.4.29（“ORT 只要求 GLIBC_2.4”是错的，已实测纠正）；旧版 v1.10.34/v1.9.29/v1.8.10 全部要求 GLIBC 2.29，无可用版本；微软从未发布 armv7 ORT 预编译包。剩余两条路（源码重编 ORT+sherpa / KWS 移出音箱）待 Friday 复议；.so 出路定下来之前不要刷机。详见 TEST_REPORT.md“P1-40 方案 B”一节与 `~/.kws-exp/survey/SURVEY_REPORT.md`。
- **文件:行号**：`client/build-armv7.sh:33-42`（`cargo build --release --target armv7-unknown-linux-musleabihf "${ALSA_FEATURE_ARGS[@]}" "$@"`，无 `--features sherpa-kws`）；`client/Cargo.toml:100-104`（`sherpa-kws` 是可选 feature，默认不开）；`client/src/main.rs:233-235`（feature 不开时直接 `Kws::new` 纯 RMS，`feed` 恒返回 `None`，见 `client/src/kws.rs:180-200`）；`client/src/main.rs:234-236`（无模型时只打 warn，继续跑传输）。
- **为什么重要**：TEST_REPORT 宣称“KWS 已验证、ARM 产物已交付”，但验证用的是 `cargo test --features sherpa-kws`（x86_64），而实际交付的 ARM 二进制（`target/armv7-…/open-xiaoai-client`，2,852,620 字节）编译时根本没开该 feature——且 ARM 版 `libsherpa-onnx-c-api.so` 本就还没交叉编译（TEST_REPORT 已承认）。即：**刷进音箱的 Client 在 FR-01（P0，唤醒率 ≥95%）上是零能力**，只会上报 RMS，永不产生 `wake` 事件；Hub 侧永远收不到唤醒，整条语音链路无从开始。这是“第一关口”（刷一台 LX06 + Hub 跑起来）的前提条件失效；且失败是静默的（Client 日志只有一条 warn，进程正常跑、正常连 Hub）。
- **建议修复**：先交叉编译 ARM 版 `libsherpa-onnx-c-api.so`（TEST_REPORT 已列为 BLOCKED），再把 `build-armv7.sh` 改为默认 `--features sherpa-kws`（或把 sherpa 设为默认 feature），并在构建脚本末尾断言产物含 sherpa 符号/能加载模型；同时让 Client 在 KWS 后端缺失时以非零退出或在 Web 状态中明确告警，而非静默跑 RMS。

### P1-41 FR-09 记忆只写不读；“2 小时后关灯”这类定时动作根本没实现
- **文件:行号**：`hub/internal/router/memory.go:37-49`（`Memory.Set` 按时间戳存原文）；`hub/internal/router/router.go:266-285`（`sceneMemory` 只调 `Set`，全仓库除测试外无 `Memory.All` 调用——`grep All(` 仅 `registry.All` 与 `web` 的设备列表）；`hub/internal/router/router.go:198`（`afterRe` 只匹配 `(\d+)小时后提醒我(.+)`，即只建**提醒**）；`hub/internal/router/router.go:294-339`（`music`）、`:341-398`（`homeAssistant` 均不处理定时动作）。
- **为什么重要**：(a) “记住空调 26 度”写入 `memory.json` 后没有任何代码读它，换对话、换设备都不会生效——FR-09 “教一次即记，换对话仍有效”的验收标准不成立；(b) “2 小时后关鱼缸灯”（PLAN FR-09 示例）既不匹配提醒正则（缺“提醒我”三字），也不匹配家电正则（不以开/关开头），最终落到 Agent 闲聊，灯不会在 2 小时后关。且“记住”会给用户“我已记住”的虚假确认。
- **建议修复**：家电命令执行前查 `Memory.All(deviceID)` 做偏好回填（如默认温度）；新增定时动作分支（解析“X 分钟/小时后 + 开/关 + 别名”→ 建一次性提醒，到点执行 HA 服务而非仅播报）；或明确把这两项移出 FR-09 验收。

### P1-42 FR-06 查状态是“能查就查、查不到也照调”；状态查询类命令完全没接 HA
- **文件:行号**：`hub/internal/router/router.go:380-393`（`GetEntityState` 出错时 `if state, err := …; err == nil` 跳过比较，**直接继续调 `CallService`**）；`:341-360`（只认以“打开/关掉/开启/关闭/开/关”开头的命令）；`hub/internal/ha/ha.go:218-231`（`GetEntityState` 每次都 `GetStates()` 拉全量实体）。
- **为什么重要**：(a) 与文件头注释“先查状态再调服务，不编数字”相反：HA 查状态失败时静默跳过检查，重复调用照常发生；(b) “客厅灯开着吗”“冰箱多少度”“空调现在几度”这类查询不以开/关开头，全部落到 Agent——Agent 没有 HA 状态输入，只能凭空回答（PLAN §4.4 明确要求“查冰箱温度先查 HA 再答”）。另 `GetEntityState` 每次全量拉取在实体多时是 O(N) 且多一次往返。
- **建议修复**：查状态失败时返回“查不到状态，先不执行”而非继续调服务；增加查询分支（“X 开着吗/X 多少度/X 什么状态”→ `GetEntityState` 后照实播报）；HA 侧增加按实体 ID 单查（`get_states` 后本地缓存 + `state_changed` 增量更新，或调 HA 的 `states/<id>` REST）。

### P1-43 Client 无限重连：退避上限仅 1s、无 jitter、错 Token 也永重试；重连期间 Ctrl-C 无效
- **文件:行号**：`client/src/transport.rs:33-42`（`BACKOFF_INITIAL_MS=100`，`BACKOFF_MAX_MS=1000`，`backoff_delay` 翻倍封顶 1s，无 jitter）；`:538-560`（`connect_with_retry`：`loop { match connect_once … Err → sleep(delay) → attempt+1 }`，**无最大次数、无总时限、无对 `hello_reject` 的区分**）；`client/src/main.rs:300-330`（主循环只在 `run_session` 的 `select!` 里监听 `ctrl_c`，`connect_with_retry` 期间 SIGINT 无人处理）。
- **为什么重要**：Hub 停机时两台 Client 各以 1 次/秒的频率永重试（无 jitter，两台还可能同步化，形成 1s 周期的重连脉冲）；Token 配错时同样永重试，Hub 日志被 `hello_reject` 刷屏，且 Client 永远不退出、用户无从得知是配置错；Hub 宕机期间用户 Ctrl-C 也退不出 Client（必须 kill）。NFR-05 要求“断网 30s 恢复 1s 内重连”，当前退避满足恢复速度，但以“永不放弃 + 固定 1s 节拍”为代价换来重连风暴与配置错误不可见；对 256MB 设备也是持续的 CPU/日志开销。
- **建议修复**：退避加 jitter（如 ±50%）与更合理的上限（如 5-30s）；`hello_reject`（认证失败）重试 3 次后以非零退出并打明错（配置错不应重试）；`connect_with_retry` 接受一个 shutdown 信号（Ctrl-C）并在每次重试间隔里监听。

### P1-44 自动发现扫错了方向：音箱是 WS 客户端、根本不监听 :4399，生产扫描永远找不到设备
- **文件:行号**：`hub/internal/discovery/discovery.go:47-89`（`Scanner.New(4399, token)`，`ProbeModel` 对候选 IP 的 `:4399/ws` 做 WS 握手发 hello）；`hub/cmd/hub/main.go:449-473`（启动时扫一次，`discovery.New(4399, token)`，`TLS` 字段恒为 false）；`client/src/transport.rs` 与 `client/src/main.rs`（Client 只有出站 `connect_async`，无任何监听 socket）。
- **为什么重要**：发现逻辑假设“音箱在 :4399 上监听”，但协议是 Client 主动连 Hub，音箱侧没有任何 server。生产扫描对每个 IP 都是连接被拒，`Scan` 永远返回空（且 `Scan` 恒返回 `nil` 错误，主循环连失败都感知不到）。测试之所以通过（`discovery_test.go`），是因为测试里起了一个假 transport server——测的是“能找到 server”，不是“能找到音箱”。另：即使某 IP 真有服务，`ProbeModel` 先试 `lx06` 再试 `oh2p`，而 Hub 的 `handleWS` 只校验 model 在 `lx06/oh2p` 之内、不校验真实型号（`transport.go:208-212`），第一个探针（lx06）对任何一台音箱都会“成功”，型号会被误标为 lx06。
- **建议修复**：改发现方式：音箱侧无需改动——Hub 应以 hello 注册为唯一事实来源，删除或重写扫描（例如 mDNS 广播：Client 启动时发 mDNS `_open-xiaoai._tcp` 通告，Hub 监听；或 Hub 定期向网段发 UDP 发现包、Client 回应）；若保留 TCP 探针，必须校验型号真实性（不能靠自报 model），并把 `Scan` 的错误如实返回。

### P1-45 配置校验形同虚设：关键字段不校验、路由缺失时 Agent 随机选、音乐文件夹“启动校验”根本没写
- **文件:行号**：`hub/internal/config/config.go:112-130`（`Validate()` 只查 `wake_word` 非空、`hub.listen` 非空、`devices[].model`、`music_folder` 非空）；`hub/cmd/hub/main.go:258-262`（回退分支 `for _, a := range cfg.Agents { agentClient = …; break }`，Go map 随机顺序）；`hub/internal/config/config.go:26-27` 与 `hub/cmd/hub/main.go`（注释写“启动时自动校验，不存在再问”，但 main 全文件无 `os.Stat(cfg.MusicFolder)`）。
- **为什么重要**：以下配置错误都在启动时放行、运行时静默错：(a) `do_not_disturb: "23:00-7:00"`（`time.Parse("15:04")` 要求零填充，非法格式在 `alerts.InDoNotDisturb` 里按“不在勿扰”处理——见 `alerts.go:131-151`，即半夜漏水照常大声播报）；(b) `scan_cidr` 非法 → `hostsInCIDR` 返回 nil，扫描静默无结果；(c) `wake_word_routes` 指向不存在的 Agent 名 → 回退到随机 Agent（见 P1-38）；(d) `agents[].base_url` 为空或非法 URL → 每次对话才报错；(e) `home_assistant.url` 为空 → main 静默跳过 HA（`:237` 的 `if != ""`），FR-06 全灭且无告警；(f) `music_folder` 不存在 → 无人校验，MPD 挂载空目录后点歌失败才暴露；(g) `config.yaml` 种子设备 `livingroom-speaker`/`bedroom-speaker` 的 `ip: ""` 通过校验，以空 IP 进注册表。
- **建议修复**：`Validate()` 补全：`do_not_disturb` 用 `alerts.InDoNotDisturb` 同款解析预检、`scan_cidr` 用 `net.ParseCIDR` 预检、`wake_word_routes` 的值必须在 `agents` 里存在、`agents[].base_url` 必须可解析、`home_assistant.url` 非空时必须可解析；启动时 `os.Stat(music_folder)`，不存在则明确告警（或按注释“再问”）；种子设备空 IP 应在首次发现后被替换而非长期残留。

### P1-46 部署物无时区：容器默认 UTC，循环提醒与勿扰时段整体错 8 小时
- **文件:行号**：`deploy/Dockerfile.hub` 与 `deploy/docker-compose.yml` 全文件无 `TZ` 环境变量、`tzdata`、`/etc/localtime` 挂载（`grep -ri tz` 零命中）；`hub/internal/reminders/reminders.go:164-175`（`NextDaily` 用 `now.Location()` 即容器本地时区）；`hub/internal/alerts/alerts.go:131-151`（`InDoNotDisturb` 用 `now.Hour()`）；`hub/cmd/hub/main.go:342-356` 与 `:477-485`（提醒与告警都以容器本地时间为准）。
- **为什么重要**：`debian:bookworm-slim` 默认时区 UTC，NAS 在杭州（UTC+8）。“每天 8 点提醒吃药”会在北京时间 16:00 触发；勿扰 `23:00-07:00` 在容器里是 UTC 23:00-07:00，即北京时间 07:00-15:00——白天该响的漏水告警被静默，半夜该静默的反而播报。FR-14（P1）与 FR-10（P2，半夜也响/勿扰除外）的验收标准直接建立在“北京时间”上；时区错 8 小时是系统性错误，不是边界 case。疑似程度低（debian slim 默认 UTC 是确定行为），但仍建议在 NAS 上实测确认。
- **建议修复**：compose 给 hub 服务加 `TZ=Asia/Shanghai`（并在 Dockerfile 装 `tzdata` 或挂载宿主 `/etc/localtime:/etc/localtime:ro`）；同时在配置校验里要求 `do_not_disturb` 格式合法（见 P1-45），并在 Web 状态页显示 Hub 当前时区与时间。

---

## P2 — 健壮性 / 可维护性（56 项）

### P2-1 Hub 自签证书 10 年有效、SAN 只有回环地址、指纹校验忽略有效期与主机名
- **文件:行号**：`hub/internal/tlscert/tlscert.go:39-106`（`LoadOrCreate()`：RSA 2048，`NotAfter: 10*365天`（:79），`IPAddresses: [127.0.0.1]`，`DNSNames: [localhost, open-xiaoai-hub]`；证书/私钥均 `0600`、目录 `0700`——这部分是好的）；`client/src/transport.rs:119-128`（`FingerprintVerifier::verify_server_cert()` 忽略 `_server_name` 与 `_now`，只比指纹）；`:43-61`（证书存在但 `LoadX509KeyPair` 失败即返回错误，`main.go:416-418` 随即 `log.Fatalf`）。
- **为什么重要**：10 年有效期意味着私钥一旦泄露（`hub-data/tls/hub.key` 在 NAS 卷里），攻击者可用同一证书冒充 Hub 长达 10 年，而客户端 pin 的就是该证书指纹，不会过期失效。SAN 里没有 NAS 的局域网 IP/主机名，非 pin 客户端会校验失败。证书损坏/截断会让 Hub 无法启动，只能人工删文件。
- **建议修复**：有效期缩短到 1-2 年并做轮换流程（新证书 + 指纹经安全通道下发）；SAN 加上 Hub 的局域网 IP/主机名；指纹校验同时校验 `NotAfter`；加载失败时备份损坏文件并重新生成（记告警日志）；启动时检查剩余有效期并告警。

### P2-2 Token 比较非恒定时间，且 hello 的拒绝原因可区分枚举
- **文件:行号**：`hub/internal/transport/transport.go:206`（`hello.Token != s.token`，Go 字符串 `!=` 非恒定时间）；`:196-214`（首条非 hello、hello 解析失败、bad token、unsupported model 各回不同的 `reason`）。
- **为什么重要**：局域网内可计时的攻击者理论上能逐字节推断 Token（实际利用难度高，因网络抖动大，故列 P2 而非 P0）；不同的 `reason` 还能让攻击者区分“Token 对了但型号不对”与“Token 错了”，辅助枚举。
- **建议修复**：Token 比较改 `crypto/subtle.ConstantTimeCompare`（先判长度）；hello 失败统一回 `hello_reject` 且不区分原因（日志里可区分）。

### P2-3 `room_claim` / hello / status 字段无长度与内容校验：超长房间名可进注册表、配置文件与日志
- **文件:行号**：`hub/cmd/hub/main.go:139-152`（`OnRoomClaim()`：只 `TrimSpace`、判空，即 `SetRoom` + `Save(config.yaml)`，无长度/字符集限制）；`hub/internal/transport/transport.go:26-55`（`Hello/Wake/RoomClaim/Status` 结构体无任何长度校验，`Room/Version` 原样进注册表并经 `/api/devices` 返回）；`hub/cmd/hub/main.go:157`（`log.Printf("房间认领：%s → %s", …)`，房间名含 `\n` 可伪造日志行）。
- **为什么重要**：已鉴权设备（或持 Token 者）可认领超长/含换行符/控制字符的房间名，污染 `config.yaml`、Web 面板与日志；超长 `Version`/`Room` 也会进注册表快照。语音认领本应是“我说了客厅就是客厅”，但无任何归一化（大小写/空白/同义）与长度上限。
- **建议修复**：房间名限长（如 ≤16 字符）、只允许中文/字母/数字/常用标点、拒绝控制字符与换行；日志输出前做清洗；`Hello` 的 `Model/Room/Version` 同样限长。

### P2-4 配置与用户语音数据文件均为 `0644`、数据目录 `0755`：同 NAS 多用户可读
- **文件:行号**：`hub/internal/config/config.go:189-198`（`Save()`：`os.WriteFile(tmp, b, 0644)`）；`hub/internal/reminders/reminders.go:100-110`（提醒 `saveLocked()` 同样 `0644`）；`hub/internal/router/memory.go:66-76`（记忆 `saveLocked()` 同样 `0644`）；`hub/cmd/hub/main.go:226`（`os.MkdirAll(cfg.Hub.DataDir, 0755)`）；`hub/internal/tlscert/tlscert.go:46,91,94`（证书目录 `0700`、证书/私钥 `0600`——这部分正确，可作参照）。
- **为什么重要**：`config.yaml` 本身不含密钥（Token 走环境变量，是好的），但提醒与记忆文件含用户语音原文（“每天 8 点提醒我吃药”等），`0644` 意味着 NAS 上任何本地用户/容器都可读。`hub-data` 目录 `0755` 也让同机其它用户可列目录。
- **建议修复**：提醒/记忆/配置的持久化文件改 `0600`，数据目录改 `0700`（与 `tlscert` 一致）；已有文件在启动时收紧权限。

### P2-5 Web PUT 与房间认领共用同一临时文件且用不同指针写同一配置：并发写回可互相覆盖
- **文件:行号**：`hub/internal/web/web.go:85-115`（`configAPI` PUT：先 `Save(s.cfgPath)` 再换 `s.cfg`，换指针前后无统一锁）；`hub/cmd/hub/main.go:139-152`（`OnRoomClaim()`：`_ = h.cfg.Save(h.cfgPath)`，错误被忽略，且 `h.cfg` 与 `web.Server.cfg` 是两个指针）；`hub/internal/config/config.go:189-198`（`Save()` 固定用 `path+".tmp"` 再 `Rename`）。
- **为什么重要**：Web PUT 与语音认领同时写 `config.yaml` 时：(a) 两个 `Save()` 共用同一 `.tmp` 文件，内容可能交叉；(b) `h.cfg` 是启动时的旧指针，认领写回会把 Web PUT 已改的字段（新设备、新路由）用旧内存覆盖丢；(c) `OnRoomClaim` 的 `Save` 错误被 `_ =` 丢弃，落盘失败无感知。
- **建议修复**：配置读写集中到一个带锁的 `Store`（单指针 + `sync.Mutex`），`Save` 用唯一临时文件名（如 `path+".tmp."+pid`）并检查错误；`OnRoomClaim` 写失败要记日志并回 `room` 失败给设备。

### P2-6 `patch.sh` 打包后验证不完整：只查存在与三处 grep，不查 shadow/PAM/禁 OTA 与文件权限
- **文件:行号**：`firmware-patch/scripts/patch.sh:106-134`（解开验证：只查 11 个文件存在、`dropbear` 含 `mkdir`、`inittab` 含 ash、`asound.conf` 含 Capture、LX06 的 `libxaudio` 无 `hw:0,3`）；`firmware-patch/scripts/patch_firmware.py:287-334,371-410`（`patch_login`/`patch_disable_ota` 自带校验，但只在打包前对 `squashfs-root` 副本做，打包/解包循环后不再复核）。
- **为什么重要**：打包后真正决定安全的是：shadow 的 root 行是不是新 hash、PAM 的 `libmico` 是否已注释、`bin/ota`/`flash.sh`/`wireless ota()` 是否已禁、`/etc/shadow` 权限是否仍受限——这四项在 `patch.sh` 的验证里一项都没查。`mksquashfs -all-root` 后若某步静默损坏（如 `unsquashfs -no-exit-code` 掩盖的错误），产物仍会显示“打包完成”。
- **建议修复**：打包后验证补齐：shadow 含新 hash 且无其它可登录空密码行、PAM 无 `auth sufficient libmico`、三处 OTA 入口均含 `return 1`/`download_fail`、关键文件权限符合预期；任一失败即删半成品（已有 `fail()` 机制）。验证步骤复用 `patch_firmware.py` 的 `check_*` 函数对解开后的目录再跑一遍。

### P2-7 上游 HTTP 响应体无上限读取，错误信息回显原文
- **文件:行号**：`hub/internal/music/music.go:312`（`MAClient.command()`：`io.ReadAll(resp.Body)` 无上限）；`hub/internal/agent/agent.go:86,124`（`Chat/ChatStream` 错误分支：`b, _ := io.ReadAll(resp.Body)` 后把全文拼进 `fmt.Errorf("agent %s 返回 %d: %s", …)`）；`hub/internal/music/music.go:186-260`（网易云 `Search/SongURL/Lyric` 用 `json.NewDecoder(resp.Body).Decode`，无 `MaxBytesReader`）；`hub/internal/web/web.go:92,123`（`configAPI/otaAPI`：`json.NewDecoder(r.Body).Decode` 无 `http.MaxBytesReader`）。
- **为什么重要**：恶意/异常上游（MA、Agent、网易云）可返回超大 body 打满 Hub 内存；错误信息把上游原文带进 Hub 日志，可能泄露上游内部信息。Web 侧同样无 body 上限，PUT 超大 JSON 可耗内存（结合 P0-1 更严重）。
- **建议修复**：全部外部 body 用 `io.LimitReader`/`http.MaxBytesReader` 限长（如 MA/Agent 1MB、网易云 1MB、Web PUT 64KB）；错误信息截断到前 200 字符并脱敏。

### P2-8 `patch_firmware.py` 的 `--password` 与 hash 进进程参数/日志
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:578-579`（`--password` 命令行参数）；`:559,587-588`（`login_hash` 进返回字典并被 `print`）。
- **为什么重要**：`--password xxx` 会进 shell 历史与 `ps` 输出；`login_hash` 被打印到终端/日志，与 P0-2 的弱口令叠加。
- **建议修复**：`--password` 改为只从环境变量/交互式 `getpass` 读，禁止命令行传密码；不打印 hash，只打印“已更新”（或仅在显式 `--show-hash` 时打印）。

### P2-9 版本号校验正则未转义 `.`，可含 `/` 造成路径穿越（仅限本地操作员自攻击）
- **文件:行号**：`firmware-patch/scripts/patch.sh:36-41`（`case "$VERSION" in [0-9]*.[0-9]*.[0-9]*)`，`.` 未转义，可匹配任意字符）；`firmware-patch/scripts/unpack.sh:24-29`（同款正则）；`firmware-patch/scripts/patch.sh:49-51`、`unpack.sh:31-33`（`DIR="$WORK/${MODEL}_${VERSION}"`，`OUT="$DIR/root-patched.squashfs"` 直接拼路径）；`firmware-patch/scripts/download-ota.py:78-81`（不校验）。
- **为什么重要**：`[0-9]*.[0-9]*.[0-9]*` 中的 `.` 是 case 通配符（任意单字符），`1/2/3`、`../../x` 类字符串也可能匹配通过，随后被拼进 `DIR`/`OUT` 路径。调用者是本地操作员本人，自攻击意义有限，故列 P2；但若将来该脚本被 Web/自动化调用，即升级为路径穿越。`patch.sh:69` 的 `rm -rf "$SQFS_ROOT"` 会删任意目录树，一次手误即丢数据。
- **建议修复**：三处统一用严格正则 `^[0-9]+\.[0-9]+\.[0-9]+$` 校验 VERSION，MODEL 只允许 lx06/oh2p（已有）；`download-ota.py` 同理。

### P2-10 Sherpa `.so` 路径来自配置文件任意指定，ARM 32 位结构体尺寸未锚定（疑似，需 ARM 实测确认）
- **文件:行号**：`client/src/sherpa.rs:200-260`（`SherpaSpotter::open()`：`lib_path` 非空时只试该路径，`libloading::Library::new(cand)` 直接 `dlopen`）；`client/src/config.rs:64-70`（`kws_lib` 路径来自 `client.toml`）；`client/src/sherpa.rs:480-510`（`c_api_struct_sizes_match_c_header` 只在 `#[cfg(target_pointer_width = "64")]` 下断言 `OnlineModelConfig=136` 等尺寸，32 位 ARM 无锚定）。
- **为什么重要**：`kws_lib` 指向的任意 `.so` 会被客户端进程（音箱上以 root 运行）`dlopen` 并直接调其 C 函数：若 `client.toml` 可被 Hub OTA/其它写路径改写，等于留了一个“指定任意 .so 即代码执行”的口子（当前 OTA 尚未接通写配置，故为潜在项）。另 32 位 ARM 上 `repr(C)` 结构体的指针为 4 字节，尺寸与 64 位不同是预期的，但代码未在 ARM 上实测锚定各字段偏移，FFI 布局一旦与 sherpa 2.0.1 头文件不符即内存不安全。
- **建议修复**：`kws_lib` 只允许白名单路径（如 `/data/open-xiaoai/kws/lib/libsherpa-onnx-c-api.so`），拒绝其它路径；ARM 上跑一遍结构体尺寸/偏移探针测试并记录实测值；`.so` 文件校验 SHA256 后再加载。

### P2-11 HA 事件 handler 在读循环 goroutine 里同步执行：告警 TTS 会堵住 HA 结果分发
- **文件:行号**：`hub/internal/ha/ha.go:104-145`（`readLoop()`：事件 handler 在读循环 goroutine 里同步执行；`alerts` 的播报回调会调 `speakText` 做 TTS+Opus 编码，阻塞 HA 事件分发）；`hub/cmd/hub/main.go:477-485`（`watcher.Start()` 只调一次 `SubscribeEvents`，HA 断线后 `readLoop` 退出，订阅永久丢失）。
- **为什么重要**：一次漏水事件的 TTS（数秒）会堵住 HA 的 `result` 分发，使并发的家电命令超时（10s）；回调 panic 会直接杀死 `readLoop`。
- **建议修复**：事件回调改为异步投递（有界 channel + 工作 goroutine）；回调加 recover；结合 P1-27 的 HA 重连，恢复后重新订阅。

### P2-12 紧急呼叫无二次确认：任何语音（含电视/录音/对讲广播）说“救命/打120”即触发
- **文件:行号**：`hub/internal/router/router.go:170-230`（`emergency()`：任何语音——含电视/录音/对讲广播——说“救命/打120”即触发紧急呼叫，无声纹/二次确认；取消只按 `deviceID` 隔离，同一 IP 的设备可互相取消，见 P1-3 的 deviceID 问题）。
- **为什么重要**：电视里的一句台词、录音回放都可能触发紧急呼叫 + Notify，结合 P1-18 的谎报成功，误报链完整。
- **建议修复**：紧急呼叫加二次语音确认（“确认要呼叫急救吗”）或声纹/按键确认；取消按设备唯一 ID 隔离。

### P2-13 提醒调度器可重复 Start 导致双倍触发；Stop 从不调用
- **文件:行号**：`hub/internal/reminders/reminders.go:177-201`（`Start` 无“已启动”守卫，每调一次多一个 ticker goroutine；`Stop` 在 `main.go` 全文无调用）。
- **为什么重要**：两次 `Start` → 两个 goroutine 各扫各的，同一条提醒触发两次 `onFire`（双倍 TTS）。当前 `main.go` 只调一次，故为隐患而非现网故障。
- **建议修复**：`Start` 加 `started bool` 守卫（重复调直接 return）；`main.go` 退出信号处调 `remStore.Stop()`。

### P2-14 提醒 tick/`Cancel` 的落盘错误被吞掉：一次性提醒删了但没存住，重启后复活重播
- **文件:行号**：`hub/internal/reminders/reminders.go:230`（`tick` 里 `_ = s.saveLocked()`）、`:133`、`:148`（`Cancel`/`CancelByText` 同样 `_ =`）。
- **为什么重要**：`tick` 先在内存里删掉一次性提醒/推进下一次，再落盘；落盘失败（磁盘满、权限）被吞，进程重启后从旧文件重新加载，提醒复活并再次触发。
- **建议修复**：落盘失败记 error 日志并（对一次性提醒）保留“已触发待落盘”标记或重试保存；`Cancel` 的保存失败应返回 error 给 Router 播报。

### P2-15 `Memory.Set` 用秒级时间戳做 key，同一秒内两次“记住”会互相覆盖
- **文件:行号**：`hub/internal/router/memory.go:44-52`（`m.data[deviceID][time.Now().Format(time.RFC3339)] = text`）。
- **为什么重要**：与 `TEST_REPORT.md` 已修复的提醒 ID 毫秒碰撞是同一类 bug，只是换了个地方：RFC3339 只精确到秒，1 秒内两条记忆用同一个 key，后一条覆盖前一条。
- **建议修复**：key 改为纳秒时间戳 + 自增序号（复用 `reminders` 的 `idSeq` 模式），或改用 `[]string` 追加。

### P2-16 HA `call()` 写失败时泄漏 pending 项
- **文件:行号**：`hub/internal/ha/ha.go:165-195`（`WriteJSON` 出错直接 `return nil, err`，未删 `c.pending[id]`）。
- **为什么重要**：每次写失败都留一个永不回收的 `chan resultMsg` 与 map 项；`readLoop` 之后若收到该 id 的 result，会发向一个无人接收的 buffered channel（`select default` 丢弃，倒不会阻塞），但 map 持续增长。
- **建议修复**：写失败分支里 `delete(c.pending, id)`；超时分支已有同样处理，补齐即可。

### P2-17 Wyoming `Transcribe`/`Synthesize` 全程无超时，未知事件类型会死循环
- **文件:行号**：`hub/internal/wyoming/wyoming.go:100-160`（`for { header, _, err := c.readEvent() ... }`，只有 `Describe` 用了 `writeWithDeadline`）。
- **为什么重要**：服务端挂起或只发未知类型事件时，循环永不退出且无 deadline；`answer`（P1-29）因此可被无限期拖住。
- **建议修复**：`Transcribe`/`Synthesize` 入口设整体 deadline（如 STT 60 秒、TTS 30 秒），读循环遇未知类型记日志并计数，超限即报错。

### P2-18 `set_volume` 参数非法时静默重置音量为 50
- **文件:行号**：`client/src/main.rs:174-185`（`.and_then(|x| x.as_u64()).unwrap_or(50)`）。
- **为什么重要**：`volume` 为浮点、字符串、负数时 `as_u64()` 得 None，音量被置为 50 而不是“忽略本次调用”。一次格式错误的 RPC 就会把用户音量改掉。
- **建议修复**：取不到合法值时保持原音量并回 `ok:false`（或忽略），不要用 50 兜底。

### P2-19 非推流时仍每帧编码 Opus，白白消耗音箱 CPU
- **文件:行号**：`client/src/main.rs` 捕获任务（`audio.encode_pcm(&pcm)` 在 `wake_tx`/`audio_tx` 发送前无条件执行；`run_session` 非 streaming 时照常 drain 后丢弃）。
- **为什么重要**：256MB 音箱上 50 帧/秒的 Opus 编码持续跑，只为编码出随后被丢弃的帧；KWS 与编码同任务，CPU 争用会抬高唤醒延迟。
- **建议修复**：捕获任务只在“已收到 wake、等仲裁”或 streaming 时才编码；平时只喂 KWS 算 RMS。或把编码移到主循环 streaming 分支内做。

### P2-20 KWS 推理失败每帧打一条 warn，日志刷屏
- **文件:行号**：`client/src/kws.rs:141-175`（`Err(e) => tracing::warn!("sherpa feed failed: {e}")`，每 20ms 一帧）。
- **为什么重要**：模型持续出错时每秒 50 条 warn，淹没真正有用的日志，也占 CPU/闪存写寿命。
- **建议修复**：失败日志节流（如 1 秒一条 + 计数），或连续失败 N 次后停掉 sherpa 后端并报一次错。

### P2-21 Sherpa `is_ready` 循环无上限，模型卡住则录音任务永久阻塞（疑似，需真机/实测确认）
- **文件:行号**：`client/src/sherpa.rs:404-435`（`while (self.api.is_ready)(...) != 0 { decode }`，无迭代上限、无超时）。
- **为什么重要**：若 `is_ready` 因模型/音频异常始终返回非零，循环永不退出，录音任务（KWS + 采集 + 编码全在该任务）永久卡住，音箱变“聋子”且无任何日志。代码层面可确认无上限；是否真会卡住取决于 sherpa 内部行为，故标疑似。
- **建议修复**：循环加迭代上限（如 100 次）与看门狗，超限返回 error 并 reset stream。

### P2-22 AEC 零能量参考信号时 NLMS 步长爆炸
- **文件:行号**：`hub/internal/aec/aec.go:58-90`（`step := n.mu * e / (norm + n.eps)`，`eps = 1e-6`；`ProcessFrame` 对短于 mic 的 ref 补 0）。
- **为什么重要**：参考信号全零/静默时 `norm ≈ 0`，步长 ≈ `0.5*e/1e-6`，滤波器系数瞬间发散；之后参考信号一恢复，输出即爆炸。目前 `EchoCancel` 生产零调用（P1-15），故为 P2；一旦接线就会在“没放音时对讲”场景触发。
- **建议修复**：参考能量低于阈值时跳过自适应更新（只输出 `mic - y` 或直接输出 mic）；或 `step` 按 `mu*e/(norm+eps)` 再钳制上限。

### P2-23 `download-ota.py` 无参数校验、无下载超时
- **文件:行号**：`firmware-patch/scripts/download-ota.py:78-105`（`model, version = sys.argv[1], sys.argv[2]`，无参数个数/型号校验；`urllib.request.urlretrieve(link, out)` 无 timeout）。
- **为什么重要**：无参数时抛 `IndexError` 堆栈而非用法提示；`model` 未校验可拼出任意 URL；下载无超时，网络挂起时脚本无限等待。下载后有 MD5/大小校验兜底，故 fail-closed，P2。
- **建议修复**：`argparse` 校验型号/版本格式；`urlretrieve` 改为带 `timeout=60` 的 `urlopen` + 流式写文件。

### P2-24 `nas-install.sh` 多处检查水分：未查 4399 端口、容器计数靠肉眼、sed 改 HA 地址无校验、远端命令注入面
- **文件:行号**：`deploy/nas-install.sh` §5（`docker compose ps` 只打印不计数，却断言“8 个容器都应为 Up”）、§1（端口检查清单 `10300 10200 1883 8095 3000 8080`，漏了 Hub 的 `4399` 与 Snapcast 的 `1705/1704`）、§3（`sed -i 's#ws://192.168.1.10:8123#...#'`，若配置里 HA 地址已不是该默认值则静默 no-op，无任何替换成功校验）、`:35-40,119-123`（`$SSH "… $NAS_IP …"` 与 `sed -i 's#…#ws://$NAS_IP:8123#'` 把 `.env` 里的 `NAS_IP` 未转义地拼进远端 shell，`.env` 被篡改即远端命令注入；`ssh` 未显式校验主机密钥指纹，首次连接存在 MITM 窗口）、`:12`（`set -u`，无 `-e`/`-o pipefail`；第 1 步 docker/`/volume1/music` 检查失败只记 bad 不退出，仍继续传文件；`:100-104`（pull/up 失败无 `docker compose down` 回滚，半起的容器与已传 `.env` 留在 NAS；`:104`（`sleep 15` 固定等待，Whisper 首启下模型 ~500MB 可能超时致误报；`:27`（`NAS_IP` 用 `cut -d= -f2` + `tr` 去空白解析，带引号/注释的 `.env` 会错）；端口检查依赖 NAS 有 `ss` 与 bash `/dev/tcp`，缺失时误报；Hub `/api/status` 检查用的 curl 没加 `-f`，HTTP 500 也算过；FATAL/PANIC 检查在 ssh/docker 失败时因空输入而报 ok）。
- **为什么重要**：部署脚本的“全部检查通过”结论不可信：4399 被占要到 `up` 时才暴露，HA 地址没改成功也不会报错；脚本在“半自动部署”场景下多处 fail-open：失败继续跑、失败不清理、检查假阳性/假阴性并存。首次上 NAS 时最需要脚本诚实报告状态；假阳性会让人误以为部署成功。
- **建议修复**：`set -euo pipefail` 并重构错误处理；第 1 步 `FAIL>0` 即退出；失败时 trap 执行 `docker compose down`；`sleep` 改轮询健康检查带超时；校验 `NAS_IP` 格式（`^[0-9.]+$`）并用 `printf %q` 转义远端参数；检查命令先探 `ss` 是否存在；curl 一律 `-f`；端口清单补 `4399 1705 1704`；`ps` 后 `grep -c " Up "` 计数 == 8 才 PASS；sed 后 `grep -q "ws://$NAS_IP:8123"` 校验替换成功；显式校验 SSH 主机密钥。

### P2-25 勿扰时段格式非法时静默按“不在勿扰”处理，深夜可能误播报
- **文件:行号**：`hub/internal/alerts/alerts.go:121-138`（`InDoNotDisturb` 解析失败返回 `false`）。
- **为什么重要**：配置写错（如 `"23:00-7"`、`"23:00:00-07:00"`）时 fail-open：FR-10 半夜也播报，与勿扰设计相反，且无任何日志告警。
- **建议修复**：`config.Validate` 校验 `DoNotDisturb` 格式，非法即启动失败；或解析失败时记 warn 并按“在勿扰”保守处理，二选一。

### P2-26 安全事件判定只用子串匹配，易误判
- **文件:行号**：`hub/internal/alerts/alerts.go:52-63`（`KindFor`：entity_id 含 `leak`/`smoke`/`water` 任一子串即算；不校验 domain 是否为 `binary_sensor`）。
- **为什么重要**：`sensor.water_heater_status`（热水器状态为 on）会被判为“水浸”播报；`sensor.smoke_detector_battery` 之类非报警实体同样误判。
- **建议修复**：`KindFor` 要求 `strings.HasPrefix(entityID, "binary_sensor.")` 且 state 为 on（`ShouldAnnounce` 已要求 on，但 domain 未限）；或维护“安全实体 domain + 类型词”白名单。

### P2-27 同房间多设备时 `ByRoom` 随机取一台；启动播种的占位设备成幽灵行
- **文件:行号**：`hub/internal/registry/registry.go:112-122`（`ByRoom` 遍历 map，第一个在线即返回，Go map 迭代顺序随机）；`hub/cmd/hub/main.go` 启动播种 + `hub/config.yaml` 的 `devices:`（`livingroom-speaker`/`bedroom-speaker` 两个 IP 为空的占位）。
- **为什么重要**：（a）两台设备认领同一房间时，房间定向提醒每次随机挑一台播报；（b）占位设备永不匹配真实设备（真实 ID 是 `model-IP` 格式），却永久显示在 `/api/devices` 里，造成“有 4 台设备、2 台离线”的假象。
- **建议修复**：（a）`ByRoom` 返回全部在线设备，调用方（提醒播报）向同房间全部设备播报，或 `SetRoom` 拒绝重复认领；（b）删除配置里的占位设备，改为启动时无设备、发现后写入。

### P2-28 `decoders` 永不回收
- **文件:行号**：`hub/cmd/hub/main.go:63-75`（`decoderFor` 只增不减，`OnDisconnect` 不删）。
- **为什么重要**：每台设备一个 Opus 解码器常驻，设备换 IP/重注册即泄漏一个（量小，2 台场景可忽略）；残留语音在设备重连后可能与新语音拼接后一起结算。
- **建议修复**：`OnDisconnect` 删 `decoders[deviceID]`（需给 `opus.Decoder` 补 `Close` 或直接丢弃）。

### P2-29 发现探针会用 `Version: "probe"` 覆盖真实版本号
- **文件:行号**：`hub/internal/discovery/discovery.go` 的 `ProbeModel`（hello 里 `version: "probe"`）+ `hub/internal/registry/registry.go` 的 `Upsert`（非空 Version 即覆盖）。
- **为什么重要**：启动扫描期间探针成功后，设备在注册表里的 Version 先被写成 `"probe"`，直到真实 hello 才纠正；若探针发生在设备已连接后，还会叠加 P1-12 的顶号竞态。
- **建议修复**：`ProbeModel` 成功后不经 `Upsert` 写 Version（只写 Model/IP），或 `Upsert` 增加“不覆盖已有非空 Version 除非来自真实 hello”的来源标记。

### P2-30 mksquashfs 参数与原厂不完全一致，镜像增大且无预警阈值
- **文件:行号**：`firmware-patch/scripts/patch.sh:87-93`（注释称“与原始参数一致”）；实测原 `root.squashfs` superblock：`Always-use-fragments option is not specified`、`Filesystem is exportable via NFS`、`Xattrs are not stored`。
- **为什么重要**：`patch.sh` 加了原厂没有的 `-always-use-fragments`、加了 `-no-exports`（原厂是 exportable）；xz 字典大小未按原厂参数匹配，镜像比原厂大 952,549 字节（38,453,248 vs 37,500,699），余量 3,489,792 字节；大小检查只有 40MB 硬上限，没有预警线，后续补丁再加内容可能悄悄逼近上限。
- **建议修复**：按原厂 superblock 对齐打包参数（去掉 `-always-use-fragments`/`-no-exports` 或论证无害）；试出与原厂一致的 xz 字典参数；40MB 上限外加 38MB 预警线。

### P2-31 `patch.sh` 里 `file` 解析的显式检查是死代码
- **文件:行号**：`firmware-patch/scripts/patch.sh:79-84`；已实测：`set -euo pipefail` 下，`$(…)` 里 grep 找不到匹配时脚本直接退出（ERR trap 清理），走不到 `[ -n "$COMPRESSION" ] || fail`。
- **为什么重要**：失败时仍安全（trap 删 `OUT`），但报错信息丢失，操作者看不到“读不到压缩方式”这类明确提示；且 `SQFS_ROOT` 不会被 trap 清理。
- **建议修复**：关掉这段的 pipefail/set -e（如用 `|| true` 包裹后显式判断），或把检查移到 trap 之外；trap 里顺带清理 `SQFS_ROOT`。另：用 `unsquashfs -s` 的结构化输出（或 `binwalk`/`sasquatch` 的字段）取压缩方式与 blocksize，不要解析 `file` 的人类可读输出。

### P2-32 `patch_firmware.py` 不可重复执行（非幂等）
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:537-550`（`preflight`）；`:240`（`patch_ssh` 要求 enable 行仍在）；`:287`（`patch_login` 要求 pam_unix 行仍被注释）；`:368`（`patch_disable_ota` 要求函数头后尚未 `return 1`）。
- **为什么重要**：对已补丁 rootfs 再跑一次会直接 preflight 失败；想只改 root 密码也必须重走 unpack→patch 全流程。`patch.sh` 可重复跑只是因为它每次 `rm -rf` 重拷 pristine rootfs（`:69`），不是补丁本身幂等。运维上改密码、补打单个补丁都没有便宜路径；容易诱使操作者去手工改已补丁镜像。
- **建议修复**：文档写明“补丁只对 pristine rootfs 跑一次”；或加 `--password-only` 模式（仅重写 shadow hash 并校验）。

### P2-33 asound 改动无备份，设备端不可逆
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py` `patch_asound`（`_replace_pcm_block` 整块替换原 `mico_record`/`noop`，不留原内容；`RATE_CONVERTER_LINE` 追加）。
- **为什么重要**：补丁镜像里没有任何原厂 asound 的备份；设备上改坏/想恢复原厂录音链时，只能重刷 boot1 原厂系统或重新走 unpack+patch 流程，没有“还原音频配置”的就地操作。音频链是真机最可能出问题的部分（ALSA 接线仍待真机验证），恢复路径越重，调试成本越高。
- **建议修复**：镜像内留 `/etc/asound.conf.orig` 与 `asound.conf.dts.orig`，或附一个 revert 脚本；并在文档里写清恢复路径。

### P2-34 `/data/init.sh` 是无校验的开机 root 钩子
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:42`（`RC_LOCAL_LINE`）、`:422-433`（`patch_start`）。
- **为什么重要**：`rc.local` 在每次开机以 root 执行 `/data/init.sh`（只要文件存在），不验签名、不验 hash；`/data` 分区刷 rootfs 时不动，钩子跨刷机持久存在；执行输出丢到 `/dev/null` 且后台运行，失败静默。任何能写 `/data` 的路径（未来 Hub OTA、文件同步 bug、被攻破的客户端）都等于持久 root；且排障时看不到它的日志。
- **建议修复**：至少对 `init.sh` 做 hash pin（启动时比对记录值，不符则拒绝执行并告警）；执行日志写到 `/data/open-xiaoai/init.log`；文档里标明这是有意留下的信任根。

### P2-35 OTA 禁用有残留路径未验证（疑似，需真机/实测确认）
- **文件:行号**：`firmware-patch/scripts/patch_firmware.py:368-405`（只禁 `bin/ota` 的 `download_upgrade`/`upgrade`、`flash.sh` 的 `upgrade_param_check`、wireless 的 `ota()`）；已核对的原厂调用链：`/etc/crontabs/root` 每日 3 点跑 `/bin/ota slient` → `slient_check_and_upgrade` → `check_and_upgrade` → `upgrade()`（已被 `return 1` 截断），`bin/ota` 的 check/ble/upgrade/test 子命令也都经 `upgrade()`。
- **为什么重要**：三处脚本入口与 cron 触发链已确认堵住，但 `check()` 调的 `matool_check_upgrade` 是二进制（`usr/bin/matool`），其内部行为无法从脚本确认——它是否只查询、是否会自行下载或触发升级，未验证；cron 行本身未删，每天仍会唤醒并向小米发一次查询。若 `matool` 自身能触发下载，脚本层禁用就被绕过；即使不能，每天一次对外查询也是残留行为。
- **建议修复**：真机上抓一次 3 点 cron 的行为（或 strace `matool_check_upgrade`），确认只查询；考虑删 cron 行或把查询也禁掉；文档写清残留假设。

### P2-36 Dockerfile.hub 无 USER/HEALTHCHECK、无 .dockerignore、未 pin
- **文件:行号**：`deploy/Dockerfile.hub:8`（`FROM debian:bookworm-slim` 浮动标签）；`:14`（`COPY` 预编译二进制，无校验）；全文件无 `USER`、无 `HEALTHCHECK`；`deploy/` 下无 `.dockerignore`（构建上下文含带密钥的 `.env`）。
- **为什么重要**：容器以 root 运行；`.env` 随构建上下文发给 Docker daemon（虽未 `COPY` 进镜像，但多一次泄露面）；基础镜像与二进制均无版本/哈希钉死，重建不可复现，旧二进制被静默打进镜像。
- **建议修复**：加 `USER nobody`（或专用用户）+ `HEALTHCHECK`（curl `/api/status`）；加 `.dockerignore` 排除 `.env`；`FROM` pin 摘要，构建时校验 `hub-linux-amd64` 的 SHA256。

### P2-37 compose 无资源限制、无 user
- **文件:行号**：`deploy/docker-compose.yml` 全文件无 `deploy.resources`、无 `user:`。
- **为什么重要**：无内存/CPU 上限，异常服务可拖垮 NAS；NAS 是 7×24h 家用设备，一次镜像漂移或资源失控影响全家服务。
- **建议修复**：加 `mem_limit`/`cpus`；端口只绑 `127.0.0.1`（需局域网访问的再放行）；MPD 加 password 或只走 socket（与 P1-4 的修复合并实施）。

### P2-38 Hub OTA 30s 超时偏短；未来复用 flash.sh 会重写 u-boot（厂商代码风险）
- **文件:行号**：`hub/internal/ota/ota.go:55-61`（`VerifyTimeout` 默认 30s）；已核对的原厂 `bin/boardupgrade.sh:81-156`（`upboot` 用 `flash_erase`+`nandwrite` 重写多份 u-boot，无写后校验、无回退）。
- **为什么重要**：音箱冷启动 + KWS 加载 + 联网可能超过 30s，Hub 会把一次成功的升级误判为失败；若将来客户端 OTA 复用 `flash.sh` 路径，升级包里的 u-boot 会被重写，写一半断电即变砖（A/B 只保护 kernel/rootfs，不保护 bootloader）。
- **建议修复**：`VerifyTimeout` 改为可配置，默认放宽到 90s；客户端 OTA 设计文档写死“只更新非当前槽的 kernel+rootfs，绝不写 u-boot/dtb”（除非单独论证）。

### P2-39 `unpack.sh` 的 unsquashfs `-no-exit-code` 会掩盖解包错误
- **文件:行号**：`firmware-patch/scripts/unpack.sh:69`（`unsquashfs -d … -f -no-exit-code`）；`:70-78`（随后只判目录存在与 3 个文件存在）。
- **为什么重要**：`-no-exit-code` 让 unsquashfs 即使提取出错也返回 0；后续检查只看少数关键文件，解包不完整（如大量文件损坏/缺失）也能通过。损坏的 rootfs 可能一路走到补丁与刷机。
- **建议修复**：不用 `-no-exit-code`，改为容忍已知的 `/dev` 节点错误（如过滤报错输出），或解包后比对 inode/文件数（原 squashfs 2137 inodes）。

### P2-40 Client 录音任务死一次就永久失去唤醒/采集能力
- **文件:行号**：`client/src/main.rs:244-280`（录音任务：`Audio::open()` 或 `capture_pcm()` 任一出错即 `warn!` 后 `return`，任务退出，不再重试；主循环只把 `wake_alive`/`audio_alive` 置 false 继续跑传输）。
- **为什么重要**：ALSA 设备在启动瞬间未就绪（如驱动晚加载）会导致 KWS 永久死亡，只能重启进程恢复。
- **建议修复**：录音任务出错后按退避重试打开设备（上限内），而非直接退出。

### P2-41 Client 每次重连都丢音量与会话状态
- **文件:行号**：`client/src/main.rs:286-300`（每次重连 `Session::new()`，`volume` 硬编码 50，`device_id`/`room` 从 transport 重取）；`client/src/transport.rs:538-560`（重连不恢复任何状态）。
- **为什么重要**：Hub 发过的 `set_volume` 在 P1-11 的 60s 误杀重连后即丢失，音量跳回 50；`streaming`/`playing` 同样丢失，正在进行的对话/播放无恢复。
- **建议修复**：把 `Session` 移到重连循环外持有（或重连后由 Hub 下发一次 `set_volume` + 当前播放状态做状态同步）。

### P2-42 Client 配置缺省是静默的危险默认值
- **文件:行号**：`client/src/config.rs:13-22`（默认 `hub_url: wss://192.168.1.10:4399`、`token: "CHANGE_ME"`、`model: lx06`、`ram_mb: 256`）；`:186-193`（`load_from_path`：读文件任何错误都回退默认配置）；`client/src/main.rs:203-210`（仅 `warn!`，继续跑）。
- **为什么重要**：配置文件缺失、权限不足、路径写错时，Client 不报错、不退出，而是用写死的 192.168.1.10 + `CHANGE_ME` 无限重连（见 P1-43）；`model`/`ram_mb` 无任何校验（`model: "lx05"` 会在 Hub 侧被拒，但 Client 侧无提示；`ram_mb: 0` 照常上报）；无指纹时回退系统根证书的分支打的 warn 文案是“skipping fingerprint check”，实际并未跳过校验（自签证书会握手失败），误导排查。
- **建议修复**：配置文件缺失时明确报错退出（或至少每 10s 打一条 error 而非静默重试）；启动时校验 `model ∈ {lx06, oh2p}`、`ram_mb > 0`、`hub_url` 可解析；把无指纹分支的日志改为“未配置指纹，改用系统根证书校验（自签 Hub 将失败）”。

### P2-43 日志里出现用户语音文本（隐私）
- **文件:行号**：`client/src/main.rs:162-171`（`info!("rpc play_text: {text}")`，把 Hub 下发的 TTS 文本全文记日志）；`hub/cmd/hub/main.go:360`（`log.Printf("TTS 不可用，无法播报给 %s: %s", deviceID, text)`，把提醒/告警全文记日志）。
- **为什么重要**：语音助手播报的文本常含提醒事由、通知内容等私人信息，进日志即进 NAS 日志文件（可能随 `docker compose logs` 被贴出排查）。其余位置处理得当：Agent 请求、HA Token、LLM Key、Client Token 均未进日志，值得肯定。
- **建议修复**：两处日志只记长度/前 N 字摘要或脱敏（如 `text[:12]+"…"`），或降为 debug 级别。

### P2-44 提醒存储：每秒全量写盘、损坏文件静默丢全部提醒、Stop 后无法再启动、到点回调阻塞调度
- **文件:行号**：`hub/internal/reminders/reminders.go:202-237`（`tick()` 每秒调一次 `_ = s.saveLocked()`，无论有无变化）；`:69-90`（`Load()` 的 JSON 错误原样返回）；`hub/cmd/hub/main.go:267-269`（`_ = remStore.Load()`、`_ = memory.Load()`，错误被丢弃且无日志）；`:177-200`（`Start()` 用 `stopCh`，`Stop()` 关闭后再次 `Start()` 会立即退出，因为新 goroutine 一启动就读到已关闭的 channel）；`hub/cmd/hub/main.go:342-356`（`onFire` 回调内同步做 TTS 合成 + 网络发送，多个到点提醒会串行拖慢后续 tick）。
- **为什么重要**：每秒写盘是无意义的磁盘磨损（且 `saveLocked` 在持锁下做文件 IO）；`reminders.json`/`memory.json` 损坏时启动静默丢掉全部提醒/记忆；`Stop` 后无法重启调度器；到点 TTS 慢会拖住 1s tick，使后续提醒延迟。
- **建议修复**：`tick()` 仅在有增删改时才 `saveLocked`；`Load()` 损坏时记 error 日志并把损坏文件改名备份（不要静默丢）；`Start()` 用新的 `stopCh` 或 `sync.Once` 语义；`onFire` 改为每条提醒起独立 goroutine（或有界工作队列）执行。

### P2-45 提醒输入细节：`每 0 分钟`立即误触发、取消需全文精确相等、无“改时间”入口
- **文件:行号**：`hub/internal/router/router.go:196-264`（`intervalRe` 的 `mins` 为 0 时 `Next=now` 立即触发一次随后被 `tick` 删除）；`hub/internal/reminders/reminders.go:138-151`（`CancelByText` 要求全文精确相等）。
- **为什么重要**：“每 0 分钟提醒我喝水”会立即播一次然后消失；“取消吃药的提醒”因与存储文本“吃药”不完全相等而取消失败；PLAN 要求的“可语音取消/改时间”里“改时间”根本没有入口。
- **建议修复**：见 P1-25 的范围校验；`CancelByText` 改为子串/归一化匹配；增加“把 X 提醒改到 Y 点”分支。

### P2-46 `ChatStream` 的 SSE 行缓冲上限 1MB：超长行会丢弃整段已生成的回答
- **文件:行号**：`hub/internal/agent/agent.go:102-158`（`sc.Buffer(make([]byte, 1024*1024), 1024*1024)`，`sc.Err()` 后 `return "", err`，已拼接的 `full` 被丢弃）。
- **为什么重要**：单个 SSE `data:` 行超过 1MB（如一次返回超长 JSON）时，整个流式调用报错且已生成内容全部丢失。当前 Router 未用流式（见 P1-29），所以今天是潜伏问题。
- **建议修复**：提高上限并按 `data:` 行流式解析时用 `bufio.Reader.ReadString` 分段读；或出错时返回已拼接的部分内容。

### P2-47 自动发现：1024 个 goroutine 一次性拉起、错误恒为 nil、网段推导武断
- **文件:行号**：`hub/internal/discovery/discovery.go:139-189`（`Scan` 为每个候选 IP 起一个 goroutine，上限 1024+ARP，信号量 32 只限制探针并发、不限制 goroutine 数；结尾 `return found, nil`，错误永为 nil）；`hub/cmd/hub/main.go:514-533`（`localCIDR` 取第一个非回环接口的 IP 并强制 `/24`，无视真实掩码；多网卡/NAS 的 docker 网桥可能被优先选中）。
- **为什么重要**：扫描本身不贵，但 goroutine 风暴 + 恒 nil 错误使调用方无法区分“扫完了没找到”与“扫的过程出错了”；网段推导错会使扫描扫到 docker 网段而非家庭局域网。
- **建议修复**：用固定大小 worker 池替代每 IP 一个 goroutine；`Scan` 返回真实错误（至少返回 ctx 超时与探针失败统计）；`localCIDR` 改为取默认路由对应的接口与真实掩码，失败时明确报错而非静默跳过。

### P2-48 语音管线细节：TTS 尾音丢失、不校验合成采样率、同设备可并发跑两次回答
- **文件:行号**：`hub/internal/voice/voice.go:158-171`（`for i := 0; i+opus.FrameSize <= len(samples); i += opus.FrameSize`，尾部不足 320 点的采样直接丢弃）；`:133-149`（`Synthesize` 返回的 PCM 被假定为 16kHz 单声道 int16，不校验长度/采样率）；`:83-112`（`Frame` 在释放 `mu` 后调 `answer()`，同一设备的 `Frame` 与 `End` 可并发进入两次 `answer()`，两次 TTS 回复会交错播放）。
- **为什么重要**：尾音丢失 <20ms 可忽略，但“零帧”场景（TTS 返回 <320 点）会使 `onReply` 收到空帧列表仍发 `start_play`（见 P1-16）；Piper 配置改采样率后语音会变调/变速而无任何告警；两句连续语音可能产出两段交错回复。
- **建议修复**：尾部补零到整帧；`Synthesize` 返回后校验字节数与期望采样率（或从 Wyoming `audio-start` 的 `rate` 字段取）；给每设备加“回答中”标志，第二次结算排队或丢弃。

### P2-49 回家场景绕过高风险 domain allowlist
- **文件:行号**：`hub/internal/router/router.go:266-285`（`sceneMemory` 的“我回来了”直接 `CallService("scene","turn_on","scene.coming_home")`，场景内容可含门锁等高风险设备，绕过 `:365-375` 的 domain allowlist）。
- **为什么重要**：场景是 allowlist 的旁路；HA 场景内容由用户在 HA 侧配置，可能含门锁/燃气，一句语音即可触发。
- **建议修复**：场景执行前校验场景内实体 domain，或在文档中明确场景内容责任。

### P2-50 点歌不拆分歌手/歌名
- **文件:行号**：`hub/internal/router/router.go:294-339`（`music()` 把“周杰伦的青花瓷”整个串当歌名传给 MA `track_by_name`，`artist` 参数恒为 `""`）。
- **为什么重要**：MA 点歌成功率低于直接搜网易云；同名歌曲多时易播错。
- **建议修复**：点歌时用分隔符（“的/ - ”）拆分歌手与歌名，拆不出时再整体搜。

### P2-51 注册表房间语义三处不一致：房间清不掉、hello 会覆盖认领、种子键与真实 ID 永远对不上
- **文件:行号**：`hub/internal/registry/registry.go:44-62`（`Upsert` 只覆盖非空字段，`Room` 为空时不覆盖——房间一旦写入便无法清空）；`hub/internal/transport/transport.go:213-220`（hello 的 `Room` 非空即 `Upsert`，每次重连都会用 `client.toml` 里的静态 room 覆盖语音认领的房间）；`hub/config.yaml:23-31`（种子键 `livingroom-speaker`/`bedroom-speaker`）与 `hub/internal/transport/transport.go:206-208`（真实 ID 为 `lx06-<ip>`）不一致，`hub/cmd/hub/main.go:139-156`（`OnRoomClaim` 按真实 ID 写 `cfg.Devices`，会与种子条目并存、越积越多）。
- **为什么重要**：用户语音认领“客厅”后，Client 重启（hello 带旧 room 或空 room）可能把认领结果覆盖回旧值；配置文件里新旧设备条目并存，`RoomNames()` 会返回重复房间名。
- **建议修复**：明确房间来源优先级（语音认领 > hello 上报 > 配置种子），hello 的 room 仅在注册表无房间时填充；`Upsert` 增加显式的“清空房间”语义（如 `room: "__clear__"` 或单独 `ClearRoom`）；启动时用真实 ID 归一化种子条目（按 model+IP 匹配去重）。

### P2-52 `build-armv7.sh` 会静默产出“无 ALSA、无 KWS”的应急版并以 exit 0 结束
- **文件:行号**：`client/build-armv7.sh:33-42`（`ALSA_ARM_PREFIX` 不存在时打印一句 WARNING 后 `--no-default-features` 继续编；且全程无 `--features sherpa-kws`，见 P1-40）。
- **为什么重要**：换机/重装后 musl libasound 目录丢失时，构建脚本不失败，而是产出一个不能采集、不能播放的 Client；exit 0 会让调用方误以为交付物正常。TEST_REPORT 的 ARM 产物校验只看了 ELF 类型与大小，未校验功能。
- **建议修复**：缺 musl libasound 时默认失败退出，仅在显式传 `--allow-no-alsa` 时才降级；构建后断言产物含 ALSA 与 sherpa 相关符号（或跑一次 `--help`/冒烟确认特性开启）。

### P2-53 Hub 主循环的收尾与定时器：`time.Tick` 泄漏、goroutine 里 `Fatalf`、信号处理不完整
- **文件:行号**：`hub/cmd/hub/main.go:198-210`（`sweepSessions` 用 `time.Tick`，ticker 永不 `Stop`）；`:412-427`（wss/ws 服务 goroutine 里 `log.Fatalf`，直接 `os.Exit`，跳过任何清理）；`:490-498`（收到信号后只 `haClient.Close()`，不调 `remStore.Stop()`、不关 Wyoming 连接、不停 Web 服务——进程随即退出，实际影响小，但 `reminders.json` 可能在写盘中途被杀）。
- **为什么重要**：`time.Tick` 在进程生命周期内无害，但属明确的泄漏写法；`Fatalf` 使一次 TLS 证书加载失败即整个进程退出而非降级；信号处理不完整在 kill 时可能丢一次提醒写盘。
- **建议修复**：`time.Tick` 改 `time.NewTicker` + `defer Stop()`；goroutine 内错误改记日志 + 退避重试；信号处理里先 `remStore.Stop()`、等写盘完成再退出。

### P2-54 Client 两个公开自由函数失败即 panic（目前仅测试调用，但签名是危险的）
- **文件:行号**：`client/src/audio.rs:104-126`（`encode_pcm_frame`/`decode_opus_frame` 用线程本地编解码器 + `.expect(...)`，失败即 panic；注释自承“调用方应在流式路径用 `OpusCodec`”）。
- **为什么重要**：公开 API 的失败语义是 panic 而非 `Result`，后续任何生产调用都会把一次可恢复的编解码错误升级为进程崩溃；且线程本地 `CODEC` 的 `RefCell` 在重入时也会 panic。
- **建议修复**：改为返回 `Result<Vec<u8>>`/`Result<Vec<i16>>`，或标记 `#[doc(hidden)]` 并只在测试模块内保留。

### P2-55 部署物缺失：`.env.example` 不存在、二进制会悄悄过期、sed 静默无操作
- **文件:行号**：`deploy/docker-compose.yml:1-3`（注释写 `cp .env.example .env`，但 `deploy/` 下无 `.env.example`）；`deploy/nas-install.sh:39-45`（只检查 `deploy/hub-linux-amd64` 存在，不校验它是否比 `hub/` 源码新）；`deploy/nas-install.sh:104-112`（三处 `sed -i` 替换 HA/MA 地址，替换零次也不报错，仅 `grep` 展示结果行）。
- **为什么重要**：文档化的第一步（复制 `.env.example`）无法执行；Hub 改代码后忘重新 `go build`，NAS 会静默跑旧二进制；NAS 上 `config.yaml` 已被改过格式时，sed 静默不替换，Hub 仍用容器名访问 MA/HA 而失败。
- **建议修复**：补 `deploy/.env.example`（占位值 + 注释）；`nas-install.sh` 校验二进制 mtime ≥ 最新 Go 源码 mtime，否则拒绝；sed 后用 `grep -q` 断言替换确实发生。

### P2-56 Web 面板：GET 配置暴露内部拓扑、未匹配路径都返回 200 首页
- **文件:行号**：`hub/internal/web/web.go:85-96`（GET `/api/config` 返回完整 `s.cfg`，含 HA 地址、Agent base_url、网易云地址）；`:33-41,49-58`（`/` 的 `index` 处理器会匹配所有未命中路径并返回 200 HTML）。
- **为什么重要**：超大 PUT/POST 可撑内存（见 P2-7 的请求体上限）；配置 GET 向局域网任意主机暴露内部服务拓扑；不存在的 API 路径返回 200 而非 404，易掩盖调用方拼错路径的错误。
- **建议修复**：PUT/POST 包一层 `http.MaxBytesReader(w, r.Body, 1<<20)`；GET 配置只返回非敏感摘要（或要求鉴权，见 P0-1）；`index` 只响应 `/`，其余返回 404。

---

## FR / NFR 对照（逐项核对，证据在上）

| 编号 | 结论 | 证据 |
|---|---|---|
| FR-01 本地唤醒词 | **未实现（生产）** | P1-40：ARM 二进制无 sherpa feature；另 KWS 仅合成语音验证，真人唤醒率未测（TEST_REPORT 已知） |
| FR-02 就近仲裁 | **部分实现** | 仲裁纯函数与窗口计时正确（`arbitration.go`）；但 P1-13 迟到/重复窗口、P1-14 单帧 RMS；+6dB 补偿按决策不加，不算缺口 |
| FR-03 协同播放 | **未实现（生产）** | P1-26：Snapcast 从未 Dial；分组/延迟校准代码仅测试调用 |
| FR-04 对讲 | **未实现（生产）** | P1-15：无触发路径；另 Opus 级回声接线未做（TEST_REPORT 已知） |
| FR-05 网易云音乐 | **未实现（生产）** | P1-26：QueueID/MPD 接线断；歌词/暂停/下一首无语音入口（仅库函数存在） |
| FR-06 HA 家电 | **部分实现** | 开/关 + 查状态（先查后答）已实现；但 P1-42：查状态失败照调、查询类命令未接 HA；P1-27：HA 无重连 |
| FR-07 连续对话 + 打断 | **部分实现** | 20s 会话与 interrupt 消息存在；但 P1-29/P1-16：回答同步阻塞 + playing 判定错，打断在播放中基本收不到 |
| FR-08 本地 LLM 对话 | **部分实现** | 非流式 Chat 通；流式（首字 ≤800ms、边生成边播）未接线，见 P1-29 |
| FR-09 场景/定时/记忆 | **部分实现** | “我回来了”场景与提醒式定时存在；记忆只写不读、动作定时未实现，见 P1-41 |
| FR-10 摄像头/主动播报 | **部分实现** | 判定逻辑与勿扰正确；但 P1-46 时区错 8h；摄像头 motion 类实体未覆盖；HA 断线后订阅丢失（P2-11） |
| FR-11 OTA | **未实现（端到端）** | P1-9：Client 忽略、Hub 状态机无调用方；回滚 boot1 无代码 |
| FR-12 多 Agent 路由 | **未实现** | P1-38：启动时选定一次，per-wake 路由不存在 |
| FR-13 唤醒词管理 | **未实现（端到端）** | 换模型文件 + 改配置的路径存在，但无模型分发机制（OTA 不传模型文件，见 P1-9），Client 侧换词需手动放文件 |
| FR-14 循环提醒 | **部分实现** | 调度/持久化/语音取消已实现并有测试；但 P1-46 时区、P1-16 播报可能无声、P2-44/P2-45 细节缺口 |
| FR-15 紧急呼叫 | **部分实现** | 触发 + 取消状态机已实现并有测试；但 P1-18 谎报成功、P1-19 误触发；真实通知链路 10s 送达未验证（TEST_REPORT 已知） |
| NFR-01 本地优先 | **基本满足** | 唤醒/VAD/仲裁/音乐/HA 均在局域网；仅 LLM 文本出网（DeepSeek 在 config 默认模型名中出现，但 base_url 指向本地 Hermes/OpenClaw，见 `hub/config.yaml:33-41`） |
| NFR-02 安全 | **不满足** | P0-1、P0-2、P0-3、P0-4；另 P2-2、P2-56 |
| NFR-03 音频 | **基本满足** | 语音 Opus 16kHz/16kbps、音乐 48kHz Snapcast（协议层正确；生产接线见 P1-26） |
| NFR-04 延迟 | **不满足** | P1-29：非流式导致首字延迟超标；对讲接通无触发路径 |
| NFR-05 稳定 | **不满足** | P1-11（60s 误杀）、P1-12（重连误删）、P1-27（无下游重连）、P1-28（内存增长）、P1-2（nil panic） |
| NFR-06 可维护 | **部分满足** | 单 config.yaml 结构正确；但 P1-45（校验缺）、P1-22（PUT 假热更新）、P2-55（部署物缺失） |
| NFR-07 兼容 | **基本满足** | 型号校验 `lx06/oh2p` 多处存在；固件版本锁死在补丁脚本参数中 |

---

## 审过且没问题的清单

- **run_shell 任意执行**：客户端 `Session::handle_rpc`（`client/src/main.rs:139-200`）为白名单：仅 `start_play/stop_play/start_recording/stop_recording/play_text/set_volume`，`run_shell` 及一切未知方法均回 `unknown rpc method`（测试 `rpc_unknown_method_reports_error`，`main.rs:510-532` 覆盖）。Hub 侧全仓 grep 只有一处 `SendRPC`（`main.go:333`，`start_play`），从不下发 `run_shell`。**原项目的裸奔 `run_shell` 已不存在。**
- **密钥进日志**：grep 全仓 `log/info/warn` 含 token/password/secret/key 的行：仅 `main.rs:207` 的“token 仍是 CHANGE_ME”警告（不打印值）、`transport.rs:520` 的 `device_id`（非密钥）。`OPEN_XIAOAI_TOKEN/HASS_TOKEN/LLM_API_KEY/MA_TOKEN` 均只从环境变量读（`config.go:201-214`、`main.go:222,247`），不打印、不写文件。**例外见 P0-2（`login_hash` 被打印）与 P2-8。**
- **密钥落盘**：`config.yaml` 不含任何密钥字段（注释明确“Token 走环境变量，不要写进文件”，`config.yaml:29`）；`.env.example` 的 `CHANGE_ME` 是占位，`.gitignore` 已排除 `.env`。**例外是 P0-2 的 root 密码默认值与 P2-4 的文件权限。**
- **固件补丁的 preflight 与半成品清理**：`patch_firmware.py:552-570` 先 `preflight()`（全部只读检查）再应用；`patch.sh:53-60,106-134` 有 `fail()` + `trap ERR` 删除半成品镜像、40MB 上限检查（:98-104）、解包后关键文件与补丁标记抽查。**不足见 P1-8（无哈希校验）与 P2-6（验证不完整）。**
- **KWS 模型下载校验**：`client/scripts/download-kws-model.sh:57-60`：`curl` 下载后立即 `sha256sum -c`（硬编码 SHA256），校验通过才 `tar --no-same-owner` 解包、只拷固定文件名；`tar` 路径与模型名均固定。**该链路可信，未发现问题。**
- **OTA 下载的已知版本二次校验**：`download-ota.py:31-34,94-99` 对 `lx06 1.94.13` 与 `oh2p 1.58.6` 有硬编码 MD5+大小二次比对。**不足见 P0-5（其它版本无此保护）。**
- **Hub TLS 证书文件权限**：`tlscert.go:46,91,94`：目录 `0700`、证书/私钥 `0600`，正确。
- **家电高风险域隔离**：`router.go:340-360`：即使别名表配了门锁/燃气/热水器，`domain` 白名单只放行 `light/switch/climate/cover`，其它一律拒绝并提示当面确认；先查 HA 状态再调服务，不编数字。**该隔离是有效的。**
- **语音路由顺序与 LLM 隔离**：`router.go:120-170`：紧急→提醒→场景/记忆→音乐→家电→Agent；LLM 只返回播报文本，无工具调用、不能直接调 HA/提醒/紧急接口。**提示注入经语音进入 LLM 的影响限于一句播报文本。**
- **MPD `add %s` 的 format 串安全**：`music.go:465` 的 URL 是 `Fprintf` 的参数而非格式串，`%` 本身不构成 format 注入。**不足是换行注入，见 P1-7。**
- **Opus 编解码边界**：`hub/internal/opus/opus.go:40-77`：编码强制 320 点、解码缓冲固定 320 点；客户端 `audio.rs:80-88,130-142`：空包拒绝、`unwrap_audio_frame` 对 `[0x01]` 空帧与错误首字节返回 `None`。**未发现越界。**
- **WebSocket 文本解析的 panic 面**：`transport.go:260-290` 与 `client/src/transport.rs:250-290`：JSON 解析失败均 `continue`/忽略，不 panic；字段缺失时用 `unwrap_or("")`/`as_str()` 兜底。**不足是无大小/速率限制，见 P1-3。**
- **仲裁纯函数**：`arbitration.go:33-44`：单台直接赢、空事件返回 `""`、平局 `>`（先到者赢）与测试一致；`Arbiter.close` 在锁外调 `onResult`，无锁内回调死锁；窗口计时 `time.AfterFunc` + 首条开窗，测试为真计时。
- **对讲包内逻辑**：`intercom.go`：`participantBusy` 双向检查（发起方/目标方任一忙即拒绝）；`Forward` 在锁外调 relay、不持锁做网络 IO；`SetFullDuplex` 广播返回 false；`SetHalfDuplex` 丢 AEC 状态。
- **提醒 ID**：`reminders.go:116-127`：纳秒时间戳 + 进程内自增序号，同一毫秒连加不撞（`TEST_REPORT` 已修项，代码中已落实）；`tick` 在锁外调 `onFire`；持久化 tmp 写 + rename 原子。
- **提醒调度**：`NextDaily` 今天已过则明天；一次性触发后删除；每日重排；`Start` ticker 1 秒；`Stop` 的 `close(stopCh)` 有 `stopped` 守卫。
- **注册表合并语义**：`registry.go: Upsert`：只覆盖非空字段、不碰 Online、`LastSeen` 恒刷新（`TEST_REPORT` #7 的修复已落实）；`Get`/`All`/`Online` 返回拷贝，无外部 mutation 泄漏。
- **配置**：`config.go`：TLS 用 `*bool`、缺省默认 true；`Validate` 校验唤醒词/listen/型号/music_folder；`Save` tmp + rename。
- **传输鉴权**：`transport.go`：hello 先验 token 再验型号，失败回 `hello_reject`；`Conn.SendJSON`/`SendBinary` 各自持 `Conn.mu`；`BroadcastJSON` 持读锁迭代。
- **TLS 证书**：`tlscert.go`：证书/私钥文件 0600、目录 0700；已存在则复用（指纹不变）；`Fingerprint` 取 PEM 首块 DER 的 SHA-256。
- **HA 客户端**：`ha.go`：`call` 10 秒超时后删 pending；`readLoop` 按 id 分发 result、拷贝 handlers 后调用；`Close` 的 `done` 关闭有守卫；`GetEntityState` 查不到返回明确错误。
- **Router 家电**：`router.go: homeAssistant`：domain 白名单（light/switch/climate/cover）才放行；先查状态、已是目标状态不重复调；别名不在表里返回 false 落到 Agent。
- **Router 音乐降级**：`router.go: music`，代码层面：MA 失败继续走 Netease + MPD，不直接报错；搜不到歌有明确回复。
- **语音管线**：`voice.go`：纯静音（无 speech）不回答；`End` 无 speech 时删除 utterance；`answer` 里 `router == nil` 与 `onReply == nil` 有守卫；`RMS` 空输入返回 0。
- **AEC 数学**：`aec.go`：NLMS 三段循环（估计/归一化/更新）与环形缓冲索引正确；`ProcessFrame` 对短 ref 补 0 的行为是明确的；`SetMu` 校验 `(0,2)`。
- **Client RPC 白名单**：`client/src/main.rs: handle_rpc`：未知方法（含 `run_shell`）回 `ok:false`，不执行。
- **Client TLS 指纹**：`client/src/transport.rs`：指纹格式非法在配置期报错；无指纹时走系统根证书 + 警告，不静默跳过；签名仍用 webpki 按公钥验证。
- **Client 重连退避**：`transport.rs: backoff_delay`：100ms 起步翻倍、上限 1s；`connect_with_retry` 无限重试（P1-43 只针对鉴权失败的区分）。
- **KWS 诚实性**：`client/src/kws.rs`：无模型/无后端时恒返回 None、不谎报唤醒；`feed` 按 320 切帧、残余保留；静音 RMS 返回 -120 下限而非 -inf。
- **Sherpa FFI**：`client/src/sherpa.rs`：`Api` 的 `_lib` 最后声明（按声明顺序 drop，符号先失效）；`Drop` 先毁 stream 再毁 spotter；`_paths` 与 spotter 同寿命；显式 `.so` 路径失败不回退默认名；`feed_frame` 每次 `destroy_result`。
- **ALSA**：`client/src/audio.rs`：XRUN 后 `prepare()` 重试；短读循环读满 320；采集/播放两路独立打开、失败只记不整体失败；`Audio::open()` 不因缺硬件失败。
- **补丁脚本**：`firmware-patch/scripts/patch_firmware.py`：每步 check → apply → verify；`libxaudio` 要求 `hw:0,3` 恰好出现 1 次才改；`md5_crypt` 与 openssl 校验值一致（代码注释 + 测试）；OH2P 跳过 libxaudio。`patch.sh`：40MB 上限检查（`>= 40MB` 即失败删镜像）；失败出口删半成品；`mksquashfs` 参数取自原始镜像的 `file` 输出。
- **OTA 脚本层禁用已核对完整**：`bin/ota` 全部子命令（check/ble/upgrade/test/slient）均经 `upgrade()`，`flash.sh` 主流程检查 `upgrade_param_check` 返回值后中止并删输入文件，wireless 的 `ota()` 被 `exit 3` 截断，cron 触发链亦然（证据：已解包原厂 `bin/ota:281-430`、`bin/flash.sh:158-164`、`/etc/crontabs/root`）。残留仅 P2-35 的 `matool` 二进制一项。
- **asound Capture 参数校验只查 Capture 块内**：`patch_firmware.py` `patch_asound` 的分块校验，避免 OH2P 播放链 `period 480` 误判。
- **部署二进制**：实测 `deploy/hub-linux-amd64` 的 `ldd` 只依赖 `libc`/`libm`，Opus 已静态编入——`deploy/Dockerfile.hub` 不装 libopus 是正确的（与 `hub/internal/opus/opus.go` 顶部注释的旧说法不同，以实测为准）。
- **音乐客户端**：`music.go`：Snapcast `call` 持锁做 seq 递增与读写；MA `command` 的 seq 只在锁内递增、HTTP 各自独立响应不串话；MPD `cmd` 按行读到 OK/ACK；Netease `SongURL` 对空 URL 报错。
- **发现扫描**：`discovery.go`：信号量 32 并发、ctx 取消时停止、跳过本机 IP；`hostsInCIDR` 对 /16 截断 1024 是注释中写明的行为。
- **Web PUT 先校验后写盘**：`hub/internal/web/web.go:85-116`：非法型号 PUT 返回 400 且文件不被改写，有测试。
- **OTA Push 去重与 sha 校验**：`hub/internal/ota/ota.go:68-89`：进行中任务拒绝重复 Push；sha256 必须 64 位 hex。
- **Dockerfile.hub**：config 不进镜像；`ca-certificates` 已装；二进制 glibc 需求最高 2.34 ≤ bookworm 2.36（已 `readelf` 核对，兼容）；apt lists 已清；`--no-install-recommends` 已加。
- **compose**：`MUSIC_FOLDER` 以 `:ro` 挂载；命名卷隔离；`restart: unless-stopped`；`depends_on` + Hub 对 Wyoming 失败记日志重试（`TEST_REPORT.md`）。
- **Hub config Save 是原子写**：tmp + rename（`hub/internal/config/config.go:189-198`）。
- **nas-install.sh 本机预检查存在**：YAML 语法、文件存在、token 非空，且步骤化输出 PASS/FAIL 汇总。
- **Client→Hub 的 wss + Token + 证书指纹**：`TEST_REPORT.md` 已覆盖，不重复。
- **mksquashfs `-no-xattrs` 与原厂一致**：原 superblock 无 xattrs；`-all-root` 与原厂单 id 表一致。
- **下游 HTTP 超时**：`hub/internal/agent/agent.go:62` 120s；`hub/internal/music/music.go:172,283` 15s；`hub/internal/ha/ha.go:146-175` 10s 调用超时；Snap/MPD 5s 读写 deadline：均有显式超时，未发现无超时 HTTP 调用（除 `download-ota.py` 的 `urlretrieve`，见 P2-23）。
- **提醒/记忆/配置的写盘原子性**：均先写 `.tmp` 再 rename，避免写一半损坏。
- **Opus 帧校验**：`client/src/audio.rs:76-130`、`hub/internal/opus/opus.go:40-77`：帧长必须 320 点、空包拒绝；往返 RMS 误差 <10% 有测试。

---

## 未审 / 审不了的

- **`firmware-patch/work/` 下的全部解包文件与固件镜像**：任务明确排除（官方固件内容）；`.pytest-tmp/`、`client/target/` 等构建产物未审。
- **真机验证类**（与 `TEST_REPORT.md` 的 BLOCKED 关口一致）：KWS 真人唤醒率（1m/3m ≥95%）、双机同时唤醒仲裁实测、ALSA `hw:0,0` 采集 / `hw:0,2` 播放与 XRUN 恢复、Snapcast 两台同步播放延迟差、刷机后开机/SSH/Hub 连接、boot1 回滚实测、OTA 写分区、ARM 版 `libsherpa-onnx-c-api.so` 交叉编译、AEC 真实回声抑制率、Sherpa `is_ready` 是否会长期非零（P2-21）、KWS 检出延迟几帧对 RMS 的影响（P1-14 的时延部分）。
- **NAS 实机部署**：`docker-compose.yml` 在 UGREEN DXP6800 Pro + UGOS 上的实际运行、端口占用、`/volume1/music` 挂载、MA 与 Snapcast 的组网、网易云扫码登录与 VIP 歌、mosquitto 默认配置是否匿名（P1-37）、Web/4399 在 NAS 上的实际监听地址、SSH 默认密码是否真实生效。
- **真实 HA 环境**：实体 ID、场景 `scene.coming_home` 内容、摄像头/motion 实体、电话网关/推送链路的 10s 送达。
- **性能与长期运行**：7×24h 内存曲线、两台同时对话的并发负载、Whisper 在 NAS i5 上的实际延迟——代码层面未发现除 P1-28 外的明显泄漏，但未经 soak 测试。
- **第三方镜像内部默认配置**：whisper/piper/mosquitto/mpd/snapserver/MA/netease-api 均未拉取核查，只沿用 `TEST_REPORT.md` 的镜像存在性结论。
- **供应链深审**：`go.mod`/`Cargo.toml` 的依赖版本、Docker 镜像的实际默认配置（如 mosquitto 镜像内置 `mosquitto.conf`）、小米 OTA 接口的服务端行为，均只以仓库内代码与注释为准，未做外部抓包/镜像解包验证。
- **密码学强度实测**：未对 `md5crypt $1$` 做实际破解耗时测量，“可离线高速破解”是基于算法公开参数（1000 轮 MD5、固定 8 字节盐）的定性判断；未对 Token 做熵测量（Token 由操作员生成，仓库内无真实 Token）。
- **`docs/`、README.md、NAS_CHECKLIST.md**：只作对照，未当代码审；`docs/PROTOCOL.md` 与代码的一致性除传输帧格式（0x01 前缀、hello/wake/audio_start/audio_end/status/rpc）已核对外，协议文档全文未逐字比对。
- **`firmware/` 下的 REA 反编译结论**（miso RSA+SHA1、DTS 共享内存 key `0x2234` 等）未复核，仅作为补丁参数的背景引用。
- **测试未重跑**：本轮审计未运行 `go test`/`cargo test`/`pytest`；测试结论引用 `TEST_REPORT.md`（88/37/36 通过）。
- **非本次方向**：性能/功耗、渗透测试、隐私合规未系统审计。

---

*审计分工：安全、正确性、固件补丁与部署、健壮性与质量 4 个方向并行审计，原始发现 154 项，去重合并后 108 项（P0 6 / P1 46 / P2 56）。只读审计，未修改任何源代码。*
