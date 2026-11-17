# kws-sidecar 第二轮审计报告（Python 边车面）

- 审计员：kws-sidecar 面审计员（只审计、不修代码）
- 日期：2026-11-17
- 分支：`audit-round2-ci-fix`（自 main `509e57a` 切出）；审计后未动代码、未推 GitHub、未动 main
- 审计对象：`kws-sidecar/`（Python + sherpa-onnx 2.0.1，服务端唤醒边车）

## 0. P0 级新问题（置顶）

**无 P0 新问题。** 本轮在边车面未发现可导致变砖、丢数据、远程执行或认证绕过到可直接利用级别的问题。

新问题统计：**P0 × 0，P1 × 4，P2 × 13**（详见 §3、§4）。最严重的 3 项见 §1 摘要。

## 1. 最严重的 3 项摘要

1. **P1-1 重复 `device_id`：旧连接的孤儿流继续检出并发送 wake（已实证）。** 同一 `device_id` 第二次连上来时，`server.py:175-177` 只把 `devices` 字典指向新流、记一条 warning，**不关闭旧连接**；旧 handler 仍拿着旧 `stream` 对象继续喂音频、继续发 wake。探针实证：旧连接喂完后半句关键词后收到 wake（rms 0.136），新连接什么也没收到。后果：Hub 侧一次重连窗口内可能出现**同一设备两条连接各发一次 wake**，与首轮 P1-13（仲裁无去重/无冷却）叠加即双窗口各赢一次；且 README“旧流被丢弃”的说法与代码行为不符。
2. **P1-2 空 `keywords.txt` 时边车带病上线：`/health` 照常 200 ok，但永远检不出唤醒。** `load_spotter`（`server.py:58-83`）只校验关键词文件**存在**，不校验内容；实测空文件（0 字节）时进程正常启动、`GET /health` 返回 `{"status":"ok","model_loaded":true}`。此时关键词表为零行，`get_result` 恒为空，FR-01 在 Hub 健康检查全绿的情况下静默死亡。错误 token 行会 fail-loud（实测 exit 255），唯独空表是 fail-open。
3. **P1-3 边车在局域网无鉴权、无连接数上限。** `server.py:267` 的 `serve()` 未配任何鉴权；compose 里边车 `network_mode: host` + `KWS_HOST=0.0.0.0`（`deploy/docker-compose.yml` kws-sidecar 段），即 `10400` 直接暴露在局域网。局域网内任何主机可匿名建连、开任意多个 `device_id` 流、持续喂音频打满单事件循环的 CPU（每流一次 sherpa 解码）。不能直接向 Hub 注入 wake（wake 只回发到攻击者自己的 socket），但可 DoS 整个唤醒面；与 Hub 4399 的 mTLS + 强 Token 形成反差。

## 2. 审计范围与方法

### 2.1 范围

- 边车全部代码：`kws-sidecar/server.py`（279 行）、`kws-sidecar/tests/test_kws_sidecar.py`、`tests/conftest.py`、`tests/bootstrap.py`、`entrypoint.sh`、`Dockerfile`、`requirements.txt`、`requirements-dev.txt`、`README.md`、`scripts/download-model.sh`。
- 对照：`TEST_REPORT.md`“服务端唤醒”一节（§298-465 行）、`AUDIT_REPORT.md` 首轮 KWS 相关项、`.github/workflows/ci.yml` 的 sidecar 步骤、`hub/internal/kws/kws.go`（仅为定位 `kws_status` 滞后责任方）、`deploy/docker-compose.yml` 的 kws-sidecar 服务段。
- 不在范围：Hub 侧仲裁/重连逻辑（只在与边车交互处引用）、客户端本地 KWS 代码（只在复查首轮延期项时引用）、NAS 部署实测（无 NAS）。

### 2.2 方法

只读文档不下结论；文档与代码冲突时以代码为准；凡能实测的都实测：

1. **基线测试**（仓库根目录）：`kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q`。
2. **真边车 + 真模型**：另起边车（**10401 测试端口**，未占 10400），`curl /health` 与未知路径核对；探针脚本（`/tmp/kws-audit-probe.py`，已随审计清理）做协议探针：非法 hello ×7、hello 后异常帧、重复 `device_id`、trailing 静音计数、500 帧洪泛后的唤醒；另做 20,000 帧（400s 音频）RSS 前后对照。
3. **关键词表故障实验**：同一模型目录、10402 端口，分别用空 `keywords.txt`、错误 token 行、正确 token 行起边车，观察退出码与 `/health`。
4. **代码通读**：逐行读 `server.py` 与 `bootstrap.py`，核对每一处断言。

### 2.3 基线测试结果

```
kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q
16 passed in 21.89s
```

与 TEST_REPORT 记录的基线（16 passed in 14.67s）一致：通过数持平，耗时差异在模型加载/机器负载波动范围内。16 项构成：正样本 5、负样本 6、双设备隔离 1、断开重连 1、非法采样率 1、/health 1、模型缺失非零退出 1。

真边车核对（10401 端口，真实模型 `sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01`，模型加载 2.54s）：

```
GET /health  → 200 {"status": "ok", "model_loaded": true}
GET /unknown → 404 not found
```

探针与实验结果见 §5。测试全部使用 Piper TTS **合成语音**；真人唤醒率、真机性能一律记 BLOCKED（§7），不写成通过。探针结束后边车进程已停、测试端口已释放、临时日志与关键词文件已清理；`git status` 干净，无仓库改动。

## 3. P1 新问题（4 项）

### P1-1 重复 `device_id` 时旧连接不关闭，孤儿流继续出 wake

