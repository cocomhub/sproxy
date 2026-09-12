// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// feature_bucket_volume_test.go 验证 T6a 特征桶跟随 + meta 归属 + 版本端点 home 卷：
//  1. TestMeta_DefaultVolumeOnly：分叉配置（显式 volumes[0].root ≠ storage_root）下凭据/checksum
//     落默认卷 meta，非默认卷与 cfg.StorageRoot 均无全局 meta（建议 9 回归）。
//  2. TestVersioning_FollowsUserVolume：disk2 文件的版本同卷（list/restore/delete 先定位 home 卷）。
//  3. TestChunkedUpload_InitPinsVolume：init 即定卷（默认卷满换 disk2），chunk/complete 同卷原子。
//  4. TestCloudArchive_DefaultVolumeBinding：cloud/archive 产物绑定默认卷根（非 cfg.StorageRoot）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
)

// divergentVolumeConfig 构造「默认卷根 ≠ cfg.StorageRoot」的分叉配置：
// cfg.StorageRoot = dirA（旧单根语义根），volumes[0].root = dirB（显式非占位 → 默认卷根），
// disk2 = dirC。resolveDefaultVolumeRoot 应裁决默认卷根 = dirB。
func divergentVolumeConfig(t *testing.T) (dirA, dirB, dirC string, cfg *Config) {
	t.Helper()
	dirA, dirB, dirC = t.TempDir(), t.TempDir(), t.TempDir()
	cfg = Default()
	cfg.StorageRoot = dirA
	cfg.Placement = "prefer-default"
	cfg.MaxStorageBytes = 1 << 20
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirB, VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirC, VolCapacity: 1 << 20},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	if got := resolveDefaultVolumeRoot(cfg); got != dirB {
		t.Fatalf("resolveDefaultVolumeRoot = %q, want %q（分叉配置默认卷 = 显式 volumes[0].root）", got, dirB)
	}
	return dirA, dirB, dirC, cfg
}

// TestMeta_DefaultVolumeOnly 验证 meta 归属默认卷不变式（建议 9）：分叉配置下凭据
// （credentials.json）与 checksum（checksums.json）只在默认卷根 <owner>/meta/，cfg.StorageRoot
// 与非默认卷均不出现全局 meta。
func TestMeta_DefaultVolumeOnly(t *testing.T) {
	t.Parallel()

	// ---- 凭据落点：BootstrapServerCredentials 按默认卷根（非 cfg.StorageRoot）建 meta ----
	t.Run("credentials_default_volume_root", func(t *testing.T) {
		dirA, dirB, _, cfg := divergentVolumeConfig(t)
		ring, store, err := BootstrapServerCredentials(cfg, nil)
		if err != nil {
			t.Fatalf("BootstrapServerCredentials: %v", err)
		}
		if ring == nil {
			t.Fatal("ring nil")
		}
		keys := seedTestRing(t, "ak-meta-defaultvol", testAccessSecret, false)
		if serr := store.Save(keys); serr != nil {
			t.Fatalf("store.Save: %v", serr)
		}
		// 凭据必须落默认卷 dirB/anonymous/meta/credentials.json。
		credDefault := filepath.Join(dirB, anonymousOwner, "meta", "credentials.json")
		if _, statErr := os.Stat(credDefault); statErr != nil {
			t.Fatalf("凭据应落默认卷根 %s: %v", credDefault, statErr)
		}
		// cfg.StorageRoot（dirA）下不得出现凭据（修复前落这里 → 重启丢 Ring）。
		if _, statErr := os.Stat(filepath.Join(dirA, anonymousOwner, "meta", "credentials.json")); statErr == nil {
			t.Fatalf("凭据不应落 cfg.StorageRoot（分叉配置）：%s", filepath.Join(dirA, anonymousOwner, "meta"))
		}
		// 重启载入还原同一 Ring。
		ring2, _, err := BootstrapServerCredentials(cfg, nil)
		if err != nil {
			t.Fatalf("second Bootstrap: %v", err)
		}
		snap := ring2.Snapshot()
		if len(snap) != 1 || snap[0].AK != "ak-meta-defaultvol" {
			t.Fatalf("重启后凭据 Ring 还原失败: %+v", snap)
		}
	})

	// ---- checksum + user 文件落默认卷根；非默认卷无 meta/checksums ----
	t.Run("checksum_and_upload_on_default_volume", func(t *testing.T) {
		dirA, dirB, dirC, cfg := divergentVolumeConfig(t)
		h := buildVolSetHandlers(t, cfg)
		ts := httptest.NewServer(actorUploadMux(h, "alice"))
		t.Cleanup(ts.Close)

		body := []byte("meta-default-volume content")
		status, _, respBody := volumeUpload(t, ts.URL, "f.txt", body, "")
		if status != http.StatusOK {
			t.Fatalf("上传应 200, got %d %s", status, respBody)
		}
		// user 文件在默认卷 dirB（tenantFor = globalRoot = vs.DefaultRoot）。
		if !diskFileExists(t, dirB, "alice", "f.txt") {
			t.Fatal("文件应落默认卷 dirB/alice/user/f.txt")
		}
		if diskFileExists(t, dirC, "alice", "f.txt") {
			t.Fatal("文件不应落 disk2（prefer-default + dirB 容量足）")
		}
		// checksum 落默认卷 dirB/alice/meta/checksums.json。
		csDefault := filepath.Join(dirB, "alice", "meta", "checksums.json")
		data, err := os.ReadFile(csDefault)
		if err != nil {
			t.Fatalf("checksum 应落默认卷 meta %s: %v", csDefault, err)
		}
		if !bytes.Contains(data, []byte("user/f.txt")) {
			t.Fatalf("checksum.json 应含 user/f.txt 记录, got %s", data)
		}
		// cfg.StorageRoot（dirA）不得出现租户树（写入侧恒经 tenantFor 默认卷）。
		if _, err := os.Stat(filepath.Join(dirA, "alice")); err == nil {
			t.Fatalf("cfg.StorageRoot 不应出现租户树（分叉配置）")
		}
		// 非默认卷 disk2 不得有 alice/meta/checksums.json（meta 单一权威在默认卷）。
		if _, err := os.Stat(filepath.Join(dirC, "alice", "meta")); err == nil {
			t.Fatalf("非默认卷不应预建 meta 桶")
		}
	})
}

