// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package policy 是 secretdata 卷的**分块规则引擎**：把「路径正则 + 体积区间」映射为块级
// 分块模式（single / random）。
//
// 两层形态（设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §3）：
//   - Policy / Rule 是**用户面与持久化形状**（JSON 直接落盘/回显）；
//   - Compiled 是**运行期形态**（规则已排序 + 正则已编译）——写路径每写一个文件都要匹配，
//     把 regexp.Compile 放在写路径就是每文件一次编译开销，故只在装配/热更新时编译一次。
//
// 校验与编译是**同一份实现**（Compile）：静态配置、运行期写入、装配载入三处共用，
// 不存在「一处严一处松」的漂移。所有拒因都带规则下标，便于 UI 精确定位。
package policy

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

const (
	// HardMaxSingleBytes 是 single 模式的**硬上限**（64 MiB）。
	//
	// 为什么必须有上限：单块加密会把整块明文载入内存（shardseal.encryptWriteChunks 的
	// `make([]byte, b.Size)`）⇒ 「无上限的整文件单块」就是一条 OOM 通道。64 MiB 覆盖
	// 「图片/文本/日志/已压缩包」这些目标场景，同时低于默认块上限 200 MiB。
	//
	// **单一事实源**：直接引用 shardseal.MaxSingleBlockBytes（规划器用同一常量兜底**所有**
	// 构造入口）。注意：卷级 `extra.block_policy.mode: single` 已在装配期被拒（无法表达超限
	// 语义），现只剩规则入口与程序内直接构造 BlockPolicy 两条路径。不要把数值复制到这里。
	HardMaxSingleBytes int64 = shardseal.MaxSingleBlockBytes
	// MaxRules 是单卷规则条数上限（匹配是每写一次全表扫描，条数直接决定写路径开销）。
	MaxRules = 64
	// MaxRegexLen 是 PathRegex 的字符串长度上限。Go regexp 是 RE2（线性时间、无回溯爆炸），
	// 故不需要防灾难性回溯；此上限只防超大模式占内存与拖慢编译。
	MaxRegexLen = 512
)

// Rule 是一条分块规则：路径正则 + 体积区间 → 块级模式。
type Rule struct {
	// PathRegex 匹配**用户可见相对路径**（`<owner>/user/<rel>` 剥离后的 `<rel>`）。
	//
	// **匹配语义 = 子串匹配（非整串）**：直接用 regexp.MatchString，不加 `^`/`$` 包装。
	// 因此 `\.jpg$` 命中 `photos/a.jpg`、`^archive/` 命中 `archive/x.zip`、`^photos/` 命中
	// `photos/2026/a.png`——这些正是用户会写的形态。若改成整串匹配，`\.jpg$` 就只等于
	// 「文件名叫 .jpg」，规则形同废纸（设计文档 §3.1 早先写「整串匹配」已按实测订正）。
	// 需要整串语义的调用方自行写 `^...$`。
	PathRegex string `json:"path_regex"`
	// MinSize/MaxSize 是可选的体积**闭区间**（字节）。0 = 该侧不限。
	MinSize sizex.ByteSize `json:"min_size,omitempty"`
	MaxSize sizex.ByteSize `json:"max_size,omitempty"`
	// Mode 是块级模式：shardseal.BlockModeSingle（整文件 1 块）或 BlockModeRandom（显式回落）。
	Mode string `json:"mode"`
	// Priority 是排序键（大者先）；同 priority 按声明顺序（稳定排序）。
	Priority int `json:"priority,omitempty"`
	// Note 是可选的人类可读说明（仅回显，不参与匹配）。
	Note string `json:"note,omitempty"`
}

// Policy 是一个卷的分块规则集（用户面/持久化形状）。
type Policy struct {
	Rules   []Rule `json:"rules"`
	Version int64  `json:"version"`
}

// compiledRule 是 Rule 的运行期形态（正则已编译）。
type compiledRule struct {
	rule Rule
	re   *regexp.Regexp
}

// Compiled 是编译后的只读规则快照：可被多 goroutine 并发使用（构造后不再变）。
// nil *Compiled 表示「无规则」⇒ 调用方回落卷默认策略。
type Compiled struct {
	rules   []compiledRule
	version int64
}

// Compile 校验并编译规则集（装配期与热更新各调用一次；写路径不重复编译）。
//
// volumeBlockMax 是卷级 block_policy.max（≤0 = 未知，跳过该条比较）：规则允许的块大小
// 不得超过卷级上界——因为配额上界（PhysBound）按卷级 max 折算，规则块超出会让物理上界
// 被低估。校验失败返回 error（含规则下标）。
func Compile(p Policy, volumeBlockMax int64) (*Compiled, error) {
	if len(p.Rules) > MaxRules {
		return nil, fmt.Errorf("policy: 规则条数 %d 超过上限 %d", len(p.Rules), MaxRules)
	}
	out := &Compiled{rules: make([]compiledRule, 0, len(p.Rules)), version: p.Version}
	for i, r := range p.Rules {
		re, err := compileRule(i, r, volumeBlockMax)
		if err != nil {
			return nil, err
		}
		out.rules = append(out.rules, compiledRule{rule: r, re: re})
	}
	// priority 降序 + 声明序稳定（SliceStable 保证同 priority 保持输入顺序）。
	sort.SliceStable(out.rules, func(a, b int) bool {
		return out.rules[a].rule.Priority > out.rules[b].rule.Priority
	})
	return out, nil
}

