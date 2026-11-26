// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// block_policy.go 是**分块规则引擎在 secretdata 写路径的接入点**
// （设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §5）。
//
// 为什么单独一个文件：规则选型是「读快照 + 剥离键前缀 + 命中判定」三段纯逻辑，与既有的
// blocklet 自动选型（helpers.go，按扩展名/显式配置）是**两个正交维度**——规则只覆盖块级
// （Mode/Max），blocklet 仍由既有逻辑决定。混在一个函数里会让两者互相污染。

// SetPolicy 原子替换**运行期**规则快照（nil = 清空 ⇒ 回落静态基线/卷默认策略）。
//
// 编译（含校验）由调用方经 policy.Compile 完成——本方法不做校验、不返回错误，因此不存在
// 「落盘成功但内存注入失败」的半生效窗口（设计 §10）。写路径读快照，无锁竞争。
func (s *SecretdataFS) SetPolicy(c *policy.Compiled) {
	s.policy.Store(c)
}

// policySnapshot 返回当前**运行期**规则快照（nil = 无运行期覆盖）。
func (s *SecretdataFS) policySnapshot() *policy.Compiled {
	return s.policy.Load()
}

// PolicyState 返回 (运行期快照, 静态基线)，供设置回显区分「没配」与「回落到基线」。
// 两者都可能为 nil；Compiled 构造后不可变 ⇒ 无需拷贝。
func (s *SecretdataFS) PolicyState() (runtime, baseline *policy.Compiled) {
	return s.policy.Load(), s.basePolicy
}

// applyRuleToPolicy 把命中的规则应用到块级策略副本上，返回 (新策略, 是否命中)。
//
// single：Max 设为**实际文件大小**——块规划器只产 1 块，Max 的唯一作用是让
// stagingReserveInterleaved（按 min(size, policy.Max) 预留本地暂存）与 physfootprint
// 的口径正确；Min 设为 1（与既有「最小块」语义无关，仅保证校验通过）。
// random：显式回落（保留卷级 Min/Max），用于给某目录压制更宽的 single 规则。
func applyRuleToPolicy(base shardseal.BlockPolicy, r policy.Rule, size int64) shardseal.BlockPolicy {
	switch r.Mode {
	case shardseal.BlockModeSingle:
		base.Mode = shardseal.BlockModeSingle
		base.Min = 1
		base.Max = max(size, 1)
	case shardseal.BlockModeRandom:
		base.Mode = shardseal.BlockModeRandom
	}
	return base
}

// effectivePolicy 返回**当前生效**的规则快照：运行期规则优先，未设置则回落静态基线
// （`Options.Policy` = `extra.block_rules`）。
//
// 为什么必须是两层而不是一个指针：运行期「清空规则」= 删除运行期覆盖，语义上是**回落基线**
// （设计 §7 的优先级链 StateStore > extra.block_rules > 卷默认）。若清空时写入一个空快照，
// 基线会被永久压制（重启载入空记录后同样压制），用户再也无法通过 UI 回到基线配置。
func (s *SecretdataFS) effectivePolicy() *policy.Compiled {
	if c := s.policy.Load(); c != nil {
		return c
	}
	return s.basePolicy
}

// ruleForWrite 按 rel+size 解析规则命中的块级策略（未命中返回原策略与 false）。
//
// 非用户面键（形状不符）不参与匹配——规则只描述用户可见路径的语义。
func (s *SecretdataFS) ruleForWrite(rel string, size int64) (shardseal.BlockPolicy, bool) {
	base := s.opts.Block
	c := s.effectivePolicy()
	if c == nil {
		return base, false
	}
	visible, ok := volume.UserVisibleRelOK(rel)
	if !ok {
		return base, false
	}
	r, hit := c.Resolve(visible, size)
	if !hit {
		return base, false
	}
	return applyRuleToPolicy(base, r, size), true
}

// 编译期断言：SecretdataFS 必须继续满足既有的写面能力（改 Options 时防手滑破坏）。
var _ syncpkg.FS = (*SecretdataFS)(nil)

// policyPtr 是 SecretdataFS 内的规则快照字段类型别名（便于 atomic.Pointer 声明）。
type policyPtr = atomic.Pointer[policy.Compiled]
