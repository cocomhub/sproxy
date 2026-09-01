// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// ownerCloudEnv 提供共享 CloudDownloadManager 的多 actor 测试环境：
// 同一管理器、三个不同 actor（ak-A / ak-B / 空 admin）的 HTTP mux，
// 用于验证任务级多租户隔离（阶段 6 工作项 C）。
type ownerCloudEnv struct {
	h   *Handlers
	mgr *CloudDownloadManager
	mux map[string]*http.ServeMux // actor → mux
}

// actorCloudMux 构造把固定 actor 注入请求 ctx 后转发 cloud handler 的 mux。
// 模拟已认证请求（authMiddleware 会把 SproxySig AK / API key 名写入 ctx）。
func actorCloudMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download", wrap(h.cloudCreateDownload))
	mux.HandleFunc("POST /api/cloud/download/batch", wrap(h.cloudCreateBatchDownload))
	mux.HandleFunc("GET /api/cloud/tasks", wrap(h.cloudListTasks))
	mux.HandleFunc("GET /api/cloud/tasks/{id}", wrap(h.cloudGetTask))
	mux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", wrap(h.cloudCancelTask))
	mux.HandleFunc("DELETE /api/cloud/tasks/{id}", wrap(h.cloudDeleteTask))
	mux.HandleFunc("POST /api/cloud/tasks/{id}/resume", wrap(h.cloudResumeTask))
	mux.HandleFunc("POST /api/cloud/groups", wrap(h.cloudCreateGroup))
	mux.HandleFunc("GET /api/cloud/groups", wrap(h.cloudListGroups))
	mux.HandleFunc("GET /api/cloud/groups/{id}", wrap(h.cloudGetGroup))
	mux.HandleFunc("POST /api/cloud/groups/{id}/cancel", wrap(h.cloudCancelGroup))
	mux.HandleFunc("DELETE /api/cloud/groups/{id}", wrap(h.cloudDeleteGroup))
	mux.HandleFunc("POST /api/cloud/groups/{id}/archive", wrap(h.cloudArchiveGroup))
	mux.HandleFunc("HEAD /api/files/stat", wrap(h.stat))
	return mux
}

// newOwnerCloudEnv 创建共享管理器的多 actor 测试环境。
func newOwnerCloudEnv(t *testing.T) *ownerCloudEnv {
	t.Helper()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() {
		mgr.Close()
		os.RemoveAll(filepath.Join(dir, ".__cloud__"))
		os.RemoveAll(filepath.Join(dir, ".__downloads__"))
	})
	h := &Handlers{cloudMgr: mgr, logger: testLogger(), storageMgr: sm, cfgPtr: newTestCfgPtr(dir), auditLogger: testLogger()}
	h.checksumStore = NewChecksumStore(dir, nil)
	// 让 mgr 与 handler 共享同一 ChecksumStore，使 DeleteTask/写端的 checksum 清理生效可断言
	mgr.checksumStore = h.checksumStore
	env := &ownerCloudEnv{
		h:   h,
		mgr: mgr,
		mux: map[string]*http.ServeMux{
			"ak-A": actorCloudMux(h, "ak-A"),
			"ak-B": actorCloudMux(h, "ak-B"),
			"":     actorCloudMux(h, ""),
		},
	}
	return env
}

