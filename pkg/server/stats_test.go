// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
)

func TestStats_Empty(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var stats StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.DiskUsage.TotalFiles != 0 {
		t.Fatalf("expected 0 files, got %d", stats.DiskUsage.TotalFiles)
	}
}

func TestStats_AfterUpload(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("hello stats")
	uploadFile(t, url, "stats-test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var stats StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.DiskUsage.TotalFiles != 1 {
		t.Fatalf("expected 1 file, got %d", stats.DiskUsage.TotalFiles)
	}
	if stats.DiskUsage.TotalSize != int64(len(body)) {
		t.Fatalf("expected size %d, got %d", len(body), stats.DiskUsage.TotalSize)
	}
}

func TestStats_Fields(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}

	// 验证顶层字段存在
	for _, field := range []string{"disk_usage", "request_counts", "active_connections", "files_uploaded", "bytes_uploaded"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing field: %s", field)
		}
	}
}

func TestStats_StorageFields(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024 // 100 MiB
	})

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}

	// 验证存储字段存在
	for _, field := range []string{"max_storage_bytes", "storage_usage", "storage_user_files", "storage_chunked", "storage_versions", "storage_cloud"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing storage field: %s", field)
		}
	}

	// 验证 max_storage_bytes 与配置一致
	maxBytes, ok := raw["max_storage_bytes"].(float64)
	if !ok || int64(maxBytes) != 100*1024*1024 {
		t.Errorf("expected max_storage_bytes=%d, got %v", 100*1024*1024, raw["max_storage_bytes"])
	}

	// 验证 disk_total/disk_free 存在（值取决于实际文件系统）
	for _, field := range []string{"disk_total", "disk_free", "disk_used"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing disk stat field: %s", field)
		}
	}
}

