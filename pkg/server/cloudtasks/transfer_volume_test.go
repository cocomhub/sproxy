// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// transfer_volume_test.go 钉住统一转存执行核心（transfer.go 的 archiveToVolume 及其
// 记账/登记/删除同源助手）的包内契约：写前门（容量预检 + 取锁）、写前配额探针、写后
// owner 配额差分结算与卷级容量池结算、本地卷 checksum 登记、审计、以及删除侧的同源释放。
//
// 失败面按「状态码 + 文案」逐条锁定（这些文案是既有 HTTP 契约：改文案即客户端行为变化），
// 成功面锁定「写前探针拒绝 ⇒ 不写目标」「写前测量必须与删除侧同一函数」两个不变量。
package cloudtasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// memFS 是转存目标卷的内存实现（sync.FS + WriteIfAbsent + ReserveSpace + meta.Provider）。
//
// 四个可选能力都实现是有意的：生产装配的装饰器链（trusted/capacity/staging）无条件
// 实现它们，本替身据此覆盖「能力命中」分支；「能力未命中」分支由 plainFS 包装覆盖。
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte
	metas map[string]string

	writeErr   error // WriteFile/WriteIfAbsent 失败注入
	reserveErr error // ReserveSpace 返回（syncpkg.ErrUnsupported = 跳过）
	statErr    error // Stat 失败注入
	deleteErr  error // Delete 失败注入
	writeCount int
	reserveCts []int64
}

func newMemFS() *memFS {
	return &memFS{files: map[string][]byte{}, metas: map[string]string{}}
}

func (m *memFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }

func (m *memFS) Stat(_ context.Context, path string) (*syncpkg.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statErr != nil {
		return nil, m.statErr
	}
	b, ok := m.files[path]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: filepath.Base(path), Path: path, Size: int64(len(b))}, nil
}

func (m *memFS) OpenRead(_ context.Context, path string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

func (m *memFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, path string, r io.Reader, _ int64, _ int64) error {
	return m.write(path, r)
}

func (m *memFS) WriteIfAbsent(_ context.Context, path string, r io.Reader, _ int64, _ int64) (bool, error) {
	m.mu.Lock()
	_, exists := m.files[path]
	m.mu.Unlock()
	if exists {
		return false, nil
	}
	if err := m.write(path, r); err != nil {
		return false, err
	}
	return true, nil
}

func (m *memFS) write(path string, r io.Reader) error {
	m.mu.Lock()
	if m.writeErr != nil {
		err := m.writeErr
		m.mu.Unlock()
		return err
	}
	m.writeCount++
	m.mu.Unlock()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = b
	return nil
}

func (m *memFS) Rename(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[from]
	if !ok {
		return fs.ErrNotExist
	}
	delete(m.files, from)
	m.files[to] = b
	return nil
}

func (m *memFS) Delete(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.files, path)
	return nil
}

func (m *memFS) MakeDir(context.Context, string) error { return nil }

func (m *memFS) ReserveSpace(_ context.Context, _ string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserveCts = append(m.reserveCts, size)
	return m.reserveErr
}

func (m *memFS) FileMeta(_ context.Context, rel string) (*meta.FileMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sum, ok := m.metas[rel]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &meta.FileMeta{TotalSHA256: sum}, nil
}

func (m *memFS) data(path string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[path]
	return b, ok
}

// plainFS 只暴露 sync.FS 基础能力（类型断言全部落空）：覆盖「无 ReserveSpace / 无
// meta.Provider」的降级分支。
type plainFS struct{ inner *memFS }

func (p plainFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return p.inner.ListDir(ctx, path)
}
func (p plainFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return p.inner.Stat(ctx, path)
}
func (p plainFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return p.inner.OpenRead(ctx, path)
}
func (p plainFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	return p.inner.WriteFile(ctx, key, path, r, size, mtime)
}
func (p plainFS) Rename(ctx context.Context, from, to string) error {
	return p.inner.Rename(ctx, from, to)
}
func (p plainFS) Delete(ctx context.Context, path string) error  { return p.inner.Delete(ctx, path) }
func (p plainFS) MakeDir(ctx context.Context, path string) error { return p.inner.MakeDir(ctx, path) }

