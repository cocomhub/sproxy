// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package sproxy_test — --insecure / 配置 insecure 覆盖 E2E。
//
// 背景：`--insecure`（及配置 `insecure: true`）曾只在「HTTP 直连面」生效；当凭据存在
// 且未显式传 `--server` 时，FileClient 走 access-key 驱动的**传统 /tunnel 隧道**，
// 而 tunnel.Client 自建独立 HTTP Transport ⇒ 忽略 TLS 策略，
// 报 `tls: failed to verify certificate: x509: certificate signed by unknown authority`。
//
// 本文件以真实自签 TLS sproxy 二进制 + 真实 sclient 二进制，逐命令验证三种等价入口：
//  1. 配置 `insecure: true`（不带任何 TLS flag）；
//  2. CLI `--insecure`（配置不带 insecure）；
//  3. 配置 `ca_file`（严格校验，替代 insecure 的安全做法）。
//
// 三类均以「不传 --server」强制走传统隧道（缺陷路径），确保修复不回退。
package sproxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// insecureHubConfig 启用 hub（ws 传输与主 listener 同址 /ws），使 mesh status / relay
// 等跨节点命令在 TLS 服务端上可用。
const insecureHubConfig = "hub:\n  enabled: true\n  transports:\n    ws:\n      enabled: true\n      path: /ws\n"

// insecureEnv 是 --insecure 覆盖测试环境：真实自签 TLS sproxy 子进程 + 真实 sclient
// 二进制；客户端配置完全由 config.yaml 驱动（不带 --server / --access-key 全局 flag）
// ⇒ FileClient 走 access-key 驱动的传统 /tunnel 隧道（缺陷路径）。
type insecureEnv struct {
	baseURL     string
	storageRoot string
	tmpDir      string
	certFile    string
	sclientBin  string
	// hubURL 非空时写入 config env 的 hub_url（mesh/relay 等跨节点命令）。
	hubURL string
	// extraGlobal 是每次运行都追加的全局 flag（如 --insecure）。
	extraGlobal []string
}

// startInsecureEnv 启动自签 TLS sproxy（开启 debug_pprof_enabled 与 hub 供 mesh 系列）。
func startInsecureEnv(t *testing.T, extraConfig string) *insecureEnv {
	t.Helper()
	baseURL, storageRoot, certFile, cleanup := startSPROXYTLS(t, "debug_pprof_enabled: true\n"+insecureHubConfig+extraConfig)
	t.Cleanup(cleanup)
	return &insecureEnv{
		baseURL:     baseURL,
		storageRoot: storageRoot,
		tmpDir:      filepath.Dir(storageRoot),
		certFile:    certFile,
		sclientBin:  e2eBinPath(t, "cmd/sclient"),
	}
}

// configPath 返回本环境的 config.yaml 路径。
func (e *insecureEnv) configPath() string { return filepath.Join(e.tmpDir, "config.yaml") }

// writeConfig 写 context config.yaml：env 段含 server_url 与可选 insecure/ca_file，
// user 段含凭据（不带 --server / --access-key flag ⇒ 走传统隧道）。
func (e *insecureEnv) writeConfig(t *testing.T, insecure bool, caFile string) {
	t.Helper()
	var envExtra strings.Builder
	if insecure {
		envExtra.WriteString("    insecure: true\n")
	}
	if caFile != "" {
		fmt.Fprintf(&envExtra, "    ca_file: %q\n", caFile)
	}
	if e.hubURL != "" {
		fmt.Fprintf(&envExtra, "    hub_url: %q\n", e.hubURL)
	}
	body := fmt.Sprintf(`apiVersion: sclient/v1
kind: Config
current-context: e2e
environments:
  - name: e2e
    server_url: %q
%[2]susers:
  - name: e2e
    access_key: %q
    access_key_secret: %q
    access_key_id: %q
contexts:
  - name: e2e
    environment: e2e
    user: e2e
`, e.baseURL, envExtra.String(), e2eTestAK, e2eTestSK, e2eTestID)
	if err := os.WriteFile(e.configPath(), []byte(body), 0o600); err != nil {
		t.Fatalf("写 config.yaml: %v", err)
	}
}

