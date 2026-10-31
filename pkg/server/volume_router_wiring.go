// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_router_wiring.go 把卷定位/卷 ACL 的**单一实现**（pkg/volume.Router，见
// pkg/volume/router.go）接到装配层：注入 DefaultTenant / DefaultAllowed / Views 三个回调，
// pkg/server 侧不再保留「默认卷快路径 + 视图扫描」的重复实现。
//
// 接线范围（P1-a）：读定位（h.locateOwnerFile）。`volumeAllowedFor` 仍走本地判定——两者
// fail-closed 口径不同（它要求卷集**已装配**，Router 的默认卷 ACL 在旧装配路径上恒放行），
// 合并需另行裁定语义，不在本片范围。

import (
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// volumeRouter 构造卷路由器（Router 无可变状态，按调用构造即可：Deps 只是三个函数值）。
func (h *Handlers) volumeRouter() *volume.Router {
	return volume.NewRouter(volume.RouterDeps{
		DefaultTenant:  h.tenantFor,
		DefaultAllowed: h.defaultVolumeAllows,
		Views:          h.volumeRouterViews,
	})
}

// volumeRouterViews 返回 owner 视图内的**非默认卷**（默认卷由 DefaultTenant 承担）。
// 顺序与卷声明序一致（= 既有 locateOwnerFileScan 的遍历序）。
//
// Allowed 与 Tenant 分开求值：ACL 与「卷上是否已有该 owner 的目录」无关（Allowed 恒按
// volume.Authorize，未命中的卷也列出 Allowed=false 以便 Allowed 判定精确）；Tenant 仅在
// ACL 放行时才取（放行的卷才可能被 stat）。
func (h *Handlers) volumeRouterViews(owner string) []volume.RouterView {
	if h.volSet == nil {
		return nil
	}
	defaultName := h.volSet.Default().Name
	vols := h.volSet.All()
	out := make([]volume.RouterView, 0, len(vols))
	for _, v := range vols {
		if v.Name == defaultName {
			continue
		}
		if !v.Authorize(owner) {
			out = append(out, volume.RouterView{Name: v.Name})
			continue
		}
		out = append(out, volume.RouterView{
			Name:    v.Name,
			Tenant:  h.existingVolumeTenant(v.Name, owner),
			Allowed: true,
		})
	}
	return out
}

// existingVolumeTenant 取卷上 owner 的租户句柄，**但不创建租户目录**：定位是只读操作
// （pkg/volume.Router 的不变量 + 既有 locateOwnerFileScan「只探测，不创建租户目录」），
// 而 h.volumeTenant → storage.OpenTenant 会 MkdirAll 租户根。故先用**既有只读探针**
// h.volumeFileExists 确认 owner 段已存在（探针 rel 取 "." = owner 目录自身，只 stat），
// 存在时取到的租户句柄不会新建任何目录（MkdirAll 为 no-op）。
//
// 不自建裸卷根探针：裸卷根访问点由 R28 棘轮登记（set_root_calls.tsv），复用既有探针
// 不新增登记项。
//
// 外部卷（无本地根）/ 未装配 / 尚无该 owner 目录 → nil（Router 视同未命中）。
func (h *Handlers) existingVolumeTenant(volName, owner string) *storage.Tenant {
	exists, err := h.volumeFileExists(volName, normalizeOwner(owner), ownerDirProbeRel)
	if err != nil || !exists {
		return nil
	}
	return h.volumeTenant(volName, owner)
}

// ownerDirProbeRel 是「探测 owner 目录自身是否存在」的 rel（经 Root.Stat 解析为
// <卷根>/<owner>/，不创建任何目录）。
const ownerDirProbeRel = "."
