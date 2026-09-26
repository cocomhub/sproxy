// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package aiembed 是 embedding 客户端（roadmap 11.9-④ 向量索引）：OpenAI 兼容
// /v1/embeddings 与 Ollama /api/embed 的最小实现。标准库 + netutil，无外部依赖。
//
// 设计约束（同 llmgate）：
//   - 纯标准库 HTTP（netutil.IsolatedTransport 基座 + ResponseHeaderTimeout）；
//   - APIKey 只出现在 Authorization Bearer 头，不落日志；
//   - 响应体上限保护（io.LimitReader），防畸形响应撑爆内存；
//   - 失败一律返回 error（fail-closed），由调用方决定降级策略。
package aiembed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

const (
	// maxResponseBytes 是响应体读取上限（1 MiB），防畸形响应撑爆内存。
	maxResponseBytes = 1 << 20
	// defaultDim 是默认向量维数（openai text-embedding-3-small）。
	defaultDim = 1536
)

// 默认超时（cfg.Timeout <= 0 时回落）。
const defaultTimeout = 30 * time.Second

// Config 是 embedding 配置（构造期传入，只读）。
type Config struct {
	Provider string        // "openai"（默认）| "ollama"（本地）
	BaseURL  string        // openai: https://api.openai.com/v1；ollama: http://127.0.0.1:11434
	APIKey   string        // openai 需要；ollama 可空（空 key → New 仍构造，调用方决定启用）
	Model    string        // openai: text-embedding-3-small；ollama: nomic-embed-text
	Dim      int           // 向量维数（<=0 → 默认 1536）
	Timeout  time.Duration // 默认 30s
}

// Client 是 embedding 客户端。
type Client struct {
	cfg Config
	hc  *http.Client // netutil.IsolatedTransport 基座 + ResponseHeaderTimeout
}

// embedRequest 是 OpenAI 兼容请求体（Ollama 同形状，字段名兼容）。
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse 是 OpenAI 兼容响应体（data[i].embedding）。
type embedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// ollamaResponse 是 Ollama /api/embed 响应体（embeddings 数组）。
type ollamaResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// New 构造客户端（provider/baseURL/model 归一）。
func New(cfg Config) *Client {
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	switch cfg.Provider {
	case "ollama":
		if cfg.BaseURL == "" {
			cfg.BaseURL = "http://127.0.0.1:11434"
		}
		if cfg.Model == "" {
			cfg.Model = "nomic-embed-text"
		}
	default:
		cfg.Provider = "openai"
		if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.openai.com/v1"
		}
		if cfg.Model == "" {
			cfg.Model = "text-embedding-3-small"
		}
	}
	if cfg.Dim <= 0 {
		cfg.Dim = defaultDim
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = cfg.Timeout
	return &Client{cfg: cfg, hc: &http.Client{Transport: tr}}
}

// Embed 单条文本 → []float32（归一化，供余弦相似度）。
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("aiembed: 响应 %d 条向量，want 1", len(vecs))
	}
	return vecs[0], nil
}

// EmbedBatch 批量（≤32 条）→ [][]float32（每条归一化）。
//
// openai: POST {base}/embeddings {"input": [...]} → data[i].embedding
// ollama: POST {base}/api/embed {"input": [...]} → embeddings[i]
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("aiembed: input 为空")
	}
	if slices.Contains(texts, "") {
		return nil, fmt.Errorf("aiembed: 含空文本")
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	path := "/api/embed"
	if c.cfg.Provider == "openai" {
		path = "/embeddings"
	}
	body, _ := json.Marshal(embedRequest{Model: c.cfg.Model, Input: texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("aiembed: 构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aiembed: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 有限读错误体（防恶意响应），不含 APIKey。
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("aiembed: HTTP %d: %s", resp.StatusCode, string(msg))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("aiembed: 读响应: %w", err)
	}
	var raw [][]float64
	if c.cfg.Provider == "ollama" {
		var or ollamaResponse
		if err := json.Unmarshal(data, &or); err != nil {
			return nil, fmt.Errorf("aiembed: 解析 ollama 响应: %w", err)
		}
		raw = or.Embeddings
	} else {
		var er embedResponse
		if err := json.Unmarshal(data, &er); err != nil {
			return nil, fmt.Errorf("aiembed: 解析 openai 响应: %w", err)
		}
		for _, d := range er.Data {
			raw = append(raw, d.Embedding)
		}
	}
	if len(raw) != len(texts) {
		return nil, fmt.Errorf("aiembed: 响应 %d 条向量，want %d", len(raw), len(texts))
	}
	out := make([][]float32, len(raw))
	for i, v := range raw {
		vec := make([]float32, len(v))
		for j, f := range v {
			vec[j] = float32(f)
		}
		out[i] = normalize(vec)
	}
	return out, nil
}

// normalize 归一化向量（供余弦相似度；零向量返回原样避免除零）。
func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / norm)
	}
	return out
}
