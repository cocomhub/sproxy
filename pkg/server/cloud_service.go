// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cloud_service.go 是 `pkg/cloud` 云下载域的**装配适配**：把 Handlers 持有的装配项
// （容量账本、租户/校验和/配额解析、审计、日志）适配为 pkg/cloud 声明的窄接口，
// 并把云任务的错误映射回 HTTP 语义。
//
// 与 files_service.go 的分工相同：领域包不直接依赖子包 `pkg/storage/capacity`
// （门禁 R2 禁止跨域直连子包），类别常量在这里固定，领域侧只表达「云下载桶的预留/归还」。
//
// 注：`isStorageFull`（容量满仓判定）已在 cloud_download_handler.go——它需要同时认
// capacity 与 quota 两个哨兵，仍属装配层语义，不随领域搬迁。
package server

import (
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// cloudStorageManager 把 *capacity.StorageManager 适配为 cloud.StorageManager。
// 子包不得见 capacity.StorageCategory，故类别由本适配器固定为 CategoryCloud。
type cloudStorageManager struct{ m *capacity.StorageManager }

func (a cloudStorageManager) TryReserveCloud(n int64) error {
	return a.m.TryReserve(n, capacity.CategoryCloud)
}
func (a cloudStorageManager) ReleaseCloud(n int64) { a.m.Release(n, capacity.CategoryCloud) }
func (a cloudStorageManager) Usage() int64         { return a.m.Usage() }
func (a cloudStorageManager) MaxBytes() int64      { return a.m.MaxBytes() }