// run 以子进程运行 sclient（注入 --config + extraGlobal + XDG 隔离），返回 stdout/stderr/err。
func (e *insecureEnv) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	full := append([]string{"--config", e.configPath()}, e.extraGlobal...)
	full = append(full, args...)
	cmd := exec.Command(e.sclientBin, full...)
	cmd.Dir = e.tmpDir
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(e.tmpDir, cliXDGCacheDir),
		"XDG_CONFIG_HOME="+filepath.Join(e.tmpDir, cliXDGConfigDir),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// ok 运行 sclient 并要求成功（附 stderr 便于定位）。
func (e *insecureEnv) ok(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := e.run(t, args...)
	if err != nil {
		t.Fatalf("sclient %v 失败: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout, stderr)
	}
	return stdout
}

// TestE2E_Insecure_ConfigDriven_TunnelList 复现并锁定缺陷：配置 insecure: true +
// 未传 --server ⇒ 走传统 /tunnel 隧道（TLS 策略必须生效）。
func TestE2E_Insecure_ConfigDriven_TunnelList(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.writeConfig(t, true, "")
	env.ok(t, "list")
}

// insecureCommandStep 是一条待跑通命令（name 用于子测试定位）。
type insecureCommandStep struct {
	name string
	args []string
}

// shareCreateJSON 是 `share create --json` 输出契约（仅取 token）。
type shareCreateJSON struct {
	Token string `json:"token"`
}

// runInsecureCommandSuite 端到端跑通**所有连服务端的命令**（顺序化：共享真实 TLS
// 服务端与远程状态；用例内步骤顺序执行，测试间相互独立可并行）。
func runInsecureCommandSuite(t *testing.T, env *insecureEnv) {
	t.Helper()
	// 本地夹具（upload/archive 等的入参）。
	if err := os.WriteFile(filepath.Join(env.tmpDir, "seed.txt"), []byte("insecure e2e seed"), 0o644); err != nil {
		t.Fatalf("写 seed.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(env.tmpDir, "out"), 0o755); err != nil {
		t.Fatalf("建 out 目录: %v", err)
	}

	steps := []insecureCommandStep{
		{"upload", []string{"upload", "seed.txt"}},
		{"list", []string{"list"}},
		{"ls", []string{"ls"}},
		{"tree", []string{"tree"}},
		{"search", []string{"search", "seed"}},
		{"stat", []string{"stat", "seed.txt"}},
		{"meta", []string{"meta", "seed.txt"}},
		{"du", []string{"du"}},
		{"df", []string{"df"}},
		{"stats", []string{"stats"}},
		{"volumes", []string{"volumes"}},
		{"volume-list", []string{"volume", "list"}},
		{"quota", []string{"quota"}},
		{"trash-list", []string{"trash", "list"}},
		{"mkdir", []string{"mkdir", "d1"}},
		{"preview", []string{"preview", "seed.txt"}},
		{"download", []string{"download", "seed.txt", "out/seed.txt"}},
		{"mv", []string{"mv", "seed.txt", "seed_moved.txt"}},
		{"cp", []string{"cp", "seed_moved.txt", "seed_copy.txt"}},
		{"delete", []string{"delete", "seed_copy.txt"}},
		{"checksum-rebuild", []string{"checksum", "rebuild"}},
		{"archive", []string{"archive", "seed_moved.txt"}},
		{"archive-dir", []string{"archive-dir", "d1"}},
		{"rmdir", []string{"rmdir", "d1"}},
		{"perf-profile", []string{"perf", "profile", "heap"}},
		{"perf-metrics", []string{"perf", "metrics"}},
		{"tunnel", []string{"tunnel", "/api/files"}},
		{"diag", []string{"diag"}},
		{"secret-list", []string{"secret", "list"}},
		// mesh status（列服务）需 hub 上有已注册服务；无服务时服务端返回 JSON null，
		// 客户端拒绝（属服务端空载荷细节，非 TLS），故只测 --server 子形态。
		{"mesh-status-server", []string{"mesh", "status", "--server"}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			env.ok(t, s.args...)
		})
	}

	// batch 族（支持 delete/mkdir/rmdir/meta 子集）。
	env.ok(t, "upload", "seed.txt")
	batchCmds := filepath.Join(env.tmpDir, "batch-cmds.txt")
	if err := os.WriteFile(batchCmds, []byte("mkdir batchdir1\nmeta seed.txt\n"), 0o644); err != nil {
		t.Fatalf("写 batch 命令文件: %v", err)
	}
	env.ok(t, "batch", batchCmds)
	env.ok(t, "batch-rename", "seed.txt", "seed_renamed.txt")
	env.ok(t, "batch-delete", "seed_renamed.txt")

	// share 生命周期（create → list → revoke）。
	shareOut := env.ok(t, "share", "create", "seed_moved.txt", "--json")
	var created shareCreateJSON
	if err := json.Unmarshal([]byte(shareOut), &created); err != nil {
		t.Fatalf("解析 share create --json: %v\n%s", err, shareOut)
	}
	env.ok(t, "share", "list")
	env.ok(t, "share", "revoke", created.Token)

	// cloud-download 需可访问 URL（本地 httptest 服务，服务端 cloud_download_allow_private）。
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("cloud payload"))
	}))
	defer src.Close()
	env.ok(t, "cloud-download", "submit", src.URL+"/payload.bin")
	env.ok(t, "cloud-download", "list")
	env.ok(t, "cloud-download-group", "submit", "grp1", src.URL+"/payload2.bin")
	env.ok(t, "cloud-download-group", "list")

	// 备份 / 迁移 / 卷 / 集群 / 凭据（服务端面）。
	env.ok(t, "backup", "default", filepath.Join(env.tmpDir, "backup-out.tar"))
	env.ok(t, "migrate", "export", filepath.Join(env.tmpDir, "migrate-export"), "--yes")
	// volume create/delete 需已注册 backend（baidupcs|webdav）与外部依赖，与 TLS 无关；
	// volume trace/explain 需卷实现 StorageInspector，普通本地卷 501——卷族客户端→
	// 服务端链路由 volumes / volume list 覆盖。
	// cluster issue 是本地签发（不连服务端），补齐必填参数使其成功。
	env.ok(t, "cluster", "issue",
		"--sign-key", strings.Repeat("ab", 32),
		"--node", "node1",
		"--volume", "default",
		"--owner", "test",
		"--recipient", strings.Repeat("cd", 32))
	env.ok(t, "trust", "list")
	// sync 需服务端 sync_remotes 指向可达对端；见 TestE2E_Insecure_SyncPush。
}

