// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fakeAuditor 是可编排的审计实现（可阻塞/可失败/可计数）。
type fakeAuditor struct {
	calls  atomic.Int64
	block  chan struct{}
	result syncpkg.OrphanReport
	err    error
}

// OrphanChunks 返回编排结果；block 非 nil 时阻塞到其关闭。
func (f *fakeAuditor) OrphanChunks(context.Context) (syncpkg.OrphanReport, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.result, f.err
}

// TestOrphanAuditJob_AsyncLifecycle 锁定后台任务生命周期：running → done，且结果被缓存。
func TestOrphanAuditJob_AsyncLifecycle(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	a := &fakeAuditor{block: block, result: syncpkg.OrphanReport{Containers: 3,
		Chunks: []syncpkg.OrphanBlob{{Container: "c1", Name: "n1"}}}}
	job := &orphanAuditJob{}
	job.start(a, nil)
	if st := job.snapshot(); !st.running {
		t.Fatal("启动后应处于 running")
	}
	close(block)
	if !job.waitDone(5 * time.Second) {
		t.Fatal("后台任务未在 5s 内完成")
	}
	st := job.snapshot()
	if st.running || st.err != "" {
		t.Fatalf("应完成且无错误: running=%v err=%s", st.running, st.err)
	}
	if len(st.report.Chunks) != 1 || st.report.Containers != 3 {
		t.Fatalf("结果未缓存: %+v", st.report)
	}
}

// TestOrphanAuditJob_SingleFlight 锁定「同卷单飞」：运行期间再次 start 不得重复执行。
func TestOrphanAuditJob_SingleFlight(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	a := &fakeAuditor{block: block}
	job := &orphanAuditJob{}
	job.start(a, nil)
	job.start(a, nil) // 运行中：应被忽略
	job.start(a, nil)
	close(block)
	if !job.waitDone(5 * time.Second) {
		t.Fatal("任务未结束")
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("运行期间重复 start 不得并发执行（calls=%d）", got)
	}
}

// TestOrphanAuditJob_FailureRecorded 锁定失败语义：错误写入状态（供 503 返回），且不再 running。
func TestOrphanAuditJob_FailureRecorded(t *testing.T) {
	t.Parallel()
	a := &fakeAuditor{err: errors.New("index not ready")}
	job := &orphanAuditJob{}
	job.start(a, nil)
	if !job.waitDone(5 * time.Second) {
		t.Fatal("任务未结束")
	}
	if st := job.snapshot(); st.running || st.err == "" {
		t.Fatalf("失败必须记录错误信息: running=%v err=%q", st.running, st.err)
	}
}
