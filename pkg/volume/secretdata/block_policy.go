// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"strings"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// block_policy.go 是**分块规则引擎在 secretdata 写路径的接入点**
// （设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §5）。
//
// 为什么单独一个文件：规则选型是「读快照 + 剥离键前缀 + 命中判定」三段纯逻辑，与既有的
// blocklet 自动选型（helpers.go，按扩展名/显式配置）是**两个正交维度**——规则只覆盖块级
// （Mode/Max），blocklet 仍由既有逻辑决定。混在一个函数里会让两者互相污染。

// userVisibleRel 把卷内 FS 键 `<owner>/user/<rel>` 剥离为用户可见相对路径。
//
// **形状判据**（不是「找第一个 user 段」）：`secretdata` 不知道 owner（键是上游 Location.FSPath
// 拼出来的），只能按形状识别——「首段 = owner、第二段必须 = user、其后非空」。
//
// 为什么不能用「找任意 user 段」：逻辑路径自身可以含 `user` 目录（`alice/user/docs/user/x`），
// 取第一个 `user` 段会把 `docs/user/x` 误当边界；固定位置判据则唯一。owner 名叫 `user`
// （`user/user/x` → rel=`x`）也正确。
//
// 形状不符（段数 <3、第二段不是 "user"、末段为空）⇒ ok=false：非用户面键（meta/chunk 等
// 内部桶），**不套任何用户规则**（规则面对的是用户可见路径）。
func userVisibleRel(fsKey string) (rel string, ok bool) {
	parts := strings.Split(fsKey, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] != "user" {
		return "", false
	}
	rel = strings.Join(parts[2:], "/")
	if rel == "" {
		return "", false
	}
	return rel, true
}

// SetPolicy 原子替换已编译规则快照（nil = 清空，回落卷默认策略）。
//
// 编译（含校验）由调用方经 policy.Compile 完成——本方法不做校验、不返回错误，因此不存在
// 「落盘成功但内存注入失败」的半生效窗口（设计 §10）。写路径读快照，无锁竞争。
func (s *SecretdataFS) SetPolicy(c *policy.Compiled) {
	s.policy.Store(c)
}

// policySnapshot 返回当前规则快照（nil = 无规则）。
func (s *SecretdataFS) policySnapshot() *policy.Compiled {
	return s.policy.Load()
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

// ruleForWrite 按 rel+size 解析规则命中的块级策略（未命中返回原策略与 false）。
//
// 非用户面键（形状不符）不参与匹配——规则只描述用户可见路径的语义。
func (s *SecretdataFS) ruleForWrite(rel string, size int64) (shardseal.BlockPolicy, bool) {
	base := s.opts.Block
	c := s.policySnapshot()
	if c == nil {
		return base, false
	}
	visible, ok := userVisibleRel(rel)
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
