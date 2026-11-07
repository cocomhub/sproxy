// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// transfer_quota_settle_test.go 锁定**转存产物写侧结算**（遗留②「owner Scope 幻影释放」根治）：
// 转存直接写目标卷 FS（不经 pkg/files 路由写内核），若不把产物字节结算进 owner 配额账本，
// 删除该产物时 `releaseExternalDeleteQuota` 会做「**未曾提交的释放**」——从同一 Scope 扣掉
// 同账本其它对象的占用（少计 ⇒ 过度准入）。
//
// 因此写入侧必须按**与删除侧同源的测量**（TargetPhysChargeFor / 卷 Stat 回落）结算，且结算
// 对象必须是**同一个 Scope**（TargetQuotaScopeFor = HTTP 上传路由预留与删除释放所用解析器）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// transferSettleVolumeFor 返回转存目标卷元信息（零值 ACL：测试用「任意 owner 可访问」）。
func transferSettleVolumeFor(name string) (volume.Volume, bool) {
	if name != "vault" {
		return volume.Volume{}, false
	}
	return volume.Volume{Name: "vault", Type: "secretdata"}, true
}

// TestTransferDone_SettlesOwnerQuotaScope 转存成功 → 产物字节结算进 owner 配额 Scope；
// 删除侧按同口径释放后，账本精确回到「其它对象占用」。
//
// 变异（去掉 transferOnce 的 settleTransferCharge 调用）⇒ 转存后 Usage 仍为既有占用 60 ⇒ 红。
func TestTransferDone_SettlesOwnerQuotaScope(t *testing.T) {
	t.Parallel()
	content := []byte(strings.Repeat("x", 100))
	fs := newMemFS()
	mgr, env := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{
		MaxConcurrent: 1, TaskTTL: time.Hour,
	})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) {
		if vol == "vault" {
			return fs, "secretdata"
		}
		return nil, ""
	}
	mgr.volumeFor = transferSettleVolumeFor
	// 结算账本 = owner 的 **user 桶 Scope**（键空间 `user/<rel>`：与 HTTP 上传路由预留、
	// 删除侧 Tenant.UserRel 释放同一解析器/同一对象）。
	var gotRel string
	mgr.targetScopeFor = func(owner, rel string) *quota.Scope {
		gotRel = rel
		return env.quotaBucketFor(owner, "user")
	}
	// memFS 无物理足迹能力 → 结算回落到目标卷 Stat 大小（与删除侧 ExternalStat 回落同义）。
	mgr.targetPhysChargeFor = nil

	userScope := env.quotaBucketFor("alice", "user")
	userScope.Adjust(0, 60) // 同账本既有占用（另一对象，可被幻影释放连带扣掉）

	dest := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		t.Fatal(err)
	}
	task := &CloudTask{ID: "task-q", Filename: "movie.mp4", Owner: "alice",
		Transfer: &TransferSpec{Volume: "vault", Path: "movie.mp4"}}
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Size: int64(len(content))}, nil); err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	if gotRel != "user/movie.mp4" {
		t.Fatalf("结算 Scope 键应为 user 桶相对路径，got %q", gotRel)
	}
	const want = 60 + 100
	if got := userScope.Usage(); got != want {
		t.Fatalf("转存后应结算进 owner 账本（%d 既有 + 100 产物 = %d），got %d（未结算 ⇒ 删除即幻影释放）",
			want-100, want, got)
	}

	// 删除侧释放：同一测量口径（此处直接复用 Scope 释放原语，等价于 releaseExternalDeleteQuota
	// 的 `scope.ReleaseUsage(charge)`）⇒ 账本精确回到既有占用（不多不少）。
	userScope.ReleaseUsage(int64(len(content)))
	if got := userScope.Usage(); got != 60 {
		t.Fatalf("删除后账本应回到既有占用 60（幻影释放会把它扣成 %d），got %d", max(60-100, 0), got)
	}
}

// TestTransferDone_SettleIsDifferentialOnRetry 差分语义：同一目标重写（任务内重试/幂等重放）
// 只记增量，不重复计——否则一个对象被记成两份占用（虚高 ⇒ 假 507）。
func TestTransferDone_SettleIsDifferentialOnRetry(t *testing.T) {
	t.Parallel()
	content := []byte(strings.Repeat("y", 80))
	fs := newMemFS()
	mgr, env := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{
		MaxConcurrent: 1, TaskTTL: time.Hour,
	})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) {
		if vol == "vault" {
			return fs, "secretdata"
		}
		return nil, ""
	}
	mgr.volumeFor = transferSettleVolumeFor
	mgr.targetScopeFor = func(owner, _ string) *quota.Scope { return env.quotaBucketFor(owner, "user") }
	mgr.targetPhysChargeFor = nil

	dest := filepath.Join(t.TempDir(), "retry.bin")
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		t.Fatal(err)
	}
	task := &CloudTask{ID: "task-r", Filename: "retry.bin", Owner: "alice",
		Transfer: &TransferSpec{Volume: "vault", Path: "retry.bin"}}
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Size: int64(len(content))}, nil); err != nil {
		t.Fatalf("首写 transferDone: %v", err)
	}
	scope := env.quotaBucketFor("alice", "user")
	if got := scope.Usage(); got != 80 {
		t.Fatalf("首写后 Usage=%d want 80", got)
	}
	// 第二次转存同 rel（幂等重放：目标已持相同内容，本次不写盘）→ 占用不得翻倍。
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Size: int64(len(content))}, nil); err != nil {
		t.Fatalf("重放 transferDone: %v", err)
	}
	if got := scope.Usage(); got != 80 {
		t.Fatalf("重放同内容不得重复计占用（差分应为 0），Usage=%d want 80", got)
	}
	// 覆盖写（damaged 任务自愈路径：resumeSelfOverwrite ⇒ 真覆写同 rel）同样只记增量。
	task.resumeSelfOverwrite = true
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Size: int64(len(content))}, nil); err != nil {
		t.Fatalf("覆盖写 transferDone: %v", err)
	}
	if got := scope.Usage(); got != 80 {
		t.Fatalf("覆盖写同尺寸不得增账（差分应为 0），Usage=%d want 80", got)
	}
}

