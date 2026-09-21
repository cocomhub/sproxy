// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// quota_sink_criteria_test.go 钉住「外部下载写盘记账」的两条判据关系（审计 F2 的取证与前提门禁）。
//
// 背景（审计 F2，机制成立、树内不可达）：主写盘分支的判据是「Scope 装配 ∧ 下载器实现
// `downloader.WriterDownloader`」（manager_task.go 的 sink 分派），而成功路径的记账判据只看
// 「Scope 是否装配」（sink 路径 `task.account` 已记 committed）⇒ 若配置到一个**不实现
// WriterDownloader 的下载器**（插件形态），字节会直写、不入租户 Scope，而任务账本仍记
// result.Size（幻影账本）。
//
// 实测（2026-09-16 探针，直写下载器 + 装配 Scope，同租户 user 桶先占 60B）：
//
//	完成：直写调用 1 次、size=60、account committed=60、cloud 桶=0、user 桶=60、租户=60
//	删除后：cloud 桶=0、user 桶=60、租户=60
//
// 即：幻影账本确实出现，但 #302 之后 `releaseCommittedUp` 只向上传播**本层实际扣减量**，
// 删除时释放被 cloud 桶（committed=0）钳制 ⇒ 祖先与兄弟桶**完全未被连带扣减**；审计当时
// 推导的「祖先永久欠计」在该修复后已不成立。残留影响仅为「cloud 桶在下次扫描前欠计」，
// 而这正是直写分支的既有设计语义（分派处的注释：退回普通 Download = 仅全局账本）。
//
// 因此本文件**不改行为**（树内不可达、且无副作用），只交付：
//  1. 前提门禁：树内可解析到的下载器都必须支持 sink ⇒ 一旦前提被破坏（新增/替换出非 sink
//     下载器），用例变红并给出处置指引；
//  2. 安全不变量：即便出现幻影账本，释放也不得连带扣减兄弟桶/祖先 —— 这是 #302 语义在
//     cloud 全链路（真实管理器 + 真实 Scope + 真实下载终态）上的回归守卫。
package cloud

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// 编译期断言：树内唯一的内置下载器必须支持 sink。若有人给 HTTPDownloader 去掉
// DownloadWithWriter，构建即失败——比运行期断言更早拦住 F2 前提的破坏。
var _ downloader.WriterDownloader = downloader.NewHTTPDownloader()

// plainDirectDownloader 只实现 downloader.Downloader（**不实现** DownloadWithWriter），
// 直写目标文件。用于复现审计 F2 的「插件下载器」形态。
type plainDirectDownloader struct {
	data  []byte
	sha   string
	calls int
}

func (d *plainDirectDownloader) Name() string         { return "plain-direct" }
func (d *plainDirectDownloader) Supports(string) bool { return true }

func (d *plainDirectDownloader) Download(_ context.Context, _, destPath string, _ downloader.ProgressFunc) (*downloader.Result, error) {
	d.calls++
	if err := os.WriteFile(destPath, d.data, 0o644); err != nil {
		return nil, err
	}
	return &downloader.Result{Size: int64(len(d.data)), Checksum: d.sha}, nil
}

// TestDownloadSinkCriteria_ProductionWiringSupportsSink 是审计 F2 的**前提门禁**：
// 「Scope 装配 ⇒ sink 路径必然被走到」依赖「树内可解析到的下载器都实现 WriterDownloader」。
// 该前提一旦被破坏（新增 FTP 等直写下载器、或改了默认实现），sink 分派会落到直写分支，
// 而成功路径的记账判据仍按 Scope 装配记账 ⇒ 出现幻影账本。此时必须同时把记账判据改为同源
// （只在真正走过 sink 时经 account 记 committed），本用例的失败信息即该处置指引。
func TestDownloadSinkCriteria_ProductionWiringSupportsSink(t *testing.T) {
	t.Parallel()

	// 生产装配路径：config 的 Downloader 名经 NewFromConfig 解析（含 "" 与 "http" 两种写法）。
	// 注：本包用例不向 downloader 全局注册表注册任何实现（各包测试是独立进程），故断言稳定。
	// 注：末条 `NewRegistry()` 走**新建注册表**，第三方注册**不会**让它红——它守的是**另一条前提**
	// （有人把 `NewRegistry()` 内的内置实现换成非 sink 实现），与前三条「全局注册表解析结果」不同源。
	cases := []struct {
		name string
		dl   downloader.Downloader
	}{
		{"DefaultDownloader()", downloader.DefaultDownloader()},
		{`NewFromConfig("")`, downloader.NewFromConfig("")},
		{`NewFromConfig("http")`, downloader.NewFromConfig("http")},
		{"NewRegistry().DefaultDownloader()", downloader.NewRegistry().DefaultDownloader()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := c.dl.(downloader.WriterDownloader); !ok {
				t.Fatalf("%s 解析出的下载器 %T 未实现 downloader.WriterDownloader ⇒ 主写盘分支会退回直写路径，"+
					"而成功路径仍按「Scope 已装配」经 account 记 committed（审计 F2 幻影账本）。"+
					"处置：把 manager_task.go 的成功记账判据改为与分派判据同源（只在真正走过 sink 时记 task 账本）。",
					c.name, c.dl)
			}
		})
	}

	// 管理器装配层同样断言：生产构造出的 m.dl 必须是 sink 下载器。
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 2<<40, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 1, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true,
	})
	if _, ok := mgr.dl.(downloader.WriterDownloader); !ok {
		t.Fatalf("管理器装配出的下载器 %T 未实现 WriterDownloader（同上的 F2 处置指引）", mgr.dl)
	}
}

