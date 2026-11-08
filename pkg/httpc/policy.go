// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpc

// policy.go —— **可替换的命名退避策略**（设计 docs/design-http-client-policy.md §2）。
//
// 为什么需要：迁移前各调用点内联 `pipe.Policy{...}` 字面量 ⇒ 同类调用的退避不一致，
// 调整策略要逐点改、也无法审计"到底有多少种退避"。
//
// 用法：`httpc.New(httpc.Options{Retry: httpc.PolicyFor(httpc.PolicyFast), ...})`；
// 装配层可用 `RegisterPolicy` 覆盖（例如把全部外部 API 从 standard 换成 fast）。

import (
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

// PolicyName 是命名策略键。
type PolicyName string

const (
	// PolicyNone 不重试（长传输/非幂等场景；调用点仍可用 DoOptions.Idempotent 单独放开）。
	PolicyNone PolicyName = "none"
	// PolicyFast 外部 API/LLM 类：200ms×2ⁿ 封顶 2s、3 次尝试、带抖动。
	PolicyFast PolicyName = "fast"
	// PolicyStandard 默认：1s×2ⁿ 封顶 8s、4 次尝试、带抖动。
	PolicyStandard PolicyName = "standard"
	// PolicySlow 云盘/跨网长任务：5s×2ⁿ 封顶 60s、5 次尝试、带抖动。
	PolicySlow PolicyName = "slow"
)

// builtinPolicies 是预置策略（可被 RegisterPolicy 覆盖 ⇒ "可替换"）。
func builtinPolicies() map[PolicyName]pipe.Policy {
	return map[PolicyName]pipe.Policy{
		PolicyNone:     {Attempts: 1},
		PolicyFast:     {Attempts: 3, Base: 200 * time.Millisecond, Cap: 2 * time.Second, Factor: 2, Jitter: true},
		PolicyStandard: {Attempts: 4, Base: time.Second, Cap: 8 * time.Second, Factor: 2, Jitter: true},
		PolicySlow:     {Attempts: 5, Base: 5 * time.Second, Cap: 60 * time.Second, Factor: 2, Jitter: true},
	}
}

var (
	policyMu sync.RWMutex
	policies = builtinPolicies()
)

// PolicyFor 按名取策略；未注册/空名 ⇒ **PolicyStandard**（让"忘了声明"也有安全默认）。
func PolicyFor(name PolicyName) pipe.Policy {
	policyMu.RLock()
	p, ok := policies[name]
	policyMu.RUnlock()
	if !ok {
		policyMu.RLock()
		p = policies[PolicyStandard]
		policyMu.RUnlock()
	}
	return p
}

// RegisterPolicy 注册/覆盖命名策略（进程级；装配层用于统一调整退避口径）。
// p.Attempts<=0 视为 1（不重试）。
func RegisterPolicy(name PolicyName, p pipe.Policy) {
	if p.Attempts <= 0 {
		p.Attempts = 1
	}
	policyMu.Lock()
	policies[name] = p
	policyMu.Unlock()
}

// ResetPoliciesForTest 恢复预置策略（仅测试使用：避免 RegisterPolicy 污染其它用例）。
func ResetPoliciesForTest() {
	policyMu.Lock()
	policies = builtinPolicies()
	policyMu.Unlock()
}
