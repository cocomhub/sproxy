// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package llmgate 是 LLM 网关客户端（roadmap 11.9-⑥ 智能运维 LLM）：OpenAI
// 兼容 /chat/completions 的最小实现（Anthropic 兼容端点同形状），供告警根因建议
// （pkg/server AIAdvisor）与未来 AI 文件洞察共用。
//
// 设计约束：
//   - 纯标准库 HTTP（netutil.IsolatedTransport 基座 + ResponseHeaderTimeout）；
//   - APIKey 只出现在 Authorization Bearer 头，不落日志；
//   - 响应体上限保护（io.LimitReader），防恶意/畸形响应撑爆内存；
//   - 失败一律返回 error（fail-closed），由调用方决定降级策略。
package llmgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// defaultBaseURL / defaultModel 是 provider 缺省值（模型名/URL 可配）。
const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "gpt-4o-mini"
	// maxResponseBytes 是响应体读取上限（64 KiB），防超大响应撑爆内存。
	maxResponseBytes = 64 << 10
)

// 默认超时（cfg.Timeout <= 0 时回落）。
const defaultTimeout = 10 * time.Second

// Config 是 LLM 网关配置（构造期传入，只读）。
type Config struct {
	Provider string        // "openai"（默认）| "anthropic"；模型名/URL 归一在 New 内
	BaseURL  string        // 空 = provider 默认（openai: https://api.openai.com/v1）
	APIKey   string        // 空 key → New 仍构造（调用方决定启用与否），HTTP 401 由网关返回
	Model    string        // 空 = provider 默认（gpt-4o-mini）
	Timeout  time.Duration // 默认 10s
}

// Client 是 LLM 网关客户端。
type Client struct {
	cfg Config
	hc  *http.Client // netutil.IsolatedTransport 基座 + ResponseHeaderTimeout
}

// chatMessage 是 chat completions 消息（仅字符串 content，不做多模态/工具）。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest 是 OpenAI 兼容请求体。
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

// chatResponse 是 OpenAI 兼容响应体（仅消费 choices[0].message.content）。
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// New 构造客户端（provider/baseURL/model 归一）。
func New(cfg Config) *Client {
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	switch cfg.Provider {
	case "anthropic":
		// Anthropic 兼容端点走同一 OpenAI 形状（片4 再做原生 messages API 专路）。
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.anthropic.com/v1"
		}
	default:
		cfg.Provider = "openai"
		if cfg.BaseURL == "" {
			cfg.BaseURL = defaultBaseURL
		}
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = defaultTimeout
	return &Client{cfg: cfg, hc: &http.Client{Transport: tr}}
}

// Advise 生成建议文本：构建 system+user 消息 → POST {base}/chat/completions
// → 取 choices[0].message.content（trim）。
//
// 非 2xx / 畸形 JSON / 超大响应 / 超时 → error（fail-closed，调用方降级）。
func (c *Client) Advise(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	body, err := json.Marshal(chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", fmt.Errorf("llmgate: 构造请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llmgate: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("llmgate: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llmgate: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("llmgate: 读取响应失败: %w", err)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("llmgate: 响应超限（>%d bytes）", maxResponseBytes)
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("llmgate: 解析响应失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("llmgate: 响应无 choices")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}
