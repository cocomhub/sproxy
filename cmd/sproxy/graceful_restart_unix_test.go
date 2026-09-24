// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// graceful_restart_unix_test.go 钉住优雅重启（roadmap 11.6-②，kill -USR2）：
//  1. USR2 走 handleSignalRestart 而非 handleSignalShutdown（变异：USR2 误走 shutdown → 红）。
//  2. 编排：子进程就绪前旧进程不 drain；就绪后才 drain（变异：删就绪等待直接 drain → 红）。
//  3. 超时中止：readyz 永不就绪 → 旧进程存活未 drain（变异：超时仍 drain → 红）。
//  4. fd 继承：真实 listener 经 ExtraFiles 传给 helper 子进程 → FileListener 可 accept
//     且地址一致（变异：子进程回退 net.Listen → EADDRINUSE → 红）。
//  5. 零回归：SIGTERM 仍走 handleSignalShutdown（既有语义不动）。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestGracefulRestart_StartChildAndFileListener 验证 fd 继承：
// 真实 TCPListener 经 ExtraFiles 传给 helper 子进程，子进程用 net.FileListener
// 重建后 accept 成功且地址一致（变异：子进程回退 net.Listen → EADDRINUSE → 红）。
func TestGracefulRestart_StartChildAndFileListener(t *testing.T) {
	// sproxy:serial: 启动 helper 子进程 + 绑定真实端口（包级 restartListener 无关，但
	// 子进程调度与既有 runServer 用例互斥，串行降低包并行度）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	cmd, err := startRestartChild(ln)
	if err != nil {
		t.Fatalf("startRestartChild: %v", err)
	}
	_ = cmd // 本用例直接用 exec.Command 手动构造 helper（见下）
	// helper 子进程用同仓库源码编译的二进制运行（见 helper 下方）。
	exe := buildHelperBinary(t)
	helper := exec.Command(exe, addr)
	lnFile, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("ln.File: %v", err)
	}
	defer lnFile.Close()
	helper.ExtraFiles = []*os.File{lnFile}
	helper.Env = append(os.Environ(), restartEnvKey+"="+strconv.Itoa(restartFd))
	// 子进程输出写互斥 buffer（exec 内部 goroutine 写、主 goroutine 读 → -race 必须加锁）。
	var outMu sync.Mutex
	out := new(strings.Builder)
	writeFn := func(p []byte) (int, error) {
		outMu.Lock()
		defer outMu.Unlock()
		return out.Write(p)
	}
	helper.Stdout = writerFunc(writeFn)
	helper.Stderr = writerFunc(writeFn)
	if err := helper.Start(); err != nil {
		t.Fatalf("helper.Start: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()
	// helper accept 并回显地址。
	if err := waitForHelperAccept(&outMu, out, helper); err != nil {
		t.Fatalf("helper accept 失败: %v（输出: %s）", err, out.String())
	}
	outMu.Lock()
	got := out.String()
	outMu.Unlock()
	if !strings.Contains(got, addr) {
		t.Errorf("helper 应从继承 fd 重建同一地址 %q，实际输出: %s", addr, got)
	}
}

// writerFunc 把 func([]byte)(int,error) 适配为 io.Writer（exec 子进程输出用）。
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// waitForHelperAccept 等待 helper 子进程 accept 成功并输出（有界轮询）。
// 子进程输出由 exec 内部 goroutine 写、本函数读 → 经 mu 串行化（-race 安全）。
func waitForHelperAccept(mu *sync.Mutex, out *strings.Builder, cmd *exec.Cmd) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ready := strings.Contains(out.String(), "ACCEPTED")
		mu.Unlock()
		if ready {
			return nil
		}
		if cmd.ProcessState != nil {
			return fmt.Errorf("helper 提前退出")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("helper 10s 内未 accept")
}

// buildHelperBinary 编译一个继承 fd 的 helper 二进制（同包内 helper 源码）。
func buildHelperBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "fdhelper"+exeSuffix())
	src := filepath.Join(dir, "main.go")
	code := `package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

func main() {
	fd, err := strconv.Atoi(os.Getenv("SPROXY_INHERIT_FD"))
	if err != nil || fd <= 0 {
		fmt.Println("NO_FD")
		os.Exit(1)
	}
	ln, err := net.FileListener(os.NewFile(uintptr(fd), "http"))
	if err != nil {
		fmt.Println("FILE_LISTENER_ERR:", err)
		os.Exit(1)
	}
	fmt.Println("ACCEPTED", ln.Addr().String())
	conn, err := ln.Accept()
	if err != nil {
		fmt.Println("ACCEPT_ERR:", err)
		os.Exit(1)
	}
	fmt.Println("CONN", conn.RemoteAddr().String())
}
`
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		t.Fatalf("WriteFile helper: %v", err)
	}
	build := exec.Command("go", "build", "-o", exe, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	return exe
}

// exeSuffix 返回可执行文件后缀（Windows .exe；测试仅 Unix 编译，恒空）。
func exeSuffix() string { return "" }

// TestGracefulRestart_SignalDispatch 验证 USR2 走 restart 分支而非 shutdown
// （变异：USR2 误走 handleSignalShutdown → 红）。通过 isRestartSignal 判定。
func TestGracefulRestart_SignalDispatch(t *testing.T) {
	t.Parallel()
	if !isRestartSignal(syscall.SIGUSR2) {
		t.Fatalf("Unix 上 SIGUSR2 应判定为优雅重启信号")
	}
	if isRestartSignal(syscall.SIGTERM) || isRestartSignal(syscall.SIGHUP) || isRestartSignal(os.Interrupt) {
		t.Fatalf("SIGTERM/SIGHUP/Interrupt 不应判定为重启信号（零回归）")
	}
}

