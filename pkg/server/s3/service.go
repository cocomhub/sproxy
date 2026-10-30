// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package s3 是 **S3 兼容网关**（server 作用域）：SigV4 验签、对象/桶读写、
// multipart 上传（在途 part 双账本）、列表与 Abort。
//
// 依赖只经 Deps 注入（装配层构造），不反向依赖 pkg/server（R6）；所有对外能力
// 收敛在 Service 上，装配层只需 `s3.New(deps).Handler`。
package s3

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// Deps 是 S3 网关的装配依赖（字段名即注入点；nil 语义由各消费点自行判空）。
type Deps struct {
	// Logger 是业务日志器（nil → slog.Default）。
	Logger *slog.Logger
	// Cfg 是运行时配置指针（热更新可见；nil → 空配置零回归）。
	Cfg *atomic.Pointer[config.Config]
	// Volumes 是卷集合（命名桶解析、默认卷租户）。
	Volumes *registry.Set
	// Credentials 是 Sproxysig 凭据 Ring（SigV4 AK→SK）。
	Credentials *accesskey.Ring
	// TenantFor 解析 owner 的默认卷租户。
	TenantFor func(owner string) *storage.Tenant
	// VolumePoolForTenant 返回租户所在卷的容量池。
	VolumePoolForTenant func(tnt *storage.Tenant) *quota.Pool
	// QuotaScopeFor 返回 owner 在 rel 上的配额 Scope（chunk 桶在途 part 记账）。
	QuotaScopeFor func(owner, rel string) *quota.Scope
	// FileService 返回文件服务域实例（统一写内核；nil = 未装配，回退直写）。
	FileService func() *files.Service
	// Meta 是 sidecar 元信息策略（写后建/下载前校验；nil = 不校验零回归）。
	Meta MetaPolicy
	// WithAuditVolume 把「本次请求作用的卷」绑定进 ctx（审计第 6 维）。
	WithAuditVolume func(ctx context.Context, name string) context.Context
}

// Service 是 S3 兼容网关。
type Service struct{ d Deps }

// New 构造 S3 网关（deps 由装配层注入）。
func New(d Deps) *Service { return &Service{d: d} }

// MetaPolicy 是文件 sidecar 元信息的**窄接口**（装配层用 pkg/server 的 filesMetaPolicy 实现）：
// S3 写路径需要「写后建 sidecar」与「下载前校验」，但不依赖装配层类型（R6 依赖倒置）。
type MetaPolicy interface {
	WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error
	VerifyDownload(ctx context.Context, root *storage.Root, rel string, r files.SeekReadCloser) (files.SeekReadCloser, error)
}
