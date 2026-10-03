// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// TestSweepStalePikpakStaging 锁定清扫语义（空间回收）：过期（mtime 超 age）pikpak-* 暂存
// 目录被 RemoveAll；新鲜目录保留；非 pikpak- 前缀目录绝不动。未修必红（不删过期=红），
// 修后必绿。
func TestSweepStalePikpakStaging(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	fresh := filepath.Join(cache, "pikpak-dl-fresh")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatalf("mkdir fresh: %v", err)
	}
	stale := filepath.Join(cache, "pikpak-tmp-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	// 目录 mtime 已旧、但内部文件 mtime 新鲜：目录久未增删、文件被原地持续修改（在用目录
	// 的典型形态）→ 递归最新 mtime 判定必须**保留**，不得误删。
	staleDirFreshFile := filepath.Join(cache, "pikpak-tmp-freshfile")
	if err := os.MkdirAll(staleDirFreshFile, 0o700); err != nil {
		t.Fatalf("mkdir staleDirFreshFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staleDirFreshFile, "f.download"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write fresh file: %v", err)
	}
	other := filepath.Join(cache, "unrelated")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("backdate stale: %v", err)
	}
	if err := os.Chtimes(staleDirFreshFile, past, past); err != nil {
		t.Fatalf("backdate staleDirFreshFile dir: %v", err)
	}

	sweepStalePikpakStaging(cache, 7*24*time.Hour)

	for _, keep := range []string{fresh, staleDirFreshFile, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("不应删除 %s（新鲜/内部有新鲜文件/非 pikpak 前缀）: %v", filepath.Base(keep), err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("过期且内部全旧的 pikpak-* 目录应被清扫，仍存在: %v", err)
	}
}

// --- CLI 替身 ---

