// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dav 是**服务端 WebDAV 挂载面**（server 作用域）：把租户 user 桶经
// pkg/gateway/webdav 暴露为 RFC 4918 端点，并在写/删/改名后联动 sidecar 元信息。
//
// 依赖经 Deps 注入（装配层构造），不反向依赖 pkg/server（R6）。
package dav

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// Deps 是 WebDAV 挂载面的装配依赖。
type Deps struct {
	// Logger 是业务日志器（nil → slog.Default）。
	Logger *slog.Logger
	// OwnerFromRequest 返回请求 ctx 中的操作主体（未认证返回空串）。
	OwnerFromRequest func(r *http.Request) string
	// Volumes 是卷集合（?volume= 解析、默认卷租户）。
	Volumes *registry.Set
	// TenantFor 解析 owner 的默认卷租户。
	TenantFor func(owner string) *storage.Tenant
	// DefaultVolumeAllows 判定默认卷是否允许该 owner（与文件 API 面同口径）。
	DefaultVolumeAllows func(owner string) bool
	// FileService 返回文件服务域实例（统一写内核；nil = 未装配，回退内层直写）。
	FileService func() *files.Service
	// Meta 是 sidecar 元信息策略（写后建/删后清/临时清扫）。
	Meta MetaPolicy
	// QuotaScopeFor 返回 owner 在 rel 上的配额 Scope（sidecar 释放）。
	QuotaScopeFor func(owner, rel string) *quota.Scope
	// WithAuditVolume 把本次请求作用的卷绑定进 ctx（审计第 6 维）。
	WithAuditVolume func(ctx context.Context, name string) context.Context
}

// MetaPolicy 是 sidecar 元信息策略的窄接口（装配层用 filesMetaPolicy 实现）。
type MetaPolicy interface {
	WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error
	SweepMetaTmp(root *storage.Root, mrel string)
}

// Service 是 WebDAV 挂载面。
type Service struct{ deps Deps }

// New 构造 WebDAV 挂载面。
func New(d Deps) *Service { return &Service{deps: d} }
