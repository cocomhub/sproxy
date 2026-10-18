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
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// userVolumeTestType 是用户卷 API 测试用的 fake backend 类型（全局注册一次）。
const userVolumeTestType = "user-vol-test"

// fakeUserVolBackend 是测试用 ExternalBackend（内存 FS；Close 计数）。
type fakeUserVolBackend struct {
	closed bool
	fs     *extFS
}

func (f *fakeUserVolBackend) FS() syncpkg.FS {
	if f.fs == nil {
		f.fs = &extFS{files: map[string]string{}}
	}
	return f.fs
}
func (f *fakeUserVolBackend) Close() error { f.closed = true; return nil }

// 注：曾在此实现 registry.UsageProvider（固定 42）验证列表填充；现卷级记账权威在
// capacity.Backend 计数器（FS 层），测试改为经 FS 写入后断言 Usage。

// registerUserVolTestBackend 注册 fake backend（重复注册 panic；sync.Once 保证唯一）。
func registerUserVolTestBackend() {
	registerUserVolTestBackendOnce.Do(func() {
		registry.RegisterBackend(userVolumeTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &fakeUserVolBackend{}, nil
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

// userVolOwnerProbeTypePrefix 是「owner 上下文探针」后端类型名前缀：每用例注册**唯一类型名**
// （+ t.Cleanup 反注册）⇒ 探针状态是闭包局部变量，与其它并行用例零共享（无需全局可变状态）。
const userVolOwnerProbeTypePrefix = "user-vol-owner-probe-"

// ownerProbeBackend 是最小 ExternalBackend（建卷路径只要求 FS/Close；容量包装会再叠一层）。
type ownerProbeBackend struct{}

func (ownerProbeBackend) FS() syncpkg.FS { return nil }
func (ownerProbeBackend) Close() error   { return nil }

// TestUserVolumeAPI_Create_InjectsOwnerIntoBackendCtx 锁定**建卷路径的 owner 贯穿**
// （用户裁定 2026-10-15 第 3 点的前提）：后端构造发生在请求内，故必须把请求 owner 注入构造
// ctx —— 只有这样 `extra.secret_url: secrets://default-secrets/<名>` 才能解析到**本 owner 的**
// per-owner 默认桶（`secrets.OwnerFromContext` 的唯一生产者就是该处 wiring）。
//
// 探针方式：注册一个把 ctx owner 记入闭包的假后端（唯一类型名 ⇒ 可并行、无全局状态）。
// 变异：去掉 createUserVolumeHandler 里的 `secrets.WithOwner(...)` → 探针读到空 owner → 红。
func TestUserVolumeAPI_Create_InjectsOwnerIntoBackendCtx(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)

	typ := userVolOwnerProbeTypePrefix + "alice"
	var seenOwner string
	registry.RegisterBackend(typ, func(ctx context.Context, _ volume.Volume) (registry.ExternalBackend, error) {
		seenOwner = secrets.OwnerFromContext(ctx)
		return ownerProbeBackend{}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })

	mux := userVolWrap(h, "alice")
	rec := postUserVolume(t, mux, map[string]any{"name": "probe", "type": typ, "extra": map[string]any{}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if seenOwner != "alice" {
		t.Fatalf("后端构造 ctx 中的 owner = %q, want alice（建卷路径未贯穿 owner ⇒ per-owner secret_url 不可解析）", seenOwner)
	}
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
	// 卷级容量计数从创建期即生效（不再等重启 restore）：Capacity = 请求容量。
	if up, ok := h.volSet.External("userdisk1").(registry.UsageProvider); !ok || up.Capacity() != 1000 {
		t.Fatalf("创建后 UsageProvider.Capacity = %v/%v, want true/1000（创建期包 CapacityFS）", ok, up)
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

// TestUserVolumeAPI_Create_BadName 回归（评审 F7）：非法卷名必须在**拼计数器路径之前**
// 被拒（400），不得先按 req.Name 去读 meta 目录之外的 `*.capacity`（读到脏数据/500）。
// 变异：去掉提前的 ValidSegmentName 检查 → 走 store.Create 才拒 → 本用例仍 400 但覆盖了
// 「路径已拼」的顺序缺陷（配合遍历名可观测到 500）。
func TestUserVolumeAPI_Create_BadName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../evil", "a/b", ".", "..", "a\b"} {
		h, _ := newUserVolumeAPIHandlers(t, false)
		mux := userVolWrap(h, "alice")
		rec := postUserVolume(t, mux, map[string]any{
			"name": name, "type": userVolumeTestType, "extra": map[string]any{"bduss": "t"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法卷名 %q → %d, want 400 (body: %s)", name, rec.Code, rec.Body.String())
		}
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

	// C3：卷级计数 usage 填充——经 FS 写入 42 字节后列表反映（创建期即包 CapacityFS）。
	if err := h.volSet.External("alice-disk").FS().WriteFile(
		context.Background(), syncpkg.LogicalKey{}, "f.bin", bytes.NewReader(make([]byte, 42)), 42, 0); err != nil {
		t.Fatalf("写入用户卷: %v", err)
	}
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
// category 名 "egress"）。pkg/server 测试进程无 cmd/sproxy 的生产注册（package main 不可
// import）。**不能用 "secrets"/"secretdata"**：merge origin/master(#805) 后
// cloud_download_handler_test.go 的 registerFakeSecretsScheme 注册了 "secrets"，
// TestCloudHandler_CreateGroup_TransferParam 以 per-test 注册占用了 "secretdata"——同进程
// 双注册同名 backend 会 panic（registry.RegisterBackend 重名 fail-fast）。"egress" 同为
// wrapper category（CategoryOf=="wrapper"）且无其它测试占用，测兜底语义等价。
// 注册一次、不注销（仿 registerUserVolTestBackend 模式）。
const userVolFallbackWrapType = "egress"

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

// TestUserVolumeAPI_List_RedactsCredentials D-CRITICAL 回归：GET /api/volumes/user
// 不得回显凭据（bduss/stoken/secret_key 等）——store.Get 已解密 Extra，列表响应须剔除
// 凭据键（防 BDUSS 明文泄露到浏览器）。
func TestUserVolumeAPI_List_RedactsCredentials(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	postUserVolume(t, mux, map[string]any{
		"name": "cred-disk", "type": userVolumeTestType,
		"extra": map[string]any{"bduss": "secret-bduss", "baidu_root": "/x", "capacity": "1"},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/volumes/user", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-bduss") {
		t.Fatalf("列表响应不得回显 BDUSS 明文: %s", body)
	}
	if !strings.Contains(body, "/x") {
		t.Fatalf("非敏感配置（baidu_root）应保留: %s", body)
	}
}

// TestUserVolumeAPI_DeleteRecreateKeepsUsage P2 回归：删除用户卷不清理容量计数文件，
// 同名重建必须 Load 到旧占用（否则 delete+recreate 归零 → 反复绕过「Σ占用 ≤ 卷限额」）。
func TestUserVolumeAPI_DeleteRecreateKeepsUsage(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")
	body := map[string]any{"name": "ud", "type": userVolumeTestType, "capacity": 1000, "extra": map[string]any{"bduss": "t"}}
	if rec := postUserVolume(t, mux, body); rec.Code != http.StatusOK {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	if err := h.volSet.External("ud").FS().WriteFile(context.Background(), syncpkg.LogicalKey{}, "f.bin", bytes.NewReader(make([]byte, 42)), 42, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// delete
	dreq := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name=ud", nil)
	drec := httptest.NewRecorder()
	mux.ServeHTTP(drec, dreq)
	if drec.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s)", drec.Code, drec.Body.String())
	}
	// recreate（同名）
	if rec := postUserVolume(t, mux, body); rec.Code != http.StatusOK {
		t.Fatalf("recreate = %d (%s)", rec.Code, rec.Body.String())
	}
	up, ok := h.volSet.External("ud").(registry.UsageProvider)
	if !ok || up.Usage() != 42 {
		t.Fatalf("delete+recreate 后 Usage 应保留 42（不得归零绕过限额）, ok=%v usage=%d", ok, up.Usage())
	}
}

// TestUserVolumeAPI_RecreateInheritsCapacity 回归（评审 F6）：用户卷删除时**有意保留**
// `.capacity`（远端字节仍在），但 limit 此前完全取自请求体 → delete+recreate 写 `capacity:0`
// 即把上限变「不限」（「防放大」保护不完整）。新语义：`capacity:0` 且未显式 `unlimited`
// ⇒ **继承持久化上限**；`unlimited:true` ⇒ 真不限。
func TestUserVolumeAPI_RecreateInheritsCapacity(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")
	create := func(body map[string]any) *httptest.ResponseRecorder {
		rec := postUserVolume(t, mux, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST = %d, body=%s", rec.Code, rec.Body.String())
		}
		return rec
	}
	del := func() {
		req := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name=ud1", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE = %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	capOf := func() int64 {
		up, ok := h.volSet.External("ud1").(registry.UsageProvider)
		if !ok {
			t.Fatal("卷应注册为 UsageProvider（创建期包 CapacityFS）")
		}
		return up.Capacity()
	}

	create(map[string]any{"name": "ud1", "type": userVolumeTestType, "capacity": 1000,
		"extra": map[string]any{"bduss": "t"}})
	if got := capOf(); got != 1000 {
		t.Fatalf("显式上限应生效: %d", got)
	}
	del()
	// 重建写 capacity:0（未显式 unlimited）→ 必须继承 1000（否则绕过上限）。
	create(map[string]any{"name": "ud1", "type": userVolumeTestType, "capacity": 0,
		"extra": map[string]any{"bduss": "t"}})
	if got := capOf(); got != 1000 {
		t.Fatalf("重建 capacity:0 应继承持久化上限 1000（F6）, got %d", got)
	}
	// 生效上限也必须落 store 元数据——否则重启 restore 用请求里的 0 把继承的上限抹掉。
	if v, gerr := store.Get("alice", "ud1"); gerr != nil || v == nil || v.Capacity != 1000 {
		t.Fatalf("重建后 store 元数据 capacity 应为继承值 1000, got %+v err=%v", v, gerr)
	}
	// 显式无限 → 真不限（0）。
	del()
	create(map[string]any{"name": "ud1", "type": userVolumeTestType, "capacity": 0, "unlimited": true,
		"extra": map[string]any{"bduss": "t"}})
	if got := capOf(); got != 0 {
		t.Fatalf("unlimited:true 应不限, got %d", got)
	}
}

// TestUserVolumeAPI_Create_EnclosureTypeRejected 回归：`type: enclosure` 是**内部装配机制**，
// 不是可建卷类型——建卷 API 必须 400 拒绝并给出正确写法提示（否则会一路走到
// registry.NewBackend 报「类型未注册后端」，客户端无从判断该怎么改）。
// 变异：去掉 createUserVolumeHandler 的 enclosure 分支 → 本用例拿到的错误文案不含
// 「内部装配机制」（退化为 NewBackend 的未注册文案）→ 红。
func TestUserVolumeAPI_Create_EnclosureTypeRejected(t *testing.T) {
	t.Parallel()
	h, store := newUserVolumeAPIHandlers(t, false)
	mux := userVolWrap(h, "alice")

	rec := postUserVolume(t, mux, map[string]any{
		"name": "vault", "type": "enclosure",
		"extra": map[string]any{"source": "main:videos", "wrapper": userVolumeTestType},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("type=enclosure 建卷应 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "内部装配机制") {
		t.Fatalf("错误文案应说明 enclosure 是内部装配机制, got %s", rec.Body.String())
	}
	// 不落盘（fail-closed 在建卷前置校验内）。
	if v, err := store.Get("alice", "vault"); err != nil || v != nil {
		t.Fatalf("被拒的卷不应落盘: v=%v err=%v", v, err)
	}
}
