// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"encoding/json"
	"io"
	"net/http"
)

// jsonDecode 解码 JSON 请求体（限长 1KB——登录请求体极小）。
func jsonDecode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, r.Body)
	return nil
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
