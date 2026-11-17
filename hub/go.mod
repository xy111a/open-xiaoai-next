module github.com/huajun/open-xiaoai-next/hub

go 1.23

require (
	github.com/gorilla/websocket v1.5.3
	gopkg.in/hraban/opus.v2 v2.0.0-20230925203106-0188a62cb302
	gopkg.in/yaml.v3 v3.0.1
)

// TODO Phase 1: MQTT 客户端（paho.mqtt.golang）— 设备注册表 + HA 发现
// TODO Phase 1: Wyoming 协议客户端 — faster-whisper（10300）/ Piper（10200）
// TODO Phase 2: Snapcast 控制客户端 — 分组 + 延迟校准
// TODO Phase 3: OpenAI 兼容客户端 — Hermes / OpenClaw / Ollama（/v1/chat/completions）
