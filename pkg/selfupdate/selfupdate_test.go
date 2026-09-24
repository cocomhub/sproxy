// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- AssetName：平台扩展表（变异：恒 .tar.gz → 红）----

func TestAssetName_PlatformExtensions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		goos    string
		goarch  string
		want    string
	}{
		{"v0.18.0", "linux", "amd64", "sproxy_v0.18.0_linux_amd64.tar.gz"},
		{"0.18.0", "linux", "arm64", "sproxy_v0.18.0_linux_arm64.tar.gz"},
		{"v0.18.0", "darwin", "amd64", "sproxy_v0.18.0_darwin_amd64.tar.gz"},
		{"v0.18.0", "windows", "amd64", "sproxy_v0.18.0_windows_amd64.zip"},
		{"v0.18.0", "windows", "arm64", "sproxy_v0.18.0_windows_arm64.zip"},
	}
	for _, tt := range tests {
		if got := AssetName(tt.version, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("AssetName(%q,%q,%q) = %q, want %q", tt.version, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

// ---- NormalizeVersion：v 前缀归一 + SNAPSHOT/dirty 无法判定（变异：不补 v → 红）----

func TestNormalizeVersion_VPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"v0.18.0", "v0.18.0"},
		{"0.18.0", "v0.18.0"},
		{"v1.2.3", "v1.2.3"},
		{"  v0.18.0  ", "v0.18.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeVersion(tt.in); got != tt.want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNormalizeVersion_SnapshotDirtyUnparsable 快照/脏构建视为无法判定（返回空串，
// CLI 层降级为「无法判定，--force 可强升」，绝不 panic）。
func TestNormalizeVersion_SnapshotDirtyUnparsable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"v0.18.0-SNAPSHOT-abc1234", ""},
		{"0.18.0-SNAPSHOT-abc1234", ""},
		{"v0.18.0-1-gabc1234-dirty", ""},
	}
	for _, tt := range tests {
		if got := NormalizeVersion(tt.in); got != tt.want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q（快照/脏构建应判无法判定）", tt.in, got, tt.want)
		}
	}
}

// ---- CompareVersions：MAJOR.MINOR.PATCH 数值比较，预发布段视为旧（变异：反转比较符 → 红）----

func TestCompareVersions_Order(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a    string
		b    string
		want int
	}{
		{"v0.18.0", "v0.17.0", 1},
		{"v0.17.0", "v0.18.0", -1},
		{"v0.18.0", "v0.18.0", 0},
		{"v0.18.1", "v0.18.0", 1},
		{"v0.19.0", "v0.18.99", 1},
		{"v0.18.0", "v1.0.0", -1},
		// 预发布段视为旧：0.18.0-beta < 0.18.0
		{"v0.18.0-beta", "v0.18.0", -1},
		{"v0.18.0", "v0.18.0-beta", 1},
		// git describe 计数段（v0.18.0-1-gabc1234）是 tag 之后的构建，视为同版本线
		{"v0.18.0-1-gabc1234", "v0.18.0", 0},
	}
	for _, tt := range tests {
		got, err := CompareVersions(tt.a, tt.b)
		if err != nil {
			t.Errorf("CompareVersions(%q,%q) 意外错误: %v", tt.a, tt.b, err)
			continue
		}
		if got != tt.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompareVersions_Unparsable(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"dev", "abc1234", "vx.y.z", ""} {
		if _, err := CompareVersions(v, "v0.18.0"); err == nil {
			t.Errorf("CompareVersions(%q, v0.18.0) 应返回错误（不可解析）", v)
		}
	}
}

// ---- FindAsset：精确文件名匹配（变异：子串匹配 → 红）----

func TestFindAsset_ExactMatch(t *testing.T) {
	t.Parallel()
	rel := &Release{
		TagName: "v0.18.0",
		Assets: []Asset{
			{Name: "sproxy_v0.18.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://cdn.example/a.tar.gz"},
			{Name: "sproxy_v0.18.0_windows_amd64.zip", BrowserDownloadURL: "https://cdn.example/a.zip"},
			{Name: "checksums.txt", BrowserDownloadURL: "https://cdn.example/checksums.txt"},
		},
	}
	got, err := FindAsset(rel, "linux", "amd64")
	if err != nil {
		t.Fatalf("FindAsset: %v", err)
	}
	if got.Name != "sproxy_v0.18.0_linux_amd64.tar.gz" {
		t.Fatalf("got asset %q", got.Name)
	}
}

