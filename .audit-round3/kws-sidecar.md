# kws-sidecar 面第三轮审计报告

**审计对象**：`~/workspace/speaker-open-xiaoai-rebuild/open-xiaoai-next/`，分支 `audit-round3`（已确认在该分支，基线 main `3a9d1d9`）。只读代码，未改任何文件，未跑测试。

**Headline**：边车主逻辑扎实——鉴权、握手、流隔离、模型 FATAL 均落实，SC-P2-1~13 中 11 项确认真修；但有 **3 个 P1** 部署前必须修：① SC-P2-2 的 `rms_peak` 发出来却被 Hub 整个丢弃，仲裁仍在用被静音稀释的均值，P2 修复目标落空；② SC-P2-12 只修了一半——镜像里没有卷属主处理，不走 `nas-install.sh` 直接 `compose up` 会在全新卷上 crash-loop；③ N-P2-10 的 TLS pin 只有 Hub 侧，边车根本没有 TLS 监听，pin 是死配置。**P0：0 项。P1：3 项。P2：10 项。**

## P1（重要缺陷，部署前修）

### S-1 — `rms_peak` 被 Hub 整个丢弃，SC-P2-2 的仲裁目标落空

**一句话结论**：边车算了也发了 `rms_peak`，Hub 的解析结构里根本没有这个字段，仲裁继续用会被 trailing 静音稀释的 1s 均值 `rms`。

**证据**：
- 边车发送：`kws-sidecar/server.py:441`，wake 事件含 `"rms_peak": stream.rms_peak`；`server.py:230-236` 为峰值属性；回归测试 `kws-sidecar/tests/test_sidecar_p2.py:119-141` 断言事件含 `rms_peak` 且 `rms_peak >= 0.25`（pos1 实测峰值 0.311、均值约 0.137，标定合理：峰值帧在检出窗口内，均值确被稀释）。
- Hub 丢弃：`hub/internal/kws/kws.go:798-812`，`handleMessage` 的 wake 解析结构只有 `device_id/keyword/rms/timestamp_ms` 四个字段；`WakeEvent`（`kws.go:58-64`）同样无 `RMSPeak`；全仓库 grep `rms_peak`/`RMSPeak` 在 Go 侧 **0 命中**。
- 仲裁仍用均值：`hub/cmd/hub/main.go:262-272`，`OnKWSWake` 用 `ev.RMS` 做 `20*log10(rms)` 送仲裁；两台设备检出延迟不同时，稀释程度不同，比较结果会被系统性扭曲。

**修复建议**：Hub 侧三处同步加 `RMSPeak`：`handleMessage` 解析结构、`WakeEvent`、`OnKWSWake` 仲裁换算优先用 `rms_peak`（缺失时回退 `rms`，并记一条 warn）；补一个 Hub 单测断言含 `rms_peak` 的 wake 事件进仲裁用的是峰值。

### S-2 — SC-P2-12 只修一半：全新卷 + 直接 `compose up` 会 crash-loop

**一句话结论**：compose 的 `user: "1000:1000"` 和 `nas-install.sh` 的 chown one-shot 都在，但镜像本身对卷属主毫无处理，README 的直接部署路径在全新卷上必失败。

**证据**：
- 落实的部分：`deploy/docker-compose.yml` kws-sidecar 服务有 `user: "1000:1000"`；`deploy/nas-install.sh:222` 有 `docker compose run --rm --user root --no-deps --entrypoint /bin/chown kws-sidecar -R 1000:1000 /data/kws/models`。
- 缺失的部分：`kws-sidecar/Dockerfile` 全文 grep `mkdir|chown|USER` **0 命中**——没有建 `/data/kws/models`、没有改属主、没有 `USER`。而 compose 注释却写“新卷的首次 copy-up 由 Dockerfile 里的 mkdir + chown 负责”，与事实不符。
- 失败链：全新 `kws-data` 卷以 root 属主挂载 → uid 1000 的 entrypoint 跑 `scripts/download-model.sh` 写不进 `/data/kws/models` → `set -euo pipefail` 下 `cp` 失败退出 → `restart: unless-stopped` 循环重启，唤醒全断（失败是响亮的，不会静默，但 README 写的 `cd ../deploy && docker compose up -d kws-sidecar` 路径即中招）。

**修复建议**：二选一并写进 compose 注释纠正错误说法：(a) Dockerfile 加 `RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models`（具名卷首次 copy-up 会带走属主）；或 (b) entrypoint 在下载前对不可写的卷目录做一次可读的预检报错。并补一个“不用 nas-install.sh、只 compose up 全新卷能起来”的部署冒烟项。

