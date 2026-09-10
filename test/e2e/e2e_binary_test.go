// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// e2eAK / e2eSK / e2eID 是与 test/e2e_test.go 等价的确定性 SproxySig 测试凭据。
// 本包位于 test/e2e/（独立目录、package e2e_test），无法复用 test/ 包内的
// e2eTestAK/e2eTestSK/e2eTestID 与 seedCredentialStore，故按契约自带一份等价定义
// （跨包共享需新增非测试包，会触碰 make notest 门禁，不值得）。
const (
	e2eAK = "ak-00000000000000000000000000000000"
	e2eSK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e2eID = "skey-000000000001"
)

// seedCredentialStoreLocal 在 <storageRoot>/anonymous/meta/credentials.json 预写一条
// plain alive 凭据（JSON 结构同 server.CredentialStore.Save 落盘格式）。
//
// 必须 seed：认证重构后 yaml access_keys 不再装配 Ring，凭据表改由
// <storage_root>/<tenant>/meta/credentials.json 提供；若 ring 为空（未 seed），
// 服务端首启会生成**随机** anonymous 凭据，sclient 携带的 e2eAK 不被识别 → 全部请求
// 401（这正是本用例在认证重构后长期红的历史原因）。
func seedCredentialStoreLocal(t *testing.T, storageRoot string) {
	t.Helper()
	sk, derr := hex.DecodeString(e2eSK)
	if derr != nil {
		t.Fatalf("seed credential store: decode sk: %v", derr)
	}
	f := struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}{
		Version: 1,
		Keys: []accesskey.Key{{
			AK: e2eAK, Owner: "test",
			Entries: []accesskey.SKEntry{{
				ID: e2eID, SK: sk, Kind: accesskey.KindPlain,
				CreatedAt: time.Now().UTC().Truncate(time.Second),
				Status:    accesskey.StatusActive,
				Meta:      accesskey.Meta{Type: "initial"},
			}},
		}},
	}
	metaDir := filepath.Join(storageRoot, "anonymous", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("seed credential store mkdir: %v", err)
	}
	data, jerr := json.MarshalIndent(f, "", "  ")
	if jerr != nil {
		t.Fatalf("seed credential store marshal: %v", jerr)
	}
	if werr := os.WriteFile(filepath.Join(metaDir, "credentials.json"), data, 0o644); werr != nil {
		t.Fatalf("seed credential store write: %v", werr)
	}
}

// findFilesNamedLocal 在 root 下递归找 basename == name 的**普通文件**（按路径排序）。
// 不硬编码 tenant 段——tenant 名由服务端从凭据推导，属实现细节。
func findFilesNamedLocal(t *testing.T, root, name string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			if os.IsNotExist(werr) {
				return nil
			}
			return werr
		}
		if d.Name() == name && !d.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir %s: %v", root, err)
	}
	sort.Strings(found)
	return found
}

// sclientArgs 组装 sclient 子进程的全局 flag + 子命令参数。
//   - --config 指向**不存在**的路径：隔离本机 ~/.config/sproxy/sclient.yaml
//     （--server 挡不住用户配置里的 access_key 等）；
//   - AK/SK/ID 三件套：--access-key 非空即令 clientfactory 走 client.WithTunnel →
//     加密隧道 localMux 路径（服务端 /tunnel 内层已注册文件面路由）。
func sclientArgs(baseURL, tmpDir string, args ...string) []string {
	prefix := []string{
		"--config", filepath.Join(tmpDir, "sclient.yaml"),
		"--server", baseURL,
		"--access-key", e2eAK,
		"--access-key-secret", e2eSK,
		"--access-key-id", e2eID,
	}
	return append(prefix, args...)
}

