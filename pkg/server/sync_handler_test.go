// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

// newSyncTestEnv 创建带 SyncManager 的测试服务。remoteURL 是 sync_remotes[0].url。
func newSyncTestEnv(t *testing.T, remoteURL string, modifyCfg func(*Config)) (h *Handlers, baseURL string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "test", BuildAt: "test", Logger: slog.Default(),
	}
	noAuth := defaultNoAuthRegOpts()
	opts.CredentialRing = noAuth.CredentialRing
	opts.CredentialStore = noAuth.CredentialStore
	opts.AllowInsecureLoopback = noAuth.AllowInsecureLoopback
	h = RegisterRoutes(t.Context(), opts)
	remotes := []syncmgr.RemoteConfig{{
		Name: "r1", URL: remoteURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64),
		AccessKeyID: testEntryID("test-ak"),
	}}
	exec := syncexec.NewExecutor(h.syncTenantRoot, h.logger)
	exec.SetTenantScopeResolver(h.SyncQuotaScope())
	sm := syncmgr.NewManager(h.syncTenantRoot, h.listTenantIDs, nil, int(CategoryUserFiles), remotes,
		exec, h.logger,
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour, PerFileReserve: true})
	sm.SetQuotaResolver(h.SyncQuotaStore())
	h.SetSyncMgr(sm)

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		sm.Stop()
		_ = h.Close()
	})
	return h, ts.URL
}

// writeUserFile 把 content 写入指定 owner 租户 user 桶（供 push 源文件预置）。
// 空 owner → anonymous 租户。布局迁移后 push 源在 <root>/<tenant>/user/ 下。
func writeUserFile(t *testing.T, h *Handlers, owner, name, content string) {
	t.Helper()
	tnt := h.tenantFor(owner)
	if tnt == nil {
		t.Fatal("tenantFor 不可用")
	}
	userAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		t.Fatal("派生 user 根失败")
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(userAbs, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// emptyRemote 返回一个空的远程 sproxy（GET /api/files 空列表、stat 404）。
func emptyRemote(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[],"total":0}`))
	})
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// blockingListRemote 的 GET /api/files 阻塞直到 blockCh 关闭或请求 ctx 取消。
func blockingListRemote(t *testing.T, blockCh chan struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-blockCh:
		}
		http.Error(w, "aborted", http.StatusGatewayTimeout)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doSyncJSON(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestSyncAPI_CreateTask_Push(t *testing.T) {
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	// seed 本地源文件到 anonymous 租户 user 桶，使 push 任务真正可成功（审查 M-2：
	// src 不存在会快速 failed，硬断言 pending 属时序侥幸，产生 flake）。
	writeUserFile(t, h, "", "x.txt", "data")

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析失败: %v, body=%s", err, body)
	}
	if task.ID == "" || task.Direction != "push" || task.Remote != "r1" {
		t.Fatalf("任务字段不符: %+v", task)
	}
	// 状态为异步流转（响应时可能 pending/syncing/completed）；seed 后不应 failed
	if task.Status == "failed" || task.Status == "" {
		t.Fatalf("任务状态不应为 failed/空（seed 后 src 存在）: %+v", task)
	}
}

func TestSyncAPI_CreateTask_InvalidDirection(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"sideways","remote":"r1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 direction 应返回 400，got %d: %s", code, body)
	}
}

func TestSyncAPI_CreateTask_RemoteMissing(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"nope"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置 remote 应返回 400，got %d: %s", code, body)
	}
}

// TestSyncAPI_CreateTask_QuotaContract 验证 pull 任务创建不受存储配额装配卡死（201 契约）：
// MaxStorageBytes=1024 只装配到 globalPool，而 anonymous 租户 user 桶 Scope 无上限
// （OwnerQuotaFor=0 不限制）→ TryReserve(1GiB) 恒成功（reserved=1GiB）→ 创建成功 201，
// 不真正触发降级分支（降级由 syncmgr.TestCreateTask_QuotaHeadroomDegrades 覆盖；
// 配额 enforce 由 reconcile 强制，见 TestReconcileQuota_TryReserveFailOnCompletion）。
func TestSyncAPI_CreateTask_QuotaContract(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, func(c *Config) { c.MaxStorageBytes = 1024 })

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"pull","remote":"r1","src":""}`)
	if code != http.StatusCreated {
		t.Fatalf("pull 创建应成功 201（配额装配不影响创建），got %d: %s", code, body)
	}
}

