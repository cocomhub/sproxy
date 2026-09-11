// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package volume 是 sproxy 多卷存储的纯域模型：卷定义、ACL 判定与选卷排序。
// 无 I/O、无 quota、无 storage 依赖——只做可测试的决策逻辑，装配/账本由 server 承担。
package volume

import (
	"crypto/subtle"
	"sort"
	"strings"
)

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
	// MeshReaders 是跨节点只读授权（Y 一期，AD-5）。零值 = 无任何节点被授权（fail-closed）。
	MeshReaders []MeshReader
}

// MeshReader 把「一个 mesh 节点身份」绑定到「一个可只读访问的 owner 命名空间」。
// Fingerprint 为 Ed25519 身份指纹（规范形 "sha256:<64 位小写 hex>"，由 pkg/server
// 在配置解析期经 tunnel.ParseFingerprint 归一后填入）。
type MeshReader struct {
	Node        string
	Fingerprint string
	Owner       string
}

// AuthorizeMeshRead 判定节点 node（已认证指纹 fingerprint）可否只读访问 owner 命名空间。
//
// 双重约束（任一不满足即拒，fail-closed）：
//  1. 本卷 mesh_readers 中存在三元组 (node, fingerprint, owner) 的命中条目；
//  2. owner 本身能过本卷 ACL（Authorize）——Mode=allow 须在 Owners 内，
//     Mode=deny/零值 不得在黑名单内。
//
// 指纹比较前做归一化（去空白 + 转小写），比较用 crypto/subtle.ConstantTimeCompare
// 以避免早期退出的时序差异（指纹非秘密，仅为防御一致性）。
func (v Volume) AuthorizeMeshRead(node, fingerprint, owner string) bool {
	if node == "" || owner == "" || fingerprint == "" {
		return false
	}
	if !v.Authorize(owner) {
		return false
	}
	want := normalizeFingerprint(fingerprint)
	if want == "" {
		// 归一化后为空 = 原始值只有空白 → 拒绝（与 MeshReaderFor 同构，意图显式化）。
		// 本守卫行为中性：缺了它结果同为 false（空 want 只可能与空条目指纹「恒等」，
		// 而后者已被 fingerprintEqual 的 a == "" 拦下），此处只为让「空指纹必须拒绝」
		// 这一约束在各调用方就地可见，不必跨函数推导。
		return false
	}
	for i := range v.ACL.MeshReaders {
		mr := &v.ACL.MeshReaders[i]
		if mr.Node != node || mr.Owner != owner {
			continue
		}
		if fingerprintEqual(normalizeFingerprint(mr.Fingerprint), want) {
			return true
		}
	}
	return false
}

// MeshReaderFor 返回本卷 mesh_readers 中指纹命中 fingerprint 的首个条目。
// 空指纹或无命中返回 false（fail-closed）。供 B 侧远程 handler 由「已认证对端指纹」
// 反查 (node, owner) 绑定——owner 绝不由请求方指定。
//
// 注意：本方法只做指纹反查，不施加卷 ACL 的第二重约束（不调 Authorize）。
// 它不得作为唯一授权依据——授权判定必须走 AuthorizeMeshRead（两约束齐备）。
func (v Volume) MeshReaderFor(fingerprint string) (MeshReader, bool) {
	want := normalizeFingerprint(fingerprint)
	if want == "" {
		return MeshReader{}, false
	}
	for i := range v.ACL.MeshReaders {
		if fingerprintEqual(normalizeFingerprint(v.ACL.MeshReaders[i].Fingerprint), want) {
			return v.ACL.MeshReaders[i], true
		}
	}
	return MeshReader{}, false
}

// normalizeFingerprint 归一化指纹用于比较：去首尾空白 + 转小写（前缀 "sha256:" 保留，
// 两端一致即可；配置侧已由 tunnel.ParseFingerprint 归一为规范形）。
//
// 规范形校验（"sha256:" 前缀 / 64 位 hex）刻意不在本包：畸形指纹由 pkg/server 的配置
// 加载期（Config.Validate 调 tunnel.ParseFingerprint）响亮拒绝，故本函数只需处理大小写
// 与空白。不校验意味着畸形值只会「永不命中」，方向仍是 fail-closed。
func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.TrimSpace(fp))
}

// fingerprintEqual 恒时比较两个已归一化指纹（长度不同直接判否，不做恒时比较）。
func fingerprintEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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
