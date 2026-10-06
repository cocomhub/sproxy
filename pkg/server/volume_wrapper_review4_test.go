// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_review4_test.go 验证第四轮对抗评审（PR #741）的封装卷修复：
//   - CRIT-1：外部封装卷删除比对内容 checksum（错 checksum → 400 保留文件，对齐本地契约）；
//   - CRIT-2：同卷 rename 源侧写保护（占用子目录密文不得 rename 出占用目录 → 403）；
//   - I-③：删带数据封装卷先整流删卷内文件再清占用子目录（不留密文、重建同目录可复用）；
//   - I-④：move 目标=封装卷返回明确 400（非误导性「无效文件路径」），且源侧占用 403 优先；
//   - I-5：外部删除候选按 owner ACL 过滤（未授权 owner 不泄存在性、不删文件）；
//   - I-②：跨卷 move 成功后同步搜索索引（源列表移除、目标列表写入）；
//   - C4-⑥：被占用底层子目录直查（?volume=main&subdir=videos）→ 404 + 占用提示（非空 200）。
//
// sproxy:serial: 复用包级 wrapperFSVolSet（真实 FS 封装后端工厂按 target 解析底层卷），
// 无法与其它用例并行——非并发测试。

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWrapperReview4API 装配第四轮评审封装卷测试服务（main 本地卷 + 真实 FS 封装后端 + 用户卷
// store + 卷 API/上传/删除/rename/move/列表 mux）。返回 (URL, Handlers)。
func newWrapperReview4API(t *testing.T, volumes []VolumeConfig) (string, *Handlers) {
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
	mux.HandleFunc("GET /api/volumes/user", wrap(h.listUserVolumesHandler))
	mux.HandleFunc("GET /api/volumes", wrap(h.listVolumesHandler))
	mux.HandleFunc("POST /api/volumes/move", wrap(h.moveVolumeHandler))
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /rename", wrap(h.rename))
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, h
}

// review4Req 发 HTTP 请求并返回状态码与响应体（path 为 baseURL 之后的相对路径）。
func review4Req(t *testing.T, baseURL, method, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := testHTTPClientAt()
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestWrapperDelete_ChecksumGate（评审 CRIT-1）：外部封装卷删除必须比对实际内容 checksum——
// 客户端带错 checksum → 400 保留文件（对齐本地删除契约），不再静默删。
func TestWrapperDelete_ChecksumGate(t *testing.T) {
	mainRoot := t.TempDir()
	base, h := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})

	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType, "capacity": int64(100 << 20),
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := []byte("wrapper secret content")
	status, _, respBody := volumeUpload(t, base, "prot.txt", body, "videos")
	if status != http.StatusOK {
		t.Fatalf("上传封装卷 = %d, want 200 (body: %s)", status, respBody)
	}
	if got := h.volSet.Pool("videos").Usage(); got != int64(len(body)) {
		t.Fatalf("委托子池 Usage=%d want %d", got, len(body))
	}

	// 错误 checksum → 400 + 保留文件（委托子池用量不变）。
	wrong := "deadbeef" + strings.Repeat("0", 56)
	path := "/delete?filename=" + url.QueryEscape("prot.txt") + "&volume=" + url.QueryEscape("videos")
	status, delBody := review4Req(t, base, http.MethodPost, path, map[string]string{headerFileChecksum: wrong})
	if status != http.StatusBadRequest {
		t.Fatalf("错误 checksum 删除 = %d, want 400（保留文件）(body: %s)", status, delBody)
	}
	if got := h.volSet.Pool("videos").Usage(); got != int64(len(body)) {
		t.Fatalf("错误 checksum 后文件应保留：委托子池 Usage=%d want %d", got, len(body))
	}
	// 正确 checksum 仍可删（400 后文件未被破坏）。
	status, delBody = deleteFileVol(t, base, "prot.txt", body, "videos")
	if status != http.StatusOK {
		t.Fatalf("正确 checksum 删除 = %d, want 200 (body: %s)", status, delBody)
	}
	if got := h.volSet.Pool("videos").Usage(); got != 0 {
		t.Fatalf("删除后委托子池 Usage=%d want 0", got)
	}
}

