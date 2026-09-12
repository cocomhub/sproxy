// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// options_test.go 验证新的构造入口 `New(tenants, opts...)`：
//   - **最小能力**：只注入租户解析即可完成单卷 CRUD（列表/上传/下载/删除），
//     未装配的能力按既有 nil 语义降级（分块端点不 panic、无版本、不计量、不审计）；
//   - **Option 注入**：每个 With* 注入的能力都被真正使用（行为断言，而非仅字段赋值）；
//   - 唯一必需项缺失（nil 租户）→ 构造错误。

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// newMinimalService 用租户解析 + 可选 Option 构造服务，并装配测试 actor 解析。
func newMinimalService(t *testing.T, env *dirsEnv, opts ...Option) *Service {
	t.Helper()
	actor := actorResolverFunc(func(r *http.Request) string { return r.Header.Get("X-Test-Actor") })
	all := append([]Option{WithActor(actor)}, opts...)
	svc, err := New(tenantResolverFunc(env.tenantFor), all...)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return svc
}

// TestNew_MinimalDefaults_SingleVolumeCRUD 验证「零 Option 即最小可用」：单卷下
// 上传 → 列表 → 下载 → 删除全链路可用。
func TestNew_MinimalDefaults_SingleVolumeCRUD(t *testing.T) {
	env := newDirsEnv(t)
	env.svc = newMinimalService(t, env)

	const body = "minimal-body"
	cs := sha256Hex([]byte(body))

	// 上传（默认单卷路由 + 默认租户）。
	rr := env.upload(t, "alice", "dir/f.txt", []byte(body), cs, 0)
	if rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := mustReadUserFile(t, env, "alice", "user/dir/f.txt"); got != body {
		t.Fatalf("落盘内容=%q want %q", got, body)
	}

	// 列表。
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=dir")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeList(t, rr); resp.Total != 1 {
		t.Fatalf("列表应 1 条, got %d", resp.Total)
	}

	// 下载（默认下载路径解析仅支持普通文件）。
	w := httptest.NewRecorder()
	env.svc.Download(w, readReq("alice", "GET", "/download?filename=dir/f.txt"))
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("下载应 200 且内容一致, got %d %q", w.Code, w.Body.String())
	}

	// 删除（默认 checksum 门禁与内建锁池）。
	w = httptest.NewRecorder()
	env.svc.Delete(w, deleteReq("alice", "dir/f.txt", cs))
	if w.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); err == nil {
		t.Fatal("删除后文件应消失")
	}
}

// TestNew_MinimalDefaults_ChunkedDegradesWithoutPanic 验证未注入分块能力时，分块端点
// 不 panic 且按既有 nil-store 语义回包（500）。
func TestNew_MinimalDefaults_ChunkedDegradesWithoutPanic(t *testing.T) {
	env := newDirsEnv(t)
	env.svc = newMinimalService(t, env)

	w := httptest.NewRecorder()
	env.svc.UploadInit(w, postJSONReq(t, "alice", "/upload/init", ChunkedInitRequest{
		UploadID:     "sid-1",
		Filename:     "f.bin",
		TotalSize:    4,
		ChunkSize:    4,
		TotalChunks:  1,
		FileChecksum: sha256Hex([]byte("abcd")),
	}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("未装配分块应回 500（既有 nil-store 语义）, got %d: %s", w.Code, w.Body.String())
	}
}

// TestNew_NilTenants_Error 验证唯一必需项缺失（nil 租户解析）→ 构造错误。
func TestNew_NilTenants_Error(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil TenantResolver 应返回构造错误")
	}
}

