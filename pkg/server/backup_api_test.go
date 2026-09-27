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

	"github.com/cocomhub/sproxy/pkg/quota"
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
	req.Header.Set("Content-Type", "application/json")
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

// TestBackupTargetFS_UnknownVolume 单测 backupTargetFS：未知卷名 → 明确装配错误
// （fail-closed，不静默回落默认卷）。
func TestBackupTargetFS_UnknownVolume(t *testing.T) {
	t.Parallel()
	h, _ := newBackupTestServer(t, nil)
	fs, err := h.backupTargetFS(t.Context(), "", "ghost-vol")
	if err == nil || fs != nil {
		t.Fatalf("未知卷应装配失败: fs=%v err=%v", fs, err)
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

	if err := q.WriteFile(context.Background(), "a.txt", strings.NewReader("12345"), 5, 0); err != nil {
		t.Fatalf("5B 写入应成功: %v", err)
	}
	if err := q.WriteFile(context.Background(), "b.txt", strings.NewReader(strings.Repeat("x", 20)), 20, 0); err == nil {
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

func (m *backupMemFS) WriteFile(_ context.Context, rel string, r io.Reader, _ int64, _ int64) error {
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