// TestWrapperRename_SourceOccupied403（评审 CRIT-2）：同卷 rename 源侧命中被封装卷占用的
// 子目录 → 403（此前仅目标侧守卫，占用子目录密文可同卷 rename 出占用目录破坏封装）。
func TestWrapperRename_SourceOccupied403(t *testing.T) {
	mainRoot := t.TempDir()
	base, _ := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 20}})
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 源文件直接落盘 main/alice/user/videos/x.txt（占用目录内，API 上传会被写保护拦）。
	if err := os.MkdirAll(filepath.Join(mainRoot, "alice", "user", "videos"), 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "alice", "user", "videos", "x.txt"), []byte("src"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	// rename videos/x.txt → safe/x.txt：源侧占用 → 403（目标 safe 非占用，此前仅目标守卫放行）。
	path := "/rename?from=" + url.QueryEscape("videos/x.txt") + "&to=" + url.QueryEscape("safe/x.txt")
	status, body := review4Req(t, base, http.MethodPost, path,
		map[string]string{headerFileChecksum: sha256hex([]byte("src"))})
	if status != http.StatusForbidden {
		t.Fatalf("rename 源侧占用 = %d, want 403 (body: %s)", status, body)
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "alice", "user", "videos", "x.txt")); err != nil {
		t.Fatalf("被拒绝的 rename 不应搬走源文件: %v", err)
	}
}

// TestWrapperDelete_WithData_CleansDir（评审 I-③）：删带数据封装卷先整流删除卷内密文，再清
// 占用子目录——不留残留、重建同目录可复用（此前空目录才清、有数据残留 + 409）。
func TestWrapperDelete_WithData_CleansDir(t *testing.T) {
	mainRoot := t.TempDir()
	base, h := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType, "capacity": int64(100 << 20),
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := []byte("with data content")
	if status, _, respBody := volumeUpload(t, base, "a.bin", body, "videos"); status != http.StatusOK {
		t.Fatalf("上传封装卷 = %d, want 200 (body: %s)", status, respBody)
	}
	// 数据落盘密文（main/videos/user 存在）。
	if _, err := os.Stat(filepath.Join(mainRoot, "videos", "user")); err != nil {
		t.Fatalf("封装卷数据应已落盘 main/videos/user: %v", err)
	}
	// 删封装卷（带数据）：整流删除卷内数据 + 清占用子目录。
	rec := wrapperJSON(t, base, http.MethodDelete, "/api/volumes/user?name=videos", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删带数据封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "videos")); !os.IsNotExist(err) {
		t.Fatalf("删带数据封装卷后 main/videos 应整体清理（含密文）, stat err=%v", err)
	}
	if p := h.volSet.Pool("videos"); p != nil {
		t.Fatalf("删封装卷后 Pool(videos) 应移除委托（nil）")
	}
	// 重建同目录可复用（不再 409）。
	rec = wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("删后重建同目录 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestWrapperMove_TargetWrapper_ClearMessage（评审 I-④）：move 目标=封装卷 → 明确 400
// （「封装卷不能作目标」，非误导性「无效的文件路径」）；源侧占用 403 优先于目标类型检查。
func TestWrapperMove_TargetWrapper_ClearMessage(t *testing.T) {
	mainRoot := t.TempDir()
	base, _ := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 20}})
	// 建两个封装卷：wrapB（目标）+ videos（占用 main/videos 供源侧守卫用例）。
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "wrapB", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/wrapdir"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建 wrapB = %d, want 200", rec.Code)
	}
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建 videos = %d, want 200", rec.Code)
	}
	// 普通文件落盘 main/alice/user/a.txt。
	if err := os.MkdirAll(filepath.Join(mainRoot, "alice", "user"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "alice", "user", "a.txt"), []byte("aa"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	// move a.txt main→wrapB：目标=封装卷 → 400 + 明确文案（非「无效的文件路径」）。
	status, body := moveVolume(t, base, "main", "wrapB", "a.txt")
	if status != http.StatusBadRequest {
		t.Fatalf("move 目标封装卷 = %d, want 400 (body: %s)", status, body)
	}
	if !strings.Contains(string(body), "目标卷不支持跨卷移动") {
		t.Fatalf("目标封装卷文案应明确（非误导性「无效的文件路径」）: %s", body)
	}
	// 源侧占用优先：main/videos/x 落盘 → move main→wrapB → 403（源侧守卫先于目标类型检查）。
	if err := os.MkdirAll(filepath.Join(mainRoot, "alice", "user", "videos"), 0o755); err != nil {
		t.Fatalf("mkdir videos: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "alice", "user", "videos", "x.txt"), []byte("src"), 0o600); err != nil {
		t.Fatalf("write videos/x.txt: %v", err)
	}
	status, body = moveVolume(t, base, "main", "wrapB", "videos/x.txt")
	if status != http.StatusForbidden {
		t.Fatalf("源侧占用 + 目标封装卷 = %d, want 403（源侧守卫优先）(body: %s)", status, body)
	}
}

// TestWrapperDelete_CrossOwnerACL（评审 I-5）：外部删除候选按 owner ACL 过滤——非 owner 对
// 他人封装卷（owner-only ACL）删除 → 404（不泄存在性、不删文件），而非经全卷扫描命中。
func TestWrapperDelete_CrossOwnerACL(t *testing.T) {
	mainRoot := t.TempDir()
	base, h := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})
	// alice 建封装卷 videos + 上传文件。
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200", rec.Code)
	}
	body := []byte("alice secret")
	if status, _, respBody := volumeUpload(t, base, "secret.txt", body, "videos"); status != http.StatusOK {
		t.Fatalf("上传 = %d, want 200 (body: %s)", status, respBody)
	}
	// bob（非 owner）删除 → 404：ACL 排除卷不扫描、不删、不泄存在性。直接调 handler
	// （harness mux 会把 actor 固定为 alice，故 bypass 用 h.delete 带 bob ctx）。
	req, err := http.NewRequest(http.MethodPost,
		"/delete?filename="+url.QueryEscape("secret.txt")+"&volume="+url.QueryEscape("videos"), nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set(headerFileChecksum, sha256hex(body))
	req = req.WithContext(withActor(req.Context(), "bob"))
	rec := httptest.NewRecorder()
	h.delete(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非 owner 删他人封装卷 = %d, want 404（不泄存在性）(body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "videos", "user")); err != nil {
		t.Fatalf("bob 删除不应影响 alice 封装卷数据: %v", err)
	}
}