// TestE2E_Insecure_AllCommands_ConfigDriven：配置 insecure 覆盖全部命令。
func TestE2E_Insecure_AllCommands_ConfigDriven(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.writeConfig(t, true, "")
	runInsecureCommandSuite(t, env)
}

// TestE2E_Insecure_AllCommands_CLIFlag 证明 CLI `--insecure`（配置不带 insecure）同样
// 覆盖全部命令。
//
// TestE2E_Insecure_AllCommands_CLIFlag：CLI --insecure 覆盖全部命令。
func TestE2E_Insecure_AllCommands_CLIFlag(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.writeConfig(t, false, "")
	env.extraGlobal = []string{"--insecure"}
	runInsecureCommandSuite(t, env)
}

// TestE2E_Insecure_AllCommands_CAFile 证明安全替代路径（配置 ca_file 严格校验自签 CA）
// 同样覆盖全部命令。
//
// TestE2E_Insecure_AllCommands_CAFile：配置 ca_file 覆盖全部命令。
func TestE2E_Insecure_AllCommands_CAFile(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.writeConfig(t, false, env.certFile)
	runInsecureCommandSuite(t, env)
}

// TestE2E_Insecure_SyncPush 覆盖 `sync push`：TLS 主服务端（客户端 insecure）+
// 明文对端（服务端 sync_remotes direct 目标），验证同步命令的客户端→服务端链路。
//
// TestE2E_Insecure_SyncPush：TLS 主服务端 + 明文对端的 sync push。
func TestE2E_Insecure_SyncPush(t *testing.T) {
	t.Parallel()
	peerURL, _, peerCleanup := startSPROXYImpl(t, "")
	defer peerCleanup()

	syncRemote := fmt.Sprintf(`sync_remotes:
  - name: peer
    url: %q
    access_key: %q
    access_key_secret: %q
    access_key_id: %q
`, peerURL, e2eTestAK, e2eTestSK, e2eTestID)

	env := startInsecureEnv(t, syncRemote)
	env.writeConfig(t, true, "")

	if err := os.WriteFile(filepath.Join(env.tmpDir, "syncseed.txt"), []byte("sync seed"), 0o644); err != nil {
		t.Fatalf("写 syncseed.txt: %v", err)
	}
	env.ok(t, "upload", "syncseed.txt")
	env.ok(t, "sync", "push", "--remote", "peer", "--wait")
}