// TestFindAsset_NoSubstringMatch 精确匹配：名称近似（不同 goarch）不得命中。
func TestFindAsset_NoSubstringMatch(t *testing.T) {
	t.Parallel()
	rel := &Release{
		TagName: "v0.18.0",
		Assets: []Asset{
			{Name: "sproxy_v0.18.0_linux_arm64.tar.gz"},
			{Name: "sproxy_v0.18.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://cdn.example/x"},
		},
	}
	if _, err := FindAsset(rel, "windows", "amd64"); err == nil {
		t.Fatal("windows 平台无资产应报错（该平台无发布产物）")
	}
	got, err := FindAsset(rel, "linux", "amd64")
	if err != nil {
		t.Fatalf("FindAsset linux/amd64: %v", err)
	}
	if got.Name != "sproxy_v0.18.0_linux_amd64.tar.gz" {
		t.Fatalf("精确匹配应命中 linux/amd64，got %q", got.Name)
	}
}

// ---- Checksums 解析：CRLF / 含 deb、rpm 行 / 大小写不敏感 / 缺条目报错 ----

func TestParseChecksums_CRLFAndExtras(t *testing.T) {
	t.Parallel()
	data := []byte("aabbccdd  sproxy_v0.18.0_linux_amd64.tar.gz\r\n" +
		"11223344  sproxy_v0.18.0_windows_amd64.zip\r\n" +
		"55667788  sproxy_0.18.0_amd64.deb\r\n" +
		"99aabbcc  sproxy_0.18.0_x86_64.rpm\r\n")
	sums, err := ParseChecksums(data)
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if len(sums) != 4 {
		t.Fatalf("应解析出 4 条（含 deb/rpm），got %d", len(sums))
	}
	if sums["sproxy_v0.18.0_linux_amd64.tar.gz"] != "aabbccdd" {
		t.Errorf("linux 资产校验和解析错误: %v", sums)
	}
	// 大小写不敏感查找（checksum 文件名大小写不敏感匹配）
	if v, ok := LookupChecksum(sums, "SPROXY_V0.18.0_WINDOWS_AMD64.ZIP"); !ok || v != "11223344" {
		t.Errorf("大小写不敏感查找失败: %v", sums)
	}
}

func TestParseChecksums_MissingEntry(t *testing.T) {
	t.Parallel()
	sums := map[string]string{"other.txt": "abc"}
	if _, ok := LookupChecksum(sums, "sproxy_v0.18.0_linux_amd64.tar.gz"); ok {
		t.Fatal("缺条目应返回 not ok（fail-closed）")
	}
}

// ---- DownloadAndVerify：SHA-256 校验 fail-closed（变异：去掉校验调用 → 红）----

func TestDownloadAndVerify_MatchingHash(t *testing.T) {
	t.Parallel()
	content := []byte("fake sclient binary")
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	c := New(ts.URL)
	if err := c.DownloadAndVerify(context.Background(), ts.URL, dest, want); err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读下载文件: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("下载内容不一致")
	}
}

// TestDownloadAndVerify_TamperedBytesFails 篡改字节（wantSHA 与内容不符）→ 报错且
// 临时文件被删除（fail-closed，不留下未校验产物）。
func TestDownloadAndVerify_TamperedBytesFails(t *testing.T) {
	t.Parallel()
	content := []byte("tampered bytes")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	c := New(ts.URL)
	if err := c.DownloadAndVerify(context.Background(), ts.URL, dest, "deadbeef"); err == nil {
		t.Fatal("校验和不匹配应报错（fail-closed）")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("校验失败后临时文件应被删除，got stat err=%v", err)
	}
}

func TestDownloadAndVerify_Non200(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	c := New(ts.URL)
	if err := c.DownloadAndVerify(context.Background(), ts.URL, dest, "abc"); err == nil {
		t.Fatal("HTTP 5xx 应报错")
	}
}

// ---- Latest / ByTag：httptest 假 API（URL 与解析）----

