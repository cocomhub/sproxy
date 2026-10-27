// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backup_api_test.go 覆盖备份服务端端点（roadmap 12.2-3 备份 P2）：
//  1. POST /api/backup 往返 + 增量：源=默认卷 user 桶 → 目标=第二本地卷；
//     首次全量（Files=2、目标落盘、内容一致）+ 二次增量（Skipped=2、Files=0——
//     变异：引擎 manifest 比对失效 → Skipped=0 → 红）。
//  2. 部分失败：目标配额不足（owner user 桶 Scope 上限）→ 超限文件 Failed + 其余
//     成功（HTTP 200 + Report JSON，业务完成、部分失败语义显式化；变异：配额绕过
//     → 超限文件也成功 → 红）。
//  3. 权限：认证驱动服务器无签名请求 → 401；ACL 拒绝卷 → 403。
//  4. 目标卷装配失败（未知卷）→ 显式报错不静默。
//
// 约束：纯标准库断言；httptest（127.0.0.1 回环）；client 用 testHTTPClient(t)
// （独立连接池，禁共享 http.DefaultClient）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newBackupTestServer 启动带第二本地卷（backup-vol）的完整路由测试服务器。
// 返回 *Handlers 与 baseURL（本文件用例需要 h 直读目标卷落盘核对）。
func newBackupTestServer(t *testing.T, modifyCfg func(*Config)) (*Handlers, string) {
	t.Helper()
	dir := t.TempDir()
	backupDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dir},
		{Name: "backup-vol", Root: backupDir},
	}
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
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
	return h, ts.URL
}

// doBackup 发起 POST /api/backup（无签名——no-auth 测试环境走 AllowInsecureLoopback）。
func doBackup(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/api/backup", &buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set(headerContentType, contentTypeJSON)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("do backup: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// readVolFile 读指定卷上 anonymous 租户 user 桶文件内容（"" = 不存在）。
func readVolFile(t *testing.T, h *Handlers, vol, name string) string {
	t.Helper()
	tnt := h.volumeTenant(vol, "")
	if tnt == nil {
		t.Fatalf("卷 %q 租户不可用", vol)
	}
	userAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		t.Fatalf("卷 %q user 根不可用", vol)
	}
	data, err := os.ReadFile(filepath.Join(userAbs, filepath.FromSlash(name)))
	if err != nil {
		return ""
	}
	return string(data)
}

// TestBackupAPI_FullThenIncremental 断言端点往返 + 增量语义：
// 首次全量（目标落盘 + 内容一致）→ 二次增量（manifest 命中 skip，不重复传输）。
// 变异点：引擎 readManifest 失效（返回空）→ 二次 Skipped=0 → 红。
func TestBackupAPI_FullThenIncremental(t *testing.T) {
	t.Parallel()
	h, url := newBackupTestServer(t, nil)
	writeUserFile(t, h, "", "a.txt", "hello")
	writeUserFile(t, h, "", "sub/b.txt", "world")

	status, body := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status != http.StatusOK {
		t.Fatalf("首次备份 status = %d, want 200（body=%s）", status, body)
	}
	var rep1 backupAPIReport
	if err := json.Unmarshal(body, &rep1); err != nil {
		t.Fatalf("解析 Report 失败: %v", err)
	}
	if rep1.Files != 2 || rep1.Skipped != 0 || rep1.Failed != 0 {
		t.Fatalf("首次全量应 Files=2 Skipped=0 Failed=0: %+v", rep1)
	}
	if got := readVolFile(t, h, "backup-vol", "a.txt"); got != "hello" {
		t.Errorf("backup-vol/a.txt = %q, want hello", got)
	}
	if got := readVolFile(t, h, "backup-vol", "sub/b.txt"); got != "world" {
		t.Errorf("backup-vol/sub/b.txt = %q, want world", got)
	}

	// 二次：manifest 命中 → 全部跳过。
	status2, body2 := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status2 != http.StatusOK {
		t.Fatalf("增量备份 status = %d, want 200（body=%s）", status2, body2)
	}
	var rep2 backupAPIReport
	if err := json.Unmarshal(body2, &rep2); err != nil {
		t.Fatalf("解析 Report 失败: %v", err)
	}
	if rep2.Skipped != 2 || rep2.Files != 0 {
		t.Fatalf("增量应 Skipped=2 Files=0: %+v", rep2)
	}
}

