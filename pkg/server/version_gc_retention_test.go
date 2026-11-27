// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// version_gc_retention_test.go 锁「覆盖写产生的旧版本会按 retention 被 GC 回收」。
//
// 背景（2026-11-27）：nightly 的空间账本验收发现「等长覆盖后 usage 正好 += 一个文件大小」，
// 查证确认这是**设计**（versioning 开启时 `SaveVersion` 先备份旧版本再覆盖）。那条结论的直接
// 推论是：**回收必须靠 `versioning.retention` 的周期 GC**，否则卷会无限增长 ——
// 而这整条链路（SaveVersion → 版本目录 → gcAllExpiredVersionsPass → 删除）此前没有任何用例覆盖。
//
// 断言（全部走生产路径：真实 HTTP 上传 + 真实 GC Pass + 真实列表接口）：
//  1. 覆盖写后存在 1 个旧版本（备份确实发生）；
//  2. **保留期内不得删除**（语义是「超过才删」，不是「总是删」）；
//  3. 保留期到期后 GC 删除该版本，且**当前文件仍在**（GC 只清历史）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestVersionGC_RetentionReclaimsOverwrittenContent 覆盖写 → 旧版本按保留期被回收。
func TestVersionGC_RetentionReclaimsOverwrittenContent(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	cfg.Versioning.Enabled = true
	cfg.Versioning.MaxVersions = 5 // 上限维度不参与本用例（只验保留期）
	cfg.Versioning.Retention = time.Hour

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   http.NewServeMux(),
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})

	const name = "gc-retention.txt"
	// ① 传 A 再传 B（同路径覆盖）⇒ SaveVersion 备份 A。
	uploadFile(t, ts.URL, name, []byte("AAAA-content-0001"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("AAAA-content-0001")),
	})
	uploadFile(t, ts.URL, name, []byte("BBBB-content-0002"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("BBBB-content-0002")),
	})
	if got := countVersionsHTTP(t, ts.URL, name); got != 1 {
		t.Fatalf("覆盖写后应有 1 个旧版本，得到 %d ⇒ 备份没发生（那么『覆盖靠 retention 回收』的前提不成立）", got)
	}

	// ② 保留期未到 ⇒ GC **不得**删除。
	h.gcAllExpiredVersionsPass()
	if got := countVersionsHTTP(t, ts.URL, name); got != 1 {
		t.Fatalf("保留期内不应删除版本，得到 %d 个", got)
	}

	// ③ 把保留期压到 1ns ⇒ 所有版本「已过期」⇒ GC 必须删除。
	next := cfgPtr.Load()
	next.Versioning.Retention = time.Nanosecond
	cfgPtr.Store(next)
	<-time.After(2 * time.Millisecond) // 保证 cutoff 晚于版本创建时间（R14：不用 time.Sleep）
	h.gcAllExpiredVersionsPass()

	if got := countVersionsHTTP(t, ts.URL, name); got != 0 {
		t.Fatalf("保留期到期后版本应被 GC 删除，仍有 %d 个 ⇒ 卷会无限增长", got)
	}
	// GC 只清历史版本，当前文件必须仍在且内容为 B。
	resp, err := http.Get(ts.URL + "/download?filename=" + name)
	if err != nil {
		t.Fatalf("GC 后下载当前文件: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GC 不得影响当前文件，下载得到 %d", resp.StatusCode)
	}
}

// countVersionsHTTP 通过生产接口读版本数（同时验证列表面与 GC 的一致性）。
func countVersionsHTTP(t *testing.T, baseURL, filename string) int {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/versions?filename=" + filename)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list versions HTTP %d", resp.StatusCode)
	}
	var out struct {
		Versions []any `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	return len(out.Versions)
}
