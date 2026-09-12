// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// t6b_volume_acl_test.go 验证 PR-D T6b 默认卷 ACL 门禁收口 + 孤儿版本闭合 + batch/search 多卷定位：
//  1. batch delete/rename 逐文件跨卷定位（disk2 文件可删/可改名）；默认卷被 ACL 排除时遗留不可见
//     （删：幂等成功但不删遗留；改名：源文件不存在）。
//  2. search 跨卷（每卷条目带 volume）；默认卷被排除不搜默认卷。
//  3. share create/access 视图定位（disk2 文件可分享+可访问；默认卷被排除时遗留 create 404）。
//  4. archive 输入读取 + archive-dir 视图定位（disk2 文件/目录可打包；默认卷被排除时遗留不可打包）。
//  5. 孤儿版本闭合：user 文件删除后版本仍可 list/restore（恢复回版本所在卷）。
//  6. uploadStatus?filename= 跨卷探测（disk2 已完成文件可探测）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// t6bVolCfg 构造 T6b 多卷配置。mainACLExcludeOwner 为 true 时 main 卷用 allow 白名单排除 owner
// （仅列 alice）——用于「默认卷被 ACL 排除 + owner 在默认卷有遗留」场景。
func t6bVolCfg(t *testing.T, mainACLExcludeOwner bool, owner string) (*Config, []string) {
	t.Helper()
	dirs := []string{t.TempDir(), t.TempDir()}
	vols := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	if mainACLExcludeOwner {
		vols[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"alice"}}
	}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Placement = "prefer-default"
	cfg.Volumes = vols
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	return cfg, dirs
}

// t6bMux 绑定 T6b 需要覆盖的全部 handler 路由（固定 actor 注入，/s/{token} 公开不注入）。
func t6bMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /rename", wrap(h.rename))
	mux.HandleFunc("POST /api/batch/delete", wrap(h.batchDelete))
	mux.HandleFunc("POST /api/batch/rename", wrap(h.batchRename))
	mux.HandleFunc("GET /api/files/search", wrap(h.searchFiles))
	mux.HandleFunc("POST /api/archive", wrap(h.archiveHandler))
	mux.HandleFunc("GET /api/archive-dir", wrap(h.archiveDirHandler))
	mux.HandleFunc("POST /api/share", wrap(h.createShareHandler))
	mux.HandleFunc("GET /s/{token}", h.accessShareHandler)
	mux.HandleFunc("GET /api/versions", wrap(h.listVersionsHandler))
	mux.HandleFunc("POST /api/versions/restore", wrap(h.restoreVersionHandler))
	mux.HandleFunc("DELETE /api/versions", wrap(h.deleteVersionHandler))
	mux.HandleFunc("GET /upload/status", wrap(h.uploadStatus))
	mux.HandleFunc("POST /mkdir", wrap(h.mkdir))
	return mux
}

// t6bServer 装配 T6b 多卷服务，返回 URL、Handlers 与各卷根目录。
func t6bServer(t *testing.T, actor string, cfg *Config) (string, *Handlers, []string) {
	t.Helper()
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(cfg.Volumes))
	for i := range cfg.Volumes {
		dirs[i] = cfg.Volumes[i].Root
	}
	return ts.URL, h, dirs
}

// t6bPostBatch 以 JSON body POST 到 path，返回状态与解析后的 BatchResponse。
func t6bPostBatch(t *testing.T, baseURL, path string, body any) (int, BatchResponse) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out BatchResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST %s decode %q: %v", path, string(raw), err)
	}
	return resp.StatusCode, out
}

// t6bArchiveEntries 解压 GET/POST archive 响应，返回 tar 条目名 → 内容映射。
func t6bArchiveEntries(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	defer resp.Body.Close()
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		content, _ := io.ReadAll(tr)
		out[hdr.Name] = string(content)
	}
	return out
}

// ---- batch delete：disk2 文件可删 + ACL 排除遗留不可见 ----