func TestClient_Latest(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/cocomhub/sproxy/releases/latest" {
			t.Errorf("Latest 请求路径 = %q", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"tag_name":"v0.18.0","assets":[{"name":"sproxy_v0.18.0_linux_amd64.tar.gz","browser_download_url":"https://cdn/x"}]}`)
	}))
	defer ts.Close()

	c := New(ts.URL + "/repos/cocomhub/sproxy")
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v0.18.0" {
		t.Fatalf("tag_name = %q", rel.TagName)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Name != "sproxy_v0.18.0_linux_amd64.tar.gz" {
		t.Fatalf("assets 解析错误: %+v", rel.Assets)
	}
}

func TestClient_ByTag(t *testing.T) {
	t.Parallel()
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/repos/cocomhub/sproxy/releases/tags/v0.17.0" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"tag_name":"v0.17.0","assets":[]}`)
	}))
	defer ts.Close()

	c := New(ts.URL + "/repos/cocomhub/sproxy")
	rel, err := c.ByTag(context.Background(), "v0.17.0")
	if err != nil {
		t.Fatalf("ByTag: %v", err)
	}
	if rel.TagName != "v0.17.0" {
		t.Fatalf("tag_name = %q", rel.TagName)
	}
	if gotPath != "/repos/cocomhub/sproxy/releases/tags/v0.17.0" {
		t.Fatalf("ByTag 路径 = %q", gotPath)
	}
}

func TestClient_ByTagNotFound(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	c := New(ts.URL + "/repos/cocomhub/sproxy")
	_, err := c.ByTag(context.Background(), "v9.9.9")
	if !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("ByTag 404 应返回 ErrVersionNotFound, got %v", err)
	}
}

// ---- ExtractBinary：tar.gz / zip / zip-slip / symlink / windows exe ----

// makeTarGz 构造含 sclient（及干扰条目）的 tar.gz 归档字节。
func makeTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("写 tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("写 tar 内容: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("关 gzip: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBinary_TarGz(t *testing.T) {
	t.Parallel()
	data := makeTarGz(t, map[string]string{
		"sclient":             "new-binary-content",
		"sproxy":              "server-binary",
		"docs/README.md":      "readme",
		"config.example.yaml": "config",
	})
	dest := filepath.Join(t.TempDir(), "sclient")
	if err := ExtractBinary(bytes.NewReader(data), "linux", dest); err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读解包文件: %v", err)
	}
	if string(got) != "new-binary-content" {
		t.Fatalf("解包内容 = %q", got)
	}
}

func TestExtractBinary_WindowsExe(t *testing.T) {
	t.Parallel()
	// windows 走 zip；含 sclient.exe 与干扰条目。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"sclient.exe":    "win-binary",
		"sproxy.exe":     "server",
		"README.md":      "readme",
		"docs/README.md": "docs",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "sclient.exe")
	if err := ExtractBinary(bytes.NewReader(buf.Bytes()), "windows", dest); err != nil {
		t.Fatalf("ExtractBinary(windows): %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读解包文件: %v", err)
	}
	if string(got) != "win-binary" {
		t.Fatalf("解包内容 = %q", got)
	}
}

// TestExtractBinary_ZipSlipRejected 路径穿越条目（../evil）→ 整体失败（防 zip-slip）。
func TestExtractBinary_ZipSlipRejected(t *testing.T) {
	t.Parallel()
	data := makeTarGz(t, map[string]string{
		"../evil": "escape",
		"sclient": "ok",
	})
	dest := filepath.Join(t.TempDir(), "sclient")
	err := ExtractBinary(bytes.NewReader(data), "linux", dest)
	if err == nil {
		t.Fatal("含 ../ 条目应拒绝（zip-slip）")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Fatalf("错误应说明路径越界, got: %v", err)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Fatalf("zip-slip 失败后不应留下解包产物, stat err=%v", serr)
	}
}

func TestExtractBinary_AbsolutePathRejected(t *testing.T) {
	t.Parallel()
	data := makeTarGz(t, map[string]string{
		"/etc/passwd": "escape",
		"sclient":     "ok",
	})
	dest := filepath.Join(t.TempDir(), "sclient")
	if err := ExtractBinary(bytes.NewReader(data), "linux", dest); err == nil {
		t.Fatal("绝对路径条目应拒绝")
	}
}

