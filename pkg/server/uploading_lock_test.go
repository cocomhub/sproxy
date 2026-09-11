// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// uploading_lock_test.go 覆盖 T6c「move 锁架构延伸」：delete / 版本 restore / 分块 complete
// 与单次上传 / 跨卷 move / 分块 init 共用同一把 <owner>\x00<rel> 文件级锁，move 持锁期间
// 三者 409；锁释放后各自正常恢复。

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// uploadingLockMux 绑定上传/删除/跨卷 move/版本 restore/分块 complete 到固定 actor，
// 供锁互斥测试使用（volSet 生效）。
func uploadingLockMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /upload/init", wrap(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", wrap(h.uploadChunk))
	mux.HandleFunc("POST /upload/complete", wrap(h.uploadComplete))
	mux.HandleFunc("POST /api/volumes/move", wrap(h.moveVolumeHandler))
	mux.HandleFunc("POST /api/versions/restore", wrap(h.restoreVersionHandler))
	mux.HandleFunc("GET /api/versions", wrap(h.listVersionsHandler))
	mux.HandleFunc("DELETE /api/versions", wrap(h.deleteVersionHandler))
	return mux
}

// uploadingLockServer 装配带完整锁相关路由的测试服务，返回 URL、Handlers 与各卷物理根。
func uploadingLockServer(t *testing.T, actor string, volumes []VolumeConfig, mod func(*Config)) (string, *Handlers, []string) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = volumes[0].Root
	cfg.Placement = "prefer-default"
	cfg.ChunkSize = 4 << 10
	if mod != nil {
		mod(cfg)
	}
	cfg.Volumes = volumes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	ts := httptest.NewServer(uploadingLockMux(h, actor))
	t.Cleanup(ts.Close)
	dirs := make([]string, len(volumes))
	for i := range volumes {
		dirs[i] = volumes[i].Root
	}
	return ts.URL, h, dirs
}

// singleVolumeLocks 返回单卷（default）配置，卷根为 root。
func singleVolumeLocks(root string) []VolumeConfig {
	return []VolumeConfig{{Name: "default", Root: root, VolCapacity: 1 << 20}}
}

// TestAcquireFileLock_MutualExclusion acquireFileLock 基本语义：同 owner 同 rel 互斥、
// 不同 owner 不冲突、释放后可重新获取、owner 归一（"" 与 "anonymous" 同键）。
func TestAcquireFileLock_MutualExclusion(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	h := buildVolSetHandlers(t, cfg)

	release, ok := h.acquireFileLock("", "user/a.txt")
	if !ok {
		t.Fatal("首次获取应成功")
	}
	// owner 归一：匿名（""）与显式 "anonymous" 必须落同一键。
	if _, ok2 := h.acquireFileLock("anonymous", "user/a.txt"); ok2 {
		t.Fatal("同 owner 同 rel 二次获取应冲突（owner 未归一？）")
	}
	// 不同 owner 不冲突。
	if _, ok3 := h.acquireFileLock("alice", "user/a.txt"); !ok3 {
		t.Fatal("不同 owner 不应冲突")
	}
	release()
	if _, ok4 := h.acquireFileLock("anonymous", "user/a.txt"); !ok4 {
		t.Fatal("释放后应可重新获取")
	}
}

// TestDelete_BlockedWhenFileLocked 文件被 move（或上传）持锁时，delete 必须 409 且不动文件；
// 锁释放后删除恢复正常。
func TestDelete_BlockedWhenFileLocked(t *testing.T) {
	root := t.TempDir()
	baseURL, h, dirs := uploadingLockServer(t, "alice", singleVolumeLocks(root), nil)

	content := []byte("delete-under-lock payload")
	if status, _, body := volumeUpload(t, baseURL, "lockdel.txt", content, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, body)
	}

	release, ok := h.acquireFileLock("alice", "user/lockdel.txt")
	if !ok {
		t.Fatal("预置锁失败")
	}

	status, body := deleteFile(t, baseURL, "lockdel.txt", content)
	if status != http.StatusConflict {
		t.Fatalf("持锁期间 delete 应 409, got %d body=%s", status, body)
	}
	if !diskFileExists(t, dirs[0], "alice", "lockdel.txt") {
		t.Fatal("409 的 delete 不应删除文件")
	}

	release()
	status, body = deleteFile(t, baseURL, "lockdel.txt", content)
	if status != http.StatusOK {
		t.Fatalf("释放锁后 delete 应 200, got %d body=%s", status, body)
	}
	if diskFileExists(t, dirs[0], "alice", "lockdel.txt") {
		t.Fatal("200 的 delete 应删除文件")
	}
}

