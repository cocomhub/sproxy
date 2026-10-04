// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockSecretsServer 启动带 /api/secrets 端点的 mock 服务器（白盒验证客户端封装：
// CreateSecret/ListSecrets/ExportSecret/DeleteSecret 的请求构造与响应解析）。
func mockSecretsServer(t *testing.T) (*httptest.Server, *map[string]string) {
	t.Helper()
	store := map[string]string{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/secrets", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string `json:"name"`
			Mode  string `json:"mode"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		switch req.Mode {
		case "", "random":
			// 服务端生成随机 32B hex。
			val := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			store[req.Name] = val
			writeJSON := map[string]any{"name": req.Name, "value": val, "origin": "random"}
			_ = json.NewEncoder(w).Encode(writeJSON)
		case "import", "passphrase":
			if len(req.Value) != 64 {
				http.Error(w, "value 须 64 hex", http.StatusBadRequest)
				return
			}
			store[req.Name] = req.Value
			_ = json.NewEncoder(w).Encode(map[string]any{"name": req.Name, "origin": "passphrase"})
		default:
			http.Error(w, "unknown mode", http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /api/secrets", func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0, len(store))
		for n := range store {
			names = append(names, n)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": names})
	})
	mux.HandleFunc("GET /api/secrets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
		val, ok := store[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "value": val, "origin": "export"})
	})
	mux.HandleFunc("DELETE /api/secrets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
		if _, ok := store[name]; !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		delete(store, name)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"` + name + `","deleted":"true"}`))
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &store
}

const testClientSecretHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCreateRandomSecret(t *testing.T) {
	t.Parallel()
	ts, store := mockSecretsServer(t)
	c := NewFileClient(ts.URL)
	resp, err := c.CreateRandomSecret(context.Background(), "rand1")
	if err != nil {
		t.Fatalf("CreateRandomSecret: %v", err)
	}
	if resp.Name != "rand1" || resp.Origin != "random" || len(resp.Value) != 64 {
		t.Errorf("响应=%+v（应 name=rand1 origin=random 64hex）", resp)
	}
	if (*store)["rand1"] != resp.Value {
		t.Errorf("mock 端落盘=%s 与返回 %s 不一致", (*store)["rand1"], resp.Value)
	}
}

func TestCreatePassphraseSecret(t *testing.T) {
	t.Parallel()
	ts, store := mockSecretsServer(t)
	c := NewFileClient(ts.URL)
	// 本地派生（真实高熵双口令→scrypt）；上传派生结果。
	resp, err := c.CreatePassphraseSecret(context.Background(), "imp1", "口令A", "口令B")
	if err != nil {
		t.Fatalf("CreatePassphraseSecret: %v", err)
	}
	if resp.Name != "imp1" || resp.Origin != "passphrase" || resp.Value != "" {
		t.Errorf("响应=%+v（应 name=imp1 origin=passphrase 无 value）", resp)
	}
	// mock 端收到的是派生 hex（64 字符），非口令本身。
	stored := (*store)["imp1"]
	if len(stored) != 64 {
		t.Errorf("上传值长度=%d 应 64 hex", len(stored))
	}
	if strings.Contains(stored, "口令") {
		t.Error("原始口令不应出现在上传值中")
	}
}

func TestListExportDeleteSecret(t *testing.T) {
	t.Parallel()
	ts, store := mockSecretsServer(t)
	c := NewFileClient(ts.URL)

	// 预置。
	(*store)["aaa"] = testClientSecretHex
	(*store)["bbb"] = testClientSecretHex

	// 列表。
	names, err := c.ListSecrets(context.Background())
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["aaa"] || !found["bbb"] {
		t.Errorf("列表=%v（应含 aaa/bbb）", names)
	}

	// 导出。
	val, err := c.ExportSecret(context.Background(), "aaa")
	if err != nil || val != testClientSecretHex {
		t.Errorf("导出=%s err=%v", val, err)
	}

	// 删除。
	if err := c.DeleteSecret(context.Background(), "aaa"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, ok := (*store)["aaa"]; ok {
		t.Error("删除后 mock store 不应再含 aaa")
	}
	// 删除不存在 fail-closed。
	if err := c.DeleteSecret(context.Background(), "nope"); err == nil {
		t.Error("删除不存在应报错")
	}
}
