// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_status_test.go 验证配额水位（roadmap P2 配额预警残余）：
//  1. quotaStatusOf 计算水位（MaxBytes<=0 → 0）。
//  2. /api/stats 认证响应含 quota 段（usage/max_bytes/watermark）。

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestQuotaStatusOf_NoLimit 不限配额 → watermark 0。
func TestQuotaStatusOf_NoLimit(t *testing.T) {
	t.Parallel()
	zero := quotaStatusOf(nil)
	if zero.Watermark != 0 || zero.MaxBytes != 0 {
		t.Fatalf("nil scope = %+v, want all zero", zero)
	}
}

// TestStats_QuotaSection 认证 /api/stats 含 quota 段。
func TestStats_QuotaSection(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.OwnerQuotas = map[string]ByteSize{testAccessKey: 1000}
	})
	defer cleanup()

	// 上传 100B 文件占配额。
	if st := uploadFileSigned(t, baseURL, "q.txt", make([]byte, 100)); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/stats", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if jerr := json.NewDecoder(resp.Body).Decode(&body); jerr != nil {
		t.Fatal(jerr)
	}
	quotaSec, ok := body["quota"].(map[string]any)
	if !ok {
		t.Fatalf("stats 应含 quota 段: %v", body)
	}
	if _, hasMax := quotaSec["max_bytes"]; !hasMax {
		t.Fatalf("quota 应含 max_bytes: %v", quotaSec)
	}
	wm, hasWm := quotaSec["watermark"].(float64)
	if !hasWm {
		t.Fatalf("quota 应含 watermark: %v", quotaSec)
	}
	// 上传 100B → 配额 1000 → watermark>0（变异「水位计算禁用」→ 0 红）。
	if wm <= 0 {
		t.Fatalf("上传后 watermark 应 > 0, got %v", wm)
	}
}
