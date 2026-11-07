// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// download_group_crud_test.go 补齐云任务/云任务组 CRUD 入口（download.go / group.go）的
// 分支矩阵：批量创建的**逐条结果**（成功/校验失败/提交失败/快照丢失混合）、请求体与
// 分页边界、owner 透传、取消/删除/恢复的状态映射（404 vs 400 vs 其他）。
//
// 这些分支是客户端可编程接口（CLI 链式调用按单项 status/error 决定后续动作），故除状态码
// 外还锁定**逐条结果字段**（成功用规范化 URL、失败保留用户原始值或规范化值）。
package cloudtasks

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// jsonThenErrorBody 先吐出合法 JSON，再在后续 Read 上报错：模拟 SproxySig bodyValidator
// 在 EOF 哈希校验阶段失败（json.Decoder 只读到 JSON 结束即返回，故 drainBody 才触发）。
type jsonThenErrorBody struct {
	data   string
	served bool
}

func (b *jsonThenErrorBody) Read(p []byte) (int, error) {
	if b.served {
		return 0, errors.New("body hash mismatch")
	}
	b.served = true
	return copy(p, b.data), nil
}

func (b *jsonThenErrorBody) Close() error { return nil }

// TestCreateBatchDownload_PartialResults 锁批量创建的逐条结果语义：
// 校验失败项保留**用户原始** URL；提交失败/快照丢失项保留**规范化** URL；
// 成功项取快照的 ID/Owner/Status 且 URL 为规范化值。
func TestCreateBatchDownload_PartialResults(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{
		allowPrivate: true,
		maxBatch:     10,
		tasks:        map[string]*cloud.CloudTask{"t-ok": {ID: "t-ok", Owner: "alice", Status: "pending"}},
		submitHook: func(url string) (*cloud.CloudTask, error) {
			switch {
			case strings.Contains(url, "submit-fail"):
				return nil, errors.New("submit failed")
			case strings.Contains(url, "snapshot-lost"):
				return &cloud.CloudTask{ID: "gone"}, nil
			default:
				return &cloud.CloudTask{ID: "t-ok"}, nil
			}
		},
	}
	svc := New(Deps{Tasks: tasks})
	body := `{"urls":[
		{"url":"https://example.org/ok","filename":"ok.bin"},
		{"url":"file:///etc/passwd","filename":"bad.bin"},
		{"url":"https://example.org/submit-fail","filename":"sf.bin"},
		{"url":"https://example.org/snapshot-lost","filename":"sl.bin"}
	],"save":false,"download_local":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/download/batch", strings.NewReader(body))

	svc.CreateBatchDownload(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（部分失败不中断）: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Tasks []BatchTaskResult `json:"tasks"`
	}
	decodeBody(t, rec, &got)
	if len(got.Tasks) != 4 {
		t.Fatalf("逐条结果 = %d 项，want 4：%+v", len(got.Tasks), got.Tasks)
	}
	// 1) 校验失败：保留原始 URL/Filename（此时尚无规范化值）。
	if got.Tasks[1].Status != "failed" || got.Tasks[1].URL != "file:///etc/passwd" || got.Tasks[1].Filename != "bad.bin" {
		t.Fatalf("校验失败项 = %+v，期望保留用户原始 URL/Filename", got.Tasks[1])
	}
	if got.Tasks[1].Error == "" {
		t.Fatal("校验失败项必须带 error 文案")
	}
	// 2) 提交失败：已规范化 ⇒ 用规范化后的 URL。
	if got.Tasks[2].Status != "failed" || got.Tasks[2].Error != "submit failed" ||
		got.Tasks[2].URL != "https://example.org/submit-fail" {
		t.Fatalf("提交失败项 = %+v", got.Tasks[2])
	}
	// 3) 快照丢失 ⇒ task created but not found（不让客户端拿到半成品 ID）。
	if got.Tasks[3].Status != "failed" || got.Tasks[3].Error != "task created but not found" {
		t.Fatalf("快照丢失项 = %+v", got.Tasks[3])
	}
	// 4) 成功项取快照字段。
	if got.Tasks[0].ID != "t-ok" || got.Tasks[0].Owner != "alice" || got.Tasks[0].Status != "pending" {
		t.Fatalf("成功项 = %+v", got.Tasks[0])
	}
	// 批量参数逐条统一 apply（save=false 不得回落默认 true）。
	if len(tasks.submittedParams) != 3 {
		t.Fatalf("提交次数 = %d，want 3（校验失败项不提交）", len(tasks.submittedParams))
	}
	for _, p := range tasks.submittedParams {
		if p.Save || !p.DownloadLocal {
			t.Fatalf("批量参数 = %+v，期望 save=false download_local=true", p)
		}
		if p.Transfer != nil {
			t.Fatalf("未指定 transfer 时不得凭空构造：%+v", p.Transfer)
		}
	}
	// 批量恒异步：syncCtx 必须为 nil（客户端断连不得取消批量任务）。
	for i, ctx := range tasks.submittedCtx {
		if ctx != nil {
			t.Fatalf("第 %d 项 syncCtx 非 nil，批量必须异步", i)
		}
	}
	// owner 透传 + 大小未知（-1）。
	for i := range tasks.submittedOwners {
		if tasks.submittedOwners[i] != "alice" || tasks.submittedSizes[i] != -1 {
			t.Fatalf("第 %d 项入参 = (owner=%q, size=%d)", i, tasks.submittedOwners[i], tasks.submittedSizes[i])
		}
	}
}

// TestCreateBatchDownload_Rejections 批量入口的请求级拒绝：请求体非法（含 bodyValidator
// EOF 校验失败）、urls 缺失、超上限、ACL 拒绝。
func TestCreateBatchDownload_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		maxBatch int
		want     int
		wantMsg  string
	}{
		{name: "invalid json", body: "{", maxBatch: 10, want: http.StatusBadRequest, wantMsg: msgInvalidRequestBody},
		{name: "empty urls", body: `{"urls":[]}`, maxBatch: 10, want: http.StatusBadRequest, wantMsg: "urls is required"},
		{name: "too many urls", body: `{"urls":[{"url":"https://example.org/a"}]}`, maxBatch: 0, want: http.StatusBadRequest, wantMsg: "maximum 0 URLs per batch"},
		{
			name: "transfer acl rejected", body: `{"urls":[{"url":"https://example.org/a"}],"transfer":{"volume":"vault"}}`,
			maxBatch: 10, want: http.StatusForbidden,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := New(Deps{
				Tasks:   &stubTasks{allowPrivate: true, maxBatch: c.maxBatch},
				Volumes: &stubVolumes{},
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/download/batch", strings.NewReader(c.body))

			svc.CreateBatchDownload(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.wantMsg != "" {
				var body map[string]string
				decodeBody(t, rec, &body)
				if body["error"] != c.wantMsg {
					t.Fatalf("error = %q，want %q", body["error"], c.wantMsg)
				}
			}
		})
	}

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{allowPrivate: true, maxBatch: 10}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/download/batch", nil)
		req.Body = &jsonThenErrorBody{data: `{"urls":[{"url":"https://example.org/a"}]}`}

		svc.CreateBatchDownload(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		var body uploadResponse
		decodeBody(t, rec, &body)
		if body.Message != msgBadRequest {
			t.Fatalf("message = %q，want %q", body.Message, msgBadRequest)
		}
	})
}

// TestCreateDownload_ErrorMatrix 单条创建的提交面映射：存储不足 507、其他 400、
// 快照丢失 500，以及 bodyValidator EOF 校验失败 400。
func TestCreateDownload_ErrorMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		submit  error
		task    *cloud.CloudTask
		snap    map[string]*cloud.CloudTask
		want    int
		wantMsg string
	}{
		{
			name: "tenant quota full maps 507", submit: quota.ErrStorageFull,
			want: http.StatusInsufficientStorage,
		},
		{
			name: "global ledger full maps 507", submit: capacity.ErrStorageFull,
			want: http.StatusInsufficientStorage,
		},
		{
			name: "other error maps 400", submit: errors.New("bad url"),
			want: http.StatusBadRequest, wantMsg: "bad url",
		},
		{
			name: "snapshot missing maps 500", task: &cloud.CloudTask{ID: "t1"},
			want: http.StatusInternalServerError, wantMsg: "task created but not found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := &stubTasks{submitErr: c.submit, submitResult: c.task, tasks: c.snap}
			svc := New(Deps{Tasks: tasks})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/download",
				strings.NewReader(`{"url":"https://example.org/a","filename":"a.bin"}`))

			svc.CreateDownload(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.wantMsg != "" {
				var body map[string]string
				decodeBody(t, rec, &body)
				if body["error"] != c.wantMsg {
					t.Fatalf("error = %q，want %q", body["error"], c.wantMsg)
				}
			}
		})
	}

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/download", nil)
		req.Body = &jsonThenErrorBody{data: `{"url":"https://example.org/a"}`}

		svc.CreateDownload(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid url maps 400 with validator message", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/download",
			strings.NewReader(`{"url":"file:///etc/passwd"}`))

		svc.CreateDownload(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestCreateDownload_PassesTaskParams 单条创建的参数落点：target_file 叠加 spx_target、
// save 缺省 true、integrity/meta_extra/download_local 逐字下传、syncCtx 取请求 ctx。
func TestCreateDownload_PassesTaskParams(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{
		submitResult: &cloud.CloudTask{ID: "t1"},
		tasks:        map[string]*cloud.CloudTask{"t1": {ID: "t1", Owner: "alice"}},
	}
	svc := New(Deps{Tasks: tasks})
	rec := httptest.NewRecorder()
	body := `{"url":"https://example.org/s/E1","filename":"e.bin","target_file":"E1A.mp4",
		"download_local":true,"integrity_must_pass":true,"meta_extra":{"k":"v"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/download", strings.NewReader(body))

	svc.CreateDownload(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(tasks.submittedURLs) != 1 || !strings.Contains(tasks.submittedURLs[0], "spx_target=E1A.mp4") {
		t.Fatalf("提交 URL = %v，期望叠加 spx_target", tasks.submittedURLs)
	}
	p := tasks.submittedParams[0]
	if !p.Save || !p.DownloadLocal || !p.IntegrityMustPass || p.MetaExtra["k"] != "v" {
		t.Fatalf("任务参数 = %+v，期望 save 缺省 true 且其余逐字透传", p)
	}
	if tasks.submittedCtx[0] == nil {
		t.Fatal("单条创建必须携带请求 ctx（客户端断连即放弃）")
	}
}

// TestGetTask_Success 详情成功路径回任务对象本身（不是包装容器）。
func TestGetTask_Success(t *testing.T) {
	t.Parallel()
	svc := New(Deps{Tasks: &stubTasks{tasks: map[string]*cloud.CloudTask{"t1": {ID: "t1", Owner: "alice", Status: "completed"}}}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/tasks/t1", nil)
	req.SetPathValue("id", "t1")

	svc.GetTask(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got cloud.CloudTask
	decodeBody(t, rec, &got)
	if got.ID != "t1" || got.Status != "completed" {
		t.Fatalf("响应 = %+v", got)
	}
}

// TestDeleteTask_SuccessAudits 删除成功：200 + status=deleted + success 审计（object=任务 ID，
// 且不再额外记一条 cloud_group_delete —— 历史坑的回归锁）。
func TestDeleteTask_SuccessAudits(t *testing.T) {
	t.Parallel()
	audit := &stubAudit{}
	tasks := &stubTasks{}
	svc := New(Deps{Tasks: tasks, Audit: audit})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/cloud/tasks/t1", nil)
	req.SetPathValue("id", "t1")

	svc.DeleteTask(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if tasks.deleteID != "t1" || tasks.deleteOwner != "alice" {
		t.Fatalf("DeleteTask 入参 = (%q,%q)", tasks.deleteID, tasks.deleteOwner)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["status"] != "deleted" {
		t.Fatalf("status 字段 = %q", body["status"])
	}
	if len(audit.calls) != 1 {
		t.Fatalf("审计 = %+v，期望仅 1 条（不得再记 cloud_group_delete）", audit.calls)
	}
	if audit.calls[0].action != "cloud_delete" || audit.calls[0].objectType != "task" || audit.calls[0].result != auditResultSuccess {
		t.Fatalf("审计 = %+v", audit.calls[0])
	}
}

// TestResumeTask_StateMachine 恢复的状态机：成功 200 resumed、not found 404、其他 400；
// force 参数落点；请求体解析失败回落 false。
func TestResumeTask_StateMachine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		err       error
		want      int
		wantForce bool
	}{
		{name: "success with force", body: `{"force":true}`, want: http.StatusOK, wantForce: true},
		{name: "parse failure falls back to false", body: `{`, want: http.StatusOK},
		{name: "not found maps 404", body: `{}`, err: errors.New("task not found"), want: http.StatusNotFound},
		{name: "other error maps 400", body: `{}`, err: errors.New("not resumable"), want: http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tasks := &stubTasks{resumeErr: c.err}
			svc := New(Deps{Tasks: tasks})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/resume", strings.NewReader(c.body))
			req.SetPathValue("id", "t1")

			svc.ResumeTask(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if tasks.resumeID != "t1" || tasks.resumeOwner != "alice" || tasks.resumeForce != c.wantForce {
				t.Fatalf("ResumeTask 入参 = (%q, force=%t, owner=%q)，want (t1, %t, alice)",
					tasks.resumeID, tasks.resumeForce, tasks.resumeOwner, c.wantForce)
			}
		})
	}

	t.Run("success body", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/resume", strings.NewReader(`{}`))
		req.SetPathValue("id", "t1")

		svc.ResumeTask(rec, req, "alice")

		var body map[string]string
		decodeBody(t, rec, &body)
		if body["status"] != "resumed" {
			t.Fatalf("status 字段 = %q，want resumed", body["status"])
		}
	})

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/resume", nil)
		req.Body = &jsonThenErrorBody{data: `{"force":true}`}

		svc.ResumeTask(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestParseOffsetLimit 分页参数边界：缺失/非整数回落默认（offset=-1 不偏移、limit=0 全部），
// 合法值原样透传（含 0 与负数——由领域侧决定语义）。
func TestParseOffsetLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query                 string
		wantOffset, wantLimit int
	}{
		{"", -1, 0},
		{"?offset=abc&limit=xyz", -1, 0},
		{"?offset=3&limit=7", 3, 7},
		{"?offset=0&limit=0", 0, 0},
		{"?offset=-5&limit=-1", -5, -1},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/cloud/tasks"+c.query, nil)
		offset, limit := ParseOffsetLimit(req)
		if offset != c.wantOffset || limit != c.wantLimit {
			t.Errorf("ParseOffsetLimit(%q) = (%d,%d)，want (%d,%d)", c.query, offset, limit, c.wantOffset, c.wantLimit)
		}
	}
}

// TestListGroups_RefreshesAllBeforeFiltering 组列表必须先刷新**全部**可见组再按 status 过滤
// （否则刚转换到目标状态的组会被漏掉），且 owner/status 逐层透传、total 取过滤后总数。
func TestListGroups_RefreshesAllBeforeFiltering(t *testing.T) {
	t.Parallel()
	groups := &stubGroups{
		listAll:      []*cloud.CloudTaskGroup{{ID: "g1"}, {ID: "g2"}},
		listFiltered: []*cloud.CloudTaskGroup{{ID: "g2"}},
		listTotal:    9,
	}
	svc := New(Deps{Groups: groups})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cloud/groups?status=running&offset=1&limit=2", nil)

	svc.ListGroups(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(groups.updatedIDs) != 2 || groups.updatedIDs[0] != "g1" || groups.updatedIDs[1] != "g2" {
		t.Fatalf("刷新 = %v，期望先全量刷新 [g1 g2]", groups.updatedIDs)
	}
	if groups.listStatus != "running" || groups.listOwner != "alice" {
		t.Fatalf("过滤入参 = (status=%q, owner=%q)", groups.listStatus, groups.listOwner)
	}
	var body struct {
		Groups []cloud.CloudTaskGroup `json:"groups"`
		Total  int                    `json:"total"`
	}
	decodeBody(t, rec, &body)
	if body.Total != 9 || len(body.Groups) != 1 || body.Groups[0].ID != "g2" {
		t.Fatalf("响应 = %+v，期望 1 组 + total=9", body)
	}
}

// TestCancelGroup_StateMachine 组取消：成功 200 + status=cancelled；not found 404；其他 400。
func TestCancelGroup_StateMachine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: http.StatusOK},
		{name: "not found", err: errors.New("group not found"), want: http.StatusNotFound},
		{name: "other", err: errors.New("already completed"), want: http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := New(Deps{Groups: &stubGroups{cancelErr: c.err}})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/cancel", nil)
			req.SetPathValue("id", "g1")

			svc.CancelGroup(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusOK {
				var body map[string]string
				decodeBody(t, rec, &body)
				if body["status"] != "cancelled" {
					t.Fatalf("status 字段 = %q", body["status"])
				}
			}
		})
	}
}

// TestResumeGroup_StateMachine 组恢复：成功 200 resumed / not found 404 / 其他 400；
// force 透传；bodyValidator EOF 失败 400。
func TestResumeGroup_StateMachine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		err  error
		want int
	}{
		{name: "success", body: `{"force":true}`, want: http.StatusOK},
		{name: "parse failure falls back", body: `not-json`, want: http.StatusOK},
		{name: "not found", body: `{}`, err: errors.New("group not found"), want: http.StatusNotFound},
		{name: "other", body: `{}`, err: errors.New("no failed tasks"), want: http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := New(Deps{Groups: &stubGroups{resumeErr: c.err}})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/resume", strings.NewReader(c.body))
			req.SetPathValue("id", "g1")

			svc.ResumeGroup(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Groups: &stubGroups{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/resume", nil)
		req.Body = &jsonThenErrorBody{data: `{"force":true}`}

		svc.ResumeGroup(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestCreateGroup_Rejections 组创建的请求级拒绝与创建后快照丢失。
func TestCreateGroup_Rejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		maxBatch int
		want     int
		wantMsg  string
	}{
		{name: "invalid json", body: "{", maxBatch: 10, want: http.StatusBadRequest, wantMsg: msgInvalidRequestBody},
		{name: "empty urls", body: `{"urls":[]}`, maxBatch: 10, want: http.StatusBadRequest, wantMsg: "urls is required"},
		{
			name: "too many urls", body: `{"urls":[{"url":"https://example.org/a"}]}`,
			maxBatch: 0, want: http.StatusBadRequest, wantMsg: "maximum 0 URLs per group",
		},
		{
			name: "invalid url", body: `{"urls":[{"url":"file:///etc/passwd"}]}`,
			maxBatch: 10, want: http.StatusBadRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc := New(Deps{
				Tasks:   &stubTasks{allowPrivate: true, maxBatch: c.maxBatch},
				Groups:  &stubGroups{},
				Volumes: &stubVolumes{},
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", strings.NewReader(c.body))

			svc.CreateGroup(rec, req, "alice")

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
			if c.wantMsg != "" {
				var body map[string]string
				decodeBody(t, rec, &body)
				if body["error"] != c.wantMsg {
					t.Fatalf("error = %q，want %q", body["error"], c.wantMsg)
				}
			}
		})
	}

	t.Run("body hash mismatch", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{Tasks: &stubTasks{maxBatch: 10}, Groups: &stubGroups{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", nil)
		req.Body = &jsonThenErrorBody{data: `{"urls":[{"url":"https://example.org/a"}]}`}

		svc.CreateGroup(rec, req, "alice")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("group not found after create", func(t *testing.T) {
		t.Parallel()
		groups := &stubGroups{submitResult: &cloud.CloudTaskGroup{ID: "g1"}}
		svc := New(Deps{Groups: groups, Tasks: &stubTasks{allowPrivate: true, maxBatch: 10}, Audit: &stubAudit{}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups",
			strings.NewReader(`{"urls":[{"url":"https://example.org/a"}]}`))

		svc.CreateGroup(rec, req, "alice")

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		var body map[string]string
		decodeBody(t, rec, &body)
		if body["error"] != "group created but not found" {
			t.Fatalf("error = %q", body["error"])
		}
	})

	t.Run("transfer acl rejected", func(t *testing.T) {
		t.Parallel()
		svc := New(Deps{
			Tasks:   &stubTasks{allowPrivate: true, maxBatch: 10},
			Groups:  &stubGroups{},
			Volumes: &stubVolumes{},
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups",
			strings.NewReader(`{"urls":[{"url":"https://example.org/a"}],"transfer":{"volume":"vault"}}`))

		svc.CreateGroup(rec, req, "alice")

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

// TestTransferACLRejection_NilTransferAllows 无转存目标恒放行（nil = 不转存，不做 ACL 判定）。
func TestTransferACLRejection_NilTransferAllows(t *testing.T) {
	t.Parallel()
	if msg := TransferACLRejection(&stubVolumes{}, "alice", nil); msg != "" {
		t.Fatalf("nil transfer 应放行，got %q", msg)
	}
}

// TestSaveOrDefault 缺省 true、显式 false 不回落（两处入口共用同一事实源）。
func TestSaveOrDefault(t *testing.T) {
	t.Parallel()
	f := false
	if !SaveOrDefault(nil) {
		t.Fatal("nil 应默认 true")
	}
	if SaveOrDefault(&f) {
		t.Fatal("显式 false 不得回落 true")
	}
}

// TestValidateCloudDownloadURL_NormalizesAndRejects 校验/清洗规则（三条入口共用）：
// 文件名解析、scheme 拒绝、路径穿越拒绝；allowPrivate 影响 host 校验。
func TestValidateCloudDownloadURL_NormalizesAndRejects(t *testing.T) {
	t.Parallel()
	cleaned, fn, err := ValidateCloudDownloadURL("https://example.org/a/b.bin", "", true)
	if err != nil {
		t.Fatalf("合法 URL 被拒：%v", err)
	}
	if cleaned != "https://example.org/a/b.bin" || fn != "b.bin" {
		t.Fatalf("清洗结果 = (%q,%q)", cleaned, fn)
	}
	if _, _, err := ValidateCloudDownloadURL("https://example.org/a", "../passwd", true); err == nil {
		t.Fatal("路径穿越文件名应被拒")
	}
	if _, _, err := ValidateCloudDownloadURL("ftp://example.org/a", "a", true); err == nil {
		t.Fatal("非 http(s) scheme 应被拒")
	}
}

// 编译期锚点：本文件引用的接口/替身仍满足契约。
var (
	_ = cloudfilename.Entry{}
	_ context.Context
)
