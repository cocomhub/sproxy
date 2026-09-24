// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// newUpgradeServer 构造 upgrade 命令的 httptest 全套假端点：
//   - {api}/releases/latest → 指定 tag 的 Release JSON；
//   - {release}/download/{tag}/checksums.txt → checksums；
//   - {release}/download/{tag}/sproxy_{ver}_{goos}_{goarch}.tar.gz → 含 sclient 的归档。
//
// 返回 (server, cleanup)。归档与 checksums 内容一致（校验和真实计算）。
func newUpgradeServer(t *testing.T, tag, goos, goarch string) *httptest.Server {
	t.Helper()
	assetName := assetNameFor(tag, goos, goarch)
	archiveBytes := makeUpgradeArchive(t, goos, "fake-sclient-binary")
	sum := sha256.Sum256(archiveBytes)
	wantSum := hex.EncodeToString(sum[:])
	checksums := wantSum + "  " + assetName + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/cocomhub/sproxy/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": "http://" + r.Host + "/download/" + tag + "/" + assetName},
			},
		})
	})
	mux.HandleFunc("/repos/cocomhub/sproxy/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": "http://" + r.Host + "/download/" + tag + "/" + assetName},
			},
		})
	})
	mux.HandleFunc("/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, checksums)
	})
	mux.HandleFunc("/download/"+tag+"/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archiveBytes)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// assetNameFor 生成测试资产名（与 selfupdate.AssetName 对齐，避免测试依赖包内函数）。
func assetNameFor(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "sproxy_" + version + "_" + goos + "_" + goarch + ext
}

// makeUpgradeArchive 构造含 sclient（windows 为 sclient.exe）的归档（tar.gz / zip）。
func makeUpgradeArchive(t *testing.T, goos, content string) []byte {
	t.Helper()
	binName := "sclient"
	if goos == "windows" {
		binName += ".exe"
	}
	var buf bytes.Buffer
	if goos == "windows" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(binName)
		if err != nil {
			t.Fatalf("zip create %s: %v", binName, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("zip write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("zip close: %v", err)
		}
		return buf.Bytes()
	}
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

// newUpgradeCmdForTest 构造 upgrade 命令（ios 输出可捕获；api-base 指向假服务器）。
func newUpgradeCmdForTest(ios cli.IOStreams, apiBase string, currentVersion string) *cobra.Command {
	return newUpgradeCmdWithVersion(ios, apiBase, currentVersion)
}

// newUpgradeCmdWithVersion 构造 upgrade 命令并注入当前版本（不走 buildinfo 包级变量）。
func newUpgradeCmdWithVersion(ios cli.IOStreams, apiBase, currentVersion string) *cobra.Command {
	cmd := newUpgradeCmd(ios, upgradeDeps{version: func() string { return currentVersion }})
	_ = cmd.Flags().Set("api-base", apiBase)
	// ReleaseBase 默认指向 GitHub CDN；测试用假服务器 URL（httptest 派生）。
	_ = cmd.Flags().Set("release-base", strings.TrimSuffix(apiBase, "/repos/cocomhub/sproxy"))
	return cmd
}

// newUpgradeCmdWithTarget 构造 upgrade 命令并注入当前版本 + 目标二进制路径。
func newUpgradeCmdWithTarget(ios cli.IOStreams, apiBase, currentVersion, target string) *cobra.Command {
	cmd := newUpgradeCmd(ios, upgradeDeps{
		version: func() string { return currentVersion },
		exec:    func() (string, error) { return target, nil },
	})
	_ = cmd.Flags().Set("api-base", apiBase)
	// ReleaseBase 默认指向 GitHub CDN；测试用假服务器 URL（httptest 派生）。
	_ = cmd.Flags().Set("release-base", strings.TrimSuffix(apiBase, "/repos/cocomhub/sproxy"))
	return cmd
}

// TestUpgradeCmd_UseAndFlags 验证命令注册形态与 flags。
func TestUpgradeCmd_UseAndFlags(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, "http://127.0.0.1:0", "v0.17.0")
	if cmd.Use != "upgrade" {
		t.Fatalf("Use = %q, want upgrade", cmd.Use)
	}
	for _, name := range []string{"check", "to", "force", "api-base"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("upgrade 缺少 flag --%s", name)
		}
	}
}

// TestUpgradeCmd_CheckLatest 全链路 --check：假 API 返回更新版本 → 输出含版本号
// 与「有更新」提示；JSON 含 update_available:true（变异：up-to-date 判定条件反转 → 红）。
func TestUpgradeCmd_CheckLatest(t *testing.T) {
	t.Parallel()
	ts := newUpgradeServer(t, "v0.18.0", "linux", "amd64")
	var buf strings.Builder
	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, ts.URL+"/repos/cocomhub/sproxy", "v0.17.0")
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upgrade --check 失败: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "v0.18.0") {
		t.Fatalf("输出应包含最新版本 v0.18.0, got: %q", out)
	}
	if !strings.Contains(out, "发现新版本") {
		t.Fatalf("输出应提示发现新版本, got: %q", out)
	}
}

