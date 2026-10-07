// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_api_test.go 验证用户卷管理 API（U3）：
//  1. POST /api/volumes/user 创建（type 已注册 + extra 合法 → store 落盘 + Set.External 可查）。
//  2. POST 未注册 type → 400（fail-fast 试构造）。
//  3. GET /api/volumes/user 只列 owner 自己的卷（per-owner 隔离）。
//  4. DELETE 同步任务引用中 → 409（运行中引用拒绝）。
//  5. DELETE 跨 owner → 404（防枚举）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// userVolumeTestType 是用户卷 API 测试用的 fake backend 类型（全局注册一次）。
const userVolumeTestType = "user-vol-test"

// fakeUserVolBackend 是测试用 ExternalBackend（FS 恒空；Close 计数）。
type fakeUserVolBackend struct {
	closed bool
	usage  int64
}

func (f *fakeUserVolBackend) FS() syncpkg.FS { return nil }
func (f *fakeUserVolBackend) Close() error   { f.closed = true; return nil }

// Usage/Capacity 实现 registry.UsageProvider（C3 卷级计数查询——测试固定值）。
func (f *fakeUserVolBackend) Usage() int64    { return f.usage }
func (f *fakeUserVolBackend) Capacity() int64 { return 0 }

// registerUserVolTestBackend 注册 fake backend（重复注册 panic；sync.Once 保证唯一）。
func registerUserVolTestBackend() {
	registerUserVolTestBackendOnce.Do(func() {
		registry.RegisterBackend(userVolumeTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &fakeUserVolBackend{usage: 42}, nil
		})
	})
}

var registerUserVolTestBackendOnce sync.Once

// newUserVolumeAPIHandlers 装配用户卷 API 测试 Handlers（volSet + store + syncMgr 可注入）。
func newUserVolumeAPIHandlers(t *testing.T, withSync bool) (*Handlers, *UserVolumeStore) {
	t.Helper()
	registerUserVolTestBackend()
	registerUserVolWrapBackend()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	if withSync {
		mgr := newUserVolTestManager(t, cfg.StorageRoot)
		h.SetSyncMgr(mgr)
	}
	return h, store
}

// newUserVolTestManager 构造带 baidupcs remote（volume=用户卷名）的 syncmgr。
func newUserVolTestManager(t *testing.T, storageRoot string) *syncmgr.Manager {
	t.Helper()
	// remote 名 = 用户卷名（引用检查按此匹配）。
	remotes := []syncmgr.RemoteConfig{
		{Name: "userdisk1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "userdisk1"},
	}
	tenantRoot := func(owner string) (string, string, bool) { return storageRoot, owner, true }
	mgr := syncmgr.NewManager(syncmgr.ManagerOptions{TenantRoot: tenantRoot, ListTenants: nil, Quota: nil, QuotaCat: 0, Remotes: remotes, Executor: nil, Logger: nil, Config: &syncmgr.Config{MaxConcurrent: 3, TaskTTL: 0}})
	t.Cleanup(mgr.Stop)
	return mgr
}

// userVolWrapTestType 是「封装类」测试后端（Schema 声明必填 volume-select 底层卷字段）。
// 与 base 的 userVolumeTestType 区分：后者无 Schema（不触发封装校验），零回归既有用例。
const userVolWrapTestType = "user-vol-wrap"

// wrapperSchemaBackend 实现 SchemaProvider：声明单字段 `target`（volume-select，必填，
// AllowWrapper=true）——封装卷语义，供建卷校验消费。
type wrapperSchemaBackend struct{}

func (w *wrapperSchemaBackend) FS() syncpkg.FS  { return nil }
func (w *wrapperSchemaBackend) Close() error    { return nil }
func (w *wrapperSchemaBackend) Usage() int64    { return 0 }
func (w *wrapperSchemaBackend) Capacity() int64 { return 0 }
func (w *wrapperSchemaBackend) Schema() []registry.FieldSchema {
	return []registry.FieldSchema{{
		Key: "target", Label: "底层卷", Type: "volume-select",
		Required: true, AllowWrapper: true,
	}}
}

var registerUserVolWrapBackendOnce sync.Once

// registerUserVolWrapBackend 注册封装 fake backend（重复注册 panic；sync.Once 保证唯一）。
func registerUserVolWrapBackend() {
	registerUserVolWrapBackendOnce.Do(func() {
		registry.RegisterBackend(userVolWrapTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &wrapperSchemaBackend{}, nil
		})
	})
}

