// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"errors"
	"testing"
)

func TestScope_TryReserveCommitRelease(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t/alice", 50)
	res, err := s.TryReserve(30) // 预留 30
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := s.Reserved(); got != 30 {
		t.Fatalf("Reserved=%d want 30", got)
	}
	res.Commit(25) // 实际 25：reserved 25→committed 25，多预留 5 归还
	if got := s.Usage(); got != 25 {
		t.Fatalf("Usage=%d want 25", got)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
	if got := root.Usage(); got != 25 {
		t.Fatalf("root Usage=%d want 25（父链聚合）", got)
	}
	s.ReleaseUsage(10) // 删除文件释放 10
	if got := s.Usage(); got != 15 {
		t.Fatalf("Usage=%d want 15", got)
	}
}

func TestScope_Adjust(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	s.Adjust(10, 20) // 覆盖写尺寸 10→20，diff +10
	if got := s.Usage(); got != 20 {
		t.Fatalf("Usage=%d want 20", got)
	}
	s.Adjust(20, 5) // 缩小 diff -15
	if got := s.Usage(); got != 5 {
		t.Fatalf("Usage=%d want 5", got)
	}
}

func TestScope_QuotaExceeded(t *testing.T) {
	root := NewPool(10)      // 全局兜底 10
	s := root.Scope("/t", 8) // 租户上限 8
	if _, err := s.TryReserve(9); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("租户上限应拒绝, got %v", err)
	}
	if _, err := root.Scope("/t2", 5).TryReserve(9); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("全局兜底应拒绝（8+9=17>10）, got %v", err)
	}
	if got := s.Available(); got != 8 {
		t.Fatalf("Available=%d want 8", got)
	}
}

func TestScope_ReleaseReservation(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 50)
	res, _ := s.TryReserve(40)
	res.Release() // 失败取消预留
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
	if got := s.Available(); got != 50 {
		t.Fatalf("Available=%d want 50", got)
	}
}

func TestScope_UsageByBucket(t *testing.T) {
	root := NewPool(1000)
	t1 := root.Scope("/tenant/a", 500)
	t1.Scope("/user").Adjust(0, 100) // 子桶 user 占用 100
	t1.Scope("/cloud").Adjust(0, 50) // 子桶 cloud 占用 50
	m := root.UsageByBucket()
	if m["/tenant/a/user"] != 100 || m["/tenant/a/cloud"] != 50 {
		t.Fatalf("UsageByBucket=%v", m)
	}
	if got := t1.Usage(); got != 150 {
		t.Fatalf("tenant Usage=%d want 150", got)
	}
}