// TestBackupAPI_QuotaPartialFailure 断言：目标配额不足（owner user 桶 Scope 上限）→
// 超限文件 Failed + 其余成功；HTTP 200 + Report JSON（部分失败语义显式化）。
// 变异点：配额绕过（backupQuotaFS 不 TryReserve）→ 超限文件也成功 → 红。
func TestBackupAPI_QuotaPartialFailure(t *testing.T) {
	t.Parallel()
	h, url := newBackupTestServer(t, func(c *Config) {
		// owner_quotas["anonymous"] = 150 字节 → small(5B)+manifest(~90B) 成功；big(200B) 超限。
		c.OwnerQuotas = map[string]ByteSize{"anonymous": 150}
	})
	writeUserFile(t, h, "", "small.txt", "12345")
	writeUserFile(t, h, "", "big.txt", strings.Repeat("x", 200))

	status, body := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status != http.StatusOK {
		t.Fatalf("部分失败应 200 + Report, got %d（body=%s）", status, body)
	}
	var rep backupAPIReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("解析 Report 失败: %v（body=%s）", err, body)
	}
	if rep.Files != 1 || rep.Failed != 1 {
		t.Fatalf("应 1 成功 1 失败: %+v", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "big.txt") {
		t.Fatalf("失败明细应含 big.txt: %+v", rep.Errors)
	}
	if got := readVolFile(t, h, "backup-vol", "small.txt"); got != "12345" {
		t.Errorf("small.txt 应已备份，got %q", got)
	}
	if got := readVolFile(t, h, "backup-vol", "big.txt"); got != "" {
		t.Errorf("big.txt 不应备份（配额拒绝），got %q", got)
	}
}

// TestBackupAPI_ForbiddenTarget 断言：ACL 拒绝的目标卷 → 403（权限收口）。
func TestBackupAPI_ForbiddenTarget(t *testing.T) {
	t.Parallel()
	_, url := newBackupTestServer(t, func(c *Config) {
		for i := range c.Volumes {
			if c.Volumes[i].Name == "backup-vol" {
				c.Volumes[i].ACL = &VolumeACLConfig{Mode: "deny", Owners: []string{"anonymous"}}
			}
		}
	})
	status, body := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status != http.StatusForbidden {
		t.Fatalf("ACL 拒绝 target status = %d, want 403（body=%s）", status, body)
	}
}

// TestBackupAPI_Unauthenticated 断言：认证驱动服务器（凭据 Ring 非空）无签名请求 → 401。
func TestBackupAPI_Unauthenticated(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutesCreds(t, nil)
	status, _ := doBackup(t, url, map[string]any{"target": "default"})
	if status != http.StatusUnauthorized {
		t.Fatalf("无签名 status = %d, want 401", status)
	}
}

// backupBucketFile 读目标卷租户根下 backup 功能桶内的文件（"" = 不存在）。
func backupBucketFile(t *testing.T, h *Handlers, vol, name string) string {
	t.Helper()
	tnt := h.volumeTenant(vol, "")
	if tnt == nil {
		t.Fatalf("卷 %q 租户不可用", vol)
	}
	rootAbs, ok := tnt.Root().Abs("")
	if !ok {
		t.Fatalf("卷 %q 租户根不可用", vol)
	}
	data, err := os.ReadFile(filepath.Join(rootAbs, backupManifestBucket, filepath.FromSlash(name)))
	if err != nil {
		return ""
	}
	return string(data)
}

// TestBackupAPI_ManifestIsolatedInBackupBucket 回归（P1，备份 manifest 隔离）：manifest
// 必须落在目标卷租户根的 **backup 功能桶**（用户 API 不可寻址），而不是 user 桶里的
// `backup/manifest.json`（那是用户可覆写/伪造的路径 → 增量静默跳过 + 覆盖同名用户文件）。
// 变异：ManifestFS 沿用 dst（或未传 ManifestFS/ManifestRel）→ manifest 落 user 桶 → 红。
func TestBackupAPI_ManifestIsolatedInBackupBucket(t *testing.T) {
	t.Parallel()
	h, url := newBackupTestServer(t, nil)
	writeUserFile(t, h, "", "a.txt", "hello")

	status, body := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status != http.StatusOK {
		t.Fatalf("备份 status = %d, want 200（body=%s）", status, body)
	}
	if got := backupBucketFile(t, h, "backup-vol", backupManifestKey); got == "" {
		t.Fatalf("manifest 应落在目标卷 backup 功能桶（变异：沿用 dst → 红）")
	}
	// user 桶不得出现 manifest（旧落点 = 用户可写路径）。
	if got := readVolFile(t, h, "backup-vol", "backup/manifest.json"); got != "" {
		t.Fatalf("user 桶不应出现 manifest（用户可覆写/伪造路径）: %q", got)
	}
}