func TestT6b_BatchDelete_LocatesDisk2AndHidesExcludedDefault(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob") // main allow:[alice] → bob 排除
	url, _, _ := t6bServer(t, "bob", cfg)

	// bob 默认卷遗留（ACL 收紧前）+ disk2 真实文件。
	legacyBody := []byte("BOB-LEGACY-ON-MAIN")
	seedVolumeFile(t, dirs[0], "bob", "user/legacy.txt", legacyBody)
	diskBody := []byte("BOB-ON-DISK2")
	if status, hdr, body := volumeUpload(t, url, "b.txt", diskBody, ""); status != http.StatusOK {
		t.Fatalf("bob disk2 上传应 200, got %d %s", status, body)
	} else if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("bob 上传 X-Volume=%q want disk2（main 排除 bob）", got)
	}
	if !diskFileExists(t, dirs[1], "bob", "b.txt") {
		t.Fatal("前置：bob/b.txt 应落 disk2")
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "bob", "user", "legacy.txt")); err != nil {
		t.Fatal("前置：默认卷 bob 遗留应存在")
	}

	// 1) disk2 文件批量删除 → 成功且真实删除。
	status, out := t6bPostBatch(t, url, "/api/batch/delete", map[string]any{
		"files": []map[string]string{{"filename": "b.txt", "checksum": sha256hex(diskBody)}},
	})
	if status != http.StatusOK || len(out.Results) != 1 || !out.Results[0].Success {
		t.Fatalf("batch delete disk2 文件应成功: status=%d out=%+v", status, out)
	}
	if diskFileExists(t, dirs[1], "bob", "b.txt") {
		t.Fatal("batch delete 后 disk2/bob/b.txt 应已删除")
	}

	// 2) ACL 排除的默认卷遗留 → 幂等成功提示（不泄存在性）但绝不实际删除。
	status, out = t6bPostBatch(t, url, "/api/batch/delete", map[string]any{
		"files": []map[string]string{{"filename": "legacy.txt", "checksum": sha256hex(legacyBody)}},
	})
	if status != http.StatusOK || len(out.Results) != 1 || !out.Results[0].Success {
		t.Fatalf("ACL 排除遗留 batch delete 应幂等成功（不泄存在性）: status=%d out=%+v", status, out)
	}
	got, err := os.ReadFile(filepath.Join(dirs[0], "bob", "user", "legacy.txt"))
	if err != nil || !bytes.Equal(got, legacyBody) {
		t.Fatalf("ACL 排除遗留绝不可被删除：err=%v got=%q", err, got)
	}

	// 3) 真实缺失 → 幂等成功。
	status, out = t6bPostBatch(t, url, "/api/batch/delete", map[string]any{
		"files": []map[string]string{{"filename": "ghost.txt", "checksum": sha256hex([]byte("x"))}},
	})
	if status != http.StatusOK || len(out.Results) != 1 || !out.Results[0].Success {
		t.Fatalf("真实缺失 batch delete 应幂等成功: status=%d out=%+v", status, out)
	}
}

// ---- batch rename：disk2 文件可改名 + ACL 排除遗留源不存在 ----

func TestT6b_BatchRename_LocatesDisk2AndHidesExcludedDefault(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob")
	url, _, _ := t6bServer(t, "bob", cfg)

	seedVolumeFile(t, dirs[0], "bob", "user/legacy.txt", []byte("BOB-LEGACY"))
	diskBody := []byte("RENAME-DISK2")
	if status, hdr, body := volumeUpload(t, url, "r.txt", diskBody, ""); status != http.StatusOK {
		t.Fatalf("bob 上传应 200, got %d %s", status, body)
	} else if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("bob 上传 X-Volume=%q want disk2", got)
	}

	// disk2 文件改名 → 成功，r2.txt 落 disk2。
	status, out := t6bPostBatch(t, url, "/api/batch/rename", map[string]any{
		"operations": []map[string]string{
			{"from": "r.txt", "to": "r2.txt", "checksum": sha256hex(diskBody)},
		},
	})
	if status != http.StatusOK || len(out.Results) != 1 || !out.Results[0].Success {
		t.Fatalf("batch rename disk2 文件应成功: status=%d out=%+v", status, out)
	}
	if !diskFileExists(t, dirs[1], "bob", "r2.txt") || diskFileExists(t, dirs[1], "bob", "r.txt") {
		t.Fatal("rename 后 disk2 应 r2.txt 且 r.txt 消失")
	}

	// ACL 排除遗留改名 → 源文件不存在（失败，不泄存在性），文件保留。
	status, out = t6bPostBatch(t, url, "/api/batch/rename", map[string]any{
		"operations": []map[string]string{
			{"from": "legacy.txt", "to": "legacy2.txt", "checksum": sha256hex([]byte("BOB-LEGACY"))},
		},
	})
	if status != http.StatusOK || len(out.Results) != 1 || out.Results[0].Success {
		t.Fatalf("ACL 排除遗留 rename 应失败（源文件不存在）: status=%d out=%+v", status, out)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "bob", "user", "legacy.txt")); err != nil {
		t.Fatal("ACL 排除遗留不应被改名")
	}
}

