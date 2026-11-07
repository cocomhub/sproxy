// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// prod_hardening_test.go 钉住 2026-10-21 对抗审查 P1 的装配层剩余面：删除组归档产物必须释放
// owner 配额。tar 打包助手的 checksum/收尾契约随实现迁入 cloudtasks（P1-c S4），
// 对应回归由 pkg/server/cloudtasks/archive_tar_test.go 继续钉住。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestDeleteGroupArchive_ReleasesOwnerQuota 组归档转存外部卷的产物被删除时必须**释放** owner
// 配额（写侧 settleArchiveCharge 已计入；删除侧此前只删对象 ⇒ 配额虚高、可能误 507）。
func TestDeleteGroupArchive_ReleasesOwnerQuota(t *testing.T) {
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	// 归档所需配额池（写侧结算 + 读侧 scope 同源）。
	h.globalPool = quota.NewPool(1 << 30)

	id := createCompletedTask(t, mgr, "g.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"g.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("归档 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if result.File == "" {
		t.Fatalf("响应应含归档 URL: %s", rec.Body.String())
	}

	scopeRel := "user/archive/g.tar.gz"
	scope := h.quotaScopeFor("anonymous", scopeRel)
	if scope == nil {
		t.Fatal("前置：owner 配额 Scope 未装配")
	}
	before := scope.Usage()
	if before <= 0 {
		t.Fatalf("前置：归档转存应已计入 owner 配额, got %d", before)
	}

	if err := h.deleteGroupArchive("anonymous", result.File); err != nil {
		t.Fatalf("删除组归档: %v", err)
	}
	key := "anonymous/user/archive/g.tar.gz"
	if _, ok := fs.files[key]; ok {
		t.Fatalf("对象应已删除（key=%s）", key)
	}
	if after := scope.Usage(); after >= before {
		t.Fatalf("删除归档产物后 owner 配额必须释放（before=%d after=%d；未接线 ⇒ 虚高窗口 ≤30min）",
			before, after)
	}
}
