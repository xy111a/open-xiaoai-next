# kws-sidecar — 服务端唤醒检测边车

音箱只采音，Hub 解码 Opus 后把 16kHz 单声道 PCM 发到这里做唤醒检测。
推理复用实验 `~/workspace/.kws-exp/run_kws_test.py`（sherpa-onnx 2.0.1，
sherpa-onnx-kws-zipformer-wenetspeech-3.3M，关键词“你好小李”），
包括“is_ready → decode → get_result → reset”的流式排空逻辑。

⚠️ 测试说明：本目录测试全部使用 Piper TTS 合成语音样本
（`~/workspace/.kws-exp/samples`），**合成语音，非真人唤醒率**；
真人唤醒率与真机性能不在本服务测试范围内，不宣称。

## 协议（已定）

- WebSocket：`ws://127.0.0.1:10400/v1/kws`（部署时 Hub 与边车都走 host 网络，
  见 `deploy/docker-compose.yml`；容器名 `kws-sidecar` 在 host 网络下解析不到，
  不要改回容器名）
- Hub → 边车，首条消息（JSON 文本）：
  `{"type":"hello","device_id":"lx06-192-168-1-20","sample_rate":16000,"token":"..."}`
  `sample_rate` 必须为 16000，否则返回 `error` 并关闭连接。
  `device_id` 限 1–64 字符 `[A-Za-z0-9_.-]`，非法返回 `error` 后以
  1002 关闭（超长/含换行符的 id 原样进日志可伪造日志行）。
  `token` 必须与边车启动时传入的 Token 一致（与 Hub 的
  `OPEN_XIAOAI_TOKEN` 同一机密，≥16 位，常数时间比较）；缺失/错误
  返回 `error` 后以 1008 关闭。边车未配 Token 时拒绝启动。
  WS 连接数上限默认 16，超限的新连接返回 `error` 后以 1013 关闭。
- Hub → 边车，二进制帧：原始 PCM int16 LE 单声道 16kHz，
  20ms 一帧 = 640 字节；非 640 字节的帧返回 `error` 后丢弃
  （S-4，不再切分喂入；超长帧此前切 1562 块会堵事件循环）。
- 边车 → Hub（JSON 文本）：
  - `{"type":"ready"}`：hello 确认；
  - `{"type":"wake","device_id":"...","keyword":"你好小李","rms":0.42,"rms_peak":0.55,"wake_no":1,"frames_since_wake":12,"timestamp_ms":...}`：
    检出唤醒词；`rms` 为检出时最近 1s 音频的 RMS（0~1），`rms_peak`
    为同一窗口内单帧 RMS 的峰值——`rms` 会被检出前的 trailing 静音
    稀释（检出越晚稀释越多），仲裁比较响度时应优先用 `rms_peak`；
    `wake_no` 为该设备流自连接以来的第 N 次检出，`frames_since_wake`
    为上次检出后喂入的帧数（每帧 20ms，检出延迟的近似度量，S-13）；
    `timestamp_ms` 为服务端 Unix 毫秒时间戳；检出后该设备流自动
    复位（re-arm），可继续检下一次唤醒；
  - `{"type":"error","message":"..."}`：协议错误或模型故障。
    10s 内未发 hello 返回 `{"type":"error","message":"hello timeout"}`
    后以 1008 关闭（预期内的协议行为，不打 traceback）。
- 健康检查：`GET /health` → 200
  `{"status":"ok","model_loaded":true,"keywords":1,"devices":0,
    "connections":0,"authenticated_connections":0,
    "uptime_s":...,"total_wakes":0,"lost_wakes":0,"device_wakes":{}}`；
  模型未加载、关键词表为空或推理连续失败（同一设备 ≥5 次）时 503
  `{"status":"error",...,"reason":"..."}`。`device_wakes` 为每设备
  累计唤醒次数，断开后仍保留（S-10；排障：某设备远高于同屋另一台
  即疑似误触发）。`lost_wakes` 为断开时 flush 检出、事件无处可发的
  唤醒数（同样计入 `total_wakes`）。`connections` 含未完成 hello 的
  半开连接，`authenticated_connections` 只计已通过 hello + token 的
  连接（S-11）。推理连续失败置起的不健康标志，在连续 5 次成功喂入
  后自动清除（S-5，不再粘住到重启）。模型加载失败时进程直接非零
  退出（不静默跑）。

## 并发与状态

- 每 `device_id` 一个独立的 KeywordSpotter 流（`create_stream()`），
  互不干扰；解码在单事件循环内串行执行（实验测得 RTF 很低，
  少量设备并发无压力；大规模并发未测，不要超量挂设备）。
- 连接断开即丢弃该设备流状态；同一 `device_id` 重连从零开始，
  旧的半句唤醒词不会残留（见 `tests/test_kws_sidecar.py::
  test_reconnect_drops_old_state`）。断开时先补喂 25 帧（500ms）
  静音做最后一次检出（模型需要关键词后约 200ms trailing 音频才
  吐结果；关键词尾音处恰好断线时不 flush 就会丢这句唤醒）——连接
  已断，检出不发事件，只记一条 `wake lost on disconnect` 日志，
  但计入 `total_wakes` 并单记 `lost_wakes`（S-10）。
