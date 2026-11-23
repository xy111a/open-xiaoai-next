# 决策记录

按时间倒序。每个决策写清“定了什么、为什么、谁定的”，改了就追加一条，不改旧条。

## 2026-11-23：三项优化——仲裁升级 / HA 读回验证 / 撤退词快路径（bai 定）

设计灵感来自 home-mate（yes999zc/home-mate，AGPL-3.0）与 Hermes 的 housemate
插件，**只借鉴思路、独立重新实现，未复制其代码文本**（AGPL 代码进 MIT 仓库
会触发强 copyleft，不可直接抄；协议层除外，见下）。三项都在 Hub 侧，不动
部署、固件、真机，不改唤醒词、mTLS、音乐链（PR #14 的点歌快路径行为不变）。

- 仲裁升级（`hub/internal/arbitration`）：纯“谁大谁赢”改为三级裁决——
  响度门限胜出（赢家原始响度 ≥-50dBFS 且有效响度比第二名响 ≥3dB）→
  门限内按到达时间（可信范围 = 与最响者有效值差距 <3dB，证据不可信时全体参与）
  → 完全平局按固定设备序（deviceID 字典序），记 TieBreak，永不静默；
  会话粘性加分：上一轮赢家 20s 内再进裁决时有效响度 +4dB（与 20s 连续对话
  会话同窗），靠加分才赢的记 Sticky；每次裁决记审计日志（原因/响度差/
  到达差/参与数/粘性/输家）。窗口仍是 250ms，不拉到 home-mate 的 2400ms：
  2400ms 覆盖 1.3–1.4s 跨房间触发间隔，但回答要等窗口关闭才开始，会让每次
  唤醒多等 2 秒以上；250ms + 500ms 迟到容忍 + 1s 冷却已覆盖局域网同句唤醒
  的正常 spread，超 250ms 的 spread 仍可能两台各回一次（2026-11-17 已接受
  的失败模式），部署日实测 spread 再定。刻意没做的事：无 SNR/削波/回声质量门
  （唤醒事件里只有 RMSdB 一个响度字段，边车 rms_peak 已由 kws.EffectiveRMS
  折进 RMSdB，编不出质量门）；无新增 PeakdB 字段（客户端提供峰值需改 Rust
  客户端并重刷机，改动不小，边车峰值已在 RMSdB 里）。
- HA 读回验证（`hub/internal/ha` + `hub/internal/router`）：CallService 成功
  只表示 HA 接受了请求，调服务后读回状态比对才算确认。最多读 3 次、间隔
  150ms、每次 2s 超时（本客户端是 WebSocket，读回走 get_states 全量再过滤，
  等价于 REST 的 GET /api/states/{entity}）。三种结果严格区分：确认成功走
  现有成功回复；不一致报“好像没开/没关，可能没生效”；读不到报“没法确认”，
  不谎称失败也不谎称成功。场景（“我回来了”）读回比的是状态变化（场景实体
  的 state 是上次触发时间）。白名单合并：原表 light/switch/climate/cover
  加上 home-mate 的 fan/input_boolean，climate 保留（卧室空调就是 climate，
  去掉会断语音控空调）；门锁/燃气/热水器仍排除。但只保留白名单不够：
  climate 的 state 是运行模式（heat/cool…）不是 "on"，cover 没有
  turn_on/turn_off 服务——白名单合并时“与现有行为兼容”的说法对这两个
  domain 不成立（2026-11-23 审查 M1 修的回归：开空调调服务成功后读回
  永远比对不上，必然报 mismatch）。现在按 domain 算服务与期望
  （`router.haControlTarget`）：light/switch/fan/input_boolean 用
  turn_on/turn_off + "on"/"off"；climate 用 turn_on/turn_off，但开 = state
  不是 "off"/"unknown"/"unavailable"、关 = "off"；cover 用 open_cover/
  close_cover，开 = "open"/"opening"、关 = "closed"/"closing"。幂等预查与
  读回验证用同一套判定。
- 撤退词快路径（`hub/internal/router` + `hub/cmd/hub/main.go`）：
  整句是“停/停止/停下/别说了/别播了/不要说了”时，本地立即切断声音
  （MPD 暂停 + StopAudio 发 interrupt 停 TTS 播报），不进 LLM、不进仲裁、
  不播报确认语（回空回复，复用 PR #14 的空回复跳过 TTS 机制）；覆盖唤醒后
  与 20s 会话内的撤退词。StopAudio 只发 interrupt，不调 pipeline.Cancel
  （撤退词本身正跑在一条 answer 里，Cancel 只会取消自己；已发出的 TTS 帧
  只能靠 interrupt 在客户端侧停住）。无唤醒词的撤退（边车关键词）**不在本次
  范围**，列为后续项：边车是单关键词设计（一次只生成一个唤醒词，只有
  “你好小李”有已验证 token 行），加“停止”需改边车脚本 + 入口 + Hub 过滤 +
  部署验证，且“停止”是常用词、误触发风险未测。

## 2026-11-19：Web 面板改绑 NAS 内网 IP，局域网直开（bai 定）

- `hub/config.yaml` 的 `web_listen` 从 `127.0.0.1:8081` 改到 `192.168.88.247:8081`
  （P0-1 的回环绑定就此作废）；局域网内浏览器直接开 `http://192.168.88.247:8081`，
  不用再建 SSH 隧道。
- 不绑 `0.0.0.0`（`validateWebListen` 仍禁全接口）；只绑这一个明确内网 IP。
  Token 认证不变；NAS IP 变了要同步改 `web_listen` 与健康检查地址。
- nas-install.sh 健康检查、NAS_CHECKLIST/README/CHECKLIST 的面板地址同步改到
  `192.168.88.247:8081`。

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
- P2-12：作废。2026-11-23 bai 决定删除紧急呼叫功能（触发词误报无法接受，
  修不如删），二次确认方案不再做；相关代码与 FR-15 已整体删除。
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

## 2026-11-17：循环提醒进 Phase 3（bai 定，来自 Reddit 补查）

- 循环提醒（FR-14，P1）：HA 自动化 + TTS，小工作量；提醒要说清事由。
- 紧急呼叫（FR-15）：2026-11-23 已删除（bai 定，不再做），见 P2-12 作废记。

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