### S-3 — N-P2-10 半边落实：Hub 的指纹 pin 做了，边车根本没有 TLS

**一句话结论**：Hub 侧 pin 实现正确，但边车只监听明文 `ws`、无任何 TLS/证书逻辑，`wss + 指纹` 在真实部署里不可用，token 与 PCM 目前靠回环隔离 + token 裸奔。

**证据**：
- Hub 侧落实：`hub/internal/kws/kws.go:tlsConfig()`（`InsecureSkipVerify` + `VerifyPeerCertificate` 校验有效期与 SHA-256 指纹）、拨号与健康检查共用同一配置；`kws.New` 校验指纹为 64 位 hex；`hub/internal/config/config.go:276-285` 启动期再校验一次；`hub/cmd/hub/main.go:909` 把 `cfg.KWS.Fingerprint` 传入。
- 边车侧缺失：`kws-sidecar/server.py` 全文无 `ssl`/`SSL`，`serve(..., max_size=1<<20)`（`server.py:571`）无 `ssl` 参数；`deploy/` 与 `nas-install.sh` 里没有任何为边车签发证书的逻辑；`hub/config.yaml:15` 实际配的是 `ws://127.0.0.1:10400/v1/kws`（明文）。
- 现实风险面：compose 把 `KWS_HOST=127.0.0.1`（`docker-compose.yml` kws-sidecar 环境变量），Hub 也连 `127.0.0.1`，流量不出回环，局域网抓包风险不存在；但任何 NAS 本机进程都可直连 `127.0.0.1:10400`，token 以明文 JSON 在 hello 里传输（`server.py:382-383` 只校验不加密），且 `wss://` 一配就连不上（fail-closed，算优点）。

**修复建议**：部署前二选一并写进决策：(a) 接受“回环 + token”并把“边车不启用 TLS，依赖 127.0.0.1 绑定”写为正式安全假设，同时把 `KWS_HOST` 默认值改为 `127.0.0.1`（见 S-8）；或 (b) 给边车加 TLS（自签证书 + 启动期生成/挂载），让 Hub 的 pin 真正跑通并补端到端测试。不要让 `fingerprint` 配置项长期处于“配了也永远用不上”的状态。

## P2（次要 / 改进）

### S-4 — 无单帧大小校验：协议定 640B，实际放行到 1MB 切片喂入

**一句话结论**：二进制帧只拦奇数长度（`server.py` 音频循环），1MB 以内的超长帧会被切成 1562 个 20ms 块连续喂入，`is_ready` 恒真时最坏 1562×32 次 decode 会堵住单事件循环、全设备停摆。

**证据**：`server.py:571` `max_size=1 << 20`；音频循环 `for i in range(0, len(pcm), FRAME_SAMPLES)` 无长度上限；`DeviceStream.feed`（`server.py:187-193`）每块最多 32 次 decode。

**修复建议**：二进制帧长度非 640 字节即记 error + `continue`（或按 640 整数倍放行并记 warn），把 `max_size` 收紧到如 64KB。

### S-5 — 连续错误计数是全局的，且 `_fatal_error` 粘住永不恢复

**一句话结论**：一台设备的解码异常会计入全服务计数，5 次即 `/health` 503 且必须重启恢复，另一台正常设备也被连坐。

**证据**：`server.py:267-274` `note_feed_error()` 全局 `self._consecutive_stream_errors`，达 5 置 `_fatal_error`；`health_reason()`（`server.py:260-261`）一旦置起即 503，无任何清零路径；`note_feed_success()` 只清计数不清 `_fatal_error`。

**修复建议**：计数按 `device_id` 分桶；或 `_fatal_error` 在连续 N 次成功后自动清除（带日志），避免一次瞬时故障永久拉下健康检查。

### S-6 — 单帧喂入异常直接掐断整条连接，而不是像 SC-P2-1 那样复位流

**一句话结论**：`feed()` 抛任何异常都会 `note_feed_error()` 后 `raise`，整条设备连接被撕掉、Hub 重连并排空队列；与 SC-P2-1“超限复位自愈”的哲学不一致。

**证据**：`server.py:423-427` 音频循环内 `except Exception: self.note_feed_error(); raise`。

**修复建议**：捕获后记 error、`reset_stream` 该设备流并 `continue`，仅连续多次失败才断连接（与 S-5 的分桶计数联动）。

### S-7 — Dockerfile 基础镜像未钉 digest，违反项目自己的 P1-4 标准

