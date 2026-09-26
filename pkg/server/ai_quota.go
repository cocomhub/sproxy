// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_quota.go 是 AI 调用配额（roadmap 11.9-⑦）：per-owner 日调用次数/token 估算
// 上限的内存计数器。前置门（CheckAndCharge 在 LLM 调用前扣减）——超限 429 不调网关。
//
// 零回归：Enabled=false → NewAIQuota 返回 nil（调用点判空恒通过）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ErrQuotaExceeded 是 AI 配额超限的错误（调用方 → 429）。
var ErrQuotaExceeded = errors.New("ai quota exceeded")

// AILimit 是单 owner 的 AI 配额限制（0 = 不限）。
type AILimit struct {
	DailyCalls  int `yaml:"daily_calls" mapstructure:"daily_calls"`
	DailyTokens int `yaml:"daily_tokens" mapstructure:"daily_tokens"`
}

// AIUsage 是单 owner 的当前用量。
type AIUsage struct {
	Day    string  `json:"day"` // YYYY-MM-DD
	Calls  int     `json:"calls"`
	Tokens int     `json:"tokens"`
	Limit  AILimit `json:"limit"`
}

// AIQuotaConfig 是 ai.quota 配置段（默认关零回归）。
type AIQuotaConfig struct {
	Enabled   bool               `yaml:"enabled" mapstructure:"enabled"`
	Default   AILimit            `yaml:"default" mapstructure:"default"`
	Overrides map[string]AILimit `yaml:"overrides" mapstructure:"overrides"`
}

// AIQuota 是 per-owner AI 调用配额（内存计数器，进程内有效；重启清零文档注明）。
type AIQuota struct {
	mu     sync.Mutex
	cfg    AIQuotaConfig
	usage  map[string]AIUsage
	logger *slog.Logger
	now    func() time.Time // 时钟注入（测试跨天）
}

// NewAIQuota 构造配额器。Enabled=false → 返回 nil（调用点判空 = 恒通过，零回归）。
func NewAIQuota(cfg AIQuotaConfig, logger *slog.Logger) *AIQuota {
	if !cfg.Enabled {
		return nil
	}
	return &AIQuota{cfg: cfg, usage: map[string]AIUsage{}, logger: logger, now: time.Now}
}

// setDay 是时钟注入（测试跨天重置用）。
func (q *AIQuota) setDay(day string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 把 usage 的 day 标记为已过期（下次 Check 重置）
	for owner, u := range q.usage {
		u.Day = day
		q.usage[owner] = u
	}
}

// today 返回当前日期（YYYY-MM-DD，按本地日）。
func (q *AIQuota) today() string {
	return q.now().Format("2006-01-02")
}

// limitFor 返回 owner 的配额限制（overrides 优先，缺省默认）。
func (q *AIQuota) limitFor(owner string) AILimit {
	if l, ok := q.cfg.Overrides[owner]; ok {
		return l
	}
	return q.cfg.Default
}

// CheckAndCharge 检查配额并扣减：超限返回 ErrQuotaExceeded（不扣减）。
// 跨天惰性重置（Day 与当前日期不符 → 清零重计）。
func (q *AIQuota) CheckAndCharge(owner string, estTokens int) error {
	if q == nil {
		return nil // 未启用（nil 配额）恒通过，零回归
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	today := q.today()
	u := q.usage[owner]
	if u.Day != today {
		u = AIUsage{Day: today}
	}
	limit := q.limitFor(owner)
	if limit.DailyCalls > 0 && u.Calls+1 > limit.DailyCalls {
		return fmt.Errorf("%w: owner=%s calls=%d/%d", ErrQuotaExceeded, owner, u.Calls, limit.DailyCalls)
	}
	if limit.DailyTokens > 0 && u.Tokens+estTokens > limit.DailyTokens {
		return fmt.Errorf("%w: owner=%s tokens=%d/%d", ErrQuotaExceeded, owner, u.Tokens+estTokens, limit.DailyTokens)
	}
	u.Calls++
	u.Tokens += estTokens
	q.usage[owner] = u
	return nil
}

// Usage 返回 owner 当前用量（GET /api/ai/quota 用；只读不扣）。
func (q *AIQuota) Usage(owner string) AIUsage {
	if q == nil {
		return AIUsage{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	u := q.usage[owner]
	if u.Day != q.today() {
		u = AIUsage{Day: q.today()}
	}
	u.Limit = q.limitFor(owner)
	return u
}

// SetQuota 注入配额器到 AIInsight（装配层调用；nil = 未启用零回归）。
func (a *AIInsight) SetQuota(q *AIQuota) { a.quota = q }

// handleAIQuota 是 GET /api/ai/quota 处理器（受认证 owner 自见；管理员可 ?owner= 查他人）。
func (h *Handlers) handleAIQuota(w http.ResponseWriter, r *http.Request) {
	ai := h.aiInsight
	if ai == nil || ai.quota == nil {
		writeAIError(w, http.StatusBadRequest, "AI 配额未启用")
		return
	}
	owner := ownerFromRequest(r)
	if o := r.URL.Query().Get("owner"); o != "" && owner != "" {
		// 管理员越权查询：仅当请求已认证且非 anonymous 才允许（简化：同 owner 隔离语义）
		if o != owner && owner != "admin" {
			writeAIError(w, http.StatusForbidden, "无权查看他人配额")
			return
		}
		owner = o
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"owner": owner,
		"usage": ai.quota.Usage(owner),
	})
}
