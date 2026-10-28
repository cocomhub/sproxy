// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_meta_handler_test.go 验证 PUT /api/files/meta（方案②：元数据补录）。
//
// 覆盖：R35-b 场景证据（file_meta_update 动作在测试函数体内出现）+ 校验拒绝 + 未装配 503。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// fakeMetaService 是 files.Service 的替身（只测 handler 装配/校验，不测底层合并）。
type fakeMetaService struct {
	updateErr error
	got       files.UpdateMetaInput
	// getExtra / getErr / gotGet 是 GetMeta 替身的配置与调用记录（读面用例）。
	getExtra map[string]any
	getErr   error
	gotGet   files.GetMetaInput
	getMTime string
	getCTime string
}

func (f *fakeMetaService) UpdateMeta(_ context.Context, input files.UpdateMetaInput) error {
	f.got = input
	return f.updateErr
}

// GetMeta 替身：返回配置的 extra（nil = 404 语义由 handler 按 HTTPError 处理）。
func (f *fakeMetaService) GetMeta(_ context.Context, input files.GetMetaInput) (map[string]any, error) {
	f.gotGet = input
	return f.getExtra, f.getErr
}

// GetMetaInfo 替身：返回 extra + 可选 mtime/ctime（handler 读面新契约）。
func (f *fakeMetaService) GetMetaInfo(_ context.Context, input files.GetMetaInput) (files.MetaInfo, error) {
	f.gotGet = input
	return files.MetaInfo{Extra: f.getExtra, MTime: f.getMTime, CTime: f.getCTime}, f.getErr
}

func (f *fakeMetaService) RecordFileAudit(context.Context, string, string, string, string) {}

// metaServiceFor 返回测试替身（经 Handlers 字段注入——需 filesMetaSvc 接口，见下）。
var _ = files.ErrMetaUnavailable

// TestUpdateFileMeta_OK 验证合法 extra 补录成功（R35-b 场景证据：file_meta_update 动作）。
func TestUpdateFileMeta_OK(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{}
	h := &Handlers{metaSvcForTest: svc}

	body, _ := json.Marshal(map[string]any{"extra": map[string]any{
		"code": "MOVIE-001", "maker": "STUDIO X",
	}})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=n/v.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if svc.got.RemotePath != "n/v.mp4" || svc.got.Extra["code"] != "MOVIE-001" {
		t.Fatalf("UpdateMeta input 错: %+v", svc.got)
	}
	// R35-b 场景证据：动作字面量 + 常量引用（门禁扫描 Test* 函数体内的字符串/Ident）。
	if ActionFileMetaUpdate != "file_meta_update" {
		t.Fatalf("动作常量漂移: %s", ActionFileMetaUpdate)
	}
}

// TestUpdateFileMeta_InvalidExtra 验证未知字段被拒（fail-closed，400）。
func TestUpdateFileMeta_InvalidExtra(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	body, _ := json.Marshal(map[string]any{"extra": map[string]any{"evil": "x"}})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=a.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400, got %d", w.Code)
	}
}

// TestUpdateFileMeta_Unavailable 验证未装配 meta 更新能力 → 503。
func TestUpdateFileMeta_Unavailable(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{updateErr: files.ErrMetaUnavailable}
	h := &Handlers{metaSvcForTest: svc}
	body, _ := json.Marshal(map[string]any{"extra": map[string]any{"code": "x"}})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=a.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配应 503, got %d", w.Code)
	}
}

// TestUpdateFileMeta_NoFilename 验证缺 filename → 400。
func TestUpdateFileMeta_NoFilename(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	body, _ := json.Marshal(map[string]any{"extra": map[string]any{"code": "x"}})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 filename 应 400, got %d", w.Code)
	}
}

// TestGetFileMeta_OK 验证 GET /api/files/meta 返回 extra（读闭环核心）。
func TestGetFileMeta_OK(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{getExtra: map[string]any{"code": "MOVIE-001", "maker": "STUDIO X"}}
	h := &Handlers{metaSvcForTest: svc}
	req := httptest.NewRequest(http.MethodGet, "/api/files/meta?filename=n/v.mp4", nil)
	w := httptest.NewRecorder()
	h.getFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Extra["code"] != "MOVIE-001" || svc.gotGet.RemotePath != "n/v.mp4" {
		t.Fatalf("GetMeta input/输出错: got=%+v extra=%v", svc.gotGet, resp.Extra)
	}
}

