// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestVerifyOneOwner_TenantNil 租户解析失败（TenantFor 返回 nil）：记 errors（不静默跳过）。
func TestVerifyOneOwner_TenantNil(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	d.TenantFor = func(string) *storage.Tenant { return nil }

	var rep Report
	New(d).VerifyOneOwner("bob", &rep, "")
	if rep.Total != 0 || rep.Skipped != 0 {
		t.Fatalf("rep=%+v，want 未核对任何条目", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "bob") {
		t.Fatalf("rep.Errors=%v，want 1 条含 bob 的租户不可用", rep.Errors)
	}
}

// TestVerifyOneOwner_TenantRootNil 租户存在但根句柄未装配：与「不可用」同义（记 errors）。
func TestVerifyOneOwner_TenantRootNil(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	// storage.NewTenant 拒绝 nil root，零值租户即「Root() == nil」形态。
	d.TenantFor = func(string) *storage.Tenant { return &storage.Tenant{} }

	var rep Report
	New(d).VerifyOneOwner("alice", &rep, "")
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "alice") {
		t.Fatalf("rep=%+v，want 1 条租户不可用", rep)
	}
}

// TestVerifyOneOwner_StoreMissing 台账未装配（ChecksumStoreFor 返回 nil）：记 skipped。
func TestVerifyOneOwner_StoreMissing(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	d.ChecksumStoreFor = func(string) checksum.ChecksumStoreIface { return nil }

	var rep Report
	New(d).VerifyOneOwner("alice", &rep, "")
	if rep.Skipped != 1 || rep.Total != 0 || len(rep.Errors) != 0 {
		t.Fatalf("rep=%+v，want skipped=1 且无 errors", rep)
	}
}

// TestVerifyOneOwner_EmptyLedgerSkipped 台账为空（未上传过）：Len()==0 直接跳过，不做全量遍历。
func TestVerifyOneOwner_EmptyLedgerSkipped(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	rep := New(f.deps()).VerifyOnce(context.Background(), "", "alice")
	if rep.Skipped != 1 || rep.Total != 0 {
		t.Fatalf("rep=%+v，want skipped=1 total=0", rep)
	}
}

// TestVerifyOneOwner_CorruptLedgerSkipped 台账文件损坏 → store 降级为空台账 → 该租户记 skipped
// （单租户损坏不中止全卷巡检）。
func TestVerifyOneOwner_CorruptLedgerSkipped(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", nil)
	ledger := f.abs("meta/checksums.json")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatalf("mkdir meta: %v", err)
	}
	if err := os.WriteFile(ledger, []byte("{not-json"), 0o644); err != nil {
		t.Fatalf("写损坏台账: %v", err)
	}
	f.reloadStore(t)

	rep := New(f.deps()).VerifyOnce(context.Background(), "", "alice")
	if rep.Skipped != 1 || rep.Total != 0 || len(rep.Errors) != 0 {
		t.Fatalf("rep=%+v，want skipped=1 且无 errors", rep)
	}
}

// TestVerifyOneOwner_GetAllFallback 台账未实现 Iterator/LenCounter 可选扩展：回落 GetAll
// （零回归契约），条目照常核对。
func TestVerifyOneOwner_GetAllFallback(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	ledger := &plainLedger{entries: map[string]string{"user/a.txt": shaOf(t, "aaa")}}
	d.ChecksumStoreFor = func(string) checksum.ChecksumStoreIface { return ledger }

	rep := New(d).VerifyOnce(context.Background(), "", "alice")
	if rep.Total != 1 || rep.Ok != 1 || rep.Skipped != 0 {
		t.Fatalf("rep=%+v，want total=1 ok=1 skipped=0", rep)
	}
}

// TestVerifyOneOwner_GetAllFallbackEmpty 台账替身（无 LenCounter）且 GetAll 为空：
// 无任何条目可核对 → 记 skipped。
func TestVerifyOneOwner_GetAllFallbackEmpty(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	d.ChecksumStoreFor = func(string) checksum.ChecksumStoreIface { return &plainLedger{} }

	rep := New(d).VerifyOnce(context.Background(), "", "alice")
	if rep.Skipped != 1 || rep.Total != 0 {
		t.Fatalf("rep=%+v，want skipped=1 total=0", rep)
	}
}

// TestVerifyOneOwner_VolumeKeysDeduped 同一 rel 在两个卷视图下各有一条卷维度键：
// 按 rel 去重只核对一次（多卷同名文件不重复计入 total）。
func TestVerifyOneOwner_VolumeKeysDeduped(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	sum := shaOf(t, "aaa")
	f.cs.Delete("user/a.txt") // 只保留卷维度键，隔离去重断言
	f.cs.SetInVolume("vol-a", "user/a.txt", sum)
	f.cs.SetInVolume("vol-b", "user/a.txt", sum)

	rep := New(f.deps()).VerifyOnce(context.Background(), "", "alice")
	if rep.Total != 1 || rep.Ok != 1 {
		t.Fatalf("rep=%+v，want total=1 ok=1（卷维度键按 rel 去重）", rep)
	}
}

// TestVerifyPass_AllTenantsAndAnonymousFallback owner 为空 = 全租户巡检：枚举 ListTenantIDs
// 的每个租户，并补上未出现在枚举里的 anonymous（此处未装配 → 记 errors，但不中止其它租户）。
func TestVerifyPass_AllTenantsAndAnonymousFallback(t *testing.T) {
	t.Parallel()
	alice := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	bob := newTenantFixture(t, "bob", map[string]string{"user/b.txt": "bbb"})
	tenants := map[string]*storage.Tenant{"alice": alice.tnt, "bob": bob.tnt}
	stores := map[string]checksum.ChecksumStoreIface{"alice": alice.cs, "bob": bob.cs}

	svc := New(Deps{
		Logger:        testLogger(),
		TenantFor:     func(owner string) *storage.Tenant { return tenants[owner] },
		ListTenantIDs: func() []string { return []string{"alice", "bob"} },
		LocateOwnerFile: func(owner, rel string) (*storage.Tenant, string, bool) {
			tnt, ok := tenants[owner]
			return tnt, "vol-a", ok
		},
		ChecksumStoreFor: func(owner string) checksum.ChecksumStoreIface { return stores[owner] },
		Busy:             new(atomic.Bool),
	})

	rep := svc.VerifyOnce(context.Background(), "", "")
	if rep.Total != 2 || rep.Ok != 2 || rep.Skipped != 0 {
		t.Fatalf("rep=%+v，want total=2 ok=2（两个租户各 1 个文件）", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], storage.AnonymousOwner) {
		t.Fatalf("rep.Errors=%v，want 仅 anonymous 不可用一条", rep.Errors)
	}
}

// TestVerifyPass_VolumeFilterIgnored 验收用例（缺陷已修复）：
// verifyPass 的 volName 此前完全未被使用——文档与 HTTP 面约定「非空 = 只核该卷视图」，
// 实际会核对全部卷并隔离其它卷坏文件。现按 LocateOwnerFile 返回的卷名过滤。
func TestVerifyPass_VolumeFilterIgnored(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	rep := New(f.deps()).VerifyOnce(context.Background(), "vol-does-not-exist", "alice")
	if rep.Total != 0 {
		t.Fatalf("指定不存在的卷时不应核对任何条目，got total=%d", rep.Total)
	}
}