// TestE2E_Binary tests the full build -> start -> upload -> list -> download -> delete
// workflow using the built sproxy and sclient binaries.
func TestE2E_Binary_UploadDownloadDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e binary test in short mode")
	}

	tmpDir := t.TempDir()

	// Locate module root: test/e2e/e2e_binary_test.go -> test/e2e/ -> test/ -> module root
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(currentFile)))

	binDir := filepath.Join(tmpDir, "bin")
	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// ---- Build sproxy binary ----
	t.Log("Building sproxy...")
	sproxyBin := filepath.Join(binDir, "sproxy")
	if runtime.GOOS == "windows" {
		sproxyBin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", sproxyBin, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, out)
	}

	// ---- Build sclient binary ----
	t.Log("Building sclient...")
	sclientBin := filepath.Join(binDir, "sclient")
	if runtime.GOOS == "windows" {
		sclientBin += ".exe"
	}
	buildCmd = exec.Command("go", "build", "-o", sclientBin, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	// ---- Find a free port ----
	// 变量名避开 err：后续多处 if err := ... 的作用域会遮蔽函数级 err（govet shadow）。
	l, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("find free port: %v", listenErr)
	}
	addr := l.Addr().String()
	l.Close() // close immediately; race with sproxy is acceptable for tests
	t.Logf("Using address: %s", addr)

	// ---- Start sproxy ----
	// 写入临时配置文件，禁用 TLS（E2E 测试使用纯 HTTP 连接）。
	// tunnel_key 已废除（由凭据 SK 经 HKDF 派生），不再写入。
	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	configContent := []byte("tls:\n  enabled: false\n")
	if err := os.WriteFile(configPath, configContent, 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	// 凭据 store 种子：使服务端 Ring 首启即识别 e2eAK/e2eSK（详见 seedCredentialStoreLocal）。
	seedCredentialStoreLocal(t, uploadsDir)

	args := []string{
		"--addr", addr,
		"--storage-root", uploadsDir,
		"--config", configPath,
	}
	cmd := exec.Command(sproxyBin, args...)
	cmd.Dir = moduleRoot

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	// ---- Wait for server readiness ----
	baseURL := fmt.Sprintf("http://%s", addr)
	healthOK := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				healthOK = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthOK {
		t.Fatalf("server at %s did not become ready within 5s\nstdout: %s\nstderr: %s",
			addr, stdoutBuf.String(), stderrBuf.String())
	}
	t.Logf("Server ready at %s", baseURL)

	// ---- Create test file ----
	testContent := "hello e2e binary test"
	if err := os.WriteFile(filepath.Join(tmpDir, "hello.txt"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	// runSclient 以子进程运行 sclient（配置隔离 + SproxySig 凭据 + XDG 隔离）。
	// XDG_CACHE_HOME/XDG_CONFIG_HOME 指向本次临时目录：sclient 的 cd 当前目录持久化在
	// XDG **缓存**目录（与 --config 无关），本机若存在 cd 状态会改变相对路径解析。
	runSclient := func(args ...string) ([]byte, error) {
		sCmd := exec.Command(sclientBin, sclientArgs(baseURL, tmpDir, args...)...)
		sCmd.Dir = tmpDir
		sCmd.Env = append(os.Environ(),
			"XDG_CACHE_HOME="+filepath.Join(tmpDir, "xdg-cache"),
			"XDG_CONFIG_HOME="+filepath.Join(tmpDir, "xdg-config"),
		)
		return sCmd.CombinedOutput()
	}

	// ---- Upload via sclient ----
	// Run sclient from tmpDir so the local file path "hello.txt" resolves to
	// a simple remote filename "hello.txt" (not a Windows absolute path).
	t.Log("Uploading file via sclient...")
	out, err := runSclient("upload", "hello.txt")
	if err != nil {
		t.Fatalf("upload failed: %v\n%s", err, out)
	}
	t.Logf("Upload output: %s", strings.TrimSpace(string(out)))

	// 磁盘副作用：hello.txt 落盘且内容一致
	uploaded := findFilesNamedLocal(t, uploadsDir, "hello.txt")
	if len(uploaded) != 1 {
		t.Fatalf("upload 后应恰有 1 个 hello.txt, got %d: %v", len(uploaded), uploaded)
	}
	onDisk, err := os.ReadFile(uploaded[0])
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if string(onDisk) != testContent {
		t.Errorf("落盘内容不一致: got %q, want %q", onDisk, testContent)
	}

	// ---- List via sclient ----
	t.Log("Listing files via sclient...")
	out, err = runSclient("list")
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	t.Logf("List output:\n%s", strings.TrimSpace(string(out)))
	if !strings.Contains(string(out), "hello.txt") {
		t.Errorf("list output should contain hello.txt, got: %s", out)
	}

	// ---- Download via sclient ----
	t.Log("Downloading file via sclient...")
	dstFile := filepath.Join(tmpDir, "downloaded.txt")
	out, err = runSclient("download", "hello.txt", dstFile)
	if err != nil {
		t.Fatalf("download failed: %v\n%s", err, out)
	}
	t.Logf("Download output: %s", strings.TrimSpace(string(out)))

	// Verify downloaded content
	downloaded, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != testContent {
		t.Errorf("downloaded content mismatch: got %q, want %q", string(downloaded), testContent)
	}
	t.Log("Downloaded content verified")

	// ---- Delete via sclient ----
	t.Log("Deleting file via sclient...")
	out, err = runSclient("delete", "hello.txt")
	if err != nil {
		t.Fatalf("delete failed: %v\n%s", err, out)
	}
	t.Logf("Delete output: %s", strings.TrimSpace(string(out)))

	// 磁盘副作用：delete 后不再有 hello.txt
	if got := findFilesNamedLocal(t, uploadsDir, "hello.txt"); len(got) != 0 {
		t.Errorf("delete 后磁盘不应再有 hello.txt, got %v", got)
	}
}