// versionFeatureServer 构造带 volSet + upload/list/restore/delete 版本路由的 actor 服务。
func versionFeatureServer(t *testing.T, actor string, volumes []VolumeConfig, mod func(*Config)) (string, *Handlers, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	cfg.Volumes = volumes
	if mod != nil {
		mod(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("GET /api/versions", wrap(h.listVersionsHandler))
	mux.HandleFunc("POST /api/versions/restore", wrap(h.restoreVersionHandler))
	mux.HandleFunc("DELETE /api/versions", wrap(h.deleteVersionHandler))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, h, dirs
}

type versionListResp struct {
	Versions []struct {
		Filename  string `json:"filename"`
		VersionID int64  `json:"version_id"`
		Size      int64  `json:"size"`
	} `json:"versions"`
}

func listVersionsJSON(t *testing.T, baseURL, filename string) versionListResp {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/versions?filename=" + filename)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list versions status=%d", resp.StatusCode)
	}
	var out versionListResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	return out
}

// TestVersioning_FollowsUserVolume 验证版本桶随 user 文件所在卷（AD-5）：disk2 文件的版本保存/
// 列表/删除全在 disk2 的 version 桶；默认卷（main）无该文件的版本目录。
func TestVersioning_FollowsUserVolume(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 5}, // 容量小 → 首个上传即换 disk2
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, dirs := versionFeatureServer(t, "alice", volumes, func(c *Config) {
		c.Versioning.Enabled = true
		c.Versioning.MaxVersions = 10
	})

	body1 := []byte("version-one-content") // 20B > main 容量 5 → 落 disk2
	status, _, respBody := volumeUpload(t, url, "f.txt", body1, "")
	if status != http.StatusOK {
		t.Fatalf("首次上传应 200, got %d %s", status, respBody)
	}
	if !diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("f.txt 应落 disk2（main 容量不足）")
	}

	// 覆盖写 → saveVersionBeforeOverwrite 在 disk2 version/ 保存旧版（stay-home）。
	body2 := []byte("version-two-content-longer")
	status, _, respBody = volumeUpload(t, url, "f.txt", body2, "")
	if status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, respBody)
	}
	if !diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("覆盖写 stay-home：f.txt 应仍落 disk2")
	}

	// disk2 version 桶应有一个版本文件（= body1 内容）；main 无该版本目录。
	verDirDisk2 := filepath.Join(dirs[1], "alice", "version", "f.txt")
	entries, err := os.ReadDir(verDirDisk2)
	if err != nil {
		t.Fatalf("disk2 version 目录应存在: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("disk2 version 应恰好 1 个版本, got %d", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(verDirDisk2, entries[0].Name()))
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if !bytes.Equal(got, body1) {
		t.Fatalf("版本内容 = %q, want %q（覆盖前旧内容）", got, body1)
	}
	if _, statErr := os.Stat(filepath.Join(dirs[0], "alice", "version", "f.txt")); statErr == nil {
		t.Fatal("main 卷不应有该文件版本目录（版本随 user 文件卷）")
	}

	// listVersions 先定位 home（disk2）→ 应看到 1 个版本（修复前只读默认卷 → 空）。
	listed := listVersionsJSON(t, url, "f.txt")
	if len(listed.Versions) != 1 {
		t.Fatalf("listVersions 应看到 disk2 版本 1 个, got %d（修复前只读默认卷=0）", len(listed.Versions))
	}
	if listed.Versions[0].Size != int64(len(body1)) {
		t.Fatalf("版本 size=%d want %d", listed.Versions[0].Size, len(body1))
	}

	// restoreVersion 同卷把版本拷回 disk2 user 桶。
	verID := listed.Versions[0].VersionID
	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=f.txt&version_id=%d", url, verID)
	resp, err := http.Post(restoreURL, "application/json", nil)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restoreBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore 应 200, got %d %s", resp.StatusCode, restoreBody)
	}
	if !diskFileExists(t, dirs[1], "alice", "f.txt") {
		t.Fatal("restore 后 f.txt 应仍在 disk2 user 桶")
	}
	restored, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "f.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if !bytes.Equal(restored, body1) {
		t.Fatalf("restore 内容 = %q, want %q（版本 body1 恢复覆盖 body2）", restored, body1)
	}

	// deleteVersionHandler 同卷删除（disk2）：restore 前的 saveVersion 又多存 1 个版本
	// （body2 的备份）→ 此时 disk2 version 应有 2 个；删 1 个后剩 1（默认卷 main 无版本目录）。
	listed2 := listVersionsJSON(t, url, "f.txt")
	if len(listed2.Versions) != 2 {
		t.Fatalf("restore 后 disk2 version 应 2 个（旧版+恢复前备份）, got %d", len(listed2.Versions))
	}
	delVerID := listed2.Versions[0].VersionID
	delReq, err := http.NewRequest("DELETE",
		fmt.Sprintf("%s/api/versions?filename=f.txt&version_id=%d", url, delVerID), nil)
	if err != nil {
		t.Fatalf("new delete-version req: %v", err)
	}
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete-version: %v", err)
	}
	delBody, _ := io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete-version 应 200, got %d %s", delResp.StatusCode, delBody)
	}
	entries2, err := os.ReadDir(verDirDisk2)
	if err != nil {
		t.Fatalf("disk2 version 目录应仍存在: %v", err)
	}
	if len(entries2) != 1 {
		t.Fatalf("删 1 个版本后 disk2 version 应剩 1, got %d", len(entries2))
	}
	_ = h
}

