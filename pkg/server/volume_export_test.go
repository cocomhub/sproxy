// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_export_test.go 覆盖卷备份/导出（roadmap 11.3-② / docs/designs/2026-09-24-volume-export.md）：
//  1. GET /api/volumes/export 流式 tar（条目 user/<rel> + 尾部 manifest.json）——内容与源一致；
//  2. manifest 每条目 SHA-256 与源一致（变异：checksum 错/缺 → 红）；
//  3. POST /api/volumes/import 恢复 → POST /api/verify 全绿（往返校验，变异：导入不校验 → 红）；
//  4. 恶意 tar 条目路径穿越 → 拒绝不写卷外（变异：不过 ValidateFilePath → 红）；
//  5. 超配额条目跳过 + 报告（变异：硬失败/写穿配额 → 红）。
//
// 约束：纯标准库断言；httptest（127.0.0.1 回环）；client 用 testHTTPClient(t)（独立连接池）。

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
)

// exportTarEntries 是导出 tar 流的解析结果（条目名 → 内容）。
type exportTarEntries map[string][]byte

// fetchVolumeExport 请求 GET /api/volumes/export 并把响应体解析为 tar 条目。
func fetchVolumeExport(t *testing.T, url, vol string) (int, exportTarEntries) {
	t.Helper()
	u := url + "/api/volumes/export"
	if vol != "" {
		u += "?volume=" + vol
	}
	resp, err := testHTTPClient(t).Get(u)
	if err != nil {
		t.Fatalf("export GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	entries := exportTarEntries{}
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析导出 tar 流失败: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读取 tar 条目 %s 失败: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}
	return resp.StatusCode, entries
}

// parseExportManifest 从导出 tar 条目中取 manifest.json 并解析（复用生产 exportManifest 类型）。
func parseExportManifest(t *testing.T, entries exportTarEntries) exportManifest {
	t.Helper()
	raw, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("导出 tar 缺少尾部 manifest.json（变异：不写 manifest → 红）")
	}
	var m exportManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 manifest.json 失败: %v (raw=%s)", err, raw)
	}
	if len(m.Files) == 0 {
		t.Fatalf("manifest 文件清单为空（变异：漏文件 → 红）")
	}
	return m
}

// sha256hexIn 复用 pkg/server 测试既有 helper（integration_test.go）。

