// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_presign_test.go 验证后端预签名端点（roadmap 3.3 签名 v4 直传）：
//  1. POST /api/backends/{type}/presign?path=&method= → 200 + 签名 URL（type 已注册且支持 Presigner）。
//  2. type 未注册 → 404；后端不支持 Presigner → 405；method 非法 → 400。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// presignTestBackend 是支持 Presigner 的 fake 后端（presign 端点测试）。
type presignTestBackend struct{}

func (presignTestBackend) FS() sync.FS  { return nil }
func (presignTestBackend) Close() error { return nil }
func (presignTestBackend) PresignedURL(_ context.Context, path, method string, _ int64) (string, error) {
	return "https://mock.s3/" + method + "/" + path, nil
}

// TestBackendsPresign 验证 presign 端点成功路径。
func TestBackendsPresign(t *testing.T) {
	t.Parallel()
	registry.RegisterBackend(presignTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &presignTestBackend{}, nil
	})
	defer registry.UnregisterBackendForTest(presignTestType)

	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+presignTestType+"/presign?path=dir/a.txt&method=PUT", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// P0 修复（第 5 轮对抗评审）：预签名路径由服务端收敛到调用者 owner 键空间
	// （`<owner>/user/<rel>`），后端键空间恒为 `Location.FSPath` 形态。
	if !strings.Contains(body, "https://mock.s3/PUT/anonymous/user/dir/a.txt") {
		t.Fatalf("响应缺 owner 收敛后的签名 URL: %s", body)
	}
}

// TestPresignOwnerKey 纯函数：路径 → `<owner>/user/<rel>` 收敛 + 越权拒绝。
func TestPresignOwnerKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		owner   string
		in      string
		want    string
		wantErr bool
	}{
		{"裸相对路径", "alice", "dir/a.txt", "alice/user/dir/a.txt", false},
		{"user 桶前缀", "alice", "user/dir/a.txt", "alice/user/dir/a.txt", false},
		{"已带本人 owner", "alice", "alice/user/dir/a.txt", "alice/user/dir/a.txt", false},
		{"空 owner 归一 anonymous", "", "a.txt", "anonymous/user/a.txt", false},
		{"他人 owner → 403", "alice", "bob/user/a.txt", "", true},
		{"他人 owner 非 user 桶也拒", "alice", "bob/cloud/a.txt", "", true},
		{"非 user 桶 → 403", "alice", "cloud/a.txt", "", true},
		{"user 桶根 → 拒绝", "alice", "user", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := presignOwnerKey(c.owner, c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应拒绝 %q，得到 %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("presignOwnerKey(%q, %q) = %v", c.owner, c.in, err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// TestBackendsPresign_NotFound 未注册 type → 404。
func TestBackendsPresign_NotFound(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/no-such-type/presign?path=a.txt&method=PUT", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestBackendsPresign_Unsupported 后端不支持 Presigner → 405。
func TestBackendsPresign_Unsupported(t *testing.T) {
	t.Parallel()
	registry.RegisterBackend(plainTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &fakeUserVolBackend{}, nil // 无 PresignedURL 方法
	})
	defer registry.UnregisterBackendForTest(plainTestType)

	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+plainTestType+"/presign?path=a.txt&method=PUT", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// presignTestType / plainTestType 是 presign 端点测试类型（全局注册一次）。
const (
	presignTestType = "backends-presign-test"
	plainTestType   = "backends-plain-test"
)
