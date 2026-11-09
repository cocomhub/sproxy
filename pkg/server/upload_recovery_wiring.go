// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// upload_recovery_wiring.go —— **G1**：把上传内核（`uploadcore`）的「崩溃遗留未完成上传会话」
// 接入 M3 统一恢复协调器（唯一触发点仍是 `pkg/server/recovery_wiring.go`）。
//
// 为什么走 `RegisterLegacy` 而不是 `Register`（与 `uploadcore/recovery.go` 的设计决策一致）：
// `Register`（ApplierFactory）会在重放后**立即 MarkDone**；而上传是**业务触发**的动作，
// 启动时立刻续传大文件会占用带宽、且 MarkDone 会清掉「待续传状态」⇒ 真正上传时无法续传。
// 因此这里**只枚举未完成作用域并把恢复态交给 sink**（不 MarkDone）：
//   - 启动恢复的**可见性**（日志/计数）由此获得；
//   - **真正的续传**发生在下次上传同一文件时 —— `uploadcore_wiring.uploadCoreWAL` 调
//     `uploadcore.ResumeScope` 读回 upload_id + 已完成分片，跳过已传分片（`Resume *ScopeState`）；
//   - 恢复期**不发任何网络请求**（避免启动风暴，设计 §4.2）。

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"sync"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/recovery"
	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// uploadResumeSink 记录启动扫描到的待续传上传会话（**只登记，不触发续传**）。
//
// 并发安全：扫描可能来自协调器的多个 legacy 入口/重复启动 ⇒ 全部经 mu 串行；
// 对外只提供**深拷贝**快照（不暴露内部 map）。
type uploadResumeSink struct {
	mu    sync.Mutex
	items map[audit.ScopeID]uploadcore.ScopeState
	log   *slog.Logger
}

// newUploadResumeSink 构造 sink（log 为 nil ⇒ 用丢弃 logger，避免调用点判空）。
func newUploadResumeSink(log *slog.Logger) *uploadResumeSink {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &uploadResumeSink{items: map[audit.ScopeID]uploadcore.ScopeState{}, log: log}
}

// PutResume 登记一条待续传项（**幂等**：同 scope 覆盖，不产生重复项）。
// st == nil 表示该作用域当前无需续传（`uploadcore.scopeStateFromKV` 的约定）⇒ 不登记。
func (s *uploadResumeSink) PutResume(_ context.Context, scope audit.ScopeID, st *uploadcore.ScopeState) error {
	if s == nil || st == nil {
		return nil
	}
	cp := *st
	cp.PartsDone = maps.Clone(st.PartsDone)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[scope] = cp
	s.log.Info("上传续传：发现未完成会话（将在下次上传同一文件时续传）",
		"scope", string(scope), "upload_id", cp.UploadID, "parts_done", len(cp.PartsDone))
	return nil
}

// Len 返回登记的待续传项数。
func (s *uploadResumeSink) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Snapshot 返回深拷贝快照（测试与后续指标使用）。
func (s *uploadResumeSink) Snapshot() map[audit.ScopeID]uploadcore.ScopeState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[audit.ScopeID]uploadcore.ScopeState, len(s.items))
	for k, v := range s.items {
		v.PartsDone = maps.Clone(v.PartsDone)
		out[k] = v
	}
	return out
}

// uploadLegacies 返回上传内核的 legacy 恢复入口。
//
// 守卫（缺一即**不注册**，避免启动恢复出现无意义失败项）：审计服务、sink 都必须装配
// —— 无审计就没有 WAL 作用域可扫描（`uploadcore` 此时用空 WAL，本就没有跨进程续传）。
func uploadLegacies(h *Handlers, sink *uploadResumeSink) []recovery.Legacy {
	if h == nil || h.auditSvc == nil || sink == nil {
		return nil
	}
	return []recovery.Legacy{uploadcore.NewLegacyRecovery(h.auditSvc, sink)}
}
