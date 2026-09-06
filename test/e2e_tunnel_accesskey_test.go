// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxy_test — 认证驱动隧道 E2E：
// access_keys 配置后，sclient 凭 access_key/access_key_secret 派生隧道密钥走通
// 纯隧道 upload/list（无 tunnel_key）。验证服务端 fail-fast 要求的 access_keys
// 与客户端 HKDF 派生链路在真实二进制下一致。
package sproxy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildSClient 构建 sclient 二进制到临时目录，返回路径。
func buildSClient(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}
	return binPath
}

// sclientConfig 生成隔离的临时配置：server_url + access_key/access_key_secret。
// 不用 --server 单独 flag，避免读取用户本地 ~/.config/sproxy/sclient.yaml 干扰。
func sclientConfig(t *testing.T, baseURL string) string {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sclient.yaml")
	content := "server_url: " + baseURL + "\naccess_key: " + e2eTestAK + "\naccess_key_secret: " + e2eTestSK + "\naccess_key_id: " + e2eTestID + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestE2E_TunnelAccessKey_UploadList 验证认证驱动隧道全链路：
//   - 服务端配置 access_keys（startSPROXY 已配，无 tunnel_key）；
//   - sclient 带 access_key/access_key_secret → FileClient.WithTunnel 派生隧道密钥；
//   - upload 经 /tunnel 加密上传、list 经 /tunnel 加密读取均成功。
func TestE2E_TunnelAccessKey_UploadList(t *testing.T) {
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	sclient := buildSClient(t)
	cfgPath := sclientConfig(t, baseURL)

	// 准备待上传文件
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "tunnel-accesskey.txt")
	content := "tunnel access-key e2e — 认证驱动加密隧道"
	if err := os.WriteFile(srcPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// 纯隧道 upload：配置 access_key/access_key_secret 后 FileClient 自动走 /tunnel。
	// 传相对文件名 + cwd 指向源目录，避免本地绝对路径（Windows 含 ':'）被当作远程名。
	up := exec.Command(sclient, "upload", "tunnel-accesskey.txt", "--config", cfgPath)
	up.Dir = srcDir
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("sclient upload via tunnel: %v\n%s", err, out)
	}

	// 纯隧道 list：确认文件可见。
	list := exec.Command(sclient, "list", "--config", cfgPath)
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("sclient list via tunnel: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "tunnel-accesskey.txt") {
		t.Errorf("expected tunnel-accesskey.txt in tunnel list output, got: %s", out)
	}
}
