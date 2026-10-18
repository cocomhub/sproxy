// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_recover_race_test.go 锁定「恢复期不得**无锁**写 m.tasks」这一并发场景（独立审核 P1，2026-10-18）。
//
// 缺陷：`recoverTasksFromDir` 在循环里 `m.tasks[task.ID] = &task` **不持 m.mu**，而同一循环对
// `downloading` 任务立即 `go m.executeDownload(...)`——该 goroutine 持锁读写同一 map ⇒ 循环里的
// **无锁写**与运行中的 goroutine **真并发**。Go 的 map 即便一侧持锁也不安全：
// `go test -race` 报 data race（实测：写 `manager_persist.go:179` vs goroutine 在 `:77` 的读）；
// 极端时序直接 `fatal error: concurrent map read and map write` 崩溃，且发生在**启动阶段**
// （任务状态已被部分改写 ⇒ 重启再次触发）。
//
// 用例设计（**锁定场景**，不是弱断言）：
//   - 造 N 条 `downloading` 持久化任务 ⇒ 恢复会为每条启动一个下载 goroutine；
//   - 服务端**先阻塞 ~200ms 再回错** ⇒ 这些 goroutine 在整个恢复循环期间**都活着**（把无锁写
//     与持锁访问的重叠窗口撑到最大），随后自行退出（不会挂住 mgr.Close）；
//   - 单轮恢复 + 语义断言（recovered/restarted/可见条目）⇒ 无状态漂移。
//
// **判别力如实说明**：`-race` 对本缺陷的检出是**概率性**的（交错窗口极短；实测同一用例在
// 通道阻塞与 sleep 两种形态下一次红一次绿 ✗）。因此本用例是**冒烟**，真正的锁定靠
// `internal/archcheck/recover_ordering_gate_test.go`（结构：恢复必须先完成 map 写入、
// **之后**才启动 goroutine —— 顺序颠倒即必红）。
//
// 修好后（先收集 → 持锁批量插入 → **之后**才启动 goroutine）必须干净；去掉锁必被 -race 捕获。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudDownloadManager_RecoverTasksNoUnlockedMapWrite 恢复期 m.tasks 的写入必须全部持锁。
func TestCloudDownloadManager_RecoverTasksNoUnlockedMapWrite(t *testing.T) {
	t.Parallel()
	// 服务端：**阻塞在通道上**（撑开重叠窗口，且不用 time.Sleep ⇒ 不触 R14 睡眠棘轮），
	// 由本用例在断言完成后关闭通道 ⇒ 下载随即失败、goroutine 退出（不挂住 mgr.Close）。
	var mgr *CloudDownloadManager // 先声明：handler 闭包需要引用（实际赋值在下方，早于任何请求）
	hold := make(chan struct{})
	defer close(hold)
	// **确定性判据**（比 -race 可靠）：下载 goroutine 在**首次请求**时记录它看到的 m.tasks 条数。
	// 修复后（先收集 → 持锁批量插入 → **之后**才启动 goroutine）⇒ 必为 N；缺陷版在循环中启动
	// goroutine ⇒ 记录到 < N。`go` 语句提供 happens-before ⇒ 该读取必然发生在插入之后 ⇒ 判定确定。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ = newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	const n = 40
	ids := make([]string, 0, n)
	for i := range n {
		task, err := mgr.CreateTask("url",
			fmt.Sprintf("%s/%d.bin", srv.URL, i),
			fmt.Sprintf("f%d.bin", i), 1024, "", TaskParams{Save: true})
		if err != nil {
			t.Fatalf("CreateTask(%d): %v", i, err)
		}
		mgr.mu.Lock()
		task.Status = "downloading"
		task.UpdatedAt = time.Now()
		mgr.mu.Unlock()
		if serr := mgr.saveTask(task); serr != nil {
			t.Fatalf("saveTask(%d): %v", i, serr)
		}
		ids = append(ids, task.ID)
	}

	// 清空内存态 ⇒ 恢复路径真正从磁盘读并重新填充 m.tasks（等价新进程启动时的空 map）。
	mgr.mu.Lock()
	for _, id := range ids {
		delete(mgr.tasks, id)
	}
	mgr.mu.Unlock()

	root := mgr.cloudStateRootFor("")
	if root == nil {
		t.Fatal("cloudStateRootFor 返回 nil")
	}

	// 单轮恢复：此刻 N 个下载 goroutine 均阻塞在服务端 ⇒ 与循环内无锁写全程重叠。
	recovered, restarted, _ := mgr.recoverTasksFromDir(root)
	if recovered != n {
		t.Fatalf("recovered=%d, want %d", recovered, n)
	}
	if restarted != n {
		t.Fatalf("restarted=%d, want %d（恢复必须重启全部 downloading 任务）", restarted, n)
	}
	// 语义断言：全部条目可见（修复后「先收集→持锁批量插入」不得丢条目）。
	mgr.mu.RLock()
	got := len(mgr.tasks)
	mgr.mu.RUnlock()
	if got != n {
		t.Fatalf("m.tasks 条目=%d, want %d", got, n)
	}
}
