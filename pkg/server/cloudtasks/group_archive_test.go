// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁组归档入口（P1-c S3b）的**包内契约**：跳过未完成子任务后的响应字段、
// 配额预留/结算/登记与组归档名落库的调用顺序，以及 ACL 必须早于源文件打开。
// tar 落盘与转存执行核心（仍在装配层，S3c 迁移面）由 pkg/server 的既有 HTTP 测试驱动。

// stubTenants 返回固定租户（nil 模拟租户不可用）。
type stubTenants struct {
	tenant *storage.Tenant
}

func (s stubTenants) TenantFor(string) *storage.Tenant { return s.tenant }

// stubArchiveTargets 记录归档打包/转存面的调用（本片只需判定「谁被调用/入参」）。
type stubArchiveTargets struct {
	multiChecksum string
	multiExists   bool
	multiErr      error

	// 入参取证
	multiSources []ArchiveSource
	multiOutRel  string
}

func (s *stubArchiveTargets) StagingFS(_ string, fs syncpkg.FS) syncpkg.FS { return fs }

func (s *stubArchiveTargets) Lock(string, string) (func(), bool) { return func() {}, true }

func (s *stubArchiveTargets) RecordBypassChecksum(string, string, string, string) {}

func (s *stubArchiveTargets) RecordBypassWrite(string, string, int64, string, string, *storage.Root) {
}

func (s *stubArchiveTargets) SkipVerify() bool { return true }

func (s *stubArchiveTargets) MaxArchiveBytes() int64 { return 0 }

func (s *stubArchiveTargets) ArchiveKey() []byte { return nil }

func (s *stubArchiveTargets) CreateArchiveTar(*storage.Root, string, string, *storage.Root, string, *slog.Logger) (string, bool, error) {
	return "", false, nil
}

func (s *stubArchiveTargets) CreateMultiArchiveTar(sources []ArchiveSource, _ *storage.Root, outRel string, _ *slog.Logger) (string, bool, error) {
	s.multiSources, s.multiOutRel = sources, outRel
	if s.multiErr != nil || s.multiExists {
		return "", s.multiExists, s.multiErr
	}
	return s.multiChecksum, false, nil
}

// stubArchiveQuota 记录归档配额四方法的调用顺序（预留 → 结算 → 登记）。
type stubArchiveQuota struct {
	reservePre    int64
	reserveStatus int
	reserveMsg    string
	reserveOwner  string
	reserveTotal  int64
	settleActual  int64
	settleStatus  int
	released      bool

	calls []string
}

func (s *stubArchiveQuota) BucketScope(string, string) *quota.Scope { return nil }

func (s *stubArchiveQuota) ScopeFor(string, string) *quota.Scope { return nil }

func (s *stubArchiveQuota) WaitTimeout() time.Duration { return 0 }

func (s *stubArchiveQuota) ReserveFallback(context.Context, int64, time.Duration) error { return nil }

func (s *stubArchiveQuota) ReleaseFallback(int64) {}

func (s *stubArchiveQuota) TryReserveFallback(int64) error { return nil }

func (s *stubArchiveQuota) ReserveArchive(_ context.Context, owner string, totalSourceSize int64) (*quota.Settlement, int64, int, string) {
	s.calls = append(s.calls, "reserve")
	s.reserveOwner, s.reserveTotal = owner, totalSourceSize
	if s.reserveStatus != 0 {
		return nil, 0, s.reserveStatus, s.reserveMsg
	}
	return nil, s.reservePre, 0, ""
}

func (s *stubArchiveQuota) SettleArchive(*storage.Root, string, *quota.Settlement, int64, *slog.Logger) (int64, int, string) {
	s.calls = append(s.calls, "settle")
	return s.settleActual, s.settleStatus, ""
}

func (s *stubArchiveQuota) ReleaseArchive(*quota.Settlement, int64) {
	s.calls = append(s.calls, "release")
	s.released = true
}

