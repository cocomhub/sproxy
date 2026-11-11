// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"
)

// resolveURL 处理 GET /api/resolve?url=...：通用 URL → 资源信息（大小/类型/文件清单）。
// 方法选 GET 的根因：本接口**只读**（不产生任何状态变更），GET 语义正确；同时避免
// R35-b（变更型路由必须登记业务审计）把只读查询误纳入审计动作。
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
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		sendJSONResponse(w, map[string]string{"error": "url 不能为空"}, http.StatusBadRequest)
		return
	}
	info, err := h.urlResolver.ResolveURL(r.Context(), target)
	if err != nil {
		// 扩展解析失败（分享不存在/网络错误）→ 502（上游资源问题），保留原因便于排查。
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadGateway)
		return
	}
	// **绝不回 200 + `null`**（修复 2026-11-11）：`json.Marshal(nil 指针)` 得到字面量 `null`，
	// 客户端 `json.Decode(&out)` 对 `null` **不报错**且保持零值 ⇒ 调用方看到"成功但字段全空"，
	// 真实故障被掩盖（实测 sdserver `/api/resolve` 全量 null ⇒ 误报"分享内无合格视频"）。
	// 这里 fail-closed 成 502 + 明确文案，保证客户端一定能区分"空"与"结果"。
	if info == nil {
		h.logger.Error("resolve 返回 nil 载荷（fail-closed 成 502，避免 200+null）",
			"handler", "resolveURL", "url", target)
		sendJSONResponse(w, map[string]string{"error": "解析返回空载荷"}, http.StatusBadGateway)
		return
	}
	sendJSONResponse(w, info, http.StatusOK)
}
