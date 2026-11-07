// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_delete_test.go 验证第三轮评审修复（PR #741）的封装卷删除闭环 + 配额归还 +
// 容量展示：
//   - C2：封装卷内文件经标准 delete API（?volume=wrapper）走外部 FS 整流删除 + 委托子 Scope
//     配额释放（不再 404、不再从底层根池释放造成幽灵占用）；
//   - C5：删封装卷归还委托配额给底层池 + 清理自建空占用子目录（重建同目录不 409）；
//   - I1：/api/volumes 对封装卷显示委托配额容量（Pool.MaxBytes），不再恒「不限」；
//   - I2：跨卷 move 源侧占用子目录 → 403（源侧不许搬）。
//
// sproxy:serial: 共享包级 wrapperFSVolSet（真实 FS 包装后端工厂按 target 解析底层卷），
// 无法与其它用例并行——非并发测试。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// newWrapperDeleteAPI 装配封装卷删除测试服务（main 本地卷 + 真实 FS 封装后端 + 用户卷 store
// + 卷 API/上传/删除/move mux）。返回 (URL, Handlers)。
func newWrapperDeleteAPI(t *testing.T, volumes []VolumeConfig) (string, *Handlers) {
	t.Helper()
	registerWrapperFSBackend()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)

	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), "alice"))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/volumes/user", wrap(h.createUserVolumeHandler))
	mux.HandleFunc("DELETE /api/volumes/user", wrap(h.deleteUserVolumeHandler))
	mux.HandleFunc("GET /api/volumes", wrap(h.listVolumesHandler))
	mux.HandleFunc("POST /api/volumes/move", wrap(h.moveVolumeHandler))
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, h
}

// wrapperJSON 向 baseURL 发 JSON 请求并返回响应。
func wrapperJSON(t *testing.T, baseURL, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := testHTTPClientAt()
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	rec := httptest.NewRecorder()
	rec.Code = resp.StatusCode
	rec.Body = bytes.NewBuffer(b)
	return rec
}

