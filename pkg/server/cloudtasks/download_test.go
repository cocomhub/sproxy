// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// 本文件锁云任务 CRUD 编排入口（P1-c S2a）的**包内契约**：成功路径的响应字节与
// owner 透传、错误分支的状态码、审计动作/结果。跨层的完整行为仍由 pkg/server 的
// 既有 HTTP 测试驱动（路由 → cloudTaskHandler → Service）。

// stubTasks 是 Tasks 的可控替身：只实现本文件用到的判定所需字段，其余保持零值。
type stubTasks struct {
	allowPrivate bool
	maxBatch     int
	tasks        map[string]*cloud.CloudTask
	listResult   []*cloud.CloudTask
	listTotal    int
	submitResult *cloud.CloudTask
	submitErr    error
	// submitHook 按 URL 决定逐条结果（批量「部分失败」场景）；非 nil 时优先于 submitErr。
	submitHook func(url string) (*cloud.CloudTask, error)
	cancelErr  error
	deleteErr  error
	resumeErr  error

	// 入参取证
	listOwner       string
	listStatus      string
	snapshotID      string
	submittedURLs   []string
	submittedParams []cloud.TaskParams
	submittedOwners []string
	submittedSizes  []int64
	submittedCtx    []context.Context
	cancelID        string
	cancelOwner     string
	deleteID        string
	deleteOwner     string
	resumeID        string
	resumeForce     bool
	resumeOwner     string
}

func (s *stubTasks) AllowPrivate() bool { return s.allowPrivate }

func (s *stubTasks) MaxBatchURLs() int { return s.maxBatch }

func (s *stubTasks) SubmitAndStart(_, url, _ string, totalSize int64, syncCtx context.Context, owner string, params cloud.TaskParams) (*cloud.CloudTask, error) {
	s.submittedURLs = append(s.submittedURLs, url)
	s.submittedParams = append(s.submittedParams, params)
	s.submittedOwners = append(s.submittedOwners, owner)
	s.submittedSizes = append(s.submittedSizes, totalSize)
	s.submittedCtx = append(s.submittedCtx, syncCtx)
	if s.submitHook != nil {
		return s.submitHook(url)
	}
	if s.submitErr != nil {
		return nil, s.submitErr
	}
	return s.submitResult, nil
}

func (s *stubTasks) SnapshotTask(id, _ string) (*cloud.CloudTask, bool) {
	s.snapshotID = id
	t, ok := s.tasks[id]
	return t, ok
}

func (s *stubTasks) SnapshotTasks(_ []string, _ string) []*cloud.CloudTask { return nil }

func (s *stubTasks) ListTasks(status string, _, _ int, owner string) ([]*cloud.CloudTask, int) {
	s.listStatus = status
	s.listOwner = owner
	return s.listResult, s.listTotal
}

func (s *stubTasks) CancelTask(id, owner string) error {
	s.cancelID, s.cancelOwner = id, owner
	return s.cancelErr
}

func (s *stubTasks) DeleteTask(id, owner string) error {
	s.deleteID, s.deleteOwner = id, owner
	return s.deleteErr
}

func (s *stubTasks) ResumeTask(id string, force bool, owner string) error {
	s.resumeID, s.resumeForce, s.resumeOwner = id, force, owner
	return s.resumeErr
}

// auditCall 是一次审计调用取证。
type auditCall struct {
	action, objectType, object, result, detail string
}

// stubAudit 记录审计调用（Audit 的唯一用途是记录，故只存参数）。
type stubAudit struct {
	calls []auditCall
}

func (a *stubAudit) Record(_ context.Context, action, objectType, object, result, detail string) {
	a.calls = append(a.calls, auditCall{action: action, objectType: objectType, object: object, result: result, detail: detail})
}

// stubVolumes 是 Volumes 的可控替身：ACL 判定 + 转存目标 FS/卷/本地池/物理足迹。
// 零值即「无任何卷装配、ACL 全拒、非本地卷」，与既有用例的最小形态一致。
type stubVolumes struct {
	allowed map[string]bool // key = owner + "\x00" + volume

	// 转存目标（nil ⇒ TransferFS 报未装配）。
	fs     syncpkg.FS
	scheme string
	vol    volume.Volume
	local  bool
	pool   *quota.Pool

	// footprint 是卷自报物理足迹（key = volume+"\x00"+owner+"\x00"+userRel）。
	footprint map[string]int64
	// footprintHook 覆盖 footprint 查表（写前/写后需不同值时由用例按调用序号返回）。
	footprintHook func() (int64, bool)
}