// newGroupArchiveEnv 装配组归档用例：真实租户（含 cloud/t1/a.bin）+ 组含 1 个已完成与
// 1 个进行中的子任务。
func newGroupArchiveEnv(t *testing.T) (*Service, *stubGroups, *stubArchiveTargets, *stubArchiveQuota) {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tenant, err := storage.OpenTenant(root, "alice")
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	// Windows：租户子根句柄未关会让 t.TempDir 清理失败（LIFO ⇒ 先于 root.Close 执行）。
	t.Cleanup(func() { _ = tenant.Root().Close() })
	tenantRoot := tenant.Root()
	if mkErr := tenantRoot.MkdirAll("cloud/t1", 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	f, err := tenantRoot.OpenFile(filepath.ToSlash("cloud/t1/a.bin"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := f.Write([]byte("aaaaa")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	groups := &stubGroups{groups: map[string]*cloud.CloudTaskGroup{
		"g1": {ID: "g1", TaskIDs: []string{"t1", "t2"}},
	}}
	tasks := &stubTasks{tasks: map[string]*cloud.CloudTask{
		"t1": {ID: "t1", Owner: "alice", Filename: "a.bin", Status: "completed"},
		"t2": {ID: "t2", Owner: "alice", Filename: "b.bin", Status: "downloading"},
	}}
	targets := &stubArchiveTargets{multiChecksum: "sum-1"}
	quotaStub := &stubArchiveQuota{reservePre: 100, settleActual: 1234}
	svc := New(Deps{
		Groups: groups, Tasks: tasks, Tenants: stubTenants{tenant: tenant},
		Volumes: stubVolumes{}, Quota: quotaStub, Targets: targets,
	})
	return svc, groups, targets, quotaStub
}

// TestArchiveGroup_SkipsIncompleteTasks 锁组归档（落 archive 桶）的响应字段与调用顺序：
// 未完成子任务进 skipped 列表，配额按源文件**物理大小**预留、按落盘实际结算、按实际登记，
// 组归档名落库，checksum/大小/计数逐字回包。
func TestArchiveGroup_SkipsIncompleteTasks(t *testing.T) {
	t.Parallel()
	svc, groups, targets, quotaStub := newGroupArchiveEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/archive",
		strings.NewReader(`{"archive_name":"g1-archive.tar.gz"}`))
	req.SetPathValue("id", "g1")

	svc.ArchiveGroup(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got CloudArchiveResult
	decodeBody(t, rec, &got)
	if !got.Success || got.File != "g1-archive.tar.gz" || got.Checksum != "sum-1" ||
		got.Size != 1234 || got.TaskCount != 1 || got.SkippedCount != 1 {
		t.Fatalf("响应 = %+v，期望 1 个文件 + 1 个跳过 + 实际大小 1234", got)
	}
	if len(got.SkippedTasks) != 1 || got.SkippedTasks[0] != "t2" {
		t.Fatalf("skipped_tasks = %v, want [t2]", got.SkippedTasks)
	}
	// 打包入参：只含已完成任务的源，tar 条目名保留任务目录。
	if len(targets.multiSources) != 1 || targets.multiSources[0].TarRel != "t1/a.bin" {
		t.Fatalf("打包源 = %+v，期望仅 t1/a.bin", targets.multiSources)
	}
	if targets.multiOutRel != "archive/g1-archive.tar.gz" {
		t.Fatalf("输出 rel = %q，期望 archive/g1-archive.tar.gz", targets.multiOutRel)
	}
	// 配额：按源物理大小（5 字节）预留 → 按实际结算 → 按实际登记；顺序即语义。
	if quotaStub.reserveOwner != "alice" || quotaStub.reserveTotal != 5 {
		t.Fatalf("预留入参 = (owner=%q, total=%d)，期望 (alice, 5)", quotaStub.reserveOwner, quotaStub.reserveTotal)
	}
	wantCalls := []string{"reserve", "settle"}
	if strings.Join(quotaStub.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("配额调用顺序 = %v，期望 %v", quotaStub.calls, wantCalls)
	}
	if quotaStub.released {
		t.Fatal("成功路径不得释放预留（必须 Commit 结算）")
	}
	// 占用登记（删除时按登记释放的事实源）随 S3c 迁入 Service：直接查登记值。
	if got := svc.ArchiveUsage("alice", "g1-archive.tar.gz"); got != 1234 {
		t.Fatalf("占用登记 = %d，期望 1234", got)
	}
	// 组归档名落库（供组删除联动清理）。
	if groups.setArchiveID != "g1" || groups.setArchiveFile != "g1-archive.tar.gz" {
		t.Fatalf("SetGroupArchiveFile = (%q,%q)，期望 (g1,g1-archive.tar.gz)", groups.setArchiveID, groups.setArchiveFile)
	}
}

// TestArchiveGroup_ACLRejectedBeforeOpeningSources 锁转存目标的 ACL 判定**早于**任何源文件
// 打开：ACL 拒绝回 403，且不打开 Reader、不进入转存核心（否则会泄漏 403 之外的错误面）。
func TestArchiveGroup_ACLRejectedBeforeOpeningSources(t *testing.T) {
	t.Parallel()
	svc, groups, _, _ := newGroupArchiveEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/archive",
		strings.NewReader(`{"archive_name":"g1-archive.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", "g1")

	svc.ArchiveGroup(rec, req, "alice")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	var got CloudArchiveResult
	decodeBody(t, rec, &got)
	if got.Success || got.Message != "目标卷对当前用户不可用（ACL 拒绝）" {
		t.Fatalf("响应 = %+v，期望 ACL 拒绝文案", got)
	}
	if groups.setArchiveFile != "" {
		t.Fatalf("ACL 拒绝不得登记组归档名，got %q", groups.setArchiveFile)
	}
}
