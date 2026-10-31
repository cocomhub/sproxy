// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// 本文件锁云任务组入口（P1-c S2b）的**包内契约**：owner 透传、刷新顺序、错误映射、
// 审计动作与归档清理接缝。跨层行为仍由 pkg/server 的既有 HTTP 测试驱动
// （路由 → cloudTaskHandler → Service）。

// stubGroups 是 Groups 的可控替身：记录入参取证，错误可逐方法注入。
type stubGroups struct {
	groups       map[string]*cloud.CloudTaskGroup
	listAll      []*cloud.CloudTaskGroup
	listFiltered []*cloud.CloudTaskGroup
	listTotal    int
	submitResult *cloud.CloudTaskGroup
	submitErr    error
	cancelErr    error
	deleteErr    error
	resumeErr    error
	archiveErr   error

	// 入参取证
	submittedName   string
	submittedURLs   []cloudfilename.Entry
	submittedOwner  string
	submittedParams cloud.TaskParams
	updatedIDs      []string
	archiveOwner    string
	archiveFile     string
	deletedID       string
	deletedOwner    string
	listOwner       string
	listStatus      string
}

func (s *stubGroups) SubmitAndStartGroup(name string, urls []cloudfilename.Entry, owner string, params cloud.TaskParams) (*cloud.CloudTaskGroup, error) {
	s.submittedName, s.submittedURLs, s.submittedOwner, s.submittedParams = name, urls, owner, params
	if s.submitErr != nil {
		return nil, s.submitErr
	}
	return s.submitResult, nil
}

func (s *stubGroups) GetGroup(id, _ string) (*cloud.CloudTaskGroup, bool) {
	g, ok := s.groups[id]
	return g, ok
}

func (s *stubGroups) ListGroups(status string, _, _ int, owner string) ([]*cloud.CloudTaskGroup, int) {
	s.listStatus, s.listOwner = status, owner
	if status == "" {
		return s.listAll, len(s.listAll)
	}
	return s.listFiltered, s.listTotal
}

func (s *stubGroups) CancelGroup(_, _ string) error { return s.cancelErr }

func (s *stubGroups) DeleteGroup(id, owner string) error {
	s.deletedID, s.deletedOwner = id, owner
	return s.deleteErr
}

func (s *stubGroups) ResumeGroup(_ string, _ bool, _ string) error { return s.resumeErr }

func (s *stubGroups) SetGroupArchiveFile(string, string) {}

func (s *stubGroups) UpdateGroupStatus(id string) { s.updatedIDs = append(s.updatedIDs, id) }

func (s *stubGroups) DeleteGroupArchive(owner, archiveFile string) error {
	s.archiveOwner, s.archiveFile = owner, archiveFile
	return s.archiveErr
}

// recordingGroupTasks 记录 SnapshotTasks 的子任务 ID 入参（组详情必须按组携带的子任务
// ID 取快照，且 owner 过滤由领域侧完成）。
type recordingGroupTasks struct {
	stubTasks
	snapshotIDs []string
}

func (r *recordingGroupTasks) SnapshotTasks(ids []string, _ string) []*cloud.CloudTask {
	r.snapshotIDs = append(r.snapshotIDs, ids...)
	return []*cloud.CloudTask{{ID: "g1-task", Owner: "alice"}}
}

// TestGetGroup_ChecksVisibilityBeforeRefresh 锁组详情的调用顺序：先 GetGroup 判可见性
// （跨 owner 立即 404，且**不**触发 UpdateGroupStatus 写副作用），再刷新并二次取快照，
// 最后带子任务返回。
func TestGetGroup_ChecksVisibilityBeforeRefresh(t *testing.T) {
	t.Parallel()

	t.Run("invisible group is 404 without refresh", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{}
		svc := New(Deps{Groups: groups})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/cloud/groups/g1", nil)
		req.SetPathValue("id", "g1")

		svc.GetGroup(rec, req, "alice")

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if len(groups.updatedIDs) != 0 {
			t.Fatalf("不可见组不得触发 UpdateGroupStatus，got %v", groups.updatedIDs)
		}
		var body map[string]string
		decodeBody(t, rec, &body)
		if body["error"] != msgGroupNotFound {
			t.Fatalf("error = %q, want %q", body["error"], msgGroupNotFound)
		}
	})

	t.Run("visible group is refreshed then returned with tasks", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{groups: map[string]*cloud.CloudTaskGroup{
			"g1": {ID: "g1", Owner: "alice", TaskIDs: []string{"g1-task"}},
		}}
		tasks := &recordingGroupTasks{allowPrivate: true}
		svc := New(Deps{Groups: groups, Tasks: tasks})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/cloud/groups/g1", nil)
		req.SetPathValue("id", "g1")

		svc.GetGroup(rec, req, "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(groups.updatedIDs) != 1 || groups.updatedIDs[0] != "g1" {
			t.Fatalf("刷新顺序错误：updated = %v, want [g1]", groups.updatedIDs)
		}
		if len(tasks.snapshotIDs) != 1 || tasks.snapshotIDs[0] != "g1-task" {
			t.Fatalf("SnapshotTasks 入参 = %v，want [g1-task]", tasks.snapshotIDs)
		}
		var body struct {
			Group *cloud.CloudTaskGroup `json:"group"`
			Tasks []cloud.CloudTask     `json:"tasks"`
		}
		decodeBody(t, rec, &body)
		if body.Group == nil || body.Group.ID != "g1" {
			t.Fatalf("group = %+v, want id g1", body.Group)
		}
		if len(body.Tasks) != 1 || body.Tasks[0].ID != "g1-task" {
			t.Fatalf("tasks = %+v，必须来自 SnapshotTasks", body.Tasks)
		}
	})
}

