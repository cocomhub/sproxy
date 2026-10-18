// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudDownloadManager_SaveTaskConcurrentAtomicWrite 钉住 `saveTask` 的**原子写**：
//
// 背景（CI -race 实测 flake）：`writeRootFile` 是 O_TRUNC 截断写（非原子），而 completed
// 终态保存（persistCompleted）与 flushLoop 进度批量保存会**并发**写同一任务文件 ⇒ 截断+写
// 交错会写出**损坏 JSON** ⇒ 重启恢复 `unmarshal` 失败、任务从磁盘丢失（组内 TaskIDs 被修剪）。
// 修复：每次保存写**唯一 tmp** + `AtomicRename` ⇒ 正式文件恒为某个**完整**快照。
//
// 断言：
//  1. **任意时刻**读任务文件都是完整 JSON（修复前会捕获空/半写文件，必红）；
//  2. 并发保存结束后文件仍完整、且目录无 `.tmp.` 残留。
func TestCloudDownloadManager_SaveTaskConcurrentAtomicWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)

	taskID := "cloud-atomic-test-1"
	now := time.Now()
	task := &CloudTask{
		ID:        taskID,
		URL:       "https://example.invalid/f",
		Status:    "downloading",
		TotalSize: 1,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	mgr.mu.Lock()
	mgr.tasks[taskID] = task
	mgr.mu.Unlock()

	const writers = 8
	const iters = 200
	stop := make(chan struct{})
	var readCorrupt atomic.Bool

	var wgReader sync.WaitGroup
	wgReader.Go(func() {
		root := mgr.cloudStateRootFor("")
		rel := cloudStateRel + "/" + taskID + ".json"
		for {
			select {
			case <-stop:
				return
			default:
				data, err := readRootFile(root, rel)
				if err != nil {
					continue // rename 替换期间的瞬时 IO/不存在：非损坏证据
				}
				var parsed CloudTask
				if uerr := json.Unmarshal(data, &parsed); uerr != nil {
					// 读到内容但解析失败 = 半写/损坏 JSON（修复前非原子截断写的窗口）。
					readCorrupt.Store(true)
				}
			}
		}
	})

	var wgWriters sync.WaitGroup
	for w := range writers {
		wgWriters.Add(1)
		go func(w int) {
			defer wgWriters.Done()
			for i := range iters {
				mgr.mu.Lock()
				task.UpdatedAt = time.Now()
				task.Downloaded = int64(w*iters + i)
				mgr.mu.Unlock()
				if err := mgr.saveTask(task); err != nil {
					t.Errorf("saveTask failed: %v", err)
				}
			}
		}(w)
	}
	wgWriters.Wait()
	close(stop)
	wgReader.Wait()

	if readCorrupt.Load() {
		t.Fatal("并发 saveTask 读到损坏/半写 JSON：写盘必须原子（唯一 tmp + rename）")
	}

	// 最终文件完整可解析。
	root := mgr.cloudStateRootFor("")
	rel := cloudStateRel + "/" + taskID + ".json"
	final, err := readRootFile(root, rel)
	if err != nil {
		t.Fatalf("最终任务文件不可读: %v", err)
	}
	var parsed CloudTask
	if uerr := json.Unmarshal(final, &parsed); uerr != nil {
		t.Fatalf("最终任务文件损坏: %v", uerr)
	}

	// 无 tmp 残留（成功 rename 后 tmp 必须消失；否则恢复扫描目录会积累垃圾）。
	entries, err := root.ReadDir(cloudStateRel)
	if err != nil {
		t.Fatalf("读取持久化目录失败: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("saveTask 后残留 tmp 文件: %s", e.Name())
		}
	}

	// 磁盘事实一致：残留的必须是唯一任务文件。
	if n := len(entries); n != 1 {
		t.Fatalf("期望持久化目录仅任务文件, 实际 %d 个条目", n)
	}
}
