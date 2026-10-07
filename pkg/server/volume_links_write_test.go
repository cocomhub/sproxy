// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_links_write_test.go 验证嵌套封装写保护（用户语义 #6）：普通文件写操作对「被封装卷
// 占用的原始底层卷子目录」必须 fail-closed 拒绝（占用目录只读、只能读），读操作不拦，封装卷
// 自身读写不受影响（不同卷不冲突）。坐标以端到端真实路径钉住：上传 main/videos/x 落盘路径
// 为 <mainRoot>/<owner>/user/videos/x，占用 Subdir=videos 即用户可见坐标，两者一致。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// actorFileWriteMux 构造把固定 actor 注入请求 ctx 后转发文件写/读 handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 upload/delete/rename/list/download 族）。
func actorFileWriteMux(h *Handlers, actor string) *http.ServeMux {
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
	mux.HandleFunc("POST /mkdir", wrap(h.mkdir))
	mux.HandleFunc("POST /rmdir", wrap(h.rmdir))
	mux.HandleFunc("GET /download", wrap(h.download))
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	mux.HandleFunc("POST /upload/init", wrap(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", wrap(h.uploadChunk))
	mux.HandleFunc("POST /upload/complete", wrap(h.uploadComplete))
	return mux
}

// headerFilePath 是上传子目录路径头（Go ≥1.26 multipart 文件名会 filepath.Base 掉目录，
// 子目录必须走 X-File-Path——与 upload_owner_test 等既有测试一致）。
const headerFilePath = "X-File-Path"

// writeMuxUpload 走真实 upload handler 上传 filename 到显式卷 vol（带 X-File-Checksum），
// 返回状态码与响应体。filename 含子目录时经 X-File-Path 头传递（保留完整路径）。
func writeMuxUpload(t *testing.T, mux *http.ServeMux, filename, vol string, body []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if vol != "" {
		if err := mw.WriteField("volume", vol); err != nil {
			t.Fatalf("write volume field: %v", err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set(headerContentType, mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	req.Header.Set(headerFilePath, filename)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxDelete 走真实 delete handler 删除 filename（显式卷 vol + X-File-Checksum）。
func writeMuxDelete(t *testing.T, mux *http.ServeMux, filename, vol, checksum string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/delete?filename="+filename+"&volume="+vol, nil)
	req.Header.Set(headerFileChecksum, checksum)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxRename 走真实 rename handler 从 from 改名为 to（显式卷 vol + checksum）。
func writeMuxRename(t *testing.T, mux *http.ServeMux, from, to, vol, checksum string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rename?from="+from+"&to="+to+"&volume="+vol, nil)
	req.Header.Set(headerFileChecksum, checksum)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxMkdirRmdir 走真实 mkdir/rmdir handler（rmdir 带 force=true）。
func writeMuxMkdirRmdir(t *testing.T, mux *http.ServeMux, dir string, rmdir bool) (int, string) {
	t.Helper()
	method := "/mkdir?dirname=" + dir
	if rmdir {
		method = "/rmdir?dirname=" + dir + "&force=true"
	}
	req := httptest.NewRequest(http.MethodPost, method, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// writeMuxDownload 走真实 download handler 读取 filename（显式卷 vol），返回状态码与内容。
func writeMuxDownload(t *testing.T, mux *http.ServeMux, filename, vol string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/download?filename="+filename+"&volume="+vol, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// writeMuxList 走真实 list handler 列 subdir（显式卷 vol），返回状态码。
func writeMuxList(t *testing.T, mux *http.ServeMux, subdir, vol string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/files?subdir="+subdir+"&volume="+vol, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// assertBodyHas 断言响应体含指定子串。
func assertBodyHas(t *testing.T, body string, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Fatalf("响应体缺少 %q: %s", sub, body)
	}
}

// TestNested_OccupiedSubdir_WriteProtection 端到端验证占用子目录写保护：
//
//	建封装卷占用 main/videos 后：
//	  - 上传 main/videos/x → 403（占用/只读）；上传 main/photos → 200（不同分支）；
//	  - 删除 main/videos/x（预置文件）→ 403；重命名到 main/videos/y → 403；
//	  - mkdir/rmdir main/videos → 403；
//	  - 下载/列表 main/videos → 200（只读不拦）；
//	  - 封装卷 vault 自身读写 → 200（不同卷不冲突）。
func TestNested_OccupiedSubdir_WriteProtection(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)

	// 建封装卷 vault 占用 main/videos：wrapper 测试类型（真实 LocalFS 可读写）+
	// target=main/videos → 登记 {Base:main, Subdir:videos, Wrapper:vault}。
	wmux := userVolWrap(h, "alice")
	vaultRoot := t.TempDir()
	rec := postUserVol(t, wmux, map[string]any{
		"name": "vault", "type": nestedRootType,
		"extra": map[string]any{"root": vaultRoot, "target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建封装卷 vault = %d (body: %s)", rec.Code, rec.Body.String())
	}
	refs := h.links().refsOfBase("main")
	if len(refs) != 1 || refs[0].Subdir != "videos" || refs[0].Wrapper != "vault" {
		t.Fatalf("main 关联 = %+v, want [{videos vault}]", refs)
	}

	fmux := actorFileWriteMux(h, "alice")
	body := []byte("occupied subdir write-protection")

	// 上传 main/videos/x → 403（占用子目录只读，fail-closed）。
	status, resp := writeMuxUpload(t, fmux, "videos/x", "main", body)
	if status != http.StatusForbidden {
		t.Fatalf("上传 main/videos/x = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")
	// 上传 main/videos/y（同占用分支的不同文件）→ 同样拒绝。
	st2, resp2 := writeMuxUpload(t, fmux, "videos/y", "main", body)
	if st2 != http.StatusForbidden {
		t.Fatalf("上传 main/videos/y = %d, want 403 (body: %s)", st2, resp2)
	}

	// 上传 main/photos → 200（不同分支不受影响）。
	status, resp = writeMuxUpload(t, fmux, "photos/a", "main", body)
	if status != http.StatusOK {
		t.Fatalf("上传 main/photos/a = %d, want 200 (body: %s)", status, resp)
	}

	// 封装卷 vault 自身读写 → 200（不同卷不冲突）。
	st3, resp3 := writeMuxUpload(t, fmux, "secret.txt", "vault", body)
	if st3 != http.StatusOK {
		t.Fatalf("上传 vault/secret.txt = %d, want 200 (body: %s)", st3, resp3)
	}
	st4, data4 := writeMuxDownload(t, fmux, "secret.txt", "vault")
	if st4 != http.StatusOK || string(data4) != string(body) {
		t.Fatalf("下载 vault/secret.txt = %d, want 200+内容一致", st4)
	}

	// 预置 legacy 文件于 main/videos/x（绕过已被拦截的上传，模拟封装前遗留数据）。
	legacyRel := filepath.Join(h.volSet.Default().RootDir, "alice", "user", "videos")
	if err := os.MkdirAll(legacyRel, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", legacyRel, err)
	}
	legacyPath := filepath.Join(legacyRel, "x")
	if err := os.WriteFile(legacyPath, body, 0o644); err != nil {
		t.Fatalf("write legacy %s: %v", legacyPath, err)
	}

	// 读路径不拦：下载/列表 main/videos → 200。
	st5, data5 := writeMuxDownload(t, fmux, "videos/x", "main")
	if st5 != http.StatusOK || string(data5) != string(body) {
		t.Fatalf("下载 main/videos/x = %d, want 200+内容一致", st5)
	}
	st6, listBody := writeMuxList(t, fmux, "videos", "main")
	if st6 != http.StatusOK || !strings.Contains(listBody, "x") {
		t.Fatalf("列表 main/videos = %d (want 200 且含 x), body: %s", st6, listBody)
	}

	// 删除 main/videos/x → 403。
	status, resp = writeMuxDelete(t, fmux, "videos/x", "main", sha256hex(body))
	if status != http.StatusForbidden {
		t.Fatalf("删除 main/videos/x = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "只读")

	// 重命名目标 main/videos/y → 403（目标即写侧）。
	status, resp = writeMuxRename(t, fmux, "photos/a", "videos/y", "main", sha256hex(body))
	if status != http.StatusForbidden {
		t.Fatalf("重命名 photos/a→videos/y = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")

	// mkdir main/videos → 403；rmdir main/videos（legacy 目录已存在）→ 403。
	status, resp = writeMuxMkdirRmdir(t, fmux, "videos", false)
	if status != http.StatusForbidden {
		t.Fatalf("mkdir main/videos = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")
	status, resp = writeMuxMkdirRmdir(t, fmux, "videos", true)
	if status != http.StatusForbidden {
		t.Fatalf("rmdir main/videos = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")
}

// TestNested_OccupiedSubdir_UploadAutoRoutingRejects 验证自动路由（无显式 volume）在唯一候选
// 卷为被占用卷时拒绝上传到占用路径：placement prefer-default 下 main 唯一 → main/videos/x
// 命中占用 → 403（跳过占用候选后无剩余候选，fail-closed 不静默落到占用目录）。
func TestNested_OccupiedSubdir_UploadAutoRoutingRejects(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	// 直接登记占用关联（不建可写 vault 卷——否则它成为自动路由候选，路由到非占用卷是合法的）。
	h.links().register(volumeLink{Base: "main", Subdir: "videos", Wrapper: "vault", Owner: "alice"})
	fmux := actorFileWriteMux(h, "alice")
	status, resp := writeMuxUpload(t, fmux, "videos/x", "", []byte("auto routing"))
	if status != http.StatusForbidden {
		t.Fatalf("自动路由上传 videos/x = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")
}

// TestNested_OccupiedSubdir_ChunkedCompleteRejected 验证分块上传 complete 对占用路径的纵深
// 防御：会话 init 定卷后、complete 前创建 wrapper 占用该路径 → complete 命中占用子目录 →
// 403（覆盖 init 与 complete 之间创建 wrapper 的竞态窗口）。
func TestNested_OccupiedSubdir_ChunkedCompleteRejected(t *testing.T) {
	t.Parallel()
	h, _ := newNestedWrapperAPIHandlers(t)
	uvmux := userVolWrap(h, "alice")
	fmux := actorFileWriteMux(h, "alice")
	// 先 init（此刻 main/videos 未被占用，路由到显式卷 main）+ 传分块。
	initOK, resp := chunkedInitChunk(t, fmux, "main", "videos/x")
	if !initOK {
		t.Fatalf("wrapper 创建前 init/分块应成功: %s", resp)
	}
	// complete 前创建 wrapper 占用 main/videos（模拟竞态窗口）。
	vaultRoot := t.TempDir()
	rec := postUserVol(t, uvmux, map[string]any{
		"name": "vault", "type": nestedRootType,
		"extra": map[string]any{"root": vaultRoot, "target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建封装卷 vault = %d (body: %s)", rec.Code, rec.Body.String())
	}
	status, resp := chunkedComplete(t, fmux)
	if status != http.StatusForbidden {
		t.Fatalf("分块 complete main/videos/x = %d, want 403 (body: %s)", status, resp)
	}
	assertBodyHas(t, resp, "占用")
}

// chunkedInitChunk 走真实分块上传 init→chunk 链路（目标路径显式卷 vol/rel），返回是否成功
// 与最后一个响应体。
func chunkedInitChunk(t *testing.T, mux *http.ServeMux, vol, filename string) (bool, string) {
	t.Helper()
	data := []byte("chunked complete to occupied subdir")
	cs := sha256hex(data)
	initBody, _ := json.Marshal(map[string]any{
		"upload_id": "occ-upload-1", "filename": filename, "total_size": len(data),
		"chunk_size": len(data), "total_chunks": 1, "file_checksum": cs, "file_mod_time": 0,
	})
	req := httptest.NewRequest(http.MethodPost, "/upload/init?volume="+vol, bytes.NewReader(initBody))
	req.Header.Set(headerContentType, "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		return false, rr.Body.String()
	}
	var initResp struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("解析 init 响应: %v (%s)", err, rr.Body.String())
	}
	// 上传唯一分块（chunk_index 参数 + chunk 文件段 + chunk_checksum）。
	var cbuf bytes.Buffer
	mw := multipart.NewWriter(&cbuf)
	part, _ := mw.CreateFormFile("chunk", "0")
	_, _ = part.Write(data)
	_ = mw.Close()
	creq := httptest.NewRequest(http.MethodPost, "/upload/chunk?upload_id="+initResp.UploadID+"&chunk_index=0&chunk_checksum="+cs, &cbuf)
	creq.Header.Set(headerContentType, mw.FormDataContentType())
	crr := httptest.NewRecorder()
	mux.ServeHTTP(crr, creq)
	if crr.Code != http.StatusOK {
		return false, crr.Body.String()
	}
	return true, ""
}

// chunkedComplete 完成最近一次分块会话（POST /upload/complete，JSON body 含 upload_id），
// 返回状态码与响应体。
func chunkedComplete(t *testing.T, mux *http.ServeMux) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"upload_id": "occ-upload-1"})
	creq := httptest.NewRequest(http.MethodPost, "/upload/complete", bytes.NewReader(body))
	creq.Header.Set(headerContentType, "application/json")
	crr := httptest.NewRecorder()
	mux.ServeHTTP(crr, creq)
	return crr.Code, crr.Body.String()
}
