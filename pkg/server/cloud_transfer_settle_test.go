// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cloud_transfer_settle_test.go 锁定**转存产物写侧结算 → 删除侧释放的对称性**（遗留②根治，端到端）：
// cloud 转存直写目标卷 FS（不经 pkg/files 路由写内核），故必须由转存侧把产物字节结算进 owner
// 配额账本（与删除侧 `releaseExternalDeleteQuota` 同一 Scope、同一测量口径 = filesPhysFootprinter），
// 否则删除该产物时会做「**未曾提交的释放**」——扣掉同账本其它对象的占用（少计 ⇒ 过度准入）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// registerSettleTransferProtocol 为夹具的嵌套封装卷**按 wrapper 类型**声明转存协议：
// `transferDone` 需 scheme 生成转存 URL（生产由各后端类型自行声明协议，如 secretdata ⋯）。
//
// 方案甲要点：嵌套封装卷的卷描述 Type 是 wrapper 类型（不是内部机制名 enclosure），故协议必须
// 按该类型反查可得——这正是本用例的回归点（把 Type 改回 enclosure ⇒ `SchemeOf` 无协议 ⇒
// 创建即拒 ⇒ 红）。工厂不参与构造（嵌套声明由 enclosure 路径装配），t.Cleanup 解绑防污染
// 共享注册表。
func registerSettleTransferProtocol(t *testing.T) {
	t.Helper()
	registry.RegisterBackend(nestedSecretType, func(_ context.Context, _ volume.Volume) (registry.ExternalBackend, error) {
		return nil, errString("settle-test: 类型仅用于声明协议")
	}, "settle-transfer")
	t.Cleanup(func() { registry.UnregisterBackendForTest(nestedSecretType) })
}

// startSettleSource 启动返回固定内容的源站（转存源）。
func startSettleSource(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSettleCloudManager 按生产同形接线构造 cloud 管理器（临时目录转存目标卷 + 结算钩子）。
func newSettleCloudManager(t *testing.T, h *Handlers, sm *capacity.StorageManager) *cloud.CloudDownloadManager {
	t.Helper()
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		UploadsDir:       h.volSet.Root("main").AbsPath(),
		Storage:          cloudStorageManager{m: sm},
		TenantFor:        h.tenantFor,
		ChecksumStoreFor: h.checksumStoreFor,
		ListTenants:      h.listTenantIDs,
		Logger:           testLogger(),
		Config:           defaultCloudDownloadConfig(),
		QuotaFor: []cloud.QuotaResolver{func(owner string) *quota.Scope {
			return h.quotaBucketFor(owner, "cloud") // 下载暂存桶（与结算账本不同桶，互不干扰）
		}},
		TransferFSFor: func(volumeName string) (syncpkg.FS, string) {
			fsys, scheme, _, ok := h.externalVolumeTransferFS(volumeName)
			if fsys == nil || !ok {
				return nil, ""
			}
			return fsys, scheme
		},
		VolumeFor: func(volumeName string) (volume.Volume, bool) {
			return h.volSet.ByName(volumeName)
		},
		TargetQuotaScopeFor: h.quotaScopeFor,
		TargetPhysChargeFor: func(volumeName, owner, remotePath string) (int64, bool) {
			return filesPhysFootprinter{h: h}.PhysFootprint(volumeName, owner, remotePath)
		},
	})
	t.Cleanup(mgr.Close)
	return mgr
}

// TestCloudTransferSettle_DeleteIsSymmetric 转存 → 删除的账本对称性：
//
//	① 既有占用（HTTP 上传，经路由 Commit 记账）为基准 base；
//	② 转存一份产物进外部卷 ⇒ owner 账本必须**增加**（否则删除即幻影释放）；
//	③ 经真实删除链路删该产物 ⇒ 释放量恰等于转存结算量 ⇒ 账本精确回到 base。
//
// 变异（去掉 transferOnce 的 settleTransferCharge 调用）⇒ 第 ② 步 after==base ⇒ 红；
// 若释放多于结算（口径不同源）⇒ 第 ③ 步回到 <base ⇒ 红。
func TestCloudTransferSettle_DeleteIsSymmetric(t *testing.T) {
	t.Parallel()
	registerSettleTransferProtocol(t)
	h := newNestedSecretWrapperHandlers(t, 0)
	setTestOwnerQuota(h, "alice", 1<<30)
	scope := h.quotaScopeFor("alice", "user/pre.bin")
	if scope == nil {
		t.Fatal("user 桶 Scope 未建立（配额未装配）")
	}
	mux := actorFileWriteMux(h, "alice")

	// ① 既有占用：普通上传（路由 Commit 入账）。
	existing := []byte(strings.Repeat("p", 64))
	if st, body := writeMuxUpload(t, mux, "pre.bin", "", existing); st != http.StatusOK {
		t.Fatalf("前置上传 = %d, want 200 (body: %s)", st, body)
	}
	base := scope.Usage()
	if base <= 0 {
		t.Fatalf("前置：既有占用应为 %d，got %d", len(existing), base)
	}

	// ② 转存一份产物进外部卷（vault：secretdata 封装 ⇒ 物理占用 > 逻辑，测量口径必须同源）。
	mgr := newSettleCloudManager(t, h, capacity.NewStorageManager(t.TempDir(), 8<<30, nil, testLogger()))
	payload := []byte(strings.Repeat("z", 128))
	srv := startSettleSource(t, payload)
	task, err := mgr.SubmitAndStart("GET", srv.URL, "xfer.bin", int64(len(payload)), t.Context(), "alice",
		cloud.TaskParams{Transfer: &cloud.TransferSpec{Volume: "vault", Path: "xfer.bin"}, Save: true})
	if err != nil {
		t.Fatalf("提交任务失败: %v", err)
	}
	waitTaskDone(t, mgr, task.ID)
	if snap, _ := mgr.SnapshotTask(task.ID, "alice"); snap.Status != "completed" {
		t.Fatalf("任务应 completed, got %q (%s)", snap.Status, snap.Error)
	}
	afterXfer := scope.Usage()
	if afterXfer <= base {
		t.Fatalf("转存后应把产物结算进 owner 账本（base=%d, after=%d）：未结算 ⇒ 删除即幻影释放",
			base, afterXfer)
	}

	// ③ 删除转存产物（真实外部卷删除链路）⇒ 释放 == 曾提交 ⇒ 账本精确回到 base。
	if st, body := writeMuxDelete(t, mux, "xfer.bin", "vault", sha256hex(payload)); st != http.StatusOK {
		t.Fatalf("删除转存产物 = %d, want 200 (body: %s)", st, body)
	}
	if got := scope.Usage(); got != base {
		t.Fatalf("删除后账本应精确回到既有占用 %d（释放与结算同源），got %d", base, got)
	}
}