// compileRule 校验单条规则并编译其正则（错误统一带规则下标）。
func compileRule(idx int, r Rule, volumeBlockMax int64) (*regexp.Regexp, error) {
	switch {
	case r.PathRegex == "":
		return nil, fmt.Errorf("policy: 规则 %d 的 path_regex 不能为空", idx)
	case len(r.PathRegex) > MaxRegexLen:
		return nil, fmt.Errorf("policy: 规则 %d 的 path_regex 长度 %d 超过上限 %d", idx, len(r.PathRegex), MaxRegexLen)
	case r.Mode != shardseal.BlockModeSingle && r.Mode != shardseal.BlockModeRandom:
		return nil, fmt.Errorf("policy: 规则 %d 的 mode %q 非法（须为 %q 或 %q）",
			idx, r.Mode, shardseal.BlockModeSingle, shardseal.BlockModeRandom)
	case r.MinSize > 0 && r.MaxSize > 0 && r.MinSize > r.MaxSize:
		return nil, fmt.Errorf("policy: 规则 %d 的 min_size(%d) 大于 max_size(%d)", idx, int64(r.MinSize), int64(r.MaxSize))
	}
	if err := validateSingleSize(idx, r, volumeBlockMax); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(r.PathRegex)
	if err != nil {
		return nil, fmt.Errorf("policy: 规则 %d 的 path_regex 无法编译: %w", idx, err)
	}
	return re, nil
}

// validateSingleSize 校验 single 模式的体积上限约束（拆出以控制 compileRule 的认知复杂度）。
func validateSingleSize(idx int, r Rule, volumeBlockMax int64) error {
	if r.Mode != shardseal.BlockModeSingle {
		return nil
	}
	if r.MaxSize <= 0 {
		return fmt.Errorf("policy: 规则 %d 是 single 模式，需给 max_size（无上限的整文件单块会导致内存峰值不可控）", idx)
	}
	if int64(r.MaxSize) > HardMaxSingleBytes {
		return fmt.Errorf("policy: 规则 %d 的 max_size(%d) 超过硬上限 %d", idx, int64(r.MaxSize), HardMaxSingleBytes)
	}
	if volumeBlockMax > 0 && int64(r.MaxSize) > volumeBlockMax {
		return fmt.Errorf("policy: 规则 %d 的 max_size(%d) 超过卷块上限 %d（会导致物理上界被低估）",
			idx, int64(r.MaxSize), volumeBlockMax)
	}
	return nil
}

// Resolve 返回命中的规则副本（调用方不可改内部状态）。首命中即停（已按 priority 降序）。
//
// size < 0（未知长度）时**忽略体积约束**、只按路径匹配——防御未来调用方（当前两条写路径
// 都保证 size ≥ 0，见设计 §5.1）。
func (c *Compiled) Resolve(userRel string, size int64) (Rule, bool) {
	if c == nil {
		return Rule{}, false
	}
	for _, cr := range c.rules {
		if !cr.re.MatchString(userRel) {
			continue
		}
		if !sizeInRange(cr.rule, size) {
			continue
		}
		return cloneRule(cr.rule), true
	}
	return Rule{}, false
}

// sizeInRange 判定 size 是否落在规则的体积闭区间内（size<0 视作不参与约束）。
func sizeInRange(r Rule, size int64) bool {
	if size < 0 {
		return true
	}
	if r.MinSize > 0 && size < int64(r.MinSize) {
		return false
	}
	if r.MaxSize > 0 && size > int64(r.MaxSize) {
		return false
	}
	return true
}

// HasRules 报告是否至少有一条规则（**零分配**，供写路径热路径判空用）。
//
// 为什么不能直接用 len(Rules())>0：Rules() 每次都 make 新切片并逐条 clone，而该判空在
// 每次文件写入时都会执行（secretdata 写路径）⇒ 给热路径平白增加 O(规则数) 分配。
func (c *Compiled) HasRules() bool {
	if c == nil {
		return false
	}
	return len(c.rules) > 0
}

// Rules 返回规则副本（API 回显用；调用方可自由修改）。
func (c *Compiled) Rules() []Rule {
	if c == nil {
		return nil
	}
	out := make([]Rule, 0, len(c.rules))
	for _, cr := range c.rules {
		out = append(out, cloneRule(cr.rule))
	}
	return out
}

// Version 返回编译时携带的持久化版本（0 = 未持久化）。
func (c *Compiled) Version() int64 {
	if c == nil {
		return 0
	}
	return c.version
}

// cloneRule 返回值拷贝（Rule 全为值类型，直接复制即可）。
func cloneRule(r Rule) Rule { return r }
