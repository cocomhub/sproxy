// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// srcMock 是迁移源机 mock：GET /api/files（subdir 递归）+ GET /download。
// 记录请求顺序，供步骤序列断言。
type srcMock struct {
	t     *testing.T
	dir   string
	mu    sync.Mutex
	order []string
	// listCSOverride 非空时 list 报告该 checksum（模拟索引与实时内容漂移）。
	listCSOverride string
}

func (s *srcMock) record(step string) {
	s.mu.Lock()
	s.order = append(s.order, step)
	s.mu.Unlock()
}

func (s *srcMock) steps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *srcMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		sub := r.URL.Query().Get("subdir")
		s.record("list:" + sub)
		base := filepath.Join(s.dir, filepath.FromSlash(sub))
		entries, err := os.ReadDir(base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var files []client.FileInfo
		for _, e := range entries {
			info, _ := e.Info()
			fi := client.FileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().UnixNano(), IsDir: e.IsDir()}
			if !e.IsDir() {
				data, _ := os.ReadFile(filepath.Join(base, e.Name()))
				sum := sha256.Sum256(data)
				fi.Checksum = hex.EncodeToString(sum[:])
				if s.listCSOverride != "" {
					fi.Checksum = s.listCSOverride
				}
			}
			files = append(files, fi)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		s.record("download:" + name)
		data, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(name)))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		_, _ = w.Write(data)
	})
	return mux
}

// dstMock 是迁移目标机 mock：/healthz 探活 + POST /upload + HEAD /api/files/stat。
type dstMock struct {
	t     *testing.T
	dir   string
	mu    sync.Mutex
	order []string
	// uploadFail 为指定 remote 名时返回 500（模拟上传失败）。
	uploadFail map[string]bool
	// uploadCorrupt 为指定 remote 名时写入损坏内容（模拟目标端写坏）。
	uploadCorrupt map[string]bool
}

func (d *dstMock) record(step string) {
	d.mu.Lock()
	d.order = append(d.order, step)
	d.mu.Unlock()
}

func (d *dstMock) steps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.order...)
}