- 单帧喂入异常（解码失败）不再直接撕掉整条连接：记 error、复位该
  设备流并继续喂下一帧，仅当该设备连续失败达 5 次才断连接（S-6，
  与 S-5 的按设备分桶计数联动）；另一台设备的成功喂入不影响该设备
  的分桶计数（S-5）。
- 同一 `device_id` 在旧连接未断时又连上来（且新连接已通过鉴权），
  旧连接被主动关闭（1000 "replaced by new connection"），旧流状态
  丢弃、旧连接此后不再产生 wake（见 `test_duplicate_device_id_
  closes_old_connection`）；未通过鉴权的连接杀不掉合法旧连接。

## 配置

环境变量（或同名命令行参数 `--host/--port/--model-dir/...`）：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `KWS_HOST` | `127.0.0.1` | 监听地址（默认只绑回环，见下“安全假设”；需局域网暴露时再显式改，不要悄悄改回 `0.0.0.0`） |
| `KWS_PORT` | `10400` | 监听端口（已确认与现有服务无冲突）；合法范围 1~65535（S-12） |
| `KWS_MODEL_DIR` | `/data/kws/models` | 模型目录 |
| `KWS_KEYWORDS_FILE` | `<模型目录>/keywords.txt` | 关键词表（默认含“你好小李”） |
| `KWS_THRESHOLD` | `0.25` | keywords_threshold（实验验证值）；合法范围 (0, 1]，越界时边车拒绝启动。改阈值须同步改 `tests/bootstrap.py` 的样本校验（它读同一环境变量，见下） |
| `KWS_SCORE` | `1.0` | keywords_score（实验验证值）；合法范围 [0, 10]，越界时边车拒绝启动（S-12） |
| `KWS_NUM_THREADS` | `1` | sherpa 解码线程数；合法范围 1~8（S-12） |
| `OPEN_XIAOAI_TOKEN`（或 `--token`） | （必填，无默认） | WS hello 鉴权 Token，与 Hub 的 `OPEN_XIAOAI_TOKEN` 同一机密（≥16 位）；未配或太短时边车拒绝启动。Token 只经环境变量/命令行传入，绝不写进仓库、日志或代码 |
| `KWS_MAX_CONNECTIONS`（或 `--max-connections`） | `16` | WS 连接数上限；超限的新连接以 1013 拒绝 |
| `LOG_LEVEL` | `INFO` | 日志级别 |

换唤醒词：改 `keywords.txt`（每行 `token 序列 @显示词`，拼音建模），
用 `scripts/download-model.sh` 的 `WAKE_WORD=...` 重新生成，或用
`sherpa-onnx-cli text2token --tokens-type ppinyin` 现算；其它词没有
验证过的 token 行时脚本会直接失败，不瞎写。

## 模型部署（不 bake 进仓库/镜像）

模型文件约 5.6MB（int8 编码器），部署时下载：

```bash
./scripts/download-model.sh /data/kws/models   # 默认 WAKE_WORD=你好小李
```

容器入口 `entrypoint.sh` 会在模型缺失时自动跑该脚本；下载失败
（网络/SHA 校验失败）则容器明确报错退出。模型持久化在 `kws-data`
卷，容器重建不重复下载。SHA256 与上游地址见脚本头注释
（与 `client/scripts/download-kws-model.sh` 同源同 SHA）。

卷属主（S-2，2026-11-18 第三轮审计 P1）：`docker-compose.yml` 让
边车以 uid 1000 运行，但全新 `kws-data` 卷默认是 root 属主——光靠
`user: "1000:1000"` 写不进卷。修复分两层：① `Dockerfile` 里
`RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models`
（必须在 `VOLUME` 声明之前），全新卷首次挂载时 Docker 把镜像里该
目录的属主 copy-up 进卷，不走 `nas-install.sh` 也能直接
`docker compose up -d kws-sidecar`（冒烟项见
`deploy/NAS_CHECKLIST.md` §8）；② `entrypoint.sh` 在下载前预检
卷目录可写，写不进时给可读 FATAL（含修复命令）再退出，而不是在
`download-model.sh` 的 `cp` 上失败。已存在的 root 属主旧卷不受
copy-up 影响，仍需 `nas-install.sh` §4 的 chown one-shot 迁移。

镜像 pin（S-7，2026-11-18 第三轮审计 P2）：`Dockerfile` 的基础镜像
按 P1-4 同样做法钉 digest（`python:3.12-slim@sha256:5e2bdb…`，查询
方法与更新流程见 `Dockerfile` 头注释），apt 包 `curl`/`bzip2` 同样
钉版本；镜像内以 `USER 1000:1000` 运行（与 compose 的
`user: "1000:1000"` 一致，独立 `docker run` 也不跑 root）。本机无
docker，构建验证待 NAS 实测。

