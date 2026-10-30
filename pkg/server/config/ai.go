// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ai.go：AI 配置段的数据模型（配额/洞察/语义搜索/隐私/事件流水线）。
package config

import "time"

// AILimit 是单 owner 的 AI 配额限制（0 = 不限）。
type AILimit struct {
	DailyCalls  int `yaml:"daily_calls" mapstructure:"daily_calls"`
	DailyTokens int `yaml:"daily_tokens" mapstructure:"daily_tokens"`
}

// AIQuotaConfig 是 ai.quota 配置段（默认关零回归）。
type AIQuotaConfig struct {
	Enabled   bool               `yaml:"enabled" mapstructure:"enabled"`
	Default   AILimit            `yaml:"default" mapstructure:"default"`
	Overrides map[string]AILimit `yaml:"overrides" mapstructure:"overrides"`
}

// AIAdvisorConfig 是 notify.ai_advisor 配置段（Config 引用）。
type AIAdvisorConfig struct {
	Enabled   bool          `yaml:"enabled" mapstructure:"enabled"`         // 默认 false 零回归
	Provider  string        `yaml:"provider" mapstructure:"provider"`       // openai | anthropic（默认 openai）
	BaseURL   string        `yaml:"base_url" mapstructure:"base_url"`       // 空 = provider 默认（测试/自建网关可配）
	APIKeyRef string        `yaml:"api_key_ref" mapstructure:"api_key_ref"` // 环境变量名，如 SPROXY_OPENAI_API_KEY
	APIKey    string        `yaml:"-" mapstructure:"-"`                     // 装配层解析后的 key（不落 yaml/日志）
	Model     string        `yaml:"model" mapstructure:"model"`             // 空 = provider 默认
	Timeout   time.Duration `yaml:"timeout" mapstructure:"timeout"`         // 默认 10s
}

// AIInsightConfig 是 ai.insight 配置段（Config 引用；默认关零回归）。
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

// AISearchConfig 是 ai.search 语义搜索配置（roadmap 11.9-④；默认关零回归）。
type AISearchConfig struct {
	Enabled  bool   `yaml:"enabled" mapstructure:"enabled"`
	CacheDir string `yaml:"-" mapstructure:"-"`
	TopK     int    `yaml:"topk" mapstructure:"topk"`
}

// AIPrivacyConfig 是 ai.privacy 配置段（roadmap 11.9-⑧；默认关零回归）。
type AIPrivacyConfig struct {
	Enabled bool `yaml:"enabled" mapstructure:"enabled"`
}

// aiEventsConfig 是 ai.events 配置段（Config 引用；默认关零回归）。
type aiEventsConfig struct {
	Enabled     bool          `yaml:"enabled" mapstructure:"enabled"`
	QueueSize   int           `yaml:"queue_size" mapstructure:"queue_size"`     // 默认 256
	DedupWindow time.Duration `yaml:"dedup_window" mapstructure:"dedup_window"` // 默认 5m
}

// AIEventsConfig 是 ai.events 配置段（导出供装配层）。
type AIEventsConfig = aiEventsConfig
