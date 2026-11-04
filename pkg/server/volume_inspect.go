// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_inspect.go 暴露「真实存储审查」的只读 HTTP 入口（设计
// docs/designs/2026-11-04-secretdata-storage-inspection.md §5.2）。
//
// 本文件只做**薄编排**：解析卷 → 断言能力 → 调用 → 错误映射。布局解析的唯一实现在
// 掌握布局的卷里（当前 = pkg/volume/secretdata），此处不得复制任何布局知识。

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/internal/httpjson"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// msgNotInspectable 是该卷不支持审查时的固定提示（501）。
const msgNotInspectable = "该卷不支持真实存储审查（仅 secretdata 等实现该能力的卷）"

// volumeTraceHandler 处理 GET /api/volumes/{name}/trace?path=&disk=。
func (h *Handlers) volumeTraceHandler(w http.ResponseWriter, r *http.Request) {
	insp, ok := h.inspectorFor(w, r)
	if !ok {
		return
	}
	pathArg := r.URL.Query().Get("path")
	mode := inspectModeFrom(r)
	tr, err := insp.Trace(r.Context(), pathArg, mode)
	if err != nil {
		h.writeInspectError(w, r, "volume_trace", pathArg, err)
		return
	}
	httpjson.Send(w, http.StatusOK, struct {
		Trace *volume.PathTrace `json:"trace"`
	}{Trace: tr})
}

// volumeExplainHandler 处理 GET /api/volumes/{name}/explain?real=&disk=。
func (h *Handlers) volumeExplainHandler(w http.ResponseWriter, r *http.Request) {
	insp, ok := h.inspectorFor(w, r)
	if !ok {
		return
	}
	realArg := r.URL.Query().Get("real")
	if strings.TrimSpace(realArg) == "" {
		httpjson.SendError(w, http.StatusBadRequest, "real 参数必填（卷根相对路径）")
		return
	}
	mode := inspectModeFrom(r)
	ex, err := insp.Explain(r.Context(), realArg, mode)
	if err != nil {
		h.writeInspectError(w, r, "volume_explain", realArg, err)
		return
	}
	httpjson.Send(w, http.StatusOK, struct {
		Explain *volume.RealExplanation `json:"explain"`
	}{Explain: ex})
}

// inspectorFor 解析卷名 + ACL + 能力断言；失败时已写出响应。
func (h *Handlers) inspectorFor(w http.ResponseWriter, r *http.Request) (volume.StorageInspector, bool) {
	volName := strings.TrimSpace(r.PathValue("name"))
	owner := normalizeOwner(ownerFromRequest(r))
	if volName == "" {
		httpjson.SendError(w, http.StatusNotFound, "卷不存在")
		return nil, false
	}
	if h.volSet == nil {
		httpjson.SendError(w, http.StatusNotImplemented, msgNotInspectable)
		return nil, false
	}
	v, ok := h.volSet.ByName(volName)
	if !ok {
		httpjson.SendError(w, http.StatusNotFound, "卷不存在")
		return nil, false
	}
	if !v.Authorize(owner) {
		httpjson.SendError(w, http.StatusForbidden, "无权访问该卷")
		return nil, false
	}
	fsys, ok := h.externalFSFor(volName)
	if !ok || fsys == nil {
		httpjson.SendError(w, http.StatusNotFound, "卷不存在")
		return nil, false
	}
	// 能力断言必须穿透装饰链（Guard/Wrap/Capacity 等）——用受认可的 Innermost，
	// 不在业务代码里手写 Inner 链遍历（R22 门禁）。
	insp, ok := volume.AssertStorageInspector(syncpkg.Innermost(fsys))
	if !ok {
		httpjson.SendError(w, http.StatusNotImplemented, msgNotInspectable)
		return nil, false
	}
	return insp, true
}

// inspectModeFrom 解析 disk=1/true（默认索引模式）。
func inspectModeFrom(r *http.Request) volume.InspectMode {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("disk"))) {
	case "1", "true", "yes":
		return volume.InspectDisk
	default:
		return volume.InspectIndex
	}
}

// writeInspectError 映射审查错误到 HTTP 状态（越界 403 / 不存在 404 / 其它 500）。
func (h *Handlers) writeInspectError(w http.ResponseWriter, r *http.Request, handler, arg string, err error) {
	switch {
	case errors.Is(err, volume.ErrOutsideVolume):
		httpjson.SendError(w, http.StatusForbidden, "路径越出卷根")
	case errors.Is(err, fs.ErrNotExist):
		httpjson.SendError(w, http.StatusNotFound, "路径不存在")
	default:
		h.logger.Error("真实存储审查失败", "handler", handler, "volume", r.PathValue("name"), "arg", arg, "error", err)
		httpjson.SendError(w, http.StatusInternalServerError, "审查失败")
	}
}
