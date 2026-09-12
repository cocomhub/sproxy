// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dirs_test.go 是目录族（Mkdir/Rmdir）的**域级测试**：用真实下层能力
// （pkg/storage 的租户根、pkg/checksum 台账、pkg/quota 池）+ 注入的最小装配件
// （假 actor / 租户解析 / 配额 Scope 解析）直接构造 files.Service。
//
// 与 pkg/server 的 dirs_owner_test.go 的分工：那边经**生产装配**（Handlers 薄适配 →
// pkg/files）跑跨族集成（mkdir/rmdir/list 三面互验）；这边覆盖域本身的边界与副作用
// （配额释放、checksum 清理、多卷逐卷删除），不依赖 pkg/server——领域包反向依赖装配层
// 会让门禁规则③判红，故此处的装配件是手写替身，而非 pkg/server 的实现。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// testLogger 返回丢弃输出的 slog.Logger（域级测试不校验日志内容时使用）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// dirsEnv 是目录族域级测试环境：真实存储根 + 真实配额池/checksum 台账 + 替身装配件。
type dirsEnv struct {
	// loggerFn 是可选的日志器访问器覆盖（默认 nil → 返回 e.logger）；供
	// TestService_LoggerIsLiveAccessor 验证「形状 1：取用函数」语义。
	loggerFn func() *slog.Logger
	// resolveDownloadPath 是可选的下载路径解析覆盖（默认 nil → 恒解析为不可用路径）；
	// 只读面（download/stat）的域级用例用它注入真实 (租户, rel)，避免依赖装配层的
	// resolveDownloadPath。设置后需调用 rebuild() 生效。
	resolveDownloadPath func(*http.Request) (DownloadPath, error)

	root     string // 默认卷根（<root>/<owner>/user/...）
	logger   *slog.Logger
	pool     *quota.Pool
	tenants  map[string]*storage.Tenant
	checksum map[string]*checksum.ChecksumStore
	buckets  map[string]*quota.Scope // key = owner + "/" + 功能桶名
	volSet   *registry.Set           // nil = 单卷（旧装配语义）
	volDirs  map[string]string       // 卷名 → 卷根绝对路径（断言磁盘副作用用）
	pools    map[string]*quota.Pool
	svc      *Service
}

// newDirsEnv 构造单卷（VolSet == nil）域级测试环境。
func newDirsEnv(t *testing.T) *dirsEnv {
	t.Helper()
	root := t.TempDir()
	e := &dirsEnv{
		root:     root,
		logger:   testLogger(),
		pool:     quota.NewPool(0),
		tenants:  map[string]*storage.Tenant{},
		checksum: map[string]*checksum.ChecksumStore{},
		buckets:  map[string]*quota.Scope{},
	}
	e.rebuild()
	t.Cleanup(func() {
		for _, tnt := range e.tenants {
			if tnt != nil && tnt.Root() != nil {
				_ = tnt.Root().Close()
			}
		}
		if e.volSet != nil {
			_ = e.volSet.Close()
		}
	})
	return e
}

