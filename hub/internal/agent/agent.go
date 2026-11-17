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

// Client 是一个 Agent 端点。
type Client struct {
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

// New 新建 Client。
func New(baseURL, model, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Model:   model,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
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
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent %s 返回 %d: %s", c.BaseURL, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
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
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agent %s 返回 %d: %s", c.BaseURL, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var full strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
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
	if err := sc.Err(); err != nil {
		return "", err
	}
	return full.String(), nil
}