func (d *dstMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		d.record("healthz")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		remote := r.Header.Get("X-File-Path")
		d.record("upload:" + remote)
		if d.uploadFail[remote] {
			http.Error(w, `{"success":false,"message":"simulated failure"}`, http.StatusInternalServerError)
			return
		}
		cs := r.Header.Get("X-File-Checksum")
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		outPath := filepath.Join(d.dir, filepath.FromSlash(remote))
		if mkErr := os.MkdirAll(filepath.Dir(outPath), 0o755); mkErr != nil {
			http.Error(w, mkErr.Error(), http.StatusInternalServerError)
			return
		}
		out, err := os.Create(outPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer out.Close()
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(out, h), f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		serverCS := hex.EncodeToString(h.Sum(nil))
		if serverCS != cs {
			http.Error(w, `{"success":false,"message":"checksum mismatch"}`, http.StatusBadRequest)
			return
		}
		// 目标端写坏：上传成功后立即覆写为错误内容（模拟目标存储篡改）。
		if d.uploadCorrupt[remote] {
			_ = os.WriteFile(outPath, []byte("corrupted"), 0o644)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok", "file_checksum": serverCS})
	})
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		d.record("stat:" + name)
		data, err := os.ReadFile(filepath.Join(d.dir, filepath.FromSlash(name)))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// seedSrc 在源目录写入文件（a.txt 顶层 + dir/b.bin 子目录）。
func seedSrc(t *testing.T, dir string) map[string]string {
	t.Helper()
	want := map[string]string{
		"a.txt":     "hello",
		"dir/b.bin": "0123456789",
	}
	for rel, content := range want {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return want
}

// runExport 执行导出并断言 manifest 条目与落盘内容。
func runExport(t *testing.T, src *srcMock, srcURL, outDir string, want map[string]string) *Manifest {
	t.Helper()
	ex := &Exporter{Client: client.NewFileClient(srcURL), OutDir: outDir}
	m, err := ex.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(m.Files) != len(want) {
		t.Fatalf("manifest 条目数 = %d, want %d: %+v", len(m.Files), len(want), m.Files)
	}
	for _, f := range m.Files {
		local := filepath.Join(outDir, "files", filepath.FromSlash(f.Name))
		data, err := os.ReadFile(local)
		if err != nil {
			t.Fatalf("导出文件 %s 未落盘: %v", f.Name, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != f.Checksum {
			t.Errorf("导出文件 %s checksum 不匹配: %s", f.Name, got)
		}
		if string(data) != want[f.Name] {
			t.Errorf("导出文件 %s 内容 = %q, want %q", f.Name, data, want[f.Name])
		}
	}
	return m
}

// TestExportImport_FullFlow 断言 export→import 全流程：
// 步骤顺序（list→download→healthz→stat→upload→stat）、文件数/字节一致、
// 目标文件内容与源一致、checksum 复核通过。
func TestExportImport_FullFlow(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := seedSrc(t, srcDir)
	src := &srcMock{t: t, dir: srcDir}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	runExport(t, src, srcTS.URL, outDir, want)

	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: outDir}
	sum, err := im.Import(context.Background())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if sum.Imported != 2 || sum.Conflict != 0 || sum.Failed != 0 || sum.Skipped != 0 {
		t.Errorf("导入计数不一致: %+v", sum)
	}
	if sum.ImportedBytes != int64(len("hello")+len("0123456789")) {
		t.Errorf("导入字节 = %d, want 15", sum.ImportedBytes)
	}
	for rel, content := range want {
		data, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("目标文件 %s 缺失: %v", rel, err)
		}
		if string(data) != content {
			t.Errorf("目标文件 %s 内容 = %q, want %q", rel, data, content)
		}
	}
	// 步骤序列：导出（list 根 → list dir → 下载 a → 下载 b），导入（探活 → stat → upload → stat 复核）
	wantOrder := []string{"list:/", "list:/dir", "download:a.txt", "download:dir/b.bin", "healthz", "stat:a.txt", "upload:a.txt", "stat:a.txt"}
	got := append(append([]string{}, src.steps()...), dst.steps()...)
	for i, w := range wantOrder {
		if i >= len(got) || got[i] != w {
			t.Fatalf("步骤顺序不符: got %v, want prefix %v", got, wantOrder)
		}
	}
	var sawUpload, sawRecheck bool
	for _, s := range dst.steps() {
		if strings.HasPrefix(s, "upload:") {
			sawUpload = true
		}
		if strings.HasPrefix(s, "stat:a.txt") {
			sawRecheck = true
		}
	}
	if !sawUpload || !sawRecheck {
		t.Errorf("导入缺少 upload 或 stat 复核步骤: %v", dst.steps())
	}
}

// TestImport_Idempotent 断言二次导入全 SKIPPED（同 checksum 幂等跳过）。
func TestImport_Idempotent(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := seedSrc(t, srcDir)
	src := &srcMock{t: t, dir: srcDir}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	runExport(t, src, srcTS.URL, outDir, want)
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: outDir}
	if _, err := im.Import(context.Background()); err != nil {
		t.Fatalf("首次导入: %v", err)
	}
	uploads := dst.steps()
	sum, err := im.Import(context.Background())
	if err != nil {
		t.Fatalf("二次导入: %v", err)
	}
	if sum.Imported != 0 || sum.Skipped != 2 {
		t.Errorf("二次导入应全 SKIPPED: %+v", sum)
	}
	for _, s := range dst.steps()[len(uploads):] {
		if strings.HasPrefix(s, "upload:") {
			t.Errorf("二次导入不应再 upload（幂等）: %v", dst.steps()[len(uploads):])
		}
	}
}

// TestImport_Conflict 断言目标同名不同 checksum → CONFLICT 且非零退出（fail-closed）。
func TestImport_Conflict(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := seedSrc(t, srcDir)
	src := &srcMock{t: t, dir: srcDir}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dstDir, "a.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	runExport(t, src, srcTS.URL, outDir, want)
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: outDir}
	sum, err := im.Import(context.Background())
	if err == nil {
		t.Fatalf("冲突应报错（非零退出），sum=%+v", sum)
	}
	if sum.Conflict != 1 {
		t.Errorf("冲突计数 = %d, want 1: %+v", sum.Conflict, sum)
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("冲突错误应为 ErrConflict 哨兵，got %v", err)
	}
}

// TestImport_ManifestMissing 断言 manifest 缺失 → 拒绝导入。
func TestImport_ManifestMissing(t *testing.T) {
	t.Parallel()
	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: t.TempDir()}
	if _, err := im.Import(context.Background()); err == nil {
		t.Fatal("manifest 缺失应拒绝导入")
	}
}

// TestImport_SchemaMismatch 断言 schema 不符 → 拒绝导入（防静默错迁）。
func TestImport_SchemaMismatch(t *testing.T) {
	t.Parallel()
	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	inDir := t.TempDir()
	if err := WriteFile(filepath.Join(inDir, "manifest.json"), &Manifest{Schema: 99}); err != nil {
		t.Fatal(err)
	}
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: inDir}
	_, err := im.Import(context.Background())
	if err == nil {
		t.Fatal("schema 不符应拒绝导入")
	}
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("应为 ErrSchemaMismatch，got %v", err)
	}
}