- **位置**：`kws-sidecar/server.py:175-177`（重连分支）、`:217-218`（finally 守卫）、`README.md`“并发与状态”一节。
- **证据**：
  - 代码：重连时只执行 `self.devices[device_id] = stream`（新流覆盖字典项）并记 warning，**没有任何 `await old_ws.close()`**；旧 handler 的局部变量 `stream` 仍指向旧 `KeywordSpotter` 流，`async for message in ws` 循环继续喂音频、继续 `await ws.send(wake)`。finally 的 `is stream` 守卫只防止旧连接删掉新字典项，不阻止旧连接继续工作。
  - 实证（探针 C，合成 pos1 样本）：连接 1 喂前半句 → 连接 2 同 `device_id` 连上并收到 `ready` → 连接 1 继续喂后半句 + 50 帧静音 → **连接 1 收到 wake（rms 0.136），连接 2 收到 0 条消息**。服务端日志只有 `device dup-dev reconnected while old stream alive; dropping old stream`，但旧流实际未停。
- **影响**：Hub 断线重连窗口内（旧 TCP 在边车视角仍 ESTABLISHED、新连接已建立，例如网络分区恢复瞬间），同一设备两条连接同时向 Hub 发 wake；Hub 仲裁无去重/无冷却（首轮 P1-13 未修），可能开两个仲裁窗口、各赢一次，或同一声唤醒触发两次会话。另：孤儿流持续占用 CPU；日志措辞误导排障（写着“dropping old stream”，实际没丢）。
- **修复建议**：`Server.devices` 改存 `(stream, ws)`；新连接建立时先 `await 旧 ws.close(code=1000, reason="replaced by new connection")` 再替换；或反过来拒绝新连接直到旧连接关闭。二选一，并补测试：旧连接在新连接建立后喂完整关键词应**收不到** wake（且连接已关闭），新连接喂完整关键词恰好一次 wake。

### P1-2 空 `keywords.txt` 时 fail-open：`/health` 200 但永不唤醒

- **位置**：`kws-sidecar/server.py:58-83`（`load_spotter` 只判 `os.path.isfile`，不读内容）；`server.py:120,128`（`model_loaded` 只反映 spotter 非空）。
- **证据**：实测三组（同一模型目录，10402 端口）：
  - 空文件（0 字节）：进程**不退出**，`GET /health` → 200 `{"status":"ok","model_loaded":true}`。关键词表零行，`get_result` 恒为空——边车永不唤醒，但 Hub 健康检查全绿。
  - 错误 token 行（`n i h ao`，`ao` 不在 tokens.txt）：sherpa 抛错，进程 exit 255，fail-loud（好）。
  - 正确 token 行：正常启动。
- **影响**：部署/运维手误（`download-model.sh` 写关键词失败、卷挂载空文件、误清空）会让 FR-01 在一切检查都通过的情况下静默死亡；排障时 `/health` 反而证明“边车正常”，误导方向。关键词表是唤醒的唯一配置，空表等价于无模型，应同等对待。
- **修复建议**：`load_spotter` 加载后校验关键词文件至少含一行非空、含 `@` 的有效行（或直接读文件判空）；空表/全注释时 `sys.exit(1)` 并打印 FATAL；`/health` 增加 `keywords` 计数（例如 `"keywords": 1`），让健康检查能区分“模型在、无词表”。

### P1-3 WS 端口无鉴权、无连接数上限，局域网可匿名打满唤醒面

- **位置**：`kws-sidecar/server.py:267`（`serve(...)` 无鉴权参数）；`deploy/docker-compose.yml` kws-sidecar 段（`network_mode: host`、`KWS_HOST=0.0.0.0`）；`Dockerfile`（`EXPOSE 10400`）。
- **证据**：代码层面确认——`handler` 对 hello 只校验格式，不校验任何 Token/证书；`Server.devices` 无数量上限；`serve` 未设连接数限制。探针 A 的 7 次匿名连接全部只被协议格式拦下，无一被身份拦下。部署形态上 10400 不只监听回环，而是 host 网络下的 `0.0.0.0`。
- **影响**：局域网内任何主机（或一台被入侵的设备）可匿名建连、开任意多个 `device_id` 流、持续喂音频；解码在单事件循环内串行执行，流一多即打满 CPU，真音箱的唤醒延迟随之恶化直至不可用。不能直接向 Hub 注入 wake（wake 只回发到攻击者自己的 socket，Hub 只认自己建的那条连接），所以定 P1 而非 P0；但这与 Hub 4399 的 mTLS + 强 Token（首轮 P0-3 已修）形成明显反差——唤醒面是整条语音链里唯一零鉴权的入口。
- **修复建议**：按威胁从低到高任选：(a) 最低成本——监听改 `127.0.0.1`（边车与 Hub 同 NAS、同 host 网络，127.0.0.1 即可，无需 `0.0.0.0`；Dockerfile HEALTHCHECK 与 compose 健康检查本来就走 127.0.0.1）；(b) 共享密钥 hello（与 Hub 的 Token 复用同一机密，hello 增加 `token` 字段校验，失败即 1008 关闭）；(c) 连接数上限（例如 `max_connections` + 每 device_id 单连接，超限拒绝）。建议至少做 (a)，(b) 作为纵深。

### P1-4 `/health` 的 503 分支不可达，健康语义只剩“进程活着”

- **位置**：`kws-sidecar/server.py:118-143`（`process_request`）、`:120`（`self.model_loaded = spotter is not None`）、`:263`（加载失败 `sys.exit(1)` 在 `Server` 构造之前）。
- **证据**：`model_loaded` 在 `Server.__init__` 里被置为 `spotter is not None`；而 `amain` 里模型加载失败时直接 `sys.exit(1)`，根本到不了 `Server(...)`。因此运行中的进程 `model_loaded` 恒为 True，503 分支是死代码。README 与 server.py 模块 docstring 写的“模型未加载好时 503”在现有启动流程下**永远不会发生**。结合 P1-2：空关键词表时 /health 同样是 200。
- **影响**：Hub 与 NAS 部署脚本把 `/health` 当作“唤醒可用”的判据（`nas-install.sh` 第 5 步、`deploy/NAS_CHECKLIST.md`），实际它只能证明“进程在监听”，模型/词表/推理能力一概不证明；故障模式（空词表、推理持续异常）全部漏过健康检查。
- **修复建议**：二选一：(a) 让 503 真正可达——启动时先起 HTTP 再异步加载模型，加载中返回 503（改动大）；或 (b) 收缩语义——文档改写为“/health 只证明进程存活”，并在 /health 增加 `keywords` 数量与一次启动自检结果（如加载耗时、词表行数），同时由 Hub 侧定期做一次“喂合成静音、确认无异常”的轻量推理探针。至少要修文档，消除“503 会报告模型未好”的错误承诺。