// rebuild 按当前环境状态重建 Service（重建即可切换 VolSet 等装配件）。
//
// **替身与生产装配的等价范围（如实声明）**：本替身复刻生产的四件事——① 默认卷租户懒建
// （含 meta 桶预建，由 TestDirsEnv_TenantForParityWithProduction 钉住）；② 卷集合租户懒建
// 与默认卷委托；③ per-owner checksum 台账懒建并缓存；④ 配额 Scope 的**功能桶白名单闸门**
// （首段非 user/cloud/archive/chunk/version/meta → nil，由
// TestService_QuotaScopeFor_NonBucketSegmentIgnored 钉住）。
//
// **不复刻的（有意，逐条列明以免误导后续族）**：
//   - `bucket_limits` 子目录分层 Scope——本族只按 rel 首段取桶根，子目录配额语义不受影响；
//   - 租户/台账的**锁**（生产 tenantMu / ChecksumStore 互斥）——测试单线程；
//   - 生产 `tenantFor` 的 `globalRoot == nil`（未装配存储根）fail-closed 分支及一路
//     `h.logger.Warn("非法租户名/路径越界/创建租户根目录失败…")` 告警——替身恒有 `e.root`，
//     永不进入该分支；本族用例不依赖它（该分支由 pkg/server 侧覆盖）。
//
// deps 返回当前装配状态下的完整接缝（rebuild 与需要自定义日志器的用例共用）。
func (e *dirsEnv) deps() Deps {
	loggerFn := e.loggerFn
	if loggerFn == nil {
		loggerFn = func() *slog.Logger { return e.logger }
	}
	resolveDownloadPath := e.resolveDownloadPath
	deps := Deps{
		Logger:           loggerFn,
		ActorFromRequest: func(r *http.Request) string { return r.Header.Get("X-Test-Actor") },
		TenantFor:        e.tenantFor,
		VolumeTenant:     e.volumeTenant,
		QuotaScopeFor:    e.quotaScopeFor,
		ChecksumStoreFor: e.checksumStoreFor,
	}
	// 分块族接缝：本文件的用例不触达这些路径，但 NewService 做**全量校验**（缺项 panic），
	// 故按最小可用实现填充（ChunkSize/VersioningEnabled 为配置读取；其余为不触达的桩）。
	deps.ChunkSize = func() int64 { return size.DefaultChunkSize }
	deps.VersioningEnabled = func() bool { return false }
	deps.VersioningMaxVersions = func() int { return 0 }
	deps.UploadStoreFor = func(string) *UploadStore { return nil }
	deps.Uploading = &sync.Map{}
	deps.ResolveDownloadPath = resolveDownloadPath
	if deps.ResolveDownloadPath == nil {
		deps.ResolveDownloadPath = func(*http.Request) (DownloadPath, error) { return DownloadPath{}, nil }
	}
	deps.LocateOwnerFile = func(string, string) (FileLocation, bool) { return FileLocation{}, false }
	deps.RouteUpload = func(string, string, string, int64, string) (UploadRoute, error) {
		return UploadRoute{}, nil
	}
	deps.AcquireFileLock = func(string, string) (func(), bool) { return func() {}, true }
	deps.RecordFileAudit = func(context.Context, string, string, string, string) {}
	// 与生产装配同规矩：只在非 nil 时赋值，避免 nil *registry.Set 装入接口成为非 nil 接口
	// （否则单卷场景会被误判为多卷，见 Deps.VolSet 注释）。
	if e.volSet != nil {
		deps.VolSet = e.volSet
	}
	return deps
}

// rebuild 按当前装配状态重建 Service（enableVolumes 与多卷用例调用）。
func (e *dirsEnv) rebuild() {
	e.svc = NewService(e.deps())
}

// enableVolumes 装配多卷（首卷为默认卷，物理根 = e.root），并重建 Service。
func (e *dirsEnv) enableVolumes(t *testing.T, names ...string) {
	t.Helper()
	vols := make([]volume.Volume, 0, len(names))
	roots := map[string]*storage.Root{}
	e.volDirs = map[string]string{}
	e.pools = map[string]*quota.Pool{}
	for i, name := range names {
		dir := e.root
		if i > 0 {
			dir = t.TempDir()
		}
		rt, err := storage.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot(%s): %v", name, err)
		}
		roots[name] = rt
		e.volDirs[name] = dir
		e.pools[name] = quota.NewPool(0)
		vols = append(vols, volume.Volume{Name: name, RootDir: dir})
	}
	e.volSet = registry.NewSet(vols, roots, e.pools, names[0], map[string]*storage.Tenant{})
	e.rebuild()
}

// tenantFor 复刻 pkg/server 默认卷租户语义：空 owner → anonymous、懒创建、非法名 fail-closed。
func (e *dirsEnv) tenantFor(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if tnt, ok := e.tenants[owner]; ok {
		return tnt
	}
	if !storage.ValidSegmentName(owner) {
		return nil
	}
	dir := filepath.Join(e.root, owner)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		return nil
	}
	tnt, err := storage.NewTenant(owner, rt)
	if err != nil {
		_ = rt.Close()
		return nil
	}
	// 与生产 tenantFor 一致：预建 meta 桶（供 per-tenant checksum / meta 记录写入）。
	if err := rt.MkdirAll("meta", 0o755); err != nil {
		_ = rt.Close()
		return nil
	}
	e.tenants[owner] = tnt
	return tnt
}

