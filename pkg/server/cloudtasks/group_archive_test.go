// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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
// tar.gz 打包在 S4 后由本包执行，故此处直接校验真实产物（条目名/内容/checksum）；
// 转存执行核心（transfer.go）仍由 pkg/server 的既有 HTTP 测试驱动。

// stubTenants 返回固定租户（nil 模拟租户不可用）。
type stubTenants struct {
	tenant *storage.Tenant
}

func (s stubTenants) TenantFor(string) *storage.Tenant { return s.tenant }

// readRootFile 读取 root 内 rel 文件的全部字节（验证真实落盘产物）。
func readRootFile(t *testing.T, root *storage.Root, rel string) []byte {
	t.Helper()
	f, err := root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %q: %v", rel, err)
	}
	defer f.Close()
	raw, readErr := io.ReadAll(f)
	if readErr != nil {
		t.Fatalf("read %q: %v", rel, readErr)
	}
	return raw
}

// stubArchiveTargets 实现 Targets 中不涉及打包的部分（S4 后 tar 打包已在本包，不再经接口）。
// 零值 = 不跳过校验、无总量上限、无归档密钥、取锁恒成功、staging 直通。
type stubArchiveTargets struct {
	lockFail        bool
	skipVerify      bool
	maxArchiveBytes int64
	archiveKey      []byte
	stagingFS       syncpkg.FS

	checksumCalls []bypassChecksumCall
	writeCalls    []bypassWriteCall
}

// bypassChecksumCall / bypassWriteCall 记录派生状态登记入参（本地卷只补 checksum，
// 外部卷走完整 Write 登记——分流依据必须可取证）。
type bypassChecksumCall struct {
	owner, rel, checksum, volume string
}

type bypassWriteCall struct {
	owner, rel string
	size       int64
	checksum   string
	volume     string
	rootNil    bool
}

func (s *stubArchiveTargets) StagingFS(_ string, fs syncpkg.FS) syncpkg.FS {
	if s.stagingFS != nil {
		return s.stagingFS
	}
	return fs
}

func (s *stubArchiveTargets) Lock(string, string) (func(), bool) {
	if s.lockFail {
		return func() {}, false
	}
	return func() {}, true
}

func (s *stubArchiveTargets) RecordBypassChecksum(owner, rel, checksum, volume string) {
	s.checksumCalls = append(s.checksumCalls, bypassChecksumCall{owner: owner, rel: rel, checksum: checksum, volume: volume})
}

func (s *stubArchiveTargets) RecordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) {
	s.writeCalls = append(s.writeCalls, bypassWriteCall{owner: owner, rel: rel, size: size, checksum: checksum, volume: volume, rootNil: root == nil})
}

func (s *stubArchiveTargets) SkipVerify() bool { return s.skipVerify }

func (s *stubArchiveTargets) MaxArchiveBytes() int64 { return s.maxArchiveBytes }

func (s *stubArchiveTargets) ArchiveKey() []byte { return s.archiveKey }

// stubArchiveQuota 记录归档配额四方法的调用顺序（预留 → 结算 → 登记）。
type stubArchiveQuota struct {
	reservePre    int64
	reserveStatus int
	reserveMsg    string
	reserveOwner  string
	reserveTotal  int64
	settleActual  int64
	settleStatus  int
	settleMsg     string
	released      bool

	// bucketScope/scope 是 BucketScope/ScopeFor 的返回（nil = 未装配配额）。
	bucketScope *quota.Scope
	scope       *quota.Scope

	calls []string
}

func (s *stubArchiveQuota) BucketScope(string, string) *quota.Scope { return s.bucketScope }

func (s *stubArchiveQuota) ScopeFor(string, string) *quota.Scope { return s.scope }

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
	return s.settleActual, s.settleStatus, s.settleMsg
}

func (s *stubArchiveQuota) ReleaseArchive(*quota.Settlement, int64) {
	s.calls = append(s.calls, "release")
	s.released = true
}

// newGroupArchiveEnv 装配组归档用例：真实租户（含 cloud/t1/a.bin）+ 组含 1 个已完成与
// 1 个进行中的子任务。返回租户根，供用例直接验证真实 tar.gz 产物。
func newGroupArchiveEnv(t *testing.T) (*Service, *stubGroups, *storage.Root, *stubArchiveQuota) {
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
	targets := &stubArchiveTargets{}
	quotaStub := &stubArchiveQuota{reservePre: 100, settleActual: 1234}
	svc := New(Deps{
		Groups: groups, Tasks: tasks, Tenants: stubTenants{tenant: tenant},
		Volumes: stubVolumes{}, Quota: quotaStub, Targets: targets,
	})
	return svc, groups, tenantRoot, quotaStub
}

// TestArchiveGroup_SkipsIncompleteTasks 锁组归档（落 archive 桶）的响应字段与调用顺序：
// 未完成子任务进 skipped 列表，配额按源文件**物理大小**预留、按落盘实际结算、按实际登记，
// 组归档名落库，checksum/大小/计数逐字回包。
func TestArchiveGroup_SkipsIncompleteTasks(t *testing.T) {
	t.Parallel()
	svc, groups, tenantRoot, quotaStub := newGroupArchiveEnv(t)

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
	if !got.Success || got.File != "g1-archive.tar.gz" ||
		got.Size != 1234 || got.TaskCount != 1 || got.SkippedCount != 1 {
		t.Fatalf("响应 = %+v，期望 1 个文件 + 1 个跳过 + 实际大小 1234", got)
	}
	if len(got.SkippedTasks) != 1 || got.SkippedTasks[0] != "t2" {
		t.Fatalf("skipped_tasks = %v, want [t2]", got.SkippedTasks)
	}
	// 真实 tar.gz 产物（S4 后打包在本包执行）：条目名保留任务目录、内容为源文件，
	// 响应 checksum 必须等于落盘产物的 SHA-256。
	raw := readRootFile(t, tenantRoot, "archive/g1-archive.tar.gz")
	wantSum := sha256.Sum256(raw)
	if got.Checksum != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("checksum = %q，期望落盘产物的 SHA-256 %s", got.Checksum, hex.EncodeToString(wantSum[:]))
	}
	entries := tarEntries(t, raw)
	if len(entries) != 1 || entries["t1/a.bin"] != "aaaaa" {
		t.Fatalf("归档条目 = %v，期望仅 t1/a.bin=aaaaa（未完成任务不得入包）", entries)
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
