// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api.go 实现 backend 列表 API（V4）：GET /api/backends 返回已注册卷后端类型
// （registry.BackendTypes()），供 Web UI「我的用户卷」创建表单 type 下拉动态感知——
// 未来任何新 backend（S3/…）只 RegisterBackend 注册即自动出现在前端，无需改前端代码。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

type backendsListResponse struct {
	Backends []string `json:"backends"`
}

// backendsHandler 处理 GET /api/backends。返回 registry 已注册后端类型列表（动态）。
// 不依赖 volSet（注册表是包级状态，与卷集合装配无关）——未装配卷集合也返回已注册类型。
func (h *Handlers) backendsHandler(w http.ResponseWriter, r *http.Request) {
	sendJSONResponse(w, backendsListResponse{Backends: registry.BackendTypes()}, http.StatusOK)
}
