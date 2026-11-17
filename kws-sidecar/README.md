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
  `{"type":"hello","device_id":"lx06-192-168-1-20","sample_rate":16000}`
  `sample_rate` 必须为 16000，否则返回 `error` 并关闭连接。
- Hub → 边车，二进制帧：原始 PCM int16 LE 单声道 16kHz，
  20ms 一帧 = 640 字节（超长帧按 20ms 切分喂入）。
- 边车 → Hub（JSON 文本）：
  - `{"type":"ready"}`：hello 确认；
  - `{"type":"wake","device_id":"...","keyword":"你好小李","rms":0.42,"timestamp_ms":...}`：
    检出唤醒词；`rms` 为检出时最近 1s 音频的 RMS（0~1），
    `timestamp_ms` 为服务端 Unix 毫秒时间戳；检出后该设备流自动
    复位（re-arm），可继续检下一次唤醒；
  - `{"type":"error","message":"..."}`：协议错误或模型故障。
- 健康检查：`GET /health` → 200 `{"status":"ok","model_loaded":true}`；
  模型未加载好时 503。模型加载失败时进程直接非零退出（不静默跑）。

## 并发与状态

- 每 `device_id` 一个独立的 KeywordSpotter 流（`create_stream()`），
  互不干扰；解码在单事件循环内串行执行（实验测得 RTF 很低，
  少量设备并发无压力；大规模并发未测，不要超量挂设备）。
- 连接断开即丢弃该设备流状态；同一 `device_id` 重连从零开始，
  旧的半句唤醒词不会残留（见 `tests/test_kws_sidecar.py::
  test_reconnect_drops_old_state`）。
- 同一 `device_id` 在旧连接未断时又连上来，旧流被丢弃并记一条 warning。

## 配置

环境变量（或同名命令行参数 `--host/--port/--model-dir/...`）：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `KWS_HOST` | `0.0.0.0` | 监听地址 |
| `KWS_PORT` | `10400` | 监听端口（已确认与现有服务无冲突） |
| `KWS_MODEL_DIR` | `/data/kws/models` | 模型目录 |
| `KWS_KEYWORDS_FILE` | `<模型目录>/keywords.txt` | 关键词表（默认含“你好小李”） |
| `KWS_THRESHOLD` | `0.25` | keywords_threshold（实验验证值） |
| `KWS_SCORE` | `1.0` | keywords_score（实验验证值） |
| `KWS_NUM_THREADS` | `1` | sherpa 解码线程数 |
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

## 运行

```bash
# 本地（开发机，需先有模型；可用实验目录的模型）
python -m venv .venv && .venv/bin/pip install -r requirements.txt
KWS_MODEL_DIR=$HOME/workspace/.kws-exp/models/sherpa-onnx-kws-zipformer-wenetspeech-3.3M-2024-01-01 \
KWS_KEYWORDS_FILE=$HOME/workspace/.kws-exp/keywords.txt \
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
断开重连不残留、非法 hello、/health、模型缺失非零退出。

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
重抽最多 10 次：Piper 合成是非确定性的，同一文本两次生成是不同
音频，实测 pos3（“你好小李，今天天气怎么样”）约 1/7 的抽样检不
出——干净状态验证时确实命中一次坏抽样，重抽后通过。校验不过
10 次就报错，不要改样本文本绕过去。

CI（`.github/workflows/ci.yml` 的 python job）跑
`python -m pytest kws-sidecar/tests/ -q`（`KWS_TEST_ASSETS=
/tmp/kws-test-assets`，先装 `requirements.txt` +
`requirements-dev.txt` + `piper-tts` 与 `ffmpeg`）。注意测试的
server 子进程用 `kws-sidecar/.venv/bin/python`（若存在），否则用
跑 pytest 的 python（须装好 `requirements.txt`）。
