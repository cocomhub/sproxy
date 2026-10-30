// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/s3"
)

// TestS3TenantFor_NamedBucketACLDeniedFailsClosed 评审 P2：命名桶此前只按名解析卷（无 ACL）⇒
// 受限 ACL 卷仍可被未授权 owner 经 `/s3/<卷名>/<key>` 读写删。现 ACL 拒绝时返回 nil
// **且不得回落到默认卷**（回落会造成语义漂移 + 仍是越权面）。
// 变异：去掉 Authorize 检查 → 未授权用例解析出租户 → 红。
func TestS3TenantFor_NamedBucketACLDeniedFailsClosed(t *testing.T) {
	t.Parallel()
	dir, other := t.TempDir(), t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{
		{Name: "default", Root: dir},
		{Name: "locked", Root: other, ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"alice"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	namedBucketReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/s3/locked/x.bin", nil)
		return r.WithContext(s3.WithBucket(r.Context(), "locked"))
	}

	// 未授权 owner：fail-closed（nil，且不是「回落到默认卷」的租户）。
	if tnt := h.s3Service().TenantFor("bob", namedBucketReq()); tnt != nil {
		t.Fatalf("ACL 拒绝时 s3TenantFor 必须返回 nil（不得回落默认卷），got %v", tnt)
	}
	// 授权 owner：解析到该命名桶租户。
	tnt := h.s3Service().TenantFor("alice", namedBucketReq())
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("授权 owner 应解析出该命名桶租户")
	}
	// 未注入 bucket ctx（默认卷路径）：不受影响（零回归）。
	if tnt := h.s3Service().TenantFor("bob", httptest.NewRequest(http.MethodGet, "/s3/?list-type=2", nil)); tnt == nil {
		t.Fatal("默认卷路径不应受命名桶 ACL 影响")
	}
}
