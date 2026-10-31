// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// router.go 是卷路由与卷 ACL 判定的**单一实现**（P1-a 只新增能力，接线在后续 PR）：
//
//   - `Locate` 复刻服务端读定位（`locateOwnerFile`）语义：默认卷快路径（ACL 放行 +
//     租户可用 + stat 命中）优先，未命中再按视图顺序扫描其余卷；全部未命中返回 false。
//     唯一性保证（AD-4）下同 rel 不会多卷命中；真出现多卷命中时**默认卷优先、其余首卷
//     胜出**——调用方只应依赖「至多一卷命中」的不变式。
//   - `Allowed` 复刻 ACL 判定（`volumeAllowedFor` / `defaultVolumeAllows`）：未知卷名、
//     未装配、非法 owner 一律 fail-closed（不泄卷存在性）。
//   - 探测只经 `storage.Root.Stat`（只读），**不创建任何目录**。
//
// 定位不返回错误：探测失败的卷视同未命中（调用方按 404/500 语义自行决定），与既有
// 读定位口径一致。
//
// 依赖经 RouterDeps 以函数注入——本包不 import 装配层（pkg/server），见门禁 R6。

import "github.com/cocomhub/sproxy/pkg/storage"

// RouterView 是 owner 卷视图内的一个**非默认卷**只读快照：卷名 + 该卷上 owner 的租户
// （nil = 不可用）+ 该卷对 owner 的 ACL 判定结果（由装配层按卷 ACL 预先算出）。
type RouterView struct {
	Name    string
	Tenant  *storage.Tenant
	Allowed bool
}

// RouterDeps 是 Router 的装配依赖。字段为 nil 等价于「该项不可用」（fail-closed）。
type RouterDeps struct {
	// DefaultTenant 返回默认卷上 owner 的租户（nil = 不可用）。
	DefaultTenant func(owner string) *storage.Tenant
	// Views 返回 owner 视图内的**其余卷**（默认卷由 DefaultTenant 承担，不重复列出）。
	Views func(owner string) []RouterView
	// DefaultAllowed 判定默认卷对 owner 的 ACL（nil = 拒绝）。
	DefaultAllowed func(owner string) bool
}

// RouterLocation 是一次定位结果：卷名 + 该卷上 owner 的租户。
// Volume 为空串表示**默认卷**（RouterDeps 不携带默认卷名，默认卷名由装配方自持）。
type RouterLocation struct {
	Volume string
	Tenant *storage.Tenant
}

// Router 是卷路由与 ACL 判定的实现；本身不持有可变状态（并发调用只依赖注入的函数）。
type Router struct{ deps RouterDeps }

// NewRouter 构造 Router。
func NewRouter(d RouterDeps) *Router { return &Router{deps: d} }

// Allowed 判定 owner 是否被 volName 卷的 ACL 放行（volName 为空 = 默认卷）。
// 未知卷名 / 未装配 / 非法 owner → false（fail-closed）。
func (r *Router) Allowed(owner, volName string) bool {
	if r == nil {
		return false
	}
	owner = storage.NormalizeOwner(owner)
	if !routerOwnerValid(owner) {
		return false
	}
	if volName == "" {
		return r.defaultAllowed(owner)
	}
	for _, v := range r.views(owner) {
		if v.Name == volName {
			return v.Allowed
		}
	}
	return false
}

// Locate 返回 owner 视图内 rel 所在卷的租户（读定位）：默认卷快路径 → 视图顺序扫描。
func (r *Router) Locate(owner, rel string) (RouterLocation, bool) {
	if r == nil || rel == "" {
		return RouterLocation{}, false
	}
	owner = storage.NormalizeOwner(owner)
	if !routerOwnerValid(owner) {
		return RouterLocation{}, false
	}
	if loc, ok := r.locateDefault(owner, rel); ok {
		return loc, true
	}
	for _, v := range r.views(owner) {
		if !v.Allowed || v.Name == "" || v.Tenant == nil {
			continue
		}
		root := v.Tenant.Root()
		if root == nil {
			continue
		}
		if _, err := root.Stat(rel); err != nil {
			continue
		}
		return RouterLocation{Volume: v.Name, Tenant: v.Tenant}, true
	}
	return RouterLocation{}, false
}

// locateDefault 默认卷快路径：ACL 放行 + 租户可用 + stat 命中。默认卷被 ACL 排除时
// **不得**经此路径命中（AD-6：所有定位点先过 ACL），由视图扫描接手。
func (r *Router) locateDefault(owner, rel string) (RouterLocation, bool) {
	if !r.defaultAllowed(owner) {
		return RouterLocation{}, false
	}
	tnt := r.defaultTenant(owner)
	if tnt == nil || tnt.Root() == nil {
		return RouterLocation{}, false
	}
	if _, err := tnt.Root().Stat(rel); err != nil {
		return RouterLocation{}, false
	}
	return RouterLocation{Tenant: tnt}, true
}

// defaultAllowed 默认卷 ACL：未注入判定函数 → 拒绝（fail-closed）。
func (r *Router) defaultAllowed(owner string) bool {
	return r.deps.DefaultAllowed != nil && r.deps.DefaultAllowed(owner)
}

// defaultTenant 默认卷租户：未注入 → 不可用。
func (r *Router) defaultTenant(owner string) *storage.Tenant {
	if r.deps.DefaultTenant == nil {
		return nil
	}
	return r.deps.DefaultTenant(owner)
}

// views 其余卷视图：未注入 → 空视图。
func (r *Router) views(owner string) []RouterView {
	if r.deps.Views == nil {
		return nil
	}
	return r.deps.Views(owner)
}

// routerOwnerValid 判定归一后的 owner 能否作租户段名。fail-closed 理由：非法段名
// （逃逸 / 保留字符）会让 `<卷根>/<owner>/...` 越界；保留功能桶名（user/meta/...）
// 会与桶解析歧义（见 storage.NewTenant 同口径校验）。
func routerOwnerValid(owner string) bool {
	return storage.ValidSegmentName(owner) && !storage.IsReservedBucketName(owner)
}
