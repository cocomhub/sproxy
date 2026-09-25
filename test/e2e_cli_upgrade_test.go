// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_upgrade_test.go：upgrade 命令的真实二进制端到端（--check --json 全链路）。
// 假 GitHub API + 假 CDN（httptest，127.0.0.1）→ 真实 sclient 二进制子进程
// --api-base 指向假 API → 断言 stdout 为可解析 JSON（current/latest/update_available）。
// 零真实 GitHub 请求（不耗 API 配额、可离线跑）。
package sproxy_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestE2E_CLI_UpgradeCheckJSON 真实 sclient 二进制 upgrade --check --json：
// 假 API 返回 tag v0.18.0；当前二进制为 dev（e2e 构建不带 ldflags 注入）→
// 无法判定 → update_available:true + latest 断言。恒 exit 0（脚本解析 JSON 字段）。
func TestE2E_CLI_UpgradeCheckJSON(t *testing.T) {
	t.Parallel()

	const tag = "v0.18.0"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cocomhub/sproxy/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tag)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	bin := e2eBinPath(t, "cmd/sclient")
	tmp := t.TempDir()
	cmd := exec.Command(bin,
		"--config", filepath.Join(tmp, "sclient.yaml"),
		"upgrade", "--check", "--json",
		"--api-base", ts.URL+"/repos/cocomhub/sproxy",
		"--release-base", ts.URL,
	)
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "xdg-config"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sclient upgrade --check --json 失败: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}

	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("期望纯 JSON 输出, got: %q (err=%v)", stdout.String(), err)
	}
	if out["latest"] != tag {
		t.Fatalf("latest = %v, want %s", out["latest"], tag)
	}
	if out["update_available"] != true {
		t.Fatalf("update_available 应为 true（当前 dev 无法判定 → 可升级）, got: %v", out)
	}
	if out["current"] == "" || out["current"] == nil {
		t.Fatalf("current 应为非空（dev）, got: %v", out)
	}
}

// TestE2E_CLI_UpgradeFull 真实二进制完整升级：--to 指定版本 → 假 CDN 下载归档 →
// 校验/解包 → 替换。断言目标二进制（os.Executable 的拷贝副本）内容变为
// 假归档内的 sclient（真实副作用）。用临时目录中的假二进制作为目标（os.Executable
// 不可替换运行中进程，故把可执行文件拷到临时目录、用注入路径执行——upgrade 用
// os.Executable 解析目标，这里通过把「假二进制」放在临时目录并让 --api-base 指向
// 假服务器验证下载/校验/解包/替换链路，替换目标为临时副本）。
func TestE2E_CLI_UpgradeFull(t *testing.T) {
	t.Parallel()

	const tag = "v0.18.0"
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	archiveBytes := makeE2EUpgradeArchive(t, binName, "e2e-new-binary")
	sum := sha256hex(archiveBytes)

	assetName := fmt.Sprintf("sproxy_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName += ".zip"
	} else {
		assetName += ".tar.gz"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cocomhub/sproxy/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q}]}`,
			tag, assetName, "http://"+r.Host+"/download/"+tag+"/"+assetName)
	})
	mux.HandleFunc("/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sum+"  "+assetName+"\n")
	})
	mux.HandleFunc("/download/"+tag+"/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveBytes)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// 把真实 sclient 二进制拷到临时目录作为「目标」，随后验证该副本被替换。
	bin := e2eBinPath(t, "cmd/sclient")
	tmp := t.TempDir()
	target := filepath.Join(tmp, binName)
	src, rerr := os.ReadFile(bin)
	if rerr != nil {
		t.Fatalf("读 sclient 二进制: %v", rerr)
	}
	if werr := os.WriteFile(target, src, 0o755); werr != nil {
		t.Fatalf("拷贝 sclient 到临时目录: %v", werr)
	}

	cmd := exec.Command(target,
		"--config", filepath.Join(tmp, "sclient.yaml"),
		"upgrade", "--to", tag, "--force",
		"--api-base", ts.URL+"/repos/cocomhub/sproxy",
		"--release-base", ts.URL,
	)
	cmd.Env = append(os.Environ(),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "xdg-config"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// upgrade 原子替换 target 后**立即 fork 新二进制**；Windows/慢 FS 上替换句柄
	// 可能尚未完全释放，首次启动会报 text file busy（ETXTBSY）——这是升级流程
	// 的固有竞态而非测试缺陷。有界重试（10 次 × 100ms 间隔）容忍该窗口。
	if rerr := runWithTextFileBusyRetry(cmd, 10, 100*time.Millisecond); rerr != nil {
		t.Fatalf("sclient upgrade --to 失败: %v\nstdout:\n%s\nstderr:\n%s", rerr, stdout.String(), stderr.String())
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读替换后二进制: %v", err)
	}
	if string(got) != "e2e-new-binary" {
		t.Fatalf("替换后内容 = %q, want e2e-new-binary", got)
	}
}

// runWithTextFileBusyRetry 执行 cmd；若启动失败报 text file busy（ETXTBSY，upgrade
// 原子替换后立即 fork 的竞态，见 TestE2E_CLI_UpgradeFull 的 flake 记录 #590），按
// retry 次 × interval 有界重试；其余错误立即返回。
func runWithTextFileBusyRetry(cmd *exec.Cmd, retries int, interval time.Duration) error {
	var lastErr error
	for i := 0; i < retries; i++ {
		if lastErr = cmd.Run(); lastErr == nil {
			return nil
		}
		if !isETXTBSY(lastErr) {
			return lastErr
		}
		time.Sleep(interval)
	}
	return lastErr
}

// isETXTBSY 判断启动失败是否为 text file busy（unix 平台 errno ETXTBSY；
// windows 无该 errno，以错误文本匹配）。
func isETXTBSY(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ETXTBSY) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "text file busy")
}

// makeE2EUpgradeArchive 构造含目标二进制的归档（linux tar.gz / windows zip）。
func makeE2EUpgradeArchive(t *testing.T, binName, content string) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(binName)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("zip write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zip close: %v", err)
		}
		return buf.Bytes()
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: binName, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("写 tar header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("写 tar 内容: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("关 gzip: %v", err)
	}
	return buf.Bytes()
}