// volumeTenant 复刻 pkg/server 语义：默认卷委托 tenantFor，其余卷走卷集合懒建。
func (e *dirsEnv) volumeTenant(volName, owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if e.volSet == nil || volName == "" || volName == e.volSet.Default().Name {
		return e.tenantFor(owner)
	}
	return e.volSet.Tenant(volName, owner, e.logger)
}

// checksumStoreFor 复刻 pkg/server 语义：台账落在租户 meta 桶，按 owner 懒建并缓存。
func (e *dirsEnv) checksumStoreFor(owner string) *checksum.ChecksumStore {
	owner = normalizeOwner(owner)
	if cs, ok := e.checksum[owner]; ok {
		return cs
	}
	tnt := e.tenantFor(owner)
	if tnt == nil {
		return nil
	}
	metaAbs, ok := tnt.Root().Abs("meta")
	if !ok {
		return nil
	}
	if err := os.MkdirAll(metaAbs, 0o755); err != nil {
		return nil
	}
	cs := checksum.NewChecksumStore(filepath.Join(metaAbs, "checksums.json"), e.logger)
	e.checksum[owner] = cs
	return cs
}

// quotaBucketNames 复刻 pkg/server 的功能桶白名单（handlers.go 同名变量）：
// 只有这些首段才有配额子 Scope，其余首段一律 nil。
var quotaBucketNames = []string{"user", "cloud", "archive", "chunk", "version", "meta"}

// quotaScopeFor 复刻 pkg/server 语义到功能桶粒度（目录族只按 rel 首段解析）：
// 空池 → nil；**首段不在功能桶白名单 → nil**（与生产 quotaScopeFor 一致：
// 生产的 quotaBuckets 只装 quotaBucketNames 与 bucket_limits 键）。
func (e *dirsEnv) quotaScopeFor(owner, rel string) *quota.Scope {
	owner = normalizeOwner(owner)
	if e.pool == nil {
		return nil
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if len(segs) == 0 || segs[0] == "" {
		return nil
	}
	if !slices.Contains(quotaBucketNames, segs[0]) {
		return nil // 非功能桶首段 → 无子 Scope
	}
	key := owner + "/" + segs[0]
	if sc, ok := e.buckets[key]; ok {
		return sc
	}
	sc := e.pool.Scope("/tenant/"+owner, 0).Mount(segs[0], 0)
	e.buckets[key] = sc
	return sc
}

// post 以指定 actor 发起 POST 请求（actor 空 = 未认证 → anonymous）。
func (e *dirsEnv) post(actor, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, nil)
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mkdir", e.svc.Mkdir)
	mux.HandleFunc("POST /rmdir", e.svc.Rmdir)
	mux.ServeHTTP(rr, req)
	return rr
}

// decodeResp 解析 UploadResponse 外壳。
func decodeResp(t *testing.T, rr *httptest.ResponseRecorder) UploadResponse {
	t.Helper()
	var out UploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不是合法 JSON（%v）: %s", err, rr.Body.String())
	}
	return out
}

// TestService_Mkdir_CreatesDirInUserBucket 验证 Mkdir 成功路径落盘位置与响应体：
// 目录建在 <root>/<owner>/user/<rel>，200 + {"success":true,"message":"目录已创建: cloud"}，
// 且 Content-Type 为 application/json（与 pkg/server 迁移前一致）。
func TestService_Mkdir_CreatesDirInUserBucket(t *testing.T) {
	env := newDirsEnv(t)

	rr := env.post("alice", "/mkdir?dirname=cloud")
	if rr.Code != http.StatusOK {
		t.Fatalf("mkdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", ct)
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Message != "目录已创建: cloud" {
		t.Fatalf("响应=%+v want {true, 目录已创建: cloud}", resp)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "cloud")); err != nil {
		t.Fatalf("应落盘 alice/user/cloud: %v", err)
	}
	// 子目录路径同样保留层级。
	if rr := env.post("alice", "/mkdir?dirname=a/b/c"); rr.Code != http.StatusOK {
		t.Fatalf("mkdir a/b/c 应 200, got %d", rr.Code)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "a", "b", "c")); err != nil {
		t.Fatalf("应落盘 alice/user/a/b/c: %v", err)
	}
}