// chunkedVolumeMux 绑定分块上传路由到固定 actor（volSet 生效），供多卷 init 定卷测试。
func chunkedVolumeMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/init", wrap(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", wrap(h.uploadChunk))
	mux.HandleFunc("POST /upload/complete", wrap(h.uploadComplete))
	mux.HandleFunc("GET /upload/status", wrap(h.uploadStatus))
	return mux
}

// chunkedVolumeServer 装配多卷 chunked 测试服务，返回 URL 与各卷根。
func chunkedVolumeServer(t *testing.T, actor string, volumes []VolumeConfig) (string, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	cfg.ChunkSize = 4 << 10
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(chunkedVolumeMux(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, dirs
}

// chunkedPost 发送 JSON POST 到 path，返回状态与响应体。
func chunkedPost(t *testing.T, baseURL, path string, payload any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(payload)
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// chunkedUpload 以 init → chunk(s) → complete 完成一次分块上传（单分块足够测试）。
func chunkedUpload(t *testing.T, baseURL, filename string, fileData []byte) {
	t.Helper()
	fileChecksum := sha256hex(fileData)
	status, body := chunkedPost(t, baseURL, "/upload/init", map[string]any{
		"upload_id":     "cid-" + filename,
		"filename":      filename,
		"total_size":    len(fileData),
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": fileChecksum,
	})
	if status != http.StatusOK {
		t.Fatalf("init 应 200, got %d %s", status, body)
	}
	var initResp files.ChunkedInitResponse
	if err := json.Unmarshal(body, &initResp); err != nil {
		t.Fatalf("init decode: %v", err)
	}
	if initResp.UploadID == "" || initResp.UploadID == "already_exists" {
		t.Fatalf("init 应返回新会话 upload_id, got %q body=%s", initResp.UploadID, body)
	}

	// 上传唯一分块。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("upload_id", initResp.UploadID)
	_ = mw.WriteField("chunk_index", "0")
	_ = mw.WriteField("chunk_checksum", fileChecksum)
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	_, _ = part.Write(fileData)
	_ = mw.Close()
	resp, err := http.Post(baseURL+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	chunkBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk 应 200, got %d %s", resp.StatusCode, chunkBody)
	}

	status, body = chunkedPost(t, baseURL, "/upload/complete", map[string]any{"upload_id": initResp.UploadID})
	if status != http.StatusOK {
		t.Fatalf("complete 应 200, got %d %s", status, body)
	}
}

// TestChunkedUpload_InitPinsVolume 验证分块上传 init 定卷（AD-5）：默认卷容量不足时 init 路由
// 到 disk2，chunk 直写 + complete rename 全在 disk2（非默认卷也成立），最终 user 文件落 disk2。
func TestChunkedUpload_InitPinsVolume(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 8}, // 容量小 → 新文件 init 换 disk2
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, dirs := chunkedVolumeServer(t, "alice", volumes)

	fileData := []byte("chunked file that should land on disk2 because main is full")
	chunkedUpload(t, url, "big.bin", fileData)

	// 最终 user 文件必须在 disk2（init 定卷）；main 无此文件。
	if !diskFileExists(t, dirs[1], "alice", "big.bin") {
		t.Fatal("big.bin 应落 disk2（init 定卷，main 容量不足换卷）")
	}
	if diskFileExists(t, dirs[0], "alice", "big.bin") {
		t.Fatal("big.bin 不应落 main")
	}
	// 内容完整。
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "big.bin"))
	if err != nil {
		t.Fatalf("read disk2 file: %v", err)
	}
	if !bytes.Equal(got, fileData) {
		t.Fatalf("disk2 文件内容不完整: got %d bytes want %d", len(got), len(fileData))
	}
	// 在途临时文件应已清理（complete 后 CleanupSessionAfter 删除；此处轮询等待异步清理）。
	// 直接断言最终文件存在即完成验证目标；临时清理由既有会话清理测试覆盖。
}