// TestExtractBinary_SymlinkRejected tar 符号链接条目（TypeSymlink）→ 拒绝。
func TestExtractBinary_SymlinkRejected(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "sclient", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatalf("写 symlink header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("关 tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("关 gzip: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "sclient")
	if err := ExtractBinary(bytes.NewReader(buf.Bytes()), "linux", dest); err == nil {
		t.Fatal("符号链接条目应拒绝")
	}
}

// ---- SwapBinary：原子替换 + Windows 备份链（注入 rename 错误）+ 并发锁 ----

// TestSwapBinary_BackupChain 直接覆盖失败（如 Windows 运行中 exe）→
// target → target.old → 换入成功 → .old 清理。
func TestSwapBinary_BackupChain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "new")
	target := filepath.Join(dir, "sclient")
	if err := os.WriteFile(tmp, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	fakeRename := func(old, new string) error {
		calls++
		if calls == 1 {
			return errors.New("target busy（模拟 Windows 运行中 exe 覆盖失败）")
		}
		return os.Rename(old, new)
	}
	if err := swapBinaryWith(tmp, target, fakeRename); err != nil {
		t.Fatalf("swapBinaryWith 备份链: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读替换后 target: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("target 内容 = %q, want new-binary", got)
	}
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".old 应被清理, stat err=%v", err)
	}
}

// TestSwapBinary_RollbackOnFailedSwap 备份成功但换入失败 → 回滚（target 恢复旧内容）。
func TestSwapBinary_RollbackOnFailedSwap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "new")
	target := filepath.Join(dir, "sclient")
	if err := os.WriteFile(tmp, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeRename := func(old, new string) error {
		// 第 1 次（tmp→target）失败；第 2 次（target→old）成功；第 3 次（tmp→target）再失败。
		if strings.HasSuffix(old, "new") && strings.HasSuffix(new, "sclient") {
			return errors.New("injected fail")
		}
		return os.Rename(old, new)
	}
	// 上面 fake 会让 第1次失败、第2次成功、第3次失败 → 触发回滚（old→target）。
	if err := swapBinaryWith(tmp, target, fakeRename); err == nil {
		t.Fatal("换入失败应返回错误")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回滚后 target: %v", err)
	}
	if string(got) != "old-binary" {
		t.Fatalf("回滚后 target 应恢复旧内容, got %q", got)
	}
}

// TestSwapBinary_FallbackBat 直接覆盖与备份链全失败 → 返回提示错误；Windows 上
// 额外写 .upgrade.bat 两段式兜底（提示退出后运行）。
func TestSwapBinary_FallbackBat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "new")
	target := filepath.Join(dir, "sclient")
	if err := os.WriteFile(tmp, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	failAll := func(old, new string) error { return errors.New("injected total fail") }
	err := swapBinaryWith(tmp, target, failAll)
	if err == nil {
		t.Fatal("全部 rename 失败应返回错误")
	}
	if !strings.Contains(err.Error(), "替换失败") {
		t.Fatalf("错误应提示替换失败, got: %v", err)
	}
	if _, serr := os.Stat(target + ".upgrade.bat"); serr == nil {
		// Windows 上 .bat 兜底已写
	} else if os.IsNotExist(serr) && runtime_GOOS() == "windows" {
		t.Fatalf("Windows 上应写 .upgrade.bat 兜底文件")
	}
}

func runtime_GOOS() string { return goosVar }

// TestSwapBinary_LockPreventsConcurrent 并发防重入：lock 文件存在 → 拒绝第二升级。
func TestSwapBinary_LockPreventsConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "sclient")
	lock := target + ".upgrade.lock"
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := swapBinaryWith(filepath.Join(dir, "new"), target, os.Rename)
	if err == nil {
		t.Fatal("lock 存在时第二升级应被拒绝")
	}
	if !strings.Contains(err.Error(), "正在进行") {
		t.Fatalf("错误应提示另一升级进行中, got: %v", err)
	}
}

// TestSwapBinary_LockReleased 成功后 lock 被清理（后续升级可重入）。
func TestSwapBinary_LockReleased(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "new")
	target := filepath.Join(dir, "sclient")
	if err := os.WriteFile(tmp, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := swapBinaryWith(tmp, target, os.Rename); err != nil {
		t.Fatalf("swapBinaryWith: %v", err)
	}
	if _, err := os.Stat(target + ".upgrade.lock"); !os.IsNotExist(err) {
		t.Fatalf("成功升级后 lock 应删除, stat err=%v", err)
	}
}