// TestService_Mkdir_RejectsBadInput 验证 Mkdir 的三条 400 分支：空 dirname、
// 非法路径（含 ..）、非法 owner（租户 fail-closed → 无效的目录路径）。
func TestService_Mkdir_RejectsBadInput(t *testing.T) {
	env := newDirsEnv(t)

	cases := []struct {
		name    string
		actor   string
		target  string
		wantMsg string
	}{
		{"空 dirname", "alice", "/mkdir", "dirname 不能为空"},
		{"路径穿越", "alice", "/mkdir?dirname=../evil", "无效的目录名: "},
		{"非法 owner", "..", "/mkdir?dirname=cloud", "无效的目录路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.post(tc.actor, tc.target)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
			}
			resp := decodeResp(t, rr)
			if resp.Success || !strings.HasPrefix(resp.Message, tc.wantMsg) {
				t.Fatalf("响应=%+v want message 前缀 %q", resp, tc.wantMsg)
			}
		})
	}
}

// TestService_Mkdir_AnonymousWhenNoActor 验证未认证请求（actor 为空）经注入的
// ActorFromRequest + 领域包内的 normalizeOwner 落到 anonymous 租户。
func TestService_Mkdir_AnonymousWhenNoActor(t *testing.T) {
	env := newDirsEnv(t)

	if rr := env.post("", "/mkdir?dirname=pub"); rr.Code != http.StatusOK {
		t.Fatalf("匿名 mkdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.root, "anonymous", "user", "pub")); err != nil {
		t.Fatalf("匿名目录应落盘 anonymous/user/pub: %v", err)
	}
}

// TestService_Rmdir_ForceRequired 验证未带 force 拒绝删除（400）且目录仍在。
func TestService_Rmdir_ForceRequired(t *testing.T) {
	env := newDirsEnv(t)
	target := filepath.Join(env.root, "alice", "user", "sub")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	rr := env.post("alice", "/rmdir?dirname=sub")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("无 force 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "请使用 ?force=true 确认删除" {
		t.Fatalf("响应 message=%q", resp.Message)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("未确认时目录不应被删除: %v", err)
	}
}

// TestService_Rmdir_ErrorMapping 验证 404 / 400 的错误映射：目录不存在 → 404；
// 普通文件 → 400「指定路径不是目录」；符号链接 → 400「不允许删除符号链接」（建链接
// 需要权限，建不出则跳过该子场景）。
func TestService_Rmdir_ErrorMapping(t *testing.T) {
	env := newDirsEnv(t)
	userDir := filepath.Join(env.root, "alice", "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("目录不存在", func(t *testing.T) {
		rr := env.post("alice", "/rmdir?dirname=ghost&force=true")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("应 404, got %d: %s", rr.Code, rr.Body.String())
		}
		if resp := decodeResp(t, rr); resp.Message != "目录不存在" {
			t.Fatalf("响应 message=%q", resp.Message)
		}
	})

	t.Run("普通文件不是目录", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(userDir, "plain.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		rr := env.post("alice", "/rmdir?dirname=plain.txt&force=true")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
		}
		if resp := decodeResp(t, rr); resp.Message != "指定路径不是目录" {
			t.Fatalf("响应 message=%q", resp.Message)
		}
	})

	t.Run("符号链接", func(t *testing.T) {
		link := filepath.Join(userDir, "link")
		if err := os.Symlink(userDir, link); err != nil {
			t.Skipf("本环境无法创建符号链接（%v），跳过", err)
		}
		rr := env.post("alice", "/rmdir?dirname=link&force=true")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
		}
		if resp := decodeResp(t, rr); resp.Message != "不允许删除符号链接" {
			t.Fatalf("响应 message=%q", resp.Message)
		}
	})
}