// TestStats_SkipsInternalDirsAtAnyDepth 验证（审查 #5）：stats 遍历跳过任意层级出现的服务端
// 内部目录——多租户 owner 隔离后版本目录出现在 owner 子目录下（uploadsDir/<owner>/.__versions__/），
// 若只按名字/仅根层跳过会把历史版本文件计为用户文件，导致 TotalFiles/TotalSize 虚高。
// 这里直接在当前 owner 存储根内预置 .__versions__ 子目录结构，然后组装 Handlers 调 WalkDir
// 并断言跳过（不依赖 storageMgr 首次扫描，避免脆性）。
func TestStats_SkipsInternalDirsAtAnyDepth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 布局：owner 根下用户文件 + owner 子目录下的内部版本目录 + 深层含 .__ 的普通文件
	ownerRoot := filepath.Join(dir, "ak-A")
	if err := os.MkdirAll(filepath.Join(ownerRoot, ".__versions__", "doc.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(ownerRoot, "user.txt")
	if err := os.WriteFile(userFile, []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, ".__versions__", "doc.txt", "123"), []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	// 深层含 .__ 的普通文件仍统计（非内部目录首段）：sub/foo.__bar.txt
	if err := os.MkdirAll(filepath.Join(ownerRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, "sub", "foo.__bar.txt"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{StorageRoot: dir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}

	// 复用 statsHandler 的 WalkDir 逻辑：以空 owner 全局视角统计，验证内部目录被跳过
	totalFiles, totalSize := h.walkUploadStats(dir)
	if totalFiles != 2 { // user.txt + sub/foo.__bar.txt
		t.Fatalf("全局统计文件数 = %d, want 2（用户文件 + 深层 .__ 普通文件）", totalFiles)
	}
	if totalSize != int64(len("user")+len("y")) {
		t.Fatalf("全局统计大小 = %d, want %d", totalSize, int64(len("user")+len("y")))
	}
}

// TestStats_OwnerScopedCategories 验证（审查 M5 收敛）：认证用户视角的分类用量按新布局
// 桶前缀归集（user/→user、cloud/+archive/→cloud、chunk/→chunked、version/→versions），
// 他租户目录与全局 .__cloud__（他人/全局云任务文件）不计入认证用户视角（云分类为 0）。
func TestStats_OwnerScopedCategories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownerRoot := filepath.Join(dir, "ak-A")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ownerRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("user", "user.txt"), "user")         // 4
	write(filepath.Join("version", "doc", "v1"), "ver")      // 3
	write(filepath.Join("chunk", "s", "chunk.dat"), "chunk") // 5
	write(filepath.Join("archive", "a.tar.gz"), "arc")       // 3 → cloud 分类
	// 他租户目录（不应计入 ak-A 视角）
	if err := os.MkdirAll(filepath.Join(dir, "ak-B", "user"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ak-B", "user", "other.txt"), []byte("other-tenant-data"), 0644); err != nil {
		t.Fatal(err)
	}
	// 全局遗留 .__cloud__（他人/全局云任务文件）不属于认证用户视角
	if err := os.MkdirAll(filepath.Join(dir, ".__cloud__", "t1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".__cloud__", "t1", "a.bin"), []byte("clouddata"), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{StorageRoot: dir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}

	userFiles, chunked, versions, cloud := h.walkUploadStatsByCategory(ownerRoot)
	// user=4, versions=3, chunked=5, cloud=3（archive 并入 cloud）
	if userFiles != 4 {
		t.Fatalf("userFiles = %d, want 4", userFiles)
	}
	if versions != 3 {
		t.Fatalf("versions = %d, want 3", versions)
	}
	if chunked != 5 {
		t.Fatalf("chunked = %d, want 5", chunked)
	}
	if cloud != 3 {
		t.Fatalf("cloud = %d, want 3（archive 桶并入 cloud 分类）", cloud)
	}
}

// TestStatsHandler_OwnerScoped 验证 GET /api/stats 在认证用户（owner 非空）视角下：
// 文件数/大小与分类用量（user_files/chunked/versions/cloud/usage）全部按 owner 根作用域
// 计算——他租户目录与全局 .__cloud__ 内容不计入（防跨租户元数据泄露）。
func TestStatsHandler_OwnerScoped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfgPtr := newTestCfgPtr(dir)
	h := &Handlers{
		cfgPtr:      cfgPtr,
		storageMgr:  sm,
		logger:      testLogger(),
		auditLogger: testLogger(),
	}

	mustMkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ownerRoot := filepath.Join(dir, "ak-A")
	for _, d := range []string{"user", "version", "chunk", "archive"} {
		mustMkdir(filepath.Join(ownerRoot, d))
	}
	mustWrite(filepath.Join(ownerRoot, "user", "user.txt"), "user")    // 4
	mustWrite(filepath.Join(ownerRoot, "version", "doc", "v1"), "ver") // 3
	mustWrite(filepath.Join(ownerRoot, "chunk", "s", "c1"), "chunk")   // 5
	mustWrite(filepath.Join(ownerRoot, "archive", "a.tar.gz"), "arc")  // 3 → cloud
	// 他租户文件（不应计入 ak-A 视角）
	mustMkdir(filepath.Join(dir, "ak-B", "user"))
	mustWrite(filepath.Join(dir, "ak-B", "user", "other.txt"), "other-tenant-data")
	// 全局遗留 .__cloud__（不应计入 ak-A 视角）
	mustMkdir(filepath.Join(dir, ".__cloud__", "t1"))
	mustWrite(filepath.Join(dir, ".__cloud__", "t1", "a.bin"), "clouddata")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "ak-A"))
		h.statsHandler(w, r)
	})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stats 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 stats 失败: %v", err)
	}
	// 文件数：仅 owner 租户 user/ 桶用户文件 user.txt（功能桶/他租户/全局不计入）
	if resp.DiskUsage.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d, want 1（他租户/全局不计入）", resp.DiskUsage.TotalFiles)
	}
	if resp.StorageUserFiles != 4 {
		t.Fatalf("StorageUserFiles = %d, want 4", resp.StorageUserFiles)
	}
	if resp.StorageVersions != 3 {
		t.Fatalf("StorageVersions = %d, want 3", resp.StorageVersions)
	}
	if resp.StorageChunked != 5 {
		t.Fatalf("StorageChunked = %d, want 5", resp.StorageChunked)
	}
	if resp.StorageCloud != 3 {
		t.Fatalf("StorageCloud = %d, want 3（archive 桶并入 cloud 分类；全局云任务不计入）", resp.StorageCloud)
	}
	if resp.StorageUsage != 4+3+5+3 {
		t.Fatalf("StorageUsage = %d, want %d", resp.StorageUsage, 4+3+5+3)
	}
}

// TestStats_CategoryWalker_SkipsTaskStateDirs 验证 walkUploadStatsByCategory 跳过服务端
// 任务状态目录（.__downloads__/.__sync__）——owner 根下不常见，但与 storage_manager 扫描
// 保持一致，避免任务状态文件被计入用户用量。
func TestStats_CategoryWalker_SkipsTaskStateDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "ak-A")
	for _, d := range []string{".__downloads__", ".__sync__"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".__downloads__", "t.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".__sync__", "s.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{StorageRoot: dir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}
	userFiles, chunked, versions, cloud := h.walkUploadStatsByCategory(root)
	if userFiles != 5 {
		t.Fatalf("userFiles = %d, want 5（任务状态目录跳过）", userFiles)
	}
	if chunked != 0 || versions != 0 || cloud != 0 {
		t.Fatalf("chunked/versions/cloud 应为 0, got %d/%d/%d", chunked, versions, cloud)
	}
}