**一句话结论**：`python:3.12-slim` 既无 digest 也无 patch 版本，compose 里其余镜像全部钉了 digest，只有边车是例外；且 `apt-get install curl bzip2` 未钉版本、镜像内无 `USER`。

**证据**：`kws-sidecar/Dockerfile:5` `FROM python:3.12-slim`；`deploy/docker-compose.yml` 注释称 P1-4 已钉 digest（whisper/piper/netease-api/snapserver/mpd 均有 `@sha256:`，边车是 `build:` 无 digest）；`requirements.txt` 全部 `==` 钉住（sherpa-onnx==2.0.1、websockets==17.4、numpy==2.5.5 等），反衬出基础镜像是唯一漂移点。

**修复建议**：按 P1-4 同样做法钉 `python:3.12-slim@sha256:<...>`，并记录更新流程；apt 包能钉则钉；镜像内加 `USER 1000:1000`（与 compose 一致，独立 `docker run` 也不跑 root）。

### S-8 — `KWS_HOST` 默认 `0.0.0.0`，脱离 compose 独立跑即暴露到局域网

**一句话结论**：安全默认值应是回环，当前默认是全接口；compose 靠环境变量覆盖才安全。

**证据**：`kws-sidecar/Dockerfile` ENV `KWS_HOST=0.0.0.0`；`server.py:497` `--host` 默认同样 `0.0.0.0`；只有 `deploy/docker-compose.yml` 显式设 `KWS_HOST=127.0.0.1` 兜底。

**修复建议**：两处默认值都改为 `127.0.0.1`，需局域网暴露时再显式改。

### S-9 — SC-P2-8 有缺口：部分缺失时，已存在语音文件不验 SHA

**一句话结论**：已存在文件验 SHA 只在“两个语音文件都已存在”时走，缺一个文件时现存那个直接跳过校验用到底。

**证据**：`kws-sidecar/tests/bootstrap.py:_ensure_voice()`——`if all(...)` 为真才验 SHA、坏了删掉重下；否则直接进下载循环且只下载 `not dest.is_file()` 的文件。`_download()` 本身下载后验 SHA 是对的。

**修复建议**：下载循环前先对已存在的文件逐个验 SHA（复用 `_voice_sha_ok`），坏的删掉重下，与全存在路径一致。

### S-10 — flush 检出的唤醒不计入 `total_wakes`，断开设备的计数随字典消失

**一句话结论**：断开时补喂检出的唤醒只记日志，`/health` 的 `total_wakes` 会漏计；`device_wakes` 只含当前连接设备，断开即清零。

**证据**：`server.py:475-483` finally 块调 `flush_on_disconnect()`，检出只记 `wake lost on disconnect`；`flush_on_disconnect`（`server.py:204-223`）内 `feed()` 的 `wake_count += 1` 发生在流上，但 `self.total_wakes += 1` 只在正常 wake 发送处（约 `server.py:445`）；`process_request` 的 `device_wakes`（`server.py:289-291`）直接取 `self.devices` 字典。

**修复建议**：flush 检出时 `total_wakes` 也 +1（或单独记 `lost_wakes`）；`device_wakes` 改为保留最近断开设备的累计值（或注明“仅当前连接”）。

### S-11 — 被取代的旧连接关闭超时后，旧 handler 可能滞留并占着连接数名额

**一句话结论**：`replaced` 标志能防幽灵唤醒，但旧连接 `close()` 5 秒超时后旧 handler 仍挂在 `async for` 里，`self.connections` 计数不释放，长期可挤占 16 个名额。

**证据**：`server.py` 新连接取代旧连接处 `await asyncio.wait_for(old_ws.close(...), timeout=5)`，超时只记日志继续；旧 handler 的 `finally` 才 `self.connections -= 1`，而旧 handler 要等连接真正出错/关闭才退出。

**修复建议**：取代时直接 `await old_ws.close()` 后再无条件清理，或给旧 handler 加显式取消/超时关闭；连接数统计里区分半开连接与已认证连接。

### S-12 — `KWS_SCORE` 无上界，`num_threads`/端口等参数无校验

**一句话结论**：阈值只拦了下界，`score` 无上界（过大可能抬高误触发倾向）；非法环境变量在解析默认值时直接抛 traceback 而非干净 FATAL。

**证据**：`server.py:amain` 校验 `0 < threshold <= 1`、`score >= 0`，无上界；`build_arg_parser`（`server.py:497-515`）里 `float(os.environ.get(...))` 在非法值时抛 `ValueError` traceback；`--num-threads`/`--port` 无范围校验（极端值依赖 sherpa/`serve()` 自身报错）。