## 安全假设（S-3：边车不启用 TLS，待 bai 定夺）

> **状态：部分修，未决。** 下面是正式安全假设，不是 TLS 实现。
> bai 在“接受回环假设”与“给边车加 TLS”之间二选一之前，边车保持
> 明文 `ws`；定夺后本节即失效（选 TLS 则补自签证书 + 启动期生成/
> 挂载，Hub 的指纹 pin 才真正跑通，另起一项做）。

- **假设内容**：边车不启用 TLS，只监听明文 `ws://127.0.0.1:10400/v1/kws`；
  隔离只靠回环绑定 + hello 的 token 鉴权。`KWS_HOST` 默认值与
  `server.py` 的 `--host` 默认值都是 `127.0.0.1`（S-8 同款改动）；
  `compose` 里也显式传 `KWS_HOST=127.0.0.1`，三层一致。`server.py`
  用 `serve(handler, args.host, args.port, …)` 只绑这一个地址，
  无其他监听；`server.py` 全文无 `ssl`/`SSL`，无任何证书逻辑。
- **风险边界**：流量不出回环，局域网抓包风险不存在；但**任何 NAS
  本机进程都可直连 `127.0.0.1:10400`**，token 以明文 JSON 在 hello
  里传输——本机上跑了不可信进程即假设失效。`wss://` 一配就连不上
  （fail-closed，算优点，不是 bug）。
- **Hub 侧现状**：Hub 的指纹 pin 实现正确（`hub/internal/kws/kws.go:
  tlsConfig()`），但 `hub/config.yaml` 实际配的是明文
  `ws://127.0.0.1:10400/v1/kws`，pin 目前配了也用不上；不要把
  `fingerprint` 当成已生效的安全措施。
- **待定决定**：(a) 接受本假设继续用回环 + token，或 (b) 给边车加
  TLS。决定前不要改 `KWS_HOST` 为 `0.0.0.0`、不要把 10400 发布到
  局域网。

## 运行

```bash
# 本地（开发机，需先有模型；可用实验目录的模型）
python -m venv .venv && .venv/bin/pip install -r requirements.txt
KWS_MODEL_DIR=$HOME/workspace/.kws-exp/models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01 \
KWS_KEYWORDS_FILE=$HOME/workspace/.kws-exp/keywords.txt \
OPEN_XIAOAI_TOKEN='<与 Hub 相同的 Token，≥16 位>' \
.venv/bin/python server.py

# NAS（见 deploy/docker-compose.yml 的 kws-sidecar 服务）
cd ../deploy && docker compose up -d kws-sidecar
curl http://<NAS_IP>:10400/health
```

## 测试

```bash
.venv/bin/pip install -r requirements-dev.txt
.venv/bin/python -m pytest tests/ -v
```

测试起一个真实 server 子进程（端口 10401），用实验的合成样本
做端到端 WebSocket 实测：正样本出 wake、负样本静默、双设备隔离、
断开重连不残留、非法 hello、/health、模型缺失非零退出；另有
P1 回归测试：重复 `device_id` 关闭旧连接、空关键词表拒绝启动、
hello 缺/错 Token 拒绝、未配 Token 拒绝启动、连接数上限拒绝超限、
`/health` 503 分支可达。

### 测试资产自给自足（tests/bootstrap.py）

测试不再硬依赖本机实验目录。`tests/conftest.py` 导入时调
`resolve_assets()`：资产根目录默认是 `~/workspace/.kws-exp`
（已有全部资产时只做存在性检查，不重新下载）；环境变量
`KWS_TEST_ASSETS` 指向另一根目录时（CI 用），在该目录内按同样
结构补齐：模型缺失调 `scripts/download-model.sh` 下载（SHA256
已 pin），关键词表缺失写 2026-11-17 验证过的 token 行，样本缺失
用 Piper TTS（语音从 HF rhasspy/piper-voices v1.0.0 下载，
zh_CN-huayan-medium）现生成、ffmpeg 重采样到 16kHz，噪声样本用
numpy 固定种子生成。任何一步失败都明确报错，不静默跳过。

生成后逐个离线校验（正样本必须检出、负样本必须静默），不合格的
重抽最多 10 次（即最多 11 次检测）：Piper 合成是非确定性的，同一文本两次生成是不同
音频，实测 pos3（“你好小李，今天天气怎么样”）约 1/7 的抽样检不
出——干净状态验证时确实命中一次坏抽样，重抽后通过。校验不过
10 次就报错，不要改样本文本绕过去。

CI（`.github/workflows/ci.yml` 的 python job）跑
`python -m pytest kws-sidecar/tests/ -q`（`KWS_TEST_ASSETS=
/tmp/kws-test-assets`，先装 `requirements.txt` +
`requirements-dev.txt` + `piper-tts` 与 `ffmpeg`）。注意测试的
server 子进程用 `kws-sidecar/.venv/bin/python`（若存在），否则用
跑 pytest 的 python（须装好 `requirements.txt`）。
