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
// 并发语义：同一 rel 可能被**并发写**（两个客户端/重试）⇒ 每个上传持有独立 **token**
// （`*uploadProgress`）；`end(token)` 只删除自己的条目，不会误删另一路在途上传。
//
// 语义边界（诚实声明）：
//   - **本进程内准确**：HTTP 审查与写请求在同一进程 ⇒ 命中注册表；
//   - 跨进程/跨实例不可见（另一实例的上传仍表现为未引用 blob）。此处不做 mtime 猜测——
//     blob mtime 被刻意打散（0–48h 偏移），按时间判定必然误判；
//   - GC 无需依赖本表：`writesInFlight` 已使 GC 在有任何在途写时放弃整轮（见 GC 阶段 0）。

import (
	"sync"
	"time"
)

// uploadProgress 是**不可变的进度快照**（对外返回，不含锁；避免 copylocks）。
type uploadProgress struct {
	rel       string
	container string
	total     int64 // 逻辑大小（调用方声明；<0 = 未知 ⇒ 归 0）
	committed int64 // 已上传（**密文**字节：含每块头/nonce/tag 开销，略大于明文）
	chunks    int64
	startedAt int64 // UnixNano
}

// uploadRecord 是注册表内的**可变记录**（含锁，块级并发累加）。
type uploadRecord struct {
	rel       string
	container string
	total     int64
	startedAt int64

	mu        sync.Mutex // 保护 committed/chunks
	committed int64
	chunks    int64
}

// snapshot 取一份一致快照。
func (r *uploadRecord) snapshot() uploadProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return uploadProgress{
		rel: r.rel, container: r.container, total: r.total,
		committed: r.committed, chunks: r.chunks, startedAt: r.startedAt,
	}
}

// uploadTracker 是进程内注册表（独立锁，不进入卷主锁的临界区）。
type uploadTracker struct {
	mu sync.RWMutex
	m  map[string]*uploadRecord // rel → 最近一次 begin 的上传（历史条目仅在无人引用时移除）
}

// begin 登记一次在途上传并返回其令牌；rel 空 ⇒ 返回 nil（不登记）。
func (t *uploadTracker) begin(rel, container string, total int64) *uploadRecord {
	if t == nil || rel == "" {
		return nil
	}
	if total < 0 {
		total = 0
	}
	p := &uploadRecord{rel: rel, container: container, total: total, startedAt: time.Now().UnixNano()}
	t.mu.Lock()
	if t.m == nil {
		t.m = make(map[string]*uploadRecord)
	}
	t.m[rel] = p
	t.mu.Unlock()
	return p
}

// advance 累加令牌对应的已上传字节与块数（并发 chunk sink 调用）。
func (t *uploadTracker) advance(p *uploadRecord, bytes int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.committed += bytes
	p.chunks++
	p.mu.Unlock()
}

// end 注销令牌：仅当该 rel 仍指向此令牌时才删除（不误删同 rel 的另一路并发上传）。
func (t *uploadTracker) end(p *uploadRecord) {
	if t == nil || p == nil {
		return
	}
	t.mu.Lock()
	if cur, ok := t.m[p.rel]; ok && cur == p {
		delete(t.m, p.rel)
	}
	t.mu.Unlock()
}

// snapshot 返回某个 rel 的进度快照（rel 空或未在途 ⇒ ok=false）。
func (t *uploadTracker) snapshot(rel string) (uploadProgress, bool) {
	if t == nil || rel == "" {
		return uploadProgress{}, false
	}
	t.mu.RLock()
	p, ok := t.m[rel]
	t.mu.RUnlock()
	if !ok || p == nil {
		return uploadProgress{}, false
	}
	return p.snapshot(), true
}

// forContainer 返回写目标容器等于 container（或位于其子树）的在途上传快照。
func (t *uploadTracker) forContainer(container string) []uploadProgress {
	if t == nil || container == "" {
		return nil
	}
	t.mu.RLock()
	ps := make([]*uploadRecord, 0, len(t.m))
	for _, p := range t.m {
		if p == nil {
			continue
		}
		if p.container == container || hasPathPrefix(p.container, container) {
			ps = append(ps, p)
		}
	}
	t.mu.RUnlock()
	out := make([]uploadProgress, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.snapshot())
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

func (s *SecretdataFS) beginUpload(rel, container string, total int64) *uploadRecord {
	return s.uploads.begin(rel, container, total)
}

func (s *SecretdataFS) advanceUpload(p *uploadRecord, bytes int64) {
	s.uploads.advance(p, bytes)
}

func (s *SecretdataFS) endUpload(p *uploadRecord) {
	s.uploads.end(p)
}

func (s *SecretdataFS) uploadSnapshot(rel string) (uploadProgress, bool) {
	return s.uploads.snapshot(rel)
}

func (s *SecretdataFS) uploadsForContainer(container string) []uploadProgress {
	return s.uploads.forContainer(container)
}
