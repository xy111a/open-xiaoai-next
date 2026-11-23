# 插件架构 — Agent / 音源 / 技能

Hub 的三类可换件都是**配置即接入**：改 `hub/config.yaml`（或 Web 面板
`/api/config`）就行，不改 Hub 代码、不重新编译、不重刷音箱。
KWS 仍在 NAS 端（服务端 KWS 边车），不在音箱上；本页不涉及降级。

## 1. Agent 插件（换大脑）

任何 OpenAI 兼容 `/v1/chat/completions` 的服务都能当 Agent：
Hermes / OpenClaw / 本地 OVMS / DeepSeek / 云端 Qwen / OpenRouter……

```yaml
agents:
  hermes:
    base_url: "http://hermes:8642/v1"
    model: "deepseek-chat"
    system_prompt: "你是家里的语音管家，回答简短口语化，一两句话。"
  deepseek:  # 云端例子：取消注释 + 配好密钥就能用
    base_url: "https://api.deepseek.com/v1"
    model: "deepseek-chat"
    api_key_env: "DEEPSEEK_API_KEY"
```

字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | `/v1` 结尾的根地址；`https` 允许公网（云端 LLM），`http` 只许内网（密钥不能明文上公网） |
| `model` | 是 | 模型名，原样发给上游 |
| `system_prompt` | 否 | 该 Agent 的 system 提示词；不写用默认“你是家里的语音管家，回答简短口语化，一两句话。” |
| `api_key_env` | 否 | 读密钥的环境变量名（如 `DEEPSEEK_API_KEY`）；不写用全局 `LLM_API_KEY` |

密钥**不写进配置文件**，只写变量名；密钥本身放在 NAS 的 Hub 环境里
（`deploy/.env` 或容器环境）。面板 GET 会显示 `api_key_env`
（只是变量名，不是密钥），不会泄露密钥。

路由：`wake_word_routes` 里写 `唤醒词 → Agent 名`，如
`"你好小李": hermes`。没进路由的 Agent 也可以存在（备用），
不影响启动。改 Agent 配置后重启 Hub 生效（面板会提示）。

注意：只有 OpenAI 兼容协议能直接接。Claude 原生 API（`/v1/messages`）
不是这个协议，要走一个 OpenAI 兼容的网关/代理才能接，
不能直接填 `api.anthropic.com`。

## 2. 音源插件（换“去哪找歌”）

点歌走音源链（`hub/internal/music.Source` 接口）：按顺序试，
主路找不到/出错即落到下一个。

```yaml
music:
  primary: netease   # 第一音源
  fallback: local    # 第二音源；为空表示不兜底
music_folder: /music   # 本地曲库目录（容器内路径，MPD 已挂载）
```

| 音源 | 说明 |
|---|---|
| `netease` | 网易云直连（主路）：搜歌 + 并行取可播地址；搜到但没地址（VIP/无版权）时算“没播成”，继续试下一个 |
| `local` | MPD 本地曲库（兜底）：按文件名找歌（先试歌名部分，如“周杰伦的青花瓷”→“青花瓷”），用 `MPD.PlayFile` 播 |

行为：

- 点歌主路阶段总预算 8 秒（`Router.MusicBudget`，默认 8s），超时先播报
  “网易云有点慢，先试本地”再试本地兜底。
- 本地歌的当前曲来源记为 `local`；本地歌没有在线歌词，问歌词时明说
  “本地歌曲没有在线歌词”，不谎报“还没在放歌”。
- `music_folder` 不存在时本地音源不可用，点歌失败明说“本地曲库还没配好”。

加一个**新音源类型**（如 Spotify、本地 HTTP 歌单）需要写一个小适配器：
实现 `music.Source` 接口（`Name()` + `Search(ctx, query, limit)`，
返回 `[]music.Playable`：`URL` 走 `MPD.PlayURL`，`File` 走
`MPD.PlayFile`），再在 `music.Chain` 的顺序里排进去。`Router` 不用动。
内置的两个音源换顺序、开关兜底只改配置。

