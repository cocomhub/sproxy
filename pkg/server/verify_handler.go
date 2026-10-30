// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// verify_handler.go 是 checksum 巡检的 **HTTP 面**（POST /api/verify）。
// 巡检实现已下沉 pkg/server/verify；本文件只做请求解析、审计/告警与响应写出。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// verifyHandler 处理 POST /api/verify。
// 请求体 JSON：{"volume":"","force":true}（volume 空 = 全部卷视图；force 保留给
// 客户端语义扩展，当前 busy 恒返回 Concurrent 报告不堆叠）。
// owner 从认证上下文派生（未认证 → anonymous）；空 owner（admin）→ 全租户。
func (h *Handlers) verifyHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Volume string `json:"volume"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体解析失败"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	owner := ownerFromRequest(r)
	rep := h.verifyService().VerifyOnce(r.Context(), req.Volume, owner)

	// 结果写审计（定时巡检同款；detail 汇总计数）。
	detail := fmt.Sprintf("volume=%s ok=%d mismatched=%d missing=%d errors=%d skipped=%d",
		req.Volume, rep.Ok, len(rep.Mismatched), len(rep.Missing), len(rep.Errors), rep.Skipped)
	result := AuditResultSuccess
	if len(rep.Mismatched) > 0 || len(rep.Missing) > 0 || len(rep.Errors) > 0 {
		result = AuditResultError
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "verify", ObjectType: "volume", Object: req.Volume,
		Result: result, Detail: detail,
	})

	// 告警联动：不一致（mismatched+missing）> 0 → alertEngine 挂点（source=checksum_mismatch）。
	if len(rep.Mismatched)+len(rep.Missing) > 0 && h.alertEngine != nil {
		h.alertEngine.OnChecksumMismatch(r.Context(), normalizeOwner(owner),
			len(rep.Mismatched)+len(rep.Missing), detail)
	}

	sendJSONResponse(w, rep, http.StatusOK)
}
