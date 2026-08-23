// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestConcurrentFileOperations 测试并发 sclient 命令操作无竞态问题。
func TestConcurrentFileOperations(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	// 使用 error 收集而非直接 t.Fatalf（goroutine 中 t.Fatal 不安全）
	errCh := make(chan error, 10)
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
			cmd := NewCmdStat(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, cfgSvc)
			cmd.SetArgs(nil)
			if err := cmd.Execute(); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent stat failed: %v", err)
	}
}

// TestConcurrentDownload 测试并发下载命令无竞态问题。
func TestConcurrentDownload(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
		w.Header().Set("X-File-MTime", "0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(mock.Close)

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}

	// Pre-create temp dirs for concurrent download (t.TempDir() not goroutine-safe)
	dirs := make([]string, 5)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 5)
	for i := range 5 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			outPath := filepath.Join(dirs[n], "out.txt")
			cmd := NewCmdDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
			cmd.SetArgs([]string{"test.txt", outPath})
			if err := cmd.Execute(); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent download failed: %v", err)
	}
}