## 4. P2 新问题（13 项）

### P2-1 `is_ready` 排空循环无上限（首轮 P2-21 的模式迁移到了边车）

- **位置**：`kws-sidecar/server.py:100-101`（`DeviceStream.feed` 内 `while self.spotter.is_ready(self.stream): decode_stream`）。
- **证据**：代码确认无迭代上限、无超时；`feed` 是同步调用，跑在单事件循环里。首轮 P2-21（客户端 `client/src/sherpa.rs` 同款循环）当时标“疑似，需真机/实测确认”；本轮在边车面同样**未观察到卡住实例**（探针 571 帧 + 20,000 帧均正常排空），但模式原样迁移且 blast radius 变大——边车循环卡住会堵住**全部设备**的 handler，而客户端只堵一台音箱。
- **影响**：若某路流的 `is_ready` 因模型/音频异常恒为真，整个边车事件循环永久阻塞，所有设备唤醒停摆，且无日志、无超时、无自愈。
- **修复建议**：循环加硬上限（如单帧最多 decode 32 次，超限记 error 并 `reset_stream` 该设备流）；或把 decode 移到线程池（`run_in_executor`）使单路异常不堵事件循环。补一个“构造持续 ready 的假 spotter”的单测验证上限生效。

### P2-2 wake 事件的 rms 是 1s 均值，会被 trailing 静音稀释（首轮 P1-14 的残留形态）

- **位置**：`kws-sidecar/server.py:91`（`rms_frames = deque(maxlen=50)`）、`:107-114`（`rms` 取 50 帧 RMS 的均方根）、`:94-105`（每帧都 append，含 trailing 静音帧）。
- **证据**：代码确认 rms 窗口是“最近 50 帧（含检出前喂入的 trailing 静音）”的均值，不是词内峰值。TEST_REPORT 场景 0 的实测区间下界就是 `0.000`（窗口喂满静音时 rms 归零），说明稀释真实存在；探针 D 中 pos1 在 10 帧 trailing 后检出、rms 0.136，而窗口里已有 200ms 静音。检出延迟越长（长句、弱信号），窗口里静音占比越高，rms 越小；两台设备检出延迟不同时，稀释程度不同，仲裁比较的不再是“谁喊得响”，而是“谁检出得早”。
- **影响**：仲裁公平性漂移；极端时两台都报接近 0 的 rms，胜负由浮点噪声决定。首轮 P1-14（单帧取值）已通过“1s 窗口”缓解，但未根治。
- **修复建议**：rms 改取窗口内峰值（或窗口内前 50% 分位），或在检出时回溯：取关键词时间窗（检出时刻前约 0.8s）内的帧算 rms；至少在 wake 事件里同时报 `rms_peak` 供仲裁选用。补测试：词后喂 50 帧静音再检出，断言 rms 仍接近词内水平。

### P2-3 hello 10s 超时走通用 except：全量 traceback + 误导性 "internal error"

- **位置**：`kws-sidecar/server.py:145`（`wait_for(ws.recv(), timeout=10)`）、`:~248-256`（通用 `except Exception`）。
- **证据**：探针 A7：建连后 10s 不发 hello，客户端收到 `{"type":"error","message":"internal error"}`，随后连接关闭；服务端日志打了一段完整 traceback（`asyncio/timeouts.py … TimeoutError`）。超时是**预期内**的协议行为，却被当作内部异常处理。
- **影响**：日志噪声（每次半开连接/端口扫描都刷一条 traceback，淹没真异常）；客户端收到的 "internal error" 误导排障（以为是服务端故障，实际是自己超时）。
- **修复建议**：单独捕获 `asyncio.TimeoutError`：记一条 INFO/WARNING（“hello timeout, closing”），发 `{"type":"error","message":"hello timeout"}` 后以 1008 关闭，不打 traceback。

### P2-4 连接断开时不 flush，尾部关键词随流丢弃

- **位置**：`kws-sidecar/server.py:217-218`（finally 只删字典项，无排空）。
- **证据**：代码确认——断开即 `del self.devices[device_id]`，不给流补 trailing 静音、不做最后一次 `decode`/`get_result`。探针 D 证实唤醒需要关键词后约 200ms（10 帧）trailing 音频才出结果；若设备在关键词尾音处恰好断线（网络抖动、进程重启），这句唤醒必然丢失。
- **影响**：低概率丢唤醒；正常使用下麦克风持续送静音，trailing 总会到来，所以影响有限，定 P2。
- **修复建议**：finally 里对断开的流补喂约 10-25 帧静音并做一次 `get_result`（若检出则记日志；wake 事件已无连接可发，至少留一条 `wake lost on disconnect` 日志供排障）。或在文档里明确写清“关键词尾 + 断线 = 丢唤醒”的边界。

### P2-5 `device_id` 无长度/内容校验，原样进日志

