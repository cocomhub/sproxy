// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_advisor.go 是告警建议器（roadmap 11.9-⑥ 智能运维 LLM）：
//
//   - AIAdvisor：告警上下文 → LLM 建议文本；nil 网关 = 恒空（零回归）。
//   - AlertEngine.dispatch 内同步追加「【AI 建议】」段：失败返回 "" → 追加为空
//     即固定模板原样（fail-closed：AI 是增强层，绝不允许它吞掉告警）。
//   - prompt 构造是纯函数（可测）：system 固定、user 仅作数据（无拼接注入面）。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/llmgate"
)

// aiAdviseSuffix 是追加到通知正文的建议段标题（非空建议才拼接）。
const aiAdviseSuffix = "\n\n【AI 建议】\n"

// maxAdviseRunes 是建议文本截断上限（1000 字，防超长响应撑爆通知载荷）。
const maxAdviseRunes = 1000

// AIAdvisor 告警建议器：告警上下文 → LLM 建议文本；nil 网关 = 恒空（零回归）。
type AIAdvisor struct {
	gate   *llmgate.Client // nil = 未启用（enabled=false 或无 key）
	logger *slog.Logger
}

// AIAdvisorConfig 是 notify.ai_advisor 配置段（config.go 引用）。
type AIAdvisorConfig struct {
	Enabled   bool          `yaml:"enabled" mapstructure:"enabled"`         // 默认 false 零回归
	Provider  string        `yaml:"provider" mapstructure:"provider"`       // openai | anthropic（默认 openai）
	BaseURL   string        `yaml:"base_url" mapstructure:"base_url"`       // 空 = provider 默认（测试/自建网关可配）
	APIKeyRef string        `yaml:"api_key_ref" mapstructure:"api_key_ref"` // 环境变量名，如 SPROXY_OPENAI_API_KEY
	APIKey    string        `yaml:"-" mapstructure:"-"`                     // 装配层解析后的 key（不落 yaml/日志）
	Model     string        `yaml:"model" mapstructure:"model"`             // 空 = provider 默认
	Timeout   time.Duration `yaml:"timeout" mapstructure:"timeout"`         // 默认 10s
}

// NewAIAdvisor 构造告警建议器。enabled=false 或 api_key 解析为空 → gate=nil（恒空，零回归）。
func NewAIAdvisor(cfg AIAdvisorConfig, logger *slog.Logger) *AIAdvisor {
	if logger == nil {
		logger = slog.Default()
	}
	a := &AIAdvisor{logger: logger}
	if !cfg.Enabled {
		return a
	}
	if cfg.APIKey == "" {
		// 装配层已 Warn（可观测降级）；gate=nil = 恒空，fail-closed 回退模板。
		return a
	}
	a.gate = llmgate.New(llmgate.Config{
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
		Model:    cfg.Model,
		Timeout:  cfg.Timeout,
	})
	return a
}

// promptSystem 是固定的运维助手 system 提示（无拼接注入面）。
const promptSystem = "你是一名运维专家。请根据给出的告警信息，分析可能的根因并给出处置建议。" +
	"要求：简洁（不超过 300 字）、分点列出、只输出建议正文，不要重复告警原文。"

// buildUserPrompt 构造 user 消息（纯函数，可测）：告警正文仅作数据。
func buildUserPrompt(alertKey, title, text string) string {
	return fmt.Sprintf("告警来源: %s\n标题: %s\n正文:\n%s", alertKey, title, text)
}

// Advise 生成建议文本：成功 → 返回建议（截断 ≤1000 字）；失败/未启用 → ""。
// 网关失败记 Warn（含 alertKey），调用方（dispatch）继续发固定模板——fail-closed。
func (a *AIAdvisor) Advise(ctx context.Context, alertKey, title, text string) string {
	if a.gate == nil {
		return ""
	}
	advice, err := a.gate.Advise(ctx, promptSystem, buildUserPrompt(alertKey, title, text))
	if err != nil {
		a.logger.Warn("AI 建议生成失败（回退固定模板）", "alert_key", alertKey, "error", err.Error())
		return ""
	}
	if advice == "" {
		return ""
	}
	runes := []rune(advice)
	if len(runes) > maxAdviseRunes {
		advice = string(runes[:maxAdviseRunes])
	}
	return advice
}

// SetAdvisor 注入告警建议器（nil = 现状，零回归）。
func (e *AlertEngine) SetAdvisor(a advisory) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.advisor = a
}

// newAIAdvisorFromConfig 从 AIAdvisorConfig 装配建议器：未启用 → nil（零回归）；
// enabled=true 但 APIKeyRef 解析为空 → Warn（可观测降级）+ 返回 nil（gate=nil 语义）。
func newAIAdvisorFromConfig(cfg AIAdvisorConfig, logger *slog.Logger) *AIAdvisor {
	if !cfg.Enabled {
		return nil
	}
	key := os.Getenv(cfg.APIKeyRef)
	if key == "" {
		logger.Warn("notify.ai_advisor.enabled=true 但 api_key_ref 解析为空，AI 建议降级为固定模板（fail-closed）",
			"api_key_ref", cfg.APIKeyRef)
		return nil
	}
	cfg.APIKey = key
	return NewAIAdvisor(cfg, logger)
}

// advisory 是 advisor 的窄接口（测试桩注入用）。
type advisory interface {
	Advise(ctx context.Context, alertKey, title, text string) string
}

// splitAdvice 无锁读 advisor（dispatch 在 e.mu 之外调用，竞态安全要求原子指针读）。
func (e *AlertEngine) advisorSnapshot() advisory {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.advisor
}

// appendAdvice 把建议追加到正文（非空才拼接；nil advisor → 原样）。
func appendAdvice(text, advice string) string {
	if advice == "" {
		return text
	}
	return text + aiAdviseSuffix + advice
}
