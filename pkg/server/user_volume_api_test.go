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
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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
		context.Background(), "f.bin", bytes.NewReader(make([]byte, 42)), 42, 0); err != nil {
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
	if err := h.volSet.External("ud").FS().WriteFile(context.Background(), "f.bin", bytes.NewReader(make([]byte, 42)), 42, 0); err != nil {
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

// TestFlushVolumeCounters_FlushesDirtyCounters 回归（F9 接线）：关机路径必须把各外部卷
// 计数器的待落盘变更写入快照——`Release` 已节流，不 flush 会让本次进程的删除量丢失，
// 重启后 used 偏高（伪 507）且逐次累积。
func TestFlushVolumeCounters_FlushesDirtyCounters(t *testing.T) {
	t.Parallel()
	path := capacity.VolumeCounterPath(t.TempDir(), "v1")
	cnt, err := capacity.NewPoolCounterPersistent(quota.NewPool(1000), path)
	if err != nil {
		t.Fatal(err)
	}
	if aerr := cnt.TryAdd(400); aerr != nil { // 增加恒同步落盘
		t.Fatal(aerr)
	}
	cnt.Release(100) // 减少节流：快照仍 400

	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(volume.Volume{Name: "v1", Type: "baidupcs"},
		capacity.WrapBackend(&extBackend{fs: &extFS{files: map[string]string{}}}, cnt)); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	h := &Handlers{volSet: vs, logger: testLogger()}
	h.flushVolumeCounters()

	// 快照必须已反映「减少」（400-100=300）。
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("读快照: %v", rerr)
	}
	var f struct {
		Used int64 `json:"used"`
	}
	if uerr := json.Unmarshal(raw, &f); uerr != nil {
		t.Fatalf("解析快照: %v", uerr)
	}
	if f.Used != 300 {
		t.Fatalf("关机 flush 后快照 used 应为 300, got %d", f.Used)
	}
}
