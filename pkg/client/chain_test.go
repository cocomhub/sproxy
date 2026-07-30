// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"testing"
	"time"

	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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
func (r *testChainRunner) SetClient(client *FileClient)      {}
func (r *testChainRunner) SetOptions(opts chainOptions)      {}
func (r *testChainRunner) SetChainManager(mgr *ChainManager) {}

func TestMain(m *testing.M) {
	RegisterRunner("test_chain", func() ChainRunner { return &testChainRunner{} })
	code := m.Run()
	UnregisterRunner("test_chain")
	os.Exit(code)
}

func TestChainManager_Run_Success(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
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
	ctx := t.Context()
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

func TestChainManager_RunWithCancelledContext(t *testing.T) {
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

func TestChainManager_CancelDuringRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	var ran atomic.Bool
	runner := &testChainRunner{
		id:     "test-cancel-during",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn ProgressFunc) error {
			ran.Store(true)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cm.Run(ctx, runner)
	}()

	// 等待 runner 开始运行
	for range 10 {
		if ran.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context cancellation error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Run to return after cancel")
	}
	if !ran.Load() {
		t.Fatal("expected runner to be started")
	}
}

// ---- ChainOption functions ----

func TestWithChainPollInterval(t *testing.T) {
	o := defaultChainOptions()
	opt := WithChainPollInterval(5 * time.Second)
	opt(&o)
	if o.pollInterval != 5*time.Second {
		t.Errorf("pollInterval = %v, want 5s", o.pollInterval)
	}
}

func TestWithChainPollInterval_Zero(t *testing.T) {
	o := defaultChainOptions()
	orig := o.pollInterval
	opt := WithChainPollInterval(0)
	opt(&o)
	if o.pollInterval != orig {
		t.Errorf("pollInterval should remain %v, got %v", orig, o.pollInterval)
	}
}

func TestWithChainTimeout(t *testing.T) {
	o := defaultChainOptions()
	opt := WithChainTimeout(10 * time.Minute)
	opt(&o)
	if o.timeout != 10*time.Minute {
		t.Errorf("timeout = %v, want 10m", o.timeout)
	}
}

func TestWithChainTimeout_Zero(t *testing.T) {
	o := defaultChainOptions()
	orig := o.timeout
	opt := WithChainTimeout(0)
	opt(&o)
	if o.timeout != orig {
		t.Errorf("timeout should remain %v, got %v", orig, o.timeout)
	}
}

func TestWithChainKeepFiles(t *testing.T) {
	o := defaultChainOptions()
	opt := WithChainKeepFiles()
	opt(&o)
	if !o.keepFiles {
		t.Error("expected keepFiles=true")
	}
}

func TestWithChainProgress(t *testing.T) {
	o := defaultChainOptions()
	var called bool
	fn := func(ctx context.Context, info ProgressInfo) {
		called = true
	}
	opt := WithChainProgress(fn)
	opt(&o)
	if o.progressFn == nil {
		t.Fatal("expected progressFn to be set")
	}
	o.progressFn(context.Background(), ProgressInfo{Phase: "test"})
	if !called {
		t.Error("progress callback was not called")
	}
}

// ---- FileClient methods on chain ----

func TestFileClient_CloudDownloadChain_EmptyURLs(t *testing.T) {
	// 空 URL 列表时，CloudDownloadChain 应返回错误
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tasks": []CloudTask{}})
	})
	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ArchiveResult{Success: true, File: "test.tar.gz", Size: 100, Checksum: "abc"})
	})
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	_, err := client.CloudDownloadChain(t.Context(), []string{}, "test", t.TempDir())
	// 空 URL 列表本身不会导致 SubmitError——CloudDownloadBatch 会返回 "urls is required" 错误
	if err == nil {
		t.Fatal("expected error for empty URL list")
	}
}

func TestFileClient_ListChains_NoManager(t *testing.T) {
	client := NewFileClient("http://127.0.0.1:9999")
	states, err := client.ListChains(t.Context())
	if err != nil {
		t.Fatalf("ListChains without manager: %v", err)
	}
	if states != nil {
		t.Fatalf("expected nil, got %v", states)
	}
}

func TestFileClient_DeleteChain_NoManager(t *testing.T) {
	client := NewFileClient("http://127.0.0.1:9999")
	err := client.DeleteChain(t.Context(), "some-id")
	if err != nil {
		t.Fatalf("DeleteChain without manager: %v", err)
	}
}

func TestFileClient_ResumeChain_NoManager(t *testing.T) {
	client := NewFileClient("http://127.0.0.1:9999")
	_, err := client.ResumeChain(t.Context(), "some-id")
	if err == nil {
		t.Fatal("expected error when chainManager is nil")
	}
}

func TestFileClient_ListChains_WithManager(t *testing.T) {
	store := NewMemoryKVStore()
	client := NewFileClient("http://127.0.0.1:9999", WithKVStore(store))
	// 先保存两条活跃链
	store.Save(t.Context(), "chain:active-1", map[string]any{
		"type": "test_chain", "id": "active-1", "phase": "phase1", "status": StatusRunning,
	})
	store.Save(t.Context(), "chain:active-2", map[string]any{
		"type": "test_chain", "id": "active-2", "phase": "phase2", "status": StatusRunning,
	})
	states, err := client.ListChains(t.Context())
	if err != nil {
		t.Fatalf("ListChains: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
}

func TestFileClient_DeleteChain_WithManager(t *testing.T) {
	store := NewMemoryKVStore()
	client := NewFileClient("http://127.0.0.1:9999", WithKVStore(store))
	store.Save(t.Context(), "chain:to-delete", map[string]any{
		"type": "test_chain", "id": "to-delete",
	})
	if err := client.DeleteChain(t.Context(), "to-delete"); err != nil {
		t.Fatalf("DeleteChain: %v", err)
	}
	_, err := store.Load(t.Context(), "chain:to-delete")
	if err == nil {
		t.Fatal("expected chain to be deleted")
	}
}
