// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"github.com/cocomhub/sproxy/internal/httpjson"
)

func (h *Handlers) handleAIQuota(w http.ResponseWriter, r *http.Request) {
	ai := h.aiInsight
	if ai == nil || ai.Quota() == nil {
		writeAIError(w, http.StatusBadRequest, "AI 配额未启用")
		return
	}
	owner := ownerFromRequest(r)
	if o := r.URL.Query().Get("owner"); o != "" && owner != "" {
		// 管理员越权查询：仅当请求已认证且非 anonymous 才允许（简化：同 owner 隔离语义）
		if o != owner && owner != "admin" {
			writeAIError(w, http.StatusForbidden, "无权查看他人配额")
			return
		}
		owner = o
	}
	httpjson.Send(w, http.StatusOK, map[string]any{
		"owner": owner,
		"usage": ai.Quota().Usage(owner),
	})
}
