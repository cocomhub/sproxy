// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume_mutation_wiring.go 是**本地卷结构性变异观察者的唯一接线点**
// （设计 2026-10-28-volume-state-maintenance §4.1 / V3）。
//
// 为什么需要接线：本地卷的写内核直接用 `storage.Root`（不经 `sync.FS`），因此「卷内容变了」
// 只能从 Root 观察。把 `files.Service.ApplyRootMutation` 挂到**每个本地卷根**上 ⇒ 派生状态
// （搜索索引）由机制维护，而不是靠每条旁路写「记得登记」（#874 事故族的结构性根治）。
//
// 接线时机（两处，覆盖全生命周期）：
//  1. `fileService()` 装配完成时（启动期，已有卷一次接全）；
//  2. 运行期新建本地卷成功后（`POST /api/volumes/user`）。
package server

import (
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// wireLocalVolumeMutations 给全部本地卷根装上变异观察者（幂等：重复调用只是重设同一回调）。
// svc 为 nil（文件服务未装配）或卷集合未装配时安全跳过。
func (h *Handlers) wireLocalVolumeMutations(svc *files.Service) {
	if h == nil || svc == nil || h.volSet == nil {
		return
	}
	for name, root := range h.volSet.LocalRoots() {
		root.SetMutationObserver(name, h.localVolumeMutationHandler(svc))
	}
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