// actorStatsMux 构造把固定 actor 注入请求 ctx 后转发 stats handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 download_owner_test.go 的模式）。
func actorStatsMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stats", wrap(h.statsHandler))
	return mux
}

// TestStats_OwnerScopedUsage 验证（P4 任务 17）：认证用户 stats 只含本租户占用，分类按桶归集。
// alice 上传 60（user 桶）+ 云任务 40（cloud 桶）→ storage_user_files=60、storage_cloud=40、
// usage=100；bob 全部为 0（租户隔离）。
func TestStats_OwnerScopedUsage(t *testing.T) {
	env := newOwnerEnv(t)
	env.h.storageMgr = NewStorageManager(env.root, 1024*1024, nil, testLogger())

	// alice 上传 60 字节 → user 桶
	umux := actorUploadDeleteMux(env.h, "alice")
	body60 := []byte(strings.Repeat("a", 60))
	if code, resp := uploadAs(t, umux, "f.txt", body60); code != http.StatusOK {
		t.Fatalf("alice 上传应 200, got %d: %s", code, resp)
	}

	// alice 云任务 40 字节 → cloud 桶（经 CloudDownloadManager SubmitAndStart 走写路径配额）
	content := []byte(strings.Repeat("c", 40))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	mgr := NewCloudDownloadManager(env.root, env.h.storageMgr, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}, func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	})
	env.h.cloudMgr = mgr
	t.Cleanup(func() { mgr.Close() })
	task, err := mgr.SubmitAndStart("url", srv.URL, "c.bin", int64(len(content)), t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("云下载应 completed, got %q", task.Status)
	}

	// alice stats：user_files=60, cloud=40, usage=100
	aliceStats := decodeStatsAs(t, env.h, "alice")
	if aliceStats.StorageUserFiles != 60 {
		t.Fatalf("alice StorageUserFiles = %d, want 60", aliceStats.StorageUserFiles)
	}
	if aliceStats.StorageCloud != 40 {
		t.Fatalf("alice StorageCloud = %d, want 40", aliceStats.StorageCloud)
	}
	if aliceStats.StorageUsage != 100 {
		t.Fatalf("alice StorageUsage = %d, want 100", aliceStats.StorageUsage)
	}

	// bob stats：全部为 0（隔离）
	bobStats := decodeStatsAs(t, env.h, "bob")
	if bobStats.StorageUserFiles != 0 || bobStats.StorageCloud != 0 || bobStats.StorageUsage != 0 {
		t.Fatalf("bob stats 应为 0（租户隔离）, got user_files=%d cloud=%d usage=%d",
			bobStats.StorageUserFiles, bobStats.StorageCloud, bobStats.StorageUsage)
	}
}

// decodeStatsAs 以指定 actor 调用 statsHandler 并解码响应。
func decodeStatsAs(t *testing.T, h *Handlers, actor string) StatsResponse {
	t.Helper()
	mux := actorStatsMux(h, actor)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stats 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StatsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 stats 失败: %v", err)
	}
	return resp
}

func TestStorageConfig_Put(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024 // 100 MiB
	})

	// 请求体
	body := bytes.NewReader([]byte(`{"max_storage_bytes": 21474836480}`))
	req, err := http.NewRequest(http.MethodPut, url+"/api/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if raw["success"] != true || raw["changed"] != true {
		t.Errorf("expected success=true, changed=true, got success=%v changed=%v", raw["success"], raw["changed"])
	}

	// 验证 storageMgr 上限已更新
	cfg := cfgPtr.Load()
	if cfg.MaxStorageBytes != 21474836480 {
		t.Errorf("expected config.MaxStorageBytes=21474836480, got %d", cfg.MaxStorageBytes)
	}
}

func TestStorageConfig_Put_BadRequest(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 无效请求体
	body := bytes.NewReader([]byte(`invalid json`))
	req, _ := http.NewRequest(http.MethodPut, url+"/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStorageConfig_Put_NegativeValue(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024
	})

	body := bytes.NewReader([]byte(`{"max_storage_bytes": -1}`))
	req, _ := http.NewRequest(http.MethodPut, url+"/api/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative value, got %d", resp.StatusCode)
	}
}
