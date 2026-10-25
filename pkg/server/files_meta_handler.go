// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_meta_handler.go 实现 `PUT /api/files/meta`（方案②：已上传文件的元数据补录）。
//
// 请求：PUT /api/files/meta?filename=<rel>
//      body: {"extra": {"code":"MOVIE-001","maker":"STUDIO X", ...}}
// 响应：{"success":true,"message":"..."}
//
// 语义：经 pkg/files.Service.UpdateMeta → 卷 FS 的 UpdateMetaExtra（旁路读-校验-合并-写
// sidecar，不重算内容哈希）。仅对**已有 sidecar** 的文件生效（无基线拒绝，fail-closed）。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// ActionFileMetaUpdate 是「元数据补录」的业务审计动作（R35-b 登记）。
const ActionFileMetaUpdate = "file_meta_update"

// metaUpdateService 是元数据补录依赖的最小能力面（便于测试注入替身）。
type metaUpdateService interface {
	UpdateMeta(ctx context.Context, input files.UpdateMetaInput) error
	RecordFileAudit(ctx context.Context, action, object, result, detail string)
}

// metaUpdater 返回元数据补录服务（测试可经 metaSvcForTest 注入替身）。
func (h *Handlers) metaUpdater() metaUpdateService {
	if h.metaSvcForTest != nil {
		return h.metaSvcForTest
	}
	return h.fileService()
}

func (h *Handlers) updateFileMeta(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimSpace(r.URL.Query().Get("filename"))
	if filename == "" {
		sendJSONResponse(w, map[string]string{"error": "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	extra := meta.TrimExtra(req.Extra)
	if err := meta.ValidateExtra(extra); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	if len(extra) == 0 {
		sendJSONResponse(w, map[string]string{"error": "extra 不能为空"}, http.StatusBadRequest)
		return
	}
	err := h.metaUpdater().UpdateMeta(r.Context(), files.UpdateMetaInput{
		Owner:      ownerFromRequest(r),
		RemotePath: filename,
		Extra:      extra,
	})
	if err != nil {
		switch {
		case errors.Is(err, files.ErrMetaUnavailable):
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusServiceUnavailable)
		default:
			// 底层校验失败（并发覆盖 / 无 sidecar / 不支持）→ 409（状态冲突，可重试）
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
		}
		return
	}
	// 业务审计（R35-b）：写面变更型路由留痕（动作 + 目标 + 成功）。
	h.metaUpdater().RecordFileAudit(r.Context(), ActionFileMetaUpdate, filename, "success", "meta extra update")
	sendJSONResponse(w, map[string]any{"success": true, "message": "meta extra 已更新"}, http.StatusOK)
}
