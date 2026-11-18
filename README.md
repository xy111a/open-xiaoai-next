# open-xiaoai-next

从零重写的本地语音系统：把刷了 Open-XiaoAI 的小爱音箱（LX06 / OH2P）从小米云手里接过来，
变成你自己的本地语音管家。音箱只管收音放音，NAS 上的 Hub 管仲裁、语音、音乐、家电，
Agent 随便换，Home Assistant 管设备，网易云管音乐。

> **状态：Phase 0 骨架。** 目录、配置、接口定义已定，代码是可编译的骨架，不是能跑的成品。
> 第一关口：先刷一台 LX06，Hub 在 NAS 上跑起来，两台能同步放一首歌。
> 完整需求与开发计划见 `../PLAN.md`（v4 最终版）。

## 硬件范围（冻结）

| 设备 | 内存 | 备注 |
|---|---|---|
| 小爱音箱 Pro LX06（带 DTS 老批次） | 256MB | 必须拆机一次（Micro USB 调试口在主板上）；刷后 DTS 音效回不来，NAS 侧 EQ 补约 80% |
| 小米智能音箱 Pro OH2P | 256MB | 底部 Type-C，免拆机 |
| NAS | UGREEN DXP6800 Pro，64GB | Hub + Whisper + Piper + Snapcast + MPD + NeteaseCloudMusicApi 的宿主 |

只支持这两款。LX05 / L15A（x509 加密分区）不碰。不带 DTS 的新款 LX06（128MB）跑不了本地唤醒词，
本项目没有这款机器，不做精简 KWS。

## 架构

```
音箱 (thin client, Rust)                NAS (Hub, Go)                     服务
┌──────────────────────┐   wss + Opus   ┌──────────────────────┐
│ 录音 16kHz → Opus    │ ◄────────────► │ Device Registry      │ ──► Home Assistant (8123, WebSocket)
│ 播放 ← Opus          │   Token+mTLS   │ Arbitration (250ms)  │ ──► 网易云 (Hub 直连 NeteaseCloudMusicApi)
│ 本地 KWS “你好小李”  │                │ Voice Pipeline       │ ──► LLM: DeepSeek / Qwen / Ollama
│ OTA 自更新           │                │ Music Plane (Snapcast)│ ──► Agent: Hermes / OpenClaw
└──────────────────────┘                │ MQTT + Wyoming       │
                                        └──────────────────────┘
```

原则：**Client 只转发，不做业务逻辑。语音和音乐分两个平面。**
语音走 Opus 16kHz 低延迟、可打断；音乐走 Snapcast 48kHz 同步。

## 目录

```
open-xiaoai-next/
├── README.md            # 本文件
├── LICENSE              # MIT
├── client/              # 音箱端薄客户端（Rust）：录音/放音/KWS/Opus/TLS/OTA
├── hub/                 # NAS 端 Hub（Go）：注册表/仲裁/语音管线/音乐/HA/OTA
│   └── config.yaml      # 唯一的配置文件：房间/唤醒词/Agent/音源
├── firmware-patch/      # 固件补丁流水线：OTA 下载 → 解包 → patch → 打包
├── deploy/              # NAS 部署：docker-compose + .env.example
└── docs/                # 架构说明 + 决策记录
```

配套文档（在上一级目录 `speaker-open-xiaoai-rebuild/`）：
- `PLAN.md` — 需求表 FR-01..FR-15、NFR、分阶段规划、把握评估
- `market-research.md` — 全球需求调研 + Reddit 补查
- `firmware/` — 逆向全记录（`REA_ANALYSIS.md`）与 LX06 1.94.13 固件解包

## 快速开始（Phase 0 之后，按顺序）

1. **刷机**：LX06 拆机一次，按 `firmware-patch/README.md` 的流程刷补丁固件；
   OH2P 底部 Type-C 免拆。刷完 SSH 进去 `free -m` 确认 256MB。
2. **起 Hub**：在 NAS 上 `cd deploy && docker compose up -d`，Hub 自动发现两台音箱。
3. **认领房间**：对着每台说“你好小李，我在客厅”，Hub 记住房间名。
4. **验收第一关口**：两台同步放一首歌，客厅喊只有客厅回。

## 已定决策（摘要，完整记录见 `docs/DECISIONS.md`）

- 方案 B：从零重写，参考原代码当 Spec，不直接 Fork。
- 唤醒词“你好小李”，可换；多唤醒词路由不同 Agent。
- 仲裁按 RMS 信号强度，不依赖房间信息，不追百分百就近；OH2P +6dB 补偿先不加。
- 房间自动发现 + 语音认领，不手填 IP。
- 免拆机伪造 OTA 不可能（RSA + SHA1，私钥在小米手里）——拆一次，之后走 Hub OTA。
- 不做：整点报时、恢复 DTS、语音购物、视频通话、医疗级监测、手机 App。

## 许可

MIT。参考项目：idootop/open-xiaoai、OwnDing/open-xiaoai、coderzc/open-xiaoai-bridge，
均为 MIT 协议。本仓库是重写实现，不直接复制其代码；patch 参数与协议细节以
`firmware/REA_ANALYSIS.md` 中的逆向证据为准。
