// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// quota_test.go —— 配额查询**联邦化**的 SDK 侧（片 5 读侧）：`GET /api/quota`。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetFederatedQuota_ParsesPerVolumeView 钉住 SDK 能取到**跨卷**视图（含逐卷明细）。
//
// **判别力** ✓：断言①请求路径恰为 `/api/quota`（若实现走 `/api/stats` ⇒ 红）；
// ②逐卷明细被解析（若只取总量而丢 volumes ⇒ 红）；③`partial`/`unlimited` 语义字段被解析
// （若类型漏字段 ⇒ 红）。
func TestGetFederatedQuota_ParsesPerVolumeView(t *testing.T) {
	t.Parallel()
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner":"alice","used":3000,"capacity":8192,` +
			`"unlimited":false,"partial":false,"volumes":[` +
			`{"volume":"v1","kind":"storage","used":1000,"capacity":4096,"has_quota":true},` +
			`{"volume":"v2","used":2000,"capacity":4096,"has_quota":true}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := NewFileClient(srv.URL).GetFederatedQuota(context.Background())
	if err != nil {
		t.Fatalf("GetFederatedQuota: %v", err)
	}
	if gotPath != "/api/quota" {
		t.Fatalf("请求路径=%q，want /api/quota（联邦视图专用端点）", gotPath)
	}
	if got.Owner != "alice" || got.Used != 3000 || got.Capacity != 8192 {
		t.Fatalf("汇总字段错: %+v", got)
	}
	if len(got.Volumes) != 2 {
		t.Fatalf("逐卷明细数=%d，want 2（须保留 volumes 供诊断）", len(got.Volumes))
	}
	byName := map[string]VolumeQuota{}
	for _, v := range got.Volumes {
		byName[v.Volume] = v
	}
	if byName["v1"].Used != 1000 || !byName["v1"].HasQuota || byName["v1"].Kind != "storage" {
		t.Fatalf("v1 明细错: %+v", byName["v1"])
	}
	if byName["v2"].Used != 2000 {
		t.Fatalf("v2 明细错: %+v", byName["v2"])
	}
}

// TestGetFederatedQuota_PartialAndUnlimitedSemantics 钉住「未知 ≠ 0」与「上限 0 = 不限」
// 在 SDK 侧**不丢语义**（漏字段会让 CLI 把未知当完整值 ✗）。
func TestGetFederatedQuota_PartialAndUnlimitedSemantics(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owner":"alice","used":500,"capacity":0,` +
			`"unlimited":true,"partial":true,"volumes":[` +
			`{"volume":"v1","used":500,"capacity":0,"has_quota":true},` +
			`{"volume":"v2","used":0,"capacity":0,"has_quota":false}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := NewFileClient(srv.URL).GetFederatedQuota(context.Background())
	if err != nil {
		t.Fatalf("GetFederatedQuota: %v", err)
	}
	if !got.Partial {
		t.Fatal("partial 必须被解析（未知部分未计入 ⇒ 消费方须显式处置）")
	}
	if !got.Unlimited {
		t.Fatal("unlimited 必须被解析（上限 0 = 不限，不是零配额）")
	}
	var v2 *VolumeQuota
	for i := range got.Volumes {
		if got.Volumes[i].Volume == "v2" {
			v2 = &got.Volumes[i]
		}
	}
	if v2 == nil || v2.HasQuota {
		t.Fatalf("v2 无每用户权威 ⇒ has_quota 必须 false: %+v", v2)
	}
}

// TestGetFederatedQuota_ServerErrorIsReported 钉住错误**不吞**（服务端 5xx ⇒ 返回错误）。
func TestGetFederatedQuota_ServerErrorIsReported(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := NewFileClient(srv.URL).GetFederatedQuota(context.Background()); err == nil {
		t.Fatal("5xx 必须返回错误（禁静默零值）")
	}
	// 顺带钉住 JSON 可往返（类型可被标准库编码，便于 CLI --json 直接输出）。
	if _, err := json.Marshal(FederatedQuota{}); err != nil {
		t.Fatalf("FederatedQuota 不可 JSON 编码: %v", err)
	}
}
