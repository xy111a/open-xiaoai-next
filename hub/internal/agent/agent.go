// Package agent 是 OpenAI 兼容的 Agent/LLM 客户端。
// Hermes / OpenClaw / Ollama / DeepSeek 都走 /v1/chat/completions，
// 换大脑只改 config.yaml，不动音箱。
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest 是 /v1/chat/completions 的请求体。
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// chatResponse 是非流式响应。
type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// streamChunk 是 SSE 流里的一块。
type streamChunk struct {
	Choices []struct {
		Delta Message `json:"delta"`
	} `json:"choices"`
}

// maxUpstreamErrorBody 是拼进错误的上游响应体上限（P1-6）：
// 旧代码把上游响应全文拼进错误（日志里可能含密钥/长文本），现只留摘要。
const maxUpstreamErrorBody = 512

// maxUpstreamBody 是上游成功响应体的读取上限（P2-7，1MB；
// 正常 chat 响应远小于此值，超限即报错，不读无界数据）。
const maxUpstreamBody = 1 << 20

// maxSSELineBytes 是 ChatStream 里单条 SSE 行的上限（P2-46）：
// 旧代码用 bufio.Scanner 1MB 上限，超长行直接丢弃整段已生成的回答。
// 现用 ReadLine 分段读，上限放宽到 4MB；超限时返回已拼接的部分内容 + 错误，
// 不再把已生成的回答整个丢掉。
const maxSSELineBytes = 4 << 20

// truncateErrorBody 截断上游错误摘要。
func truncateErrorBody(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…（已截断）"
	}
	return s
}

// DefaultSystemPrompt 是没配 system_prompt 时的内置提示词
// （与旧代码 Router 里写死的同一句，行为不变）。
const DefaultSystemPrompt = "你是家里的语音管家，回答简短口语化，一两句话。"

type Client struct {
	BaseURL string
	Model   string
	APIKey  string
	// SystemPrompt 是该 Agent 的 system 提示词（config agents[].system_prompt）；
	// 为空时 Prompt() 返回 DefaultSystemPrompt。
	SystemPrompt string
	HTTP         *http.Client
}

// New 新建 Client（system_prompt 为空，走默认提示词）。
func New(baseURL, model, apiKey string) *Client {
	return NewFull(baseURL, model, apiKey, "")
}

// NewFull 新建 Client 并指定 system_prompt（插件式 Agent 用）。
func NewFull(baseURL, model, apiKey, systemPrompt string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		Model:        model,
		APIKey:       apiKey,
		SystemPrompt: systemPrompt,
		HTTP:         &http.Client{Timeout: 120 * time.Second},
	}
}

// Prompt 返回生效的 system 提示词：配了用配的，没配用默认。
func (c *Client) Prompt() string {
	if c.SystemPrompt != "" {
		return c.SystemPrompt
	}
	return DefaultSystemPrompt
}

// Chat 一次问答（非流式），返回完整回答。
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+1))
		return "", fmt.Errorf("agent %s 返回 %d: %s", c.BaseURL, resp.StatusCode, truncateErrorBody(string(b), maxUpstreamErrorBody))
	}
	var out chatResponse
	// P2-7：成功响应体同样限长解码（上游异常返回超大 body 不再打满内存）。
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("agent error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("agent 返回空 choices")
	}
	return out.Choices[0].Message.Content, nil
}

// ChatStream 流式问答：每收到一块增量就调 onDelta（供 TTS 边生成边播）。
// 返回完整拼接后的回答。
func (c *Client) ChatStream(ctx context.Context, messages []Message, onDelta func(string)) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: messages, Stream: true})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBody+1))
		return "", fmt.Errorf("agent %s 返回 %d: %s", c.BaseURL, resp.StatusCode, truncateErrorBody(string(b), maxUpstreamErrorBody))
	}
	var full strings.Builder
	// P2-46：不用 bufio.Scanner（行缓冲上限 1MB，超长行会丢整段回答），
	// 改用 ReadLine 分段读：行可跨多次 ReadLine 拼接，上限 4MB；
	// 读错或行超限时返回已拼接的部分内容 + 错误（调用方可播已生成部分，
	// 而不是拿到空串）。
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := readSSELine(rd)
		if err != nil {
			if err == io.EOF {
				break
			}
			return full.String(), err
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta.Content
		if d != "" {
			full.WriteString(d)
			if onDelta != nil {
				onDelta(d)
			}
		}
	}
	return full.String(), nil
}

// readSSELine 用 ReadLine 分段读一整行（SSE 行可能很长）：
// 行长超过 maxSSELineBytes 即报错（已读片段丢弃，调用方按部分内容处理）。
func readSSELine(rd *bufio.Reader) (string, error) {
	var line []byte
	for {
		frag, isPrefix, err := rd.ReadLine()
		if err != nil {
			return "", err
		}
		if len(line)+len(frag) > maxSSELineBytes {
			return "", fmt.Errorf("agent SSE 行超过 %d 字节上限", maxSSELineBytes)
		}
		line = append(line, frag...)
		if !isPrefix {
			return string(line), nil
		}
	}
}
