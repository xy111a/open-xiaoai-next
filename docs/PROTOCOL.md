# Client ↔ Hub 线协议（v1，Phase 1 冻结）

传输：WebSocket Secure（wss），Hub 监听 `:4399`。Hub 首次启动自签 CA，
证书与私钥存在 Hub 数据目录（`hub-data/tls/`）；Client 用 `client.toml` 里的
`hub_ca_fingerprint`（SHA-256 指纹）校验 Hub 证书。Token 鉴权必做：
Client 在 `hello` 里带 `token`，与 Hub 的 `OPEN_XIAOAI_TOKEN` 一致才放行，
否则 Hub 回 `hello_reject` 并关闭连接。

本地测试可把 Hub 设为 `tls: false`（纯 ws），但生产部署必须 wss。
mTLS（Client 证书）为 Phase 4 可选项，本阶段不强制。

## 音频

- 语音：Opus，16kHz 单声道，20ms 一帧（320 采样点），VOIP 应用，约 16kbps。
- 二进制帧：第 1 字节是帧类型，`0x01` = Opus 语音帧，其后是 Opus 数据。
  文本帧一律是 JSON 消息（见下）。
- 音乐不走这条链路：Snapcast 独立进程走 48kHz。

## JSON 消息（文本帧）

### Client → Hub

| type | 字段 | 说明 |
|---|---|---|
| `hello` | `model`（lx06/oh2p）、`ram_mb`、`version`、`token`、`room`（可选，已认领的房间名） | 连接后第一条消息 |
| `wake` | `wake_word`、`rms_db`（float）、`ts_ms`（Client 毫秒时间戳） | 本地 KWS 命中后上报 |
| `audio_start` | — | 开始推语音流（唤醒仲裁赢了之后） |
| `audio_end` | — | 一句话说完（VAD 静音后） |
| `rpc_result` | `id`、`ok`、`error`（可选） | 回应 Hub 的 rpc |
| `room_claim` | `room` | 语音认领房间，如“客厅” |
| `status` | `volume`、`playing`（bool）、`version` | 定期状态上报（每 30s） |

### Hub → Client

| type | 字段 | 说明 |
|---|---|---|
| `hello_ok` | `device_id`、`room`（已认领则带） | 鉴权通过 |
| `hello_reject` | `reason` | 鉴权失败，随后关闭连接 |
| `wake_win` | — | 仲裁赢了：继续听，说话 |
| `wake_lose` | — | 仲裁输了：静默，不理这次唤醒 |
| `rpc` | `id`、`method`、`params` | 方法：`start_play` / `stop_play` / `start_recording` / `stop_recording` / `play_text`（params.text）/ `set_volume`（params.volume 0–100） |
| `interrupt` | — | 打断：立即停播正在放的 TTS |
| `ota` | `version`、`url`、`sha256` | Phase 4：一键 OTA 推送 |
| `room_ok` | `room` | 认领成功回执 |

## 仲裁流程（FR-02）

1. 各 Client 本地 KWS 命中 → 发 `wake`（带 RMS）。
2. Hub 收到第一条 `wake` 后开 250ms 窗口，收集窗口内所有 `wake`。
3. 窗口关闭：RMS 最大的赢 → 发 `wake_win`，其余发 `wake_lose`。
4. 只有一台上报时直接赢；窗口内无上报则什么都不发。
5. OH2P +6dB 补偿：**先不加**（2026-11-17 已定），实测有问题再加。

## 打断（barge-in）

1. 音箱放 TTS 时麦克风保持 live（server 模式持续推流；放音与采集是
   两路独立 ALSA 设备，不互相静音），Hub 的 KWS 边车在放音中仍能
   检出唤醒词。
2. 唤醒词到达时，若设备在放音（Hub 侧态 Speaking），Hub 立刻发
   `interrupt`（不等 250ms 仲裁）停播，设备进 Listening、开新一轮听；
   没在放音时不发多余的 `interrupt`，也不停别的设备。
3. 仲裁照常：赢家收 `wake_win` 继续说，输家收 `wake_lose` 回 Idle。
4. AEC（放音声被麦克风采到、误触发唤醒）只能真机验证：音箱在真实
   房间放 TTS 的同时说唤醒词，看打断成功率是否达标。

## 对讲流程（FR-04）

- 半双工（Phase 3）：发起方说“呼叫卧室”→ Hub 找到卧室设备 → 对卧室发 `rpc start_recording`，
  发起方音频帧原样转发到卧室 `audio` 二进制帧；说完（`audio_end`）即挂断。
- 广播：同一流程发给所有设备。
- 全双工（Phase 4）：双向同时推流，需要回声消除（OwnDing 子带 AEC 思路），
  本阶段只做半双工。

## 房间认领流程

1. 用户对音箱说“你好小李，我在客厅”（唤醒词 + 房间名在同一句）。
2. Hub 语音管线识别出房间名 → 记入 Device Registry → 回 `room_ok`。
3. 换房间再说一次就改。房间名只用于 HA 命令（“关这屋的灯”）和对讲路由，
   **不用于仲裁**。

## 版本

- v1（本文件）：hello/wake/rpc/interrupt/room_claim/status + 二进制 Opus 帧。
- 后续版本只加字段，不改已有字段语义；`hello` 里带 `version` 供协商。
