// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_restart_harness_test.go 提供**可重启**的真实进程环境（E2E 恢复测试专用）：
// 与 `startSPROXYImpl` 不同，它把「构建一次 + 固定端口 + 同一存储根」暴露出来，
// 以便 **kill -9 → 重启 → 断言恢复**（这才是「服务重启能自动继续」的真实检验）。
//
// 与既有 `pkg/recovery/crash_e2e_test.go` 的分工：
//   - 那边验证 **WAL 协调器**（进程自我 re-exec、崩溃在 MarkDone 前后）；
//   - 这里验证**服务级**恢复：云任务续传/排队任务继续、配额等待、崩溃后配额真值。
package sproxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// syncBuffer 是**并发安全**的 bytes.Buffer：子进程 stdout/stderr 由 os/exec 的拷贝 goroutine
// 写入，而断言（如「日志含 unreadable on recovery」）会在主 goroutine 读取 ⇒ 裸 bytes.Buffer
// 会被 `-race` 判数据竞争（CI ubuntu -race 实测）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// restartEnv 是可重启的 sproxy 环境（真实二进制 + 固定存储根/端口）。
type restartEnv struct {
	tmpDir      string
	binPath     string
	configPath  string
	storageRoot string // <tmp>/uploads（默认卷根，重启后同路径）
	addr        string
	baseURL     string
	sclientBin  string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
}

// newRestartEnv 构建二进制并准备配置（**不启动**）。
func newRestartEnv(t *testing.T, extraConfig string) *restartEnv {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sproxy"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, out)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	storageRoot := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(storageRoot, 0o755); err != nil {
		t.Fatalf("create uploads dir: %v", err)
	}

	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	cfg := fmt.Sprintf("tls:\n  enabled: false\ncloud_download_allow_private: true\naccess_keys:\n  - key: %q\n    secret: %q\n",
		e2eTestAK, e2eTestSK)
	cfg += extraConfig
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// 确定性凭据（与 startSPROXYImpl 同源）：首启会读到这条 plain alive 条目。
	seedCredentialStore(t, storageRoot, e2eTestAK, e2eTestSK)

	env := &restartEnv{
		tmpDir:      tmpDir,
		binPath:     binPath,
		configPath:  configPath,
		storageRoot: storageRoot,
		addr:        addr,
		baseURL:     "http://" + addr,
		sclientBin:  e2eBinPath(t, "cmd/sclient"),
	}
	// 写 sclient 的统一 context 配置（沿用既有 helper；先建目录）。
	cfgDir := filepath.Join(tmpDir, cliXDGConfigDir, "sproxy")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir sclient config dir: %v", err)
	}
	if err := writeContextConfigYAML(t, filepath.Join(cfgDir, "config.yaml"),
		env.baseURL, e2eTestAK, e2eTestSK, e2eTestID); err != nil {
		t.Fatalf("write sclient config: %v", err)
	}
	return env
}

// boot 启动服务并等待 /healthz OK。
func (e *restartEnv) boot(t *testing.T) {
	t.Helper()
	cmd := exec.Command(e.binPath, "--addr", e.addr, "--storage-root", e.storageRoot, "--config", e.configPath)
	cmd.Dir = filepath.Dir(e.binPath)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy: %v", err)
	}
	e.mu.Lock()
	e.cmd, e.stdout, e.stderr = cmd, stdout, stderr
	e.mu.Unlock()

	ok := false
	testutil.WaitFor(t, 30*time.Second, func() bool {
		resp, err := http.Get(e.baseURL + "/healthz")
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		ok = resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK"
		return ok
	}, "sproxy 未在超时内就绪")
	if !ok {
		e.dumpLogs(t)
		t.Fatal("sproxy not ready")
	}
}

// Kill9 模拟断电/被 kill -9：立即杀进程（不等待优雅收尾）。
func (e *restartEnv) Kill9(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("Kill9: 进程未启动")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// Stop 优雅停机（Unix: SIGINT；Windows: Kill）——用于验证优雅退出路径。
func (e *restartEnv) Stop(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS != "windows" {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// Close 兜底清理（测试失败时也能收尸）。
func (e *restartEnv) Close() {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// Logs 返回当前进程的 stdout+stderr（重启后为新进程的输出）。
func (e *restartEnv) Logs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stdout == nil {
		return ""
	}
	return e.stdout.String() + e.stderr.String()
}

func (e *restartEnv) dumpLogs(t *testing.T) {
	t.Helper()
	t.Logf("sproxy logs:\n%s", e.Logs())
}

// cli 运行 sclient（同一 context 配置 ⇒ 经隧道访问 localMux 端点）。
func (e *restartEnv) cli(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{
		"--config", filepath.Join(e.tmpDir, cliXDGConfigDir, "sproxy", "config.yaml"),
		"--server", e.baseURL,
		"--access-key", e2eTestAK,
		"--access-key-secret", e2eTestSK,
		"--access-key-id", e2eTestID,
	}, args...)
	cmd := exec.Command(e.sclientBin, full...)
	cmd.Dir = e.tmpDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sclient %v 失败: %v\nstdout:\n%s\nstderr:\n%s", args, err, out, stderr.String())
	}
	return string(out)
}