// TestGracefulRestart_WaitReady 验证 waitRestartReady：mock 新进程 readyz 返回
// 200 → 就绪；503 → 未就绪等待；超时 → 错误（变异：删就绪等待直接返回 → 红）。
func TestGracefulRestart_WaitReady(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx := context.Background()
	if err := waitRestartReady(ctx, ts.Listener.Addr().String(), 2*time.Second); err != nil {
		t.Fatalf("waitRestartReady(200) 应就绪: %v", err)
	}
}

// TestGracefulRestart_WaitReadyTimeout 验证 readyz 永不就绪 → 超时返回错误
// （变异：超时仍返回成功 → 红）。
func TestGracefulRestart_WaitReadyTimeout(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx := context.Background()
	start := time.Now()
	if err := waitRestartReady(ctx, ts.Listener.Addr().String(), 700*time.Millisecond); err == nil {
		t.Fatalf("readyz 恒 503 应超时（不 drain 旧进程）")
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Errorf("超时应等待至少 ~700ms，实际 %v", elapsed)
	}
}

// TestGracefulRestart_HandleRestartReadyThenDrain 验证编排：子进程就绪后旧进程
// drain（复用 handleSignalShutdown 语义）。mock 新进程：在同一 listener 地址上
// 服务 readyz 200（waitRestartReady 探测同一地址）。
func TestGracefulRestart_HandleRestartReadyThenDrain(t *testing.T) {
	// sproxy:serial: 操作包级 restartListener（storeRestartListener/getRestartListener），
	// 与其它优雅重启用例互斥
	// mock 新进程：readyz 恒 200（绑在 restartListener 同一地址上，避免 httptest 端口冲突）。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	childSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = childSrv.Serve(ln) }()
	defer childSrv.Close()

	// 用 mock spawn（替换 restartSpawn 注入点），避免把测试二进制自身递归 spawn
	// （os.Executable 在 go test 下是测试二进制，直接 startRestartChild 会无限递归
	// 跑测试导致 CI test-submodules 超时被 SIGTERM）。
	mockSpawn := func(net.Listener) (*exec.Cmd, error) {
		return nil, errors.New("mock spawn（不实际启动子进程）")
	}
	restartSpawn = mockSpawn
	t.Cleanup(func() { restartSpawn = startRestartChild })

	storeRestartListener(ln)
	t.Cleanup(func() { restartListener.Store(nil) })

	s := &http.Server{Handler: http.NewServeMux()}
	h := &server.Handlers{}

	cancelCalls := &atomic.Int64{}
	done := make(chan struct{})
	go func() {
		handleSignalRestart(func() { cancelCalls.Add(1) }, s, h, testutil.DiscardLogger(), server.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleSignalRestart 未在子进程就绪后完成")
	}
	if cancelCalls.Load() == 0 {
		t.Errorf("子进程就绪后旧进程应 drain（cancel 被调用）")
	}
}

// TestGracefulRestart_HandleRestartTimeoutNoDrain 验证 readyz 永不就绪 → 编排
// 中止且不 drain（变异：超时仍 drain → 红）。mock 新进程 readyz 恒 503（绑在
// restartListener 同一地址上）。
func TestGracefulRestart_HandleRestartTimeoutNoDrain(t *testing.T) {
	// sproxy:serial: 操作包级 restartListener，与其它优雅重启用例互斥
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	childSrv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = childSrv.Serve(ln) }()
	defer childSrv.Close()

	// 用 mock spawn（避免递归 spawn 测试二进制）。
	mockSpawn := func(net.Listener) (*exec.Cmd, error) {
		return nil, errors.New("mock spawn（不实际启动子进程）")
	}
	restartSpawn = mockSpawn
	t.Cleanup(func() { restartSpawn = startRestartChild })

	storeRestartListener(ln)
	t.Cleanup(func() { restartListener.Store(nil) })

	s := &http.Server{Handler: http.NewServeMux()}
	cancelCalls := &atomic.Int64{}
	h := &server.Handlers{}
	// 缩短超时：用 cfg 控制（restartTimeoutFor = max(60s, shutdown)）。
	cfg := server.Default()
	cfg.ServerTimeouts.Shutdown = 500 * time.Millisecond

	done := make(chan struct{})
	go func() {
		handleSignalRestart(func() { cancelCalls.Add(1) }, s, h, testutil.DiscardLogger(), cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleSignalRestart 应在超时后返回（不 drain）")
	}
	if cancelCalls.Load() != 0 {
		t.Errorf("readyz 未就绪超时后旧进程不应 drain（cancel 不应被调用）")
	}
}

// TestGracefulRestart_NoListenerNoSpawn 验证无已绑定 listener 时 spawn 失败
// → 中止（旧进程继续服务，fail-safe 不自杀）。
func TestGracefulRestart_NoListenerNoSpawn(t *testing.T) {
	// sproxy:serial: 操作包级 restartListener，与其它优雅重启用例互斥
	// 无 listener：handleSignalRestart 开头守卫直接中止（不 spawn）。
	restartListener.Store(nil)
	t.Cleanup(func() { restartListener.Store(nil) })
	s := &http.Server{}
	cancelCalls := &atomic.Int64{}
	h := &server.Handlers{}
	done := make(chan struct{})
	go func() {
		handleSignalRestart(func() { cancelCalls.Add(1) }, s, h, testutil.DiscardLogger(), server.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("无 listener 时应在 spawn 阶段失败返回（fail-safe）")
	}
	if cancelCalls.Load() != 0 {
		t.Errorf("spawn 失败不应 drain")
	}
}