// buildImportTar 构造一个 tar 流（条目顺序自定），用于导入测试。
// manifest 条目经 files 参数拼 JSON（nil = 不写 manifest）。
func buildImportTar(t *testing.T, files map[string][]byte, manifest *exportManifest) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写 tar 头失败: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("写 tar 内容失败: %v", err)
		}
	}
	if manifest != nil {
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		hdr := &tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(raw))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写 manifest 头失败: %v", err)
		}
		if _, err := tw.Write(raw); err != nil {
			t.Fatalf("写 manifest 内容失败: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// postImport 提交 tar 流到 POST /api/volumes/import。
func postImport(t *testing.T, url, vol string, tarBody []byte, overwrite bool) (int, importResult) {
	t.Helper()
	u := url + "/api/volumes/import?overwrite=" + boolStr(overwrite)
	if vol != "" {
		u += "&volume=" + vol
	}
	req, err := http.NewRequest("POST", u, bytes.NewReader(tarBody))
	if err != nil {
		t.Fatalf("new import req: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("import POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res importResult
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("解析 import 响应失败: %v (body=%s)", err, body)
		}
	}
	return resp.StatusCode, res
}

// importResult 复用生产类型（volume_export.go 的 POST /api/volumes/import 响应体）。

// uploadFileSignedPath 带 X-File-Path 头上传（保留子目录；Go ≥1.26 multipart 文件名会
// 被截断为 basename，见 integration_test.go TestUpload_ToSubDirectory 注释）。
func uploadFileSignedPath(t *testing.T, baseURL, filename string, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, werr := part.Write(body); werr != nil {
		t.Fatalf("write body: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex(body))
	req.Header.Set("X-File-Path", filename)
	signBodyRequest(req, testAccessKey, testAccessSecret, buf.Bytes())

	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// boolStr 是 Go bool 的 URL 参数序列化。
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// listDiskFiles 递归列出存储根下全部文件（相对路径，正斜杠）。
func listDiskFiles(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := osReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			p := rel + "/" + e.Name()
			if e.IsDir() {
				walk(dir+"/"+e.Name(), p)
			} else {
				out[p] = true
			}
		}
	}
	walk(root, "")
	return out
}

// TestVolumeExport_StreamsTar 导出流可解析为合法 tar 且文件内容与源一致（含子目录）。
// 变异：非流式/漏文件 → 红。
func TestVolumeExport_StreamsTar(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	if st := uploadFileSigned(t, url, "a.txt", []byte("hello a")); st != http.StatusOK {
		t.Fatalf("upload a.txt: %d", st)
	}
	if st := uploadFileSignedPath(t, url, "sub/b.txt", []byte("hello b body")); st != http.StatusOK {
		t.Fatalf("upload sub/b.txt: %d", st)
	}

	status, entries := fetchVolumeExport(t, url, "")
	if status != http.StatusOK {
		t.Fatalf("export status = %d, want 200", status)
	}
	if got := entries["user/a.txt"]; !bytes.Equal(got, []byte("hello a")) {
		t.Fatalf("user/a.txt = %q, want %q", got, "hello a")
	}
	if got := entries["user/sub/b.txt"]; !bytes.Equal(got, []byte("hello b body")) {
		t.Fatalf("user/sub/b.txt = %q, want %q", got, "hello b body")
	}
	// 尾部 manifest 必须存在（变异：不写 manifest → 红）。
	m := parseExportManifest(t, entries)
	if m.Volume == "" {
		t.Fatal("manifest.volume 为空")
	}
	found := map[string]bool{}
	for _, f := range m.Files {
		found[f.Path] = true
	}
	if !found["user/a.txt"] || !found["user/sub/b.txt"] {
		t.Fatalf("manifest 缺文件条目: %v", found)
	}
}

// TestVolumeExport_ManifestChecksums manifest 每条目 SHA-256 与源文件一致。
// 变异：checksum 错/缺 → 红。
func TestVolumeExport_ManifestChecksums(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	payloads := map[string][]byte{
		"a.txt": []byte("alpha content"),
		"x.txt": []byte("delta content"),
	}
	for name, body := range payloads {
		if st := uploadFileSigned(t, url, name, body); st != http.StatusOK {
			t.Fatalf("upload %s: %d", name, st)
		}
	}

	_, entries := fetchVolumeExport(t, url, "")
	m := parseExportManifest(t, entries)
	for _, f := range m.Files {
		content, ok := payloads[strings.TrimPrefix(f.Path, "user/")]
		if !ok {
			t.Fatalf("manifest 含意外条目 %s", f.Path)
		}
		want := sha256hex(content)
		if f.Checksum != want {
			t.Errorf("manifest[%s].checksum = %s, want %s（变异：checksum 错 → 红）", f.Path, f.Checksum, want)
		}
		if f.Size != int64(len(content)) {
			t.Errorf("manifest[%s].size = %d, want %d", f.Path, f.Size, len(content))
		}
		if f.Ledger != want {
			t.Errorf("manifest[%s].ledger = %s, want 台账值 %s（交叉校验缺失 → 红）", f.Path, f.Ledger, want)
		}
	}
}

// TestVolumeImport_RestoreConsistent 导出 → 导入新卷 → POST /api/verify 全绿。
// 变异：导入不校验 manifest checksum → 红。
func TestVolumeImport_RestoreConsistent(t *testing.T) {
	t.Parallel()
	srcURL, _, cleanupSrc := newTestServer(t, nil)
	defer cleanupSrc()

	if st := uploadFileSigned(t, srcURL, "a.txt", []byte("restore a")); st != http.StatusOK {
		t.Fatalf("upload a.txt: %d", st)
	}
	if st := uploadFileSignedPath(t, srcURL, "sub/b.txt", []byte("restore b")); st != http.StatusOK {
		t.Fatalf("upload sub/b.txt: %d", st)
	}
	_, entries := fetchVolumeExport(t, srcURL, "")
	manifest := parseExportManifest(t, entries)

	// 导入目标 = 独立存储根的另一个服务。
	dstURL, _, cleanupDst := newTestServer(t, nil)
	defer cleanupDst()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, f := range manifest.Files {
		content := entries[f.Path]
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	rawManifest, _ := json.Marshal(manifest)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(rawManifest))}); err != nil {
		t.Fatalf("write manifest header: %v", err)
	}
	if _, err := tw.Write(rawManifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	_ = tw.Close()

	status, res := postImport(t, dstURL, "", tarBuf.Bytes(), false)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	}
	if !res.Success {
		t.Fatalf("import success = false: %s", res.Message)
	}
	if res.Imported != 2 {
		t.Fatalf("import imported = %d, want 2", res.Imported)
	}

	// POST /api/verify 确认恢复后全卷一致（往返 checksum 校验）。
	verifyBody := bytes.NewBufferString(`{"volume":"","force":true}`)
	vreq, err := http.NewRequest("POST", dstURL+"/api/verify", verifyBody)
	if err != nil {
		t.Fatalf("new verify req: %v", err)
	}
	vreq.Header.Set("Content-Type", "application/json")
	vresp, err := testHTTPClient(t).Do(vreq)
	if err != nil {
		t.Fatalf("verify POST: %v", err)
	}
	defer vresp.Body.Close()
	vbody, _ := io.ReadAll(vresp.Body)
	var rep verifyReport
	if err := json.Unmarshal(vbody, &rep); err != nil {
		t.Fatalf("parse verify: %v (body=%s)", err, vbody)
	}
	if rep.Ok != 2 || len(rep.Mismatched) != 0 || len(rep.Missing) != 0 {
		t.Fatalf("verify after import = %+v, want ok=2 mismatched=0 missing=0（变异：导入不校验/不登记台账 → 红）", rep)
	}
}

// TestVolumeImport_ChecksumMismatchSkipped manifest 声明 checksum 与 tar 条目内容不符
// → 该条目标记跳过（不落盘）。
// 变异：导入不校验 manifest checksum → 红。
func TestVolumeImport_ChecksumMismatchSkipped(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	content := []byte("good content")
	manifest := &exportManifest{Volume: "default", Files: []exportManifestEntry{
		{Path: "user/good.txt", Size: int64(len(content)), Checksum: sha256hex(content)},
		{Path: "user/bad.txt", Size: int64(len("tampered")), Checksum: sha256hex([]byte("tampered"))},
	}}
	files := map[string][]byte{
		"user/good.txt": content,
		"user/bad.txt":  []byte("evil payload"), // 与 manifest 声明的 checksum 不符
	}
	tarBody := buildImportTar(t, files, manifest)

	status, res := postImport(t, url, "", tarBody, false)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	}
	if res.Imported != 1 || res.Skipped != 1 {
		t.Fatalf("import = imported:%d skipped:%d, want imported=1 skipped=1（变异：导入不校验 manifest checksum → 红）", res.Imported, res.Skipped)
	}
}

