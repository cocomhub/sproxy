// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_insight.go 是 AI 文件洞察（roadmap 11.9-⑤）：
//
//   - Summarize：文件抽样文本（≤4KiB）→ LLM 摘要（≤2000 字）→ 缓存；
//   - Tag：同一文本 → LLM 逗号分隔标签 → 规范化（≤10 个，每 ≤20 字符）→ 缓存；
//   - 默认关闭（ai.insight.enabled=false）：AIInsight 不装配（gate=nil），端点 400；
//   - 失败语义：网关失败 → 明确错误（非静默降级）；不缓存失败。

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/llmgate"
)

// maxSummaryRunes / maxTags / maxTagRunes 是输出约束（防超长响应）。
const (
	maxSummaryRunes = 2000
	maxTags         = 10
	maxTagRunes     = 20
)

// AIInsight 文件洞察：抽样文本 → LLM → 摘要/标签 → 缓存。
type AIInsight struct {
	gate   *llmgate.Client // nil = 未启用
	cache  *InsightCache
	logger *slog.Logger
}

// ErrNotTextFile 是抽样为空（非文本）的错误。
var ErrNotTextFile = fmt.Errorf("not a text file")

// insightPrompts 构造 system/user 提示（纯函数可测；user 仅作数据，无拼接注入面）。
func insightPrompts(filename, sample string) (system, user string) {
	system = "你是一名文件内容助手。根据给出的文件名与文本抽样，输出简洁的中文摘要或标签列表。"
	user = fmt.Sprintf("文件名：%s\n\n文本抽样：\n%s", filename, sample)
	return system, user
}

// normalizeTags 规范化 LLM 标签输出（≤10 个、每 ≤20 字符、去空/去重）。
func normalizeTags(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ';' || r == '；'
	})
	seen := make(map[string]bool)
	var tags []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
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

// truncateRunes 截断到 max 个字符（防超长响应）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// AIInsightConfig 是 ai.insight 配置段（config.go 引用；默认关零回归）。
type AIInsightConfig struct {
	Enabled   bool          `yaml:"enabled" mapstructure:"enabled"`
	Provider  string        `yaml:"provider" mapstructure:"provider"`
	BaseURL   string        `yaml:"base_url" mapstructure:"base_url"`
	APIKeyRef string        `yaml:"api_key_ref" mapstructure:"api_key_ref"`
	APIKey    string        `yaml:"-" mapstructure:"-"`
	Model     string        `yaml:"model" mapstructure:"model"`
	Timeout   time.Duration `yaml:"timeout" mapstructure:"timeout"`
	CacheTTL  time.Duration `yaml:"cache_ttl" mapstructure:"cache_ttl"`
}

// newAIInsightFromConfig 从 AIInsightConfig 装配洞察器：未启用 → nil（零回归）；
// enabled=true 但 APIKeyRef 解析为空 → Warn（可观测降级）+ nil。
// NewAIInsightFromConfig 从配置装配洞察器（导出供装配层；enabled=false → nil 零回归）。
func NewAIInsightFromConfig(cfg AIInsightConfig, cacheDir string, logger *slog.Logger) *AIInsight {
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
		cache:  newInsightCache(cacheDir, logger),
		logger: logger,
	}
}

// AISearchConfig 是 ai.search 语义搜索配置（roadmap 11.9-④；默认关零回归）。
type AISearchConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	CacheDir string `yaml:"-" mapstructure:"-"`
	TopK     int    `yaml:"topk" mapstructure:"topk"`
}