// TestBackupAPI_SelfTargetRejected 断言自备份（目标与源为同一卷）→ 400 显式拒绝
// （否则 manifest/数据回灌源命名空间、目标产物被再次枚举）。
func TestBackupAPI_SelfTargetRejected(t *testing.T) {
	t.Parallel()
	h, url := newBackupTestServer(t, nil)
	writeUserFile(t, h, "", "a.txt", "hello")

	// 源缺省 = 默认卷（main），目标显式给同一卷。
	status, body := doBackup(t, url, map[string]any{"target": "main"})
	if status != http.StatusBadRequest {
		t.Fatalf("自备份应 400，got %d（body=%s）", status, body)
	}
	status2, body2 := doBackup(t, url, map[string]any{"target": "main", "source": "main"})
	if status2 != http.StatusBadRequest {
		t.Fatalf("显式自备份应 400，got %d（body=%s）", status2, body2)
	}
}

// TestBackupTargetFS_UnknownVolume 单测 backupTargetFS：未知卷名 → 明确装配错误
// （fail-closed，不静默回落默认卷）。
func TestBackupTargetFS_UnknownVolume(t *testing.T) {
	t.Parallel()
	h, _ := newBackupTestServer(t, nil)
	tgt, err := h.backupTargetFS(t.Context(), "", "ghost-vol")
	if err == nil || tgt.data != nil || tgt.manifest != nil {
		t.Fatalf("未知卷应装配失败: tgt=%+v err=%v", tgt, err)
	}
	if !strings.Contains(err.Error(), "ghost-vol") {
		t.Errorf("错误应含卷名: %v", err)
	}
}

// TestBackupQuotaFS_ReserveFailureIsolated 单测 backupQuotaFS：配额不足 → 该文件
// 返回错误（不写穿），其它文件不受影响（与 syncexec.quotaLocalFS 同构）。
func TestBackupQuotaFS_ReserveFailureIsolated(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(5)
	scope := pool.Scope("/tenant/test", 5)
	inner := newBackupMemFS()
	q := &backupQuotaFS{inner: inner, scope: scope, pool: nil}

	if err := q.WriteFile(context.Background(), syncpkg.LogicalKey{}, "a.txt", strings.NewReader("12345"), 5, 0); err != nil {
		t.Fatalf("5B 写入应成功: %v", err)
	}
	if err := q.WriteFile(context.Background(), syncpkg.LogicalKey{}, "b.txt", strings.NewReader(strings.Repeat("x", 20)), 20, 0); err == nil {
		t.Fatal("20B 超出 5B 上限应被拒绝（配额绕过 → 红）")
	}
	if inner.has("b.txt") {
		t.Errorf("b.txt 不应落盘（配额拒绝）")
	}
	if got := inner.get("a.txt"); got != "12345" {
		t.Errorf("a.txt 内容 = %q, want 12345", got)
	}
}

// backupMemFS 是最小内存 sync.FS（backupQuotaFS 单测用）。
type backupMemFS struct {
	files map[string]string
}

func newBackupMemFS() *backupMemFS {
	return &backupMemFS{files: map[string]string{}}
}

func (m *backupMemFS) has(p string) bool {
	_, ok := m.files[p]
	return ok
}

func (m *backupMemFS) get(p string) string { return m.files[p] }

func (m *backupMemFS) WriteFile(_ context.Context, key syncpkg.LogicalKey, rel string, r io.Reader, _ int64, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.files[rel] = string(data)
	return nil
}