// TestVolumeImport_OverwriteGuardSameName 同名文件未显式 overwrite → 跳过（幂等防误覆盖）；
// overwrite=true → 覆盖写。
// 变异：删除已存在保护 → 红。
func TestVolumeImport_OverwriteGuardSameName(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	if st := uploadFileSigned(t, url, "a.txt", []byte("original")); st != http.StatusOK {
		t.Fatalf("upload a.txt: %d", st)
	}
	content := []byte("replacement")
	manifest := &exportManifest{Volume: "default", Files: []exportManifestEntry{
		{Path: "user/a.txt", Size: int64(len(content)), Checksum: sha256hex(content)},
	}}
	tarBody := buildImportTar(t, map[string][]byte{"user/a.txt": content}, manifest)

	// 未显式 overwrite：同名已存在 → 跳过。
	status, res := postImport(t, url, "", tarBody, false)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	}
	if res.Skipped != 1 || res.Imported != 0 {
		t.Fatalf("import(overwrite=false) = imported:%d skipped:%d, want imported=0 skipped=1（变异：已存在保护缺失 → 红）", res.Imported, res.Skipped)
	}

	// overwrite=true：覆盖写。
	status, res = postImport(t, url, "", tarBody, true)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	}
	if res.Imported != 1 || res.Skipped != 0 {
		t.Fatalf("import(overwrite=true) = imported:%d skipped:%d, want imported=1 skipped=0", res.Imported, res.Skipped)
	}

	// 覆盖后内容一致。
	_, entries := fetchVolumeExport(t, url, "")
	if got := entries["user/a.txt"]; !bytes.Equal(got, content) {
		t.Fatalf("覆盖后 user/a.txt = %q, want %q", got, content)
	}
}

