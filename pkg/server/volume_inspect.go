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
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/internal/httpjson"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// msgNotInspectable 是该卷不支持审查时的固定提示（501）。
const msgNotInspectable = "该卷不支持真实存储审查（仅 secretdata 等实现该能力的卷）"

// volumeTraceHandler 处理 GET /api/volumes/{name}/trace?path=&disk=。
func (h *Handlers) volumeTraceHandler(w http.ResponseWriter, r *http.Request) {
	insp, ok := h.inspectorFor(w, r)
	if !ok {
		return
	}
	pathArg, ok := h.traceKeyFor(w, r)
	if !ok {
		return
	}
	mode := inspectModeFrom(r)
	depth := inspectDepthFrom(r)
	tree, hasTree := syncpkg.AssertTreeInspector(insp)
	var tr *syncpkg.PathTrace
	var err error
	if depth > 1 && hasTree {
		// 子树：子项按**索引**展示（不逐节点做 disk 探测，避免一次请求打爆后端）；
		// disk 的磁盘核验语义只作用于根节点（见 docs/api.md）。
		tr, err = tree.TraceTree(r.Context(), pathArg, depth, mode)
	} else {
		tr, err = insp.Trace(r.Context(), pathArg, mode)
	}
	if err != nil {
		h.writeInspectError(w, r, "volume_trace", pathArg, err)
		return
	}
	httpjson.Send(w, http.StatusOK, struct {
		Trace *syncpkg.PathTrace `json:"trace"`
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
		Explain *syncpkg.RealExplanation `json:"explain"`
	}{Explain: ex})
}

// traceKeyFor 把「用户视角路径」映射为卷内键空间（`<owner>/<bucket>/<rel>`）。
//
// 为什么必须映射：封装/加密卷的内部索引键含 owner/bucket 前缀（如
// `ak-xxx/user/kv/a.bin`），而调用方只认 `kv/a.bin`；直接拼会把「用户路径」当键查 ⇒ 全部 404
// （真实 baidu 卷实测）。空 path = **卷根**（不映射：卷根不属于任何用户桶）。
func (h *Handlers) traceKeyFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	pathArg := strings.TrimSpace(r.URL.Query().Get("path"))
	if pathArg == "" {
		return "", true
	}
	if h.volSet == nil {
		httpjson.SendError(w, http.StatusNotFound, "卷不存在")
		return "", false
	}
	v, ok := h.volSet.ByName(strings.TrimSpace(r.PathValue("name")))
	if !ok {
		httpjson.SendError(w, http.StatusNotFound, "卷不存在")
		return "", false
	}
	loc, lerr := v.ResolveUserLocation(normalizeOwner(ownerFromRequest(r)), pathArg)
	if lerr != nil {
		httpjson.SendError(w, http.StatusBadRequest, "路径非法")
		return "", false
	}
	return loc.FSPath(), true
}

// inspectorFor 解析卷名 + ACL + 能力断言；失败时已写出响应。
func (h *Handlers) inspectorFor(w http.ResponseWriter, r *http.Request) (syncpkg.StorageInspector, bool) {
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
	insp, ok := syncpkg.AssertStorageInspector(syncpkg.Innermost(fsys))
	if !ok {
		httpjson.SendError(w, http.StatusNotImplemented, msgNotInspectable)
		return nil, false
	}
	return insp, true
}

// inspectDepthFrom 解析 depth（默认 1；超过 MaxTreeDepth 按上限处理）。
func inspectDepthFrom(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("depth"))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	if n > syncpkg.MaxTreeDepth {
		return syncpkg.MaxTreeDepth
	}
	return n
}

// inspectModeFrom 解析 disk=1/true（默认索引模式）。
func inspectModeFrom(r *http.Request) syncpkg.InspectMode {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("disk"))) {
	case "1", "true", "yes":
		return syncpkg.InspectDisk
	default:
		return syncpkg.InspectIndex
	}
}

// writeInspectError 映射审查错误到 HTTP 状态（越界 403 / 不存在 404 / 其它 500）。
func (h *Handlers) writeInspectError(w http.ResponseWriter, r *http.Request, handler, arg string, err error) {
	switch {
	case errors.Is(err, syncpkg.ErrOutsideVolume):
		httpjson.SendError(w, http.StatusForbidden, "路径越出卷根")
	case errors.Is(err, fs.ErrNotExist):
		httpjson.SendError(w, http.StatusNotFound, "路径不存在")
	default:
		h.logger.Error("真实存储审查失败", "handler", handler, "volume", r.PathValue("name"), "arg", arg, "error", err)
		httpjson.SendError(w, http.StatusInternalServerError, "审查失败")
	}
}