// TestUpgradeCmd_CheckUpToDate 已最新 → 提示「已是最新」；JSON 含 update_available:false。
func TestUpgradeCmd_CheckUpToDate(t *testing.T) {
	t.Parallel()
	ts := newUpgradeServer(t, "v0.18.0", "linux", "amd64")
	var buf strings.Builder
	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, ts.URL+"/repos/cocomhub/sproxy", "v0.18.0")
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upgrade --check 失败: %v", err)
	}
	if !strings.Contains(buf.String(), "已是最新") {
		t.Fatalf("输出应提示已是最新, got: %q", buf.String())
	}
}

// TestUpgradeCmd_CheckJSON 脚本解析：--json 输出含 current/latest/update_available。
func TestUpgradeCmd_CheckJSON(t *testing.T) {
	t.Parallel()
	ts := newUpgradeServer(t, "v0.18.0", "linux", "amd64")
	var buf strings.Builder
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, ts.URL+"/repos/cocomhub/sproxy", "v0.17.0")
	root.AddCommand(cmd)
	root.SetArgs([]string{"upgrade", "--check", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("upgrade --check --json 失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("期望纯 JSON 输出, got: %q (err=%v)", buf.String(), err)
	}
	if out["update_available"] != true {
		t.Fatalf("update_available 应为 true, got: %v", out)
	}
	if out["current"] != "v0.17.0" {
		t.Fatalf("current = %v", out["current"])
	}
	if out["latest"] != "v0.18.0" {
		t.Fatalf("latest = %v", out["latest"])
	}
}

// TestUpgradeCmd_FullUpgrade 完整升级：--to 指定版本 → 下载/校验/解包/替换自身。
// 用注入的当前二进制路径（临时目录中的假 sclient）验证替换发生。
// 注：runUpgradeFor 直接用 runtime.GOOS/GOARCH 匹配资产，假服务器按 runner 平台
// 构造对应归档（linux→tar.gz、windows→zip），跨平台可跑。
func TestUpgradeCmd_FullUpgrade(t *testing.T) {
	t.Parallel()
	goos, goarch := runtime.GOOS, runtime.GOARCH
	ts := newUpgradeServer(t, "v0.18.0", goos, goarch)

	dir := t.TempDir()
	target := filepath.Join(dir, "sclient")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	cmd := newUpgradeCmdWithTarget(cli.IOStreams{Out: &buf, ErrOut: io.Discard},
		ts.URL+"/repos/cocomhub/sproxy", "v0.17.0", target)
	cmd.SetArgs([]string{"--to", "v0.18.0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upgrade --to v0.18.0 失败: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读替换后二进制: %v", err)
	}
	if string(got) != "fake-sclient-binary" {
		t.Fatalf("替换后内容 = %q, want fake-sclient-binary", got)
	}
	if !strings.Contains(buf.String(), "已升级") {
		t.Fatalf("输出应提示升级完成, got: %q", buf.String())
	}
}

// TestUpgradeCmd_UpToDateNoForce 已最新且无 --force → 提示性 exit 0（不替换）。
func TestUpgradeCmd_UpToDateNoForce(t *testing.T) {
	t.Parallel()
	ts := newUpgradeServer(t, "v0.18.0", "linux", "amd64")

	dir := t.TempDir()
	target := filepath.Join(dir, "sclient")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	cmd := newUpgradeCmdWithTarget(cli.IOStreams{Out: &buf, ErrOut: io.Discard},
		ts.URL+"/repos/cocomhub/sproxy", "v0.18.0", target)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("已最新应 exit 0（提示性）, err=%v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old-binary" {
		t.Fatalf("已最新不应替换二进制, got %q", got)
	}
}

// TestUpgradeCmd_CurrentUnparsable 当前版本 SNAPSHOT/dirty → 无法判定；升级需 --force。
func TestUpgradeCmd_CurrentUnparsable(t *testing.T) {
	t.Parallel()
	ts := newUpgradeServer(t, "v0.18.0", "linux", "amd64")
	var buf strings.Builder
	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, ts.URL+"/repos/cocomhub/sproxy", "v0.18.0-SNAPSHOT-abc")
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--check 应恒 exit 0, err=%v", err)
	}
	if !strings.Contains(buf.String(), "无法判定") {
		t.Fatalf("SNAPSHOT 当前版本应提示无法判定, got: %q", buf.String())
	}
}

// TestUpgradeCmd_VersionNotFound --to 不存在的 tag → 明确错误「版本不存在」。
func TestUpgradeCmd_VersionNotFound(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	cmd := newUpgradeCmdForTest(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, ts.URL+"/repos/cocomhub/sproxy", "v0.17.0")
	cmd.SetArgs([]string{"--to", "v9.9.9"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--to 不存在版本应报错")
	}
	if !strings.Contains(err.Error(), "版本不存在") {
		t.Fatalf("错误应提示版本不存在, got: %v", err)
	}
}
