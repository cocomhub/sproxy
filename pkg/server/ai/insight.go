// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// ai_insight.go 是 AI 文件洞察（roadmap 11.9-⑤）：
//
//   - Summarize：文件抽样文本（≤4KiB）→ LLM 摘要（≤2000 字）→ 缓存；
//   - Tag：同一文本 → LLM 逗号分隔标签 → 规范化（≤10 个，每 ≤20 字符）→ 缓存；
//   - 默认关闭（ai.insight.enabled=false）：AIInsight 不装配（gate=nil），端点 400；
//   - 失败语义：网关失败 → 明确错误（非静默降级）；不缓存失败。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/server/config"

	"github.com/cocomhub/sproxy/pkg/llmgate"
)

// MaxSummaryRunes / maxTags / maxTagRunes 是输出约束（防超长响应）。
const (
	MaxSummaryRunes = 2000
	maxTags         = 10
	maxTagRunes     = 20
)

// AIInsight 文件洞察：抽样文本 → LLM → 摘要/标签 → 缓存。
type AIInsight struct {
	gate   *llmgate.Client // nil = 未启用
	cache  *InsightCache
	quota  *AIQuota // nil = 配额未启用（恒通过零回归）
	logger *slog.Logger
}

// AI action 审计族（roadmap 11.9-⑦）：审计过滤用 "ai." 前缀匹配。
const (
	ActionAIEmbed     = "ai.embed"
	ActionAISummarize = "ai.summarize"
	ActionAITag       = "ai.tag"
)

// ErrNotTextFile 是抽样为空（非文本）的错误。
var ErrNotTextFile = fmt.Errorf("not a text file")

// InsightPrompts 构造 system/user 提示（纯函数可测；user 仅作数据，无拼接注入面）。
func InsightPrompts(filename, sample string) (system, user string) {
	system = "你是一名文件内容助手。根据给出的文件名与文本抽样，输出简洁的中文摘要或标签列表。"
	user = fmt.Sprintf("文件名：%s\n\n文本抽样：\n%s", filename, sample)
	return system, user
}

// NormalizeTags 规范化 LLM 标签输出（≤10 个、每 ≤20 字符、去空/去重）。
func NormalizeTags(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ';' || r == '；'
	})
	seen := make(map[string]bool)
	var tags []string
	for _, p := range parts {
		// 每个标签截断到 maxTagRunes 字符（文档契约；先截断再去重，避免仅超长部分不同的重复标签）。
		p = TruncateRunes(strings.TrimSpace(p), maxTagRunes)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		tags = append(tags, p)
		if len(tags) >= maxTags {
			break
		}
	}
	return tags
}

// TruncateRunes 截断到 maxLen 个字符（防超长响应）。
func TruncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// newAIInsightFromConfig 从 config.AIInsightConfig 装配洞察器：未启用 → nil（零回归）；
// enabled=true 但 APIKeyRef 解析为空 → Warn（可观测降级）+ nil。
// NewAIInsightFromConfig 从配置装配洞察器（导出供装配层；enabled=false → nil 零回归）。
func NewAIInsightFromConfig(cfg config.AIInsightConfig, cacheDir string, logger *slog.Logger) *AIInsight {
	if !cfg.Enabled {
		return nil
	}
	key := os.Getenv(cfg.APIKeyRef)
	if key == "" {
		logger.Warn("ai.insight.enabled=true 但 api_key_ref 解析为空，AI 洞察未启用（fail-closed）",
			"api_key_ref", cfg.APIKeyRef)
		return nil
	}
	gate := llmgate.New(llmgate.Config{
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   key,
		Model:    cfg.Model,
		Timeout:  cfg.Timeout,
	})
	return &AIInsight{
		gate:   gate,
		cache:  NewInsightCache(cacheDir, logger),
		logger: logger,
	}
}

// Enabled 报告洞察是否已装配（LLM 网关非 nil）。
func (a *AIInsight) Enabled() bool { return a != nil && a.gate != nil }

// Cache 返回洞察结果缓存。
func (a *AIInsight) Cache() *InsightCache { return a.cache }

// Quota 返回配额句柄（nil = 未启用）。
func (a *AIInsight) Quota() *AIQuota { return a.quota }

// Advise 调用底层 LLM（system/user 提示）；未装配返回错误（调用方按 fail-closed 处理）。
func (a *AIInsight) Advise(ctx context.Context, system, user string) (string, error) {
	if a == nil || a.gate == nil {
		return "", errors.New("ai insight disabled")
	}
	return a.gate.Advise(ctx, system, user)
}

// NewAIInsightWithGate 构造带指定 LLM 网关的洞察器（装配/测试用；nil = 未启用）。
func NewAIInsightWithGate(gate *llmgate.Client) *AIInsight {
	return &AIInsight{gate: gate, logger: slog.Default()}
}