- **位置**：`kws-sidecar/server.py:160-170` 附近（hello 校验只判“非空字符串”）。
- **证据**：代码确认——`device_id` 只要是字符串、非空即通过，无长度上限、无字符集限制；随后 `LOG.info("device connected: %s", device_id)` 与 wake 日志原样输出。Hub 侧的 `device_id` 来自可信配置，当前风险低。
- **影响**：超长 `device_id` 可进字典与日志；含 `\n` 的 `device_id` 可伪造日志行（与首轮 P2-3 同款问题在 Hub 侧的 room 名上已提过，边车侧是同类残留）。
- **修复建议**：校验长度（如 ≤64）与字符集（`[A-Za-z0-9_-]`），非法即 error + 1002 关闭；日志输出前做单行化（去 `\r`/`\n`）。

### P2-6 `KWS_THRESHOLD` / `KWS_SCORE` 无范围校验

- **位置**：`kws-sidecar/server.py:build_arg_parser`（`--threshold` / `--score` 只 `float(...)`，无范围判断）。
- **证据**：代码确认——`KWS_THRESHOLD=5.0` 或 `-1` 都会原样传给 `KeywordSpotter`，sherpa 是否 clamp 未验证；阈值配错时边车要么永不唤醒、要么狂误触发，且启动日志只打印数值、不告警。
- **影响**：配置手误静默改变唤醒行为，无启动期拦截。
- **修复建议**：启动时校验 `0 < threshold <= 1`（按 sherpa 语义定界）、`score >= 0`，越界即 FATAL 退出；或至少在越出实验验证值（0.25/1.0）时打一条 WARNING。

### P2-7 可观测性缺口：`wake_count` 是死指标，无指标端点、无延迟统计

- **位置**：`kws-sidecar/server.py:92`（`self.wake_count += 1`）、`:107-114`（rms）、`/health` 只返回两个字段。
- **证据**：代码确认——`wake_count` 自增后**没有任何读取方**（不进日志、不进 /health、无指标导出）；/health 只有 `status`/`model_loaded`；无每设备帧数、无检出延迟、无丢帧/错误计数。排障只能靠 grep 日志里的 `wake:` 行。
- **影响**：NAS 上长期运行后无法回答“昨天误触发几次、平均检出延迟多少、哪台设备流异常”，与首轮对 Hub 可观测性的要求同类。
- **修复建议**：/health 扩展为 `{"status","model_loaded","keywords","devices","total_wakes","uptime_s"}`；或至少在 `wake` 日志行里加上“自该设备连接以来的第 N 次”与“关键词结束到检出的帧数”。改动小，收益明确。

### P2-8 bootstrap：Piper 语音文件只 pin 了版本 tag，未做 SHA 校验

- **位置**：`kws-sidecar/tests/bootstrap.py:62-67`（`VOICE_BASE` 指向 HF `rhasspy/piper-voices` `v1.0.0`）、`:_download`（`urllib.request.urlopen` 直写文件，无 hash 校验）。
- **证据**：代码确认——模型下载有 SHA256 pin（`scripts/download-model.sh` 内 `MODEL_SHA256=b2f7c896…`，bootstrap 报错信息也引用它），但两个语音文件（`.onnx`、`.onnx.json`）下载后**不校验任何摘要**；`_download` 失败时删残留文件是好的，但内容损坏/被替换时要到 piper 合成失败才暴露，报错指向 piper 而非下载。
- **影响**：供应链完整性不对称；HF tag 被重新指向或传输损坏时，测试样本悄悄变质（最坏情况：语音损坏导致样本校验反复重抽 10 次才报错，浪费 CI 时间）。
- **修复建议**：给两个语音文件 pin SHA256（实测现有 `.kws-exp/tts/` 里的文件算一次），下载后校验，不符即删文件重报；或至少记录已下载文件的 SHA 到日志。

### P2-9 bootstrap：样本校验阈值写死，与服务端运行阈值可漂移

- **位置**：`kws-sidecar/tests/bootstrap.py:104-105`（`VERIFY_THRESHOLD = 0.25`、`VERIFY_SCORE = 1.0`）、`_verify_samples` 用它建离线 spotter。
- **证据**：代码确认——校验用的阈值是写死的常量，只在注释里说“取边车默认值”；而服务端阈值由 `KWS_THRESHOLD`/`KWS_SCORE` 环境变量决定。若部署改了阈值，测试校验的仍是旧阈值：可能出现“样本校验通过、生产阈值下检不出”或反之。
- **影响**：测试与生产的判定标准悄悄分叉；目前默认值一致，所以是潜在项。
- **修复建议**：`_verify_samples` 从环境变量读阈值（默认 0.25/1.0），与 `server.py` 同源；或在 README 里写明“改阈值必须同步改 bootstrap 常量”。

### P2-10 bootstrap：默认资产根是共享实验目录，坏样本重抽会改写它

- **位置**：`kws-sidecar/tests/bootstrap.py:assets_root`（默认 `~/workspace/.kws-exp`）、`_verify_samples` + `_generate_missing`（不合格样本就地重抽覆盖）。
- **证据**：代码确认——`KWS_TEST_ASSETS` 未设时，`resolve_assets()` 直接操作 `~/workspace/.kws-exp/samples/`；`_verify_samples` 对校验不过的样本调用 `_synthesize_sample(..., path)` 原地覆盖。本地跑测试本应是只读验证，却可能改写实验目录的参考样本（TEST_REPORT 称 pos3 曾命中一次坏抽样并重抽——那次重抽就发生在共享目录里）。
- **影响**：测试副作用污染实验基线；多人/多任务共用 `.kws-exp` 时样本会被悄悄替换，复现性受损。
- **修复建议**：默认根目录不存在“写”需求时，先校验、仅当确实缺失才生成；重抽前把原文件备份（如 `pos3.wav.bak-<timestamp>`）或改写进 `KWS_TEST_ASSETS` 指向的隔离目录；文档注明“本地默认路径下测试可能改写 `.kws-exp/samples`”。

### P2-11 bootstrap：10 次上限实际是 10 次检测 / 9 次重抽，文档说“重抽最多 10 次”差一

