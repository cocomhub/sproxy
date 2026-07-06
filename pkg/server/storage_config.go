// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
)

// storageConfigHandler 处理 PUT /api/storage/config，运行时调整存储上限。
func (h *Handlers) storageConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		sendJSONResponse(w, map[string]string{"error": "method not allowed"}, http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 限 1 KiB

	var req struct {
		MaxStorageBytes int64 `json:"max_storage_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}

	if req.MaxStorageBytes < 0 {
		sendJSONResponse(w, map[string]string{"error": "max_storage_bytes must be non-negative"}, http.StatusBadRequest)
		return
	}

	// 更新 StorageManager 和 Config
	h.storageMgr.SetMaxBytes(req.MaxStorageBytes)
	cfg := h.cfgPtr.Load()
	cfg.MaxStorageBytes = req.MaxStorageBytes

	sendJSONResponse(w, map[string]any{
		"success":           true,
		"max_storage_bytes": req.MaxStorageBytes,
	}, http.StatusOK)
}
