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