- **位置**：`kws-sidecar/tests/bootstrap.py:93`（`MAX_GENERATION_ATTEMPTS = 10`）、`:345`（`if attempts >= MAX_GENERATION_ATTEMPTS: raise`）、`:356-357`（日志 `re-rolling (attempt {n+1}/{MAX})`）。
- **证据**：代码走读——`attempts` 从 1 开始计检测次数：首次检测失败（attempts=1）→ 重抽 → 第 2 次检测……第 10 次检测失败即 raise。实际重抽次数上限是 **9 次**，不是文档（bootstrap.py 模块 docstring 与 README）写的“重抽最多 10 次”。另：日志里的 `attempt {attempts+1}` 在第 1 次失败时打印 `attempt 2/10`，计数口径与“第几次重抽”不一致，易误读。
- **影响**：纯计数口径问题，不影响“10 次不合格就报错”的兜底语义；但文档与代码差一，排障时数重抽次数会对不上。
- **修复建议**：统一口径：把循环改成“最多 10 次重抽”（即 11 次检测）或把文档改为“最多 10 次检测（9 次重抽）”；日志改为 `re-roll {k}/10` 的重抽计数。

### P2-12 容器以 root 运行、无资源上限

- **位置**：`kws-sidecar/Dockerfile`（无 `USER` 指令，默认 root）；`deploy/docker-compose.yml` kws-sidecar 服务段（无 `mem_limit`/`cpus`，全文件 grep 也无任何 `mem_limit`）。
- **证据**：文件核对确认。边车是纯推理服务，不需要 root；单事件循环 + 无界设备流（见 P1-3）时，一个失控进程可吃掉 NAS 全部 CPU/内存。
- **影响**：与首轮 P1-4 的容器加固要求同类（当时针对 hub/Wyoming 等服务）；边车面是其中一块未补的拼图。风险低于 Hub（边车不写配置、不调 HA），定 P2。
- **修复建议**：Dockerfile 加 `USER nobody`（或建专用 `kws` 用户；注意 `/data/kws/models` 卷权限）；compose 加 `mem_limit: 512m`、`cpus: "1.0"`（按 NAS i5 实测再调）。

### P2-13 `download-model.sh` 的 curl 无 `--fail`，HTTP 错误靠 SHA 兜底、报错误导

- **位置**：`kws-sidecar/scripts/download-model.sh`（`curl -sSL --retry 2 -o …`，无 `--fail`）。
- **证据**：脚本走读——无 `--fail` 时 HTTP 404/403 也会写出一个（错误页面）文件并 exit 0，随后 `sha256sum -c` 失败才退出。最终结果仍是 fail-loud（SHA 兜住了），但用户看到的报错是 “SHA256 校验失败”，会误以为是上游重新打包，而真实原因是下载根本没拿到包。
- **影响**：排障误导；CI 里下载失败时多花一次排查。
- **修复建议**：curl 加 `--fail`（`curl -sSLf`），并在 SHA 校验失败的报错里同时打印 HTTP 状态与文件大小。

## 5. 必查项逐项结论

### 5.1 多设备隔离

- **结论：基本成立，有一处已实证的例外（P1-1）。**
- 每 `device_id` 独立 `KeywordSpotter` 流（`server.py:DeviceStream`，`create_stream()`），`feed` 内 `rms_frames`、`wake_count` 均为实例状态；双设备隔离测试通过（`test_two_devices_isolated`，基线内）；探针中两设备各走各的流、互不干扰。
- **例外**：重复 `device_id` 时旧连接不关闭（P1-1），旧流继续出 wake——隔离的不是“设备”，是“连接”，而字典只认一个。
- **断开清理**：正常断开时 finally 按 `is stream` 守卫删除字典项（`server.py:217-218`），重连后只喂后半句不出 wake（`test_reconnect_drops_old_state` 通过，基线内）。守卫本身写对了（防止旧连接删掉新连接的表项，与首轮 P1-12 的 Hub 侧 bug 形成正确对照）。
- **非法 hello**：探针 A 全过——二进制首帧、非法 JSON、非 hello 类型、缺 `device_id`、空 `device_id`、`sample_rate` 为字符串 `"16000"`、为 8000，全部返回 `error` 后以 1002 关闭。`sample_rate` 用 `!= 16000` 严格比较，字符串会被拒（好）。
- **异常帧**：探针 B——hello 后的文本帧 → error 但**连接保持**（`continue`，`server.py:186`）；奇数长度 PCM（641 字节）→ error、连接保持（`:191`）；空二进制帧（0 字节）→ 静默无操作、连接保持。三种处理均合理：协议错误不杀连接，避免一次脏帧断掉整条音频流。

### 5.2 断线 / 重连与背压

- **客户端断开后状态残留**：无残留（正常断开路径）。唯一残留形态是 P1-1 的“旧连接未断”而非“断开后残留”。
- **慢消费方**：wake/error 的 `ws.send` 无超时；对端不读时，该连接的 handler 任务会被 `send` 挂起——只 stall 本设备，不堵其它设备（每连接独立 task；解码是同步 CPU，在事件循环里，见 5.8）。wake 消息极少（一次唤醒一条），实际风险低。
- **队列满了怎么办**：**边车侧不丢帧**。`serve()` 未显式传 `max_queue`，实测 websockets 17.4 默认 `max_queue=16`（`inspect.signature(serve)` 核对）：入站队列满时协议层暂停读 TCP，背压传回 Hub，由 Hub 侧的 50 帧队列 + 丢最旧帧策略承接（TEST_REPORT 已记录 Hub 侧丢帧，边车不背这个锅）。出站写缓冲默认 `write_limit=32768`，超限时暂停写。结论：背压机制存在且来自库默认值，但**代码里没有任何显式声明**——换库版本或改参数时容易悄悄失效，建议在 `serve()` 调用处显式写出 `max_queue` 与注释。
- **重连**：边车侧无重连逻辑（被动接受连接），重连退避在 Hub 侧（`kws.go:72,104`：500ms→30s），与 TEST_REPORT 一致。

