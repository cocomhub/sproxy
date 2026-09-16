// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// handlers_endpoints.go 是**基础端点与自愈循环**：Handler（返回装配好的 http.Handler）、
// livez（纯进程存活探针）、readyz（依赖就绪探针：未就绪时 503）、healthz（兼容别名，
// 复用 readyz 语义）、versionHandler、webRedirect（/ → /ui/），以及上传残留文件的
// 周期清理（cleanupUploadingFilesLoop / Pass）。
//
// 拆分说明见 handlers.go 顶部。

package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
)

// Handler 返回包装了 metricsMiddleware 的 HTTP handler，用于 http.Server.Handler。
func (h *Handlers) Handler() http.Handler {
	return h.handler
}

// livez 是纯进程存活探针：不访问任何外部依赖，进程活着即 200 OK。
func (h *Handlers) livez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// readinessCheck 执行就绪检查：探活 per-tenant UploadStore，任一已创建的 store 停止
// 即判定不健康。返回 (healthy bool, detail string)。
func (h *Handlers) readinessCheck() (bool, string) {
	h.tenantMu.Lock()
	stores := make([]*files.UploadStore, 0, len(h.uploadStores))
	for _, us := range h.uploadStores {
		if us != nil {
			stores = append(stores, us)
		}
	}
	h.tenantMu.Unlock()
	for _, us := range stores {
		if err := us.Health(); err != nil {
			return false, "UploadStore: " + err.Error()
		}
	}
	return true, "OK"
}

// readyz 是就绪探针：依赖未就绪（任一 store 停止）返回 503。
func (h *Handlers) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	healthy, detail := h.readinessCheck()
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(detail))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// healthz 保持原语义（兼容现有监控），实现复用 readyz 的就绪检查。
func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	h.readyz(w, r)
}

func (h *Handlers) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Version: %s\nBuildAt: %s\n", h.version, h.buildAt)
}

func (h *Handlers) webRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}

// cleanupUploadingFilesLoop 定期清理 uploadingFiles 中已过期（不存在对应 session）的条目。
// 作为 goroutine 在 RegisterRoutes 中启动，由 Close() 通过关闭 uploadingStop 停止；单次清理
// 委托 cleanupUploadingFilesPass（独立可测）。
func (h *Handlers) cleanupUploadingFilesLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.uploadingStop:
			return
		case <-ticker.C:
			h.cleanupUploadingFilesPass()
		}
	}
}

// cleanupUploadingFilesPass 执行一轮 uploadingFiles 过期清理。
// 锁标记条目（upload/move/txn，见 isUploadingLockMarker）都无对应 session，直接跳过
// （若把 "move" 当 upload_id 查 GetSession("move")==nil 会误删锁条目：超 10 分钟的长
// move/delete/restore/complete 持锁被清理 → 同 rel 并发操作越过锁，T6c 修复轮建议 1）。
func (h *Handlers) cleanupUploadingFilesPass() {
	h.uploadingFiles.Range(func(key, value any) bool {
		filename, ok := key.(string)
		if !ok {
			return true
		}
		uploadID, ok := value.(string)
		if !ok {
			return true
		}
		if isUploadingLockMarker(uploadID) {
			return true
		}
		// 分块上传条目 value 为 upload_id（裸 id）。uploadingFiles key 为
		// <tnt.ID>\x00<rel>（chunked init 与 upload handler 同格式），从 key 解析
		// 租户名取 per-tenant store 判断会话是否已不存在（则清理过期条目）。
		owner := ""
		if before, _, ok0 := strings.Cut(filename, "\x00"); ok0 {
			owner = before
		}
		if us := h.uploadStoreFor(owner); us != nil && us.GetSession(uploadID) == nil {
			h.uploadingFiles.Delete(filename)
		}
		return true
	})
}
