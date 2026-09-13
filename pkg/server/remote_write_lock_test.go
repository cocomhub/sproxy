// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_write_lock_test.go 钉住规格 §5.8 的并发决定：**远程写与本地写共享 B 侧同一文件级锁池**
// ⇒ 跨节点写与本地写天然互斥，B 侧无需任何分布式协调。
//
// 实现方式（确定性，非竞态赛跑）：白盒预置锁池条目（`h.uploadingFiles` 就是两条路径共用的那把
// 锁——`filesRuntime.TryMark/Acquire` → `tryMarkUploadingFile/acquireFileLock`），再验证
// ①远端写 ②本地 multipart 上传 ③远端删 全部 409；释放后远端写变 200（证明阻塞原因确为该锁，
// 而非别的失败路径）。

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newSharedLockFixture 返回 (本地 HTTP 面 URL, *Handlers)：同一个 `*Handlers` 同时承载
// 本地上传面与远程写面（这是「同一锁池」的前提）。
func newSharedLockFixture(t *testing.T) (string, *Handlers) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	// owner 用 testAccessKey（签名上传的 actor 即 AK），mesh 授权同 owner、scope=rw。
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testAccessKey},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testAccessKey, Scope: "rw",
		}},
	}}}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	opts := RegisterRoutesOpts{
		Mux:     http.NewServeMux(),
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "test",
		Logger:  testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, h
}

// TestRemoteWrite_SharesFileLockWithLocalWrites 钉住共享锁池。
func TestRemoteWrite_SharesFileLockWithLocalWrites(t *testing.T) {
	url, h := newSharedLockFixture(t)
	const rel = "user/locked.txt"
	lockKey := testAccessKey + "\x00" + rel

	// 预置锁：模拟「本地在上传 / move 正在移动该路径」的窗口。
	h.uploadingFiles.Store(lockKey, "test-holder")

	rh := h.newRemoteWriteHandler(fakePeerFingerprint{fp: testReaderFP})
	body := []byte("shared-lock-payload")
	sum := sha256hex(body)

	// ① 远端写：被同一把锁拒绝（域方法 WriteFile 的 TryMark）。
	rec := doRemoteWrite(t, rh, http.MethodPost, "/remote/write?volume=main&path=locked.txt", body, sum)
	if rec.Code != http.StatusConflict {
		t.Fatalf("远端写应被文件级锁拒绝（409），got %d: %s", rec.Code, rec.Body.String())
	}

	// ② 远端删：同样被锁拒绝（域方法 DeleteFile 的 Acquire）。
	rec = doRemoteWrite(t, rh, http.MethodPost, "/remote/delete?volume=main&path=locked.txt", nil, sum)
	if rec.Code != http.StatusConflict {
		t.Fatalf("远端删应被文件级锁拒绝（409），got %d: %s", rec.Code, rec.Body.String())
	}

	// ③ 本地 multipart 上传同一路径：**同一键空间** ⇒ 同样 409。
	if code := uploadFileSigned(t, url, "locked.txt", body); code != http.StatusConflict {
		t.Fatalf("本地写应被同一把锁拒绝（409），got %d", code)
	}

	// ④ 释放后远端写通过：证明前面的拒绝原因确实是该锁（而不是 checksum/路径等其它失败）。
	h.uploadingFiles.Delete(lockKey)
	rec = doRemoteWrite(t, rh, http.MethodPost, "/remote/write?volume=main&path=locked.txt", body, sum)
	if rec.Code != http.StatusOK {
		t.Fatalf("释放锁后远端写应 200，got %d: %s", rec.Code, rec.Body.String())
	}
}