// TestRestoreVersion_BlockedWhenFileLocked 版本 restore 与文件锁互斥：持锁 409（不写回
// user 文件），释放后恢复成功。
func TestRestoreVersion_BlockedWhenFileLocked(t *testing.T) {
	root := t.TempDir()
	baseURL, h, dirs := uploadingLockServer(t, "alice", singleVolumeLocks(root), func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 5
	})

	// 两次上传同名文件 → 第二次覆盖写触发版本保存（v1 = 第一次内容）。
	first := []byte("restore version one")
	second := []byte("restore version two (current)")
	if status, _, body := volumeUpload(t, baseURL, "restore.txt", first, ""); status != http.StatusOK {
		t.Fatalf("首次上传应 200, got %d %s", status, body)
	}
	if status, _, body := volumeUpload(t, baseURL, "restore.txt", second, ""); status != http.StatusOK {
		t.Fatalf("覆盖上传应 200, got %d %s", status, body)
	}

	// 从磁盘取版本 ID（version/<rel>/<id>）。
	verDir := filepath.Join(dirs[0], "alice", "version", "restore.txt")
	entries, err := os.ReadDir(verDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("版本目录应有 1 个版本: err=%v entries=%d", err, len(entries))
	}
	versionID := entries[0].Name()

	release, ok := h.acquireFileLock("alice", "user/restore.txt")
	if !ok {
		t.Fatal("预置锁失败")
	}

	restoreURL := baseURL + "/api/versions/restore?filename=restore.txt&version_id=" + versionID
	status, body := postNoBody(t, restoreURL)
	if status != http.StatusConflict {
		t.Fatalf("持锁期间 restore 应 409, got %d body=%s", status, string(body))
	}

	release()
	status, body = postNoBody(t, restoreURL)
	if status != http.StatusOK {
		t.Fatalf("释放锁后 restore 应 200, got %d body=%s", status, string(body))
	}
	// 恢复后 user 文件内容回到 v1。
	got, rerr := os.ReadFile(filepath.Join(dirs[0], "alice", "user", "restore.txt"))
	if rerr != nil {
		t.Fatalf("读取恢复后文件: %v", rerr)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("恢复内容 = %q, want %q", got, first)
	}
}

// TestUploadComplete_BlockedWhenFileLocked 分块 complete 与文件锁互斥：持锁 409（不 rename
// 最终文件、session 保留），释放后可正常完成。
func TestUploadComplete_BlockedWhenFileLocked(t *testing.T) {
	root := t.TempDir()
	baseURL, h, dirs := uploadingLockServer(t, "alice", singleVolumeLocks(root), nil)

	fileData := []byte("chunked-under-lock payload (non-zero)")
	fileChecksum := sha256hex(fileData)
	status, body := chunkedPost(t, baseURL, "/upload/init", map[string]any{
		"upload_id":     "lock-complete-1",
		"filename":      "lockc.txt",
		"total_size":    len(fileData),
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": fileChecksum,
	})
	if status != http.StatusOK {
		t.Fatalf("init 应 200, got %d %s", status, body)
	}
	var initResp ChunkedInitResponse
	if err := json.Unmarshal(body, &initResp); err != nil {
		t.Fatalf("init decode: %v", err)
	}

	// 先上传唯一分块（complete 的前置校验要求分块齐备），再进入锁互斥断言。
	uploadLockChunk(t, baseURL, initResp.UploadID, fileData, fileChecksum)

	release, ok := h.acquireFileLock("alice", "user/lockc.txt")
	if !ok {
		t.Fatal("预置锁失败")
	}

	status, body = chunkedPost(t, baseURL, "/upload/complete", map[string]any{"upload_id": initResp.UploadID})
	if status != http.StatusConflict {
		t.Fatalf("持锁期间 complete 应 409, got %d body=%s", status, body)
	}
	if diskFileExists(t, dirs[0], "alice", "lockc.txt") {
		t.Fatal("409 的 complete 不应落最终文件")
	}

	release()
	// 释放锁后 complete 应成功（证明释放锁后流程恢复且分块数据未受影响）。
	status, body = chunkedPost(t, baseURL, "/upload/complete", map[string]any{"upload_id": initResp.UploadID})
	if status != http.StatusOK {
		t.Fatalf("释放锁后 complete 应 200, got %d body=%s", status, body)
	}
	if !diskFileExists(t, dirs[0], "alice", "lockc.txt") {
		t.Fatal("complete 200 后文件应落盘")
	}
}

