// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// upload_progress_registry.go —— **服务端写入阶段进度**的进程内注册表（进度可视化二期 T4，2026-11-10）。
//
// 用途：把卷层上报的阶段进度（加密 `encrypt` / 远端卷上传 `remote`，见 `pkg/sync` 的
// `UploadProgressFrom`）按**逻辑路径**暂存，供 `GET /upload/status?filename=` 读出，
// 从而让客户端（sdserver 进度页）能看到「服务端加密 / 远端卷上传」两个阶段推进。
//
// 设计约束（重要）：
//   - **只做可观测**：注册表**不参与**写入逻辑（失败/缺失绝不影响上传），符合本仓「进度是旁路」的定位；
//   - **进程内 + TTL**：条目在最后一次上报后 `uploadProgressTTL` 过期自动淘汰（防长期占用）；
//   - **并发安全**：卷层块级并发会上报多次 ⇒ 全部经 mu 保护；`done` 取**最大值**（乱序安全）；
//   - **不用包级全局变量做注入 seam**：注册表挂在 `Service` 上（每实例一份），测试可各自构造。

import (
	"context"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// uploadProgressTTL 是进度条目在**最后一次上报**后的存活时长。
// 取 30 分钟：足够覆盖一个大文件的服务端写入（加密+远端），又不会长期堆积。
const uploadProgressTTL = 30 * time.Minute

// uploadStageProgress 是单条路径的阶段进度快照（JSON 友好，直接映射到 ChunkStatusResponse 字段）。
type uploadStageProgress struct {
	Stage        string
	EncryptDone  int64
	EncryptTotal int64
	RemoteDone   int64
	RemoteTotal  int64
	UpdatedAt    time.Time
}

// uploadProgressRegistry 是按逻辑路径索引的阶段进度注册表（并发安全 + TTL 淘汰）。
type uploadProgressRegistry struct {
	mu    sync.Mutex
	items map[string]*uploadStageProgress
	// now 可注入（测试用固定时钟，无需 sleep）。
	now func() time.Time
}

func newUploadProgressRegistry() *uploadProgressRegistry {
	return &uploadProgressRegistry{items: map[string]*uploadStageProgress{}, now: time.Now}
}

// fnFor 返回**上报回调**：把某逻辑路径的阶段进度写入注册表。
// `stage` 决定写入哪一组计数器；`done` 取最大值（并发乱序安全）；`total>0` 才更新总量。
func (r *uploadProgressRegistry) fnFor(key string) syncpkg.UploadProgressFn {
	return func(stage syncpkg.UploadStage, done, total int64) {
		if r == nil || key == "" {
			return
		}
		if done < 0 {
			done = 0
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.items[key]
		if e == nil {
			e = &uploadStageProgress{}
			r.items[key] = e
		}
		if !e.applyStage(stage, done, total) {
			return // 未知阶段：忽略（保持受控枚举）
		}
		e.UpdatedAt = r.now()
		r.evictExpiredLocked()
	}
}

// get 读取某逻辑路径的阶段进度（已过期/不存在 ⇒ false）。
func (r *uploadProgressRegistry) get(key string) (uploadStageProgress, bool) {
	if r == nil || key == "" {
		return uploadStageProgress{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.items[key]
	if e == nil {
		return uploadStageProgress{}, false
	}
	if r.now().Sub(e.UpdatedAt) > uploadProgressTTL {
		delete(r.items, key)
		return uploadStageProgress{}, false
	}
	return *e, true
}

// applyStage 把一次上报写入条目；未知 stage ⇒ 返回 false（由调用方忽略）。
// 抽出独立方法：一是让 fnFor 认知复杂度达标（gocognit ≤15），二是把"阶段语义"集中一处。
func (e *uploadStageProgress) applyStage(stage syncpkg.UploadStage, done, total int64) bool {
	switch stage {
	case syncpkg.StageEncrypt:
		if done > e.EncryptDone {
			e.EncryptDone = done
		}
		if total > 0 {
			e.EncryptTotal = total
		}
		e.Stage = string(syncpkg.StageEncrypt)
	case syncpkg.StageRemote:
		if done > e.RemoteDone {
			e.RemoteDone = done
		}
		if total > 0 {
			e.RemoteTotal = total
		}
		e.Stage = string(syncpkg.StageRemote)
	default:
		return false
	}
	return true
}

// evictExpiredLocked 淘汰过期条目（调用方须持锁）。条目数通常等于"同时在写的文件数"，规模很小；
// 仍做线性清理以保证长期运行不堆积。
func (r *uploadProgressRegistry) evictExpiredLocked() {
	now := r.now()
	for k, e := range r.items {
		if now.Sub(e.UpdatedAt) > uploadProgressTTL {
			delete(r.items, k)
		}
	}
}

// progressRegistry 惰性返回进度注册表（避免改动既有构造函数；Once 保证并发安全）。
func (s *Service) progressRegistry() *uploadProgressRegistry {
	if s == nil {
		return nil
	}
	s.uploadProgressOnce.Do(func() {
		if s.uploadProgress == nil {
			s.uploadProgress = newUploadProgressRegistry()
		}
	})
	return s.uploadProgress
}

// withUploadProgress 若 ctx 未装配进度回调，则注入「写入注册表」的回调（key 为逻辑路径 rel）。
// 已装配（上层自带）⇒ **原样返回，不覆盖**（避免抢占更细的装配）。
func (s *Service) withUploadProgress(ctx context.Context, key string) context.Context {
	if _, ok := syncpkg.UploadProgressFrom(ctx); ok {
		return ctx
	}
	reg := s.progressRegistry()
	if reg == nil {
		return ctx
	}
	return syncpkg.WithUploadProgress(ctx, reg.fnFor(key))
}

// fillStageProgress 把注册表里的写入阶段进度填入响应（命中任一候选 key 即填；**不猜、不造**）。
// 候选顺序按"更可能精确匹配"排列：会话 Filename 优先，其次 URL 上的 filename。
func (s *Service) fillStageProgress(resp *ChunkStatusResponse, candidates ...string) {
	if resp == nil {
		return
	}
	reg := s.progressRegistry()
	if reg == nil {
		return
	}
	for _, k := range candidates {
		if k == "" {
			continue
		}
		if e, ok := reg.get(k); ok {
			resp.Stage = e.Stage
			resp.EncryptDone, resp.EncryptTotal = e.EncryptDone, e.EncryptTotal
			resp.RemoteDone, resp.RemoteTotal = e.RemoteDone, e.RemoteTotal
			return
		}
	}
}
