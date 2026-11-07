// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestVerifyOneEntry_LocateNotFound 定位未命中（多卷都找不到 rel）：记 missing（不是 errors）。
func TestVerifyOneEntry_LocateNotFound(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	d := f.deps()
	d.LocateOwnerFile = func(string, string) (*storage.Tenant, string, bool) { return nil, "", false }

	var rep Report
	New(d).verifyOneEntry("alice", "user/a.txt", "deadbeef", &rep, "")
	if rep.Total != 1 || len(rep.Missing) != 1 || rep.Missing[0] != "user/a.txt" {
		t.Fatalf("rep=%+v，want total=1 missing=[user/a.txt]", rep)
	}
}

// TestVerifyOneEntry_LocateNilTenant 定位报命中但租户为 nil：同样记 missing（防御性 nil 判定）。
func TestVerifyOneEntry_LocateNilTenant(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	d := f.deps()
	d.LocateOwnerFile = func(string, string) (*storage.Tenant, string, bool) { return nil, "vol-a", true }

	var rep Report
	New(d).verifyOneEntry("alice", "user/a.txt", "deadbeef", &rep, "")
	if rep.Total != 1 || len(rep.Missing) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("rep=%+v，want total=1 missing=1 无 errors", rep)
	}
}

// TestVerifyOneEntry_LocateNilRoot 定位命中但租户根句柄未装配：同样记 missing。
func TestVerifyOneEntry_LocateNilRoot(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	d := f.deps()
	d.LocateOwnerFile = func(string, string) (*storage.Tenant, string, bool) { return &storage.Tenant{}, "vol-a", true }

	var rep Report
	New(d).verifyOneEntry("alice", "user/a.txt", "deadbeef", &rep, "")
	if rep.Total != 1 || len(rep.Missing) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("rep=%+v，want total=1 missing=1 无 errors", rep)
	}
}

// TestVerifyOneEntry_ReadError 台账项在磁盘上是目录（读失败但**不是** NotExist）：
// 归入 errors 而非 missing（区分「缺文件」与「读不了」）。
func TestVerifyOneEntry_ReadError(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	if err := os.MkdirAll(f.abs("user/as-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var rep Report
	New(f.deps()).verifyOneEntry("alice", "user/as-dir", "deadbeef", &rep, "")
	if rep.Total != 1 || len(rep.Missing) != 0 {
		t.Fatalf("rep=%+v，want total=1 且不计 missing", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "读取 user/as-dir 失败") {
		t.Fatalf("rep.Errors=%v，want 1 条读取失败", rep.Errors)
	}
}

// TestVerifyOneEntry_QuarantineMkdirFailure 隔离目录位置被文件占位 → 建目录失败：
// 记 errors，坏文件留在原位（不静默丢现场）。
func TestVerifyOneEntry_QuarantineMkdirFailure(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "original"})
	f.write(t, "user/a.txt", "tampered")
	blocker := f.abs("meta/quarantine")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("写占位文件: %v", err)
	}

	var rep Report
	New(f.deps()).verifyOneEntry("alice", "user/a.txt", shaOf(t, "original"), &rep, "")
	if len(rep.Mismatched) != 1 || rep.Mismatched[0].Path != "user/a.txt" {
		t.Fatalf("rep=%+v，want 1 条 mismatched", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "隔离 user/a.txt 失败") {
		t.Fatalf("rep.Errors=%v，want 1 条隔离失败", rep.Errors)
	}
	if _, err := os.Stat(f.abs("user/a.txt")); err != nil {
		t.Fatalf("隔离失败时坏文件应留在原位: %v", err)
	}
}

// TestVerifyOneEntry_QuarantineRenameFailure 隔离名 = rel 扁平化 + UnixNano 后缀；当扁平化后
// 文件名超出文件系统单段上限（255 字节）时 rename 失败：记 errors、文件留在原位。
func TestVerifyOneEntry_QuarantineRenameFailure(t *testing.T) {
	t.Parallel()
	// 源文件名 240 字节（可正常读写），扁平化并追加后缀后 260+ 字节 ⇒ 必然 rename 失败。
	longRel := "user/" + strings.Repeat("a", 236) + ".bin"
	f := newTenantFixture(t, "alice", map[string]string{longRel: "original"})
	f.write(t, longRel, "tampered")

	var rep Report
	New(f.deps()).verifyOneEntry("alice", longRel, shaOf(t, "original"), &rep, "")
	if len(rep.Mismatched) != 1 {
		t.Fatalf("rep=%+v，want 1 条 mismatched", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "隔离 "+longRel+" 失败") {
		t.Fatalf("rep.Errors=%v，want 1 条隔离失败", rep.Errors)
	}
	if _, err := os.Stat(f.abs(longRel)); err != nil {
		t.Fatalf("隔离失败时坏文件应留在原位: %v", err)
	}
}
