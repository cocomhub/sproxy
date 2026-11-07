// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/server/ops"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// duResponse 是 GET /api/du 的响应体。
// data 为空表示成功但路径下无条目（或路径本身即空目录）；遍历 IO 错误返回
// success:false（fail-closed——不得把部分结果当完整结果）。
type duResponse struct {
	Success bool      `json:"success"`
	Message string    `json:"message,omitempty"`
	Data    *ops.Data `json:"data,omitempty"`
}

// duHandler 处理 GET /api/du?path=<rel>[&volume=<name>]：按目录递归统计
// （子目录数/文件数/字节）。owner 从认证派生；卷定位走 locateForRead
// （ACL 收口，未命中统一 404，不泄卷存在性）。path 缺省 = 当前卷根（user 桶）。
func (h *Handlers) duHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ActorFrom(r.Context()))
	tnt0 := h.tenantFor(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	remotePath := r.URL.Query().Get("path")
	// **卷 URI 语义**（用户口径 2026-10-14）：以 `/` 开头 = **卷根起算**（`du <卷>:/a/b`）⇒ 去掉前导
	// 斜杠后按 user 桶相对路径解析；空/`/` = 卷根。此前绝对路径被 `ValidateFilePath` 拒绝 ⇒
	// `du <卷>:/` 恒 400（实测）。
	remotePath = strings.TrimPrefix(strings.TrimSpace(remotePath), "/")
	var rel string
	if remotePath == "" {
		rel = tnt0.UserRoot()
	} else {
		if _, err := pathguard.ValidateFilePath(remotePath); err != nil {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
			return
		}
		var ok bool
		rel, ok = tnt0.UserRel(remotePath)
		if !ok {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
			return
		}
	}

	// **外部卷**（加密/远端卷：无本地 Root）：走**外部 FS** 递归统计。此前恒 404——`locateForRead`
	// 需要本地租户，而外部卷没有 ⇒ `du <外部卷>:/…` 不可用（实测）。
	if explicitVolEarly := r.URL.Query().Get("volume"); explicitVolEarly != "" {
		if v, ok := h.volSet.ByName(explicitVolEarly); ok && !h.volSet.HasLocalRoot(explicitVolEarly) {
			h.duExternalVolume(w, r, owner, v, remotePath)
			return
		}
	}

	// 卷定位（ACL 收口）：显式 ?volume= 只在指定卷定位；未命中 → 404
	// （与 download 一致，不泄卷存在性）。全视图未命中时仅当默认卷对
	// owner 授权才回落默认租户（与下载回落语义一致）。
	explicitVol := r.URL.Query().Get("volume")
	tnt := tnt0
	loc, found := h.locateForRead(owner, rel, explicitVol)
	switch {
	case found:
		tnt = loc.tenant
	case explicitVol != "", !h.defaultVolumeAllows(owner):
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	default:
		// 全视图未命中（默认卷回落）：仍以默认租户根 stat 探测路径存在性——
		// 不存在 → 404（与 download 回落语义一致，不泄卷存在性）。
		if _, serr := tnt.Root().Stat(rel); serr != nil {
			sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			return
		}
	}

	data, werr := ops.WalkRoot(tnt.Root(), rel)
	if werr != nil {
		h.log().WarnContext(r.Context(), "du: 遍历目录失败", "path", rel, "volume", tnt.ID, "error", werr)
		sendJSONResponse(w, duResponse{Success: false, Message: "遍历目录失败"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, duResponse{Success: true, Data: data}, http.StatusOK)
}

// duExternalVolume 统计**外部卷**（无本地 Root）子树：经同一可信链取外部 FS，用 `externalVolumeKey`
// 把用户可见路径翻译为卷内键（与 ListDirView/ExternalStat 同一解析），再递归列举统计。
// 语义与本地 `walkDu` 对齐（跳过 .__ 魔法目录与 checksums.json/LAYOUT_VERSION 元数据）。
func (h *Handlers) duExternalVolume(w http.ResponseWriter, r *http.Request, owner string, v volume.Volume, remotePath string) {
	fsys, ok := h.externalFSFor(v.Name)
	if !ok || !v.Authorize(owner) {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	// 用户可见路径 → 卷内键：`ResolveUserLocation` 是**唯一入口**（含路径安全校验；空路径 = user 桶根）。
	loc, kerr := v.ResolveUserLocation(owner, remotePath)
	if kerr != nil {
		sendJSONResponse(w, duResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	key := loc.FSPath()
	data, werr := ops.WalkFS(r.Context(), fsys, key)
	if werr != nil {
		h.log().WarnContext(r.Context(), "du: 遍历外部卷失败", "volume", v.Name, "path", remotePath, "error", werr)
		sendJSONResponse(w, duResponse{Success: false, Message: "遍历目录失败"}, http.StatusInternalServerError)
		return
	}
	data.Path = "/" + strings.TrimPrefix(remotePath, "/")
	sendJSONResponse(w, duResponse{Success: true, Data: data}, http.StatusOK)
}

// walkDuFS 递归统计外部 FS 上 key 子树的目录数/文件数/字节（与 walkDu 同语义）。
