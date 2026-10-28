// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume_mutation_wiring.go 是**本地卷结构性变异观察者的唯一接线点**
// （设计 2026-10-28-volume-state-maintenance §4.1 / V3）。
//
// 为什么需要接线：本地卷的写内核直接用 `storage.Root`（不经 `sync.FS`），因此「卷内容变了」
// 只能从 Root 观察。把 `files.Service.ApplyRootMutation` 挂到**每个本地卷根**上 ⇒ 派生状态
// （搜索索引）由机制维护，而不是靠每条旁路写「记得登记」（#874 事故族的结构性根治）。
//
// 接线时机（**两阶段**，覆盖全生命周期）：
//  1. **装配早期**（`RegisterRoutes` 里 `assembleVolumes` 之后、任何租户被创建之前）：
//     `installLocalVolumeObservers` 把**转发器**装到每个本地卷根上——租户子根在创建时继承
//     观察者，晚装会让已缓存的租户根（如启动期就创建的 anonymous）永不发事件；
//  2. **文件服务装配完成**（`fileService()` 的 Once 内）：`wireLocalVolumeMutations` 把真正的
//     处理函数（索引 + 缓存）存入原子指针，转发器随即生效。
package server

import (
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// installLocalVolumeObservers 在**装配早期**把转发器装到全部本地卷根上（必须在任何
// 租户子根创建之前调用——子根只在创建时继承观察者）。
func (h *Handlers) installLocalVolumeObservers() {
	if h == nil || h.volSet == nil {
		return
	}
	for name, root := range h.volSet.LocalRoots() {
		root.SetMutationObserver(name, h.dispatchRootMutation)
	}
}

// dispatchRootMutation 把事件转给当前已装配的处理函数（未装配 ⇒ 丢弃：此时索引/缓存都还不存在，
// 丢弃是安全的——索引按需从磁盘构建，缓存此时为空）。
func (h *Handlers) dispatchRootMutation(m storage.Mutation) {
	if fn := h.mutationHandler.Load(); fn != nil {
		(*fn)(m)
	}
}

// wireLocalVolumeMutations 绑定真正的处理函数（文件服务装配完成后调用一次）。同时重装观察者，
// 覆盖「装配后新增的本地卷根」（幂等）。
func (h *Handlers) wireLocalVolumeMutations(svc *files.Service) {
	if h == nil || svc == nil || h.volSet == nil {
		return
	}
	fn := h.localVolumeMutationHandler(svc)
	h.mutationHandler.Store(&fn)
	h.installLocalVolumeObservers()
}

// localVolumeMutationHandler 把一次 Root 变异**同时**投递给两个派生状态消费方：
//  1. `files.ApplyRootMutation`——搜索索引（结构 + 内容写）；
//  2. `volumestate.Cache.Invalidate`——卷状态缓存（本地卷启用缓存后，写内核经 Root 的写入
//     不会经过 FS 装饰器，必须由事件驱动失效；否则列表要等 TTL 才更新）。
//
// 两者都只做内存操作（+ 一次 Stat），同步执行、不阻塞写路径。
func (h *Handlers) localVolumeMutationHandler(svc *files.Service) func(storage.Mutation) {
	return func(m storage.Mutation) {
		svc.ApplyRootMutation(m)
		if h.stateCache == nil {
			return
		}
		h.stateCache.Invalidate(volumestate.ObjectRef{Volume: m.Tag, Path: m.Rel})
		if m.Rel2 != "" {
			h.stateCache.Invalidate(volumestate.ObjectRef{Volume: m.Tag, Path: m.Rel2})
		}
	}
}