// TestMoveVsDelete_LockSerializes 真并发（seam 确定性阻塞）：move 持锁期间 delete 409，
// 释放后 move 完成；文件最终只存在于目标卷（无跨卷双份 / 无账本错配）。
func TestMoveVsDelete_LockSerializes(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	baseURL, _, _ := uploadingLockServer(t, "alice", volumes, nil)

	content := []byte("move-vs-delete payload")
	if status, _, body := volumeUpload(t, baseURL, "mvdel.txt", content, ""); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, body)
	}

	// seam：move 复制完成后、删源前阻塞，暴露「持锁窗口」供并发 delete 观测。
	reached := make(chan struct{})
	proceed := make(chan struct{})
	orig := removeMovedSource
	removeMovedSource = func(root *storage.Root, rel string) error {
		close(reached)
		<-proceed
		return orig(root, rel)
	}
	t.Cleanup(func() { removeMovedSource = orig })

	moveDone := make(chan int, 1)
	go func() {
		code, _, _ := moveVolumeCore(baseURL, "main", "disk2", "mvdel.txt")
		moveDone <- code
	}()

	select {
	case <-reached: // move 已持锁并在删源前阻塞
	case code := <-moveDone:
		t.Fatalf("move 未到达删源步骤即退出（code=%d）", code)
	case <-time.After(5 * time.Second):
		t.Fatal("move 未在 5s 内到达删源步骤（seam 未被调用）")
	}

	status, body := deleteFile(t, baseURL, "mvdel.txt", content)
	if status != http.StatusConflict {
		t.Fatalf("move 持锁期间 delete 应 409, got %d body=%s", status, body)
	}

	close(proceed)
	select {
	case code := <-moveDone:
		if code != http.StatusOK {
			t.Fatalf("move 应 200, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("move 未在 5s 内完成")
	}
	// 最终一致性：文件只在 disk2，main 无残留（无跨卷双份）。
	if diskFileExists(t, dirs[0], "alice", "mvdel.txt") {
		t.Fatal("move 后 main 不应残留 mvdel.txt")
	}
	if !diskFileExists(t, dirs[1], "alice", "mvdel.txt") {
		t.Fatal("move 后 mvdel.txt 应在 disk2")
	}
	// 释放锁后 delete 正常。
	status, body = deleteFile(t, baseURL, "mvdel.txt", content)
	if status != http.StatusOK {
		t.Fatalf("move 完成后 delete 应 200, got %d body=%s", status, body)
	}
}

// TestCleanupUploadingFilesPass_SkipsTxnLock txn 锁标记条目（delete/restore/complete）无对应
// session，过期清理必须跳过（否则长事务持锁被误删，锁失效）。
func TestCleanupUploadingFilesPass_SkipsTxnLock(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	h := buildVolSetHandlers(t, cfg)

	h.uploadingFiles.Store("alice\x00txn.txt", uploadingLockTxn) // txn 锁：无 session，保留
	h.uploadingFiles.Store("alice\x00ghost2.txt", "no-such-session")
	h.cleanupUploadingFilesPass()

	if _, ok := h.uploadingFiles.Load("alice\x00txn.txt"); !ok {
		t.Fatal("txn 锁条目不应对被清理")
	}
	if _, ok := h.uploadingFiles.Load("alice\x00ghost2.txt"); ok {
		t.Fatal("无对应 session 的分块上传条目应被清理")
	}
}

// uploadLockChunk 上传单个分块（multipart：upload_id/chunk_index/chunk_checksum + chunk 文件件）。
func uploadLockChunk(t *testing.T, baseURL, uploadID string, data []byte, checksum string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("upload_id", uploadID); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("chunk_index", "0"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("chunk_checksum", checksum); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("chunk", "00000.chunk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(baseURL+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chunk 应 200, got %d %s", resp.StatusCode, body)
	}
}

// postNoBody 发送无 body 的 POST，返回状态与响应体。
func postNoBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
