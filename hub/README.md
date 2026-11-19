# Hub（NAS 端服务，Go）

NAS 上唯一常驻的服务，管六件事：设备注册表、就近仲裁、语音管线、音乐平面、HA、OTA。
Agent（Hermes / OpenClaw / DeepSeek / Qwen）挂在 Hub 外面，走 OpenAI 兼容接口。

## 构建

```bash
cd hub
go build -o hub ./cmd/hub
./hub -config config.yaml
```

## 模块

| 包 | 职责 | 阶段 |
|---|---|---|
| `internal/config` | config.yaml 的结构与加载 | Phase 0 已定 |
| `internal/discovery` | mDNS/ARP 扫描找音箱 IP + 型号，不用手填 IP | Phase 1 |
| `internal/arbitration` | 250ms 窗口比 RMS 信号强度，响的回，静的静默 | Phase 2 |
| `internal/voice` | VAD → faster-whisper → Agent → Piper，流式 | Phase 1–3 |
| `internal/music` | Snapcast 分组 + 延迟校准，音源网易云；跟随播放（follow-me，见 `followme.go`）：在 B 房间喊唤醒词，音乐从 A 房间跟过来 | Phase 2 |
| `internal/ha` | HA WebSocket：查状态、调服务、读场景 | Phase 1 |
| `internal/ota` | Client/Hub 远程升级，失败回滚 boot1 | Phase 4 |

## 端口（与 Client / 服务约定）

| 端口 | 用途 |
|---|---|
| 4399 | wss，接 Client（Token 鉴权；原项目裸奔端口，本项目必须加密 + 鉴权） |
| 10300 | Wyoming STT（faster-whisper，small-int8，zh） |
| 10200 | Wyoming TTS（Piper） |
| 1883 | MQTT（设备注册表 + HA 发现） |
| 9092 | HTTP API（播放文本/音频/TTS、唤醒、打断、状态查询，抄 coderzc bridge 的接口） |
| 1704/1705/1780 | Snapcast（server/stream/http） |

## 配置

只改 `config.yaml`。密钥（HASS_TOKEN、LLM API Key）走环境变量，**不要写进文件**。

跟随播放（follow-me）：`hub.follow_me`（默认 `true`），在 A 房间放音乐时去 B
房间喊唤醒词，音乐跟到 B 房间（同一首歌、同一位置；A 房间停播）。
Web 面板 `PUT /api/config` 也能改，立即生效。实现上 MPD 不停——全屋只有
一台 MPD、一条流，搬家只在 Snapcast 路由层做（`internal/music/followme.go`），
所以位置精确连续、直播流也不用 seek；目标音箱离线时不搬、只记日志。

## 参考实现（当 Spec，不直接抄）

- coderzc/open-xiaoai-bridge：服务端插件结构 + HTTP API 9092 + Token 鉴权
- OwnDing/open-xiaoai：连续对话 + 打断 + 子带 AEC（已在同款硬件跑通）
