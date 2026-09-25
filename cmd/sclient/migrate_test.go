// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/migrate"
)

// migrateSourceServer 是 migrate export 子命令的源机 mock：
// GET /api/files（subdir 递归）+ GET /download。
func migrateSourceServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		sub := r.URL.Query().Get("subdir")
		base := filepath.Join(dir, filepath.FromSlash(sub))
		entries, err := os.ReadDir(base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var out []client.FileInfo
		for _, e := range entries {
			info, _ := e.Info()
			fi := client.FileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().UnixNano(), IsDir: e.IsDir()}
			if !e.IsDir() {
				data, _ := os.ReadFile(filepath.Join(base, e.Name()))
				sum := sha256.Sum256(data)
				fi.Checksum = hex.EncodeToString(sum[:])
			}
			out = append(out, fi)
		}
		writeMigrateJSON(w, map[string]any{"files": out})
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		_, _ = w.Write(data)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// migrateTargetServer 是 migrate import / mirror-config 子命令的目标机 mock：
// /healthz + POST /upload + HEAD /api/files/stat + GET /api/volumes。
func migrateTargetServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		remote := r.Header.Get("X-File-Path")
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
		outPath := filepath.Join(dir, filepath.FromSlash(remote))
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
		_, _ = io.Copy(out, f)
		writeMigrateJSON(w, map[string]any{"success": true, "message": "ok"})
	})
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Header().Set("X-File-Size", "0")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeMigrateJSON(w, map[string]any{"volumes": []map[string]any{
			{"name": "default", "allowed": true},
			{"name": "redundant", "allowed": true},
		}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// writeMigrateJSON 写 JSON 响应。
func writeMigrateJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// migrateSeedFiles 在目录写入文件并返回 rel→内容。
func migrateSeedFiles(t *testing.T, dir string) map[string]string {
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

// migrateWriteManifest 在目录写入最小合法 manifest（mirror-config 测试用）。
func migrateWriteManifest(t *testing.T, inDir string) {
	t.Helper()
	m := &migrate.Manifest{
		Schema: migrate.SchemaVersion,
		Server: migrate.ServerRef{URL: "https://src.example:18083"},
		Files:  []migrate.FileEntry{{Name: "a.txt", Size: 5, Checksum: "abc", Volume: "disk2"}},
	}
	if err := migrate.WriteFile(filepath.Join(inDir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateCmd_ExportImport 断言 export → import 全流程（CLI 装配层）：
// export 落盘 manifest + files/，import 上传到目标并输出汇总。
func TestMigrateCmd_ExportImport(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	migrateSeedFiles(t, srcDir)
	srcTS := migrateSourceServer(t, srcDir)

	dstDir := t.TempDir()
	dstTS := migrateTargetServer(t, dstDir)

	outDir := filepath.Join(t.TempDir(), "mig")

	// export 子命令（--yes 跳过交互）
	var exportBuf strings.Builder
	exportCmd := NewCmdMigrate(clientfactory.NewMock(client.NewFileClient(srcTS.URL), nil),
		cli.IOStreams{In: strings.NewReader("y\n"), Out: &exportBuf, ErrOut: io.Discard})
	exportCmd.SetArgs([]string{"export", outDir, "--yes"})
	if err := exportCmd.Execute(); err != nil {
		t.Fatalf("migrate export 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "manifest.json")); err != nil {
		t.Fatalf("manifest.json 未生成: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "files", "dir", "b.bin")); err != nil {
		t.Fatalf("files/dir/b.bin 未落盘: %v", err)
	}
	if !strings.Contains(exportBuf.String(), "导出完成") {
		t.Errorf("export 输出缺成功文案: %s", exportBuf.String())
	}

	// import 子命令（--yes 跳过交互）
	var importBuf strings.Builder
	importCmd := NewCmdMigrate(clientfactory.NewMock(client.NewFileClient(dstTS.URL), nil),
		cli.IOStreams{In: strings.NewReader("y\n"), Out: &importBuf, ErrOut: io.Discard})
	importCmd.SetArgs([]string{"import", outDir, "--yes"})
	if err := importCmd.Execute(); err != nil {
		t.Fatalf("migrate import 失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dstDir, "dir", "b.bin"))
	if err != nil {
		t.Fatalf("目标文件 dir/b.bin 缺失: %v", err)
	}
	if string(data) != "0123456789" {
		t.Errorf("目标内容 = %q, want 0123456789", data)
	}
	if !strings.Contains(importBuf.String(), "导入汇总") {
		t.Errorf("import 输出缺汇总: %s", importBuf.String())
	}
}

// TestMigrateCmd_MirrorConfigPrint 断言 mirror-config 子命令打印 YAML 片段。
func TestMigrateCmd_MirrorConfigPrint(t *testing.T) {
	t.Parallel()
	dstDir := t.TempDir()
	dstTS := migrateTargetServer(t, dstDir)

	inDir := t.TempDir()
	migrateWriteManifest(t, inDir)

	var buf strings.Builder
	cmd := NewCmdMigrate(clientfactory.NewMock(client.NewFileClient(dstTS.URL), nil),
		cli.IOStreams{In: strings.NewReader("y\n"), Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"mirror-config", inDir, "--print"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mirror-config 失败: %v", err)
	}
	for _, key := range []string{"mirror_to", "sync_remotes", "federation"} {
		if !strings.Contains(buf.String(), key) {
			t.Errorf("mirror-config 输出缺 %q: %s", key, buf.String())
		}
	}
}

// TestMigrateCmd_ExportMissingOut 断言 export 缺 <out> → 明确报错。
func TestMigrateCmd_ExportMissingOut(t *testing.T) {
	t.Parallel()
	var errBuf strings.Builder
	cmd := NewCmdMigrate(clientfactory.NewMock(nil, nil),
		cli.IOStreams{Out: io.Discard, ErrOut: &errBuf})
	cmd.SetArgs([]string{"export"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("export 缺 <out> 应报错")
	}
	if !strings.Contains(err.Error(), "导出目录不能为空") {
		t.Errorf("错误信息应说明导出目录缺失，got: %v", err)
	}
}

// TestMigrateCmd_MissingSubcommand 断言无子命令 → 参数校验错误。
func TestMigrateCmd_MissingSubcommand(t *testing.T) {
	t.Parallel()
	cmd := NewCmdMigrate(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("migrate 至少需要 1 个子命令参数，got nil")
	}
}