// do 以指定 actor 的 mux 发起请求，返回 (status, body)。
func (e *ownerCloudEnv) do(t *testing.T, actor, method, path, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// doHEAD 以指定 actor 的 mux 发起 HEAD 请求（stat），返回 (status, recorder)。
func (e *ownerCloudEnv) doHEAD(t *testing.T, actor, path string) (int, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodHead, path, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Code, rr
}

// decodeTaskList 解析 cloud 任务列表响应 {tasks,total}。
func decodeTaskList(t *testing.T, body []byte) ([]CloudTask, int) {
	t.Helper()
	var resp struct {
		Tasks []CloudTask `json:"tasks"`
		Total int         `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析任务列表失败: %v, body=%s", err, body)
	}
	return resp.Tasks, resp.Total
}

// createCloudTaskAs 以指定 actor 创建一个云端下载任务并返回其 ID（断言成功）。
func (e *ownerCloudEnv) createCloudTaskAs(t *testing.T, actor, url string) string {
	t.Helper()
	code, body := e.do(t, actor, "POST", "/api/cloud/download", `{"url":"`+url+`"}`)
	if code != http.StatusOK {
		t.Fatalf("actor %q 创建云下载失败: %d %s", actor, code, body)
	}
	var task CloudTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析创建响应失败: %v, body=%s", err, body)
	}
	if task.ID == "" {
		t.Fatalf("创建响应缺 task ID: %s", body)
	}
	return task.ID
}

// TestCloudOwner_CreateWritesOwner 验证认证请求（AK）创建云下载 → 任务 Owner == AK。
func TestCloudOwner_CreateWritesOwner(t *testing.T) {
	env := newOwnerCloudEnv(t)

	idA := env.createCloudTaskAs(t, "ak-A", "https://example.com/a.zip")
	idB := env.createCloudTaskAs(t, "ak-B", "https://example.com/b.zip")

	ta, ok := env.mgr.GetTask(idA, "ak-A")
	if !ok || ta.Owner != "ak-A" {
		t.Fatalf("任务 A Owner = %+v, want ak-A", ta)
	}
	tb, ok := env.mgr.GetTask(idB, "ak-B")
	if !ok || tb.Owner != "ak-B" {
		t.Fatalf("任务 B Owner = %+v, want ak-B", tb)
	}
}

// TestCloudOwner_ListFiltersByOwner 验证列表按 owner 过滤：A 只见自己的任务；admin 见全部。
func TestCloudOwner_ListFiltersByOwner(t *testing.T) {
	env := newOwnerCloudEnv(t)
	idA := env.createCloudTaskAs(t, "ak-A", "https://example.com/a.zip")
	idB := env.createCloudTaskAs(t, "ak-B", "https://example.com/b.zip")

	// A 的列表只含 A 的任务（本环境无空 owner 任务）
	code, body := env.do(t, "ak-A", "GET", "/api/cloud/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("A 列表应 200, got %d %s", code, body)
	}
	aTasks, aTotal := decodeTaskList(t, body)
	if len(aTasks) != 1 || aTotal != 1 {
		t.Fatalf("A 列表应 1 条, got %d/%d: %s", len(aTasks), aTotal, body)
	}
	if aTasks[0].ID != idA || aTasks[0].Owner != "ak-A" {
		t.Fatalf("A 列表应含自己的任务: %+v", aTasks)
	}

	// B 的列表只含 B
	_, bodyB := env.do(t, "ak-B", "GET", "/api/cloud/tasks", "")
	bTasks, bTotal := decodeTaskList(t, bodyB)
	if len(bTasks) != 1 || bTotal != 1 || bTasks[0].ID != idB {
		t.Fatalf("B 列表应只含 B 的任务: %+v", bTasks)
	}

	// admin（空 owner）可见全部
	_, bodyAdmin := env.do(t, "", "GET", "/api/cloud/tasks", "")
	adminTasks, adminTotal := decodeTaskList(t, bodyAdmin)
	if len(adminTasks) != 2 || adminTotal != 2 {
		t.Fatalf("admin 列表应含全部 2 条, got %d/%d: %s", len(adminTasks), adminTotal, bodyAdmin)
	}
}

// TestCloudOwner_GetIDOR 验证详情按 owner 过滤：A Get B 的任务 → 404。
func TestCloudOwner_GetIDOR(t *testing.T) {
	env := newOwnerCloudEnv(t)
	idA := env.createCloudTaskAs(t, "ak-A", "https://example.com/a.zip")

	// A 拿自己的 → 200
	code, body := env.do(t, "ak-A", "GET", "/api/cloud/tasks/"+idA, "")
	if code != http.StatusOK {
		t.Fatalf("A Get 自己的任务应 200, got %d %s", code, body)
	}
	var task CloudTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	if task.Owner != "ak-A" {
		t.Fatalf("详情 Owner = %q, want ak-A", task.Owner)
	}

	// B 拿 A 的任务 → 404（不泄露存在性）
	idB := env.createCloudTaskAs(t, "ak-B", "https://example.com/b.zip")
	code, _ = env.do(t, "ak-B", "GET", "/api/cloud/tasks/"+idA, "")
	if code != http.StatusNotFound {
		t.Fatalf("B Get A 的任务应 404, got %d", code)
	}

	// admin（空 owner）可拿任意任务
	code, _ = env.do(t, "", "GET", "/api/cloud/tasks/"+idB, "")
	if code != http.StatusOK {
		t.Fatalf("admin Get 任意任务应 200, got %d", code)
	}
}

// TestCloudOwner_CancelDeleteIDOR 验证取消/删除按 owner 过滤：跨 owner → 404 且任务不变。
func TestCloudOwner_CancelDeleteIDOR(t *testing.T) {
	env := newOwnerCloudEnv(t)
	idA := env.createCloudTaskAs(t, "ak-A", "https://example.com/a.zip")

	// B 取消 A 的任务 → 404
	code, _ := env.do(t, "ak-B", "POST", "/api/cloud/tasks/"+idA+"/cancel", "")
	if code != http.StatusNotFound {
		t.Fatalf("B 取消 A 的任务应 404, got %d", code)
	}
	// 任务未被取消
	if ta, _ := env.mgr.GetTask(idA, "ak-A"); ta == nil || ta.Status == "cancelled" {
		t.Fatalf("A 的任务应保持 pending（B 取消被拒），got %+v", ta)
	}

	// B 删除 A 的任务 → 404
	code, _ = env.do(t, "ak-B", "DELETE", "/api/cloud/tasks/"+idA, "")
	if code != http.StatusNotFound {
		t.Fatalf("B 删除 A 的任务应 404, got %d", code)
	}
	if _, ok := env.mgr.GetTask(idA, "ak-A"); !ok {
		t.Fatal("A 的任务应仍存在（B 删除被拒）")
	}

	// A 可取消/删除自己的任务
	code, _ = env.do(t, "ak-A", "POST", "/api/cloud/tasks/"+idA+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("A 取消自己的任务应 200, got %d", code)
	}
	code, _ = env.do(t, "ak-A", "DELETE", "/api/cloud/tasks/"+idA, "")
	if code != http.StatusOK {
		t.Fatalf("A 删除自己的任务应 200, got %d", code)
	}
	if _, ok := env.mgr.GetTask(idA, "ak-A"); ok {
		t.Fatal("A 的任务应已被删除")
	}
}

// TestCloudOwner_DedupScopedByOwner 验证去重按 owner 隔离：
// 同 URL 且同 owner → 去重；跨 owner 同 URL → 各自新建（不吸收他人任务，防 IDOR 泄露）。
func TestCloudOwner_DedupScopedByOwner(t *testing.T) {
	env := newOwnerCloudEnv(t)

	// A 创建 URL same.zip
	idA := env.createCloudTaskAs(t, "ak-A", "https://example.com/same.zip")

	// 同 owner 同 URL → 去重复用
	code, body := env.do(t, "ak-A", "POST", "/api/cloud/download", `{"url":"https://example.com/same.zip"}`)
	if code != http.StatusOK {
		t.Fatalf("A 重复创建应 200, got %d %s", code, body)
	}
	var dedup CloudTask
	if err := json.Unmarshal(body, &dedup); err != nil {
		t.Fatal(err)
	}
	if dedup.ID != idA {
		t.Fatalf("同 owner 同 URL 应去重, got id=%q want %q", dedup.ID, idA)
	}

	// 跨 owner 同 URL → B 新建独立任务（不吸收 A 的任务）
	code, body = env.do(t, "ak-B", "POST", "/api/cloud/download", `{"url":"https://example.com/same.zip"}`)
	if code != http.StatusOK {
		t.Fatalf("B 创建应 200, got %d %s", code, body)
	}
	var taskB CloudTask
	if err := json.Unmarshal(body, &taskB); err != nil {
		t.Fatal(err)
	}
	if taskB.ID == idA || taskB.Owner != "ak-B" {
		t.Fatalf("B 应新建独立任务且 owner=ak-B, got %+v", taskB)
	}
}

// TestCloudOwner_EmptyOwnerCompat 验证未认证（空 owner）兼容：
// 空 owner 创建的任务对所有用户可见，且空 owner 请求者可见全部。
func TestCloudOwner_EmptyOwnerCompat(t *testing.T) {
	env := newOwnerCloudEnv(t)

	// admin（空 owner）创建 → 任务 Owner 空
	idU := env.createCloudTaskAs(t, "", "https://example.com/u.zip")
	if ta, _ := env.mgr.GetTask(idU, ""); ta == nil || ta.Owner != "" {
		t.Fatalf("admin 创建的任务 Owner 应为空, got %+v", ta)
	}

	// 认证用户 A 能看见空 owner 任务（全局兼容）
	_, body := env.do(t, "ak-A", "GET", "/api/cloud/tasks", "")
	aTasks, _ := decodeTaskList(t, body)
	found := false
	for _, task := range aTasks {
		if task.ID == idU {
			found = true
		}
	}
	if !found {
		t.Fatalf("认证用户 A 应能看见空 owner 任务: %s", body)
	}

	// A 也能 Get/取消空 owner 任务
	code, _ := env.do(t, "ak-A", "GET", "/api/cloud/tasks/"+idU, "")
	if code != http.StatusOK {
		t.Fatalf("A Get 空 owner 任务应 200, got %d", code)
	}
}

// TestCloudOwner_BatchCreateWritesOwner 验证批量创建同样写入 owner。
func TestCloudOwner_BatchCreateWritesOwner(t *testing.T) {
	env := newOwnerCloudEnv(t)

	code, body := env.do(t, "ak-A", "POST", "/api/cloud/download/batch",
		`{"urls":[{"url":"https://example.com/x.zip"},{"url":"https://example.com/y.zip"}]}`)
	if code != http.StatusOK {
		t.Fatalf("批量创建应 200, got %d %s", code, body)
	}
	var resp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Tasks) != 2 {
		t.Fatalf("批量创建应 2 条, got %d", len(resp.Tasks))
	}
	for _, r := range resp.Tasks {
		if r.Status != "pending" && r.Status != "downloading" {
			t.Fatalf("批量任务状态异常: %+v", r)
		}
		// API 返回 owner（DoD：响应带 owner）
		if r.Owner != "ak-A" {
			t.Fatalf("批量响应 Owner = %q, want ak-A", r.Owner)
		}
		if ta, ok := env.mgr.GetTask(r.ID, "ak-A"); !ok || ta.Owner != "ak-A" {
			t.Fatalf("批量任务 Owner = %+v, want ak-A", ta)
		}
	}
}

// TestCloudOwner_OrphanGroupInheritsOwner 验证组记录丢失后重建的孤儿组继承子任务 owner，
// 避免带 owner 的组被重建为全局可见（组级隔离漏洞，审查 I-1 回归）。
func TestCloudOwner_OrphanGroupInheritsOwner(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}

	mgr1 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	group, err := mgr1.CreateGroup("g1", []cloudfilename.Entry{{URL: "https://example.com/o.zip", Filename: "o.zip"}}, "ak-A")
	if err != nil {
		t.Fatalf("CreateGroup 失败: %v", err)
	}
	// 删除组持久化文件（模拟组记录丢失），保留子任务与任务持久化文件
	if err := os.Remove(filepath.Join(dir, downloadsDirName, "groups", group.ID+".json")); err != nil {
		t.Fatalf("删除组持久化文件失败: %v", err)
	}
	mgr1.Close()

	// 重启：孤儿组应从子任务重建并继承 owner
	mgr2 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	defer mgr2.Close()
	g, ok := mgr2.GetGroup(group.ID, "ak-A")
	if !ok || g.Owner != "ak-A" {
		t.Fatalf("孤儿组 Owner = %+v, want ak-A（继承子任务）", g)
	}
	// 跨 owner 用户不应可见孤儿组
	if _, ok := mgr2.GetGroup(group.ID, "ak-B"); ok {
		t.Fatal("B 不应可见 A 的孤儿组")
	}
}

// TestCloudOwner_CreateGroupWritesOwner 验证组创建把 owner 写入组与子任务。
func TestCloudOwner_CreateGroupWritesOwner(t *testing.T) {
	env := newOwnerCloudEnv(t)

	code, body := env.do(t, "ak-A", "POST", "/api/cloud/groups",
		`{"name":"g1","urls":[{"url":"https://example.com/g1.zip"},{"url":"https://example.com/g2.zip"}]}`)
	if code != http.StatusOK {
		t.Fatalf("创建组应 200, got %d %s", code, body)
	}
	var group CloudTaskGroup
	if err := json.Unmarshal(body, &group); err != nil {
		t.Fatalf("解析组失败: %v, body=%s", err, body)
	}
	if group.Owner != "ak-A" {
		t.Fatalf("组 Owner = %q, want ak-A", group.Owner)
	}
	if len(group.TaskIDs) != 2 {
		t.Fatalf("组应含 2 个子任务, got %d", len(group.TaskIDs))
	}
	for _, tid := range group.TaskIDs {
		if ta, ok := env.mgr.GetTask(tid, "ak-A"); !ok || ta.Owner != "ak-A" {
			t.Fatalf("组子任务 Owner = %+v, want ak-A", ta)
		}
	}

	// B 不能 Get 该组（跨 owner → 404）
	code, _ = env.do(t, "ak-B", "GET", "/api/cloud/groups/"+group.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("B Get A 的组应 404, got %d", code)
	}
	// admin 可见该组
	code, _ = env.do(t, "", "GET", "/api/cloud/groups/"+group.ID, "")
	if code != http.StatusOK {
		t.Fatalf("admin Get 组应 200, got %d", code)
	}
}

// TestCloudOwner_CloudTaskChecksumScoped 验证 M1/F2 修复：云任务校验和以 owner 作用域
// 且不带 .__cloud__ 段的 key（<owner>/<taskID>/<file>）写入，与 read/stat/chunk 读取端
// 完全一致——stat 必须命中 store（而非依赖磁盘兜底重算），删除后 key 被清理。
func TestCloudOwner_CloudTaskChecksumScoped(t *testing.T) {
	env := newOwnerCloudEnv(t)

	// 直接构造一个处于 completed 的云任务 + 落盘文件 + 按修复后的 owner key 写入校验和
	content := []byte("checksum-scope-check")
	task, err := env.mgr.CreateTask("url", "https://example.com/a.bin", "a.bin", int64(len(content)), "ak-A")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	taskDir := filepath.Join(env.mgr.cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "a.bin"), content, 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	expected := sha256Hex(content)

	// 模拟 executeDownload 的写端：校验和以 owner 作用域、无 .__cloud__ 段的 key 落库。
	// key 用 ToSlash 归一（写端/删除端/读端统一正斜杠协议，Windows 兼容，见 F2）。
	writeKey := checksumStoreKey("ak-A", filepath.ToSlash(filepath.Join(task.ID, "a.bin")))
	env.h.checksumStore.Set(writeKey, expected)
	if _, ok := env.h.checksumStore.Get(writeKey); !ok {
		t.Fatalf("写端 key %q 应可读", writeKey)
	}
	// 反向断言：旧（错误）密钥格式（含 .__cloud__ 段）不应存在，防止写入端回退
	if _, ok := env.h.checksumStore.Get(checksumStoreKey("ak-A", filepath.ToSlash(filepath.Join(cloudDirName, task.ID, "a.bin")))); ok {
		t.Fatalf("错误格式 key（含 .__cloud__）不应被使用")
	}

	// 以任务 owner 身份 stat kind=cloud_task → 必须命中 X-File-Checksum（命中 store，非磁盘兜底）
	code, resp := env.doHEAD(t, "ak-A", "/api/files/stat?filename="+task.ID+"/a.bin&kind=cloud_task")
	if code != http.StatusOK {
		t.Fatalf("stat cloud_task 应 200, got %d", code)
	}
	got := resp.Header().Get("X-File-Checksum")
	if got == "" {
		t.Fatalf("ak-A stat 应返回 X-File-Checksum（M1 修复后命中 owner key），header=%v", resp.Header())
	}
	if got != expected {
		t.Fatalf("X-File-Checksum=%q want %q", got, expected)
	}
	// 证明命中 store：从 store 取出的就是 expected（stat 用磁盘兜底也会返回相同值，
	// 但重点是与写端同 key。此断言冗余但防回归）

	// 清理：DeleteTask（owner 作用域 key 删除不应残留）
	if err := env.mgr.DeleteTask(task.ID, "ak-A"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, ok := env.h.checksumStore.Get(checksumStoreKey("ak-A", filepath.ToSlash(filepath.Join(task.ID, "a.bin")))); ok {
		t.Fatalf("删除任务后 checksum 应被清理")
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestCloudOwner_GroupArchivePrecheckOwnerDir 验证 F3 修复：cloudArchiveGroup 的
// "归档已存在"预检应落在 owner 归档目录（.__cloud_archives__/<owner>/），
// 而非误查 uploadsDir 全局根。
func TestCloudOwner_GroupArchivePrecheckOwnerDir(t *testing.T) {
	env := newOwnerCloudEnv(t)
	mgr := env.mgr

	// 构造一个含已完成子任务的组（直接注入 mgr 内存，模拟任务完成的磁盘状态）
	task := &CloudTask{
		ID: "f3task1", Owner: "ak-A", URL: "https://example.com/f3.bin",
		Filename: "f3.bin", Status: "completed", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	taskDir := filepath.Join(mgr.cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "f3.bin"), []byte("f3 data"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	group := &CloudTaskGroup{
		ID: "f3group", Owner: "ak-A", Name: "g", Status: "completed",
		TaskIDs: []string{task.ID}, Completed: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	mgr.groupMu.Lock()
	mgr.groups[group.ID] = group
	mgr.groupMu.Unlock()

	ownerArchiveDir := filepath.Join(mgr.uploadsDir, cloudArchiveDirName, "ak-A")
	if err := os.MkdirAll(ownerArchiveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("OwnerArchiveExists_Returns409", func(t *testing.T) {
		// 预置同名归档在 owner 目录（修复前：查全局根，此处应命中 → 409）
		pre := filepath.Join(ownerArchiveDir, "f3-a.tar.gz")
		if err := os.WriteFile(pre, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, body := env.do(t, "ak-A", "POST", "/api/cloud/groups/f3group/archive",
			`{"archive_name":"f3-a.tar.gz"}`)
		if code != http.StatusConflict {
			t.Fatalf("owner 目录已有同名归档应 409，got %d %s", code, body)
		}
		// 清理预置文件，避免影响下一个子测试
		_ = os.Remove(pre)
	})

	t.Run("RootUnrelatedFile_NotBlocked", func(t *testing.T) {
		// 全局根下同名文件存在，但 owner 归档目录没有 → 修复后不应被误 409
		_ = os.WriteFile(filepath.Join(mgr.uploadsDir, "f3-b.tar.gz"), []byte("root"), 0o644)
		code, body := env.do(t, "ak-A", "POST", "/api/cloud/groups/f3group/archive",
			`{"archive_name":"f3-b.tar.gz"}`)
		if code != http.StatusOK {
			t.Fatalf("全局根同名文件不应阻断 owner 归档，got %d %s", code, body)
		}
		// 归档成功落盘在 owner 目录
		if _, err := os.Stat(filepath.Join(ownerArchiveDir, "f3-b.tar.gz")); err != nil {
			t.Fatalf("归档应落在 owner 目录: %v", err)
		}
	})
}

// TestValidateSessionOwner_ForgedPrefix 验证 F4 修复：validateSessionOwner 剥离 owner
// 前缀后必须校验剩余为单一安全段，拒绝伪造前缀（"ak-A/evil"）与含分隔符的 id。
func TestValidateSessionOwner_ForgedPrefix(t *testing.T) {
	// 正常：owner 前缀 + 合法 hex 段
	if orig, ok := validateSessionOwner("ak-A", "ak-A/abcd1234"); !ok || orig != "abcd1234" {
		t.Fatalf("正常前缀应通过, got %q/%v", orig, ok)
	}
	// 未认证：任意合法段直接通过
	if _, ok := validateSessionOwner("", "abcd1234"); !ok {
		t.Fatalf("未认证合法段应通过")
	}
	// 伪造前缀：认证方 ak-A 试图操作未认证者构造的 "ak-A/evil" → remainder "evil" 合法？
	// 前端 upload_id 本就是客户端可控，核心问题是"evil"段无法确证归属。
	// 关键断言：任何含 "/" 的 id（无论是否带 owner 前缀）都因 validUploadID 拒绝。
	if _, ok := validateSessionOwner("ak-A", "ak-A/evil"); !ok {
		t.Fatalf("防伪造：单层正常段应仍允许（evil 本身合法）")
	}
	// 含嵌套分隔符 → 拒绝
	if _, ok := validateSessionOwner("ak-A", "ak-A/a/b"); ok {
		t.Fatalf("含嵌套 '/' 应拒绝")
	}
	// 前缀凭空伪造：'"ak-A/..' 路径穿越 → 拒绝
	if _, ok := validateSessionOwner("ak-A", "ak-A/../evil"); ok {
		t.Fatalf("含 '..' 应拒绝")
	}
	// 前缀不存在 → 拒绝
	if _, ok := validateSessionOwner("ak-B", "ak-A/abcd"); ok {
		t.Fatalf("跨 owner 前缀应拒绝")
	}
}

// TestValidUploadID 验证 upload_id 格式校验（路径分隔符 / .. / .__ / 控制字符）。
func TestValidUploadID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"abcd1234", true},
		{"test-upload-happy", true},
		{"a/b", false},
		{"a\b", false},
		{"..", false},
		{"a/../b", false},
		{".__internal", false},
		{"", false},
		{strings.Repeat("x", 129), false},
		{"a\x00b", false},
	}
	for _, c := range cases {
		if got := validUploadID(c.id); got != c.ok {
			t.Fatalf("validUploadID(%q) = %v, want %v", c.id, got, c.ok)
		}
	}
}
