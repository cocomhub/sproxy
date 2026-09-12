// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// response_drift_test.go 是**响应契约漂移守卫**：文件服务抽取后，`UploadResponse`、
// `anonymousOwner`/`normalizeOwner` 在 pkg/server 与 pkg/files 各有一份定义（pkg/files
// 不能反向导入装配层，故迁移期必须两份）。现有测试**一条都拦不住漂移**——而漂移的后果是
// 文件端点与其余端点（cloud/auth/share，共 489 处）对外响应契约静默分叉。
//
// 本文件是本包少数能同时看到两侧定义的地方，故在此逐字节比对。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// TestUploadResponse_NoDriftAgainstFilesDomain 断言两侧 UploadResponse：① 字段名 + json tag
// 集合逐项相同；② 三个样本 Marshal 后逐字节相同（success / failure / success+checksum）。
func TestUploadResponse_NoDriftAgainstFilesDomain(t *testing.T) {
	st, ft := reflect.TypeFor[UploadResponse](), reflect.TypeFor[files.UploadResponse]()
	if st.NumField() != ft.NumField() {
		t.Fatalf("字段数漂移：server=%d files=%d", st.NumField(), ft.NumField())
	}
	for i := range st.NumField() {
		sf, ff := st.Field(i), ft.Field(i)
		if sf.Name != ff.Name || sf.Type != ff.Type || sf.Tag != ff.Tag {
			t.Fatalf("第 %d 个字段漂移：server=%s(%s,%s) files=%s(%s,%s)",
				i, sf.Name, sf.Type, sf.Tag, ff.Name, ff.Type, ff.Tag)
		}
	}

	samples := []struct {
		name string
		s, f any
	}{
		{"success", UploadResponse{Success: true, Message: "目录已创建: a"}, files.UploadResponse{Success: true, Message: "目录已创建: a"}},
		{"failure", UploadResponse{Success: false, Message: "目录不存在"}, files.UploadResponse{Success: false, Message: "目录不存在"}},
		{"with-checksum", UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}, files.UploadResponse{Success: true, Message: "ok", Checksum: "abc123"}},
	}
	for _, s := range samples {
		sb, err := json.Marshal(s.s)
		if err != nil {
			t.Fatalf("%s: server marshal: %v", s.name, err)
		}
		fb, err := json.Marshal(s.f)
		if err != nil {
			t.Fatalf("%s: files marshal: %v", s.name, err)
		}
		if string(sb) != string(fb) {
			t.Fatalf("%s 响应体漂移：\nserver=%s\nfiles =%s", s.name, sb, fb)
		}
	}
}

// TestAnonymousOwner_NoDriftAgainstFilesDomain 断言两侧 owner 归一化产出同一 owner 段：
// 本侧直接比字面量，领域侧经**真实 handler 落盘路径**反查（空 actor = 未认证 → anonymous）。
// 任一侧改了 "anonymous" 或归一逻辑，本测试或 pkg/files 侧的 TestOwnerNormalization_Contract 变红。
func TestAnonymousOwner_NoDriftAgainstFilesDomain(t *testing.T) {
	if got := normalizeOwner(""); got != "anonymous" {
		t.Fatalf(`pkg/server.normalizeOwner("")=%q want "anonymous"`, got)
	}
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	req := httptest.NewRequest("POST", "/mkdir?dirname=drift", nil)
	rr := httptest.NewRecorder()
	actorDirsMux(h, "").ServeHTTP(rr, req) // 空 actor：模拟未认证
	if rr.Code != http.StatusOK {
		t.Fatalf("mkdir 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	want := filepath.Join(root, normalizeOwner(""), "user", "drift")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("领域包落盘 owner 段应与 pkg/server.normalizeOwner 一致（期望 %s）: %v", want, err)
	}
}