func TestSyncAPI_ListAndGet(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	// 列表
	code, body = doSyncJSON(t, "GET", base+"/api/sync/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("列表应返回 200，got %d: %s", code, body)
	}
	var listResp struct {
		Success bool `json:"success"`
		Tasks   []syncmgr.SyncTaskMeta
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatal(err)
	}
	if !listResp.Success || len(listResp.Tasks) != 1 {
		t.Fatalf("列表不符: %s", body)
	}
	if listResp.Tasks[0].ID != task.ID {
		t.Fatalf("列表任务 ID 不符: %+v", listResp.Tasks[0])
	}

	// 单查
	code, body = doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusOK {
		t.Fatalf("单查应返回 200，got %d: %s", code, body)
	}
	var got syncmgr.SyncTask
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID {
		t.Fatalf("单查任务不符: %+v", got)
	}

	// 未知 ID → 404
	code, _ = doSyncJSON(t, "GET", base+"/api/sync/tasks/does-not-exist", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知任务应返回 404，got %d", code)
	}
}

func TestSyncAPI_CancelTask(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	srv := blockingListRemote(t, blockCh)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	// pull src="" → GET /api/files 阻塞 → 任务保持 syncing
	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"pull","remote":"r1","src":""}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	// 等待 syncing
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		if cur.Status == "syncing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, body = doSyncJSON(t, "POST", base+"/api/sync/tasks/"+task.ID+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("取消应返回 200，got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "cancelled") {
		t.Fatalf("取消响应应包含 cancelled: %s", body)
	}

	// 复查状态
	_, b := doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	var cur syncmgr.SyncTask
	_ = json.Unmarshal(b, &cur)
	if cur.Status != "cancelled" {
		t.Fatalf("取消后状态应为 cancelled，got %q", cur.Status)
	}
}