func (m *backupMemFS) ListDir(_ context.Context, _ string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *backupMemFS) Stat(_ context.Context, _ string) (*syncpkg.Entry, error)     { return nil, nil }
func (m *backupMemFS) OpenRead(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (m *backupMemFS) Rename(_ context.Context, _, _ string) error { return nil }
func (m *backupMemFS) Delete(_ context.Context, _ string) error    { return nil }
func (m *backupMemFS) MakeDir(_ context.Context, _ string) error   { return nil }

// _ 编译期断言：backupMemFS 实现 sync.FS。
var _ syncpkg.FS = (*backupMemFS)(nil)

// backupSizedMemFS 带 Stat 的内存 FS（覆盖写差分测试用）。
type backupSizedMemFS struct{ backupMemFS }

func (m *backupSizedMemFS) Stat(_ context.Context, rel string) (*syncpkg.Entry, error) {
	s, ok := m.files[rel]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &syncpkg.Entry{Path: rel, Name: rel, Size: int64(len(s))}, nil
}
func (m *backupSizedMemFS) Delete(_ context.Context, rel string) error {
	delete(m.files, rel)
	return nil
}
func (m *backupSizedMemFS) Rename(_ context.Context, from, to string) error {
	if s, ok := m.files[from]; ok {
		m.files[to] = s
		delete(m.files, from)
	}
	return nil
}

// TestBackupQuotaFS_OverwriteReleasesPrev 第 4 轮对抗评审 P1 回归：备份目标覆盖写必须
// 按 prev 差分（否则每轮备份同 rel 单调累加 → 假 507）。
func TestBackupQuotaFS_OverwriteReleasesPrev(t *testing.T) {
	t.Parallel()
	ownerPool := quota.NewPool(1000)
	scope := ownerPool.Scope("/tenant/test", 1000)
	volPool := quota.NewPool(1000)
	inner := &backupSizedMemFS{backupMemFS{files: map[string]string{}}}
	q := &backupQuotaFS{inner: inner, scope: scope, pool: volPool}
	ctx := context.Background()
	for i := range 3 {
		if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader(strings.Repeat("x", 10)), 10, 0); err != nil {
			t.Fatalf("第 %d 次覆盖写: %v", i+1, err)
		}
	}
	if got := scope.Usage(); got != 10 {
		t.Fatalf("覆盖写后 Scope.Usage=%d want 10（旧字节必须释放）", got)
	}
	if got := volPool.Usage(); got != 10 {
		t.Fatalf("覆盖写后 volPool.Usage=%d want 10", got)
	}
	// 改名到新名（覆盖既有 30B 目标）→ 目标旧字节释放。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "b.txt", strings.NewReader(strings.Repeat("y", 30)), 30, 0); err != nil {
		t.Fatal(err)
	}
	if err := q.Rename(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	// 改名把源 10B 搬到 b（覆盖 b 原 30B）：源字节已在写时计入，故只释放目标旧字节 →
	// 账面 = 10（恰为 b.txt 实际内容），既不双计也不漏计。
	if got := scope.Usage(); got != 10 {
		t.Fatalf("改名覆盖后 Usage=%d want 10", got)
	}
	// 删除释放。
	if err := q.Delete(ctx, "b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("删除后 Usage=%d want 0", got)
	}
}

// TestBackupQuotaFS_DurableCapacityLedger 验证本地备份写面同步更新**持久容量账本**
// （capacity.StorageManager）：写入 / 覆盖写差分 / 删除 / 改名覆盖四路记账对称，
// 超限拒绝不产生预留泄漏（账本漂移）。
func TestBackupQuotaFS_DurableCapacityLedger(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 100, nil, testLogger())
	inner := &backupSizedMemFS{backupMemFS{files: map[string]string{}}}
	q := &backupQuotaFS{inner: inner, sm: sm, cat: capacity.CategoryChunked, waitTimeout: 50 * time.Millisecond}
	ctx := context.Background()
	usage := func() int64 { return sm.UsageByCategory()[capacity.CategoryChunked] }

	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader(strings.Repeat("x", 40)), 40, 0); err != nil {
		t.Fatalf("40B 写入: %v", err)
	}
	if got := usage(); got != 40 {
		t.Fatalf("写入后持久账本=%d want 40（本地备份写面未记账 → 红）", got)
	}
	// 覆盖写差分：同 rel 覆盖为 10B → 账本 10（旧字节释放，不累加）。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader(strings.Repeat("y", 10)), 10, 0); err != nil {
		t.Fatal(err)
	}
	if got := usage(); got != 10 {
		t.Fatalf("覆盖写后持久账本=%d want 10（旧字节未释放）", got)
	}
	// 新文件 50 → 60。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "b.txt", strings.NewReader(strings.Repeat("z", 50)), 50, 0); err != nil {
		t.Fatal(err)
	}
	if got := usage(); got != 60 {
		t.Fatalf("新文件后持久账本=%d want 60", got)
	}
	// 超限（剩余 40，写 60）→ 拒绝且账本不变（预留已回退，无泄漏）。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "c.txt", strings.NewReader(strings.Repeat("w", 60)), 60, 0); err == nil {
		t.Fatal("超限写应被拒绝")
	}
	if got := usage(); got != 60 {
		t.Fatalf("拒绝后持久账本=%d want 60（预留泄漏）", got)
	}
	// 删除释放 → 10。
	if err := q.Delete(ctx, "b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := usage(); got != 10 {
		t.Fatalf("删除后持久账本=%d want 10", got)
	}
	// 覆盖改名（目标旧字节释放；源字节已在写时计入）→ 仍 10。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "d.txt", strings.NewReader(strings.Repeat("q", 30)), 30, 0); err != nil {
		t.Fatal(err)
	}
	if err := q.Rename(ctx, "a.txt", "d.txt"); err != nil {
		t.Fatal(err)
	}
	if got := usage(); got != 10 {
		t.Fatalf("改名覆盖后持久账本=%d want 10", got)
	}
}