// TestWrapperMove_IndexUpdated（评审 I-②）：跨卷 move 成功后同步搜索索引——先列表建索引缓存
// 源卷 a.txt，move 到目标卷后再列表：源卷不再列（幽灵清除）、目标卷列出（索引写入）。
func TestWrapperMove_IndexUpdated(t *testing.T) {
	mainRoot := t.TempDir()
	diskRoot := t.TempDir()
	base, _ := newWrapperReview4API(t, []VolumeConfig{
		{Name: "main", Root: mainRoot, VolCapacity: 1 << 30},
		{Name: "disk2", Root: diskRoot, VolCapacity: 1 << 30},
	})
	// 源文件直接落盘 main/alice/user/a.txt。
	if err := os.MkdirAll(filepath.Join(mainRoot, "alice", "user"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainRoot, "alice", "user", "a.txt"), []byte("aaa"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	// 先列表 main（volume 过滤）→ 建索引缓存含 a.txt。
	st, body := review4Req(t, base, http.MethodGet, "/api/files?volume=main", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "a.txt") {
		t.Fatalf("列表 main 应含 a.txt（索引构建）: status=%d body=%s", st, body)
	}
	// move a.txt main→disk2（物理搬移）。
	status, mbody := moveVolume(t, base, "main", "disk2", "a.txt")
	if status != http.StatusOK {
		t.Fatalf("move = %d, want 200 (body: %s)", status, mbody)
	}
	// 源列表 main：a.txt 应被移除（索引同步，幽灵清除）。
	st, body = review4Req(t, base, http.MethodGet, "/api/files?volume=main", nil)
	if st != http.StatusOK {
		t.Fatalf("源列表 main status=%d", st)
	}
	if strings.Contains(string(body), "a.txt") {
		t.Fatalf("move 后源卷 main 列表仍含 a.txt（索引未同步，stale 幽灵）: %s", body)
	}
	// 目标列表 disk2：a.txt 应列出（索引写入目标卷）。
	st, body = review4Req(t, base, http.MethodGet, "/api/files?volume=disk2", nil)
	if st != http.StatusOK {
		t.Fatalf("目标列表 disk2 status=%d", st)
	}
	if !strings.Contains(string(body), "a.txt") {
		t.Fatalf("move 后目标卷 disk2 列表应含 a.txt（索引同步失败）: %s", body)
	}
}

// TestWrapperList_OccupiedSubdir_ReadOnly（评审 C4-⑥，2026-10-07）：被封装卷占用的底层
// 子目录直查（?volume=main&subdir=videos）保持**读路径不拦**——基础卷视图可查（空 200、
// 不泄露封装卷数据），WebUI 从基础卷列表隐藏占用目录；此读侧契约由既有写保护测试钉住
// （200+legacy 内容），本测试钉住「不泄露封装卷密文视图」。
func TestWrapperList_OccupiedSubdir_ReadOnly(t *testing.T) {
	mainRoot := t.TempDir()
	base, _ := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})
	if rec := wrapperJSON(t, base, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200", rec.Code)
	}
	// 封装卷内上传数据（密文落 main/videos，明文不在 main 基础卷视图）。
	body := []byte("wrapper ciphertext secret")
	if status, _, respBody := volumeUpload(t, base, "c.bin", body, "videos"); status != http.StatusOK {
		t.Fatalf("上传封装卷 = %d, want 200 (body: %s)", status, respBody)
	}
	// 占用子目录直查：200（读路径不拦）+ 基础卷视图不泄露封装卷文件（c.bin 不见）。
	st, listBody := review4Req(t, base, http.MethodGet, "/api/files?volume=main&subdir=videos", nil)
	if st != http.StatusOK {
		t.Fatalf("占用子目录直查 = %d, want 200（读路径不拦）(body: %s)", st, listBody)
	}
	if strings.Contains(string(listBody), "c.bin") {
		t.Fatalf("占用子目录基础卷视图不应泄露封装卷明文文件: %s", listBody)
	}
	// 主列表 main（根目录）正常 200。
	st, body = review4Req(t, base, http.MethodGet, "/api/files?volume=main", nil)
	if st != http.StatusOK {
		t.Fatalf("主列表 main = %d, want 200 (body: %s)", st, body)
	}
}

