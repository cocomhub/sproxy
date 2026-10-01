// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildFakeCLI 用 Go 编译一个假 pikpak 可执行：按 args 回显预置 JSON。
// login → loginJSON；status → statusJSON。
func buildFakeCLI(t *testing.T, loginJSON, statusJSON string) string {
	t.Helper()
	src := fmt.Sprintf(`package main

import (
	"os"
)

var login = %q
var status = %q

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "auth" && args[1] == "login" {
		os.Stdout.WriteString(login)
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Stdout.WriteString(status)
		os.Exit(0)
	}
	os.Exit(1)
}
`, loginJSON, statusJSON)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "pikpak-fake")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake cli: %v: %s", err, out)
	}
	return bin
}

// TestAuth_DeviceLogin 未登录 → 发起 device → 解析 device 码。
func TestAuth_DeviceLogin(t *testing.T) {
	t.Parallel()
	bin := buildFakeCLI(t,
		`{"flow":"device","device_code":"dc1","verification_uri":"https://mypikpak.com/drive/activate?client_id=x","user_code":"ABC123","expires_in":300,"interval":2}`,
		`{"logged_in":false}`,
	)
	cli, err := NewCli(CliConfig{BinaryPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	auth := NewAuth(cli)

	// 未登录
	ok, err := auth.LoggedIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected not logged in")
	}

	// 发起 device 授权（拿到码）
	dc, err := auth.DeviceLoginStart(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABC123" {
		t.Fatalf("expected user code ABC123, got %q", dc.UserCode)
	}
	if dc.VerificationURI == "" {
		t.Fatal("expected verification uri")
	}
}

// TestNewCli_Install 自动安装：发布清单服务器 + 资产下载。
func TestNewCli_Install(t *testing.T) {
	t.Parallel()
	// 注入命令工厂：避免测试运行真实 pikpak 二进制
	origRun := runCommandContext
	runCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo '{\"logged_in\":true}'")
	}
	defer func() { runCommandContext = origRun }()

	var assetBody = []byte("#!/bin/sh\necho fake-cli\n")
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/config/v1/command_line", func(w http.ResponseWriter, r *http.Request) {
		name := "pikpak_linux_amd64"
		if runtime.GOOS == "windows" {
			name = "pikpak_windows_amd64.exe"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"values": map[string]any{
				"command_line": map[string]any{
					"assets": []map[string]any{
						{"name": name, "browser_download_url": srv.URL + "/dl/" + name},
					},
				},
			},
		})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(assetBody)
	})

	cfg := CliConfig{InstallDir: t.TempDir(), AutoInstall: true, HTTPClient: srv.Client(), ConfigURL: srv.URL + "/config/v1/command_line"}
	cli, err := NewCli(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cli == nil {
		t.Fatal("expected cli")
	}
	if !fileExists(cli.bin) {
		t.Fatalf("expected installed binary at %s", cli.bin)
	}
}
