// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cloud_quota_writer_test.go 验证任务 7：cloud download 外部下载流接入 QuotaWriter
// 边写边记 + 自动补留（替换占位预留 + 完成后收尾 Adjust 的相对后端对账）。
//
// 被测语义：
//  1. 未知大小任务创建期占位 1 GiB 预留，完成后 QuotaWriter 收尾释放未用占位、Scope 收敛到实际大小；
//  2. 配额真满（占位预留失败）ubmitAndStart 返回 ErrStorageFull，Scope 无泄漏；
//  3. QuotaWriter 初始预留不足时自动补留、不 507（直接调用集成断言——真实 HTTP 传输层按 CL 截停，无法在 Download 全链路触发）；
//  4. 已知大小任务下载失败（传输层 unexpected EOF / 超时）便捷地 failed + 保留 .partial，ResumeTask 续传成功后完成、账本收敛；
//  5. 全局 storageMgr 账本（/api/stats CategoryCloud）与 Scope 双轨并行，无泄漏。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestCloudQuotaWriter_UnknownSizePlaceholder 验证未知大小任务占位 1 GiB 预留、完成后
// QuotaWriter 收尾释放未用占位并收敛到实际大小；配额真满时占位预留失败返回 storage full 且无泄漏。
func TestCloudQuotaWriter_UnknownSizePlaceholder(t *testing.T) {
	contentA := []byte(strings.Repeat("x", 60))
	srvA := startRawSource(t, contentA)

	dir := t.TempDir()
	sm := NewStorageManager(dir, 2<<40, nil, testLogger()) // 全局 1 TiB，占位 1 GiB 足够
	cfg := &CloudDownloadConfig{
		SyncThreshold: 1,
		MaxConcurrent: 3,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	setTestOwnerQuota(h, "alice", 2<<30)

	// 场景 A：未知大小（totalSize=-1）→ 初始占位 1 GiB，响应 60 → 完成。
	taskA, err := mgr.SubmitAndStart("url", srvA.URL, "auto.bin", -1, t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, taskA.ID)
	if snap, _ := mgr.SnapshotTask(taskA.ID, "alice"); snap.Status != "completed" {
		t.Fatalf("场景 A 应 completed, got %q (%s)", snap.Status, snap.Error)
	}
	if got := h.quotaFor("alice").Usage(); got != int64(len(contentA)) {
		t.Fatalf("场景 A 完成后 Scope Usage()=%d want %d（边写边记收敛，占位已释放）", got, len(contentA))
	}
	if got := sm.UsageByCategory()[CategoryCloud]; got != int64(len(contentA)) {
		t.Fatalf("场景 A 完成后 CategoryCloud=%d want %d", got, len(contentA))
	}

	// 场景 B：配额真满——bob 上限 200，未知大小任务创建成功（任务 7：创建期不再占位，
	// Scope 预留延迟到下载流 QuotaWriter 首次写盘），首次写盘占位 1 GiB 预留失败 →
	// 下载失败 → 任务 failed（storage full），Scope 无泄漏。
	setTestOwnerQuota(h, "bob", 200)
	srvB := startRawSource(t, []byte(strings.Repeat("y", 500)))
	taskB, err := mgr.SubmitAndStart("url", srvB.URL, "full.bin", -1, t.Context(), "bob")
	if err != nil {
		t.Fatalf("未知大小任务创建应成功（Scope 延迟到写盘预留）: %v", err)
	}
	waitTaskDone(t, mgr, taskB.ID)
	snapB, _ := mgr.SnapshotTask(taskB.ID, "bob")
	// 错误文本含 "storage quota exceeded"（downloader 包装 "create quota sink: <ErrStorageFull>"），
	// 任务 Error 是字符串无法做 errors.Is 类型断言，用文本特征判定。
	if snapB.Status != "failed" || !strings.Contains(snapB.Error, "storage quota exceeded") {
		t.Fatalf("小配额下未知大小任务下载应 failed(storage full), got %q (%s)", snapB.Status, snapB.Error)
	}
	cloudB := h.quotaBucketFor("bob", "cloud")
	if cloudB == nil {
		t.Fatal("bob cloud 桶 Scope 应为非 nil")
	}
	if got := cloudB.Usage(); got != 0 {
		t.Fatalf("写盘预留失败后 cloud 桶 Usage()=%d want 0（无泄漏）", got)
	}
	if got := cloudB.Reserved(); got != 0 {
		t.Fatalf("写盘预留失败后 cloud 桶 Reserved()=%d want 0", got)
	}
}

// TestCloudQuotaWriter_AutoTopUpAcrossWrites 用直接调用 QuotaWriter 验证「初始预留不足时
// 自动补留、不 507」的集成语义：Content-Length 已知 10，但响应体达 90，单次大 Write 触发
// 自动补留。真实 HTTP 传输层会按 Content-Length 截停 body，故在下载全链路用超预留的
// 响应无法触发（HTTP 层直接 unexpected EOF）；这里对 QuotaWriter 接入 cloud 的等价物断言。
func TestCloudQuotaWriter_AutoTopUpAcrossWrites(t *testing.T) {
	root := quota.NewPool(10 * 1024 * 1024)
	scope := root.Scope("/tenant/t/cloud", 1000)
	w, err := quota.NewQuotaWriter(scope, &countWriter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 80)); err != nil {
		t.Fatalf("Write(80) 应自动补留成功（不 507）, got %v", err)
	}
	if got := scope.Usage(); got != 80 {
		t.Fatalf("Usage()=%d want 80（边写边记）", got)
	}
}

