// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestLocalCoordinator_AllowWithinLimit(t *testing.T) {
	t.Parallel()
	c := newLocalCoordinator(3, time.Second)
	for i := range 3 {
		if !c.Allow("k", 1) {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if c.Allow("k", 1) {
		t.Fatal("4th call should be rejected")
	}
}

func TestLocalCoordinator_KeyIsolated(t *testing.T) {
	t.Parallel()
	c := newLocalCoordinator(1, time.Second)
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

func TestLocalCoordinator_Reset(t *testing.T) {
	t.Parallel()
	c := newLocalCoordinator(1, 50*time.Millisecond)
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

func TestLocalCoordinator_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	c := newLocalCoordinator(1000, time.Second)
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