// TestTransferDone_SettlesVolumePoolForLocalVolume 锁定 R16 P1 修复（②）：cloud download
// 转存到**本地卷**（rootfs 无 CapacityFS）时产物字节须计入**卷级容量池**——否则写入漏记 +
// 删除侧 ReleaseCommitted 幻影释放（扣掉同卷其它文件占用）。
// 变异（去掉 settleVolumePoolCharge 的 pool.Adjust）⇒ 转存后 pool.Usage 仍 0 ⇒ 红。
func TestTransferDone_SettlesVolumePoolForLocalVolume(t *testing.T) {
	t.Parallel()
	content := []byte(strings.Repeat("z", 100))
	fs := newMemFS()
	pool := quota.NewPool(1 << 20)
	mgr, env := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{
		MaxConcurrent: 1, TaskTTL: time.Hour,
	})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) {
		if vol == "local1" {
			return fs, "local"
		}
		return nil, ""
	}
	mgr.volumeFor = func(name string) (volume.Volume, bool) {
		if name != "local1" {
			return volume.Volume{}, false
		}
		return volume.Volume{Name: "local1", Type: "local"}, true
	}
	mgr.targetScopeFor = func(owner, _ string) *quota.Scope { return env.quotaBucketFor(owner, "user") }
	mgr.targetPhysChargeFor = nil
	// 本地卷的卷级容量池（装配层 h.volSet.Pool 同形）。
	mgr.volumePoolChargeFor = func(vol string) *quota.Pool {
		if vol == "local1" {
			return pool
		}
		return nil
	}
	// 同卷既有占用（用于验证差分不被幻影释放连带扣掉）。
	pool.Adjust(0, 500)

	dest := filepath.Join(t.TempDir(), "local.bin")
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		t.Fatal(err)
	}
	task := &CloudTask{ID: "task-local", Filename: "local.bin", Owner: "alice",
		Transfer: &TransferSpec{Volume: "local1", Path: "local.bin"}}
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Size: int64(len(content))}, nil); err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	const want = 500 + 100
	if got := pool.Usage(); got != want {
		t.Fatalf("转存到本地卷后卷池应增长到 %d（既有 500 + 产物 100），got %d（漏记 ⇒ 删除即幻影释放）", want, got)
	}

	// 删除侧对称：ReleaseCommitted(实测 100) ⇒ 回到既有 500（非幻影）。
	pool.ReleaseCommitted(int64(len(content)))
	if got := pool.Usage(); got != 500 {
		t.Fatalf("删除后卷池应回到既有占用 500，got %d（幻影释放会把它扣成 %d）", got, max(500-100, 0))
	}
}

// TestTransferQuotaGate_VolumePoolProbeForLocal 锁定 R16 P1 修复③：转存到**本地卷**时
// 写前**卷级容量池探测**（不足早失败，探测即释放）——rootfs 无 ReserveSpace ⇒ 容量权威
// 在路由层 Pool。变异（删探测）⇒ 卷池满时仍「成功」转存 ⇒ 红。
func TestTransferQuotaGate_VolumePoolProbeForLocal(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	pool := quota.NewPool(100) // 卷容量 100B，产物 200B ⇒ 应拒绝
	mgr, env := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{
		MaxConcurrent: 1, TaskTTL: time.Hour,
	})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) {
		if vol == "local1" {
			return fs, "local"
		}
		return nil, ""
	}
	mgr.volumeFor = func(name string) (volume.Volume, bool) {
		if name != "local1" {
			return volume.Volume{}, false
		}
		return volume.Volume{Name: "local1", Type: "local"}, true
	}
	mgr.targetScopeFor = func(owner, _ string) *quota.Scope { return env.quotaBucketFor(owner, "user") }
	mgr.volumePoolChargeFor = func(vol string) *quota.Pool {
		if vol == "local1" {
			return pool
		}
		return nil
	}
	dest := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(dest, make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &CloudTask{ID: "task-big", Filename: "big.bin", Owner: "alice",
		Transfer: &TransferSpec{Volume: "local1", Path: "big.bin"}}
	env0 := &transferEnv{task: task, result: &downloader.Result{Size: 200},
		targetFS: fs, rel: "alice/user/big.bin", ctx: context.Background()}
	if err := mgr.transferQuotaGate(env0); err == nil {
		t.Fatal("本地卷卷池满（100B < 200B 产物）应被写前探测拒绝（fail-closed）")
	}
}