// ---- batch rename：跨卷目标 409（AD-4）----

func TestT6b_BatchRename_CrossVolumeTargetConflict(t *testing.T) {
	cfg, _ := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8 // main 只够 1×8B → 第二个文件换 disk2
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	body := []byte("01234567") // 8B = main 容量

	if status, _, rb := volumeUpload(t, url, "target.txt", body, ""); status != http.StatusOK {
		t.Fatalf("target 上传应 200, got %d %s", status, rb)
	}
	// main 满 → src.txt 落 disk2。
	if status, hdr, rb := volumeUpload(t, url, "src.txt", body, ""); status != http.StatusOK {
		t.Fatalf("src 上传应 200, got %d %s", status, rb)
	} else if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("src X-Volume=%q want disk2（main 满换卷）", got)
	}

	// rename src.txt(disk2) → target.txt(main 已存在) → 跨卷目标冲突。
	status, out := t6bPostBatch(t, url, "/api/batch/rename", map[string]any{
		"operations": []map[string]string{
			{"from": "src.txt", "to": "target.txt", "checksum": sha256hex(body)},
		},
	})
	if status != http.StatusOK || len(out.Results) != 1 || out.Results[0].Success {
		t.Fatalf("跨卷目标 rename 应失败（目标路径已存在）: status=%d out=%+v", status, out)
	}
	if !strings.Contains(out.Results[0].Message, "目标路径已存在") {
		t.Fatalf("跨卷目标冲突消息应含「目标路径已存在」, got %q", out.Results[0].Message)
	}
}

// ---- search：跨卷 + ACL 排除默认卷不泄漏 ----

func TestT6b_Search_CrossVolumeResults(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8 // main 只够 1×8B
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	bodyA := []byte("AAAAAAAA") // 8B 占满 main
	bodyB := []byte("BBBBBBBB") // 8B：main 满 → 落 disk2
	if status, _, b := volumeUpload(t, url, "needle_m.txt", bodyA, ""); status != http.StatusOK {
		t.Fatalf("needle_m.txt 上传应 200, got %d %s", status, b)
	}
	if status, hdr, b := volumeUpload(t, url, "needle_n.txt", bodyB, ""); status != http.StatusOK {
		t.Fatalf("needle_n.txt 上传应 200, got %d %s", status, b)
	} else if got := hdr.Get("X-Volume"); got != "disk2" {
		t.Fatalf("needle_n.txt X-Volume=%q want disk2（main 满换卷）", got)
	}
	_ = dirs

	// search q=needle → 跨卷命中 main + disk2 各一条，条目带各自 volume。
	resp, err := http.Get(url + "/api/files/search?q=needle")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var shape struct {
		Files []fileInfo `json:"files"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&shape); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if shape.Total != 2 || len(shape.Files) != 2 {
		t.Fatalf("search 应跨卷命中 2 条, got %+v", shape)
	}
	volOf := map[string]string{}
	for _, f := range shape.Files {
		volOf[f.Name] = f.Volume
	}
	if volOf["needle_m.txt"] != "main" || volOf["needle_n.txt"] != "disk2" {
		t.Fatalf("search 条目 volume 不符: %+v", volOf)
	}
	if shape.Files[0].Checksum == "" {
		t.Fatal("search 命中条目应带 checksum")
	}
}

func TestT6b_Search_ACLExcludedDefaultNotLeaked(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob") // main 排除 bob
	url, _, _ := t6bServer(t, "bob", cfg)

	// bob 默认卷遗留含机密内容 + bob disk2 文件（可搜）。
	seedVolumeFile(t, dirs[0], "bob", "user/topsecret-needle.txt", []byte("MAIN-SECRET"))
	diskBody := []byte("DISK2-needle-visible")
	if status, hdr, b := volumeUpload(t, url, "visible-needle.txt", diskBody, ""); status != http.StatusOK {
		t.Fatalf("bob disk2 上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("bob 上传 X-Volume=%q want disk2", hdr.Get("X-Volume"))
	}

	resp, err := http.Get(url + "/api/files/search?q=needle")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var shape struct {
		Files []fileInfo `json:"files"`
		Total int        `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&shape); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	for _, f := range shape.Files {
		if f.Name == "topsecret-needle.txt" {
			t.Fatalf("默认卷被排除时 search 不得泄漏默认卷遗留: %+v", shape.Files)
		}
	}
	if shape.Total != 1 || shape.Files[0].Name != "visible-needle.txt" || shape.Files[0].Volume != "disk2" {
		t.Fatalf("search 应只命中 disk2 可见文件, got %+v", shape.Files)
	}
}

