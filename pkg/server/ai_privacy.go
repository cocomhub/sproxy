// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func (h *Handlers) handleAIPrivacy(w http.ResponseWriter, r *http.Request) {
	p := h.aiPrivacy
	if p == nil || !p.Enabled() {
		writeAIError(w, http.StatusBadRequest, "AI 隐私未启用")
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))
	if p.Root() == nil {
		writeAIError(w, http.StatusInternalServerError, "存储不可用")
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled":    true,
		"key_source": "volume-master-key",
		"artifacts":  p.List(p.Root(), owner),
	})
}

// handleAIPrivacyPurge 是 POST /api/ai/privacy/purge（删除 owner 全部 AI 派生数据；
// 用户行使删除权；审计 ai.privacy_purge）。
func (h *Handlers) handleAIPrivacyPurge(w http.ResponseWriter, r *http.Request) {
	p := h.aiPrivacy
	if p == nil || !p.Enabled() {
		writeAIError(w, http.StatusBadRequest, "AI 隐私未启用")
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))
	if p.Root() == nil {
		writeAIError(w, http.StatusInternalServerError, "存储不可用")
		return
	}
	n, err := p.PurgeOwner(p.Root(), owner)
	if err != nil {
		writeAIError(w, http.StatusInternalServerError, "清除失败: "+err.Error())
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "ai.privacy_purge", ObjectType: "privacy", Object: owner,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("deleted=%d", n),
	})
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": n})
}
