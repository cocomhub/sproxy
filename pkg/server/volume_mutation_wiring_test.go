// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_mutation_wiring_test.go 锁定 V3 的**接线**承诺：装配完成后，本地卷根上发生结构性
// 变异（绕过 files.Service 写内核，如转存/归档落盘）时，搜索索引**自动**跟上——不需要调用方
// 记得登记（#874 事故族的根治）。
//
// 变异验证：注释掉 fileService() 里的 h.wireLocalVolumeMutations(svc) ⇒ 本用例红。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestWireLocalVolumeMutations_IndexFollowsBypassWrite 用真实卷根模拟旁路写：
// mkdir → 临时文件 → AtomicRename 落位（与转存/归档落盘同形），断言搜索可见。
func TestWireLocalVolumeMutations_IndexFollowsBypassWrite(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	svc := h.fileService() // 装配 + 接线（Once 内 wireLocalVolumeMutations）

	// 预热索引（空库全量构建）。
	if rr := serveSearch(t, svc, "seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}

	root := h.volSet.DefaultRoot()
	if root == nil {
		t.Fatal("默认卷根不可用")
	}
	if err := root.MkdirAll("anonymous/user/transfer", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tmp := "anonymous/user/transfer/.inflight-bypass.part"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, werr := f.Write([]byte("bypass content")); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	if rerr := root.AtomicRename(tmp, "anonymous/user/transfer/readme.md"); rerr != nil {
		t.Fatalf("AtomicRename: %v", rerr)
	}

	rr := serveSearch(t, svc, "readme")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Total int `json:"total"`
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if uerr := json.Unmarshal(rr.Body.Bytes(), &resp); uerr != nil {
		t.Fatalf("解析搜索响应: %v (%s)", uerr, rr.Body.String())
	}
	if resp.Total != 1 || len(resp.Files) != 1 || resp.Files[0].Name != "transfer/readme.md" {
		t.Fatalf("旁路写后应自动命中 transfer/readme.md（观察者未接线）, got total=%d files=%+v", resp.Total, resp.Files)
	}
}

// TestWireLocalVolumeMutations_IgnoresNonUserBuckets 保证 meta/version 等内部桶的高频写
// 不进用户视图（否则侧车写会污染搜索）。
func TestWireLocalVolumeMutations_IgnoresNonUserBuckets(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	svc := h.fileService()
	if rr := serveSearch(t, svc, "seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	root := h.volSet.DefaultRoot()
	if err := root.MkdirAll("anonymous/meta/state", 0o755); err != nil {
		t.Fatalf("MkdirAll meta: %v", err)
	}
	if rr := serveSearch(t, svc, "state"); rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d", rr.Code)
	}
	var resp struct {
		Total int `json:"total"`
	}
	if uerr := json.Unmarshal(serveSearch(t, svc, "state").Body.Bytes(), &resp); uerr != nil {
		t.Fatalf("解析: %v", uerr)
	}
	if resp.Total != 0 {
		t.Fatalf("meta 桶不应命中用户搜索, got total=%d", resp.Total)
	}
}

// serveSearch 以 anonymous 身份调用搜索（owner 经 ctx 注入，与生产路由同语义）。
func serveSearch(t *testing.T, svc interface {
	SearchFiles(http.ResponseWriter, *http.Request)
}, q string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/files/search?q="+q, nil)
	req = withActorContext(req, "anonymous")
	rr := httptest.NewRecorder()
	svc.SearchFiles(rr, req)
	return rr
}
