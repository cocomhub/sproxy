// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package verify 是**全仓 checksum 巡检**（server 作用域）：重算 SHA-256 比对 per-tenant
// 台账、坏文件隔离、周期调度与告警联动。依赖经 Deps 注入，不反向依赖 pkg/server（R6）。
package verify

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// Deps 是巡检的装配依赖。
type Deps struct {
	// Logger 是业务日志器（nil → slog.Default）。
	Logger *slog.Logger
	// TenantFor 解析 owner 的默认卷租户。
	TenantFor func(owner string) *storage.Tenant
	// ListTenantIDs 返回全部租户 ID（空 = 磁盘扫描兜底）。
	ListTenantIDs func() []string
	// LocateOwnerFile 定位 rel 实际所在卷的租户（多卷路由）。
	LocateOwnerFile func(owner, rel string) (*storage.Tenant, bool)
	// ChecksumStoreFor 返回 owner 的 checksum 台账。
	ChecksumStoreFor func(owner string) checksum.ChecksumStoreIface
	// Audit 记录巡检动作。
	Audit func(ctx context.Context, action, objectType, object, result, detail string)
	// Busy 是单飞标记（并发巡检时跳过）。
	Busy *atomic.Bool
	// Hook 是测试注入钩子（生产恒 nil）。
	Hook func()
}

// Service 是 checksum 巡检器。
type Service struct{ d Deps }

// New 构造巡检器。
func New(d Deps) *Service { return &Service{d: d} }