var (
	_ syncpkg.FS            = (*memFS)(nil)
	_ syncpkg.WriteIfAbsent = (*memFS)(nil)
	_ syncpkg.ReserveSpace  = (*memFS)(nil)
	_ meta.Provider         = (*memFS)(nil)
	_ syncpkg.FS            = plainFS{}
)

// newTransferEnv 装配统一转存用例：真实租户（cloud/t1/a.bin）+ 内存目标卷 + 可注入配额。
// 返回值齐全以便逐项断言（目标 FS、卷池、配额 stub、目标卷 stub、服务）。
type transferEnv struct {
	svc     *Service
	mem     *memFS
	targets *stubArchiveTargets
	vols    stubVolumes
	quota   *stubArchiveQuota
	audit   *stubAudit
	pool    *quota.Pool
	scope   *quota.Scope
}

func newTransferEnv(t *testing.T, opts ...func(*transferEnv)) *transferEnv {
	t.Helper()
	root := openTestRoot(t)
	tenant := openTestTenant(t, root, "alice")
	writeRootFile(t, tenant.Root(), "cloud/t1/a.bin", "aaaaa")

	mem := newMemFS()
	pool := quota.NewPool(0)
	scope := quota.NewPool(0).Scope("user", 0)
	env := &transferEnv{
		mem:     mem,
		targets: &stubArchiveTargets{skipVerify: true},
		vols: stubVolumes{
			fs:     mem,
			scheme: "vault",
			vol:    volume.Volume{Name: "vault"},
			pool:   pool,
		},
		quota: &stubArchiveQuota{scope: scope},
		audit: &stubAudit{},
		pool:  pool,
		scope: scope,
	}
	for _, opt := range opts {
		opt(env)
	}
	// 依赖全部经指针注入（Volumes 的 stub 用 &env.vols）：用例可在构造后替换目标卷/
	// 本地池/物理足迹，逐分支取证同一实例。
	env.svc = New(Deps{
		Tasks:   &stubTasks{},
		Groups:  &stubGroups{},
		Tenants: stubTenants{tenant: tenant},
		Volumes: &env.vols,
		Quota:   env.quota,
		Targets: env.targets,
		Audit:   env.audit,
		Logger:  slog.New(slog.DiscardHandler),
	})
	return env
}

// openTestRoot 建临时 storage.Root（Cleanup LIFO：先关租户句柄再关根、最后清目录）。
func openTestRoot(t *testing.T) *storage.Root {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// openTestTenant 在 root 下打开租户并登记句柄关闭（先于 root.Close 执行）。
func openTestTenant(t *testing.T, root *storage.Root, owner string) *storage.Tenant {
	t.Helper()
	tenant, err := storage.OpenTenant(root, owner)
	if err != nil {
		t.Fatalf("open tenant %q: %v", owner, err)
	}
	t.Cleanup(func() { _ = tenant.Root().Close() })
	return tenant
}

// writeRootFile 在 root 内写文件（自动建中间目录）。
func writeRootFile(t *testing.T, root *storage.Root, rel, payload string) {
	t.Helper()
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("create %q: %v", rel, err)
	}
	if _, wErr := f.Write([]byte(payload)); wErr != nil {
		_ = f.Close()
		t.Fatalf("write %q: %v", rel, wErr)
	}
	if cErr := f.Close(); cErr != nil {
		t.Fatalf("close %q: %v", rel, cErr)
	}
}

// transferReq 构造统一转存入参（单条目，Reader 由调用方在用例内打开）。
func transferReq(reader io.Reader, size int64) ArchiveVolumeTransfer {
	return ArchiveVolumeTransfer{
		Owner: "alice", TargetVolume: "vault", ArchiveName: "a.tar.gz",
		Entries:   []transfer.SourceEntry{{Rel: "a.bin", Size: size, Reader: io.NopCloser(reader)}},
		TotalSize: size,
	}
}

// decodeArchive 解析归档响应（失败即 Fatal）。
func decodeArchive(t *testing.T, rec *httptest.ResponseRecorder) CloudArchiveResult {
	t.Helper()
	var got CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON（%v）：%s", err, rec.Body.String())
	}
	return got
}

