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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestTenantEnv 构造测试用租户根解析器 + 租户列表（与生产装配同路径语义）：
// <base>/<owner>/user 为 user 根、<base>/<owner>/meta/sync 为持久化目录；空 owner →
// anonymous 租户。owner 用 storage.ValidSegmentName 校验（fail-closed）；list 扫描
// base 下合法租户目录。外部测试包无法访问 syncmgr 内部 helper，故本地实现。
func newTestTenantEnv(t *testing.T) (base string, resolver syncmgr.TenantRootResolver, list func() []string) {
	t.Helper()
	base = t.TempDir()
	resolver = func(owner string) (string, string, bool) {
		if owner == "" {
			owner = "anonymous"
		}
		if !storage.ValidSegmentName(owner) {
			return "", "", false
		}
		return filepath.Join(base, owner, "user"),
			filepath.Join(base, owner, "meta", "sync"), true
	}
	list = func() []string {
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			if e.IsDir() && storage.ValidSegmentName(e.Name()) && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, e.Name())
			}
		}
		sort.Strings(out)
		return out
	}
	return base, resolver, list
}

// userRootFor 返回 owner 租户 user 根（空 owner → anonymous）。
func userRootFor(base, owner string) string {
	if owner == "" {
		owner = "anonymous"
	}
	return filepath.Join(base, owner, "user")
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
	return syncmgr.RemoteConfig{Name: "r1", URL: srvURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64),
		AccessKeyID: "skey-test-remote"}
}

func waitForStatus(t *testing.T, mgr *syncmgr.Manager, id, want string, timeout time.Duration) *syncmgr.SyncTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task := mgr.Get(id, "")
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
	if task := mgr.Get(id, ""); task != nil {
		cur = task.Status
	}
	t.Fatalf("task %s 未在 %v 内达到 %s，当前 %v", id, timeout, want, cur)
	return nil
}

// TestManager_RealExecutor_Push 通过 Manager 提交 push 任务，验证真实同步落盘到远程。
func TestManager_RealExecutor_Push(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	base, resolver, list := newTestTenantEnv(t)
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "hello push")

	mgr := syncmgr.NewManager(resolver, list, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(resolver, discardLogger()), discardLogger(),
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
	base, resolver, list := newTestTenantEnv(t)

	mgr := syncmgr.NewManager(resolver, list, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(resolver, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "completed", 10*time.Second)
	if got := readLocalFile(t, userRootFor(base, ""), "local/r.txt"); got != "remote content" {
		t.Fatalf("本地内容不符: %q", got)
	}
}

// TestManager_RealExecutor_Cancel 通过 Manager 取消执行中的真实同步任务。
func TestManager_RealExecutor_Cancel(t *testing.T) {
	// GET /api/files 阻塞，使 pull 任务停在枚举阶段（syncing）。
	// execStarted 是「executor 已进入远程枚举」的确定性信号：blocking GET /api/files
	// handler 首次被调用即 close，替代 waitForStatus("syncing") 固定轮询（死等必然 flake）。
	blockCh := make(chan struct{})
	execStarted := make(chan struct{})
	srv := newBlockingServer(t, newBlockingListMux(blockCh, execStarted))
	_, resolver, list := newTestTenantEnv(t)

	mgr := syncmgr.NewManager(resolver, list, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(resolver, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)
	t.Cleanup(func() { close(blockCh) })

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{Direction: "pull", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	// 确定性等待 executor 开始枚举（等价于任务进入 syncing），而非固定 5s 死等。
	select {
	case <-execStarted:
	case <-time.After(10 * time.Second):
		if st := mgr.Get(task.ID, ""); st != nil {
			t.Fatalf("executor 未在 10s 内开始枚举；任务状态 = %q", st.Status)
		}
		t.Fatal("executor 未在 10s 内开始枚举；任务不存在")
	}
	if err := mgr.CancelTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "cancelled", 10*time.Second)
}

// TestSync_NewLayout 验证多租户存储布局迁移（P3 任务 15）：创建 pull 任务 owner=alice
// → 拉取文件落 <root>/alice/user/<dst>（user 桶，非 alice 根）、任务状态落
// <root>/alice/meta/sync/<taskID>.json（meta/sync 桶，非 uploadsDir/.__sync__/）。
func TestSync_NewLayout(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("sub/r.txt", "remote content")
	remote.SeedDir("sub")
	base, resolver, list := newTestTenantEnv(t)

	mgr := syncmgr.NewManager(resolver, list, &memQuota{}, 0, []syncmgr.RemoteConfig{remoteConfig(srv.URL)},
		syncexec.NewExecutor(resolver, discardLogger()), discardLogger(),
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{
		Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, Owner: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "completed", 10*time.Second)

	// 拉取文件落 <base>/alice/user/local/r.txt（user 桶），不在 alice 根直接下
	userRoot := filepath.Join(base, "alice", "user")
	if got := readLocalFile(t, userRoot, "local/r.txt"); got != "remote content" {
		t.Fatalf("alice/user 桶下内容不符: %q", got)
	}
	if _, err := os.Stat(filepath.Join(base, "alice", "local", "r.txt")); err == nil {
		t.Fatalf("文件不应落在 alice 根（非 user 桶）: 隔离失败")
	}

	// 任务状态落 <base>/alice/meta/sync/<taskID>.json
	persistFile := filepath.Join(base, "alice", "meta", "sync", task.ID+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("任务状态应落 <tenant>/meta/sync/<id>.json: %v", err)
	}
}

// newBlockingListMux 返回 GET /api/files 阻塞直到 blockCh 关闭或 ctx 取消的 mux。
// hitCh（可 nil）在 handler 被调用时 close 一次——这是 executor 已开始枚举（ListDir
// 已进入任务执行路径）的**确定性信号**，供 TestManager_RealExecutor_Cancel 用
// select 等待替代固定 waitForStatus("syncing") 轮询（CI -race+cover 下死等必然偶发超时）。
func newBlockingListMux(blockCh chan struct{}, hitCh chan struct{}) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		if hitCh != nil {
			select {
			case <-hitCh:
			default:
				close(hitCh) // 首次调用即报到（仅 close 一次）
			}
		}
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
