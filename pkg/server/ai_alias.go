// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ai_alias.go 是**迁移期兼容层**：AI 域实现已下沉 pkg/server/ai，装配层用别名/转发
// 保持 handler 与存量测试零改动。移除条件：AI 的 HTTP 面整体搬入 ai 切片后删除。
package server

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/llmgate"
	"github.com/cocomhub/sproxy/pkg/server/ai"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
)

type (
	// AIInsight 是文件洞察（抽样 → LLM → 摘要/标签 → 缓存）。
	AIInsight = ai.AIInsight
	// AIAdvisor 是告警建议器。
	AIAdvisor = ai.AIAdvisor
	// AIPrivacy 是 AI 派生数据隐私。
	AIPrivacy = ai.AIPrivacy
	// AIQuota 是 per-owner AI 调用配额。
	AIQuota = ai.AIQuota
	// AIUsage 是 AI 用量快照。
	AIUsage = ai.AIUsage
	// AIArtifactInfo 是 AI 产物元信息。
	AIArtifactInfo = ai.AIArtifactInfo
	// AIEventConsumer 订阅文件事件并路由到 AI 流水线。
	AIEventConsumer = ai.AIEventConsumer
	// InsightCache 是洞察结果缓存。
	InsightCache = ai.InsightCache
)

const (
	// AI action 审计族。
	ActionAIEmbed     = ai.ActionAIEmbed
	ActionAISummarize = ai.ActionAISummarize
	ActionAITag       = ai.ActionAITag
	// MaxSummaryRunes 是摘要截断上限。
	MaxSummaryRunes = ai.MaxSummaryRunes
	maxSummaryRunes = ai.MaxSummaryRunes
)

// ErrNotTextFile 是抽样为空（非文本）的错误。
var ErrNotTextFile = ai.ErrNotTextFile

// ErrQuotaExceeded 是 AI 配额超限错误。
var ErrQuotaExceeded = ai.ErrQuotaExceeded

// 函数转发（迁移期）。
func NewAIInsightFromConfig(cfg config.AIInsightConfig, cacheDir string, logger *slog.Logger) *ai.AIInsight {
	return ai.NewAIInsightFromConfig(cfg, cacheDir, logger)
}

// NewAIInsightWithGate 构造带指定网关的洞察器（测试/装配用）。
func NewAIInsightWithGate(gate *llmgate.Client) *ai.AIInsight {
	return ai.NewAIInsightWithGate(gate)
}

// NewAIQuota 构造 per-owner 配额。
func NewAIQuota(cfg config.AIQuotaConfig, logger *slog.Logger) *ai.AIQuota {
	return ai.NewAIQuota(cfg, logger)
}

// NewAIPrivacy 构造 AI 派生数据隐私。
func NewAIPrivacy(enabled bool, root *storage.Root, logger *slog.Logger) *ai.AIPrivacy {
	return ai.NewAIPrivacy(enabled, root, logger)
}

// NewAIAdvisor 构造告警建议器。
func NewAIAdvisor(cfg config.AIAdvisorConfig, logger *slog.Logger) *ai.AIAdvisor {
	return ai.NewAIAdvisor(cfg, logger)
}

// newAIEventConsumer 用 EventBus 适配 ai.EventSource。
func NewAIEventConsumer(bus *EventBus, enqueue func(owner, rel, op string), cfg config.AIEventsConfig, logger *slog.Logger) *ai.AIEventConsumer {
	return ai.NewAIEventConsumer(aiEventSource{bus: bus}, enqueue, cfg, logger)
}

// aiEventSource 把 EventBus 快照适配为 ai.FileEvent。
type aiEventSource struct{ bus *EventBus }

func (s aiEventSource) DrainFileEvents() []ai.FileEvent {
	evs := s.bus.DrainFileEvents()
	out := make([]ai.FileEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, ai.FileEvent{Action: e.Action, Owner: e.Owner, Rel: e.Rel, Size: e.Size, Cursor: e.Cursor})
	}
	return out
}

// 内部辅助转发（handler 复用，避免与 ai 包漂移）。
func insightPrompts(filename, sample string) (string, string) {
	return ai.InsightPrompts(filename, sample)
}
func normalizeTags(raw string) []string       { return ai.NormalizeTags(raw) }
func truncateRunes(s string, n int) string    { return ai.TruncateRunes(s, n) }
func appendAdvice(text, advice string) string { return ai.AppendAdvice(text, advice) }