// TestArchiveToVolume_SuccessSettlesOwnerAndVolumePool 成功转存的不变量：
// 目标键恒带 owner 前缀（与读路径同键空间）、owner 配额按实测差分结算（Adjust）、
// 本地卷池按同源测量结算（Adjust）、本地卷只补 checksum 台账、审计留痕。
func TestArchiveToVolume_SuccessSettlesOwnerAndVolumePool(t *testing.T) {
	t.Parallel()
	var calls int
	env := newTransferEnv(t, func(e *transferEnv) {
		e.vols.local = true
		// 写前实测 0、写后实测 42：覆盖「写成功后按差分结算」。
		e.vols.footprintHook = func() (int64, bool) {
			calls++
			if calls == 1 {
				return 0, true
			}
			return 42, true
		}
		e.mem.metas["alice/user/archive/a.tar.gz"] = "sha-abc"
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/archive", nil)

	env.svc.archiveToVolume(rec, r, transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decodeArchive(t, rec)
	if !got.Success || got.File != "vault://vault/alice/user/archive/a.tar.gz" || got.TaskCount != 1 {
		t.Fatalf("响应 = %+v，期望成功 + 卷 URL + 1 个条目", got)
	}
	if _, ok := env.mem.data("alice/user/archive/a.tar.gz"); !ok {
		t.Fatal("目标键必须恒带 owner 前缀（volume.ResolveUserLocation 唯一入口）")
	}
	// owner 配额差分：写前 0（无足迹前的实测）→ 写后 42 ⇒ Adjust(0,42)。
	if usage := env.scope.Usage(); usage != 42 {
		t.Fatalf("owner 配额占用 = %d，期望按 archiveTargetCharge 实测 42", usage)
	}
	// 卷级容量池同源结算（本地卷）。
	if usage := env.pool.Usage(); usage != 42 {
		t.Fatalf("卷池占用 = %d，期望 42", usage)
	}
	// 本地卷只补 checksum（索引由 Root 事件维护），checksum 取可信卷 meta。
	if len(env.targets.checksumCalls) != 1 || env.targets.checksumCalls[0].checksum != "sha-abc" {
		t.Fatalf("checksum 登记 = %+v，期望 1 条 sha-abc", env.targets.checksumCalls)
	}
	if len(env.targets.writeCalls) != 0 {
		t.Fatalf("本地卷不得走完整 Write 登记（会重复 upsert 索引）：%+v", env.targets.writeCalls)
	}
	// 审计：cloud_archive/volume，成功且detail 含 scheme/key。
	if len(env.audit.calls) != 1 {
		t.Fatalf("审计 = %+v，期望 1 条", env.audit.calls)
	}
	call := env.audit.calls[0]
	if call.action != "cloud_archive" || call.objectType != "volume" || call.object != "vault" || call.result != auditResultSuccess {
		t.Fatalf("审计 = %+v", call)
	}
	if !strings.Contains(call.detail, "scheme=vault") || !strings.Contains(call.detail, "alice/user/archive/a.tar.gz") {
		t.Fatalf("审计 detail = %q，期望含 scheme 与目标键", call.detail)
	}
}

// TestArchiveToVolume_NonLocalSkipsLocalState 外部卷（非本地根）不登记本地 checksum
// （外部卷 list 走卷视图，checksum 由卷自报），也不结算卷池（CapacityFS 层已记，双记会重复占用）。
func TestArchiveToVolume_NonLocalSkipsLocalState(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/archive", nil)

	env.svc.archiveToVolume(rec, r, transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), func(url string) {
		if url != "vault://vault/alice/user/archive/a.tar.gz" {
			t.Errorf("onSuccess URL = %q", url)
		}
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(env.targets.writeCalls) != 0 {
		t.Fatalf("转存核心不得为外部卷做完整 Write 登记（由装配层 cloud manager 负责）：%+v", env.targets.writeCalls)
	}
	if len(env.targets.checksumCalls) != 0 {
		t.Fatalf("外部卷不得只补 checksum：%+v", env.targets.checksumCalls)
	}
	if usage := env.pool.Usage(); usage != 0 {
		t.Fatalf("外部卷不得结算本地卷池（双记），got %d", usage)
	}
}

// TestArchiveToVolume_FailureMatrix 逐条锁定失败面：状态码与文案是既有 HTTP 契约。
func TestArchiveToVolume_FailureMatrix(t *testing.T) {
	t.Parallel()

	t.Run("invalid compression", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		req := transferReq(strings.NewReader("x"), 1)
		req.Compression = "lzma"
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), req, slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown cipher", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		req := transferReq(strings.NewReader("x"), 1)
		req.Encrypt, req.Cipher = true, "not-a-cipher"
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), req, slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing archive key", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.svc.deps.Targets = &stubArchiveTargets{skipVerify: true}
		req := transferReq(strings.NewReader("x"), 1)
		req.Encrypt = true
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), req, slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(decodeArchive(t, rec).Message, "归档加密密钥不可用") {
			t.Fatalf("message 应提示 archive.key_file：%+v", decodeArchive(t, rec))
		}
	})

	t.Run("volume not assembled", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.vols.fs = nil
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("x"), 1), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("secrets volume rejected", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.vols.scheme = "secrets"
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("x"), 1), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid target path", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		req := transferReq(strings.NewReader("x"), 1)
		req.TargetPath = "../escape.tar.gz"
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), req, slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("exceeds max archive bytes", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.svc.deps.Targets = &stubArchiveTargets{skipVerify: true, maxArchiveBytes: 4}
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(decodeArchive(t, rec).Message, "cloud_archive_max_bytes") {
			t.Fatalf("message 应提示总量上限：%s", rec.Body.String())
		}
	})

	t.Run("capacity precheck fails", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.mem.reserveErr = errors.New("no space")
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("x"), 1), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, want 507: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("lock conflict", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.svc.deps.Targets = &stubArchiveTargets{skipVerify: true, lockFail: true}
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("x"), 1), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner quota probe rejects before write", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		full := quota.NewPool(10).Scope("user", 10)
		full.Adjust(0, 10)
		env.quota.scope = full
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("status = %d, want 507: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(decodeArchive(t, rec).Message, "目标卷配额不足") {
			t.Fatalf("message = %q", decodeArchive(t, rec).Message)
		}
		if env.mem.writeCount != 0 {
			t.Fatalf("写前探针拒绝后不得写入目标（got %d 次写）", env.mem.writeCount)
		}
	})

	t.Run("target already exists", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.mem.files["alice/user/archive/a.tar.gz"] = []byte("old")
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		// 审计仍必须留痕（失败也要可追溯）。
		if len(env.audit.calls) != 1 || env.audit.calls[0].result != auditResultError {
			t.Fatalf("审计 = %+v，期望 1 条 error", env.audit.calls)
		}
	})

	t.Run("transfer write failure", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.mem.writeErr = errors.New("disk full")
		rec := httptest.NewRecorder()

		env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(decodeArchive(t, rec).Message, "transfer failed") {
			t.Fatalf("message = %q", decodeArchive(t, rec).Message)
		}
	})
}