### 5.3 模型行为

- **加载失败是否非零退出**：是。`load_spotter` 缺文件即抛 `FileNotFoundError`，`amain` 捕获后记 FATAL、`sys.exit(1)`（`server.py:263`）。测试 `test_model_missing_exits_nonzero` 通过；另实测错误 token 行时 exit 255（sherpa 抛错，未被 `load_spotter` 的 `missing` 检查拦下，但同样 fail-loud）。
- **关键词表格式与缺失处理**：缺失（文件不存在）→ FATAL 退出（好）；格式错误（未知 token）→ sherpa 抛错退出（好）；**空文件 → 静默带病运行**（P1-2，坏）。格式本身是 `token 序列 @显示词`（拼音建模），与 `scripts/download-model.sh` 的 fallback 行一致。
- **Trailing 音频与唤醒延迟（实测）**：探针 D（合成 pos1“你好小李”）：关键词喂完后，**第 10 帧（200ms）trailing 静音时出 wake**，rms 0.136；另一次全程计时 0.52s（含 63 帧语音 + 10 帧静音的发送与处理）。结论：
  - TEST_REPORT/测试里用的“1s trailing 静音”是**安全余量**，不是实测下限；pos1 实测下限约 200ms。
  - 对真实唤醒延迟的实际影响：关键词尾音 → wake 事件约 **200ms**（本样本；长句/弱信号可能更长，未测）→ 再加 Hub 250ms 仲裁窗口 → `wake_win` 约 450ms 起。离“唤醒到首字 ≤1.5s”仍有余量，但 200ms 是 transducer 固有延迟，压缩空间在模型/阈值，不在代码。
  - 若关键词后音频流立即中断（断线），无 trailing 可喂，唤醒丢失（P2-4）。

### 5.4 误触发面：跨句拼接“你好”+1s 停顿+“小李”

- **TEST_REPORT 已证实的现象**：neg1“你好”+ 1s 静音 + neg2“小李”串在一路流里被拼成一次“你好小李”（rms 0.132；反向 neg2+neg1 静默，neg1+neg1 静默）。
- **成因（代码 + 模型行为）**：边车 `DeviceStream.feed`（`server.py:94-105`）把音频**无间断地**喂给同一个 transducer 流，流内没有任何“静音超 X 秒即 reset”的逻辑；`reset_stream` 只在检出 wake 后执行。zipformer-transducer 是流式模型，靠左 64 帧上下文 + 内部状态做 token 拼接；1s 静音在模型看来只是“词间停顿”，不构成句子边界——只要“你好”的声学证据还在 transducer 的 lattice/状态里，后来的“小李”就能接上同一条关键词路径。**静音不切分句子**是 transducer KWS 的固有行为，不是边车喂法写错了；边车代码里没有任何机制能区分“一次说完的你好小李”和“停顿 1s 后补的小李”。
- **缓解办法（按成本排序）**：
  1. **静音超时 reset（推荐）**：边车维护每流的连续静音帧计数（如 RMS < 阈值连续 50 帧 = 1s），超限即 `spotter.reset_stream(stream)`，把“你好”的残留状态清掉。代价：说话慢、词间停顿 >1s 的真唤醒会被切断——阈值要实测（建议从 1.5-2s 起）；且 reset 会丢掉“你好小李，今天……”这类长句的后半部分吗？不会，reset 只清已检出前的状态，长句的关键词在句首，检出发生在关键词后，reset 点在静音段，不影响已检出的词。需真人测试定阈值。
  2. **提高阈值/分数**：`keywords_threshold` 0.25 → 更高会压低拼接路径的分数，但也会压低真唤醒率（pos3 已有 1/7 抽样检不出，阈值上调会更糟），不推荐单独用。
  3. **Hub 侧冷却/去重**：对“rms 异常低或检出延迟异常长”的 wake 加怀疑权重——治标，rms 稀释问题（P2-2）会让这条信号本身不可靠。
  4. **接受并标注**：若真人测试证明“停顿后补小李”在真实使用中极少发生，可把本条降为已知行为、写进 README/TEST_REPORT，不修代码。但“是否算误唤醒”仍需真人验证（BLOCKED，§7）。
- **本轮未复现该现象**：未在探针里重跑 neg1+静音+neg2 串联（TEST_REPORT 已有对照实验证据，不重复消耗）；成因分析基于代码走读 + transducer 行为 + TEST_REPORT 的对照实验（反向静默说明不是单纯“两词都出现就触发”，而是**顺序 + 状态残留**）。

### 5.5 pos3 抽样敏感与 bootstrap verify-and-reroll

- **机制是否真正兜住**：**代码层面是，实测层面只部分验证。**
  - `_verify_samples`（`bootstrap.py:304-360`）：生成后用离线 spotter 逐个校验，喂法与服务端一致（20ms 块 + 50 帧 trailing 静音）；正样本必须检出、负样本必须静默；不合格的 Piper 样本重抽，噪声样本（确定性）直接报错。
  - **10 次不合格后行为**：`attempts >= MAX_GENERATION_ATTEMPTS (10)` 即抛 `RuntimeError`，写明样本名、期望结果与“不要改样本文本绕过去”（`bootstrap.py:345-352`）；噪声样本首次失败即报错。conftest 在导入时调 `resolve_assets()`，失败即整个测试套件在收集期报错，不静默跳过。兜底语义正确。
  - **本轮基线未触发重抽**：16 passed 一次过，资产已存在且校验全过；TEST_REPORT 记录的“干净状态验证命中一次坏抽样、重抽后通过”是上一轮的实测，本轮**未独立复现 1/7 的失效率**（复现需多次生成同一文本，超出本次审计范围；如需确认应单独跑 20 次 pos3 生成统计）。
  - **两个口径问题**：P2-11（10 次检测/9 次重抽与文档差一）；P2-9（校验阈值写死，与服务端运行阈值可漂移）；另 `_verify_samples` 每次跑测试都全量重验 11 个样本（含已存在的），耗时但无害。