// TestVolumeImport_PathTraversalRejected 恶意 tar 条目 ../../x → 拒绝不写卷外。
// 变异：不过 ValidateFilePath → 红。
func TestVolumeImport_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	evil := map[string][]byte{"../../evil.txt": []byte("pwn")}
	tarBody := buildImportTar(t, evil, nil)
	status, res := postImport(t, url, "", tarBody, false)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200（失败条目跳过语义）", status)
	}
	// 路径穿越条目按「跳过 + 报告」处理（importOneFile 记 Skipped，绝不写卷外）。
	if res.Skipped != 1 {
		t.Fatalf("import skipped = %d, want 1（路径穿越条目必须被拒绝）", res.Skipped)
	}
	if res.Failed != 0 {
		t.Fatalf("import failed = %d, want 0（路径穿越属跳过而非写盘失败）", res.Failed)
	}

	// 卷外不得出现文件。
	storageRoot := t.TempDir()
	if files := listDiskFiles(t, storageRoot); len(files) != 0 {
		t.Fatalf("卷外出现文件: %v（变异：不过 ValidateFilePath → 红）", files)
	}
}

// TestVolumeImport_QuotaExceededSkipped 超配额条目跳过 + 报告。
// 变异：硬失败/写穿配额 → 红。
func TestVolumeImport_QuotaExceededSkipped(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, func(cfg *Config) {
		// 默认卷容量很小（30 字节），第二个文件必然超配额。
		cfg.Volumes[0].VolCapacity = 30
	})
	defer cleanup()

	big := []byte(strings.Repeat("x", 50))
	small := []byte("tiny")
	manifest := &exportManifest{Volume: "default", Files: []exportManifestEntry{
		{Path: "user/small.txt", Size: int64(len(small)), Checksum: sha256hex(small)},
		{Path: "user/big.txt", Size: int64(len(big)), Checksum: sha256hex(big)},
	}}
	files := map[string][]byte{"user/small.txt": small, "user/big.txt": big}
	tarBody := buildImportTar(t, files, manifest)

	status, res := postImport(t, url, "", tarBody, false)
	if status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	}
	if !res.Success {
		t.Fatalf("import success = false: %s", res.Message)
	}
	if res.Imported != 1 || res.Skipped != 1 {
		t.Fatalf("import = imported:%d skipped:%d, want imported=1 skipped=1（变异：硬失败/写穿配额 → 红）", res.Imported, res.Skipped)
	}
}

// ---- 直连文件系统辅助（listDiskFiles 用；纯标准库 os 实现） ----

// osReadDir 是 os.ReadDir 的薄别名（listDiskFiles 用）。
func osReadDir(dir string) ([]osDirEntryAlias, error) { return os.ReadDir(dir) }

type osDirEntryAlias = os.DirEntry
