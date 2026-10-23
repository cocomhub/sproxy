// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
)

// maxResolveBodyBytes 是 /api/resolve 请求体上限（URL 文本，1 MiB 足够）。
const maxResolveBodyBytes = 1 << 20

// resolveURL 处理 POST /api/resolve：通用 URL → 资源信息（大小/类型/文件清单）。
//
// 设计（用户裁定 2026-10-23）：**统一解析入口，扩展按 URL 自动识别**
// （pikpak 分享等由 downloader.Registry 的 URLResolver 扩展提供），
// 而非为每种分享类型新增专用端点（收敛原则：新增类型只需注册扩展）。
// 未识别 URL 回落通用结果（文件名由路径推导、大小未知）。
func (h *Handlers) resolveURL(w http.ResponseWriter, r *http.Request) {
	if h.urlResolver == nil {
		sendJSONResponse(w, map[string]string{"error": "URL 解析接口未装配（pikpak 扩展未注册）"}, http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxResolveBodyBytes)
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		sendJSONResponse(w, map[string]string{"error": "url 不能为空"}, http.StatusBadRequest)
		return
	}
	info, err := h.urlResolver.ResolveURL(r.Context(), req.URL)
	if err != nil {
		// 扩展解析失败（分享不存在/网络错误）→ 502（上游资源问题），保留原因便于排查。
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadGateway)
		return
	}
	sendJSONResponse(w, info, http.StatusOK)
}