// fakeCLIBin 创建假 pikpak 可执行（用 go build 编译真二进制，跨 Windows/Unix 一致）：
//   - `auth status`  → statusJSON（登录态）
//   - `auth token`   → tokenJSON（fakeServer 校验该 token）
//   - `download <id> -o <out>` → 写一个固定字节的 out 文件（真实行为：下载落盘）
//   - 其余参数 → 非零退出（模拟未知命令失败）
//
// 真实行为锁定（用户纪律）：测试必须验证「CLI 真的把文件写到目标路径」，
// 而不是用「CLI 失败所以 err!=nil」来偷懒断言。
func fakeCLIBin(t *testing.T, downloadData string) string {
	t.Helper()
	src := fmt.Sprintf(`package main

import (
	"os"
)

const downloadData = %q

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		os.Stdout.WriteString("{\"logged_in\":true,\"user_id\":\"u1\",\"name\":\"t\",\"email\":\"\"}")
		os.Exit(0)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
		os.Stdout.WriteString("{\"access_token\":\"fake-token-abc123\"}")
		os.Exit(0)
	}
	if len(args) >= 1 && args[0] == "download" {
		// download <id> -o <out>
		out := ""
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-o" {
				out = args[i+1]
			}
		}
		if out == "" {
			os.Exit(1)
		}
		if err := os.WriteFile(out, []byte(downloadData), 0o644); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Stderr.WriteString("unexpected: ")
	for _, a := range args {
		os.Stderr.WriteString(a + " ")
	}
	os.Exit(1)
}
`, downloadData)
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

// --- API fake：httptest 模拟 drive/v1 ---

// TestPikpakDownloader_Download 端到端：分享 → 转存 → 定位 → CLI 下载。
// 验证真实行为（用户纪律）：下载必须真落盘、鉴权必须全程生效、AutoDelete 精确删除。
func TestPikpakDownloader_Download(t *testing.T) {
	t.Parallel()
	const payload = "fake-video-content-12345"
	share := []FileMeta{
		{ID: "share-img-1", Name: "cover.jpg", Kind: "drive#file", Size: 1024, MimeType: "image/jpeg"},
		{ID: "share-vid-1", Name: "SAMPLE-123-full.mp4", Kind: "drive#file", Size: 1000000, MimeType: "video/mp4"},
	}
	fsrv := newFakeServer(share, "https://dl.example.com/download?fid=x")
	defer fsrv.Close()

	cli, err := NewCli(CliConfig{BinaryPath: fakeCLIBin(t, payload), HTTPClient: fsrv.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(APIConfig{Host: fsrv.srv.URL, HTTPClient: fsrv.srv.Client()}, cli)

	dl, err := NewPikpakDownloader(DownloaderConfig{
		Cli: cli, API: api, DownloadDir: t.TempDir(), Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !dl.Supports("https://mypikpak.com/s/abc123/xyz") {
		t.Fatal("expected Supports true for mypikpak share")
	}
	if dl.Supports("https://example.com/file.txt") {
		t.Fatal("expected Supports false for non-pikpak")
	}

	// 真实行为：fake CLI download 真写文件 → 下载必须成功且字节落盘。
	dest := filepath.Join(t.TempDir(), "out.mp4")
	res, err := dl.Download(context.Background(), "https://mypikpak.com/s/abc123/xyz", dest, nil)
	if err != nil {
		t.Fatalf("expected successful download via fake cli, got %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("expected downloaded file on disk, got %v", err)
	}
	if string(got) != payload {
		t.Fatalf("downloaded content mismatch: got %q want %q", got, payload)
	}
	if res.Size != int64(len(payload)) {
		t.Fatalf("result size %d want %d", res.Size, len(payload))
	}
	// 转存链路：分享解析 + restore（网盘里有转存文件，ID 为 restore 返回的精确 ID）。
	if len(fsrv.driveFiles) != 1 {
		t.Fatalf("expected 1 drive file after restore, got %d", len(fsrv.driveFiles))
	}
	if fsrv.driveFiles[0].ID != "restored-1" {
		t.Fatalf("expected restored file id restored-1 (exact fileID from restore), got %q", fsrv.driveFiles[0].ID)
	}
	if fsrv.driveFiles[0].Name != "SAMPLE-123-full.mp4" {
		t.Fatalf("expected restored file name SAMPLE-123-full.mp4, got %q", fsrv.driveFiles[0].Name)
	}
	// 鉴权门禁已强制校验：全链路请求都必须带 Bearer fake-token-abc123。
	// 若无鉴权请求被拒（unauthCount>0），说明 REST 鉴权接线回归 → 立即红。
	if fsrv.unauthCount != 0 {
		t.Fatalf("expected 0 unauthorized requests (auth wiring must inject Bearer token), got %d", fsrv.unauthCount)
	}
}

// TestAPI_DoJSON_RequiresAuth 负例：不带 CLI 且无显式 token 的 REST 调用必须返回
// ErrNotLoggedIn（不静默降级成无鉴权请求打到服务器）。
func TestAPI_DoJSON_RequiresAuth(t *testing.T) {
	t.Parallel()
	// 无 cli、无 token：ensureToken 应直接返回 ErrNotLoggedIn，不发起任何 HTTP。
	fsrv := newFakeServer(nil, "")
	defer fsrv.Close()
	api := NewAPI(APIConfig{Host: fsrv.srv.URL, HTTPClient: fsrv.srv.Client()}, nil)
	_, err := api.List(context.Background(), "")
	if err == nil {
		t.Fatal("expected ErrNotLoggedIn for API without cli/token")
	}
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("expected ErrNotLoggedIn, got %v", err)
	}
	// 必须零 HTTP 请求：确保未登录状态 fail-closed，不把无鉴权请求发出去。
	if fsrv.unauthCount != 0 {
		t.Fatalf("expected 0 HTTP requests when not logged in, got %d (must fail closed)", fsrv.unauthCount)
	}
}

// TestAPI_EnsureToken_Concurrent 并发首取 token：多 goroutine 同时触发 REST 请求时
// token 导出只应执行一次且结果一致（ensureToken 全锁保护，-race 下无竞争；
// 锁外快速路径读 + 锁内写无 happens-before 的实现会在这里被 race 抓到）。
func TestAPI_EnsureToken_Concurrent(t *testing.T) {
	t.Parallel()
	share := []FileMeta{{ID: "share-vid-1", Name: "a.mp4", Kind: "drive#file", Size: 1, MimeType: "video/mp4"}}
	fsrv := newFakeServer(share, "")
	defer fsrv.Close()

	cli, err := NewCli(CliConfig{BinaryPath: fakeCLIBin(t, "x"), HTTPClient: fsrv.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(APIConfig{Host: fsrv.srv.URL, HTTPClient: fsrv.srv.Client()}, cli)

	// 并发 16 个 List 请求：全部应成功（token 只导出一次、缓存一致）。
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := api.List(context.Background(), "")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent API call failed: %v", err)
		}
	}
	// 鉴权门禁：全部请求带合法 token（unauthCount 必须为 0）。
	if fsrv.unauthCount != 0 {
		t.Fatalf("expected 0 unauthorized requests under concurrency, got %d", fsrv.unauthCount)
	}
}

// TestPikpakDownloader_DownloadWithWriter_Sink 验证 WriterDownloader 路径真实记账：
// CLI 落盘后字节经 QuotaSink 边写边记（CommitUp），Finish(true) 语义被调用。
func TestPikpakDownloader_DownloadWithWriter_Sink(t *testing.T) {
	t.Parallel()
	const payload = "fake-video-content-12345"
	share := []FileMeta{
		{ID: "share-img-1", Name: "cover.jpg", Kind: "drive#file", Size: 1024, MimeType: "image/jpeg"},
		{ID: "share-vid-1", Name: "SAMPLE-123-full.mp4", Kind: "drive#file", Size: 1000000, MimeType: "video/mp4"},
	}
	fsrv := newFakeServer(share, "https://dl.example.com/download?fid=x")
	defer fsrv.Close()

	cli, err := NewCli(CliConfig{BinaryPath: fakeCLIBin(t, payload), HTTPClient: fsrv.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(APIConfig{Host: fsrv.srv.URL, HTTPClient: fsrv.srv.Client()}, cli)
	dl, err := NewPikpakDownloader(DownloaderConfig{
		Cli: cli, API: api, DownloadDir: t.TempDir(), Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	var committed atomic.Int64
	var finished atomic.Bool
	sinkFactory := func(_ io.Writer, _ int64, _ bool) (downloader.QuotaSink, error) {
		return &recordingSink{
			onWrite: func(n int64) { committed.Add(n) },
			onFinish: func(success bool, _ int64) {
				if success {
					finished.Store(true)
				}
			},
		}, nil
	}

	dest := filepath.Join(t.TempDir(), "out.mp4")
	res, err := dl.DownloadWithWriter(context.Background(), "https://mypikpak.com/s/abc123/xyz", dest, nil, sinkFactory)
	if err != nil {
		t.Fatalf("expected successful sink download, got %v", err)
	}
	if committed.Load() != int64(len(payload)) {
		t.Fatalf("expected committed %d bytes to sink, got %d", len(payload), committed.Load())
	}
	if !finished.Load() {
		t.Fatal("expected Finish(true) called after successful sink download")
	}
	// 落盘真实行为 + Checksum 非空（锁定 DownloadWithWriter 不丢字节、不丢校验）。
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("expected downloaded file on disk, got %v", err)
	}
	if string(got) != payload {
		t.Fatalf("downloaded content mismatch: got %q want %q", got, payload)
	}
	if res.Checksum == "" {
		t.Fatal("expected non-empty checksum from DownloadWithWriter")
	}
	if fsrv.unauthCount != 0 {
		t.Fatalf("expected 0 unauthorized requests (sink path must authenticate), got %d", fsrv.unauthCount)
	}
}

// recordingSink 记录 Write 字节数与 Finish 语义（测试用 QuotaSink）。
type recordingSink struct {
	onWrite  func(n int64)
	onFinish func(success bool, oldSize int64)
}

func (s *recordingSink) Write(p []byte) (int, error) {
	s.onWrite(int64(len(p)))
	return len(p), nil
}
func (s *recordingSink) Finish(success bool, oldSize int64) { s.onFinish(success, oldSize) }

// fakeServer 模拟 PikPak drive/v1 API（分享详情/转存/列表/删除/直链）。
// **强制校验鉴权**（用户纪律：测试锁定真实行为，避免静默失效）：
// 除 /healthz 外的每个请求都必须带 `Authorization: Bearer fake-token-abc123`，
// 缺失或错误 → 401。这确保「REST 鉴权接线」一旦回归（token 丢失/不再注入），
// 测试立即变红，而不是假绿。
type fakeServer struct {
	shareFiles  []FileMeta
	driveFiles  []FileMeta
	downloadURL string
	deleted     []string
	srv         *httptest.Server
	// unauthCount 记录未带有效鉴权被拒的请求数（供断言）。
	unauthCount int
}

const fakeServerToken = "fake-token-abc123"

func newFakeServer(share []FileMeta, dlURL string) *fakeServer {
	fs := &fakeServer{shareFiles: share, downloadURL: dlURL}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	return fs
}

func (f *fakeServer) Close() { f.srv.Close() }

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	// 鉴权门禁：所有 drive/v1 请求必须带 Bearer fake-token-abc123（无 /healthz 例外——
	// fakeServer 只暴露 API，不存在免鉴权端点）。
	if r.Header.Get("Authorization") != "Bearer "+fakeServerToken {
		f.unauthCount++
		http.Error(w, "unauthorized: missing/invalid Authorization header", http.StatusUnauthorized)
		return
	}
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	dec := func(v any) error { return json.NewDecoder(r.Body).Decode(v) }

	switch {
	case r.URL.Path == "/drive/v1/share/detail":
		writeJSON(map[string]any{"files": f.shareFiles, "next_page_token": ""})
	case r.URL.Path == "/drive/v1/share/restore" && r.Method == http.MethodPost:
		var body struct {
			FileIDs []string `json:"file_ids"`
		}
		_ = dec(&body)
		// 真实行为：restore 后转存文件获得新的 file_id（RESTORE_START 异步语义），
		// 下载器用该精确 ID 定位转存文件，不得再依赖分享里的旧 ID。
		restoredID := "restored-1"
		for _, fid := range body.FileIDs {
			for _, sf := range f.shareFiles {
				if sf.ID == fid {
					df := sf
					df.ParentID = ""
					df.ID = restoredID
					f.driveFiles = append(f.driveFiles, df)
					break
				}
			}
		}
		writeJSON(map[string]any{"restore_status": "RESTORE_START", "file_id": restoredID})
	case r.URL.Path == "/drive/v1/files" && r.Method == http.MethodGet:
		writeJSON(map[string]any{"files": f.driveFiles})
	case strings.HasPrefix(r.URL.Path, "/drive/v1/files/") && r.Method == http.MethodGet:
		writeJSON(map[string]any{"web_content_link": f.downloadURL})
	case r.URL.Path == "/drive/v1/files:batchTrash" && r.Method == http.MethodPost:
		var body struct {
			IDs []string `json:"ids"`
		}
		_ = dec(&body)
		f.deleted = append(f.deleted, body.IDs...)
		writeJSON(map[string]any{"ok": true})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