// TestBackupAPI_LocalTargetDurableCapacityLedger 端到端：备份到**本地卷**后持久容量账本
// 与实际落盘字节一致；随后增量备份（全部 skip）**不产生账本漂移**。
func TestBackupAPI_LocalTargetDurableCapacityLedger(t *testing.T) {
	t.Parallel()
	h, url := newBackupTestServer(t, nil)
	sm := capacity.NewStorageManager(t.TempDir(), 1<<20, nil, testLogger())
	h.storageMgr = sm
	writeUserFile(t, h, "", "a.txt", "hello")
	writeUserFile(t, h, "", "sub/b.txt", "world!")

	if status, body := doBackup(t, url, map[string]any{"target": "backup-vol"}); status != http.StatusOK {
		t.Fatalf("备份 status=%d want 200（body=%s）", status, body)
	}
	cat := sm.UsageByCategory()[capacity.CategoryChunked]
	if cat != 11 {
		t.Fatalf("备份后持久账本=%d want 11（hello=5 + world!=6）", cat)
	}
	// 增量备份：全部 skip → 账本不变（覆盖写差分无漂移）。
	if status, body := doBackup(t, url, map[string]any{"target": "backup-vol"}); status != http.StatusOK {
		t.Fatalf("增量备份 status=%d want 200（body=%s）", status, body)
	}
	if got := sm.UsageByCategory()[capacity.CategoryChunked]; got != 11 {
		t.Fatalf("增量备份后持久账本=%d want 11（漂移 → 红）", got)
	}
}

// TestBackupAPI_EncryptedSourceBacksUpPlaintext 评审 P0 残余闭环：备份源此前是**裸 LocalFS**
// （`syncpkg.NewLocalFS(srcAbs)` 纯 os I/O）⇒ 加密卷源会读到**密文**，故代码里显式 409 拒绝
// （"加密卷暂不支持作为备份源（待接入经 Root 的备份源）"）。本用例钉住新语义：源经
// `NewPrefixFS(NewRootFS(tnt.Root()), "user/")` ⇒ 透明解密 + 封印语义 ⇒ 加密卷源可用，
// 且产物是**明文**（备份产物可用，而非无法还原的密文）。
//
// 变异：源回退裸 LocalFS → 产物为密文（`readVolFile` != 明文）→ 红；保留 409 拒绝 → 红。
func TestBackupAPI_EncryptedSourceBacksUpPlaintext(t *testing.T) {
	t.Parallel()
	keyFile := filepath.Join(t.TempDir(), "src.key")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte("k"), 32), 0o600); err != nil {
		t.Fatal(err)
	}
	h, url := newBackupTestServer(t, func(c *Config) {
		c.Volumes[0].Extra = map[string]any{"encrypt": true, "encrypt_key_file": keyFile}
	})
	// 经**用户可见入口**写入（加密卷上落密文）。
	srcTnt := h.volumeTenant("main", "")
	if srcTnt == nil || srcTnt.Root() == nil {
		t.Fatal("源卷租户不可用")
	}
	if err := srcTnt.Root().MkdirAll("user/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	w, werr := writeUserVisible(srcTnt.Root(), "user/sub/secret.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("writeUserVisible: %v", werr)
	}
	if _, cerr := w.Write([]byte("top secret body")); cerr != nil {
		t.Fatal(cerr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	// 前置条件：源卷磁盘上是密文（否则本用例失去意义）。
	if raw := readVolFile(t, h, "main", "sub/secret.txt"); raw == "top secret body" {
		t.Fatal("前置条件不成立：加密源卷磁盘上应为密文")
	}
	status, body := doBackup(t, url, map[string]any{"target": "backup-vol"})
	if status != http.StatusOK {
		t.Fatalf("加密卷作为备份源应成功, status=%d body=%s", status, body)
	}
	var rep backupAPIReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("解析 Report: %v", err)
	}
	if rep.Files != 1 || rep.Failed != 0 {
		t.Fatalf("应备份 1 个文件且无失败: %+v", rep)
	}
	if got := readVolFile(t, h, "backup-vol", "sub/secret.txt"); got != "top secret body" {
		t.Fatalf("备份产物必须是**明文**（源经 Root 透明解密）, got %q", got)
	}
}
