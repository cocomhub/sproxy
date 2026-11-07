// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package volumestate 是**卷状态缓存**的唯一实现：把目录视图（list）与内容派生值
// （checksum）的缓存、失效收敛到一处，避免每个卷各写一套。
//
// 设计：docs/designs/2026-10-28-volume-state-maintenance.md。四条不变量（长期稳定）：
//
//	P1 卷维度：缓存键恒为 (卷名, FS 相对路径)（路径首段即 owner）——不存在 owner 级全局状态；
//	   跨卷聚合是「对卷集合的派生遍历」，不是独立索引。
//	P2 横切关注点：本层在**装配期唯一入口**包住卷的 FS（`CacheFS`），业务代码零感知。
//	P3 失效由**变更事件**驱动：装饰器在写方法（WriteFile/Delete/Rename/MakeDir）成功后
//	   自动失效，调用方无需登记（根因 #874：靠调用点自觉 ⇒ 漏一处即漂移）。
//	P4 卷自管只报告事实：卷通过 `sync.Entry.Checksum*` 与可选 `volume.StateGeneration`
//	   如实上报，**不得**自建 list 缓存（门禁 R40）。
//
// 权威口径：**卷是真相源，缓存只加速**——A 类（内容派生值）永不以缓存值充当最终依据；
// 需要 checksum 的安全操作必须走「调用方给 → 现取 Stat → fail-closed」三段式。
//
// 落盘：受管 staging 目录 `<storage_root>/meta/state/<卷>/<FS 路径>/entries.json`
// （节点本地，与容量账本 `<root>/meta/quota/<卷>` 同约定；不进卷、不进网盘）。
package volumestate
