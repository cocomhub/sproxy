// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// upload_tracker.go 是**在途上传注册表**（内存态，仅本进程可见）。
//
// 为什么需要：分块边加密边上传，meta 在**最后**提交 ⇒ 上传窗口内目标逻辑路径在索引里不存在。
// 若无注册表，「审查」能力在中途会给出误导结论：
//   - `trace <目标路径>` ⇒ 404（与「文件不存在」无法区分）；
//   - `explain <容器>` ⇒ 在途分块被判为 `blob_orphan`「疑似孤儿（删除请走 GC）」。
//
// 语义边界（诚实声明）：
//   - **本进程内准确**：HTTP 审查与写请求在同一进程 ⇒ 命中注册表；
//   - 跨进程/跨实例不可见（另一实例的上传仍表现为未引用 blob）。此处不做 mtime 猜测——
//     blob mtime 被刻意打散（0–48h 偏移），按时间判定必然误判；
//   - GC 无需依赖本表：`writesInFlight` 已使 GC 在有任何在途写时放弃整轮（见 GC 阶段 0）。

import (
	"sync"
	"sync/atomic"
	"time"
)

// uploadProgress 是单个在途上传的进度快照。
type uploadProgress struct {
	rel       string
	container string
	total     int64 // 逻辑大小（调用方声明；<0 = 未知 ⇒ 归 0）
	committed int64 // 已上传（**密文**字节：含每块头/nonce/tag 开销，略大于明文）
	chunks    int64
	startedAt int64 // UnixNano
}

// uploadTracker 是进程内注册表（独立锁，不进入卷主锁的临界区）。
type uploadTracker struct {
	mu   sync.RWMutex
	m    map[string]*uploadProgress
	live atomic.Int64
}

func (t *uploadTracker) begin(rel, container string, total int64) {
	if t == nil || rel == "" {
		return
	}
	if total < 0 {
		total = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]*uploadProgress)
	}
	t.m[rel] = &uploadProgress{rel: rel, container: container, total: total, startedAt: time.Now().UnixNano()}
	t.live.Add(1)
}

// advance 累加已上传字节与块数（高并发 chunk sink 调用 ⇒ 持锁时间 O(1)）。
func (t *uploadTracker) advance(rel string, bytes int64) {
	if t == nil {
		return
	}
	t.mu.RLock()
	p := t.m[rel]
	t.mu.RUnlock()
	if p == nil {
		return
	}
	t.mu.Lock()
	p.committed += bytes
	p.chunks++
	t.mu.Unlock()
}

func (t *uploadTracker) end(rel string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if _, ok := t.m[rel]; ok {
		delete(t.m, rel)
		t.live.Add(-1)
	}
	t.mu.Unlock()
}

// snapshot 返回某个 rel 的进度快照（rel 空或未在途 ⇒ ok=false）。
func (t *uploadTracker) snapshot(rel string) (uploadProgress, bool) {
	if t == nil || rel == "" {
		return uploadProgress{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.m[rel]
	if !ok || p == nil {
		return uploadProgress{}, false
	}
	return *p, true
}

// forContainer 返回写目标容器等于 container（或位于其子树）的在途上传快照。
func (t *uploadTracker) forContainer(container string) []uploadProgress {
	if t == nil || container == "" {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []uploadProgress
	for _, p := range t.m {
		if p == nil {
			continue
		}
		if p.container == container || hasPathPrefix(p.container, container) {
			out = append(out, *p)
		}
	}
	return out
}

// hasPathPrefix 报告 p 是否位于 prefix 子树（含相等）。
func hasPathPrefix(p, prefix string) bool {
	if p == prefix {
		return true
	}
	return len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/'
}

// ---- SecretdataFS 侧薄封装（注册表懒初始化，零值安全）----

func (s *SecretdataFS) beginUpload(rel, container string, total int64) {
	s.uploads.begin(rel, container, total)
}

func (s *SecretdataFS) advanceUpload(rel string, bytes int64) {
	s.uploads.advance(rel, bytes)
}

func (s *SecretdataFS) endUpload(rel string) {
	s.uploads.end(rel)
}

func (s *SecretdataFS) uploadSnapshot(rel string) (uploadProgress, bool) {
	return s.uploads.snapshot(rel)
}

func (s *SecretdataFS) uploadsForContainer(container string) []uploadProgress {
	return s.uploads.forContainer(container)
}