// TestCloudQuotaWriter_TruncatedResponseFailsCleanly 验证下载中途失败（Content-Length 谎报 →
// 传输层 unexpected EOF）→ 任务 failed、已写字节占账但 reserve 无泄漏（QuotaWriter Finish(false)
// 回拨）；.partial 保留给 ResumeTask 复用。
func TestCloudQuotaWriter_TruncatedResponseFailsCleanly(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("bob", 1000)
	sm := NewStorageManager(env.root, 1024*1024, nil, testLogger())

	// 服务器：Content-Length 谎报 200，实际只发 30 后停流 → unexpected EOF → 失败。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "200")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 30))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(1 * time.Second)
	}))
	defer srv.Close()

	mgr := NewCloudDownloadManager("", sm, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 1, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true, DownloadTimeout: 300 * time.Millisecond, MaxRetries: 1,
	}, func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	})
	defer mgr.Close()

	task, err := mgr.SubmitAndStart("url", srv.URL, "big.bin", 200, t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	snap, _ := mgr.SnapshotTask(task.ID, "bob")
	if snap.Status != "failed" {
		t.Fatalf("截断响应应 failed, got %q (%s)", snap.Status, snap.Error)
	}
	cloudB := env.h.quotaBucketFor("bob", "cloud")
	// 已写 30 字节占账，不超；reserve 无泄漏。
	if got := cloudB.Usage(); got > 30 {
		t.Fatalf("失败后 cloud 桶 Usage()=%d 不应超过已写 30 字节", got)
	}
	if got := cloudB.Reserved(); got != 0 {
		t.Fatalf("失败后 cloud 桶 Reserved()=%d want 0", got)
	}
}

// TestCloudWriteFailureKeepsPartialAndResume 验证写失败（读取超时）保留 .partial、
// ResumeTask 续传成功后正常完成、账本收敛到实际大小；全局与 Scope 双轨一致无泄漏。
func TestCloudWriteFailureKeepsPartialAndResume(t *testing.T) {
	full := make([]byte, 100)
	for i := range full {
		full[i] = byte(i % 251)
	}
	var sawRange atomicBool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange.set(true)
			w.Header().Set("Content-Range", "bytes 10-99/100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[10:])
			return
		}
		// first：只发 10 字节后停流 → 触发整体超时，保留 .partial。
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full[:10])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold:   1,
		MaxConcurrent:   1,
		TaskTTL:         time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		DownloadTimeout: 300 * time.Millisecond,
		MaxRetries:      1,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	setTestOwnerQuota(h, "alice", 1000)

	task, err := mgr.SubmitAndStart("url", srv.URL, "keep.bin", int64(len(full)), t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	snap, _ := mgr.SnapshotTask(task.ID, "alice")
	if snap.Status != "failed" {
		t.Fatalf("写失败后应 failed, got %q (%s)", snap.Status, snap.Error)
	}

	// 失败后保留 .partial（10 字节）。
	taskDir := mgr.taskDirFor("alice", task.ID)
	partialPath := filepath.Join(taskDir, "keep.bin.partial")
	fi, err := os.Stat(partialPath)
	if err != nil {
		t.Fatalf("写失败后应保留 .partial: %v", err)
	}
	if fi.Size() != 10 {
		t.Fatalf(".partial 大小=%d want 10", fi.Size())
	}

	// 失败后 Scope 已 commit 的 10 字节占账（QuotaWriter 边写边记），reserve 无泄漏。
	if got := h.quotaFor("alice").Usage(); got != 10 {
		t.Fatalf("写失败后 Scope Usage()=%d want 10（已写 10 字节占账）", got)
	}
	if got := h.quotaFor("alice").Reserved(); got != 0 {
		t.Fatalf("写失败后 Scope Reserved()=%d want 0（reserve 无泄漏）", got)
	}

	// 手动续传（Range）→ 完成，账本收敛到实际大小。
	if rerr := mgr.ResumeTask(task.ID, false, "alice"); rerr != nil {
		t.Fatal(rerr)
	}
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID, "alice"); cur.Status != "completed" {
		t.Fatalf("续传后应 completed, got %q (%s)", cur.Status, cur.Error)
	}
	if !sawRange.get() {
		t.Fatal("续传应发送 Range 头")
	}
	if got := h.quotaFor("alice").Usage(); got != int64(len(full)) {
		t.Fatalf("续传完成后 Scope Usage()=%d want %d", got, len(full))
	}
	if got := sm.UsageByCategory()[CategoryCloud]; got != int64(len(full)) {
		t.Fatalf("续传完成后 CategoryCloud=%d want %d", got, len(full))
	}
	dest := filepath.Join(mgr.taskDirFor("alice", task.ID), "keep.bin")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Fatal("续传文件内容不一致")
	}
}

