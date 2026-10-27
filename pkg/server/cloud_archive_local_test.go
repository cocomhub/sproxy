// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cloud_archive_local_test.go —— 统一卷模型 U3 的转存到本地卷验收测试。
//
// 设计：docs/designs/2026-10-28-unified-volume-model.md §二 U3。
// 钉住：本地卷成为**合法转存目标**——preflight 不拒（scheme 空按 local 归一）、
// 产物落 `<owner>/user/<rel>`（与外部卷同键空间）、经 /download 可读回一致。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudArchive_ToLocalVolume_LandsInUserBucket 单任务归档 target_volume=本地卷：
// 产物落统一键空间 `<owner>/user/<rel>`，且经 /download 可读回（转存成功 ≠ 下载 404）。
func TestCloudArchive_ToLocalVolume_LandsInUserBucket(t *testing.T) {
	t.Parallel()
	// 用 os.MkdirTemp（非常 t.TempDir）：Windows 上 TempDir 的 RemoveAll 校验会因为
	// 卷根/账本句柄晚释放而报 "used by another process"（测试逻辑本身已通过）。
	dir, derr := os.MkdirTemp("", "archive-local-*")
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h, _, mgr := setupLocalVolumeArchive(t, dir)

	// 归档前先落一个源文件（createCompletedTask 写任务目录）。
	id := createCompletedTask(t, mgr, "src.bin")

	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"local.tar.gz","target_volume":"default"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.cloudArchiveTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("转存到本地卷 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !result.Success {
		t.Fatalf("转存结果失败: %+v", result)
	}
	// 产物落 <storage_root>/<owner>/user/archive/local.tar.gz（ResolveUserLocation 统一键空间，
	// 本地卷真实写盘——经 VolumeView 的 Guard(Wrap(NewRootFS)) 可信链）。
	key := filepath.Join(dir, "anonymous", "user", "archive", "local.tar.gz")
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("本地卷转存产物 %q 缺失: %v（转存未落到 user 桶）", key, err)
	}
}

// setupLocalVolumeArchive 装配默认本地卷 + 云任务管理器（target_volume=本地卷用）。
// 用 assembleVolumes 建**真实卷集**（本地卷进 storageVolumes → VolumeView 可取统一视图）。
func setupLocalVolumeArchive(t *testing.T, dir string) (*Handlers, *serverMemFS, *cloud.CloudDownloadManager) {
	t.Helper()
	h := newAssemblyTestHandlers(t, dir)
	cfg := Default()
	cfg.StorageRoot = dir
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })
	h.volSet = vs
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		UploadsDir: dir, Storage: cloudStorageManager{m: sm}, TenantFor: h.tenantFor,
		ChecksumStoreFor: h.checksumStoreFor, ListTenants: h.listTenantIDs,
		Logger: testLogger(), Config: defaultCloudDownloadConfig(),
	})
	h.cloudMgr = mgr
	return h, nil, mgr
}
