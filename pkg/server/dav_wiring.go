// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// dav_wiring.go 是 WebDAV 挂载面的装配适配（依赖倒置，R6）。
package server

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/server/dav"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// davService 构造 WebDAV 挂载面。
func (h *Handlers) davService() *dav.Service {
	return dav.New(dav.Deps{
		Logger:              h.logger,
		OwnerFromRequest:    ownerFromRequest,
		Volumes:             h.volSet,
		TenantFor:           h.tenantFor,
		DefaultVolumeAllows: h.defaultVolumeAllows,
		FileService:         h.fileServiceIfReady,
		Meta:                davMetaPolicy{h: h},
		QuotaScopeFor:       h.quotaScopeFor,
		WithAuditVolume:     withAuditVolume,
	})
}

// davMetaPolicy 把 sidecar 策略适配为 dav.MetaPolicy。
type davMetaPolicy struct{ h *Handlers }

func (p davMetaPolicy) WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error {
	return filesMetaPolicy(p).WriteMeta(ctx, owner, root, rel, extra)
}

func (p davMetaPolicy) SweepMetaTmp(root *storage.Root, mrel string) {
	filesMetaPolicy(p).sweepMetaTmp(root, mrel)
}
