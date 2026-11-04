// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// volume_inspect_test.go 锁审查 API 的客户端契约（URL/参数/错误映射）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

func TestVolumeTrace_QueryAndDecode(t *testing.T) {
	t.Parallel()
	var gotPath, gotDisk string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		gotDisk = r.URL.Query().Get("disk")
		if r.URL.Path != "/api/volumes/vault/trace" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trace":{"schema_version":1,"mode":"disk","rel":"a/b.txt","kind":"file","size":3,` +
			`"chunks":[{"index":0,"blob":{"real_path":"c/x","bytes":9},"blocklets":1}]}}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewFileClient(srv.URL, WithHTTPClient(srv.Client()))
	tr, err := svc.VolumeTrace(context.Background(), "vault", "a/b.txt", true)
	if err != nil {
		t.Fatalf("VolumeTrace: %v", err)
	}
	if gotPath != "a/b.txt" || gotDisk != "1" {
		t.Fatalf("query = path=%q disk=%q", gotPath, gotDisk)
	}
	if tr.Rel != "a/b.txt" || tr.Mode != "disk" || len(tr.Chunks) != 1 {
		t.Fatalf("解码异常: %+v", tr)
	}
}

func TestVolumeExplain_DecodesOwnerAndAnomalies(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes/vault/explain" || r.URL.Query().Get("real") != "c/x" {
			t.Errorf("请求异常: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"explain":{"schema_version":1,"mode":"index","real_path":"c/x","kind":"chunk",` +
			`"owner":{"rel":"a/b.txt","role":"file","chunk_index":2},` +
			`"anomalies":[{"code":"blob_orphan","real_path":"c/y","detail":"疑似孤儿"}]}}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewFileClient(srv.URL, WithHTTPClient(srv.Client()))
	ex, err := svc.VolumeExplain(context.Background(), "vault", "c/x", false)
	if err != nil {
		t.Fatalf("VolumeExplain: %v", err)
	}
	if ex.Kind != "chunk" || ex.Owner == nil || ex.Owner.Rel != "a/b.txt" || ex.Owner.ChunkIndex == nil || *ex.Owner.ChunkIndex != 2 {
		t.Fatalf("owner 解码异常: %+v", ex)
	}
	if len(ex.Anomalies) != 1 || ex.Anomalies[0].Code != volume.AnomalyBlobOrphan {
		t.Fatalf("anomaly 解码异常: %+v", ex.Anomalies)
	}
}

func TestVolumeInspect_EmptyVolumeRejected(t *testing.T) {
	t.Parallel()
	svc := NewFileClient("http://127.0.0.1:1")
	if _, err := svc.VolumeTrace(context.Background(), "", "a", false); err == nil {
		t.Fatal("空卷名应报错")
	}
	if _, err := svc.VolumeExplain(context.Background(), "", "a", false); err == nil {
		t.Fatal("空卷名应报错")
	}
}

// TestVolumeInspect_NilResponseRejected 服务端回空结果 ⇒ 客户端必须报错（不得静默当空）。
func TestVolumeInspect_NilResponseRejected(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewFileClient(srv.URL, WithHTTPClient(srv.Client()))
	if _, err := svc.VolumeTrace(context.Background(), "v", "a", false); err == nil {
		t.Fatal("空 trace 应报错")
	}
	if _, err := svc.VolumeExplain(context.Background(), "v", "a", false); err == nil {
		t.Fatal("空 explain 应报错")
	}
}