func (v stubVolumes) Allowed(owner, volume string) bool {
	return v.allowed[owner+"\x00"+volume]
}

func (v stubVolumes) TransferFS(string) (syncpkg.FS, string, volume.Volume, bool) {
	if v.fs == nil {
		return nil, "", volume.Volume{}, false
	}
	return v.fs, v.scheme, v.vol, true
}

func (v stubVolumes) IsLocal(string) bool { return v.local }

func (v stubVolumes) LocalPool(string) *quota.Pool { return v.pool }

func (v stubVolumes) PhysFootprint(volume, owner, userRel string) (int64, bool) {
	if v.footprintHook != nil {
		return v.footprintHook()
	}
	n, ok := v.footprint[volume+"\x00"+owner+"\x00"+userRel]
	return n, ok
}

// decodeBody 解析响应 JSON（失败即 t.Fatalf，避免各用例重复样板）。
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("响应不是合法 JSON（%v）：%s", err, rec.Body.String())
	}
}

// TestListTasks_PassesOwnerAndFillsTotal 锁列表语义：owner 透传给 Tasks、status 原样
// 下传、响应是 {tasks,total} 且 total 取自 Tasks（不受分页影响）。
func TestListTasks_PassesOwnerAndFillsTotal(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{
		listResult: []*cloud.CloudTask{{ID: "t1", Owner: "alice", Status: "completed"}},
		listTotal:  7,
	}
	svc := New(Deps{Tasks: tasks})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/tasks?status=completed&offset=2&limit=1", nil)

	svc.ListTasks(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if tasks.listOwner != "alice" || tasks.listStatus != "completed" {
		t.Fatalf("列表入参 = (owner=%q, status=%q)，owner/status 必须原样透传", tasks.listOwner, tasks.listStatus)
	}
	var body struct {
		Tasks []cloud.CloudTask `json:"tasks"`
		Total int               `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 7 || len(body.Tasks) != 1 || body.Tasks[0].ID != "t1" {
		t.Fatalf("响应 = %+v，want tasks=[t1] total=7", body)
	}
}

// TestGetTask_NotFoundMaps404 锁「不可见/不存在统一 404」的既有语义（不泄漏存在性）。
func TestGetTask_NotFoundMaps404(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Tasks: &stubTasks{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/tasks/missing", nil)
	req.SetPathValue("id", "missing")

	svc.GetTask(rec, req, "alice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] != "task not found" {
		t.Fatalf("error = %q, want %q（文案是既有 HTTP 契约）", body["error"], "task not found")
	}
}

// TestCancelTask_NotFoundMaps404AndAuditsFailure 锁取消路径：错误文案含 not found ⇒ 404
// （其它 ⇒ 400），且失败分支必须留审计（action/object/result/detail 逐字）。
func TestCancelTask_NotFoundMaps404AndAuditsFailure(t *testing.T) {
	t.Parallel()
	audit := &stubAudit{}
	svc := New(Deps{Tasks: &stubTasks{cancelErr: errors.New("task not found")}, Audit: audit})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/cancel", nil)
	req.SetPathValue("id", "t1")

	svc.CancelTask(rec, req, "alice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(audit.calls) != 1 {
		t.Fatalf("审计调用 %d 次，want 1", len(audit.calls))
	}
	got := audit.calls[0]
	want := auditCall{action: "cloud_cancel", objectType: "task", object: "t1", result: auditResultError, detail: "task not found"}
	if got != want {
		t.Fatalf("审计 = %+v，want %+v", got, want)
	}
}

// TestCancelTask_SuccessAuditsWithoutDetail 锁成功分支：200 + status=cancelled，审计
// result=success 且 detail 为空（既有契约）。
func TestCancelTask_SuccessAuditsWithoutDetail(t *testing.T) {
	t.Parallel()
	audit := &stubAudit{}
	svc := New(Deps{Tasks: &stubTasks{}, Audit: audit})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/cancel", nil)
	req.SetPathValue("id", "t1")

	svc.CancelTask(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["status"] != "cancelled" {
		t.Fatalf("status 字段 = %q, want cancelled", body["status"])
	}
	want := auditCall{action: "cloud_cancel", objectType: "task", object: "t1", result: auditResultSuccess}
	if len(audit.calls) != 1 || audit.calls[0] != want {
		t.Fatalf("审计 = %+v，want [%+v]", audit.calls, want)
	}
}

// TestDeleteTask_AnyErrorMaps404 锁删除路径的非直觉映射：任何错误都是 404（不是 400/500）。
func TestDeleteTask_AnyErrorMaps404(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Tasks: &stubTasks{deleteErr: errors.New("internal io failure")}, Audit: &stubAudit{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/cloud/tasks/t9", nil)
	req.SetPathValue("id", "t9")

	svc.DeleteTask(rec, req, "alice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（删除失败恒 404 是既有语义）", rec.Code)
	}
}

// TestCreateDownload_InvalidBodyMaps400 锁请求体错误分支（不再进入任何依赖调用）。
func TestCreateDownload_InvalidBodyMaps400(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{}
	svc := New(Deps{Tasks: tasks})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/download", strings.NewReader("{not json"))
	req.SetPathValue("id", "x")

	svc.CreateDownload(rec, req, "alice")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] != msgInvalidRequestBody {
		t.Fatalf("error = %q, want %q", body["error"], msgInvalidRequestBody)
	}
}

// TestCreateDownload_TransferACLRejectedMaps403 锁转存目标卷 ACL 拒绝的 403 与文案
// （单条/批量/组三条入口共用同一文案，漂移即回归）。
func TestCreateDownload_TransferACLRejectedMaps403(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Tasks: &stubTasks{}, Volumes: stubVolumes{allowed: map[string]bool{}}})
	rec := httptest.NewRecorder()
	body := `{"url":"https://example.com/file.zip","transfer":{"volume":"private-vault"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/download", strings.NewReader(body))

	svc.CreateDownload(rec, req, "alice")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var resp map[string]string
	decodeBody(t, rec, &resp)
	want := `转存目标卷 "private-vault" 对当前用户不可用（ACL 拒绝）`
	if resp["error"] != want {
		t.Fatalf("error = %q, want %q", resp["error"], want)
	}
}

// TestCreateDownload_SuccessReturnsSnapshot 锁成功路径：URL/文件名规范化 + spx_target
// 叠加后提交任务，响应是任务快照（200）。
func TestCreateDownload_SuccessReturnsSnapshot(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{submitResult: &cloud.CloudTask{ID: "t1", Owner: "alice"}, tasks: map[string]*cloud.CloudTask{"t1": {ID: "t1", Owner: "alice", Status: "pending"}}}
	svc := New(Deps{Tasks: tasks})
	rec := httptest.NewRecorder()
	body := `{"url":"https://example.com/file.zip","target_file":"EXAMPLE-001A.mp4"}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/download", strings.NewReader(body))

	svc.CreateDownload(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
	var task cloud.CloudTask
	decodeBody(t, rec, &task)
	if task.ID != "t1" {
		t.Fatalf("响应任务 = %+v，want 快照 t1", task)
	}
}

// TestWithShareTarget 验证 spx_target 参数叠加（已有 query 保留、同键覆盖）。
func TestWithShareTarget(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, target, want string }{
		{"https://example.com/s/EXAMPLE-001", "EXAMPLE-001A.mp4", "spx_target=EXAMPLE-001A.mp4"},
		{"https://example.com/s/EXAMPLE-001?x=1", "b.mp4", "spx_target=b.mp4"},
		{"https://example.com/magnet:?xt=urn:btih:ab", "m.mp4", "spx_target=m.mp4"},
	}
	for _, c := range cases {
		got := withShareTarget(c.in, c.target)
		if !strings.Contains(got, c.want) {
			t.Fatalf("withShareTarget(%q, %q) = %q, want contains %q", c.in, c.target, got, c.want)
		}
		if !strings.HasPrefix(got, strings.SplitN(c.in, "?", 2)[0]) {
			t.Fatalf("withShareTarget 改变了 URL 路径: %q -> %q", c.in, got)
		}
	}
}

// 编译期断言：测试替身覆盖被依赖的接口（防接口加方法后替身静默失效）。
var (
	_ Tasks   = (*stubTasks)(nil)
	_ Audit   = (*stubAudit)(nil)
	_ Volumes = stubVolumes{}
)