// TestArchiveToVolume_ReserveUnsupportedSkipsPrecheck ReserveSpace 未实现（ErrUnsupported）
// 必须降级放行而非当作容量不足（否则所有无容量概念的外部卷都被拒）。
func TestArchiveToVolume_ReserveUnsupportedSkipsPrecheck(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	env.mem.reserveErr = syncpkg.ErrUnsupported
	rec := httptest.NewRecorder()

	env.svc.archiveToVolume(rec, httptest.NewRequest(http.MethodPost, "/", nil), transferReq(strings.NewReader("hello"), 5), slog.New(slog.DiscardHandler), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("ErrUnsupported 应跳过容量预检，status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestArchiveVolumeWriteGate_NoReserveCapability 无 ReserveSpace 能力的目标 FS（裸 FS）直接放行，
// 且取锁恒被调用（写锁是与其它写面同键空间的并发保护）。
func TestArchiveVolumeWriteGate_NoReserveCapability(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	targets := &stubArchiveTargets{skipVerify: true}
	env.svc.deps.Targets = targets

	release, status, msg := env.svc.archiveVolumeWriteGate(t.Context(), plainFS{inner: env.mem}, "alice", "alice/user/archive/a.tar.gz", 5)
	if status != 0 || msg != "" {
		t.Fatalf("无 ReserveSpace 能力应放行，got (%d, %q)", status, msg)
	}
	if release == nil {
		t.Fatal("放行时必须返回非 nil release")
	}
	release()
}

// TestArchivePreprocessor_EncryptWithKey 加密预处理器选型：合法 cipher + 32 字节密钥 ⇒
// 返回非 nil（加密产物后缀逻辑由 archiveToVolume 控制）。
func TestArchivePreprocessor_EncryptWithKey(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	env.svc.deps.Targets = &stubArchiveTargets{skipVerify: true, archiveKey: []byte("0123456789abcdef0123456789abcdef")}
	rec := httptest.NewRecorder()

	pre, ok := env.svc.archivePreprocessor(rec, true, "aes-256-gcm", "zstd")
	if !ok || pre == nil {
		t.Fatalf("加密预处理器构造失败：code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestUserRelFromFSKeyAndArchiveTargetCharge archiveTargetCharge 的两级回落：
// ① 卷自报物理足迹优先（secretdata 的 pad/parity 不可推）；② 无足迹能力 ⇒ 卷内实测大小；
// ③ 无足迹且 Stat 失败 ⇒ 0（不编造占用）。
func TestUserRelFromFSKeyAndArchiveTargetCharge(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	env.vols.footprint = map[string]int64{"vault\x00alice\x00archive/a.tar.gz": 99}

	if got := env.svc.archiveTargetCharge("vault", "alice", "alice/user/archive/a.tar.gz", "archive/a.tar.gz"); got != 99 {
		t.Fatalf("有物理足迹时 = %d，want 99（必须优先于卷内实测）", got)
	}
	env.mem.files["alice/user/archive/b.tar.gz"] = []byte("1234567")
	if got := env.svc.archiveTargetCharge("vault", "alice", "alice/user/archive/b.tar.gz", "archive/b.tar.gz"); got != 7 {
		t.Fatalf("无足迹时 = %d，want 卷内实测 7", got)
	}
	if got := env.svc.archiveTargetCharge("missing", "alice", "k", "rel"); got != 0 {
		t.Fatalf("卷未装配时 = %d，want 0", got)
	}
}

// TestSettleVolumePoolCharge 卷池结算：仅本地卷且实际增长才 Adjust；外部卷/未增长/池缺失不改账。
func TestSettleVolumePoolCharge(t *testing.T) {
	t.Parallel()

	t.Run("local volume grows", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.vols.local = true
		env.svc.settleVolumePoolCharge("vault", 10, 25)
		if env.pool.Usage() != 15 {
			t.Fatalf("卷池 = %d，want 15（差分结算）", env.pool.Usage())
		}
	})

	t.Run("external volume skipped", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.svc.settleVolumePoolCharge("vault", 10, 25)
		if env.pool.Usage() != 0 {
			t.Fatalf("外部卷不得结算卷池，got %d", env.pool.Usage())
		}
	})

	t.Run("shrink and nil pool are no-ops", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.vols.local = true
		env.svc.settleVolumePoolCharge("vault", 25, 10)
		env.svc.settleVolumePoolCharge("vault", 0, 0)
		if env.pool.Usage() != 0 {
			t.Fatalf("收缩/零增长不得记账，got %d", env.pool.Usage())
		}
		env.vols.pool = nil
		env.svc.settleVolumePoolCharge("vault", 0, 5) // 池缺失不 panic
	})
}

// TestProbeArchiveQuota probeArchiveQuota 的跳过条件：nil scope（未装配配额）与 size<=0
// （打包流长度不可知）一律放行；只有可判定时才探测。
func TestProbeArchiveQuota(t *testing.T) {
	t.Parallel()
	if err := probeArchiveQuota(nil, 10); err != nil {
		t.Fatalf("nil scope 应放行：%v", err)
	}
	full := quota.NewPool(10).Scope("user", 10)
	full.Adjust(0, 10)
	if err := probeArchiveQuota(full, 0); err != nil {
		t.Fatalf("size<=0 应跳过探测：%v", err)
	}
	if err := probeArchiveQuota(full, 1); err == nil {
		t.Fatal("超限时应返回错误")
	}
}

// TestSettleArchiveCharge Adjust 差分语义：nil scope / 无变化为空操作，变化才记 diff。
func TestSettleArchiveCharge(t *testing.T) {
	t.Parallel()
	SettleArchiveCharge(nil, 0, 10) // 不 panic
	scope := quota.NewPool(0).Scope("user", 0)
	SettleArchiveCharge(scope, 5, 5)
	if scope.Usage() != 0 {
		t.Fatalf("无变化不得记账，got %d", scope.Usage())
	}
	SettleArchiveCharge(scope, 5, 12)
	if scope.Usage() != 7 {
		t.Fatalf("差分结算 = %d，want 7", scope.Usage())
	}
}

// TestRecordBypassWrite_SplitsLocalAndExternal 分流唯一入口：本地卷只补 checksum，
// 外部/远端卷走完整 Write 登记（checksum + 索引一次登记）。
func TestRecordBypassWrite_SplitsLocalAndExternal(t *testing.T) {
	t.Parallel()
	env := newTransferEnv(t)
	root := openTestRoot(t)

	env.svc.RecordBypassWrite("alice", "rel", 5, "sum", "vault", root)
	if len(env.targets.writeCalls) != 1 || len(env.targets.checksumCalls) != 0 {
		t.Fatalf("外部卷应走 Write 登记：writes=%+v checksums=%+v", env.targets.writeCalls, env.targets.checksumCalls)
	}

	env.vols.local = true
	env.svc.RecordBypassWrite("alice", "rel", 5, "sum", "vault", root)
	if len(env.targets.checksumCalls) != 1 || len(env.targets.writeCalls) != 1 {
		t.Fatalf("本地卷应只补 checksum：writes=%+v checksums=%+v", env.targets.writeCalls, env.targets.checksumCalls)
	}
}

// TestDeleteCloudArchive 删除归档桶文件：不存在视为幂等成功；按磁盘实测释放 Scope
// （实测缺失时按登记释放）；非法归档名/租户不可用返回错误。
func TestDeleteCloudArchive(t *testing.T) {
	t.Parallel()

	t.Run("missing file is idempotent", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		if err := env.svc.DeleteCloudArchive("alice", "nope.tar.gz"); err != nil {
			t.Fatalf("文件不存在应幂等成功：%v", err)
		}
	})

	t.Run("releases measured size and clears usage", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		tenant := env.svc.deps.Tenants.TenantFor("alice")
		writeRootFile(t, tenant.Root(), "archive/g1.tar.gz", "0123456789")
		bucket := quota.NewPool(0).Scope("archive", 0)
		bucket.Adjust(0, 10)
		env.quota.bucketScope = bucket
		env.svc.recordArchiveUsage("alice", "g1.tar.gz", 10)

		if err := env.svc.DeleteCloudArchive("alice", "g1.tar.gz"); err != nil {
			t.Fatalf("删除失败：%v", err)
		}
		if bucket.Usage() != 0 {
			t.Fatalf("桶占用 = %d，期望释放到 0", bucket.Usage())
		}
		if got := env.svc.ArchiveUsage("alice", "g1.tar.gz"); got != 0 {
			t.Fatalf("登记未清除：%d", got)
		}
		if _, ok := env.mem.data("archive/g1.tar.gz"); ok {
			t.Fatal("文件应已删除")
		}
	})

	t.Run("releases registered size when file gone", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		bucket := quota.NewPool(0).Scope("archive", 0)
		bucket.Adjust(0, 30)
		env.quota.bucketScope = bucket
		env.svc.recordArchiveUsage("alice", "gone.tar.gz", 30)

		if err := env.svc.DeleteCloudArchive("alice", "gone.tar.gz"); err != nil {
			t.Fatalf("删除失败：%v", err)
		}
		if bucket.Usage() != 0 {
			t.Fatalf("桶占用 = %d，期望按登记释放到 0", bucket.Usage())
		}
	})

	t.Run("invalid name and unavailable tenant", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		if err := env.svc.DeleteCloudArchive("alice", ".__bad"); err == nil {
			t.Fatal("非法归档名应报错")
		}
		env.svc.deps.Tenants = absentTenants{}
		if err := env.svc.DeleteCloudArchive("alice", "x.tar.gz"); err == nil {
			t.Fatal("租户不可用应报错")
		}
	})
}