// userVolWrap 绑定用户卷 API handler 到固定 actor。
func userVolWrap(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/volumes/user", wrap(h.createUserVolumeHandler))
	mux.HandleFunc("GET /api/volumes/user", wrap(h.listUserVolumesHandler))
	mux.HandleFunc("DELETE /api/volumes/user", wrap(h.deleteUserVolumeHandler))
	return mux
}

// postUserVolume 发送创建用户卷请求。
func postUserVolume(t *testing.T, mux *http.ServeMux, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/volumes/user", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestUserVolumeAPI_Create 创建 → store 有 + Set.External 可查。
func TestUserVolumeAPI_Create(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	rec := postUserVolume(t, mux, map[string]any{
		"name": "userdisk1", "type": userVolumeTestType, "capacity": 1000,
		"extra": map[string]any{"bduss": "test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// store 有
	v, err := store.Get("alice", "userdisk1")
	if err != nil || v == nil {
		t.Fatalf("store.Get = %v/%v, want 存在", v, err)
	}
	if v.Owner != "alice" || v.Capacity != 1000 {
		t.Fatalf("卷 owner/capacity = %q/%d, want alice/1000", v.Owner, v.Capacity)
	}
	// Set.External 可查
	if h.volSet.External("userdisk1") == nil {
		t.Fatal("Set.External(userdisk1) = nil, want 已注册")
	}
}

// TestUserVolumeAPI_Create_BadType 未注册 type → 400。
func TestUserVolumeAPI_Create_BadType(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	rec := postUserVolume(t, mux, map[string]any{
		"name": "bad", "type": "no-such-type", "extra": map[string]any{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestUserVolumeAPI_List_OwnerFilter 两个 owner 各建 → GET 只列自己。
func TestUserVolumeAPI_List_OwnerFilter(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	muxAlice := userVolWrap(h, "alice")
	muxBob := userVolWrap(h, "bob")

	postUserVolume(t, muxAlice, map[string]any{"name": "alice-disk", "type": userVolumeTestType, "extra": map[string]any{}})
	postUserVolume(t, muxBob, map[string]any{"name": "bob-disk", "type": userVolumeTestType, "extra": map[string]any{}})

	req := httptest.NewRequest(http.MethodGet, "/api/volumes/user", nil)
	rec := httptest.NewRecorder()
	muxAlice.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	var resp struct {
		Volumes []UserVolume `json:"volumes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Volumes) != 1 || resp.Volumes[0].Name != "alice-disk" {
		t.Fatalf("alice 列表 = %+v, want 只含 alice-disk", resp.Volumes)
	}
	// C3：外部卷 usage 填充（fake backend UsageProvider 返回 42）。
	if resp.Volumes[0].Usage != 42 {
		t.Fatalf("alice-disk usage = %d, want 42（卷级计数查询）", resp.Volumes[0].Usage)
	}
}

// TestUserVolumeAPI_Delete_InUse 同步任务引用中 → 409。
func TestUserVolumeAPI_Delete_InUse(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, true)
	mux := userVolWrap(h, "alice")

	// 建卷 + 建引用任务（remote=userdisk1，volume=userdisk1）
	postUserVolume(t, mux, map[string]any{"name": "userdisk1", "type": userVolumeTestType, "extra": map[string]any{}})
	mgr := h.syncMgr
	_, _, err := mgr.CreateTask(syncmgr.CreateRequest{
		Direction: "push", Remote: "userdisk1", Owner: "alice",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name=userdisk1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestUserVolumeAPI_Delete_OwnerMismatch 跨 owner 删除 → 404。
func TestUserVolumeAPI_Delete_OwnerMismatch(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	muxAlice := userVolWrap(h, "alice")
	muxBob := userVolWrap(h, "bob")

	postUserVolume(t, muxAlice, map[string]any{"name": "alice-disk", "type": userVolumeTestType, "extra": map[string]any{}})

	req := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name=alice-disk", nil)
	rec := httptest.NewRecorder()
	muxBob.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob DELETE alice-disk = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateUserVolume_Wrapper_RequiresTarget 封装卷建卷缺失/非法底层卷 → 400、不落盘。
func TestCreateUserVolume_Wrapper_RequiresTarget(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	// 缺 target 字段 → 400（schema volume-select 必填）。
	rec := postUserVolume(t, mux, map[string]any{
		"name": "v1", "type": userVolWrapTestType, "extra": map[string]any{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 target POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if v, _ := store.Get("alice", "v1"); v != nil {
		t.Fatal("缺 target 的卷仍落盘（应不落盘）")
	}

	// target 指向不存在的卷 → 400。
	rec = postUserVolume(t, mux, map[string]any{
		"name": "v2", "type": userVolWrapTestType, "extra": map[string]any{"target": "nonexistent"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("target 不存在 POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if v, _ := store.Get("alice", "v2"); v != nil {
		t.Fatal("底层卷不存在的卷仍落盘（应不落盘）")
	}
}

// TestCreateUserVolume_Wrapper_CycleRejected 封装卷底层卷成环 → 400、不落盘。
//
// 场景：合法链（leaf v1→main；wrapper v2→v1 允许）成立；预先登记相互成环的 cyc-x/cyc-y，
// 新建指向其中任意一者的卷 → 环检测命中 → 400 errVolumeCycle、不落盘。
func TestCreateUserVolume_Wrapper_CycleRejected(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	// 合法：v1 底层 main（默认本地叶卷）。
	if rec := postUserVolume(t, mux, map[string]any{"name": "v1", "type": userVolWrapTestType, "extra": map[string]any{"target": "main"}}); rec.Code != http.StatusOK {
		t.Fatalf("v1→main = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 合法：v2→v1（wrapper 可作底层，链继续；v1→main 终止）。
	if rec := postUserVolume(t, mux, map[string]any{"name": "v2", "type": userVolWrapTestType, "extra": map[string]any{"target": "v1"}}); rec.Code != http.StatusOK {
		t.Fatalf("v2→v1 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// 预置相互成环的既有卷（cyc-x.target=cyc-y，cyc-y.target=cyc-x）——模拟装配期绕过
	// 建卷校验的历史/config 遗留，用于验证新卷追链命中环。
	be := &wrapperSchemaBackend{}
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: "cyc-x", Type: userVolWrapTestType, Extra: map[string]any{"target": "cyc-y"}}, be); err != nil {
		t.Fatalf("预置 cyc-x: %v", err)
	}
	if err := h.volSet.AddExternalVolume(volume.Volume{Name: "cyc-y", Type: userVolWrapTestType, Extra: map[string]any{"target": "cyc-x"}}, be); err != nil {
		t.Fatalf("预置 cyc-y: %v", err)
	}
	// 新建卷指向环上任意节点 → 追链回落已访问节点 → 400 防环、不落盘。
	rec := postUserVolume(t, mux, map[string]any{
		"name": "cyc-new", "type": userVolWrapTestType, "extra": map[string]any{"target": "cyc-x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("成环 POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if v, _ := store.Get("alice", "cyc-new"); v != nil {
		t.Fatal("成环卷仍落盘（应 fail-closed 不落盘）")
	}
}

// userVolFallbackWrapType 是「wrapper 但未登记任何 schema」的 fake 后端类型（用 wrapper
// category 名 "secrets"）。pkg/server 测试进程无 cmd/sproxy 的 secrets 生产注册（package
// main 不可 import），且 pkg/server 无其它测试用它（"secretdata" 被
// TestCloudHandler_CreateGroup_TransferParam 以 per-test 注册占用，不宜并发冲突；"egress"
// 同理）。注册一次、不注销（仿 registerUserVolTestBackend 模式）。
const userVolFallbackWrapType = "secrets"

// noSchemaBackend 不实现 SchemaProvider（无 schema），亦响应 NewBackend。
type noSchemaBackend struct{}

func (n *noSchemaBackend) FS() syncpkg.FS { return nil }
func (n *noSchemaBackend) Close() error   { return nil }

var registerUserVolFallbackOnce sync.Once

// registerUserVolFallbackBackend 注册未登记 schema 的 wrapper fake backend。
func registerUserVolFallbackBackend() {
	registerUserVolFallbackOnce.Do(func() {
		registry.RegisterBackend(userVolFallbackWrapType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &noSchemaBackend{}, nil
		})
	})
}

// TestCreateUserVolume_Wrapper_SchemaFallback 兜底：wrapper 类型即使未登记 schema
// （无 SchemaProvider、无静态表）也强制 target 校验——防环不空转（R1-C1 修复）。
func TestCreateUserVolume_Wrapper_SchemaFallback(t *testing.T) {
	t.Parallel()
	registerUserVolFallbackBackend()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	// 缺 target → 400（兜底强制 volume-select 必填）。
	rec := postUserVolume(t, mux, map[string]any{"name": "sf1", "type": userVolFallbackWrapType, "extra": map[string]any{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 target POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if v, _ := store.Get("alice", "sf1"); v != nil {
		t.Fatal("缺 target 的卷仍落盘（应不落盘）")
	}
	// target 不存在 → 400 统一文案。
	rec = postUserVolume(t, mux, map[string]any{"name": "sf2", "type": userVolFallbackWrapType, "extra": map[string]any{"target": "no-such"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("target 不存在 POST = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	// 合法.target=main → 200。
	rec = postUserVolume(t, mux, map[string]any{"name": "sf3", "type": userVolFallbackWrapType, "extra": map[string]any{"target": "main"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("sf3→main = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestCreateUserVolume_Wrapper_OwnerIsolation owner 隔离：目标底层卷仅限本 owner 可用卷
// （volume.AllowedVolumes）；无权/不存在统一文案（防枚举 + 防跨 owner 封装他人私有卷）。
func TestCreateUserVolume_Wrapper_OwnerIsolation(t *testing.T) {
	t.Parallel()
	registerUserVolWrapBackend()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "bob-private", Root: filepath.Join(cfg.StorageRoot, "bobp"),
			ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"bob"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	muxAlice := userVolWrap(h, "alice")
	muxBob := userVolWrap(h, "bob")

	// alice 无权封装 bob-private → 400 统一文案（不区分「不存在/无权」，防枚举）。
	rec := postUserVolume(t, muxAlice, map[string]any{"name": "a1", "type": userVolWrapTestType, "extra": map[string]any{"target": "bob-private"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("alice wrap bob-private = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "不存在或无权") {
		t.Fatalf("应统一文案（不泄露存在性）: %s", rec.Body.String())
	}
	if v, _ := store.Get("alice", "a1"); v != nil {
		t.Fatal("越权卷不应落盘")
	}
	// bob 封装自己的私有卷 → 200。
	rec = postUserVolume(t, muxBob, map[string]any{"name": "b1", "type": userVolWrapTestType, "extra": map[string]any{"target": "bob-private"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("bob wrap own = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if v, _ := store.Get("bob", "b1"); v == nil {
		t.Fatal("bob 的合法封装应落盘")
	}
}

// TestUserVolumeAPI_ACL_CrossOwnerDenied（评审 S1 回归）：新建用户卷强制 owner-only ACL——
// 非 owner（bob）经 ?volume=<alice卷> 的数据面候选一律不命中（404 语义、不泄存在性），
// alice 自可访问。此前零值 ACL（Mode==""）令 volume.Authorize 对**任意 owner** 恒 true，
// 绕过 I2 隔离（列表/下载/封装成链/转存目标跨 owner 滥用）。
func TestUserVolumeAPI_ACL_CrossOwnerDenied(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	muxAlice := userVolWrap(h, "alice")

	rec := postUserVolume(t, muxAlice, map[string]any{
		"name": "userdisk1", "type": userVolumeTestType, "extra": map[string]any{"bduss": "test"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 持久化 ACL（owner-only）：store 落盘即携带，重启恢复据此重建。
	uv, err := store.Get("alice", "userdisk1")
	if err != nil || uv == nil {
		t.Fatalf("store.Get = %v/%v, want 存在", uv, err)
	}
	if uv.ACL.Mode != volume.ModeAllow {
		t.Fatalf("新建用户卷 ACL.Mode = %q, want %q", uv.ACL.Mode, volume.ModeAllow)
	}
	if _, ok := uv.ACL.Owners["alice"]; !ok {
		t.Fatalf("新建用户卷 ACL.Owners 缺 alice: %v", uv.ACL.Owners)
	}
	// registry 注册的卷同样带 ACL（Authorize 据此 fail-closed）。
	rv, ok := h.volSet.ByName("userdisk1")
	if !ok {
		t.Fatal("ByName(userdisk1) 未命中")
	}
	if !rv.Authorize("alice") {
		t.Fatal("alice 对自己卷应授权")
	}
	if rv.Authorize("bob") {
		t.Fatal("bob 对 alice 卷应 fail-closed（零值 ACL 修复前恒 true）")
	}
	// 数据面统一入口 externalCandidates：显式 ?volume= 非 owner 不命中（404 语义）。
	reqBob := httptest.NewRequest(http.MethodGet, "/api/files?volume=userdisk1", nil).
		WithContext(withActor(context.Background(), "bob"))
	if got := h.externalCandidates(reqBob, "bob", "userdisk1"); len(got) != 0 {
		t.Fatalf("bob ?volume=alice 卷 candidates = %+v, want 空（不泄存在性）", got)
	}
	reqAlice := httptest.NewRequest(http.MethodGet, "/api/files?volume=userdisk1", nil).
		WithContext(withActor(context.Background(), "alice"))
	if got := h.externalCandidates(reqAlice, "alice", "userdisk1"); len(got) != 1 {
		t.Fatalf("alice ?volume=自己卷 candidates = %d, want 1", len(got))
	}
}