// TestImport_LocalChecksumMismatch 断言 manifest checksum 与本地文件不一致 → 该文件
// 拒绝上传（防静默错迁：篡改 checksum 后导入必红）。
func TestImport_LocalChecksumMismatch(t *testing.T) {
	t.Parallel()
	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	inDir := t.TempDir()
	// 本地文件内容是 "hello"，manifest 记录错误 checksum（"evil"）
	filesDir := filepath.Join(inDir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Schema: 1, Files: []FileEntry{{Name: "a.txt", Size: 5, Checksum: "evil"}}}
	if err := WriteFile(filepath.Join(inDir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: inDir}
	sum, err := im.Import(context.Background())
	if err == nil {
		t.Fatalf("本地 checksum 不匹配应失败，sum=%+v", sum)
	}
	if sum.Failed != 1 {
		t.Errorf("失败计数 = %d, want 1", sum.Failed)
	}
}

// TestImport_FailedUpload 断言上传失败默认中止（快速失败），--ignore-errors 跳过继续。
func TestImport_FailedUpload(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := seedSrc(t, srcDir)
	src := &srcMock{t: t, dir: srcDir}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{"a.txt": true}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	runExport(t, src, srcTS.URL, outDir, want)

	// 默认：a.txt 上传失败 → 中止（快速失败）
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: outDir}
	if _, err := im.Import(context.Background()); err == nil {
		t.Fatal("上传失败应中止并报错")
	}
	steps := dst.steps()
	wantSteps := []string{"healthz", "stat:a.txt", "upload:a.txt"}
	if len(steps) != len(wantSteps) {
		t.Fatalf("默认应快速失败（healthz→stat→upload 失败即停），got %v", steps)
	}
	for i, s := range wantSteps {
		if steps[i] != s {
			t.Fatalf("快速失败步骤不符: got %v, want %v", steps, wantSteps)
		}
	}

	// --ignore-errors：跳过继续，dir/b.bin 仍导入
	im.IgnoreErrors = true
	sum, err := im.Import(context.Background())
	if err != nil {
		t.Fatalf("ignore-errors 下失败不应中止: %v", err)
	}
	if sum.Failed != 1 || sum.Imported != 1 {
		t.Errorf("ignore-errors 计数: %+v", sum)
	}
	if !sum.Ok() {
		t.Error("ignore-errors 下应 Ok()=true")
	}
}

// TestImport_TargetUnreachable 断言目标不可达 → 预检失败，不开跑。
func TestImport_TargetUnreachable(t *testing.T) {
	t.Parallel()
	outDir := filepath.Join(t.TempDir(), "mig")
	if err := WriteFile(filepath.Join(outDir, "manifest.json"), &Manifest{Schema: 1, Files: []FileEntry{{Name: "a.txt", Checksum: "x"}}}); err != nil {
		t.Fatal(err)
	}
	im := &Importer{Client: client.NewFileClient("http://127.0.0.1:1"), InDir: outDir}
	if _, err := im.Import(context.Background()); err == nil {
		t.Fatal("目标不可达应预检失败")
	}
}

// TestExport_ChecksumDrift 断言：list 报告 checksum 与下载内容不一致（索引漂移）
// → 导出失败（假成功红线：manifest 绝不记录与落盘内容不符的值）。
func TestExport_ChecksumDrift(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := seedSrc(t, srcDir)
	src := &srcMock{t: t, dir: srcDir, listCSOverride: "deadbeef"}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	ex := &Exporter{Client: client.NewFileClient(srcTS.URL), OutDir: outDir}
	if _, err := ex.Export(context.Background()); err == nil {
		t.Fatalf("list/download checksum 漂移应导出失败（红线），want=%v", want)
	}
}

// TestImport_PostUploadCorruption 断言：上传成功后目标文件被篡改（checksum 与
// manifest 不符）→ 导入标 FAILED（假成功红线：绝不上报成功）。
func TestImport_PostUploadCorruption(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	want := map[string]string{"a.txt": "hello"}
	for rel, content := range want {
		p := filepath.Join(srcDir, rel)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := &srcMock{t: t, dir: srcDir}
	srcTS := httptest.NewServer(src.handler())
	t.Cleanup(srcTS.Close)

	dstDir := t.TempDir()
	dst := &dstMock{t: t, dir: dstDir, uploadFail: map[string]bool{}, uploadCorrupt: map[string]bool{"a.txt": true}}
	dstTS := httptest.NewServer(dst.handler())
	t.Cleanup(dstTS.Close)

	outDir := filepath.Join(t.TempDir(), "mig")
	runExport(t, src, srcTS.URL, outDir, want)
	im := &Importer{Client: client.NewFileClient(dstTS.URL), InDir: outDir}
	sum, err := im.Import(context.Background())
	if err == nil {
		t.Fatalf("上传后目标被篡改应失败（红线），sum=%+v", sum)
	}
	if sum.Failed != 1 || sum.Imported != 0 {
		t.Errorf("失败计数 = %d (imported=%d)，want Failed=1 Imported=0（快速失败）: %+v", sum.Failed, sum.Imported, sum)
	}
	if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0].Err, "校验不一致") {
		t.Errorf("a.txt 失败原因应为上传后校验不一致: %+v", sum.Failures)
	}
}