// TestService_Rmdir_ReleasesQuotaAndCleansChecksum 验证删除的副作用：
//   - 各文件按自身 rel 分键释放配额子 Scope；
//   - per-tenant checksum 台账清理 rel 前缀与 rel 自身，目录外记录不受影响。
func TestService_Rmdir_ReleasesQuotaAndCleansChecksum(t *testing.T) {
	env := newDirsEnv(t)
	dir := filepath.Join(env.root, "alice", "user", "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("a", 30)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	// 配额：先记账 30 字节（预留 + Commit）。
	scope := env.quotaScopeFor("alice", "user/subdir/a.txt")
	if scope == nil {
		t.Fatal("quotaScopeFor 应返回 user 桶 Scope")
	}
	res, err := scope.TryReserve(int64(len(payload)))
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	res.Commit(int64(len(payload)))
	if got := scope.Usage(); got != int64(len(payload)) {
		t.Fatalf("记账后 Usage=%d want %d", got, len(payload))
	}

	cs := env.checksumStoreFor("alice")
	if cs == nil {
		t.Fatal("checksumStoreFor 应返回台账")
	}
	cs.Set("user/subdir/a.txt", "sha256hex")
	cs.Set("user/subdir", "dirchecksum")
	cs.Set("user/other.txt", "other")

	rr := env.post("alice", "/rmdir?dirname=subdir&force=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("rmdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); !resp.Success || resp.Message != "目录已删除: subdir" {
		t.Fatalf("响应=%+v", resp)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("目录应已删除")
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("删除后 Usage=%d want 0（按文件 rel 分键释放）", got)
	}
	if _, ok := cs.Get("user/subdir/a.txt"); ok {
		t.Fatal("子文件 checksum 应被 DeletePrefix(rel + \"/\") 清理")
	}
	if _, ok := cs.Get("user/subdir"); ok {
		t.Fatal("目录自身 checksum 应被 Delete(rel) 清理")
	}
	if _, ok := cs.Get("user/other.txt"); !ok {
		t.Fatal("目录外 checksum 不应被误删")
	}
}

// TestService_Rmdir_MultiVolumeDeletesEachAndReleasesPool 验证多卷路径：同一相对路径
// 在两个卷上并存时逐卷删除，且各卷容量池按本卷被删字节释放。
func TestService_Rmdir_MultiVolumeDeletesEachAndReleasesPool(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	sizes := map[string]int{"main": 20, "disk2": 35}
	for name, volDir := range env.volDirs {
		dir := filepath.Join(volDir, "alice", "user", "subdir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(strings.Repeat("b", sizes[name])), 0o644); err != nil {
			t.Fatal(err)
		}
		pool := env.pools[name]
		res, err := pool.TryReserve(int64(sizes[name]))
		if err != nil {
			t.Fatalf("%s TryReserve: %v", name, err)
		}
		res.Commit(int64(sizes[name]))
	}

	rr := env.post("alice", "/rmdir?dirname=subdir&force=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("多卷 rmdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	for name, volDir := range env.volDirs {
		if _, err := os.Stat(filepath.Join(volDir, "alice", "user", "subdir")); !os.IsNotExist(err) {
			t.Fatalf("卷 %s 上的 subdir 应已删除", name)
		}
		if got := env.pools[name].Usage(); got != 0 {
			t.Fatalf("卷 %s 池 Usage=%d want 0（按本卷被删字节释放）", name, got)
		}
	}
}

// TestService_Rmdir_MissingOnAllVolumesReturns404 验证多卷下「目录不存在于任何卷」→ 404。
func TestService_Rmdir_MissingOnAllVolumesReturns404(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	rr := env.post("alice", "/rmdir?dirname=ghost&force=true")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("应 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestService_LoggerIsLiveAccessor 验证接缝的「取用函数而非快照」约定（形状 1）：Service
// 构造后装配层替换日志器，后续日志应写到**新**日志器（与 pkg/server 热更新 h.logger 同语义）。
func TestService_LoggerIsLiveAccessor(t *testing.T) {
	env := newDirsEnv(t)
	var first, second bytes.Buffer
	cur := slog.New(slog.NewTextHandler(&first, nil))
	env.loggerFn = func() *slog.Logger { return cur }
	env.svc = NewService(env.deps())

	// 替换「当前生效」的日志器后再请求。
	cur = slog.New(slog.NewTextHandler(&second, nil))
	if rr := env.post("alice", "/mkdir?dirname=swap"); rr.Code != http.StatusOK {
		t.Fatalf("mkdir 应 200, got %d", rr.Code)
	}
	if first.Len() != 0 {
		t.Fatalf("旧日志器不应再收到日志: %s", first.String())
	}
	if !strings.Contains(second.String(), "目录已创建") {
		t.Fatalf("新日志器应收到日志: %q", second.String())
	}
}

// TestNewService_RejectsIncompleteDeps 验证构造期全量校验：缺任一必填项即 panic
// （缺项只会在对应族的请求路径上炸，故 fail-fast），且 panic 信息点名缺失字段。
// 四类合法例外：Logger 缺省（回落 slog.Default）、VolSet/StorageManager/Metrics 的 nil
// （未装配该能力 = 跳过对应路径）。
func TestNewService_RejectsIncompleteDeps(t *testing.T) {
	env := newDirsEnv(t)
	full := func() Deps {
		d := Deps{
			Logger:           func() *slog.Logger { return env.logger },
			ActorFromRequest: func(*http.Request) string { return "" },
			TenantFor:        env.tenantFor,
			VolumeTenant:     env.volumeTenant,
			QuotaScopeFor:    env.quotaScopeFor,
			ChecksumStoreFor: env.checksumStoreFor,
			ChunkSize:        func() int64 { return size.DefaultChunkSize },
			VersioningEnabled: func() bool {
				return false
			},
			VersioningMaxVersions: func() int { return 0 },
			UploadStoreFor:        func(string) *UploadStore { return nil },
			Uploading:             &sync.Map{},
			ResolveDownloadPath:   func(*http.Request) (DownloadPath, error) { return DownloadPath{}, nil },
			LocateOwnerFile:       func(string, string) (FileLocation, bool) { return FileLocation{}, false },
			RouteUpload:           func(string, string, string, int64, string) (UploadRoute, error) { return UploadRoute{}, nil },
			AcquireFileLock:       func(string, string) (func(), bool) { return func() {}, true },
			RecordFileAudit:       func(context.Context, string, string, string, string) {},
		}
		return d
	}

	// 例外（四个，不参与校验）：Logger 缺省、VolSet/StorageManager/Metrics 的 nil 是语义。
	d := full()
	d.Logger = nil
	d.VolSet = nil
	d.StorageManager = nil
	d.Metrics = nil
	if svc := NewService(d); svc == nil {
		t.Fatal("四个例外项缺失应可构造")
	}

	// 其余每一项缺失都必须 panic，且点名该字段。
	for _, name := range []string{
		"ActorFromRequest", "TenantFor", "VolumeTenant", "QuotaScopeFor", "ChecksumStoreFor",
		"ChunkSize", "VersioningEnabled", "VersioningMaxVersions", "UploadStoreFor", "Uploading",
		"ResolveDownloadPath", "LocateOwnerFile", "RouteUpload", "AcquireFileLock", "RecordFileAudit",
	} {
		t.Run(name, func(t *testing.T) {
			d := full()
			d.Logger = nil // Logger 是缺省项，一并置 nil 以证明它不参与校验
			switch name {
			case "ActorFromRequest":
				d.ActorFromRequest = nil
			case "TenantFor":
				d.TenantFor = nil
			case "VolumeTenant":
				d.VolumeTenant = nil
			case "QuotaScopeFor":
				d.QuotaScopeFor = nil
			case "ChecksumStoreFor":
				d.ChecksumStoreFor = nil
			case "ChunkSize":
				d.ChunkSize = nil
			case "VersioningEnabled":
				d.VersioningEnabled = nil
			case "VersioningMaxVersions":
				d.VersioningMaxVersions = nil
			case "UploadStoreFor":
				d.UploadStoreFor = nil
			case "Uploading":
				d.Uploading = nil
			case "ResolveDownloadPath":
				d.ResolveDownloadPath = nil
			case "LocateOwnerFile":
				d.LocateOwnerFile = nil
			case "RouteUpload":
				d.RouteUpload = nil
			case "AcquireFileLock":
				d.AcquireFileLock = nil
			case "RecordFileAudit":
				d.RecordFileAudit = nil
			}
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("缺 %s 应 panic", name)
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, name) {
					t.Fatalf("panic 信息应点名 %s, got %v", name, r)
				}
			}()
			NewService(d)
		})
	}
}

// TestOwnerNormalization_Contract 钉住匿名租户名契约：`normalizeOwner("")` = "anonymous"、
// 非空原样返回。pkg/server 侧同名实现由 `pkg/server/response_drift_test.go` 经领域 handler
// 的落盘路径反查本包实现，两侧任一侧改动都会变红。
func TestOwnerNormalization_Contract(t *testing.T) {
	if anonymousOwner != "anonymous" {
		t.Fatalf("匿名租户名契约变更: %q（存储布局 <root>/<owner>/… 的 owner 段）", anonymousOwner)
	}
	if got := normalizeOwner(""); got != "anonymous" {
		t.Fatalf("normalizeOwner(\"\")=%q want \"anonymous\"", got)
	}
	if got := normalizeOwner("alice"); got != "alice" {
		t.Fatalf("normalizeOwner(\"alice\")=%q want \"alice\"", got)
	}
}

// TestDirsEnv_TenantForParityWithProduction 钉住替身 `tenantFor` 与生产的语义对齐点之一：
// 生产在创建租户后**预建 meta 桶**（pkg/server/handlers.go 的 tenantFor 末段，供 per-tenant
// checksum / meta 记录写入），替身必须同样预建。
//
// 为什么需要专门钉：替身的 `checksumStoreFor` 自己也会 `MkdirAll(meta)`，会**掩盖**预建缺失
// （移除预建后其余用例仍全绿）——即该行为无其它用例承重，只有本用例能拦住对齐失效。
func TestDirsEnv_TenantForParityWithProduction(t *testing.T) {
	env := newDirsEnv(t)

	tnt := env.tenantFor("alice")
	if tnt == nil {
		t.Fatal("tenantFor(alice) 应创建租户")
	}
	metaPath := filepath.Join(env.root, "alice", "meta")
	fi, err := os.Stat(metaPath)
	if err != nil {
		t.Fatalf("替身应与生产一致预建 meta 桶（%s）: %v", metaPath, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s 应为目录（生产用 MkdirAll(\"meta\", 0o755)）", metaPath)
	}
	// 匿名租户同样预建（生产：空 owner → anonymous 走同一路径）。
	if env.tenantFor("") == nil {
		t.Fatal("tenantFor(\"\") 应创建 anonymous 租户")
	}
	if _, err := os.Stat(filepath.Join(env.root, "anonymous", "meta")); err != nil {
		t.Fatalf("anonymous 租户也应预建 meta: %v", err)
	}
}

// TestService_QuotaScopeFor_NonBucketSegmentIgnored 钉住接缝 QuotaScopeFor 的语义边界：
// rel 首段不在功能桶白名单时返回 nil（与生产 quotaScopeFor 一致）。这是替身与生产对齐后
// 新增的守卫——此前替身会无条件 Mount 任意首段，与它自己的注释相悖。
func TestService_QuotaScopeFor_NonBucketSegmentIgnored(t *testing.T) {
	env := newDirsEnv(t)
	if sc := env.quotaScopeFor("alice", "notabucket/x.txt"); sc != nil {
		t.Fatalf("非功能桶首段应返回 nil, got %v", sc)
	}
	if sc := env.quotaScopeFor("alice", "user/x.txt"); sc == nil {
		t.Fatal("功能桶首段 user 应返回非 nil")
	}
}
