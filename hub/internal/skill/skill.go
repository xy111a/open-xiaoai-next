// Package skill 是技能插件：名字 + 触发词 + HTTP 端点，纯配置接入。
//
// 路由：语音文本含任一触发词（子串匹配）即命中该技能，Hub POST 文本
// 给端点，把端点回的文本经现有 TTS 通路播出。技能在 Agent 之前、
// 音乐/家电/提醒之后；技能调用失败或回空时落回 Agent（不谎报成功）。
// 加新技能只需在 config.yaml 的 skills 里加一条，不改 Hub 代码。
// 端点只许内网地址（见 config.Validate），不要指向公网服务。
package skill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxReplyBody 是技能回复体的读取上限（64KB；技能只应回一两句话）。
const maxReplyBody = 64 << 10

// defaultTimeout 是没配 timeout_seconds 时的调用超时。
const defaultTimeout = 10 * time.Second

// maxClientTimeout 是共享 HTTP 客户端的安全上限：config.Validate 允许
// 单个技能配 timeout_seconds 到 120s（见 config/config.go），客户端
// 超时必须不小于它，否则会截断合法调用；单次调用的主约束仍是
// Call 里的 context.WithTimeout（s.timeout()），客户端超时只是
// 防止连接挂住无上限的兜底。
const maxClientTimeout = 12 * defaultTimeout // 120s = timeout_seconds 上限

// Skill 是一个技能插件的配置（与 config.Skill 同源，main.go 做转换）。
type Skill struct {
	// Name 是技能名，全局唯一，例如 "news"。
	Name string
	// Triggers 是触发词表，例如 ["新闻", "今日头条"]。
	Triggers []string
	// Endpoint 是技能 HTTP 端点，只许内网 http(s) 地址。
	Endpoint string
	// TimeoutSeconds 是调用超时（秒）；<=0 时用默认 10s。
	TimeoutSeconds int
}

// timeout 返回生效的调用超时。
func (s Skill) timeout() time.Duration {
	if s.TimeoutSeconds > 0 {
		return time.Duration(s.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

// Registry 是技能表：按配置顺序匹配，第一个命中的胜出。
type Registry struct {
	skills []Skill
	http   *http.Client
}

// New 新建技能表（skills 为空时 Match 恒返回 nil，Router 直接走 Agent）。
func New(skills []Skill) *Registry {
	return &Registry{skills: skills, http: &http.Client{Timeout: maxClientTimeout}}
}

// Skills 返回已注册的技能名（Web /api/status 之类展示用，
// 触发词与端点不外泄到状态接口——只给名字）。
func (r *Registry) Skills() []string {
	names := make([]string, 0, len(r.skills))
	for _, s := range r.skills {
		names = append(names, s.Name)
	}
	return names
}

// Match 按触发词匹配文本：含任一触发词（子串匹配，不区分大小写，
// 中文不受大小写影响）即命中；多个技能都命中时按配置顺序取第一个。
// 未命中返回 nil。
func (r *Registry) Match(text string) *Skill {
	lowered := strings.ToLower(text)
	for i := range r.skills {
		for _, tr := range r.skills[i].Triggers {
			if tr == "" {
				continue
			}
			if strings.Contains(lowered, strings.ToLower(tr)) {
				return &r.skills[i]
			}
		}
	}
	return nil
}

// skillRequest 是 POST 给技能端点的请求体。
type skillRequest struct {
	Text     string `json:"text"`      // STT 识别出的原文本
	DeviceID string `json:"device_id"` // 说话的设备 ID
}

// skillResponse 是技能端点可回的 JSON 形态之一。
type skillResponse struct {
	Reply string `json:"reply"`
	Text  string `json:"text"`
}

// Call 调一次技能：POST {"text": ..., "device_id": ...}，
// 接受两种回复形态：JSON {"reply": "..."}（或 {"text": "..."}）
// 或纯文本（Content-Type 不限，按体裁自动识别）。
// 回复体裁剪到 64KB；非 200、超时、空回复都返回 error，
// 调用方（Router）据此落回 Agent。
func (r *Registry) Call(ctx context.Context, s *Skill, deviceID, text string) (string, error) {
	body, err := json.Marshal(skillRequest{Text: text, DeviceID: deviceID})
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("技能 %q 调用失败: %w", s.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("技能 %q 返回 %d: %s", s.Name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBody))
	if err != nil {
		return "", err
	}
	// 先按 JSON 解析 {"reply": "..."} / {"text": "..."}；
	// 不是 JSON 就当纯文本（去首尾空白）。
	var out skillResponse
	if err := json.Unmarshal(raw, &out); err == nil {
		if strings.TrimSpace(out.Reply) != "" {
			return strings.TrimSpace(out.Reply), nil
		}
		if strings.TrimSpace(out.Text) != "" {
			return strings.TrimSpace(out.Text), nil
		}
		// JSON 能解析但两个字段都空：当空回复处理，不回退到纯文本
		// （{"reply": ""} 是端点明确说“没话说”，不是协议错误）。
		return "", fmt.Errorf("技能 %q 回了空回复", s.Name)
	}
	reply := strings.TrimSpace(string(raw))
	if reply == "" {
		return "", fmt.Errorf("技能 %q 回了空回复", s.Name)
	}
	return reply, nil
}
