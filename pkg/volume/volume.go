// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package volume 是 sproxy 多卷存储的纯域模型：卷定义、ACL 判定与选卷排序。
// 无 I/O、无 quota、无 storage 依赖——只做可测试的决策逻辑，装配/账本由 server 承担。
package volume

import "sort"

// Mode 是卷 ACL 模式与选卷放置策略共用的枚举字符串类型。
type Mode string

const (
	ModePreferDefault Mode = "prefer-default"
	ModeSpread        Mode = "spread"
	ModeDeny          Mode = "deny"  // ACL 模式：默认开放 + 黑名单
	ModeAllow         Mode = "allow" // ACL 模式：默认拒绝 + 白名单
)

// ACL 是卷访问控制（装配期由 config 解析而来）。零值 = ModeDeny + 空名单（默认开放）。
type ACL struct {
	Mode   Mode
	Owners map[string]struct{}
}

// Volume 是装配后不可变卷描述。RootDir 由装配层持有根句柄，此处仅配置元数据。
type Volume struct {
	Name     string
	RootDir  string
	Capacity int64 // 0 = 不限制
	ACL      ACL
}

// Authorize 判定 owner 是否可用本卷。ACL.Mode 假定已由 config Validate 校验为 allow|deny；
// 此处对未知值 fail-closed：Mode==""（零值/未配，上游未设）归入 deny 开放语义（AD-6 兼容
// 默认卷缺省开放），Mode 为其它非空未知字符串 → 拒绝。
func (v Volume) Authorize(owner string) bool {
	switch v.ACL.Mode {
	case ModeAllow:
		_, ok := v.ACL.Owners[owner]
		return ok
	case "", ModeDeny: // 零值/未配 或 deny：默认开放，黑名单命中才拒
		_, banned := v.ACL.Owners[owner]
		return !banned
	default: // 未知/未来 mode：fail-closed
		return false
	}
}

// AllowedVolumes 返回 owner 允许的卷子集（保持声明序）。
func AllowedVolumes(vols []Volume, owner string) []Volume {
	out := make([]Volume, 0, len(vols))
	for _, v := range vols {
		if v.Authorize(owner) {
			out = append(out, v)
		}
	}
	return out
}

// DefaultVolume 返回默认卷（首个）。空列表返回零值 Volume{Name:"<none>"}。
func DefaultVolume(vols []Volume) Volume {
	if len(vols) == 0 {
		return Volume{Name: "<none>"}
	}
	return vols[0]
}

// OrderCandidates 依 placement 策略对候选卷排序（均已过 ACL）：
//   - prefer-default：首个（默认卷）最前，其余保持声明序（默认卷满才轮询后续）；
//   - spread：按 (Capacity-used) 余量降序——有容量上限的卷按余量优先（真正多盘利用）；
//     容量 0（不限）的卷余量按 0 计、排末尾，作溢出兜底（无限余量会让 spread 恒选它、
//     有界盘永不被均衡利用，故反语义，见 TestOrderCandidates）。
func OrderCandidates(vols []Volume, placement Mode, used func(name string) int64) []Volume {
	out := append([]Volume(nil), vols...)
	switch placement {
	case ModeSpread:
		sort.SliceStable(out, func(i, j int) bool {
			return remain(out[i], used) > remain(out[j], used)
		})
	default: // prefer-default：声明序即默认卷优先（AllowedVolumes 保持声明序）
	}
	return out
}

// remain 返回卷余量 (Capacity-used)。used 缺省 0；used≥Capacity 或容量 0（不限）均返回 0——
// 后者使不限容量卷在 spread 中排末尾（见 OrderCandidates 语义注释）。
func remain(v Volume, used func(name string) int64) int64 {
	u := int64(0)
	if used != nil {
		u = used(v.Name)
	}
	if v.Capacity <= 0 {
		return 0 // 不限容量：spread 下无「余量压力」，作兜底排末尾
	}
	if u >= v.Capacity {
		return 0
	}
	return v.Capacity - u
}