// TestDeleteGroupArchive 组归档产物删除的两条形态：归档桶归档名 → DeleteCloudArchive；
// 外部卷转存 URL → 按卷可信 FS 删除 + owner Scope 释放 + 本地卷池释放（写侧同源）。
func TestDeleteGroupArchive(t *testing.T) {
	t.Parallel()

	t.Run("bucket name delegates", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		tenant := env.svc.deps.Tenants.TenantFor("alice")
		writeRootFile(t, tenant.Root(), "archive/g1.tar.gz", "abc")
		if err := env.svc.DeleteGroupArchive("alice", "g1.tar.gz"); err != nil {
			t.Fatalf("桶归档删除失败：%v", err)
		}
		if _, err := tenant.Root().Stat("archive/g1.tar.gz"); err == nil {
			t.Fatal("桶归档文件应已删除")
		}
	})

	t.Run("external url releases owner and volume quota", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		env.vols.local = true
		env.vols.footprint = map[string]int64{"vault\x00alice\x00archive/g1.tar.gz": 12}
		env.mem.files["alice/user/archive/g1.tar.gz"] = []byte("0123456789ab")
		scope := quota.NewPool(0).Scope("user", 0)
		scope.Adjust(0, 12)
		env.quota.scope = scope

		if err := env.svc.DeleteGroupArchive("alice", "vault://vault/alice/user/archive/g1.tar.gz"); err != nil {
			t.Fatalf("外部卷归档删除失败：%v", err)
		}
		if _, ok := env.mem.data("alice/user/archive/g1.tar.gz"); ok {
			t.Fatal("目标对象应已删除")
		}
		if scope.Usage() != 0 {
			t.Fatalf("owner 占用 = %d，期望释放到 0", scope.Usage())
		}
	})

	t.Run("error branches", func(t *testing.T) {
		t.Parallel()
		env := newTransferEnv(t)
		cases := []struct {
			name, archive string
			disableFS     bool
		}{
			{name: "url missing volume", archive: "vault:///alice/user/x.tar.gz"},
			{name: "url missing path", archive: "vault://vault"},
			{name: "volume not assembled", archive: "ghost://ghost/alice/user/x.tar.gz", disableFS: true},
		}
		for _, c := range cases {
			if c.disableFS {
				env.vols.fs = nil
			}
			if err := env.svc.DeleteGroupArchive("alice", c.archive); err == nil {
				t.Fatalf("%s：应返回错误", c.name)
			}
		}
		// 删除调用失败必须冒泡（不得吞掉）。
		env.vols.fs = env.mem
		env.mem.deleteErr = errors.New("delete failed")
		if err := env.svc.DeleteGroupArchive("alice", "vault://vault/alice/user/x.tar.gz"); err == nil {
			t.Fatal("删除失败应冒泡")
		}
	})
}

