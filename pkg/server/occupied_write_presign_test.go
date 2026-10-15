// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// occupied_write_presign_test.go 验证**预签名签发期**的占用写保护（复审 §3.3/§8.3）：presign
// 签发的 PUT URL 由客户端直传对象存储，服务端看不到那次写 ⇒ 占用判定只能在**签发时**做。
// 此前只在 `.../presign/complete`（登记）拦 ⇒ 对象已经被直传进「只读」占用子目录，只是登记时
// 才 403——留下一个未登记、不计配额的游离对象。签发期拦下即封死该窗口。
//
// 语义边界：占用目录是**只读**而非不可见 ⇒ GET 签名不受影响（读不拦），只有 PUT 拒绝。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// presignOccType 是预签名占用写保护用例的后端类型（须注册一次，且在 presignOutcome 前完成）。
const presignOccType = "backends-presign-occ-test"

// presignOccHandlers 装配：main 本地卷 + direct 卷（type=presignOccType，支持 Presigner）+
// 直接登记「vault 占用 direct/videos」关联（占用坐标 = 卷内用户可见相对路径）。
func presignOccHandlers(t *testing.T) *Handlers {
	t.Helper()
	registry.RegisterBackend(presignOccType, func(_ context.Context, _ volume.Volume) (registry.ExternalBackend, error) {
		return &presignTestBackend{}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(presignOccType) })

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "direct", Type: presignOccType},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.links().register(volumeLink{Base: "direct", Subdir: "videos", Wrapper: "vault", Owner: "alice"})
	return h
}

// presignRequest 走真实 presign handler（POST /api/backends/{type}/presign），actor 固定 alice。
func presignRequest(t *testing.T, h *Handlers, path, method string) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", func(w http.ResponseWriter, r *http.Request) {
		h.backendPresignHandler(w, r.WithContext(withActor(r.Context(), "alice")))
	})
	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+presignOccType+"/presign?path="+
		url.QueryEscape(path)+"&method="+method, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestBackendsPresign_OccupiedSubdir_PutRejected 占用子目录的 **PUT** 签名在签发期即 403
// （fail-closed）；同卷非占用路径照常签发（防一刀切）；占用子目录的 **GET** 签名仍放行
// （占用 = 只读，不是不可读）。
func TestBackendsPresign_OccupiedSubdir_PutRejected(t *testing.T) {
	t.Parallel()
	h := presignOccHandlers(t)

	status, body := presignRequest(t, h, "videos/x.bin", http.MethodPut)
	if status != http.StatusForbidden {
		t.Fatalf("占用子目录 PUT 签发 = %d, want 403 (body: %s)", status, body)
	}
	if !strings.Contains(body, "占用") {
		t.Fatalf("拒绝文案应说明占用只读: %s", body)
	}
	// 占用子目录的**自身**路径同样拒绝（定向包含：rel == subdir）。
	status, body = presignRequest(t, h, "videos", http.MethodPut)
	if status != http.StatusForbidden {
		t.Fatalf("占用子目录自身 PUT 签发 = %d, want 403 (body: %s)", status, body)
	}
	// 对照：同卷非占用分支 → 正常签发（URL 收敛到调用者 owner 键空间）。
	status, body = presignRequest(t, h, "photos/y.bin", http.MethodPut)
	if status != http.StatusOK || !strings.Contains(body, "https://mock.s3/PUT/alice/user/photos/y.bin") {
		t.Fatalf("非占用路径 PUT 签发 = %d (body: %s), want 200 + 签名 URL", status, body)
	}
	// 读路径不拦：占用子目录 GET 仍可签发。
	if gs, gb := presignRequest(t, h, "videos/x.bin", http.MethodGet); gs != http.StatusOK {
		t.Fatalf("占用子目录 GET 签发 = %d, want 200（占用为只读，读不拦）(body: %s)", gs, gb)
	}
}