**修复建议**：`score` 加合理上界（如 ≤ 10，超出 FATAL 并注明）；环境变量解析失败时捕获并打印 FATAL 后 `sys.exit(1)`；`num_threads` 限 1~8、`port` 限 1~65535。

### S-13 — `wake_no` / `frames_since_wake` 只有日志有，线上的 wake 事件没有

**一句话结论**：这两个字段未进 wire 事件，Hub 无法按事件观测检出延迟；任务简报里把它们当事件字段与实现不符，需澄清而非默认已实现。

**证据**：边车 wake 事件（`server.py:436-445`）只有 `type/device_id/keyword/rms/rms_peak/timestamp_ms`；`wake_no`/`frames_since_wake` 只在 `LOG.info`（`server.py:450-454`）；协议文档（`server.py:22-30`、`README.md`）同样只定义了 7 字段事件。

**修复建议**：若 Hub 需要这两个值做排障/监控，把 `wake_no` 与 `frames_since_wake` 加进事件（Hub 侧容错解析即可）；若不需要，在协议文档里明确写“仅日志，不上事件”，避免后人误以为 Hub 能拿到。

### S-14 — 指纹归一化两处不一致：`config.go` 不认 `-`，`kws.go` 认

**一句话结论**：带 `-` 分隔的指纹会被 config 启动校验拒绝，却是 `kws.New` 能接受的格式；另 config 报错文案说“小写 hex”但实际先 `ToLower`，大写也能过。

**证据**：`hub/internal/config/config.go:277` 只 strip `:` 和空格；`hub/internal/kws/kws.go:normalizeFingerprintHex` 还 strip `-`。

**修复建议**：两处共用同一个归一化函数；报错文案改为“64 位 hex（大小写不限）”。

## SC-P2-1 ~ SC-P2-13 逐项复核（13 项）

| 项 | 结论 | 证据 |
|---|---|---|
| SC-P2-1 单帧 decode 上限 32 + 超限复位 | ✅ 真修 | `server.py:69,187-193`；测试 `test_p2_1` |
| SC-P2-2 wake 事件 `rms_peak` | ⚠️ 边车真修、Hub 侧丢弃（见 S-1） | `server.py:230-236,441`；测试 `test_p2_2`（pos1 峰值 0.311/均值 0.137 标定合理） |
| SC-P2-3 hello 超时单独捕获、error + 1008 | ✅ 真修 | `server.py:328-337`（`wait_for(..., timeout=10)`，`except asyncio.TimeoutError` 单独分支，warning 无 traceback）；测试 `test_p2_3` |
| SC-P2-4 断开补 25 帧静音最后检出 | ✅ 真修 | `server.py:72,204-223` + finally `server.py:475-483`（被取代的流不 flush，条件正确）；测试 `test_p2_4`（fake + 真模型） |
| SC-P2-5 device_id 1–64 字符含 `.` | ✅ 自洽、无注入风险 | `server.py:76` 正则 + `server.py:363` 校验；仅作字典键/日志/回显，无路径或 shell 拼接；`_safe_device_id`（`server.py` 约 145 行）净化校验前输入；含点是测试钉住的既有协议（`test_p2_5_dotted_device_id_still_accepted`），与审计报告建议的 `[A-Za-z0-9_-]` 出入属有意的协议兼容 |
| SC-P2-6 阈值越界 FATAL | ✅ 真修 | `server.py:amain` 阈值 `(0,1]`、分数 `>=0` 越界 `sys.exit(1)`；测试 `test_p2_6`（上界缺口见 S-12） |
| SC-P2-7 /health `device_wakes` + wake 日志 `wake_no`/`frames_since_wake` | ✅ 真修 | `server.py:289-291`、`server.py:450-454`；测试 `test_p2_7`（计数口径缺口见 S-10） |
| SC-P2-8 语音文件 SHA256 pin（下载/已存在都验） | ⚠️ 基本真修、一处缺口（见 S-9） | `bootstrap.py:VOICE_SHA256`、`_download` 下载后验 SHA、`_ensure_voice` 已存在验 SHA；测试 `test_p2_8` |
| SC-P2-9 样本校验阈值读环境变量、与服务端同源 | ✅ 真修 | `bootstrap.py:_verify_thresholds` 读 `KWS_THRESHOLD`/`KWS_SCORE`，范围与 `server.py` 一致；测试 `test_p2_9` |
| SC-P2-10 重抽前备份 `.bak-<时间戳>` | ✅ 真修 | `bootstrap.py:_backup_existing` + `_verify_samples` 调用点；测试 `test_p2_10`（注：时间戳秒级精度，同一秒内两次重抽会覆盖备份，实测重抽间隔远大于 1 秒，风险可忽略） |
| SC-P2-11 按重抽次数计、最多 10 次重抽/11 次检测 | ✅ 真修 | `bootstrap.py:MAX_REROLLS=10`，`rerolls` 计数器；测试 `test_p2_11` 两例 |
| SC-P2-12 compose `user: "1000:1000"` + 卷属主 one-shot | ⚠️ 半修（见 S-2） | compose `user` 与 `nas-install.sh:222` chown 落实；但 Dockerfile 无 mkdir/chown/USER，compose 注释说法失实 |
| SC-P2-13 curl `--fail` + SHA 报错带大小 | ✅ 真修 | `download-model.sh:curl -sSLf --retry 2`，SHA 失败打印文件大小并说明下载已成功；测试 `test_p2_13` 两例 |