// TestArchiveEntries_OpenFailureClosesOpened archiveEntries 中途打开失败必须关闭已打开
// Reader（防句柄泄漏），且返回错误（调用方 500）。
func TestArchiveEntries_OpenFailureClosesOpened(t *testing.T) {
	t.Parallel()
	root := openTestRoot(t)
	writeRootFile(t, root, "a.bin", "aaaaa")
	_, err := archiveEntries([]ArchiveSource{
		{Root: root, Rel: "a.bin", TarRel: "a.bin"},
		{Root: root, Rel: "missing.bin", TarRel: "missing.bin"},
	})
	if err == nil {
		t.Fatal("源文件缺失应返回错误")
	}
}

// TestArchiveTotalSizeSumsVisibleBytes archiveTotalSize 汇总用户可见（逻辑）大小。
func TestArchiveTotalSizeSumsVisibleBytes(t *testing.T) {
	t.Parallel()
	root := openTestRoot(t)
	writeRootFile(t, root, "a.bin", "aaa")
	writeRootFile(t, root, "b.bin", "bbbbb")
	got := archiveTotalSize([]ArchiveSource{
		{Root: root, Rel: "a.bin"}, {Root: root, Rel: "b.bin"},
		{Root: root, Rel: "missing.bin"}, // 不可读的源不计入（不编造大小）
	})
	if got != 8 {
		t.Fatalf("archiveTotalSize = %d，want 8", got)
	}
}