// startRawSource 启动一个固定内容测试源，设置正确 Content-Length 并一次性写出 body。
func startRawSource(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// atomicBool 是轻量原子布尔（测试辅助）。
type atomicBool struct{ b atomic.Bool }

func (ab *atomicBool) set(x bool) { ab.b.Store(x) }
func (ab *atomicBool) get() bool  { return ab.b.Load() }

// setTestOwnerQuota 设置 owner 的配额上限（修改共享 Config 的 OwnerQuotas，在首次 quotaFor 前调用）。
func setTestOwnerQuota(h *Handlers, owner string, bytes int64) {
	cfg := h.cfgPtr.Load()
	if cfg.OwnerQuotas == nil {
		cfg.OwnerQuotas = make(map[string]int64)
	}
	cfg.OwnerQuotas[owner] = bytes
}

// countWriter 统计写入字节的 io.Writer（QuotaWriter 集成测试辅助）。
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// TestCloudDownloadManager_CancelDuringWrite_Race 直测审查 C 的 cancel 竞态（严重缺失 2）：
// 慢速源写盘进行中反复 Cancel → 等下载 goroutine 完全退出 → 断言 releaseTaskScope 幂等：
// Scope committed/reserved 最终精确归零（不重复回拨、不反负），storageMgr 全局账本同步归零。
func TestCloudDownloadManager_CancelDuringWrite_Race(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600") // 100MB，避免意外 EOF
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 200)) // 先落 200 字节，使 QW 进入已 commit 状态
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-blockCh // 停流，制造「写盘进行中」窗口
	}))
	t.Cleanup(func() { close(blockCh); srv.Close() })

	dir := t.TempDir()
	sm := NewStorageManager(dir, 4<<30, nil, testLogger()) // 全局 4 GiB
	cfg := &CloudDownloadConfig{
		SyncThreshold:   1,
		MaxConcurrent:   1,
		TaskTTL:         time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		DownloadTimeout: 30 * time.Second,
		MaxRetries:      1,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	setTestOwnerQuota(h, "alice", 2<<30) // 2 GiB，容纳未知大小占位 1 GiB

	task, err := mgr.SubmitAndStart("url", srv.URL, "cancel-race.bin", -1, t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	// 等待 .partial 出现（下载已开始写盘）
	taskDir := mgr.taskDirFor("alice", task.ID)
	deadline := time.Now().Add(5 * time.Second)
	partialSeen := false
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(filepath.Join(taskDir, "cancel-race.bin.partial")); statErr == nil {
			partialSeen = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !partialSeen {
		t.Fatal("下载未开始写盘（.partial 未出现）")
	}

	// 写盘进行中反复 Cancel：首次成功，后续因状态已 cancelled 返回错误（幂等复查路径）
	for range 5 {
		_ = mgr.CancelTask(task.ID, "alice")
		time.Sleep(5 * time.Millisecond)
	}
	if !mgr.waitTaskStopped(task.ID, 5*time.Second) {
		t.Fatal("cancel 后下载 goroutine 未退出")
	}

	// 终态为 cancelled
	snap, ok := mgr.SnapshotTask(task.ID, "alice")
	if !ok || snap.Status != "cancelled" {
		t.Fatalf("任务状态=%q want cancelled", snap.Status)
	}

	// releaseTaskScope 幂等：Scope committed/reserved 精确归零、不反负
	cloudB := h.quotaBucketFor("alice", "cloud")
	if cloudB == nil {
		t.Fatal("alice cloud 桶 Scope 应为非 nil")
	}
	if got := cloudB.Usage(); got != 0 {
		t.Fatalf("cancel 后 cloud 桶 Usage()=%d want 0（已 commit 回拨）", got)
	}
	if got := cloudB.Reserved(); got != 0 {
		t.Fatalf("cancel 后 cloud 桶 Reserved()=%d want 0（reserve 释放）", got)
	}
	if got := h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("cancel 后租户根 Usage()=%d want 0", got)
	}
	// 幂等复调 releaseTaskScope：不得重复回拨（任务已结算）
	mgr.mu.Lock()
	realTask := mgr.tasks[task.ID]
	mgr.mu.Unlock()
	if realTask != nil {
		mgr.releaseTaskScope(realTask)
	}
	if got := cloudB.Usage(); got != 0 {
		t.Fatalf("幂等复调后 cloud 桶 Usage()=%d want 0（不重复回拨）", got)
	}
	if got := cloudB.Reserved(); got != 0 {
		t.Fatalf("幂等复调后 cloud 桶 Reserved()=%d want 0", got)
	}
	// storageMgr 全局账本同步归零（CancelTask 已释放 ReservedSize）
	if got := sm.Usage(); got != 0 {
		t.Fatalf("cancel 后 storageMgr Usage()=%d want 0", got)
	}
}

// TestCloudDownloadManager_ConcurrentResumeAndCancel 直测审查 C 的并发 resume+cancel
// 竞态（缺口 8）：并发 ResumeTask 与 CancelTask 不得竞争写同一 .partial（running 护栏），
// -race 下无数据竞争；终态后 Scope/storageMgr 账本不反负、不虚高（<= 磁盘实际占用）。
func TestCloudDownloadManager_ConcurrentResumeAndCancel(t *testing.T) {
	full := make([]byte, 100)
	for i := range full {
		full[i] = byte(i % 251)
	}
	var first atomicBool
	first.set(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// 续传：返回 206 剩余部分（Range 续传成功路径）
			w.Header().Set("Content-Range", "bytes 10-99/100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[10:])
			return
		}
		if first.compareAndSwap(true, false) {
			// 首次：截断响应（Content-Length 谎报 100，只发 10 字节）→ 下载失败保留 .partial
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full[:10])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		// 兜底：全量（不应再出现无 Range 请求）
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold:   1,
		MaxConcurrent:   1,
		TaskTTL:         time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		DownloadTimeout: 300 * time.Millisecond,
		IdleTimeout:     300 * time.Millisecond,
		MaxRetries:      1,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	setTestOwnerQuota(h, "alice", 1000)

	// 首次下载：截断失败 → failed 保留 10 字节 .partial
	task, err := mgr.SubmitAndStart("url", srv.URL, "resume-race.bin", int64(len(full)), nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	if snap, _ := mgr.SnapshotTask(task.ID, "alice"); snap.Status != "failed" {
		t.Fatalf("首次下载应 failed, got %q (%s)", snap.Status, snap.Error)
	}
	if got := mgr.diskUsageOfTask("alice", task.ID); got != 10 {
		t.Fatalf("首次失败后磁盘占用=%d want 10（.partial）", got)
	}
	if got := h.quotaFor("alice").Usage(); got != 10 {
		t.Fatalf("首次失败后 Scope Usage()=%d want 10", got)
	}

	// 并发 resume+cancel（每个 goroutine 交替调用；-race 检测写 .partial 竞争）
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 25 {
				_ = mgr.ResumeTask(task.ID, false, "alice")
				_ = mgr.CancelTask(task.ID, "alice")
			}
		})
	}
	wg.Wait()

	// 等待所有下载 goroutine 退出（running 护栏的终点）
	deadline := time.Now().Add(10 * time.Second)
	for {
		mgr.mu.RLock()
		running := mgr.running[task.ID]
		mgr.mu.RUnlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("并发 resume+cancel 后仍有下载 goroutine 运行")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 终态必须为 failed/cancelled/completed 之一
	snap, ok := mgr.SnapshotTask(task.ID, "alice")
	if !ok {
		t.Fatal("任务消失")
	}
	switch snap.Status {
	case "failed", "cancelled", "completed":
	default:
		t.Fatalf("并发后任务状态=%q want failed/cancelled/completed", snap.Status)
	}
	disk := mgr.diskUsageOfTask("alice", task.ID)
	cloudB := h.quotaBucketFor("alice", "cloud")
	if cloudB.Usage() < 0 || cloudB.Reserved() < 0 {
		t.Fatalf("并发后账本反负: Usage=%d Reserved=%d", cloudB.Usage(), cloudB.Reserved())
	}
	if cloudB.Usage() > disk {
		t.Fatalf("并发后 Scope Usage()=%d > 磁盘占用=%d（虚高/泄漏）", cloudB.Usage(), disk)
	}
	if cloudB.Reserved() != 0 {
		t.Fatalf("并发后 cloud 桶 Reserved()=%d want 0", cloudB.Reserved())
	}
	if sm.Usage() < 0 {
		t.Fatalf("并发后 storageMgr Usage()=%d 反负", sm.Usage())
	}
}

// compareAndSwap 是 atomicBool 的 CAS 便捷方法（并发首请求判定）。
func (ab *atomicBool) compareAndSwap(old, new bool) bool { return ab.b.CompareAndSwap(old, new) }
