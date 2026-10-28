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

import "github.com/cocomhub/sproxy/pkg/files"

// wireLocalVolumeMutations 给全部本地卷根装上变异观察者（幂等：重复调用只是重设同一回调）。
// svc 为 nil（文件服务未装配）或卷集合未装配时安全跳过。
func (h *Handlers) wireLocalVolumeMutations(svc *files.Service) {
	if h == nil || svc == nil || h.volSet == nil {
		return
	}
	for name, root := range h.volSet.LocalRoots() {
		// 观察者只做内存索引更新 + 一次 Stat（见 files.ApplyRootMutation 注释）。
		root.SetMutationObserver(name, svc.ApplyRootMutation)
	}
}
