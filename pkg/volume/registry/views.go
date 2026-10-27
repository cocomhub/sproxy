// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// views.go —— 按用途统一访问器（U2，docs/designs/2026-10-28-unified-volume-model.md §二）。
//
// 核心原则：`StorageVolume` 是唯一卷身份，`VolumeView` 是唯一取卷入口——同一卷按用途
// 看到不同装饰链/键空间（用户文件 / 控制面 / 密钥）。消费方**永远拿不到裸 FS**。

// ViewUse 是卷用途视图的枚举（按用途封装——本地卷不是「一个 FS」，而是多个用途视图）。
type ViewUse string

const (
	// ViewUser 用户文件视图（转存目标 / 上传 Sink / 下载）：Guard → Wrap(trusted) →
	// capacity → staging（staging per-owner 由消费方 `ApplyStagingQuota` 在取值时套）。
	ViewUser ViewUse = "user"
	// ViewMeta 控制面视图（sidecar / checksum / 台账 / 审计）：Guard（不经容量/staging）。
	// meta 桶是本地卷概念；外部卷经同一 Guard 视图返回（无 meta 桶语义，消费方按需跳过）。
	ViewMeta ViewUse = "meta"
	// secrets 不入通用表（安全：R26 白名单维持）——装配期单独构造，`VolumeView` 拒绝该 use。
)

// VolumeViewDecorator 是装配层注入的装饰器：对裸卷 FS 按 use 套完整装饰链
// （封印 → trusted.Guard/Wrap → capacity）。registry 不 import server（避免成环），
// 由装配层（pkg/server）经 `Set.SetVolumeViewDecorator` 注入。
//
// **默认值 = 内置 trusted 装饰链**（Ruling，2026-10-28）：registry 可 import trusted
// （无环）——测试 fake 卷集不注入装饰器时仍得到 Guard/Wrap 语义（meta sidecar 生成/校验、
// Delete 联动 sidecar+记账），避免「生产有装饰、测试裸 FS」的行为漂移。装配层覆盖
// 以注入 Logger/ChunkSize。
type VolumeViewDecorator func(name string, use ViewUse, raw syncpkg.FS) syncpkg.FS

// defaultVolumeViewDecorator 是内置默认装饰器（trusted.Guard/Wrap；ViewMeta 只 Guard）。
func defaultVolumeViewDecorator(_ string, use ViewUse, raw syncpkg.FS) syncpkg.FS {
	switch use {
	case ViewMeta:
		return trusted.Guard(raw)
	default: // ViewUser
		return trusted.Guard(trusted.Wrap(raw, trusted.Options{}))
	}
}

// SetVolumeViewDecorator 注入卷视图装饰器（装配期调用一次；重复注入覆盖——装配层单一）。
func (vs *Set) SetVolumeViewDecorator(d VolumeViewDecorator) {
	vs.viewDecorator = d
}

// volumeViewDecorator 返回当前装饰器（未显式注入 → 内置 trusted 默认）。
func (vs *Set) volumeViewDecorator() VolumeViewDecorator {
	if d := vs.viewDecorator; d != nil {
		return d
	}
	return defaultVolumeViewDecorator
}

// VolumeView 返回指定卷名的按用途 FS 视图（U2 唯一取卷入口）。
//
// - 未知卷名 / 未装配 → (nil, false)（fail-closed）；
// - `secrets` use → (nil, false)（密钥视图不入通用表，R26 白名单维持）；
// - 已装配且 use 合法 → 经装配期装饰器（nil 则原样返回）给出视图。
func (vs *Set) VolumeView(name string, use ViewUse) (syncpkg.FS, bool) {
	// R16 P2 修复：use 必须显式 ∈ {ViewUser, ViewMeta}——此前只拒 secrets，拼写错误
	// （"User"/"user " 等）会静默走 default 分支等同 user 视图（按用途封装的承诺被弱化）。
	switch use {
	case ViewUser, ViewMeta:
		// ok
	default:
		return nil, false // 未知 use 与 secrets 一律 fail-closed（密钥视图不入通用表，R26）
	}
	sv, ok := vs.StorageVolume(name)
	if !ok || sv == nil {
		return nil, false
	}
	raw := sv.FS()
	if raw == nil {
		return nil, false
	}
	return vs.volumeViewDecorator()(name, use, raw), true
}
