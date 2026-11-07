// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// s3_wiring.go 是 S3 网关的**装配适配**：把 Handlers 的装配态映射为 s3.Deps，
// 让 pkg/server/s3 只依赖窄接口、不反向依赖装配层（R6）。
package server

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/server/s3"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// s3Service 构造 S3 网关（每次返回轻量包装；路由注册一次后由闭包持有）。
func (h *Handlers) s3Service() *s3.Service {
	return s3.New(s3.Deps{
		Logger:              h.logger,
		Cfg:                 h.cfgPtr,
		Volumes:             h.volSet,
		Credentials:         h.credentialRing,
		TenantFor:           h.tenantFor,
		VolumePoolForTenant: h.volumePoolForTenant,
		QuotaScopeFor:       h.quotaScopeFor,
		FileService:         h.fileService,
		Meta:                s3MetaPolicy{h: h},
		WithAuditVolume:     withAuditVolume,
	})
}

// s3MetaPolicy 把 sidecar 元信息策略适配为 s3.MetaPolicy（依赖倒置的装配侧实现）。
type s3MetaPolicy struct{ h *Handlers }

func (p s3MetaPolicy) WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error {
	return filesMetaPolicy(p).WriteMeta(ctx, owner, root, rel, extra)
}

func (p s3MetaPolicy) VerifyDownload(ctx context.Context, root *storage.Root, rel string, r files.SeekReadCloser) (files.SeekReadCloser, error) {
	return filesMetaPolicy(p).VerifyDownload(ctx, root, rel, r)
}
