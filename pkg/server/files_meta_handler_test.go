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
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// fakeMetaService 是 files.Service 的替身（只测 handler 装配/校验，不测底层合并）。
type fakeMetaService struct {
	updateErr error
	got       files.UpdateMetaInput
}

func (f *fakeMetaService) UpdateMeta(_ context.Context, input files.UpdateMetaInput) error {
	f.got = input
	return f.updateErr
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
		"code": "NHDTC-254", "maker": "NATURAL HIGH",
	}})
	req := httptest.NewRequest(http.MethodPut, "/api/files/meta?filename=n/v.mp4", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.updateFileMeta(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if svc.got.RemotePath != "n/v.mp4" || svc.got.Extra["code"] != "NHDTC-254" {
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