// TestNew_Options_QuotaLedgerMetricsAudit 验证四个「副作用类」Option 真正生效：
// 配额记账、checksum 台账、计量、审计各在对应操作后被观察到。
func TestNew_Options_QuotaLedgerMetricsAudit(t *testing.T) {
	env := newDirsEnv(t)
	metrics := &fakeMetrics{}
	var audits []string
	env.svc = newMinimalService(t, env,
		WithQuota(quotaScopesFunc(env.quotaScopeFor)),
		WithChecksumLedger(checksumLedgersFunc(env.checksumStoreFor)),
		WithMetrics(metrics),
		WithAudit(auditorFunc(func(_ context.Context, action, object, result, _ string) {
			audits = append(audits, action+"|"+object+"|"+result)
		})),
	)

	const body = "opt-body"
	cs := sha256Hex([]byte(body))
	if rr := env.upload(t, "alice", "f.txt", []byte(body), cs, 0); rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if got := env.quotaScopeFor("alice", "user/f.txt").Usage(); got != int64(len(body)) {
		t.Fatalf("配额应记入 %d 字节, got %d", len(body), got)
	}
	if got, ok := env.checksumStoreFor("alice").Get("user/f.txt"); !ok || got != cs {
		t.Fatalf("checksum 台账应命中 %q, got %q ok=%v", cs, got, ok)
	}
	if metrics.uploadCalls != 1 || metrics.uploadBytes != int64(len(body)) {
		t.Fatalf("计量应记 1 次/%d 字节, got %+v", len(body), metrics)
	}

	// 删除应写审计（success）+ 计量 + 释放配额。
	w := httptest.NewRecorder()
	env.svc.Delete(w, deleteReq("alice", "f.txt", cs))
	if w.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(audits) != 1 || audits[0] != "delete|f.txt|success" {
		t.Fatalf("审计应为 delete|f.txt|success, got %v", audits)
	}
	if metrics.deleteCalls != 1 {
		t.Fatalf("删除计量应 1 次, got %d", metrics.deleteCalls)
	}
	if got := env.quotaScopeFor("alice", "user/f.txt").Usage(); got != 0 {
		t.Fatalf("删除应释放配额, got %d", got)
	}
}