// TestChunkedUpload_RecoverDisk2SessionResumes 回归 T6a 修复轮发现-1：非默认卷（disk2）在途
// 分块会话重启恢复时，UploadStore 的卷根注册必须先于 recoverSessions——否则 recover 把 temp
// 解析到默认卷 → 打开失败清空 bitmap → 断点续传退化为整文件重传。断言重启后已收分块 bitmap
// 保留（chunk 追加成功而非整文件重传），且续传补完分块可 complete 出完整文件。
func TestChunkedUpload_RecoverDisk2SessionResumes(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	mkCfg := func() *Config {
		cfg := Default()
		cfg.StorageRoot = dirs[0]
		cfg.Placement = "prefer-default"
		cfg.ChunkSize = 4 << 10
		cfg.Volumes = []VolumeConfig{
			{Name: "main", Root: dirs[0], VolCapacity: 8}, // 容量小 → 新文件 init 换 disk2
			{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("cfg.Validate: %v", err)
		}
		return cfg
	}

	// 第一生命周期：init + 上传 chunk 0 → disk2 在途会话（分 2 片，只收 1 片）。
	h1 := buildVolSetHandlers(t, mkCfg())
	ts := httptest.NewServer(chunkedVolumeMux(h1, "alice"))
	t.Cleanup(ts.Close)

	fileData := []byte("0123456789abcdefghij") // 20B > main 8 → disk2；2 片 x10B
	fileChecksum := sha256hex(fileData)
	const uploadID = "resume-disk2-session"
	status, body := chunkedPost(t, ts.URL, "/upload/init", map[string]any{
		"upload_id":     uploadID,
		"filename":      "resume.bin",
		"total_size":    len(fileData),
		"chunk_size":    10,
		"total_chunks":  2,
		"file_checksum": fileChecksum,
	})
	if status != http.StatusOK {
		t.Fatalf("init 应 200, got %d %s", status, body)
	}
	// 上传 chunk 0（前 10B）。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("upload_id", uploadID)
	_ = mw.WriteField("chunk_index", "0")
	_ = mw.WriteField("chunk_checksum", sha256hex(fileData[:10]))
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	_, _ = part.Write(fileData[:10])
	_ = mw.Close()
	resp, err := http.Post(ts.URL+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("chunk 0: %v", err)
	}
	chunkBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk 0 应 200, got %d %s", resp.StatusCode, chunkBody)
	}
	// 同步落盘 session.json（重启前确定性持久化 bitmap）。
	if perr := h1.uploadStoreFor("alice").PersistNow(uploadID); perr != nil {
		t.Fatalf("PersistNow: %v", perr)
	}

	// 重启：同一卷根新建 Handlers → 新 UploadStore → recoverSessions。
	h2 := buildVolSetHandlers(t, mkCfg())
	store2 := h2.uploadStoreFor("alice")
	s := store2.GetSession(uploadID)
	if s == nil {
		t.Fatalf("重启后应恢复会话 %s", uploadID)
	}
	if !s.ReceivedChunks[0] || s.ReceivedChunks[1] {
		t.Fatalf("重启恢复后 bitmap 应保留 chunk0 已收 / chunk1 待收: got %v（发现-1：temp 解析错卷清空 bitmap → 整文件重传）", s.ReceivedChunks)
	}

	// 续传补 chunk 1 → complete → disk2 完整文件（非整文件重传路径）。
	ts2 := httptest.NewServer(chunkedVolumeMux(h2, "alice"))
	t.Cleanup(ts2.Close)
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	_ = mw2.WriteField("upload_id", uploadID)
	_ = mw2.WriteField("chunk_index", "1")
	_ = mw2.WriteField("chunk_checksum", sha256hex(fileData[10:]))
	part2, _ := mw2.CreateFormFile("chunk", "00001.chunk")
	_, _ = part2.Write(fileData[10:])
	_ = mw2.Close()
	resp2, err := http.Post(ts2.URL+"/upload/chunk", mw2.FormDataContentType(), &buf2)
	if err != nil {
		t.Fatalf("chunk 1 (续传): %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("chunk 1 (续传) 应 200, got %d", resp2.StatusCode)
	}
	status, body = chunkedPost(t, ts2.URL, "/upload/complete", map[string]any{"upload_id": uploadID})
	if status != http.StatusOK {
		t.Fatalf("complete 应 200, got %d %s", status, body)
	}
	got, err := os.ReadFile(filepath.Join(dirs[1], "alice", "user", "resume.bin"))
	if err != nil {
		t.Fatalf("read disk2 resume.bin: %v", err)
	}
	if !bytes.Equal(got, fileData) {
		t.Fatalf("续传 complete 内容不完整: got %q want %q", got, fileData)
	}
}

// TestCloudArchive_DefaultVolumeBinding 验证 cloud/archive 写根绑定默认卷（AD-5 例外）：
// 分叉配置下 tenantFor 解析的租户根在默认卷根（dirB）而非 cfg.StorageRoot（dirA）——
// cloud/archive 产物经 tenantFor 落默认卷，与 meta 归属一致。
func TestCloudArchive_DefaultVolumeBinding(t *testing.T) {
	dirA, dirB, _, cfg := divergentVolumeConfig(t)
	h := buildVolSetHandlers(t, cfg)

	// tenantFor（cloudMgr/archive handler 共用的 owner 租户解析器）根 = 默认卷 dirB。
	tnt := h.tenantFor("alice")
	if tnt == nil {
		t.Fatal("tenantFor(alice) nil")
	}
	abs, ok := tnt.Root().Abs("")
	if !ok {
		t.Fatal("tenant root abs fail")
	}
	if filepath.Clean(abs) != filepath.Clean(dirB+"/alice") {
		t.Fatalf("tenantFor 根 = %q, want 默认卷根 %q（cloud/archive 产物经其落默认卷）", abs, filepath.Join(dirB, "alice"))
	}
	// cfg.StorageRoot（dirA）不承载任何租户（cloud/archive 不落旧单根）。
	if _, err := os.Stat(filepath.Join(dirA, "alice")); err == nil {
		t.Fatalf("cfg.StorageRoot 不应有 alice 租户")
	}
	// cloud/archive 桶派生路径确在默认卷根下。
	cloudAbs, _ := tnt.Root().Abs("cloud")
	archAbs, _ := tnt.Root().Abs("archive")
	if filepath.Dir(cloudAbs) != filepath.Join(dirB, "alice") || filepath.Dir(archAbs) != filepath.Join(dirB, "alice") {
		t.Fatalf("cloud/archive 桶应在默认卷租户下: cloud=%s archive=%s", cloudAbs, archAbs)
	}
}
