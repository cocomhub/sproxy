// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cloud_archive_quota_settle_test.go 钉住「归档转存必须结算 owner 配额，且与删除侧释放对称」
// （独立审核 P1，2026-10-18）。
//
// 缺陷：归档转存（cloudArchiveToVolume）写侧**不做 owner 配额结算**，而删除侧
// `releaseExternalDeleteQuota` 会从**同一 Scope** 释放 `physChargeFor` 实测字节 ⇒ 那次释放是
// 「**未曾提交的释放**」（幻影释放）⇒ 扣掉同账本其它对象的占用 ⇒ 少计 ⇒ 后续写入沿父链检查
// 放行 ⇒ **可超出 owner 配额**（可重复放大）。
//
// cloud 下载转存早已有同款结算（`pkg/cloud/transfer.go` 的 `settleTransferCharge`，其注释亦
// 明写该因果）；本用例锁定归档路径的结算语义与**对称性**。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestSettleArchiveCharge 结算语义：新对象计入、覆盖写按差分、无变化/nil scope 无操作。
func TestSettleArchiveCharge(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	scope := pool.Scope("/tenant/alice/user", 1<<20)

	// 新对象：before=0, after=1000 ⇒ 计入 1000。
	settleArchiveCharge(scope, 0, 1000)
	if got := scope.Usage(); got != 1000 {
		t.Fatalf("新对象结算后 Usage=%d, want 1000", got)
	}
	// 覆盖写：before=1000, after=1500 ⇒ 差分 +500（不得双计成 2500）。
	settleArchiveCharge(scope, 1000, 1500)
	if got := scope.Usage(); got != 1500 {
		t.Fatalf("覆盖写结算后 Usage=%d, want 1500（差分，不得双计）", got)
	}
	// 无变化 ⇒ 无操作；nil scope（未装配配额）⇒ 无操作（零回归）。
	settleArchiveCharge(scope, 1500, 1500)
	settleArchiveCharge(nil, 0, 999)
	if got := scope.Usage(); got != 1500 {
		t.Fatalf("无变化/nil scope 后 Usage=%d, want 1500", got)
	}
}

// TestSettleArchiveCharge_SymmetricWithDelete 与删除侧**对称**：结算后按删除侧口径释放，
// 账本必须回到原值（未修必红：不结算 ⇒ 释放变成「未曾提交的释放」⇒ 账本被扣成负数方向）。
func TestSettleArchiveCharge_SymmetricWithDelete(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	scope := pool.Scope("/tenant/alice/user", 1<<20)

	// 模拟：账本上已有 500 属于**其它对象**（预留后**提交** = 真实占用；注意 Scope.Usage()
	// 是**池**用量，仅 TryReserve 不会计入）。
	res, rerr := scope.TryReserve(500)
	if rerr != nil {
		t.Fatalf("TryReserve: %v", rerr)
	}
	res.Commit(500)
	if got := scope.Usage(); got != 500 {
		t.Fatalf("前置：Usage=%d, want 500", got)
	}

	// 归档转存写入 1500（写侧结算）。
	settleArchiveCharge(scope, 0, 1500)
	if got := scope.Usage(); got != 2000 {
		t.Fatalf("写入后 Usage=%d, want 2000（其它 500 + 本次 1500）", got)
	}
	// 删除该产物：删除侧 `releaseExternalDeleteQuota` 的口径就是 `scope.ReleaseUsage(charge)`。
	scope.ReleaseUsage(1500)
	if got := scope.Usage(); got != 500 {
		t.Fatalf("删除后 Usage=%d, want 500（必须恰好回到其它对象的占用；"+
			"未修必红：写侧不结算 ⇒ 这次释放扣掉的是别人的 500 ⇒ 账本少计）", got)
	}
}

// TestCloudArchive_ToVolume_SettlesOwnerQuota **端到端锁定接线**（不只是纯函数语义）：
// 归档转存成功后 owner 配额账本必须增加产物字节数。删掉 handler 里的结算调用（变异）⇒ 必红。
//
// 为何需要本用例：纯函数用例只锁 `settleArchiveCharge` 的差分语义，删掉调用点它照样绿
// （假绿 ✗）。本用例经真实 HTTP 路径（cloudArchiveTask → cloudArchiveToVolume）断言账本变化，
// 才真正锁定「写侧结算」这一接线。
func TestCloudArchive_ToVolume_SettlesOwnerQuota(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	// 装配 owner 配额池（quotaScopeFor 经 ensureTenantQuotaLocked 使用 h.globalPool）。
	h.globalPool = quota.NewPool(1 << 30)

	id := createCompletedTask(t, mgr, "settle.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.cloudArchiveTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	scope := h.quotaScopeFor("anonymous", "user/archive/s.tar.gz")
	if scope == nil {
		t.Fatal("前置：owner 配额 Scope 未装配（本用例无效）")
	}
	if got := scope.Usage(); got <= 0 {
		t.Fatalf("归档转存成功后 owner 账本必须 >0（未接线必红：删除侧 releaseExternalDeleteQuota "+
			"会做「未曾提交的释放」⇒ 幻影释放 ⇒ 少计/可超 owner 配额）；实际 Usage=%d", got)
	}
}

// TestCloudArchive_ToVolume_QuotaFullRejectsBeforeWrite 锁定 R14 C6：**写前**配额探针。
// 修复前只做写后 `settleArchiveCharge`（`scope.Adjust` 永不失败）⇒ 配额已满仍写入并越界。
func TestCloudArchive_ToVolume_QuotaFullRejectsBeforeWrite(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	// 池容量 1 字节：源文件 12 字节 ⇒ 探针必然失败（放不下）。
	h.globalPool = quota.NewPool(1)

	id := createCompletedTask(t, mgr, "q.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"q.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.cloudArchiveTask(rec, req)

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("配额不足必须 507（写前探针），got status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 不得落任何产物：目标卷上 `user/archive/q.tar.gz` 不存在。
	fs, ok := h.externalFSFor("vault")
	if !ok {
		t.Fatal("前置：vault 外部卷未装配")
	}
	if _, serr := fs.Stat(t.Context(), "user/archive/q.tar.gz"); serr == nil {
		t.Fatal("配额不足时不得写入目标卷（写前探针必须早于 transfer.Execute）")
	}
}