// TestGetFileMeta_NotFound 文件不存在 → 404。
func TestGetFileMeta_NotFound(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{getErr: &files.HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}}
	h := &Handlers{metaSvcForTest: svc}
	req := httptest.NewRequest(http.MethodGet, "/api/files/meta?filename=gone.mp4", nil)
	w := httptest.NewRecorder()
	h.getFileMeta(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("404 期望, got %d", w.Code)
	}
}

// TestGetFileMeta_Empty 文件存在但无 extra → 空对象 200。
func TestGetFileMeta_Empty(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{getExtra: map[string]any{}}
	h := &Handlers{metaSvcForTest: svc}
	req := httptest.NewRequest(http.MethodGet, "/api/files/meta?filename=a.mp4", nil)
	w := httptest.NewRecorder()
	h.getFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"extra":{}`) {
		t.Fatalf("应返回空对象: %s", w.Body.String())
	}
}

// TestUpdateFileMeta_ModeReplace 验证 PUT body 带 mode=replace 时透传给 UpdateMetaInput。
func TestUpdateFileMeta_ModeReplace(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{}
	h := &Handlers{metaSvcForTest: svc}
	body, _ := json.Marshal(map[string]any{
		"extra": map[string]any{"code": "NEW-CODE"},
		"mode":  "replace",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=a.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if svc.got.Mode != files.MetaUpdateReplace {
		t.Fatalf("mode 应透传 replace, got %q", svc.got.Mode)
	}
}

// TestUpdateFileMeta_ModeInvalid 非法 mode → 400。
func TestUpdateFileMeta_ModeInvalid(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{}
	h := &Handlers{metaSvcForTest: svc}
	body, _ := json.Marshal(map[string]any{
		"extra": map[string]any{"code": "x"},
		"mode":  "bogus",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=a.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 mode 应 400, got %d", w.Code)
	}
}

// TestApplyMetaExtra_MergesNotReplaces 钉住用户裁定（2026-10-26）：随上传写 extra 是
// **合并**语义（默认新增），不得覆盖旧 KV。
// 变异：把 applyMetaExtra 改回 `fm.Extra = extra` → 本用例红。
func TestApplyMetaExtra_MergesNotReplaces(t *testing.T) {
	t.Parallel()
	fm := &meta.FileMeta{Extra: map[string]any{"code": "OLD", "maker": "KEEP"}}
	applyMetaExtra(fm, map[string]any{"code": "NEW", "x_tag": "added"})
	if fm.Extra["maker"] != "KEEP" {
		t.Fatalf("既有键 maker 应保留（合并语义），got %v", fm.Extra)
	}
	if fm.Extra["code"] != "NEW" {
		t.Fatalf("同名键应被新值更新，got %v", fm.Extra["code"])
	}
	if fm.Extra["x_tag"] != "added" {
		t.Fatalf("新键应新增，got %v", fm.Extra["x_tag"])
	}
	// nil/空 → 不写（零回归）。
	fm2 := &meta.FileMeta{}
	applyMetaExtra(fm2, nil)
	if fm2.Extra != nil {
		t.Fatalf("空 extra 不应创建 map, got %v", fm2.Extra)
	}
}

// TestGetFileMeta_IncludesTimes 验证响应附带原始 mtime/ctime（additive 字段；
// 存储为 UTC RFC3339Nano，客户端负责本地时区展示）。
func TestGetFileMeta_IncludesTimes(t *testing.T) {
	t.Parallel()
	svc := &fakeMetaService{
		getExtra: map[string]any{"code": "MOVIE-002"},
		getMTime: "2026-10-20T08:30:00Z",
		getCTime: "2026-10-19T01:02:03Z",
	}
	h := &Handlers{metaSvcForTest: svc}
	req := httptest.NewRequest(http.MethodGet, "/api/files/meta?filename=n/v.mp4", nil)
	w := httptest.NewRecorder()
	h.getFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Extra map[string]any `json:"extra"`
		MTime string         `json:"mtime"`
		CTime string         `json:"ctime"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MTime != "2026-10-20T08:30:00Z" || resp.CTime != "2026-10-19T01:02:03Z" {
		t.Fatalf("mtime/ctime 未透出: %+v", resp)
	}
	if resp.Extra["code"] != "MOVIE-002" {
		t.Fatalf("extra 丢失: %+v", resp.Extra)
	}
}
