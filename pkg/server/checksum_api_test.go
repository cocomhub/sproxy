// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// checksum_api_test.go 钉住 checksum 台账的**运维重建端点**（§15.1 步骤②）：
// `POST /api/checksums/rebuild?owner=` 从对象 meta sidecar 回填台账，并把条数报给调用方
// （部分缺失——而非全空——时唯一可用的修复入口；全空由 List 自动自愈）。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestRebuildChecksumsHandler_FillsFromMeta 钉住「台账部分缺失 → 显式重建补齐」。
// 变异：把 handler 里的 RebuildChecksums 换成无操作 ⇒ entries=0 且台账仍缺 ⇒ 红。
func TestRebuildChecksumsHandler_FillsFromMeta(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	const owner, rel, sum = "alice", "user/docs/a.bin", "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	tnt := h.tenantFor(owner)
	if tnt == nil {
		t.Fatal("租户不可用")
	}
	if err := tnt.Root().MkdirAll("user/docs", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := tnt.Root().OpenFile(rel, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, _ = f.WriteString("payload")
	_ = f.Close()
	// 合法 sidecar（server 的 meta 策略写的是真实 FileMeta；这里直接构造最小 JSON）。
	if err := tnt.Root().MkdirAll("meta/docs", 0o755); err != nil {
		t.Fatalf("MkdirAll meta: %v", err)
	}
	sf, serr := tnt.Root().OpenFile("meta/docs/a.bin.meta", os.O_CREATE|os.O_WRONLY, 0o644)
	if serr != nil {
		t.Fatalf("OpenFile meta: %v", serr)
	}
	if _, werr := fmt.Fprintf(sf, `{"version":1,"total_sha256":%q}`, sum); werr != nil {
		t.Fatalf("写 meta: %v", werr)
	}
	_ = sf.Close()

	// 跨租户修复仅限 admin：非 admin 指定他人 owner ⇒ 403（防跨租户写）。
	denied := httptest.NewRequest(http.MethodPost, "/api/checksums/rebuild?owner="+owner, nil)
	denied = withActorContext(denied, "bob")
	rrDenied := httptest.NewRecorder()
	h.rebuildChecksumsHandler(rrDenied, denied)
	if rrDenied.Code != http.StatusForbidden {
		t.Fatalf("非 admin 指定他人 owner 应 403，got %d body=%s", rrDenied.Code, rrDenied.Body.String())
	}

	// 本人 owner（缺省 = 请求主体）⇒ 200 并回填。
	req := httptest.NewRequest(http.MethodPost, "/api/checksums/rebuild", nil)
	req = withActorContext(req, owner)
	rr := httptest.NewRecorder()
	h.rebuildChecksumsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// 业务审计动作必须与实现常量一致（R35-b：action=checksum_rebuild 的场景证据）。
	if auditActionChecksumRebuild != "checksum_rebuild" {
		t.Fatalf("审计动作 = %q，期望 checksum_rebuild", auditActionChecksumRebuild)
	}
	if got, ok := ledgerGetVolume(h.checksumStoreFor(owner), "main", rel); !ok || got != sum {
		t.Fatalf("重建后台账应有 %q，got %q ok=%v（body=%s）", sum, got, ok, rr.Body.String())
	}
}
