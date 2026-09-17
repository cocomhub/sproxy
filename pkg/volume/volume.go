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

// TypeLocal 是本地卷的卷后端类型字面量（V3 通用卷模型）。
// 装配层以 `Type == "" || Type == TypeLocal` 走本地卷路径；外部后端（如 baidupcs）
// 用各自类型字面量（见各自后端包的注册）。空串与 TypeLocal 等义（零迁移：旧配置不写
// Type 恒为本地卷）。
const TypeLocal = "local"

// ACL 是卷访问控制（装配期由 config 解析而来）。零值 = ModeDeny + 空名单（默认开放）。
type ACL struct {
	Mode   Mode
	Owners map[string]struct{}
	// MeshReaders 是跨节点授权（Y 一期只读 + Y 二期 scope 轴）。零值 = 无任何节点被授权
	// （fail-closed）。
	MeshReaders []MeshReader
}

// MeshReader 把「一个 mesh 节点身份」绑定到「一个 owner 命名空间」与**权限范围 scope**。
// Fingerprint 为 Ed25519 身份指纹（规范形 "sha256:<64 位小写 hex>"，由 pkg/server
// 在配置解析期经 tunnel.ParseFingerprint 归一后填入）。
type MeshReader struct {
	Node        string
	Fingerprint string
	Owner       string
	// Scope 是授权范围（Y 二期 P3；规格 §5.7）：read（只读，**空值等价于此** ⇒ 老配置零回归）
	// | write（只写）| rw（读写）。**读不隐含写、写不隐含读**；未知值 fail-closed（读写都拒，
	// 绝不「不认识就当 read」）。比较前做去空白 + 小写归一。
	Scope string
}

// mesh 授权范围字面量（**配置校验与授权判定的单一事实源**：pkg/server 的配置校验必须经
// NormalizeMeshScope，不得自列一份值集合，否则「配置放行但授权拒绝」或反之）。
//
// **不加类型化枚举**：值直接来自配置字符串，判定即「归一后精确匹配三值之一」。
const (
	// MeshScopeRead 只读（**空值等价于此** ⇒ 老配置零回归）。
	MeshScopeRead = "read"
	// MeshScopeWrite 只写（**写不隐含读**）。
	MeshScopeWrite = "write"
	// MeshScopeRW 读写。
	MeshScopeRW = "rw"
)

// NormalizeMeshScope 把配置/条目里的 scope 归一为三值之一；**空值与 read 等价**（零回归）。
// 未知值返回 ("", false)——调用方（配置校验 fail-fast、授权判定 fail-closed）据此拒绝。
func NormalizeMeshScope(scope string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", MeshScopeRead:
		return MeshScopeRead, true
	case MeshScopeWrite:
		return MeshScopeWrite, true
	case MeshScopeRW:
		return MeshScopeRW, true
	default:
		return "", false
	}
}

// scopeAllows 判定条目 scope 是否授予 want（MeshScopeRead / MeshScopeWrite）。
// rw 授予两者；未知 scope 一律不授予（fail-closed）。
func scopeAllows(scope, want string) bool {
	s, ok := NormalizeMeshScope(scope)
	if !ok {
		return false
	}
	return s == MeshScopeRW || s == want
}

// AuthorizeMeshRead 判定节点 node（已认证指纹 fingerprint）可否**只读**访问 owner 命名空间。
// 要求命中条目的 scope ∈ {read（含缺省）, rw}。
func (v Volume) AuthorizeMeshRead(node, fingerprint, owner string) bool {
	return v.authorizeMesh(node, fingerprint, owner, MeshScopeRead)
}

// AuthorizeMeshWrite 判定节点 node（已认证指纹 fingerprint）可否**写** owner 命名空间
// （Y 二期 P3）。要求命中条目的 scope ∈ {write, rw}——**写不隐含读**，反之亦然。
func (v Volume) AuthorizeMeshWrite(node, fingerprint, owner string) bool {
	return v.authorizeMesh(node, fingerprint, owner, MeshScopeWrite)
}

// authorizeMesh 是 mesh 授权的**单一判定入口**：三重约束（任一不满足即拒，fail-closed）——
//
//  1. 本卷 mesh_readers 中存在 (node, fingerprint, owner) 命中条目；
//  2. 该条目 scope 授予 want（read 只授读 / write 只授写 / rw 授两者 / 未知或空归一结果不授）；
//  3. owner 本身能过本卷 ACL（Authorize）——Mode=allow 须在 Owners 内，
//     Mode=deny/零值 不得在黑名单内。
//
// 指纹比较前做归一化（去空白 + 转小写），比较用 crypto/subtle.ConstantTimeCompare
// 以避免早期退出的时序差异（指纹非秘密，仅为防御一致性）。
func (v Volume) authorizeMesh(node, fingerprint, owner, want string) bool {
	if node == "" || owner == "" || fingerprint == "" {
		return false
	}
	if !v.Authorize(owner) {
		return false
	}
	fp := normalizeFingerprint(fingerprint)
	if fp == "" {
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
		if !scopeAllows(mr.Scope, want) {
			continue
		}
		if fingerprintEqual(normalizeFingerprint(mr.Fingerprint), fp) {
			return true
		}
	}
	return false
}

// MeshReaderFor 返回本卷 mesh_readers 中指纹命中 fingerprint 的首个条目（**含 Scope**，
// Y 二期写 listener 需要它才能判定写权限）。
// 空指纹或无命中返回 false（fail-closed）。供 B 侧远程 handler 由「已认证对端指纹」
// 反查 (node, owner) 绑定——owner 绝不由请求方指定。
//
// 注意：本方法只做指纹反查，不施加卷 ACL 与 scope 约束。它不得作为唯一授权依据——
// 授权判定必须走 AuthorizeMeshRead / AuthorizeMeshWrite（三重约束齐备）。
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
//
// 空值短路（a == "" 判否）在本包当前调用点下不可达——两个调用方都先拦下空 want，
// 且 a=="" 与 b!="" 会被长度比较先短路。保留它是纵深防御：本函数是包私有恒等比较
// 原语，「空值永不判等」是自洽的安全不变量，与调用方守卫解耦；将来新增调用方若忘写
// 空值守卫，缺了它会静默把两个空指纹判等。
func fingerprintEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Volume 是装配后不可变卷描述。RootDir 由装配层持有根句柄，此处仅配置元数据。
//
// Type 是卷后端类型（V3 通用卷模型）：空串/"local" = 本地卷（缺省，零迁移）；
// 其它取值（如 "baidupcs"）由装配层经 registry 后端注册表分派到对应构造器。
// Type 不参与 Authorize/选卷（纯元数据，决定「怎么装配」而非「谁能用」）。
//
// Extra 是类型特有配置（map[string]any，JSON 友好）：本地卷恒 nil；外部卷后端
// 构造器从其中读取（如 baidupcs 的 BDUSS/root/binary_path）。
type Volume struct {
	Name     string
	Type     string // 卷后端类型；空 = local（缺省）
	RootDir  string
	Capacity int64 // 0 = 不限制
	ACL      ACL
	Extra    map[string]any // 类型特有配置（外部卷后端消费；本地卷恒 nil）
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