// TestNew_WithVersioning_SavesVersionOnOverwrite 验证版本策略 Option 生效：
// 同名不同 checksum 触发版本化覆盖，旧内容保存为版本。
func TestNew_WithVersioning_SavesVersionOnOverwrite(t *testing.T) {
	env := newDirsEnv(t)
	env.svc = newMinimalService(t, env,
		WithVersioning(testVersioning{enabled: true}),
	)

	writeUserFile(t, env, "alice", "user/f.txt", "v1")
	newBody := []byte("v2")
	if rr := env.upload(t, "alice", "f.txt", newBody, sha256Hex(newBody), 0); rr.Code != http.StatusOK {
		t.Fatalf("版本化覆盖应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("应保存 1 个版本, got %d err=%v", len(entries), err)
	}
}

// TestNew_WithFileLocks_SharedLockSpace 验证注入的锁池被真正使用：预先占用即 409。
func TestNew_WithFileLocks_SharedLockSpace(t *testing.T) {
	env := newDirsEnv(t)
	locks := &mapFileLocks{}
	if _, ok := locks.TryMark("alice", "user/f.txt", uploadingLockUpload); !ok {
		t.Fatal("前置占用失败")
	}
	env.svc = newMinimalService(t, env, WithFileLocks(locks))

	body := []byte("x")
	if rr := env.upload(t, "alice", "f.txt", body, sha256Hex(body), 0); rr.Code != http.StatusConflict {
		t.Fatalf("注入锁池被占用应 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestNew_WiringOnlyOptions 验证「接线类」Option 被记录到运行时（配置项/日志/卷/分块）。
func TestNew_WiringOnlyOptions(t *testing.T) {
	env := newDirsEnv(t)
	logger := slog.New(slog.DiscardHandler)
	env.enableVolumes(t, "main")
	env.svc = newMinimalService(t, env,
		WithLogger(func() *slog.Logger { return logger }),
		WithChunkSize(func() int64 { return 8 }),
		WithVolumes(testVolumes{
			set:    env.volSet,
			tenant: env.volumeTenant,
			locate: func(string, string) (FileLocation, bool) { return FileLocation{}, false },
			route:  env.routeUploadDefault,
		}),
		WithChunkedUploads(testChunked{
			storeFor: func(string) *UploadStore { return nil },
			capacity: &fakeCapacity{},
		}),
	)

	if got := env.svc.rt.logger(); got != logger {
		t.Fatal("WithLogger 未生效")
	}
	if got := env.svc.rt.chunkSize(); got != 8 {
		t.Fatalf("WithChunkSize 未生效, got %d", got)
	}
	if env.svc.rt.volSet() == nil {
		t.Fatal("WithVolumes 未生效")
	}
	if env.svc.rt.storageManager() == nil {
		t.Fatal("WithChunkedUploads.Capacity 未生效")
	}
}

// TestNew_DefaultDownloadPaths_RejectsCloudKind 验证默认下载路径解析只支持普通文件：
// 云端 kind 需要装配层注入，未注入时 404（而不是误解析为普通路径）。
func TestNew_DefaultDownloadPaths_RejectsCloudKind(t *testing.T) {
	env := newDirsEnv(t)
	env.svc = newMinimalService(t, env)

	_, err := env.svc.rt.resolveDownloadPath(readReq("alice", "GET", "/download?filename=f.txt&kind=cloud_task"))
	var he *HTTPError
	if err == nil || !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("云端 kind 未注入应 404, got %v", err)
	}
}

// TestNew_MinimalDefaults_DefaultCapabilities 钉住「零 Option」时各能力的默认值：
// 分块大小回落 internal/size 默认；版本关闭；actor 恒匿名；单卷租户委托；
// 配额/台账/容量未装配时为 nil（各调用点按既有语义跳过）。
func TestNew_MinimalDefaults_DefaultCapabilities(t *testing.T) {
	env := newDirsEnv(t)
	svc, err := New(tenantResolverFunc(env.tenantFor)) // 不注入 actor，验证匿名默认
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := svc.rt.chunkSize(); got != size.DefaultChunkSize {
		t.Fatalf("默认分块大小=%d want %d", got, size.DefaultChunkSize)
	}
	if svc.rt.versioningEnabled() || svc.rt.versioningMaxVersions() != 0 {
		t.Fatal("默认版本策略应为关闭且不清理")
	}
	if got := svc.rt.actorOf(readReq("alice", "GET", "/api/files")); got != "" {
		t.Fatalf("默认 actor 应为空（anonymous）, got %q", got)
	}
	if tnt := svc.rt.volumeTenant("", "alice"); tnt == nil {
		t.Fatal("单卷默认 Tenant 应委托 TenantResolver")
	}
	if svc.rt.quotaScope("alice", "user/f.txt") != nil {
		t.Fatal("未注入配额应为 nil")
	}
	if svc.rt.checksumStore("alice") != nil {
		t.Fatal("未注入台账应为 nil")
	}
	if svc.rt.storageManager() != nil {
		t.Fatal("未注入容量能力应为 nil")
	}
	if svc.rt.uploadStore("alice") != nil {
		t.Fatal("未注入分块能力时 store 应为 nil")
	}
}

// TestNew_WithDownloadPaths_OverridesDefault 验证下载路径解析 Option 覆盖默认实现。
func TestNew_WithDownloadPaths_OverridesDefault(t *testing.T) {
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	env.svc = newMinimalService(t, env, WithDownloadPaths(downloadPathsFunc(
		func(*http.Request) (DownloadPath, error) {
			return DownloadPath{Filename: "custom.txt", Tenant: tnt, Rel: "user/custom.txt"}, nil
		},
	)))

	dp, err := env.svc.rt.resolveDownloadPath(readReq("alice", "GET", "/download?filename=ignored&kind=cloud_task"))
	if err != nil || dp.Filename != "custom.txt" {
		t.Fatalf("注入的解析器应生效, got %+v err=%v", dp, err)
	}
}

// ---- 测试用能力实现（Option 注入） ----

// testVersioning 是 Versioning 能力的测试实现。
type testVersioning struct {
	enabled bool
	max     int
}

func (v testVersioning) Enabled() bool    { return v.enabled }
func (v testVersioning) MaxVersions() int { return v.max }

// testVolumes 是 VolumeRouter 能力的测试实现（委托给各函数）。
type testVolumes struct {
	set    VolumeSet
	tenant func(volName, owner string) *storage.Tenant
	locate func(owner, rel string) (FileLocation, bool)
	route  func(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error)
}

func (v testVolumes) Volumes() VolumeSet { return v.set }
func (v testVolumes) Tenant(volName, owner string) *storage.Tenant {
	return v.tenant(volName, owner)
}
func (v testVolumes) Locate(owner, rel string) (FileLocation, bool) { return v.locate(owner, rel) }
func (v testVolumes) Route(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
	return v.route(owner, rel, explicitVol, size, forceHomeVol)
}

// testChunked 是 ChunkedUploads 能力的测试实现。
type testChunked struct {
	storeFor func(owner string) *UploadStore
	capacity StorageManager
}

func (c testChunked) UploadStoreFor(owner string) *UploadStore { return c.storeFor(owner) }
func (c testChunked) Capacity() StorageManager                 { return c.capacity }