// failingConfigWriter 是评审 I-4 的测试 mock：AppendVolume 成功、RemoveVolume 恒失败——
// 模拟「config 文件不可写/格式非法」下删除路径应终止删除（卷保留）。
type failingConfigWriter struct{}

func (f *failingConfigWriter) AppendVolume(v UserVolume) error { return nil }
func (f *failingConfigWriter) RemoveVolume(name string) error {
	return fmt.Errorf("config 文件不可写（测试注入）")
}

// TestWrapperDelete_ConfigRemoveFail_Aborts（评审 I-4，2026-10-07）：删除封装卷时 config
// 写回**先行**且失败即终止删除——否则「store 已删 + config 残留」会生成重启复活的僵尸卷 +
// 子目录 409。断言：config RemoveVolume 失败 → DELETE 返回 500、卷仍在 store/Set（可列出）。
func TestWrapperDelete_ConfigRemoveFail_Aborts(t *testing.T) {
	mainRoot := t.TempDir()
	url, h := newWrapperReview4API(t, []VolumeConfig{{Name: "main", Root: mainRoot, VolCapacity: 1 << 30}})
	// 建封装卷（configWriter nil 创建路径不写回，volume 正常创建）。
	if rec := wrapperJSON(t, url, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": "videos", "type": wrapperFSType,
		"extra": map[string]any{"target": "main/videos"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建封装卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	// 注入失败写回器 → DELETE 应 500、卷保留（未删 Set/store/links）。
	h.SetConfigWriter(&failingConfigWriter{})
	rec := wrapperJSON(t, url, http.MethodDelete, "/api/volumes/user?name=videos", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("config 写回失败删除 = %d, want 500（删除终止）(body: %s)", rec.Code, rec.Body.String())
	}
	// 卷仍在（store/Set/占用关联未清除——不是僵尸路径）。
	if v, err := h.userVolumes.Get("alice", "videos"); err != nil || v == nil {
		t.Fatalf("config 写回失败后卷应保留在 store: v=%+v err=%v", v, err)
	}
	if h.volSet.External("videos") == nil {
		t.Fatal("config 写回失败后卷不应从 Set 移除")
	}
	// 占用关联仍在（links 未清）。
	if refs := h.links().refsOfBase("main"); len(refs) != 1 {
		t.Fatalf("config 写回失败后占用关联应保留: %+v", refs)
	}
}
