# 架构 — 从零重写版（现代化，无历史包袱）

## 三层

```
音箱层（薄 Client，Rust）  →  边缘层（Hub，Go，NAS 上唯一常驻服务）  →  Agent 层（可换）
```

- **音箱层**：只管收音放音。录音 16kHz → Opus → Hub；Hub 下发的 Opus → 解码 → 播放；
  本地 KWS 监听唤醒词，命中上报 `{wake_word, rms_db, timestamp}`；执行 Hub 的 RPC。
  不跑 ASR、不跑 LLM、不管房间、不管音乐同步。
- **边缘层（Hub）**：Device Registry（自动发现 + 语音认领）、Arbitration（250ms 比 RMS）、
  Voice Pipeline（VAD → 流式 STT → Agent → 流式 TTS）、Music Plane（Snapcast 分组 + 延迟校准）、
  HA 接入、OTA。Hub ↔ Agent 走 OpenAI 兼容 `/v1/chat/completions` 或 Xiaozhi 协议；
  Hub ↔ HA 走 Wyoming / ESPHome + MQTT；每台音箱在 HA 里是一个 `assist_satellite`。
- **Agent 层**：Hermes（管家：记忆/技能/定时/场景）、OpenClaw（通用 Agent）、
  DeepSeek / Qwen（直连 LLM）。换 Agent 改 `config.yaml`，不用动音箱。

## 两个平面

| 平面 | 编码 | 采样率 | 要求 |
|---|---|---|---|
| 语音 | Opus | 16kHz | 低延迟、可打断；家电执行 ≤2s，首字 ≤800ms |
| 音乐 | Snapcast（FLAC） | 48kHz | 多房间同步，延迟差 ≤10ms；可分组 |

语音和音乐绝不混在一条链路上：语音打断不能等音乐缓冲，音乐同步不能被语音抢占。

## 关键接口

| 接口 | 方向 | 说明 |
|---|---|---|
| wss :4399 | Client ↔ Hub | 音频帧 / 唤醒事件 / RPC；TLS + Token 鉴权 |
| Wyoming :10300 / :10200 | Hub ↔ Whisper / Piper | STT / TTS |
| MQTT :1883 | Hub ↔ 设备/HA | 注册表 + 发现 |
| HTTP :9092 | 外部 → Hub | 播放文本/音频/TTS、唤醒、打断、状态（抄 coderzc bridge） |
| HA WebSocket :8123 | Hub ↔ HA | 查状态、调服务、读场景 |
| OpenAI 兼容 /v1/chat/completions | Hub ↔ Agent | 换 Agent 不动音箱 |

## 为什么是这个形状

- 原项目的问题：Client 和 Server 耦合、裸 PCM、4399 裸奔、立体声只支持 2 台、
  无仲裁、无多房间。重写时把这五处一次性改掉，而不是逐个打补丁。
- OwnDing 的 Fork 最完整，但是家庭定制版（Hermes + 海尔 + TCL 写死）；
  coderzc 的 bridge 服务端最模块化，但是 Client 还是裸 PCM、无仲裁、多房间靠外部。
  本项目取两边的长处：服务端抄 coderzc 的插件结构，Client 抄 OwnDing 的中断/AEC，
  设备管理与音乐交给 HA + Snapcast。