// TestDeleteGroup_AuditsAndCleansArchive 锁组删除的审计与归档清理接缝：
// 失败恒按 "not found" → 404 且记 error 审计；成功记 success 审计并**按 owner**
// 清理组归档；归档清理失败不改变组删除结果（仍 200）。
func TestDeleteGroup_AuditsAndCleansArchive(t *testing.T) {
	t.Parallel()

	t.Run("delete failure maps 404 and audits error", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{deleteErr: errors.New("group not found")}
		audit := &stubAudit{}
		svc := New(Deps{Groups: groups, Audit: audit})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/cloud/groups/g1", nil)
		req.SetPathValue("id", "g1")

		svc.DeleteGroup(rec, req, "alice")

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if len(audit.calls) != 1 || audit.calls[0].action != "cloud_group_delete" || audit.calls[0].result != auditResultError {
			t.Fatalf("审计 = %+v，want 1 条 cloud_group_delete/error", audit.calls)
		}
		if groups.archiveFile != "" {
			t.Fatalf("删除失败不得清理归档，got %q", groups.archiveFile)
		}
	})

	t.Run("success audits and cleans archive by owner", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{groups: map[string]*cloud.CloudTaskGroup{
			"g1": {ID: "g1", Owner: "alice", ArchiveFile: "g1-1.tar.gz"},
		}}
		audit := &stubAudit{}
		svc := New(Deps{Groups: groups, Audit: audit})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/cloud/groups/g1", nil)
		req.SetPathValue("id", "g1")

		svc.DeleteGroup(rec, req, "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if groups.deletedID != "g1" || groups.deletedOwner != "alice" {
			t.Fatalf("DeleteGroup 入参 = (%q,%q)，owner 必须透传", groups.deletedID, groups.deletedOwner)
		}
		if groups.archiveOwner != "alice" || groups.archiveFile != "g1-1.tar.gz" {
			t.Fatalf("归档清理入参 = (%q,%q)，want (alice,g1-1.tar.gz)", groups.archiveOwner, groups.archiveFile)
		}
		if len(audit.calls) != 1 || audit.calls[0].result != auditResultSuccess {
			t.Fatalf("审计 = %+v，want 1 条 success", audit.calls)
		}
		var body map[string]string
		decodeBody(t, rec, &body)
		if body["status"] != "deleted" {
			t.Fatalf("status 字段 = %q, want deleted", body["status"])
		}
	})

	t.Run("archive cleanup failure keeps group delete result", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{
			groups:     map[string]*cloud.CloudTaskGroup{"g1": {ID: "g1", Owner: "alice", ArchiveFile: "g1-1.tar.gz"}},
			archiveErr: errors.New("卷未装配"),
		}
		svc := New(Deps{Groups: groups, Audit: &stubAudit{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/cloud/groups/g1", nil)
		req.SetPathValue("id", "g1")

		svc.DeleteGroup(rec, req, "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("归档清理失败不得改变组删除结果，status = %d", rec.Code)
		}
	})
}

// TestCreateGroup_MapsErrorsAndPassesNormalizedURLs 锁组创建：规范化后的 URL/Filename
// 与 owner/三参逐字下传，错误按既有字符串契约映射（冲突 409 / 语义空洞 400 / 未知 500）。
func TestCreateGroup_MapsErrorsAndPassesNormalizedURLs(t *testing.T) {
	t.Parallel()

	t.Run("normalizes urls and passes owner and params", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{submitResult: &cloud.CloudTaskGroup{ID: "g1", Owner: "alice"}}
		groups.groups = map[string]*cloud.CloudTaskGroup{"g1": {ID: "g1", Owner: "alice"}}
		audit := &stubAudit{}
		svc := New(Deps{Groups: groups, Tasks: &stubTasks{allowPrivate: true, maxBatch: 10}, Audit: audit})
		body := `{"name":"n","urls":[{"url":"https://example.org/a","filename":"a.bin"}],"save":false}`
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", strings.NewReader(body))
		rec := httptest.NewRecorder()

		svc.CreateGroup(rec, req, "alice")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if groups.submittedOwner != "alice" {
			t.Fatalf("owner = %q，必须透传", groups.submittedOwner)
		}
		if len(groups.submittedURLs) != 1 || groups.submittedURLs[0].Filename != "a.bin" {
			t.Fatalf("规范化入参 = %+v", groups.submittedURLs)
		}
		if groups.submittedParams.Save {
			t.Fatal("save=false 必须下传（不得回落默认 true）")
		}
		if len(audit.calls) != 1 || audit.calls[0].action != "cloud_group_create" || audit.calls[0].detail != "urls=1" {
			t.Fatalf("审计 = %+v，want cloud_group_create urls=1", audit.calls)
		}
	})

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"filename conflict", errors.New("filename conflict: a"), http.StatusConflict},
		{"duplicate url", errors.New("duplicate URL: https://example.org/a"), http.StatusConflict},
		{"语义空洞", errors.New("语义空洞：无任何产出"), http.StatusBadRequest},
		{"未装配", errors.New("转存目标卷未装配"), http.StatusBadRequest},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			groups := &stubGroups{submitErr: tc.err}
			svc := New(Deps{Groups: groups, Tasks: &stubTasks{allowPrivate: true, maxBatch: 10}})
			body := `{"urls":[{"url":"https://example.org/a","filename":"a.bin"}]}`
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", strings.NewReader(body))
			rec := httptest.NewRecorder()

			svc.CreateGroup(rec, req, "alice")

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d（error=%v）", rec.Code, tc.want, tc.err)
			}
		})
	}
}

// 编译期锚点：替身覆盖 Groups 全方法（新增方法未实现时此处先红，避免测试假绿）。
var _ Groups = (*stubGroups)(nil)
