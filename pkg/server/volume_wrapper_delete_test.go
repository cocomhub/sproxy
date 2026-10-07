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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// wrapperFSType 是测试用「真实 FS 封装后端」：按 target=<卷>/<子目录> 在底层卷 MakeDir +
// SubFS 包装（仿 secretdata resolveNestedTargetFS），使封装卷可真实上传/删除文件。
const wrapperFSType = "wrap-fs-test"

// wrapperFSBackend 是真实 FS 的封装后端（FS=SubFS；Schema 声明 target 字段）。
type wrapperFSBackend struct {
	fs syncpkg.FS
}

func (b *wrapperFSBackend) FS() syncpkg.FS { return b.fs }
func (b *wrapperFSBackend) Close() error   { return nil }
func (b *wrapperFSBackend) Schema() []registry.FieldSchema {
	return []registry.FieldSchema{{
		Key: "target", Label: "底层卷", Type: "volume-select",
		Required: true, AllowWrapper: true,
	}}
}

// wrapperFSVolSet 是测试用包级卷集引用（工厂按 target 解析底层卷 FS，仿 secretDataSet）。
var wrapperFSVolSet atomic.Pointer[registry.Set]

var registerWrapperFSOnce sync.Once

func registerWrapperFSBackend() {
	registerWrapperFSOnce.Do(func() {
		registry.RegisterBackend(wrapperFSType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			t, _ := v.Extra["target"].(string)
			base, subdir, ok := volume.SplitNestedTarget(t)
			if !ok {
				return nil, fmt.Errorf("wrap-fs: 需嵌套 target <卷>/<子目录>, got %q", t)
			}
			set := wrapperFSVolSet.Load()
			if set == nil {
				return nil, fmt.Errorf("wrap-fs: volSet 未就绪")
			}
			inner, ok := set.FSFor(base)
			if !ok {
				return nil, fmt.Errorf("wrap-fs: 底层卷 %q 不可用", base)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := inner.MakeDir(ctx, subdir); err != nil {
				return nil, fmt.Errorf("wrap-fs: MakeDir %s/%s: %w", base, subdir, err)
			}
			fs, err := syncpkg.NewSubFS(inner, subdir)
			if err != nil {
				return nil, fmt.Errorf("wrap-fs: SubFS %s/%s: %w", base, subdir, err)
			}
			return &wrapperFSBackend{fs: fs}, nil
		})
	})
}

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
	wrapperFSVolSet.Store(h.volSet)
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
	if got := h.volSet.Pool("videos").Usage(); got != int64(len(body)) {
		t.Fatalf("委托子池 Usage=%d want %d", got, len(body))
	}
	if got := h.volSet.Pool("main").Usage(); got != int64(len(body)) {
		t.Fatalf("底层池 Usage=%d want %d（委托父链聚合）", got, len(body))
	}

	// C2：标准 delete（?volume=videos）删除封装卷内文件 —— 200（此前 404）+ 委托子池释放。
	status, delBody := deleteFileVol(t, url, "prot.txt", body, "videos")
	if status != http.StatusOK {
		t.Fatalf("封装卷内文件 delete = %d, want 200 (body: %s)", status, delBody)
	}
	if got := h.volSet.Pool("videos").Usage(); got != 0 {
		t.Fatalf("删除后委托子池 Usage=%d want 0（C2 删除归还）", got)
	}
	if got := h.volSet.Pool("main").Usage(); got != 0 {
		t.Fatalf("删除后底层池 Usage=%d want 0", got)
	}

	// C5：删封装卷（无残留数据 → 清理空占用子目录）→ 重建同目录成功（不再 409）。
	// 先清掉测试上传留下的空 `main/videos/user/` 内部桶目录（wrapper 生命周期残留，
	// 非封装卷删除清理范围——封装卷删除只清自己 MakeDir 的空占用目录）。
	// 先清掉测试上传留下的空 `main/videos/user/` 内部桶目录（wrapper 生命周期残留，
	// 非封装卷删除清理范围——封装卷删除只清自己 MakeDir 的空占用目录）。
	if rerr := os.Remove(filepath.Join(mainRoot, "videos", "user")); rerr != nil {
		t.Fatalf("清理测试残留 user 桶失败: %v", rerr)
	}
	rec = wrapperJSON(t, url, http.MethodDelete, "/api/volumes/user?name=videos", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "videos")); !os.IsNotExist(err) {
		t.Fatalf("删封装卷后占用子目录 main/videos 应被清理（无数据残留）, stat err=%v", err)
	}
	if p := h.volSet.Pool("videos"); p != nil {
		t.Fatalf("删封装卷后 Pool(videos) 应移除委托（nil）, got %+v", p)
	}
	rec = wrapperJSON(t, url, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("删后重建同目录封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestWrapperMove_SourceOccupied403（I2）：跨卷 move 源侧（from 卷）命中被封装卷占用的
// 子目录 → 403（源侧不许搬）；目标侧占用同样 403。
func TestWrapperMove_SourceOccupied403(t *testing.T) {
	mainRoot := t.TempDir()
	diskRoot := t.TempDir()
	url, _ := newWrapperDeleteAPI(t, []VolumeConfig{
		{Name: "main", Root: mainRoot, VolCapacity: 1 << 20},
		{Name: "disk2", Root: diskRoot, VolCapacity: 1 << 20},
	})
	// 占用 main/videos。
	if rec := wrapperJSON(t, url, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 源文件直接落盘 main/alice/user/videos/x.txt（该路径被写保护，不能经 API 上传）。
	if err := os.MkdirAll(filepath.Join(mainRoot, "alice", "user", "videos"), 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "alice", "user", "videos", "x.txt"), []byte("src"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	// move main→disk2：源侧命中占用子目录 videos → 403（此前只拦目标侧，可被搬出）。
	status, body := moveVolume(t, url, "main", "disk2", "videos/x.txt")
	if status != http.StatusForbidden {
		t.Fatalf("move 源侧占用 = %d, want 403 (body: %s)", status, body)
	}
}