## 其余审计面（无新发现，简述结论）

- **鉴权与握手**：hello 前喂不了音频（二进制首帧即 `error + 1002` 关闭）；token 缺失/错误 `error + 1008`（`server.py:382-383`），`hmac.compare_digest` 常数时间比较，token 不进日志；未配/短 token 启动即 FATAL（`server.py:553` 起）；鉴权在取代旧连接之前，未鉴权连接杀不掉合法旧连接；连接上限 16，超限 `error + 1013`（半开连接也计数）。
- **Opus/损坏帧**：边车收的是 Hub 解码后的 PCM，Opus 解码失败在 Hub 侧；边车侧奇数长度帧记 error 后 `continue` 不致命，空帧/短尾块可安全喂入，垃圾字节只会被当作 int16 采样，不会卡死或崩溃（超大帧缺口见 S-4）。
- **并发与资源**：每 `device_id` 独立 `DeviceStream`（`server.py` `DeviceStream` 类），单事件循环串行解码，无共享可变状态串话；断开即 `del self.devices[device_id]` 丢弃流状态（`server.py:483`）；模型缺失/加载失败进程非零退出（`load_spotter` 抛异常 → `amain` FATAL `sys.exit(1)`），不静默降级；Hub 侧 `kws_status` 联动正确——`hub/internal/kws/kws.go:CheckHealth/Status` 10 秒轮询 + 连接变化即时触发，`main.go:982` 映射为 `ok/down/disabled`，`web.go:360-374` 报 `kws_down`，边车挂掉时 Hub 降级为日志 + 丢帧计数 + 退避重连（`kws.go:run/nextBackoff` 含 jitter）。
- **依赖与构建**：`requirements.txt` 全部 `==` 钉版本；`requirements-dev.txt` 仅 `pytest==9.2.0`；`entrypoint.sh` 引用均加引号、`set -euo pipefail`、模型缺失下载失败即退出，未发现注入/权限问题（`download-model.sh` 先验 SHA 再解包，`tar --no-same-owner`）。
- **Hub 契约容错**：Hub 用 `encoding/json` 解 wake 事件，未知字段忽略、缺失 `rms` 时为 0 → `-100dB`（该设备仲裁必输，不崩溃）；`device_id` 一律以流自身 ID 为准，不轻信边车回传（`kws.go:813` 起，N-P2-4）；`keyword` 不在配置表时忽略；`timestamp_ms` 为 0 时回退 Hub 收到时间（`main.go:wakeTimestamp`）。

## 计数

- **P0：0 项**
- **P1：3 项**（S-1 `rms_peak` 被 Hub 丢弃；S-2 SC-P2-12 半修，全新卷直接 up 失败；S-3 边车无 TLS，N-P2-10 pin 不可用）
- **P2：10 项**（S-4 ~ S-14；其中 S-9、S-12、S-14 为 SC-P2-8/6/10 的边角缺口，其余为帧大小、健康计数、异常处理、镜像钉版、默认绑定、计数口径、旧连接滞留、事件字段澄清）
- **SC-P2-1~13 复核**：11 项确认真修，2 项部分落实（SC-P2-2 边车修了但 Hub 未接；SC-P2-12 部署路径缺一半；SC-P2-8 另有 S-9 小缺口）。

**给协调员的建议**：P1 三项都应在真机部署前修，其中 S-1 是纯 Hub 侧改动（`kws.go` + `main.go` + 单测），S-2 改 Dockerfile 一行即可验证，S-3 需先定决策（接受回环假设还是给边车加 TLS）。
