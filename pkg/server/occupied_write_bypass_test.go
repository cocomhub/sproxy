// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// occupied_write_bypass_test.go 验证「被封装卷占用的底层子目录只读」在两个**旁路写面**上的
// 闭环（评审 F1/F2/F5）：
//   - 统一写内核的显式卷分支（WriteTarget：S3 单对象 PUT / DAV / 版本恢复 / presign 直传登记）
//     ——此前只做 ACL + 双预留，不做占用判定；
//   - 统一转存（归档 → 外部卷，#805）：转存目标 fs 仅 staging 配额，无占用 guard。
//
// 两处都是**服务端自己执行写**且目标路径客户端可控 ⇒ 不接占用判定即可落进「只读」占用目录，
// 破坏封装卷不变量（与 volume_links.go 宣称的闭环矛盾）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// TestWriteTarget_ExplicitVolume_OccupiedSubdir_403 显式卷写入命中被封装的占用子目录 →
// 403 且**不落盘**（fail-closed）。覆盖 routes：S3 单对象 PUT / presign 登记 / DAV / 版本恢复
// 共用的 routeUploadForTarget。
func TestWriteTarget_ExplicitVolume_OccupiedSubdir_403(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	uvmux := userVolWrap(h, "alice")
	// 建封装卷 vault 占用 main/videos（UserVisibleRel 坐标 = videos）。
	rec := postUserVol(t, uvmux, map[string]any{
		"name": "vault", "type": userVolWrapTestType,
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d (body: %s)", rec.Code, rec.Body.String())
	}

	body := []byte("direct write should be blocked")
	_, werr := h.fileService().WriteTarget(context.Background(), files.WriteTargetInput{
		Owner: "alice", RemotePath: "videos/x.bin", Volume: "main",
		Verify: false, ClientSize: int64(len(body)),
	}, bytes.NewReader(body))
	he, ok := werr.(*files.HTTPError)
	if !ok {
		t.Fatalf("占用目录显式卷写入错误 = %v (%T), want *files.HTTPError", werr, werr)
	}
	if he.Status != http.StatusForbidden {
		t.Fatalf("状态 = %d, want 403 (msg=%s)", he.Status, he.Message)
	}
	if !strings.Contains(he.Message, "占用") {
		t.Fatalf("文案应说明占用只读: %s", he.Message)
	}
	// 真实副作用：磁盘上没有该文件（写前拒绝，不留钉子文件）。
	written := filepath.Join(h.cfgPtr.Load().StorageRoot, "alice", "user", "videos", "x.bin")
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Fatalf("占用目录内不应落盘 %s（stat err=%v）", written, err)
	}
	// 对照：非占用路径同一内核写成功（防「一刀切拒绝」）。
	okBody := []byte("allowed")
	if _, err := h.fileService().WriteTarget(context.Background(), files.WriteTargetInput{
		Owner: "alice", RemotePath: "photos/y.bin", Volume: "main",
		Verify: false, ClientSize: int64(len(okBody)),
	}, bytes.NewReader(okBody)); err != nil {
		t.Fatalf("非占用路径写入应成功: %v", err)
	}
}

// TestCloudArchiveToVolume_OccupiedSubdir_403 归档→外部卷转存命中占用子目录 → 403 且不写目标卷。
func TestCloudArchiveToVolume_OccupiedSubdir_403(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	// 登记「otherwrap 占用 vault/occ」——占用坐标是用户可见相对路径。
	if !h.registerWrapperLink("", "otherwrap", map[string]any{"target": "vault/occ"}) {
		t.Fatal("登记占用关联失败")
	}
	id := createCompletedTask(t, mgr, "single.bin")
	post := func(targetPath string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"archive_name": "s.tar.gz", "target_volume": "vault", "target_path": targetPath,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive", bytes.NewReader(body))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.cloudArchiveTask(rec, req)
		return rec
	}
	rec := post("occ/s.tar.gz")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("占用子目录转存 = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(fs.files) != 0 {
		t.Fatalf("403 后目标卷不应有任何产物: %v", fs.files)
	}
	// 对照：非占用路径转存成功（防「一刀切拒绝」）。
	if rec := post("free/s.tar.gz"); rec.Code != http.StatusOK {
		t.Fatalf("非占用路径转存 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