// TestDownloadSinkCriteria_DirectWriteOverClaimDoesNotTouchSiblings 是 #302 语义在 cloud 全链路上的
// 回归守卫：直写路径（非 sink 下载器）下任务账本会「超额声明」（account committed=文件大小，而桶零入账），
// 删除该任务时释放**不得**连带扣减同租户其它桶或祖先。
//
// 变异（把 #302 的钳制传播改回「全额传父链」）⇒ 本用例断言 tenant.Usage() 会掉到 0 而变红。
func TestDownloadSinkCriteria_DirectWriteOverClaimDoesNotTouchSiblings(t *testing.T) {
	t.Parallel()
	content := []byte(strings.Repeat("x", 60))
	srv := startRawSource(t, content)

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 2<<40, nil, testLogger())
	mgr, h := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 1, TaskTTL: time.Hour, FailedTaskTTL: time.Hour,
		AllowPrivate: true, MaxRetries: 1, RetryDelay: time.Millisecond, DownloadTimeout: 10 * time.Second,
	})
	h.setOwnerQuota("alice", 1000)
	// 同租户的 user 桶先占 60 字节：它是「被超额释放连带扣减」的观测对象。
	userBucket := h.quotaBucketFor("alice", "user")
	userBucket.Adjust(0, 60)
	tenant := h.quotaFor("alice")
	if got := tenant.Usage(); got != 60 {
		t.Fatalf("前置：租户 Usage()=%d want 60", got)
	}

	// 注入非 sink 下载器（复现 F2 形态）：分派必然走直写分支。
	dl := &plainDirectDownloader{data: content, sha: testutil.SHA256Hex(content)}
	mgr.dl = dl

	task, err := mgr.SubmitAndStart("GET", srv.URL, "direct.bin", int64(len(content)), t.Context(), "alice")
	if err != nil {
		t.Fatalf("提交任务失败: %v", err)
	}
	waitTaskDone(t, mgr, task.ID)
	if snap, _ := mgr.SnapshotTask(task.ID, "alice"); snap.Status != "completed" {
		t.Fatalf("直写路径任务应 completed, got %q (%s)", snap.Status, snap.Error)
	}
	if dl.calls != 1 {
		t.Fatalf("直写分支应被调用 1 次（F2 前提：下载器不支持 sink 时走直写），实际 %d", dl.calls)
	}

	if err := mgr.DeleteTask(task.ID, "alice"); err != nil {
		t.Fatalf("DeleteTask 失败: %v", err)
	}
	// 任务账本对其他桶的释放必须被本层钳制：兄弟桶（user）与祖先（租户）都不受影响。
	// DeleteTask 的配额释放是最终一致（goroutine 退出时经 releaseAbandonedTaskScope 回拨），
	// 断言前先等收敛——否则 CI 繁忙/慢环境（+Vault）偶发读到释放前中间态（`Usage=120`）。
	waitUsage(t, userBucket, 60, "user 桶")
	waitUsage(t, tenant, 60, "租户")
	cloudBucket := h.quotaBucketFor("alice", "cloud")
	if cloudBucket != nil && cloudBucket.Usage() < 0 {
		t.Fatalf("cloud 桶 Usage()=%d 不应为负", cloudBucket.Usage())
	}
}