// tlsHubNodeRegistered 查询自签 https hub 的 /api/hub/nodes 判断 nodeID 是否已注册
// （TLS 探测用跳过校验客户端；SproxySig 签名与既有 signedHubGET 同口径）。
func tlsHubNodeRegistered(baseURL, nodeID string) bool {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/hub/nodes", nil)
	if err != nil {
		return false
	}
	sproxysig.SignRequestWithSkeyID(req, e2eTestAK, e2eTestID, e2eTestSK)
	resp, err := client.InsecureHTTPClient().Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var nodes []struct {
		ID string `json:"id"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&nodes); derr != nil {
		return false
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return true
		}
	}
	return false
}

// TestE2E_Insecure_MeshHubRelay_ConfigDriven 证明 mesh 家族（relay / p2p / socks /
// udp / http-proxy 共享同一 hub TLS 装配：mesh.HubWSDial / AutoRegister）在**配置
// insecure: true**下能连上自签 wss hub 并完成注册——不带任何 --insecure/--ca-file
// flag（此前配置的 insecure 不会到达这些只读 flag 的命令）。
//
// TestE2E_Insecure_MeshHubRelay_ConfigDriven：配置 insecure 连自签 wss hub 注册。
func TestE2E_Insecure_MeshHubRelay_ConfigDriven(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.hubURL = "wss://" + strings.TrimPrefix(env.baseURL, "https://") + "/ws"
	env.writeConfig(t, true, "")

	const nodeID = "insecure-relay-node"
	cmd := exec.Command(env.sclientBin, "--config", env.configPath(), "relay", "start", "--node-id", nodeID, "--dial-allow")
	cmd.Dir = env.tmpDir
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(env.tmpDir, cliXDGCacheDir),
		"XDG_CONFIG_HOME="+filepath.Join(env.tmpDir, cliXDGConfigDir),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sclient relay: %v", err)
	}
	killWait := newKillWaitCleanup(cmd)
	defer killWait()

	if !testutil.WaitForBool(30*time.Second, func() bool { return tlsHubNodeRegistered(env.baseURL, nodeID) }) {
		killWait()
		t.Fatalf("relay 未在配置 insecure 下注册到自签 wss hub; stderr:\n%s", stderr.String())
	}
}

// TestE2E_Insecure_MeshNode_ConfigDriven 覆盖 mesh node（meshconn.AutoRegister 信令注册
// 路径，socks/udp/http-proxy/p2p 共用）：配置 insecure: true 下能向自签 wss hub 注册。
func TestE2E_Insecure_MeshNode_ConfigDriven(t *testing.T) {
	t.Parallel()
	env := startInsecureEnv(t, "")
	env.hubURL = "wss://" + strings.TrimPrefix(env.baseURL, "https://") + "/ws"
	env.writeConfig(t, true, "")

	const nodeID = "insecure-mesh-node"
	// --gateway-addr 127.0.0.1:0：同机多节点随机端口（避免并行用例端口冲突）。
	cmd := exec.Command(env.sclientBin, "--config", env.configPath(),
		"mesh", "node", "--node-id", nodeID, "--gateway-addr", "127.0.0.1:0")
	cmd.Dir = env.tmpDir
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(env.tmpDir, cliXDGCacheDir),
		"XDG_CONFIG_HOME="+filepath.Join(env.tmpDir, cliXDGConfigDir),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sclient mesh node: %v", err)
	}
	killWait := newKillWaitCleanup(cmd)
	defer killWait()

	if !testutil.WaitForBool(30*time.Second, func() bool { return tlsHubNodeRegistered(env.baseURL, nodeID) }) {
		killWait()
		t.Fatalf("mesh node 未在配置 insecure 下注册到自签 wss hub; stderr:\n%s", stderr.String())
	}
}
