# 决策记录

按时间倒序。每个决策写清“定了什么、为什么、谁定的”，改了就追加一条，不改旧条。

## 2026-11-18：六项决定——S-3 / HA / P1-40 / P2-12 / C-15 / gnueabihf（bai 定）

- S-3：接受回环 + token，不给边车加 TLS。`kws-sidecar/server.py` 启动期守门
  （`_is_loopback_host`）：非回环 `--host` / `KWS_HOST` 即拒绝启动；局域网
  暴露须先给边车加 TLS（另起一项做）。
- HA：`hub/config.yaml` 的 `home_assistant.url` 从 `wss://192.168.1.10:8123`
  改回 `ws://192.168.1.10:8123`（bai 的 HA 未配 TLS；此前误写成 wss，握手
  直接失败）。HA 套上带 TLS 的反向代理后再改回 wss。
- P1-40：服务端 KWS 是长期路线（Hub → kws-sidecar，当前唯一能工作的唤醒
  路径）；path A（从源码重编 ORT + sherpa 跑本地 KWS）只留文档，不投入。
  理由：没有 Hub 音箱本来就什么都干不了，本地 KWS 只能解决“Hub 挂了还能
  唤醒”，收益几乎为零；真正的风险是 Hub 单点故障，解法是把 Hub 跑稳。
- P2-12：急救二次确认方案通过，Phase 4 再实现（不挡刷机）。首次触发先问
  “确认要呼叫急救吗？请说'确认呼叫'”，60 秒窗口；说确认/是的/确认呼叫
  或再次触发 = 确认，取消 = 取消，超时 = 不呼叫。触发词须选不易被电视和
  日常对话误触的短语；每次触发与结果记日志，方便以后调阈值。
- C-15 / P2-42：`hub_url` 必填，无默认值（`client/src/config.rs`，与空
  Token 的 fail-closed 一个风格）。写死的 `wss://192.168.1.10:4399` 默认值已删。
- gnueabihf：vendor `.so` 不兼容全局仍只警告；但构建开了 sherpa-kws
  feature（本地 KWS 路径，运行时真的会 dlopen 它）时硬失败。服务端模式
  不加载它，只警告。见 `client/build-armv7-gnueabihf.sh` 第 7 节与
  `client/build-armv7.sh` 第 4 节。

## 2026-11-18：音乐融合 B——砍 Music Assistant，Hub 直连网易云（bai 定）

- 砍掉 Music Assistant 容器（`ghcr.io/music-assistant/server`、`ma-data` 卷、8095 端口全部删除）；
  主路改为 Hub 直连 NeteaseCloudMusicApi（`127.0.0.1:3000`）→ MPD → Snapcast，登录收进 Hub Web 面板扫码。
- `music_folder` 从“不存在即 Fatal”改为可选本地兜底：缺了只警告，不阻塞启动。
  本条取代 2026-11-17“房间自动发现”条里“启动时自动校验，不存在再问”的说法，旧条不改。
- 代价：HA 里没了现成的音乐 `media_player`；语音点歌与 Hub Web 控制不受影响。
  详见仓库根目录 `MUSIC_FUSION.md`（盘点、改动清单、迁移说明、待 Hub 面确认项）。

## 2026-11-17：方案选 B，从零重写（bai 定）

- 选 B 不选 A（Fork OwnDing/coderzc 继续改）或 C（只做 Hub）。
- 理由：B 架构干净、无历史包袱、可维护 2 年；但不是闭门造车——参考原代码当 Spec，
  抄它的 patch 参数和 ALSA 配置，重写 Server 和 Client 结构。
- 成果：本仓库 `open-xiaoai-next`，Phase 0 骨架。

## 2026-11-17：整点报时不做（bai 定）

- 海外盲人用户有这需求（Reddit / Facebook 2026-08 有人发帖问 Alexa 团队），
  但 bai 决定不做。已从 PLAN.md 和 market-research.md 的“顺手可加”里删掉。
- 循环提醒保留（FR-14，Phase 3）。

## 2026-11-17：循环提醒进 Phase 3，紧急呼叫 Phase 4 可选（bai 定，来自 Reddit 补查）

- 循环提醒（FR-14，P1）：HA 自动化 + TTS，小工作量；提醒要说清事由。
- 紧急呼叫（FR-15，P2，可选）：喊“救命”/“打 120”触发 HA 电话网关或推送家人。

## 2026-11-17：仲裁按信号强度，不追百分百就近（bai 定）

- 仲裁机制就是 RMS 信号强度：250ms 窗口内比唤醒语音量，响的回，静的静默。
- 不依赖房间信息（房间只为 HA 命令和对讲服务），不追求百分百选最近，
  只保证永远只回一台、且是能听清的那台；偶尔远处那台赢了也能接受。
- OH2P +6dB 补偿是可选微调，先不加，实测有问题再加。

## 2026-11-17：房间自动发现 + 语音认领（bai 定）

- Hub 启动时自动扫描局域网（mDNS/ARP）发现两台音箱的 IP 和型号，不用手填 IP。
- 房间名每台语音认领一次（“你好小李，我在客厅”），换房间再喊一声就改。
- NAS 音乐文件夹默认 /volume1/music，启动时自动校验，不存在再问。

## 2026-11-17：唤醒词“你好小李”（bai 定）

- 自定义唤醒词，替代默认“小爱同学”；换词 = 换 KWS 模型文件 + 改 config，不用重刷机。
- 多唤醒词可路由不同 Agent（当前只有“你好小李”→Qwen）。
- 2026-11-19 bai 定：删掉“你好龙虾”→OpenClaw 路由（死路由：边车只检测“你好小李”，该路由永远触发不了）；openclaw Agent 保留作备用。
- 原词“你好小七”已废弃，全仓替换。

## 2026-11-16：免拆机伪造 OTA 不可能（逆向结论，已闭环）

- LX06 OTA 校验是 RSA + SHA1（miso），签名覆盖包体从偏移 0xc 起，私钥只在小米手里；
  公钥首选来自 `/dev/miso` 内核驱动（AES-128-CBC 加密后还要 XOR），换公钥文件也没用。
- 唯一现实路线：拆机一次刷补丁固件，之后全部走 Hub 自有 OTA。
- 证据：`../firmware/REA_ANALYSIS.md`（miso 240 函数反编译，172 个写出伪代码）。

## 2026-11-16：DTS 回不来，NAS 侧 EQ 补约 80%（逆向结论，已闭环）

- `controls [4]` 是摆设（Gain 口从不被读）；真模式在共享内存 key `0x2234`，
  写内存的是 `/usr/bin/sound_effect`（ubus `dtsctl`）。断的是整条链，不是丢了一个参数。
- 米家“柔和 / 澎湃 / 人声”对应模式 0/1/2 的映射固件里没有，名字只在米家 App/云端。

## 2026-11-16：硬件确认（bai 确认）

- LX06 带 DTS = 256MB 老批次，能跑本地 KWS；不带 DTS 的新款是 128MB，跑不了。
- OH2P 也是 256MB，支持本地 KWS。家里现在只有这两台。
- 刷机后 DTS 音效没了，代价是换本地 KWS；双分区可切回原厂系统恢复 DTS，但会丢本地功能。
