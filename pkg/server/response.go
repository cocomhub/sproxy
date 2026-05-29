// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
)

type UploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(response)
}