func TestSyncAPI_DeleteTask(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	code, body = doSyncJSON(t, "DELETE", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusOK {
		t.Fatalf("删除应返回 200，got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "deleted") {
		t.Fatalf("删除响应应包含 deleted: %s", body)
	}

	// 再查 → 404
	code, _ = doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("删除后单查应返回 404，got %d", code)
	}
}

func TestSyncAPI_NotConfigured(t *testing.T) {
	// 不注入 SyncManager → 400
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{Mux: mux, CfgPtr: &cfgPtr, Version: "test", BuildAt: "test", Logger: slog.Default()}
	noAuth := defaultNoAuthRegOpts()
	opts.CredentialRing = noAuth.CredentialRing
	opts.CredentialStore = noAuth.CredentialStore
	opts.AllowInsecureLoopback = noAuth.AllowInsecureLoopback
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	code, body := doSyncJSON(t, "POST", ts.URL+"/api/sync/tasks",
		`{"direction":"push","remote":"r1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置 sync 应返回 400，got %d: %s", code, body)
	}
}

// syncOwnerMux 构造把固定 actor 注入请求 ctx 后转发 sync handler 的 mux。
// 模拟已认证请求（authMiddleware 会把 SproxySig AK / API key 名写入 ctx）。
func syncOwnerMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", wrap(h.syncCreateTask))
	mux.HandleFunc("GET /api/sync/tasks", wrap(h.syncListTasks))
	mux.HandleFunc("GET /api/sync/tasks/{id}", wrap(h.syncGetTask))
	mux.HandleFunc("POST /api/sync/tasks/{id}/cancel", wrap(h.syncCancelTask))
	mux.HandleFunc("DELETE /api/sync/tasks/{id}", wrap(h.syncDeleteTask))
	return mux
}

// doSyncOwner 以指定 actor 的 mux 发起请求，返回 (status, body)。
func doSyncOwner(t *testing.T, mux *http.ServeMux, method, path, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// TestSyncAPI_OwnerFiltering 验证同步任务 API 的多租户隔离（阶段 6 工作项 C）：
// 不同 AK 用户只能看到/操作自己的任务；admin（空 owner）可见全部；API 返回 owner。
func TestSyncAPI_OwnerFiltering(t *testing.T) {
	srv := emptyRemote(t)
	h, _ := newSyncTestEnv(t, srv.URL, nil)
	// 预置源文件到各自 owner 租户 user 桶，使 push 任务可成功（避免快速 failed）
	writeUserFile(t, h, "ak-A", "a.txt", "a")
	writeUserFile(t, h, "ak-B", "b.txt", "b")

	muxA := syncOwnerMux(h, "ak-A")
	muxB := syncOwnerMux(h, "ak-B")
	muxAdmin := syncOwnerMux(h, "")

	// A 与 B 各创建任务
	code, body := doSyncOwner(t, muxA, "POST", "/api/sync/tasks", `{"direction":"push","remote":"r1","src":"a.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("A 创建应 201, got %d: %s", code, body)
	}
	var taskA syncmgr.SyncTask
	if err := json.Unmarshal(body, &taskA); err != nil {
		t.Fatal(err)
	}
	if taskA.Owner != "ak-A" {
		t.Fatalf("A 创建任务 Owner = %q, want ak-A", taskA.Owner)
	}

	code, body = doSyncOwner(t, muxB, "POST", "/api/sync/tasks", `{"direction":"push","remote":"r1","src":"b.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("B 创建应 201, got %d: %s", code, body)
	}
	var taskB syncmgr.SyncTask
	if err := json.Unmarshal(body, &taskB); err != nil {
		t.Fatal(err)
	}
	if taskB.Owner != "ak-B" {
		t.Fatalf("B 创建任务 Owner = %q, want ak-B", taskB.Owner)
	}

	// A 的列表只含 A 的任务
	code, body = doSyncOwner(t, muxA, "GET", "/api/sync/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("A 列表应 200, got %d", code)
	}
	var listA struct {
		Tasks []syncmgr.SyncTaskMeta `json:"tasks"`
	}
	if err := json.Unmarshal(body, &listA); err != nil {
		t.Fatal(err)
	}
	if len(listA.Tasks) != 1 || listA.Tasks[0].ID != taskA.ID || listA.Tasks[0].Owner != "ak-A" {
		t.Fatalf("A 列表应只含 A 的任务且带 owner: %s", body)
	}

	// A Get B 的任务 → 404
	code, _ = doSyncOwner(t, muxA, "GET", "/api/sync/tasks/"+taskB.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("A Get B 的任务应 404, got %d", code)
	}

	// A 取消/删除 B 的任务 → 404
	code, _ = doSyncOwner(t, muxA, "POST", "/api/sync/tasks/"+taskB.ID+"/cancel", "")
	if code != http.StatusNotFound {
		t.Fatalf("A 取消 B 的任务应 404, got %d", code)
	}
	code, _ = doSyncOwner(t, muxA, "DELETE", "/api/sync/tasks/"+taskB.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("A 删除 B 的任务应 404, got %d", code)
	}

	// B 的任务仍存在（未被 A 取消/删除）
	code, _ = doSyncOwner(t, muxB, "GET", "/api/sync/tasks/"+taskB.ID, "")
	if code != http.StatusOK {
		t.Fatalf("B 的任务应仍存在, got %d", code)
	}

	// admin（空 owner）可见全部
	code, body = doSyncOwner(t, muxAdmin, "GET", "/api/sync/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("admin 列表应 200, got %d", code)
	}
	var listAdmin struct {
		Tasks []syncmgr.SyncTaskMeta `json:"tasks"`
	}
	if err := json.Unmarshal(body, &listAdmin); err != nil {
		t.Fatal(err)
	}
	if len(listAdmin.Tasks) != 2 {
		t.Fatalf("admin 列表应含 2 条任务: %s", body)
	}
}

// TestSyncAPI_UnauthenticatedOwnerEmpty 验证未认证创建同步任务 → owner 空（全局兼容）。
func TestSyncAPI_UnauthenticatedOwnerEmpty(t *testing.T) {
	srv := emptyRemote(t)
	h, _ := newSyncTestEnv(t, srv.URL, nil)
	// 预置源文件到 anonymous 租户 user 桶（未认证创建的任务 owner 为空）
	writeUserFile(t, h, "", "u.txt", "u")
	muxAdmin := syncOwnerMux(h, "")

	code, body := doSyncOwner(t, muxAdmin, "POST", "/api/sync/tasks", `{"direction":"push","remote":"r1","src":"u.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("未认证创建应 201, got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	if task.Owner != "" {
		t.Fatalf("未认证创建任务 Owner = %q, want 空", task.Owner)
	}

	// 认证用户 A 的列表应能看到空 owner 任务（全局兼容）
	muxA := syncOwnerMux(h, "ak-A")
	code, body = doSyncOwner(t, muxA, "GET", "/api/sync/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("A 列表应 200, got %d", code)
	}
	var listResp struct {
		Tasks []syncmgr.SyncTaskMeta `json:"tasks"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Tasks) != 1 {
		t.Fatalf("A 应能看见空 owner 任务, got %d: %s", len(listResp.Tasks), body)
	}
}

// TestSyncAPI_OwnerNotClientForgeable 验证（审查 Minor 4）：客户端 body 无法伪造
// owner——CreateRequest.Owner 为 json:"-"，服务端 decode 后强制覆盖为 ActorFrom。
// 请求 actor=A、body 声称 owner=B → 实际任务 owner 必须为 A。
func TestSyncAPI_OwnerNotClientForgeable(t *testing.T) {
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	mux := syncOwnerMux(h, "ak-A") // 注入 actor=ak-A
	_ = base

	code, body := doSyncOwner(t, mux, "POST", "/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt","owner":"ak-B"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析失败: %v, body=%s", err, body)
	}
	if task.Owner != "ak-A" {
		t.Fatalf("owner 应由服务端从请求 actor 派生（防伪造），got %q, want ak-A", task.Owner)
	}
}

// TestSyncAPI_PullChargesUserBucketQuota 端到端验证逐文件 guard 装配进真实 sync 链路：
// pull 走后端 SyncManager + syncexec.Executor（装配 SyncQuotaScope），两个远端文件落
// local 子目录 → alice user 桶 Scope committed == 两文件字节和（10）、Reserved 归 0，
// 且创建期占位 1GiB 在完成 reconcile 时已释放（PerFileReserve 模式）。
func TestSyncAPI_PullChargesUserBucketQuota(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("d1/a.txt", "aaaa")
	remote.SeedFile("d1/b.txt", "bbbbbb")
	remote.SeedDir("d1")
	h, _ := newSyncTestEnv(t, srv.URL, func(c *Config) { c.OwnerQuotas = map[string]int64{"alice": 10 << 30} })

	// 以 alice 身份创建并同步 pull 任务（syncOwnerMux 注入 actor=alice 到 ctx）
	mux := syncOwnerMux(h, "alice")
	code, body := doSyncOwner(t, mux, "POST", "/api/sync/tasks",
		`{"direction":"pull","remote":"r1","src":"d1","dst":"local","recursive":true}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应 201, got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析失败: %v, body=%s", err, body)
	}
	// 等待完成
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doSyncOwner(t, mux, "GET", "/api/sync/tasks/"+task.ID, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		if cur.Status == "completed" {
			break
		}
		if cur.Status == "failed" {
			t.Fatalf("任务应完成，实际 failed: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// alice user 桶配额 == 两个文件字节和（逐文件 guard 入账）
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 10 {
		t.Fatalf("pull 后 alice user 桶 Usage()=%d want 10（逐文件入账）", got)
	}
	if got := h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("pull 后 alice user 桶 Reserved()=%d want 0（占位已释放）", got)
	}
	// 磁盘真实落盘 <root>/alice/user/local/...
	tnt := h.tenantFor("alice")
	absA, ok := tnt.Root().Abs("user/local/a.txt")
	if !ok {
		t.Fatalf("派生文件绝对路径失败")
	}
	data, err := os.ReadFile(absA)
	if err != nil {
		t.Fatalf("alice/user/local/a.txt 应落盘: %v", err)
	}
	if string(data) != "aaaa" {
		t.Fatalf("a.txt 内容=%q want aaaa", data)
	}
}

// TestSyncAPI_PushDoesNotChargeUserBucket 验证（审查 D）push 不做本地配额记账：
// push 方向本地不预留、不逐文件 guard（syncexec.Executor 的 quotaLocalFS 只装饰 pull
// 的 dstFS；push 的 srcFS 是裸 LocalFS，数据单向流向远端 mock）——任务完成后 alice
// user 桶 Usage 必须仍等于源文件预置字节（零额外记账），而非把推送出去的字节重复计费。
func TestSyncAPI_PushDoesNotChargeUserBucket(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	h, _ := newSyncTestEnv(t, srv.URL, func(c *Config) { c.OwnerQuotas = map[string]int64{"alice": 100} })
	_ = h

	// 预置源文件到 alice 租户 user 桶（经上传 handler 记账，源文件本身占 user 桶字节——
	// 这部分是 user 桶的真实占用；raw writeUserFile 绕过配额池无法验证 push 不重复计费）。
	umux := http.NewServeMux()
	umux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		h.upload(w, r)
	})
	if code, resp := uploadAsPath(t, umux, "local/send.txt", []byte("0123456789")); code != http.StatusOK {
		t.Fatalf("预置上传应 200, got %d: %s", code, resp)
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 10 {
		t.Fatalf("预置后 alice user 桶 Usage()=%d want 10（源文件占用）", got)
	}

	// 以 alice 身份创建并同步 push 任务（数据流向本地 → 远端 mock）。
	mux := syncOwnerMux(h, "alice")
	code, body := doSyncOwner(t, mux, "POST", "/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"local","recursive":true}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应 201, got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析失败: %v, body=%s", err, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doSyncOwner(t, mux, "GET", "/api/sync/tasks/"+task.ID, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		if cur.Status == "completed" {
			break
		}
		if cur.Status == "failed" {
			t.Fatalf("任务应完成，实际 failed: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// push 完成后 user 桶 Usage 保持 = 源文件字节（10），零额外记账（无 reserved 泄漏）。
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 10 {
		t.Fatalf("push 后 alice user 桶 Usage()=%d want 10（push 不本地预留/记账）", got)
	}
	if got := h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("push 后 alice user 桶 Reserved()=%d want 0", got)
	}
	// 远端 mock 应已收到推送的文件（push 以 src 相对路径命名，local/send.txt → send.txt，
	// 与 pull 的 dst 相对命名对称；数据确实发出去，且源文件本地仍在）。
	snap := remote.SnapshotFiles()
	f, ok := snap["send.txt"]
	if !ok || string(f.Data) != "0123456789" {
		t.Fatalf("远端应收到 send.txt, got %v", snap)
	}
}

// TestQuota_TwoConcurrentPulls_CombinedUnderOwnerCap 验证（审查 D）多任务叠加父链竞争：
// owner 上限 10，两个并发 pull 任务各拉 6 字节文件（PerFileReserve 逐文件 guard）——
// 两个 6 字节不可能都通过租户聚合上限（6+6=12>10），至少一个文件/任务失败；
// alice user 桶最终 committed<=10 且无预留泄漏。
func TestQuota_TwoConcurrentPulls_CombinedUnderOwnerCap(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	// 两个独立子目录各 6 字节文件（src 不同避免去重吸收）。
	remote.SeedFile("d1/a.txt", "aaaaaa")
	remote.SeedFile("d2/b.txt", "bbbbbb")
	remote.SeedDir("d1")
	remote.SeedDir("d2")
	h, _ := newSyncTestEnv(t, srv.URL, func(c *Config) { c.OwnerQuotas = map[string]int64{"alice": 10} })

	mux := syncOwnerMux(h, "alice")
	create := func(src, dst string) string {
		t.Helper()
		code, body := doSyncOwner(t, mux, "POST", "/api/sync/tasks",
			`{"direction":"pull","remote":"r1","src":"`+src+`","dst":"`+dst+`","recursive":true}`)
		if code != http.StatusCreated {
			t.Fatalf("创建 pull(%s) 应 201, got %d: %s", src, code, body)
		}
		var task syncmgr.SyncTask
		if err := json.Unmarshal(body, &task); err != nil {
			t.Fatalf("解析失败: %v, body=%s", err, body)
		}
		return task.ID
	}
	idA := create("d1", "la")
	idB := create("d2", "lb")

	// 等待两任务都到终态。
	deadline := time.Now().Add(15 * time.Second)
	status := func(id string) string {
		t.Helper()
		_, b := doSyncOwner(t, mux, "GET", "/api/sync/tasks/"+id, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		return cur.Status
	}
	for time.Now().Before(deadline) {
		sA, sB := status(idA), status(idB)
		terminal := func(s string) bool { return s == "completed" || s == "failed" || s == "cancelled" }
		if terminal(sA) && terminal(sB) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sA, sB := status(idA), status(idB)
	if sA != "completed" && sA != "failed" {
		t.Fatalf("任务 A 应到终态, got %q", sA)
	}
	if sB != "completed" && sB != "failed" {
		t.Fatalf("任务 B 应到终态, got %q", sB)
	}

	// 至少一个任务失败（failed）或其文件级 ActionError（engine 吞错误后 completed）。
	anyFileError := func(id string) bool {
		_, b := doSyncOwner(t, mux, "GET", "/api/sync/tasks/"+id, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		for _, r := range cur.Results {
			if r.Action == "error" && r.Error != "" {
				return true
			}
		}
		return false
	}
	if sA != "failed" && sB != "failed" && !anyFileError(idA) && !anyFileError(idB) {
		t.Fatalf("两 pull 共 12 字节 > 上限 10，至少一个文件/任务应失败（A=%s B=%s）", sA, sB)
	}

	// user 桶最终 committed<=10、无预留泄漏。
	if got := h.quotaBucketFor("alice", "user").Usage(); got > 10 {
		t.Fatalf("并发 pull 后 alice user 桶 Usage()=%d want <=10（租户聚合上限）", got)
	}
	if got := h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("并发 pull 后 alice user 桶 Reserved()=%d want 0", got)
	}
}
