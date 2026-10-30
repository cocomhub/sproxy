// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/server/verify"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// verifyService 构造 checksum 巡检器（依赖倒置，R6）。
func (h *Handlers) verifyService() *verify.Service {
	return verify.New(verify.Deps{
		Logger:           h.logger,
		TenantFor:        h.tenantFor,
		ListTenantIDs:    h.listTenantIDs,
		LocateOwnerFile:  h.locateOwnerFileForVerify,
		ChecksumStoreFor: h.checksumStoreFor,
		Alert: func(ctx context.Context, owner, detail string) {
			if h.alertEngine != nil {
				h.alertEngine.OnChecksumMismatch(ctx, owner, 0, detail)
			}
		},
		Audit: func(ctx context.Context, action, objectType, object, result, detail string) {
			h.RecordAudit(ctx, AuditEvent{Action: action, ObjectType: objectType, Object: object, Result: result, Detail: detail})
		},
		Busy: &h.verifyBusy,
		Hook: h.verifyHook,
	})
}

// locateOwnerFileForVerify 适配 locateOwnerFile 为 verify.Deps 的窄签名。
func (h *Handlers) locateOwnerFileForVerify(owner, rel string) (*storage.Tenant, bool) {
	loc, ok := h.locateOwnerFile(owner, rel)
	if !ok || loc == nil {
		return nil, false
	}
	return loc.tenant, true
}