// deleteFileVol 按内容 checksum 删除 user 文件（带显式 volume）。
func deleteFileVol(t *testing.T, baseURL, filename string, content []byte, vol string) (int, []byte) {
	t.Helper()
	u := baseURL + "/delete?filename=" + url.QueryEscape(filename)
	if vol != "" {
		u += "&volume=" + url.QueryEscape(vol)
	}
	req, err := http.NewRequest("POST", u, nil)
	if err != nil {
		t.Fatalf("new delete req: %v", err)
	}
	req.Header.Set(headerFileChecksum, sha256hex(content))
	hc := testHTTPClientAt()
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", filename, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestWrapperDelete_QuotaRelease_CapacityDisplay（C2/C5/I1）：
//
//	建封装卷（容量）→ /api/volumes 显示委托容量 → 经封装卷上传文件 → 委托子池计量 →
//	标准 delete（?volume=wrap）删除文件（外部 FS 整流 + 委托子池释放）→ 删封装卷归还配额
//	+ 清理空占用子目录 → 重建同目录成功。
func TestWrapperDelete_QuotaRelease_CapacityDisplay(t *testing.T) {
	mainRoot := t.TempDir()
	url, h := newWrapperDeleteAPI(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})

	// 建封装卷 videos（capacity=100MiB）。
	rec := wrapperJSON(t, url, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType, "capacity": int64(100 << 20),
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 委托池已挂载：Pool(videos) = 底层池子 Scope，MaxBytes = capacity。
	if p := h.volSet.Pool("videos"); p == nil || p.MaxBytes() != 100<<20 {
		t.Fatalf("Pool(videos) 委托缺失/容量错: %+v", p)
	}

	// I1：/api/volumes 对封装卷显示委托容量（不再恒 0/不限）。
	rec = wrapperJSON(t, url, http.MethodGet, "/api/volumes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/volumes = %d", rec.Code)
	}
	var out volumesListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /api/volumes: %v", err)
	}
	var found bool
	for _, v := range out.Volumes {
		if v.Name == "videos" {
			found = true
			if v.Capacity != 100<<20 {
				t.Fatalf("封装卷 videos capacity=%d want %d（委托容量展示）", v.Capacity, 100<<20)
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/volumes 未含封装卷 videos: %+v", out.Volumes)
	}

	// 经封装卷上传文件（外部 FS 整流写 + 委托子池计量）。multipart 文件名会被清洗为
	// basename（Go 语义），故用 prot.txt 而非 videos/prot.txt。
	body := []byte("wrapper secret content")
	status, _, respBody := volumeUpload(t, url, "prot.txt", body, "videos")
	if status != http.StatusOK {
		t.Fatalf("上传封装卷 = %d, want 200 (body: %s)", status, respBody)
	}
	// 继承池（阶段 2 迁移：Pool(videos) = NewPoolWithParent(Pool(main), max_bytes)）：
	// 上传经 FS 层容量计数器入账，提交沿父链聚合到底层 main 池（同一字节只在父池计一次）。
	vp, mp := h.volSet.Pool("videos"), h.volSet.Pool("main")
	if vp.Usage() <= 0 {
		t.Fatal("上传后封装卷继承池 Usage 应 > 0（容量记账未接线）")
	}
	if mp.Usage() != vp.Usage() {
		t.Fatalf("底层池 Usage=%d ≠ 封装卷池 %d（继承父链聚合断裂）", mp.Usage(), vp.Usage())
	}

	// C2：标准 delete（?volume=videos）删除封装卷内文件 —— 200（此前 404）+ 委托子池释放。
	status, delBody := deleteFileVol(t, url, "prot.txt", body, "videos")
	if status != http.StatusOK {
		t.Fatalf("封装卷内文件 delete = %d, want 200 (body: %s)", status, delBody)
	}
	if got := h.volSet.Pool("videos").Usage(); got != 0 {
		t.Fatalf("删除后封装卷继承池 Usage=%d want 0（C2 删除归还）", got)
	}
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("删除后底层池 Usage=%d want 0", got)
	}

	// C5：删封装卷 → 拆除闭环（回收源卷封印 + 归还并摘除继承池 + **擦除独占区间内容**）
	// → 重建同区间成功（enclosure 无「子目录须不存在」前置条件，重建天然可行）。
	rec = wrapperJSON(t, url, http.MethodDelete, "/api/volumes/user?name=videos", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "videos")); !os.IsNotExist(err) {
		t.Fatalf("删封装卷后占用区间 main/videos 应被擦除（不留密文残留）, stat err=%v", err)
	}
	if p := h.volSet.Pool("videos"); p != nil {
		t.Fatalf("删封装卷后 Pool(videos) 应摘除登记（nil）, got %+v", p)
	}
	if rule := h.volSet.Root("main").SealAt("videos/x"); rule != nil {
		t.Fatalf("删封装卷后源卷封印应被回收（否则区间永久只读）, got %+v", rule)
	}
	rec = wrapperJSON(t, url, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("删后重建同区间封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// wrapperCounterUsage 返回封装卷的 FS 层容量计数（外部卷容量由 CapacityFS 计数器权威记账
// ——装配时 `capacity.WrapBackend` 包装，计数提交进该卷**继承池**并沿父链聚合到源卷池）。
// 阶段 2 迁移后这是封装卷占用的**唯一**账本来源（路由侧对封装卷只做探测 + 立即释放）。
func wrapperCounterUsage(t *testing.T, h *Handlers, volName string) int64 {
	t.Helper()
	be := h.volSet.External(volName)
	if be == nil {
		t.Fatalf("卷 %q 未注册（External 缺失）", volName)
	}
	up, ok := be.(registry.UsageProvider)
	if !ok {
		t.Fatalf("卷 %q 未实现 UsageProvider（容量计数缺失）", volName)
	}
	return up.Usage()
}
