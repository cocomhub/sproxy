// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 外部测试包：通过 Manager + 真实执行器（pkg/syncexec → pkg/sync）做端到端同步，
// 验证任务生命周期与同步引擎的完整集成。
package syncmgr_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memQuota 实现 syncmgr.QuotaStore（外部测试无法访问 syncmgr 内部 mock）。
type memQuota struct {
	mu   sync.Mutex
	used int64
}

func (q *memQuota) TryReserve(size int64, _ int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used += size
	return nil
}

func (q *memQuota) Release(size int64, _ int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used -= size
	if q.used < 0 {
		q.used = 0
	}
}

func (q *memQuota) Usage() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

func (q *memQuota) MaxBytes() int64 { return 0 }

func writeLocalFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readLocalFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取本地文件 %s 失败: %v", rel, err)
	}
	return string(data)
}

func remoteConfig(srvURL string) syncmgr.RemoteConfig {
	return syncmgr.RemoteConfig{Name: "r1", URL: srvURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64)}
}

func waitForStatus(t *testing.T, mgr *syncmgr.Manager, id, want string, timeout time.Duration) *syncmgr.SyncTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task := mgr.Get(id)
		if task == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if task.Status == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	cur := "<deleted>"
	if task := mgr.Get(id); task != nil {
		cur = task.Status
	}
	t.Fatalf("task %s 未在 %v 内达到 %s，当前 %v", id, timeout, want, cur)
	return nil
}

// TestManager_RealExecutor_Push 通过 Manager 提交 push 任务，验证真实同步落盘到远程。
func TestManager_RealExecutor_Push(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	dir := t.TempDir()
	writeLocalFile(t, dir, "a.txt", "hello push")

	mgr := syncmgr.NewManager(dir, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(dir, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{Direction: "push", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, mgr, task.ID, "completed", 10*time.Second)
	if done.FilesDone != 1 || done.BytesDone != int64(len("hello push")) {
		t.Fatalf("进度不符: files=%d bytes=%d", done.FilesDone, done.BytesDone)
	}
	files := remote.SnapshotFiles()
	if f, ok := files["a.txt"]; !ok || string(f.Data) != "hello push" {
		t.Fatalf("远程应存在 a.txt: %+v", files)
	}
}

// TestManager_RealExecutor_Pull 通过 Manager 提交 pull 任务，验证真实同步落盘到本地。
func TestManager_RealExecutor_Pull(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("sub/r.txt", "remote content")
	remote.SeedDir("sub")
	dir := t.TempDir()

	mgr := syncmgr.NewManager(dir, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(dir, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "completed", 10*time.Second)
	if got := readLocalFile(t, dir, "local/r.txt"); got != "remote content" {
		t.Fatalf("本地内容不符: %q", got)
	}
}

// TestManager_RealExecutor_Cancel 通过 Manager 取消执行中的真实同步任务。
func TestManager_RealExecutor_Cancel(t *testing.T) {
	// GET /api/files 阻塞，使 pull 任务停在枚举阶段（syncing）
	blockCh := make(chan struct{})
	srv := newBlockingServer(t, newBlockingListMux(blockCh))
	dir := t.TempDir()

	mgr := syncmgr.NewManager(dir, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(dir, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)
	t.Cleanup(func() { close(blockCh) })

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{Direction: "pull", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "syncing", 5*time.Second)
	if err := mgr.CancelTask(task.ID); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "cancelled", 10*time.Second)
}

// newBlockingListMux 返回 GET /api/files 阻塞直到 blockCh 关闭或 ctx 取消的 mux。
func newBlockingListMux(blockCh chan struct{}) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-blockCh:
		}
		http.Error(w, "aborted", http.StatusGatewayTimeout)
	})
	return mux
}

// newBlockingServer 启动一个由 mux 驱动的 httptest.Server（t.Cleanup 自动关闭）。
func newBlockingServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
