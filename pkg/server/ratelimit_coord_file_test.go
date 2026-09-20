// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestFileCoordinator_CrossProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c1 := newFileCoordinator(3, time.Minute, dir, testutil.DiscardLogger())
	c2 := newFileCoordinator(3, time.Minute, dir, testutil.DiscardLogger())
	// c1 消耗 3 次配额（跨实例共享：同一 key 同一计数文件）。
	for i := range 3 {
		if !c1.Allow("shared", 1) {
			t.Fatalf("c1 call %d should be allowed", i)
		}
	}
	// c2 第 4 次应被拒（共享 limit=3）。
	if c2.Allow("shared", 1) {
		t.Fatal("c2 4th call should be rejected (shared limit)")
	}
}

func TestFileCoordinator_KeyIsolated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newFileCoordinator(1, time.Minute, dir, testutil.DiscardLogger())
	if !c.Allow("a", 1) {
		t.Fatal("a:1 should pass")
	}
	if c.Allow("a", 1) {
		t.Fatal("a:2 should be rejected")
	}
	if !c.Allow("b", 1) {
		t.Fatal("b:1 should pass (independent key)")
	}
}

func TestFileCoordinator_AtomicUnderRace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newFileCoordinator(1000, time.Minute, dir, testutil.DiscardLogger())
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for range 200 {
		wg.Go(func() {
			if c.Allow("k", 1) {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got > 1000 {
		t.Fatalf("allowed %d exceeds limit 1000", got)
	}
}

func TestFileCoordinator_WindowExpiry(t *testing.T) {
	t.Parallel()
	// window 用 2s（而非 500ms）：Windows 上文件锁/IO 较慢 + -race 下运行慢 2-3 倍，
	// 两次连续 Allow 间隔可能超过 500ms → 第二次被误判为新窗口放行（CI Test (windows)
	// 实证 2026-09-20：second call must be rejected）。2s 给足余量（窗口滑动验证的
	// WaitForBool 超时 30s 远大于 2s），同时仍验证窗口滑动后放行语义。
	dir := t.TempDir()
	c := newFileCoordinator(1, 2*time.Second, dir, testutil.DiscardLogger())
	if !c.Allow("k", 1) {
		t.Fatal("first call must pass")
	}
	if c.Allow("k", 1) {
		t.Fatal("second call must be rejected (still within window)")
	}
	if !testutil.WaitForBool(30*time.Second, func() bool { return c.Allow("k", 1) }) {
		t.Fatal("call after window slide should be allowed")
	}
}

func TestFileCoordinator_SanitizeKey(t *testing.T) {
	t.Parallel()
	// 路径穿越形状必须被归一（不产生越界路径）。
	if got := sanitizeKey("../etc/passwd"); got != "_._etc_passwd" {
		t.Fatalf("sanitize ../etc/passwd = %q, want _._etc_passwd", got)
	}
	if got := sanitizeKey("192.168.1.1"); got != "192.168.1.1" {
		t.Fatalf("sanitize ip = %q, want 192.168.1.1", got)
	}
	// 空 key 落安全名。
	if got := sanitizeKey(""); got != "_" {
		t.Fatalf("sanitize empty = %q, want _", got)
	}
}

func TestFileCoordinator_InvalidBackend(t *testing.T) {
	t.Parallel()
	// 未知后端 → 装配错误（由调用方决定回退 local）。
	c, err := newCoordinator("unknown", 5, time.Second, t.TempDir(), testutil.DiscardLogger())
	if err == nil {
		t.Fatal("unknown backend should error")
	}
	if c != nil {
		t.Fatalf("unknown backend should return nil coordinator, got %T", c)
	}
	// file 后端缺 dir → 错误。
	if _, err := newCoordinator("file", 5, time.Second, "", testutil.DiscardLogger()); err == nil {
		t.Fatal("file backend with empty dir should error")
	}
	// local 默认（空 backend）→ 成功。
	if _, err := newCoordinator("", 5, time.Second, "", testutil.DiscardLogger()); err != nil {
		t.Fatalf("empty backend should default to local: %v", err)
	}
}

func TestFileCoordinator_MkdirLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newFileCoordinator(5, time.Minute, dir, testutil.DiscardLogger())
	if !c.Allow("k", 1) {
		t.Fatal("first call should pass")
	}
	// 计数文件落在 <dir>/ratelimit/<key>。
	if _, err := filepath.Glob(filepath.Join(dir, "ratelimit", "k")); err != nil {
		t.Fatalf("glob ratelimit/k: %v", err)
	}
}
