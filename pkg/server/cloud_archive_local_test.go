// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cloud_archive_local_test.go —— 统一卷模型 U3 的转存到本地卷验收测试。
//
// 设计：docs/designs/2026-10-28-unified-volume-model.md §二 U3。
// 钉住：本地卷成为**合法转存目标**——preflight 不拒（scheme 空按 local 归一）、
// 产物落 `<owner>/user/<rel>`（与外部卷同键空间）、经 /download 可读回一致。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestCloudArchive_ToLocalVolume_LandsInUserBucket 单任务归档 target_volume=本地卷：
// 产物落统一键空间 `<owner>/user/<rel>`，且经 /download 可读回（转存成功 ≠ 下载 404）。
func TestCloudArchive_ToLocalVolume_LandsInUserBucket(t *testing.T) {
	t.Parallel()
	// 用 os.MkdirTemp（非常 t.TempDir）：Windows 上 TempDir 的 RemoveAll 校验会因为
	// 卷根/账本句柄晚释放而报 "used by another process"（测试逻辑本身已通过）。
	dir := testutil.MkdirTemp(t, "archive-local-")
	h, _, mgr := setupLocalVolumeArchive(t, dir)

	// 归档前先落一个源文件（createCompletedTask 写任务目录）。
	id := createCompletedTask(t, mgr, "src.bin")

	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"local.tar.gz","target_volume":"default"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
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
	// **读回一致**（Minor-4，独立复审）：经读路径同一装饰链（externalFSFor → VolumeView）
	// 取回产物，断言内容非空且为 gzip（与 /download 底层同路径，避免「转存成功但下载 404」）。
	rfs, rok := h.externalFSFor("default")
	if !rok || rfs == nil {
		t.Fatal("读路径应能解析默认卷视图（转存产物须可读回）")
	}
	rc, rerr := rfs.OpenRead(context.Background(), "anonymous/user/archive/local.tar.gz")
	if rerr != nil {
		t.Fatalf("读回转存产物失败（/download 会 404）: %v", rerr)
	}
	defer rc.Close()
	head := make([]byte, 2)
	if _, rerr := io.ReadFull(rc, head); rerr != nil {
		t.Fatalf("读回产物内容失败: %v", rerr)
	}
	if head[0] != 0x1f || head[1] != 0x8b {
		t.Fatalf("产物应为 gzip（头 %x %x）", head[0], head[1])
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

// TestCloudArchive_ToLocalVolume_ChargesVolumePool 钉住 reviewer Important-1（2026-10-28）：
// 归档转存到**本地卷**的产物必须计入该卷的**卷级容量池**（此前只有外部卷经 FS 层
// CapacityFS 记账，本地卷 rootfs 无 ReserveSpace ⇒ 卷池漏记 ⇒ 长期漂移）。
func TestCloudArchive_ToLocalVolume_ChargesVolumePool(t *testing.T) {
	dir := testutil.MkdirTemp(t, "archive-local-pool-")
	h, _, mgr := setupLocalVolumeArchive(t, dir)

	pool := h.volSet.Pool("default")
	if pool == nil {
		t.Fatal("默认卷池应存在")
	}
	before := pool.Usage()

	id := createCompletedTask(t, mgr, "src.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"pooled.tar.gz","target_volume":"default"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("转存 status=%d body=%s", rec.Code, rec.Body.String())
	}
	after := pool.Usage()
	if after <= before {
		t.Fatalf("转存到本地卷后卷池 Usage 应增长：before=%d after=%d（卷级记账漏接）", before, after)
	}

	// 删除产物 → 卷池同步释放（与写侧同源）。
	var result CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if err := h.deleteGroupArchive("", result.File); err != nil {
		t.Fatalf("删除归档: %v", err)
	}
	if got := pool.Usage(); got != before {
		t.Fatalf("删除后卷池 Usage 应回到 %d，got %d（释放漏接）", before, got)
	}
}