// ---- share：disk2 分享 + ACL 排除 create 404 + 未排除正常 ----

func TestT6b_Share_Disk2CreateAndAccess(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8
	h := buildVolSetHandlers(t, cfg)
	h.shareStore = NewShareStore(testLogger())
	t.Cleanup(h.shareStore.Stop)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	// disk2 文件（main 满换卷）。
	body := []byte("share-disk2-content")
	if status, hdr, b := volumeUpload(t, url, "diskfile.txt", body, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("diskfile X-Volume=%q want disk2", hdr.Get("X-Volume"))
	}
	_ = dirs

	// create share → token；access → 内容一致。
	shareResp := t6bCreateShare(t, url, "diskfile.txt", http.StatusOK)
	token := shareResp.Token
	if token == "" {
		t.Fatal("share token 为空")
	}
	resp, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("access share disk2: status=%d got=%q want %q", resp.StatusCode, got, body)
	}
}

func TestT6b_Share_DefaultOpenNormal(t *testing.T) {
	cfg, _ := t6bVolCfg(t, false, "alice")
	h := buildVolSetHandlers(t, cfg)
	h.shareStore = NewShareStore(testLogger())
	t.Cleanup(h.shareStore.Stop)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	body := []byte("share-main-content")
	if status, hdr, b := volumeUpload(t, url, "mainfile.txt", body, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "main" {
		t.Fatalf("mainfile X-Volume=%q want main", hdr.Get("X-Volume"))
	}
	shareResp := t6bCreateShare(t, url, "mainfile.txt", http.StatusOK)
	resp, err := http.Get(url + "/s/" + shareResp.Token)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != string(body) {
		t.Fatalf("access share main: status=%d got=%q want %q", resp.StatusCode, got, body)
	}
}

func TestT6b_Share_ACLExcludedDefaultCreate404(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob") // main 排除 bob
	h := buildVolSetHandlers(t, cfg)
	h.shareStore = NewShareStore(testLogger())
	t.Cleanup(h.shareStore.Stop)
	seedVolumeFile(t, dirs[0], "bob", "user/legacy.txt", []byte("MAIN-SECRET"))
	ts := httptest.NewServer(t6bMux(h, "bob"))
	t.Cleanup(ts.Close)
	// bob 默认卷遗留不可分享（create 404，内容不可经 token 流出）。
	t6bCreateShare(t, ts.URL, "legacy.txt", http.StatusNotFound)
}

// t6bCreateShare 创建分享并断言期望状态，返回响应。
func t6bCreateShare(t *testing.T, baseURL, filename string, wantStatus int) ShareCreateResponse {
	t.Helper()
	body := fmt.Sprintf(`{"filename":%q,"ttl":"1h"}`, filename)
	resp, err := http.Post(baseURL+"/api/share", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out ShareCreateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode share resp %q: %v", string(raw), err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("create share %s status=%d want %d, body=%s", filename, resp.StatusCode, wantStatus, string(raw))
	}
	return out
}

// ---- mkdir：默认卷被 ACL 排除时落到视图卷（不直写默认卷遗留）----

func TestT6b_Mkdir_WritesToViewVolumeWhenDefaultExcluded(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob") // main 排除 bob
	url, _, _ := t6bServer(t, "bob", cfg)

	// bob mkdir → 视图仅 disk2 → ndir 落 disk2，main 不得出现 bob user 目录写入。
	if status, body := t6bPostForm(t, url, "/mkdir?dirname=ndir"); status != http.StatusOK {
		t.Fatalf("mkdir status=%d body=%s", status, body)
	}
	if _, err := os.Stat(filepath.Join(dirs[1], "bob", "user", "ndir")); err != nil {
		t.Fatalf("bob ndir 应落 disk2（视图卷）: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "bob", "user", "ndir")); err == nil {
		t.Fatal("bob ndir 不得落默认卷 main（ACL 排除直写遗留）")
	}

	// alice（默认卷开放）mkdir → ndir2 落默认卷 main（零回归语义）。
	cfg2, dirs2 := t6bVolCfg(t, false, "alice")
	h2 := buildVolSetHandlers(t, cfg2)
	aliceTS := httptest.NewServer(t6bMux(h2, "alice"))
	t.Cleanup(aliceTS.Close)
	if status, body := t6bPostForm(t, aliceTS.URL, "/mkdir?dirname=ndir2"); status != http.StatusOK {
		t.Fatalf("alice mkdir status=%d body=%s", status, body)
	}
	if _, err := os.Stat(filepath.Join(dirs2[0], "alice", "user", "ndir2")); err != nil {
		t.Fatalf("alice ndir2 应落默认卷 main: %v", err)
	}
}

// t6bPostForm 以空 body POST 到 URL，返回状态与 body。
func t6bPostForm(t *testing.T, baseURL, path string) (int, string) {
	t.Helper()
	resp, err := http.Post(baseURL+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// ---- archive：输入读取跨卷 + archive-dir 视图 + ACL ----

func TestT6b_Archive_InputLocatesDisk2(t *testing.T) {
	cfg, _ := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	body := []byte("archive-disk2-payload")
	if status, hdr, b := volumeUpload(t, url, "disk.txt", body, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("disk.txt X-Volume=%q want disk2", hdr.Get("X-Volume"))
	}

	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(`{"files":["disk.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive status=%d", resp.StatusCode)
	}
	entries := t6bArchiveEntries(t, resp)
	if entries["disk.txt"] != string(body) {
		t.Fatalf("archive disk2 输入读取失败: entries=%+v", entries)
	}
}

func TestT6b_Archive_ACLExcludedDefaultInputSkipped(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob") // main 排除 bob
	url, _, _ := t6bServer(t, "bob", cfg)
	const secret = "MAIN-ARCHIVE-SECRET"
	seedVolumeFile(t, dirs[0], "bob", "user/legacy.txt", []byte(secret))

	// 混合请求（F7 抗变异）：可见 disk2 文件（须打包）+ ACL 排除默认卷遗留（须跳过）。
	// 实现若退化为「不打包任何文件」→ disk2 断言红；若直读排除遗留 → legacy 断言红。
	visibleBody := []byte("VISIBLE-ON-DISK2")
	if status, hdr, b := volumeUpload(t, url, "visible.txt", visibleBody, ""); status != http.StatusOK {
		t.Fatalf("visible.txt 上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("visible.txt X-Volume=%q want disk2", hdr.Get("X-Volume"))
	}

	reqBody := `{"files":["legacy.txt","visible.txt"]}`
	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive status=%d（流式 200）", resp.StatusCode)
	}
	entries := t6bArchiveEntries(t, resp)
	if entries["visible.txt"] != string(visibleBody) {
		t.Fatalf("可见 disk2 文件必须打包在产物中（抗变异）: %+v", entries)
	}
	for name, content := range entries {
		if name == "legacy.txt" || content == secret {
			t.Fatalf("ACL 排除默认卷遗留绝不可被归档读取（内容泄漏）: %+v", entries)
		}
	}
}

func TestT6b_ArchiveDir_LocatesDisk2(t *testing.T) {
	cfg, _ := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	body := []byte("dir-on-disk2")
	// 用 multipart 上传到 mydir/a.txt（保持目录名），先占满 main 让目录落 disk2。
	if status, _, b := volumeUpload(t, url, "filler.txt", []byte("01234567"), ""); status != http.StatusOK {
		t.Fatalf("filler 上传应 200, got %d %s", status, b)
	}
	if status, hdr, b := uploadIntoDir(t, url, "mydir/a.txt", body); status != http.StatusOK {
		t.Fatalf("mydir/a.txt 上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("mydir/a.txt X-Volume=%q want disk2（main 满换卷）", hdr.Get("X-Volume"))
	}

	resp, err := http.Get(url + "/api/archive-dir?dirname=mydir")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive-dir status=%d", resp.StatusCode)
	}
	entries := t6bArchiveEntries(t, resp)
	if entries["mydir/a.txt"] != string(body) {
		t.Fatalf("archive-dir disk2 目录打包失败: entries=%+v", entries)
	}
}

func TestT6b_ArchiveDir_ACLExcludedDefaultNotFound(t *testing.T) {
	cfg, dirs := t6bVolCfg(t, true, "bob")
	url, _, _ := t6bServer(t, "bob", cfg)
	seedVolumeFile(t, dirs[0], "bob", "user/legacydir/secret.txt", []byte("SECRET"))

	resp, err := http.Get(url + "/api/archive-dir?dirname=legacydir")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ACL 排除默认卷 archive-dir 应 404, got %d", resp.StatusCode)
	}
}

// uploadIntoDir 以 multipart 上传到指定目录路径（X-File-Path 不存在时用 CreateFormFile 文件名）。
func uploadIntoDir(t *testing.T, baseURL, filename string, body []byte) (int, http.Header, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	req.Header.Set("X-File-Path", filename)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// ---- orphan 版本闭合：user 文件删除后版本仍可 list/restore ----

func TestT6b_OrphanVersion_RestoreAfterFileDeleted(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 5}, // 容量小 → 上传即换 disk2
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, _, dirs := versionFeatureServer(t, "alice", volumes, func(c *Config) {
		c.Versioning.Enabled = true
		c.Versioning.MaxVersions = 10
	})

	body1 := []byte("version-one-content")
	body2 := []byte("version-two-content-longer")
	if status, _, b := volumeUpload(t, url, "f.txt", body1, ""); status != http.StatusOK {
		t.Fatalf("首传应 200, got %d %s", status, b)
	}
	if status, _, b := volumeUpload(t, url, "f.txt", body2, ""); status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, b)
	}
	// 版本 1 保存在 disk2 version/f.txt。
	verDirDisk2 := filepath.Join(dirs[1], "alice", "version", "f.txt")
	if entries, err := os.ReadDir(verDirDisk2); err != nil || len(entries) != 1 {
		t.Fatalf("disk2 version 应有 1 个版本: err=%v entries=%d", err, len(entries))
	}
	// 删除 user 文件（保留版本）。
	if status, _, b := volumeDelete(t, url, "f.txt", sha256hex(body2)); status != http.StatusOK {
		t.Fatalf("delete f.txt 应 200, got %d %s", status, b)
	}
	if diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("user f.txt 应已删除")
	}
	// 孤儿版本仍可 list（修复前：默认卷无版本目录 → 空）。
	listed := listVersionsJSON(t, url, "f.txt")
	if len(listed.Versions) != 1 {
		t.Fatalf("孤儿版本 list 应 1 个（disk2）, got %d（修复前默认卷=0）", len(listed.Versions))
	}
	// restore 从 disk2 版本恢复文件回 disk2。
	verID := listed.Versions[0].VersionID
	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=f.txt&version_id=%d", url, verID)
	resp, err := http.Post(restoreURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore 孤儿版本应 200, got %d %s", resp.StatusCode, rb)
	}
	if !diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("restore 后 f.txt 应恢复到 disk2 user 桶")
	}
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "f.txt"))
	if err != nil || !bytes.Equal(got, body1) {
		t.Fatalf("restore 内容=%q want %q（版本 body1）", got, body1)
	}
}

// ---- uploadStatus?filename= 跨卷探测 ----

func TestT6b_UploadStatus_FilenameProbesDisk2(t *testing.T) {
	cfg, _ := t6bVolCfg(t, false, "alice")
	cfg.Volumes[0].VolCapacity = 8
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(t6bMux(h, "alice"))
	t.Cleanup(ts.Close)
	url := ts.URL

	body := []byte("status-probe-disk2")
	if status, hdr, b := volumeUpload(t, url, "done.bin", body, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, b)
	} else if hdr.Get("X-Volume") != "disk2" {
		t.Fatalf("done.bin X-Volume=%q want disk2", hdr.Get("X-Volume"))
	}

	resp, err := http.Get(url + "/upload/status?filename=done.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out files.ChunkStatusResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode status %q: %v", string(raw), err)
	}
	if resp.StatusCode != http.StatusOK || !out.Success || !out.Completed {
		t.Fatalf("uploadStatus disk2 已完成文件应 Completed: status=%d out=%+v", resp.StatusCode, out)
	}
	if out.FileChecksum != sha256hex(body) {
		t.Fatalf("uploadStatus checksum=%s want %s", out.FileChecksum, sha256hex(body))
	}
}
