// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// ai_advisor.go 是告警建议器（roadmap 11.9-⑥ 智能运维 LLM）：
//
//   - ai.AIAdvisor：告警上下文 → LLM 建议文本；nil 网关 = 恒空（零回归）。
//   - Engine.dispatch 内同步追加「【AI 建议】」段：失败返回 "" → 追加为空
//     即固定模板原样（fail-closed：AI 是增强层，绝不允许它吞掉告警）。
//   - prompt 构造是纯函数（可测）：system 固定、user 仅作数据（无拼接注入面）。

import (
	"context"
	"log/slog"
	"os"

	"github.com/cocomhub/sproxy/pkg/server/ai"
	"github.com/cocomhub/sproxy/pkg/server/config"
)

func (e *Engine) SetAdvisor(a adviser) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.advisor = a
}

// NewAIAdvisorFromConfig 从 config.AIAdvisorConfig 装配建议器：未启用 → nil（零回归）；
// enabled=true 但 APIKeyRef 解析为空 → Warn（可观测降级）+ 返回 nil（gate=nil 语义）。
func NewAIAdvisorFromConfig(cfg config.AIAdvisorConfig, logger *slog.Logger) *ai.AIAdvisor {
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
	return ai.NewAIAdvisor(cfg, logger)
}

// adviser 是 advisor 的窄接口（测试桩注入用）。
type adviser interface {
	Advise(ctx context.Context, alertKey, title, text string) string
}

// splitAdvice 无锁读 advisor（dispatch 在 e.mu 之外调用，竞态安全要求原子指针读）。
func (e *Engine) advisorSnapshot() adviser {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.advisor
}
