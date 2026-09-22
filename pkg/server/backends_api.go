// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api.go 实现 backend 列表 API（V4）：GET /api/backends 返回已注册卷后端类型
// （registry.BackendTypes()），供 Web UI「我的用户卷」创建表单 type 下拉动态感知——
// 未来任何新 backend（S3/…）只 RegisterBackend 注册即自动出现在前端，无需改前端代码。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/volume"
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

// backendPresignHandler 处理 POST /api/backends/{type}/presign?path=&method=&expires=：
// 按 type 从注册表构造临时后端 → 断言 Presigner → 返回签名 URL（签名 v4 直传）。
// 未注册 type → 404；后端不支持 Presign → 405；缺 path / method 非法 → 400。
func (h *Handlers) backendPresignHandler(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path 必填（卷内相对路径）", http.StatusBadRequest)
		return
	}
	method := strings.ToUpper(r.URL.Query().Get("method"))
	if method != http.MethodPut && method != http.MethodGet {
		http.Error(w, "method 需为 PUT 或 GET", http.StatusBadRequest)
		return
	}
	expires := int64(0)
	if es := r.URL.Query().Get("expires"); es != "" {
		n, err := strconv.ParseInt(es, 10, 64)
		if err != nil || n <= 0 {
			http.Error(w, "expires 需为正整数秒", http.StatusBadRequest)
			return
		}
		expires = n
	}
	be, err := registry.NewBackend(r.Context(), volume.Volume{Type: typ})
	if err != nil {
		http.Error(w, "后端类型未注册: "+typ, http.StatusNotFound)
		return
	}
	defer be.Close()
	p, ok := be.(registry.Presigner)
	if !ok {
		http.Error(w, "后端不支持预签名直传: "+typ, http.StatusMethodNotAllowed)
		return
	}
	u, err := p.PresignedURL(r.Context(), path, method, expires)
	if err != nil {
		http.Error(w, "预签名失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]string{"url": u}, http.StatusOK)
}

// backendPresignCompleteHandler 处理 POST /api/backends/{type}/presign/complete?path=：
// 客户端直传完成后登记——服务端确认对象已存在（sync.FS.Stat）。
// 未注册 type → 404；对象不存在 → 404（fail-closed）。
func (h *Handlers) backendPresignCompleteHandler(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path 必填（卷内相对路径）", http.StatusBadRequest)
		return
	}
	be, err := registry.NewBackend(r.Context(), volume.Volume{Type: typ})
	if err != nil {
		http.Error(w, "后端类型未注册: "+typ, http.StatusNotFound)
		return
	}
	defer be.Close()
	fs := be.FS()
	if fs == nil {
		http.Error(w, "后端无文件视图", http.StatusInternalServerError)
		return
	}
	if _, err := fs.Stat(r.Context(), path); err != nil {
		http.Error(w, "对象不存在（直传未完成或路径错误）", http.StatusNotFound)
		return
	}
	sendJSONResponse(w, map[string]string{"ok": "registered"}, http.StatusOK)
}
