// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// TaskFailure 是一次任务失败的通知载荷（供装配层登记事项/告警）。
//
// 为什么放在 pkg/cloud：领域层不认识事项中心（分层 R6），只产出**事实**；
// 由装配层（cmd/sproxy）注入 `OnTaskFailed` 把事实翻译成事项。
type TaskFailure struct {
	TaskID string
	Owner  string
	URL    string
	// Volume/Path 是转存目标（空 = 无转存目标，事项登记时需按 URL 处理）。
	Volume string
	Path   string
	// Error 是失败原文；ErrorClass 见 ErrClass*（决定重试策略）。
	Error      string
	ErrorClass string
	// Attempts 是累计失败次数（诊断快照）。
	Attempts int
}

// TaskFailureObserver 是任务失败观察者（装配层注入；nil = 不通知）。
type TaskFailureObserver func(TaskFailure)

// SetTaskFailureObserver 后期注入失败观察者（装配层在事项中心就绪后调用；幂等）。
//
// 为什么允许后置（2026-11-21）：manager 在 `routes_setup` 构造，而事项中心要到
// `cmd/sproxy/root.go` 才就绪（且与卷/审计装配同序）。后置注入避免把装配顺序硬绑死。
func (m *CloudDownloadManager) SetTaskFailureObserver(fn TaskFailureObserver) {
	m.failureMu.Lock()
	m.failureObserver = fn
	m.failureMu.Unlock()
}

// observeFailure 把失败事实交给观察者（在 failTask 释放锁后调用，避免回调持锁）。
func (m *CloudDownloadManager) observeFailure(t *CloudTask) {
	m.failureMu.RLock()
	obs := m.failureObserver
	m.failureMu.RUnlock()
	if obs == nil {
		obs = m.config.OnTaskFailed
	}
	if obs == nil {
		return
	}
	f := TaskFailure{TaskID: t.ID, Owner: t.Owner, URL: t.URL, Error: t.Error}
	if t.Transfer != nil {
		f.Volume, f.Path = t.Transfer.Volume, t.Transfer.Path
	}
	d := t.Diag()
	f.ErrorClass, f.Attempts = d.LastErrorClass, d.Attempts
	obs(f)
}
