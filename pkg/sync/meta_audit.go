// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// MetaAuditReport 是**只读**元数据审计结果（清理墓碑 / 排查损坏前先确认元数据层健康）。
//
// 背景（2026-11-20）：清理孤儿分块后，容器里仍残留大量历史 file meta（墓碑）；
// 跑 `GC()` 前必须先确认「存活 meta 引用的分块都在、meta 可解密」，否则可能误删。
type MetaAuditReport struct {
	// Containers 是扫描的容器数。
	Containers int `json:"containers"`
	// FileMetas 是读到的 file meta blob 总数（含墓碑）。
	FileMetas int `json:"file_metas"`
	// Live 是存活文件数（非墓碑）。
	Live int `json:"live"`
	// Tombstones 是墓碑数（`Deleted=true`，可由 GC 清理）。
	Tombstones int `json:"tombstones"`
	// Unreadable 是**无法读/解密**的 file meta 数（数据损坏或密钥错误信号）。
	Unreadable int `json:"unreadable"`
	// DirMetas / DirUnreadable 是目录 meta 的总数与不可解密数。
	DirMetas      int `json:"dir_metas"`
	DirUnreadable int `json:"dir_unreadable"`
	// MissingChunks 是「存活 meta 引用、但容器中不存在」的分块数（**数据缺失，危险**）。
	MissingChunks int `json:"missing_chunks"`
	// MissingSamples 是缺失分块的样例（最多 20 条，`容器/名字`）。
	MissingSamples []string `json:"missing_samples,omitempty"`
}

// MetaAuditor 是**可选能力接口**：只读审计卷的元数据层。
//
// 与 `OrphanAuditor` 一样，**所有透明包装器必须转发**（已纳入 `DecoratorCapabilities`
// ⇒ 漏转发编译期失败）。
type MetaAuditor interface { // NOSONAR: S8196 — 能力接口，设计保留
	AuditMeta(ctx context.Context) (MetaAuditReport, error)
}

// AssertMetaAuditor 查询 fs 是否具备「元数据审计」能力（未实现 ⇒ nil）。
func AssertMetaAuditor(fs FS) MetaAuditor {
	if fs == nil {
		return nil
	}
	if a, ok := fs.(MetaAuditor); ok {
		return a
	}
	return nil
}