## 3. 技能插件（语音触发词 → HTTP 端点）

```yaml
skills:
  - name: news
    triggers: ["新闻", "今日头条", "热点"]
    endpoint: "http://127.0.0.1:18080/v1/skill/news"
    timeout_seconds: 10
```

字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | 是 | 技能名，全局唯一 |
| `triggers` | 是 | 触发词表，至少一个；STT 文本含任一词（子串匹配）即命中 |
| `endpoint` | 是 | HTTP 端点，只许内网 `http(s)` 地址，不许公网 |
| `timeout_seconds` | 否 | 调用超时（秒），默认 10，范围 0–120 |

端点协议：Hub `POST {"text": "<STT 原文>", "device_id": "<设备 ID>"}`，
`Content-Type: application/json`；端点回以下任一形态：

- JSON `{"reply": "..."}`（或 `{"text": "..."}`）；
- 纯文本（直接当回复）。

回复体上限 64KB；技能只应回一两句话（Hub 经现有 TTS 通路播出）。

路由顺序（`Router.Route`）：撤退词 → 对讲/房间认领 → 提醒 →
场景/记忆 → 音乐 → 家电 → **技能** → Agent。技能在 Agent 之前，
但在音乐/家电之后：所以“播放新闻联播”这类话仍先走音乐，
只有音乐/家电都不接的文本才会落到技能。多个技能都命中时按配置顺序
取第一个。

失败处理：技能调用失败（连不上、非 200、超时）或回空时，
Hub 记日志并**落回 Agent**，不谎报成功、不卡死。

### 示例技能：news（桩）

`config.yaml` 自带的 `news` 是**占位桩**，endpoint 指向本机一个
不存在的端口（`127.0.0.1:18080`，什么都没监听），只是演示配置形状：
说“今天有什么新闻”会命中技能、调用失败、落回 Agent。
接真实技能时把 `endpoint` 改成真实内网地址即可，不改代码。

本地起一个桩端点验证（返回固定新闻）：

```bash
python3 -c "
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        print('skill got:', body)
        out = json.dumps({'reply': '今日头条：测试新闻。'}, ensure_ascii=False).encode()
        self.send_response(200); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(out))); self.end_headers()
        self.wfile.write(out)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', 18080), H).serve_forever()"
```

注意：不要把技能 endpoint 指向 Intel Hub——技能端点是家里的本地服务，
新闻类技能应由专门的技能服务提供，不要直连情报后台。

## 改动索引（实现位置）

| 插件 | 配置 | 实现 |
|---|---|---|
| Agent | `config.Agent`（`api_key_env` / `system_prompt`）、`Validate` 放行 `https` 公网 Agent | `hub/internal/agent`（`NewFull` / `Prompt()`）、`hub/cmd/hub/main.go`（逐 Agent 解密钥） |
| 音源 | `config.Music`（`primary` / `fallback`） | `hub/internal/music/source.go`（`Source` / `NeteaseSource` / `LocalSource` / `Chain`）、`hub/internal/router`（`musicChain` / `playPlayables`） |
| 技能 | `config.Skill`（`skills` 表） | `hub/internal/skill`（`Registry.Match` / `Call`）、`hub/internal/router`（`skillRoute`，在 Agent 之前） |
| 面板 | ——（`PUT /api/config` 通用合并已覆盖） | `hub/internal/web`（`sensitiveKey` 不脱敏 `*_env` 变量名） |

测试：`hub/internal/agent/agent_plugin_test.go`、
`hub/internal/music/source_test.go`、
`hub/internal/skill/skill_test.go`、
`hub/internal/router/router_plugins_test.go`（桩 Agent + 桩 Skill +
桩音源走真实 Router 的端到端路由）、
`hub/internal/config/config_plugins_test.go`、
`hub/internal/web/web_plugins_test.go`。