### 5.6 bootstrap.py

- **下载源与 pin**：模型——`scripts/download-model.sh`，GitHub k2-fsa/sherpa-onnx releases，tag `kws-models`，**SHA256 已 pin**（`b2f7c89690dc8ce4c6ed6afeab7cd800c36ad1421fb6b6302b4a4b194cf7f35f`），下载后 `sha256sum -c` 才解包（好；curl 缺 `--fail` 见 P2-13）。语音——HF `rhasspy/piper-voices`，tag `v1.0.0`，**只 pin 了 tag，未 pin SHA**（P2-8）。
- **失败时是否明确报错**：是。模型下载失败 → `RuntimeError`（含 exit 码与脚本输出尾部）；样本工具缺失（piper/ffmpeg）→ `RuntimeError`（含安装提示）；样本 10 次不过 → `RuntimeError`（含“不要改样本文本”）；噪声失败 → `RuntimeError`（注明“模型/阈值问题，不是坏抽样”）。全程无 `pytest.skip`、无空断言（TEST_REPORT 称此前两处 skip 已删，本轮核对测试文件确认无 skip）。
- **缓存目录行为**：资产只写 `KWS_TEST_ASSETS` 指向的目录或默认 `~/workspace/.kws-exp`；已存在且完整时只做存在性检查、不重新下载（基线跑无下载）。`_download` 失败删残留文件（好）。
- **会不会往仓库里写文件**：**不会**。`resolve_assets` 的一切写操作都在资产根目录下（模型/样本/关键词/tts）；`download-model.sh` 的目标目录是传入的模型目录。核对：审计后 `git status` 干净。唯一例外是 P2-10：默认根是共享实验目录时，重抽会改写 `~/workspace/.kws-exp/samples/` 里的参考样本（仓库外，但仍是共享基线）。

### 5.7 健康检查与可观测性

- **`/health` 语义**：200 + `{"status":"ok","model_loaded":true}` 当且仅当“进程在跑且模型加载成功过一次”；404 对未知路径。503 分支不可达（P1-4）；空词表仍 200（P1-2）。Dockerfile 与 compose 的健康检查都调 `/health`（30s 间隔、5s 超时、3 次重试、60s 启动期），与边车行为一致。
- **`kws_status` 滞后 30s 的成因**：**Hub 侧，不是边车侧。** `hub/internal/kws/kws.go:75-76,106-107`：`HealthInterval` 默认 30s；`:218-224`：`Start` 后 `time.NewTicker(HealthInterval)` 每 30s GET 一次 `/health` 更新状态。边车 `/health` 本身是即时响应（探针 curl 毫秒级返回）。TEST_REPORT 的表述（“唤醒恢复本身即时，状态显示滞后”）与代码一致；改健康检查间隔是 Hub 侧的可选优化，边车无需改。
- **日志是否含敏感信息**：否。日志只有：模型加载耗时/阈值/词表路径、设备连接/断开（`device_id`）、wake 事件（`device_id`/`keyword`/`rms`）、协议错误文本。无音频内容、无 Token、无密码、无模型文件内容。唯一注意点是 P2-5 的 `device_id` 原样进日志（日志注入面，非敏感信息泄露）。

### 5.8 并发与资源

- **线程模型**：单 asyncio 事件循环；每连接一个 handler task；解码是同步 CPU 调用（`feed` 内 numpy + sherpa），直接跑在事件循环里，无线程池、无进程池。sherpa `num_threads=1`（默认）。无共享可变状态竞争（`devices` 字典只在事件循环内读写；`feed` 不跨 task 共享流）。
- **长期运行内存增长面**：未做长 soak。短测：20,000 帧（400s 音频）洪泛后 RSS 由 92,192 KB → 96,776 KB（+4.6MB），断开流后**未回落**；不能定性为泄漏（可能是 Python/numpy 的 arena 未归还，也可能是 sherpa 流内缓冲），但“断开后 RSS 不回落”值得一次 24h soak + 定期 RSS 采样确认（见 P2-14 的观测记录——本节把它记为未确认项而非定级问题，见 §4 之外的“未确认”清单）。
- **单边车带多设备的上限（实测估算）**：探针 E：571 帧（含 500 帧静音 + 63 帧语音 + 8 帧 trailing）在 0.75s 墙钟内处理完并出 wake，即单流 RTF 约 0.066（1s 音频约 66ms CPU，VM 单核）；按每设备 50 帧/s、每帧约 1.3ms CPU 估算，**单核约 10-15 台设备打满**（未计 wake 事件、网络与 Hub 开销；VM CPU ≠ NAS i5-1235U，NAS 上限需实测）。当前两台设备下余量充足；“大规模并发未测，不要超量挂设备”的 README 警示仍然成立。建议：NAS 上跑 2 台 + 1 台模拟设备的 24h soak，采样 CPU/RSS/检出延迟后再定上限。

## 6. 首轮审计 KWS 相关延期项复查

