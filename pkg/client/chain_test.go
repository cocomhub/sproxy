// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"maps"
	"sync/atomic"
	"testing"
)

// testChainRunner 用于测试的简单 ChainRunner 实现。
type testChainRunner struct {
	id       string
	phase    string
	status   string
	runFn    func(ctx context.Context, reportFn ProgressFunc) error
	stateMap map[string]any
}

func (r *testChainRunner) ID() string     { return r.id }
func (r *testChainRunner) Phase() string  { return r.phase }
func (r *testChainRunner) Status() string { return r.status }
func (r *testChainRunner) Run(ctx context.Context, reportFn ProgressFunc) error {
	if r.runFn != nil {
		return r.runFn(ctx, reportFn)
	}
	return nil
}
func (r *testChainRunner) State() map[string]any {
	state := map[string]any{
		"type":   "test_chain",
		"id":     r.id,
		"phase":  r.phase,
		"status": r.status,
	}
	maps.Copy(state, r.stateMap)
	return state
}
func (r *testChainRunner) Restore(state map[string]any) error {
	r.id, _ = state["id"].(string)
	r.phase, _ = state["phase"].(string)
	r.status, _ = state["status"].(string)
	return nil
}

func init() {
	RegisterRunner("test_chain", func() ChainRunner { return &testChainRunner{} })
}

func TestChainManager_Run_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	runner := &testChainRunner{
		id:     "test-run-1",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn ProgressFunc) error {
			reportFn(ctx, ProgressInfo{Phase: "phase1", Message: "doing work", Current: 1, Total: 2})
			return nil
		},
	}

	if err := cm.Run(ctx, runner); err != nil {
		t.Fatal(err)
	}

	_, err := store.Load(ctx, "chain:test-run-1")
	if err == nil {
		t.Fatal("expected cache to be deleted after successful run")
	}
}

func TestChainManager_Run_Failure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	expectedErr := errors.New("something went wrong")
	runner := &testChainRunner{
		id:     "test-run-2",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn ProgressFunc) error {
			return expectedErr
		},
	}

	err := cm.Run(ctx, runner)
	if err == nil {
		t.Fatal("expected error")
	}

	state, err := store.Load(ctx, "chain:test-run-2")
	if err != nil {
		t.Fatal("expected cache to be preserved after failed run")
	}
	if status, _ := state["status"].(string); status != StatusFailed {
		t.Errorf("expected status=failed, got %s", status)
	}
}

func TestChainManager_Resume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	store.Save(ctx, "chain:test-resume", map[string]any{
		"type": "test_chain", "id": "test-resume", "phase": "phase2", "status": StatusRunning,
	})

	runner, err := cm.Resume(ctx, "test-resume")
	if err != nil {
		t.Fatal(err)
	}
	if runner.Phase() != "phase2" {
		t.Errorf("expected phase2, got %s", runner.Phase())
	}
}

func TestChainManager_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	store.Save(ctx, "chain:active1", map[string]any{
		"type": "test_chain", "id": "active1", "phase": "phase1", "status": StatusRunning,
	})
	store.Save(ctx, "chain:active2", map[string]any{
		"type": "test_chain", "id": "active2", "phase": "phase2", "status": StatusRunning,
	})
	store.Save(ctx, "chain:done1", map[string]any{
		"type": "test_chain", "id": "done1", "phase": PhaseCompleted, "status": StatusCompleted,
	})

	runners, err := cm.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 2 {
		t.Fatalf("expected 2 active runners, got %d", len(runners))
	}
}

func TestChainManager_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	store.Save(ctx, "chain:todelete", map[string]any{
		"type": "test_chain", "id": "todelete",
	})

	if err := cm.Delete(ctx, "todelete"); err != nil {
		t.Fatal(err)
	}

	_, err := store.Load(ctx, "chain:todelete")
	if err == nil {
		t.Fatal("expected cache to be deleted")
	}
}

func TestChainManager_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	var ran atomic.Bool
	runner := &testChainRunner{
		id:     "test-cancel",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn ProgressFunc) error {
			ran.Store(true)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	cancel()
	err := cm.Run(ctx, runner)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !ran.Load() {
		t.Fatal("expected runner to be started")
	}
}
