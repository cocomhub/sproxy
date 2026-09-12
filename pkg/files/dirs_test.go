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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// rebuild 按当前环境状态重建 Service（Service 无状态，重建即可切换 VolSet 等装配件）。
func (e *dirsEnv) rebuild() {
	deps := Deps{
		Logger:            func() *slog.Logger { return e.logger },
		ActorFromRequest:  func(r *http.Request) string { return r.Header.Get("X-Test-Actor") },
		TenantFor:         e.tenantFor,
		PrimaryViewTenant: e.primaryViewTenant,
		VolumeTenant:      e.volumeTenant,
		QuotaScopeFor:     e.quotaScopeFor,
		ChecksumStoreFor:  e.checksumStoreFor,
	}
	// 与生产装配同规矩：只在非 nil 时赋值，避免 nil *registry.Set 装入接口成为非 nil 接口
	// （否则单卷场景会被误判为多卷，见 Deps.VolSet 注释）。
	if e.volSet != nil {
		deps.VolSet = e.volSet
	}
	e.svc = NewService(deps)
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

// primaryViewTenant 复刻 pkg/server 语义：视图内首个可用卷的租户；单卷回落 tenantFor。
func (e *dirsEnv) primaryViewTenant(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if e.volSet == nil {
		return e.tenantFor(owner)
	}
	for _, v := range volume.AllowedVolumes(e.volSet.All(), owner) {
		if tnt := e.volumeTenant(v.Name, owner); tnt != nil && tnt.Root() != nil {
			return tnt
		}
	}
	return nil
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

// quotaScopeFor 复刻 pkg/server 语义到功能桶粒度（目录族只按 rel 首段解析）：
// 空池/非功能桶首段 → nil。
func (e *dirsEnv) quotaScopeFor(owner, rel string) *quota.Scope {
	owner = normalizeOwner(owner)
	if e.pool == nil {
		return nil
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if len(segs) == 0 || segs[0] == "" {
		return nil
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

// TestService_LoggerIsLiveAccessor 验证接缝的「取用函数而非快照」约定：Service 构造后
// 装配层替换日志器，后续日志应写到**新**日志器（与 pkg/server 热更新 h.logger 同语义）。
func TestService_LoggerIsLiveAccessor(t *testing.T) {
	env := newDirsEnv(t)
	var first, second bytes.Buffer
	cur := slog.New(slog.NewTextHandler(&first, nil))
	env.svc = NewService(Deps{
		Logger:            func() *slog.Logger { return cur },
		ActorFromRequest:  func(r *http.Request) string { return r.Header.Get("X-Test-Actor") },
		TenantFor:         env.tenantFor,
		PrimaryViewTenant: env.primaryViewTenant,
	})

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