| 首轮项 | 首轮内容 | 本轮复查结论 |
|---|---|---|
| P1-40 | ARM 二进制根本不含 KWS，刷进音箱的 Client 零唤醒能力 | **作废（架构替代），非原地修复。** 服务端 KWS 已落地（TEST_REPORT“服务端唤醒”一节）：唤醒移出音箱，ARM `.so` 的 glibc 问题不再是生产关口。本地 KWS 代码按决策保留为未来 A 路可选项；客户端默认 `kws_mode="server"`。本轮边车审计不重新打开 ARM `.so` 问题。 |
| P1-14 | KWS 上报 RMS 是命中帧单帧值，近处设备可能报静音值输掉仲裁 | **部分缓解，残留 P2-2。** 边车改取最近 1s（50 帧）RMS 均值，单帧静音值问题已消除；但均值会被 trailing 静音稀释（TEST_REPORT 场景 0 下界 0.000），仲裁公平性仍有漂移。建议改峰值/分位（P2-2）。 |
| P2-20 | KWS 推理失败每帧打一条 warn，日志刷屏 | **本地路径未变，生产路径不适用。** 客户端 `client/src/kws.rs` 的每帧 warn 仍在（本地 KWS 代码未动）；服务端路径下边车 `feed` 抛错走通用 except、每连接记一次，不存在每帧 warn。生产链路（server 模式）不受此项影响；若将来 A 路回切，需重审。 |
| P2-21 | Sherpa `is_ready` 循环无上限，模型卡住则录音任务永久阻塞 | **未修，且随架构迁移到边车，blast radius 变大。** 边车 `server.py:100-101` 是同款无上限循环；卡住时堵住全部设备（见 P2-1）。首轮标“疑似”，本轮同样未观察到实例，但建议补上限 + 单测。 |
| P2-10 | Sherpa `.so` 路径来自配置任意指定，ARM 结构体尺寸未锚定 | **生产路径不适用。** 边车用 pip 安装的 `sherpa-onnx==2.0.1` wheel，不 `dlopen` 配置指定路径；客户端本地路径的此项未动，仍延期（A 路回切时重审）。 |
| KwsLogMonitor | 尾随日志的旁路唤醒方案 | **已作废。** TEST_REPORT 与 `client/kws/README.md` 已写明作废原因（服务端 KWS 后不再需要）；本轮核对边车代码无任何日志尾随逻辑。确认作废，无残留。 |
| P1-12 | Hub 旧连接 deferred 清理误删新连接 | Hub 侧项，**边车侧的镜像问题恰恰相反**：边车 finally 的 `is stream` 守卫写对了（不误删新连接），但旧连接本身不关闭、孤儿流继续出 wake（P1-1）。建议两边一起修：Hub 修“误删”，边车修“不关”。 |
| P1-13 | 仲裁只看到达时间、无冷却期，两台先后唤醒各赢一次 | Hub 侧项，未修；**边车 P1-1 会放大它**（同一设备两条连接各发一次 wake，天然制造“先后到达的两条 wake”）。修 P1-1 可减少触发面，但 P1-13 本身仍需 Hub 侧加去重/冷却。 |

另：TEST_REPORT“服务端唤醒”一节的各项宣称与代码核对——“独立流/断开丢弃”（除 P1-1 外成立）、“加载失败非零退出”（成立，另实测 exit 255）、“rms 最近 1s 窗口”（成立）、“模型不 bake、entrypoint 下载”（成立，SHA 已 pin）、“Hub 断线退避 500ms→30s”（Hub 侧代码确认）、“kws_status 滞后 30s”（Hub 侧确认，见 5.7）、“尽力推流打满 50 帧队列丢关键词”（Hub 侧队列，边车不丢帧，见 5.2）。**文档与代码无其他实质冲突。**

## 7. BLOCKED（真机/真人才能验证，一律不写成通过）

- **真人唤醒率（1m/3m ≥95%）**：全部证据是 Piper TTS 合成语音；真人唤醒率未测，需刷机后真机验证。
- **真机性能**：边车在 NAS（i5-1235U）上的推理延迟/RTF、多设备并发、长期运行内存均未测；VM 上的 RTF 估算（§5.8）不能当作 NAS 性能证据；24h soak 未做。
- **真机两台仲裁实测**：VM 里是假音箱 + 增益差；真机上两台同时喊、谁赢谁输，需刷机后实测。
- **跨句拼接是否算误唤醒**：TEST_REPORT 的串联实验是合成语音对照；真人场景下“停顿 1s 再喊小李”的误唤醒率需真人验证；静音超时 reset 的阈值（§5.4 办法 1）也需真人测试定。
- **pos3 的 1/7 失效率**：TEST_REPORT 的实测数据，本轮未独立复现（机制已验证，失效率未重测）。
- **长句/弱信号下的 trailing 需求**：pos1 实测 200ms；pos3 等长句、远场弱信号需要多少 trailing 帧，未测。

## 8. 未确认项（找不到证据，不猜）

- 20,000 帧后 RSS +4.6MB 未回落的原因：未确认是泄漏还是分配器 arena，需 soak + 采样确认。
- `is_ready` 是否真会长期非零：未确认，无实例；只确认代码无上限。
- `KWS_THRESHOLD` 越界时 sherpa 是否 clamp：未确认，未实测。
- `curl` 缺 `ca-certificates` 时镜像内下载是否失败：未确认（未构建镜像验证；`python:3.12-slim` 通常自带，但未实测）。
- 多设备上限在 NAS 上的准确值：未确认，只有 VM 估算（10-15 台/核）。

## 附：证据索引

- 基线：`kws-sidecar/.venv/bin/python -m pytest kws-sidecar/tests/ -q` → `16 passed in 21.89s`（2026-11-17 午间，audit-round2-ci-fix 分支）。
- 真边车：10401 端口，`/health` 200、`{"status":"ok","model_loaded":true}`，模型加载 2.54s；探针 A（非法 hello ×7 全拒 1002）、B（异常帧后连接保持）、C（重复 device_id 孤儿流出 wake，rms 0.136）、D（pos1 在 10 帧 trailing 后出 wake）、E（571 帧 / 0.75s）。
- 关键词实验：空 `keywords.txt` → 进程存活、`/health` 200；错误 token 行 → exit 255；正确 token 行 → 正常。
- RSS：20,000 帧洪泛前后 92,192 KB → 96,776 KB（+4.6MB，断开后未回落）。
- 探针脚本与临时文件均在 `/tmp`，审计结束已清理；边车进程已停；仓库无改动（`git status` 干净）。
