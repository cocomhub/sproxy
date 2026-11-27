// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// find_by_url_snapshot_test.go 锁「URL 去重命中的快照必须深拷贝」（同 ListTasks 同类缺陷）。
//
// `CloudDownloadManager.CreateTask` 在 URL 去重命中时把 `findByURL` 的结果**直接返回给调用方**
// （HTTP 层随后在解锁后 `json.Marshal`）。若该快照是浅拷贝 `c := *t`，它共享活动任务的
// `Chunks` 底层数组与 `*Diagnostics`：
//   - `recordChunkState` 对同 index 是**原地改写** `task.Chunks[i] = st`；
//   - `recordKeptPartial` 在锁内写 `PartialPath/PartialSize`。
// 快照逃逸到后续步骤（序列化/日志/回显）后，就与后续的锁内写无同步 ⇒ DATA RACE（-race 可复现）。
//
// 这与 2026-11-27 修的 `ListTasks` 是**同一个缺陷族**：#1327/#1329 只改了
// SnapshotTask/SnapshotTasks，`ListTasks` 漏；本用例钉住另一个漏网点 `findByURL`。

import (
	"encoding/json"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestFindByURL_SnapshotDeepCopy 去重命中的快照不得与活动任务共享可变结构。
func TestFindByURL_SnapshotDeepCopy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir+"/cap", 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, actionsTestConfig())

	const url = "https://example.com/s/EXAMPLE-FIND-BY-URL-1"
	created, err := mgr.CreateTask("url", url, "find.bin", 4096, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	live := mgr.tasks[created.ID]
	if live == nil {
		mgr.mu.Unlock()
		t.Fatalf("任务未注册: %s", created.ID)
	}
	recordChunkState(live, downloader.ChunkState{Index: 0, Phase: "downloading"})
	mgr.mu.Unlock()

	// ① 去重命中（同 URL 同 params ⇒ 走 findByURL 快照路径）。
	snap, cerr := mgr.CreateTask("url", url, "find.bin", 4096, "", TaskParams{Save: true})
	if cerr != nil {
		t.Fatalf("去重 CreateTask: %v", cerr)
	}
	if snap == nil || snap.ID != created.ID {
		t.Fatalf("去重应复用既有任务 %s，得到 %+v", created.ID, snap)
	}
	// ② **确定性断言**（不依赖时序）：Chunks 不得共享底层数组。
	if len(snap.Chunks) == 0 {
		t.Fatalf("快照应含 chunk 状态，得到空")
	}
	if &snap.Chunks[0] == &live.Chunks[0] {
		t.Fatal("去重快照与活动任务共享 Chunks 底层数组（应为深拷贝）")
	}
	if snap.account != nil {
		t.Fatal("快照不得暴露运行时配额句柄")
	}
	// ③ 并发读：快照逃逸 + 延迟序列化，与锁内原地改写并发（-race 检查）。
	var wg = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			mgr.mu.Lock()
			recordChunkState(live, downloader.ChunkState{Index: 0, Phase: "done", Done: 1})
			d := live.diag()
			d.PartialPath = "/tmp/partial"
			d.PartialSize = 1
			mgr.mu.Unlock()
		}
	}()
	go func() {
		defer close(wg)
		held := []*CloudTask{}
		for range 50 {
			if s, ok := mgr.SnapshotTask(created.ID, ""); ok {
				held = append(held, s)
			}
		}
		for _, s := range held {
			if _, merr := json.Marshal(s); merr != nil {
				return
			}
		}
	}()
	<-done
	<-wg
	_ = testutil.TestKey
}
