// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// checksum_api.go 是 checksum 台账的**运维修复面**（设计 2026-10-28-volume-state-maintenance
// §15.1 步骤 ②）：`POST /api/checksums/rebuild?owner=<owner>`。
//
// 为什么需要显式入口：台账为空时 `List/Search` 会**自动**从对象 meta 一次性重建（自愈读），
// 但「台账存在却部分缺失」（被裁剪、部分条目丢失、跨版本迁移遗留）不会触发自动重建——
// 这种情况需要运维显式触发，且回填结果可观测（返回条数）。
package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/cocomhub/sproxy/pkg/accesskey"

	"github.com/cocomhub/sproxy/pkg/files"
)

// auditActionChecksumRebuild 是重建端点的业务审计动作（R35-b 登记：action=checksum_rebuild）。
const auditActionChecksumRebuild = "checksum_rebuild"

// checksumRebuildResponse 是重建结果。
type checksumRebuildResponse struct {
	Success bool   `json:"success"`
	Owner   string `json:"owner"`
	Entries int    `json:"entries"`
	Message string `json:"message,omitempty"`
}

// rebuildChecksumsHandler 从对象 meta sidecar 重建指定 owner 的 checksum 台账。
// owner 缺省 = 请求主体（未认证 → anonymous）；结果恒可观测（entries = 回填条数）。
func (h *Handlers) rebuildChecksumsHandler(w http.ResponseWriter, r *http.Request) {
	actor := normalizeOwner(ownerFromRequest(r))
	owner := actor
	// 跨租户修复仅限 admin（否则任何已认证主体都能触发他租户的台账重建 = 跨租户写）。
	if q := r.URL.Query().Get("owner"); q != "" {
		target := normalizeOwner(q)
		if target != actor {
			if err := requireRole(PrincipalFrom(r.Context()), string(accesskey.RoleAdmin)); err != nil {
				sendJSONResponse(w, checksumRebuildResponse{Success: false, Owner: target, Message: "仅 admin 可指定 owner"}, http.StatusForbidden)
				return
			}
		}
		owner = target
	}
	n, err := h.fileService().RebuildChecksums(r.Context(), owner)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, files.ErrChecksumStoreUnavailable) {
			status = http.StatusServiceUnavailable
		}
		h.logger.Warn("checksum 台账重建失败", "owner", owner, "error", err.Error())
		sendJSONResponse(w, checksumRebuildResponse{Success: false, Owner: owner, Message: "重建失败: " + err.Error()}, status)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: auditActionChecksumRebuild, ObjectType: "checksum", Object: owner, Result: AuditResultSuccess,
		Detail: "entries=" + strconv.Itoa(n),
	})
	sendJSONResponse(w, checksumRebuildResponse{Success: true, Owner: owner, Entries: n}, http.StatusOK)
}
