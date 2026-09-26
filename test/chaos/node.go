// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package chaos 提供 HA 场景故障注入测试（roadmap 11.10-⑧）：
//   - ChaosNode：包装 sproxy 子进程（Start/Stop/Kill9/WaitRestart）；
//   - NetChaos：应用层 TCP proxy（Pause/Resume/Delay）复现网络分区/延迟；
//   - 场景：Kill9Restart / NetPartition。
//
// 纯 Go 应用层实现，无 tc/iptables 外部依赖；127.0.0.1 回环绑定铁律；
// --config 隔离用户本机配置（沿用 test/e2e_test.go 的 startSPROXY 模式）。
package chaos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// e2eTestAK / e2eTestSK / e2eTestID 与 e2e 套件一致的确定性凭据
// （服务端凭据 store 预置；值需与 e2e_test.go 完全一致保证签名匹配）。
const (
	e2eTestAK = "ak-00000000000000000000000000000000"
	e2eTestSK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e2eTestID = "skey-000000000001"
)

// ChaosNode 包装 sproxy 子进程。
type ChaosNode struct {
	t       *testing.T
	binPath string
	cmd     *exec.Cmd
	URL     string
	dir     string
	mu      sync.Mutex
	started bool
}

// NewChaosNode 构建真实 sproxy 二进制并返回节点包装（不启动）。
func NewChaosNode(t *testing.T) (*ChaosNode, error) {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sproxy"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(currentFile)))    // test/chaos/node.go → test/chaos → test/ → 模块根
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sproxy") //nolint:gosec // 固定参数，无 taint
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build sproxy: %w\n%s", err, out)
	}
	return &ChaosNode{t: t, binPath: binPath, dir: tmpDir}, nil
}

// Start 启动节点（临时配置 + 随机端口；127.0.0.1）。
func (n *ChaosNode) Start() error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("find free port: %w", err)
	}
	addr := l.Addr().String()
	_ = l.Close() //nolint:staticcheck // 测试用：bind:0 后立即复用端口

	uploadsDir := filepath.Join(n.dir, "storage")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return err
	}
	configPath := filepath.Join(n.dir, "sproxy.yaml")
	configContent := fmt.Sprintf("tls:\n  enabled: false\naccess_keys:\n  - key: %q\n    secret: %q\n", e2eTestAK, e2eTestSK)
	if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
		return err
	}
	// 凭据 store 化：预置 deterministic 凭据（对齐 e2e 模式）。
	seedCredentialStore(n.t, uploadsDir, e2eTestAK, e2eTestSK)

	cmd := exec.Command(n.binPath, "--addr", addr, "--storage-root", uploadsDir, "--config", configPath) //nolint:gosec // 测试装配，参数来自临时目录
	cmd.Dir = filepath.Dir(n.binPath)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sproxy: %w\nstderr: %s", err, errb.String())
	}
	n.mu.Lock()
	n.cmd = cmd
	n.URL = "http://" + addr
	n.started = true
	n.mu.Unlock()
	n.t.Cleanup(func() {
		n.mu.Lock()
		started := n.started
		cmd2 := n.cmd
		n.mu.Unlock()
		if started && cmd2 != nil && cmd2.Process != nil {
			_ = cmd2.Process.Kill()
			_, _ = cmd2.Process.Wait()
		}
	})
	// 轮询 /healthz 就绪（15s；-race 下 45s 约定）。
	return n.waitReady(45 * time.Second)
}

// waitReady 轮询 /healthz 直到 OK。
func (n *ChaosNode) waitReady(timeout time.Duration) error {
	n.mu.Lock()
	base := n.URL
	n.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	n.mu.Lock()
	stderrDump := ""
	if n.cmd != nil && n.cmd.Stderr != nil {
		if buf, ok := n.cmd.Stderr.(*bytes.Buffer); ok {
			stderrDump = buf.String()
		}
	}
	n.mu.Unlock()
	return fmt.Errorf("sproxy 未在 %s 内就绪\nstderr: %s", timeout, stderrDump)
}

// Kill9 强杀进程（SIGKILL——模拟崩溃，不走优雅关闭）。
func (n *ChaosNode) Kill9() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.started || n.cmd.Process == nil {
		return fmt.Errorf("节点未启动")
	}
	if runtime.GOOS == "windows" {
		return n.cmd.Process.Kill()
	}
	return n.cmd.Process.Signal(syscall.SIGKILL)
}

// WaitExit 等待进程退出（Kill9 后调用，确保回收）。
func (n *ChaosNode) WaitExit() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil {
		return nil
	}
	err := n.cmd.Wait()
	n.started = false
	return err
}

// Pid 返回当前进程 PID（重启后变化断言用）。
func (n *ChaosNode) Pid() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd == nil || n.cmd.Process == nil {
		return 0
	}
	return n.cmd.Process.Pid
}

// Restart 重新启动（Kill9 后复用同一 storage——崩溃恢复语义）。
func (n *ChaosNode) Restart() error {
	n.started = false
	n.cmd = nil
	return n.Start()
}

// signingTransport 自动给每个请求加 SproxySig 签名头（body 预哈希后重放；
// 照 e2e_test.go 的 signingTransport——认证驱动模式下直连 HTTP 面必须验签）。
type signingTransport struct {
	base http.RoundTripper
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyHash string
	if req.Body != nil && req.Body != http.NoBody {
		data, rerr := io.ReadAll(req.Body)
		if rerr != nil {
			return nil, rerr
		}
		_ = req.Body.Close()
		sum := sha256.Sum256(data)
		bodyHash = hex.EncodeToString(sum[:])
		req.ContentLength = int64(len(data))
		req.Body = io.NopCloser(bytes.NewReader(data))
	} else {
		bodyHash = sproxysig.EmptyBodyHash()
	}
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: e2eTestAK, EntryID: e2eTestID,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(e2eTestSK, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
	return t.base.RoundTrip(req)
}

// authedClient 是带 SproxySig 签名的 HTTP client（替代 http.DefaultClient；
// 独立 transport 不落默认池——硬规则 17）。
var authedClient = &http.Client{Transport: &signingTransport{base: netutil.IsolatedTransport()}}

// seedCredentialStore 在 <storage>/anonymous/meta/credentials.json 预置 deterministic
// 凭据（与 e2e_test.go 的 seedCredentialStore 同格式；此处独立实现避免跨包引用）。
func seedCredentialStore(t *testing.T, storageRoot, ak, sk string) {
	t.Helper()
	dir := filepath.Join(storageRoot, "anonymous", "meta")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll credentials: %v", err)
	}
	skBytes, _ := hex.DecodeString(sk)
	f := struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}{
		Version: 1,
		Keys: []accesskey.Key{{
			AK: ak, Owner: "test",
			Entries: []accesskey.SKEntry{{
				ID: e2eTestID, SK: skBytes, Kind: accesskey.KindPlain,
				CreatedAt: time.Now().UTC().Truncate(time.Second),
				Status:    accesskey.StatusActive,
				Meta:      accesskey.Meta{Type: "initial"},
			}},
		}},
	}
	data, _ := json.MarshalIndent(f, "", "  ")
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写凭据 store: %v", err)
	}
}
