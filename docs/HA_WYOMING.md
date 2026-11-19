# HA 融合（a）：把 Whisper / Piper 注册进 Home Assistant 做 Wyoming STT/TTS

Home Assistant 自带 Wyoming 集成：给它一个 host + port，它就去连
一个 Wyoming 服务做语音识别（STT）或语音合成（TTS）。NAS 上
Whisper（`oxn-whisper`，STT）与 Piper（`oxn-piper`，TTS）这两个
容器本身就是 Wyoming 服务（rhasspy 官方镜像），不用改容器、不用
装插件——只要在 HA 里加两次集成，分别指过去就行。

> 状态：本功能只在“假 HA”探针与假 Wyoming 服务上验证过
> （`hub/internal/wyoming/probe_test.go` + `cmd/wyoming-probe`），
> **尚未在真 NAS、真 HA 上走通过**。真机步骤见下面“在 NAS 上做”
> 与文末的待验证清单。

## 先决条件

- NAS 上 `docker compose up -d` 已起，且 `oxn-whisper` /
  `oxn-piper` 两个容器在跑（`docker ps | grep oxn-` 能看到）。
- HA 已装好、能打开（NAS 上的 `homeassistant` 容器，:8123）。

## 第 1 步：先在 NAS 上跑探针（5 分钟，必做）

探针就是“假 HA”：按 HA Wyoming 集成的真实调用顺序
（`describe` → `transcribe` / `synthesize`）去问两个容器，
答得对才值得去 HA 里加集成。探针不连真 HA、不改容器。

在 NAS 上（Hub 与两个容器同一台机器）：

```sh
cd open-xiaoai-next/hub
go run ./cmd/wyoming-probe --stt 127.0.0.1:10300 --tts 127.0.0.1:10200
```

都打印 `[OK]` 才继续：

```
[OK] stt 127.0.0.1:10300
  ok   dial        TCP 已连通
  ok   describe    info 正常，asr 服务：whisper
  ok   transcribe  transcribe 正常返回（静音识别结果 ""，空字符串也算通过）
[OK] tts 127.0.0.1:10200
  ok   dial        TCP 已连通
  ok   describe    info 正常，tts 服务：piper
  ok   synthesize  synthesize 正常，返回 … 字节 PCM（… 秒 16kHz 音频）
```

说明：`transcribe` 探的是 0.5 秒静音——静音本就没话，返回空
字符串也算通过，只要求协议走完、服务端不报错。

## 第 2 步：在 HA 里加两次 Wyoming 集成

HA → 设置 → 设备与服务 → 添加集成 → 搜 **Wyoming**。

| 集成 | Host | Port | 语言 | 加完得到 |
|------|------|------|------|----------|
| STT（Whisper） | 见下面“Host 填什么” | **10300** | Chinese（zh） | 一个 STT 实体 |
| TTS（Piper） | 见下面“Host 填什么” | **10200** | Chinese（zh_CN） | 一个 TTS 实体 |

加完再去 HA → 设置 → 语音助手 → 添加助理，STT 选上面加的
Whisper、TTS 选上面加的 Piper，就能对 HA 说话了（HA 的
Assist 走 Whisper 识别、Piper 合成；和 Hub 的语音管线是两条
并行的路，互不干扰）。

### Host 填什么——关键点，先确认再填

Whisper / Piper 的端口在 `deploy/docker-compose.yml` 里只绑了
`127.0.0.1`（有意为之：Wyoming 协议本身无鉴权，只许本机回环
访问，见 P1-4）。所以 **HA 必须能连到 NAS 的本机回环** 才行：

- **HA 容器用 `network_mode: host`（推荐）**：HA 与 NAS 共享网络，
  Host 填 **`127.0.0.1`**，端口照上表填。这是唯一已确认可行的填法。
- **HA 容器用 bridge 网络**：HA 容器里的 `127.0.0.1` 是 HA 自己，
  连不上宿主的 Whisper/Piper；填 NAS 局域网 IP（如 `192.168.1.x`）
  也连不上——因为端口只绑了回环，没绑局域网。此时二选一：
  1. 把 HA 容器改成 host 网络（推荐，改动最小），再按上面填
     `127.0.0.1`；或
  2. 把 `docker-compose.yml` 里 whisper/piper 的端口改绑
     `0.0.0.0` 再填 NAS 局域网 IP——**不推荐**：改完局域网内任何
     人都能免鉴权调用你家的 STT/TTS，还能冒充 Wyoming 服务端
     攻击 Hub（P1-1 的攻击面）。真要走这条路，先找 Friday
     评估，不要自己改。

> 也就是说：**先确认 HA 容器是不是 host 网络**（`docker inspect
> homeassistant | grep NetworkMode`）。不是 host 网络时不要在 HA
> 里反复加集成——先按上面二选一处理。

## 不用改的东西（已确认）

- **Whisper / Piper 容器不用改**：`rhasspy/wyoming-whisper`
 （`--model small-int8 --language zh --beam-size 1`）与
  `rhasspy/wyoming-piper`（`--voice zh_CN-huayan-medium`）
  原生就是 Wyoming 服务；`describe` 返回的 `asr` / `tts`
  信息正是 HA 认服务要的东西。Hub 自己的 STT/TTS 走同一套
  Wyoming 客户端（`hub/internal/wyoming`），已在用。
- **Hub 不用改**：Hub 只是这两个容器的另一个客户端；HA 接入
  后 HA 与 Hub 各连各的，互不抢占（Wyoming 服务端支持多客户端
  各自建连）。

## 排查表（探针 FAIL 时按检查名查）

| 探针 FAIL 在 | 意思 | 查什么 |
|---|---|---|
| `dial` | TCP 都连不上 | `docker ps` 看容器在不在；`docker logs oxn-whisper` / `oxn-piper`；端口是不是还绑着 127.0.0.1:10300/10200（`ss -ltnp \| grep -E '10300\|10200'`） |
| `describe` | 连上了但不是 Wyoming 协议 / info 里没 asr/tts | 端口是不是被别的服务占了；容器日志里有没有 Wyoming 报错；镜像是不是 `rhasspy/wyoming-whisper` / `rhasspy/wyoming-piper` |
| `transcribe` | Whisper 拒绝转写 | 容器内存够不够（whisper 限额 4G，small-int8 首次加载模型要几十秒，等容器 healthy 再跑探针）；看 `docker logs oxn-whisper` |
| `synthesize` | Piper 拒绝合成 | 音色名对不对（应为 `zh_CN-huayan-medium`）；看 `docker logs oxn-piper` |

HA 里加集成时若报“连接失败”而探针是 OK 的：先看上面“Host
填什么”——十有八九是 HA 容器网络模式的问题，不是 Whisper/Piper
的问题。

## 待真机验证（还没做过）

1. 在真 NAS 上跑探针，两个都 `[OK]`。
2. 确认 HA 容器的网络模式，按上表填 Host，在 HA 里加上两个
   Wyoming 集成，看到 STT/TTS 实体。
3. 在 HA 里建一个语音助手（STT=Whisper、TTS=Piper），对 HA
   说一句话走完一轮（识别 → 回答 → 合成播报）。
4. 顺带确认：HA 接入后 Hub 自己的语音（唤醒 → 回答）不受影响。
