// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_store_lifecycle_test.go 补齐分块会话存储（`UploadStore`）此前无包内覆盖的
// 生命周期能力：健康探活、会话目录、多卷租户根注册、storageMgr 回退预留的释放、恢复期
// 临时文件分片复核、全分片 mismatch 兜底。均为真实 `UploadStore`（临时目录）驱动，不依赖
// pkg/server 装配层——这些方法此前只由装配层（预建 store / Close / 探活）或恢复路径间接调用，
// 包内零覆盖。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCapacity 是 StorageManager 接缝的替身：只记录 ReleaseChunked 的累计字节与次数。
type fakeCapacity struct {
	released int64
	calls    int
}

func (f *fakeCapacity) TryReserveChunked(int64) error { return nil }
func (f *fakeCapacity) ReleaseChunked(bytes int64)    { f.released += bytes; f.calls++ }
func (f *fakeCapacity) Usage() int64                  { return 0 }
func (f *fakeCapacity) MaxBytes() int64               { return 0 }

// TestUploadStore_SessionDirAndHealth 覆盖会话目录推导与健康探活：
// 停止前 Health 为 nil、SessionDir 指向并已创建 <baseDir>/<upload_id>，Stop 后 Health 报错。
func TestUploadStore_SessionDirAndHealth(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	chunkDir := filepath.Join(t.TempDir(), "chunk")
	us := MustNewUploadStore(chunkDir, time.Hour, nil)

	if err := us.Health(); err != nil {
		t.Fatalf("停止前 Health 应为 nil, got %v", err)
	}
	if _, err := us.CreateSession("sid", "f.txt", 8, 4, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	want := filepath.Join(chunkDir, "sid")
	if got := us.SessionDir("sid"); got != want {
		t.Fatalf("SessionDir=%q want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("会话目录应已创建: err=%v", err)
	}

	us.Stop()
	if err := us.Health(); err == nil {
		t.Fatal("Stop 后 Health 应返回错误（探活要能感知 store 已停止）")
	}
}

// TestUploadStore_SetStorageMgr_ReleasesFallbackReservation 覆盖 P5 回退预留的释放：
// 注入 storageMgr 后，删除会话与过期清理两条路径都按 StorageMgrReserved 调用 ReleaseChunked。
func TestUploadStore_SetStorageMgr_ReleasesFallbackReservation(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	cap := &fakeCapacity{}

	// 路径 1：DeleteSession。
	us := MustNewUploadStore(filepath.Join(t.TempDir(), "chunk"), time.Hour, nil)
	defer us.Stop()
	us.SetStorageMgr(cap)
	s, err := us.CreateSession("del-sid", "f.txt", 100, 50, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s.StorageMgrReserved = 100
	us.DeleteSession("del-sid")
	if cap.released != 100 || cap.calls != 1 {
		t.Fatalf("DeleteSession 应释放 100 字节, got released=%d calls=%d", cap.released, cap.calls)
	}

	// 路径 2：CleanupExpired（负 TTL → 创建即过期）。
	us2 := MustNewUploadStore(filepath.Join(t.TempDir(), "chunk"), -time.Nanosecond, nil)
	defer us2.Stop()
	us2.SetStorageMgr(cap)
	s2, err := us2.CreateSession("exp-sid", "f.txt", 50, 25, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s2.StorageMgrReserved = 50
	us2.CleanupExpired()
	if cap.released != 150 || cap.calls != 2 {
		t.Fatalf("CleanupExpired 应累计释放 150 字节, got released=%d calls=%d", cap.released, cap.calls)
	}
}

// TestUploadStore_SetVolumeTenantRoot_ResolvesTempOnTargetVolume 覆盖多卷（AD-5）在途
// 临时文件的目标卷解析：注册卷租户根后，删除会话应在**目标卷**上删除 temp 文件；空参数为
// 幂等空操作（不注册、不 panic）。
func TestUploadStore_SetVolumeTenantRoot_ResolvesTempOnTargetVolume(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	chunkDir := filepath.Join(base, "main", "alice", "chunk")
	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	vol2Root := filepath.Join(base, "disk2", "alice")
	us.SetVolumeTenantRoot("", vol2Root) // 空卷名：空操作
	us.SetVolumeTenantRoot("disk2", "")  // 空路径：空操作
	us.SetVolumeTenantRoot("disk2", vol2Root)

	tempAbs := filepath.Join(vol2Root, "user", "f.txt")
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tempAbs, []byte("inflight"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s, err := us.CreateSession("vol2-sid", "f.txt", 8, 4, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	s.Volume = "disk2"
	s.TempPath = "user/f.txt"

	us.DeleteSession("vol2-sid")
	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("应删除目标卷上的在途临时文件, stat err=%v", err)
	}
}

// TestUploadStore_VerifyTempChunks_DropsMismatchAndMissingChecksum 覆盖恢复期分片复核：
// 匹配的保留、内容不匹配的清除、无 checksum 记录的清除。
func TestUploadStore_VerifyTempChunks_DropsMismatchAndMissingChecksum(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	us := MustNewUploadStore(filepath.Join(base, "main", "alice", "chunk"), time.Hour, nil)
	defer us.Stop()

	chunk0 := []byte("AAAA")
	chunk1 := []byte("BBBB")
	chunk2 := []byte("CCCC")
	full := append(append(append([]byte{}, chunk0...), chunk1...), chunk2...)
	tempAbs := filepath.Join(base, "main", "alice", "user", "f.txt")
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tempAbs, full, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s := newSession("sid", "f.txt", int64(len(full)), 4, 3, "", 0, time.Hour)
	s.TempPath = "user/f.txt"
	s.ChunkChecksums[0] = sha256Hex(chunk0)       // 匹配 → 保留
	s.ChunkChecksums[1] = sha256Hex([]byte("ZZ")) // 不匹配 → 清除
	s.ChunkChecksums[2] = ""                      // 无记录 → 清除
	s.ReceivedChunks[0], s.ReceivedChunks[1], s.ReceivedChunks[2] = true, true, true

	us.verifyTempChunks(s)
	if !s.ReceivedChunks[0] {
		t.Fatal("匹配分片应保留")
	}
	if s.ReceivedChunks[1] {
		t.Fatal("内容不匹配分片应清除")
	}
	if s.ReceivedChunks[2] {
		t.Fatal("无 checksum 记录的分片应清除")
	}
}

// TestUploadStore_VerifyTempChunks_ClearsAllWhenTempUnavailable 覆盖临时文件不可用
// （路径非法 / 文件不存在）时全部分片需重传的兜底语义。
func TestUploadStore_VerifyTempChunks_ClearsAllWhenTempUnavailable(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	us := MustNewUploadStore(filepath.Join(base, "main", "alice", "chunk"), time.Hour, nil)
	defer us.Stop()

	cases := []struct {
		name     string
		tempPath string
	}{
		{"非 user 桶路径非法", "chunk/f.txt"},
		{"临时文件不存在", "user/missing.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession("sid", "f.txt", 8, 4, 2, "", 0, time.Hour)
			s.TempPath = tc.tempPath
			s.ChunkChecksums[0] = sha256Hex([]byte("AAAA"))
			s.ChunkChecksums[1] = sha256Hex([]byte("BBBB"))
			s.ReceivedChunks[0], s.ReceivedChunks[1] = true, true

			us.verifyTempChunks(s)
			if s.ReceivedChunks[0] || s.ReceivedChunks[1] {
				t.Fatalf("临时文件不可用时应清空全部 bitmap, got %v", s.ReceivedChunks)
			}
		})
	}
}

// TestUploadStore_AllMismatchIndices 覆盖全分片 mismatch 兜底：直接返回 0..N-1 升序；
// 并在「临时文件缺失」的 findMismatchChunks 路径上验证同一兜底被实际使用。
func TestUploadStore_AllMismatchIndices(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	s := newSession("sid", "f.txt", 12, 4, 3, "", 0, time.Hour)
	got := allMismatchIndices(s)
	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("allMismatchIndices=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allMismatchIndices=%v want %v", got, want)
		}
	}

	base := t.TempDir()
	us := MustNewUploadStore(filepath.Join(base, "main", "alice", "chunk"), time.Hour, nil)
	defer us.Stop()
	s2 := newSession("sid2", "f.txt", 12, 4, 3, "", 0, time.Hour)
	s2.TempPath = "user/missing.txt" // 临时文件不存在 → 走 allMismatchIndices 兜底
	if idx := us.findMismatchChunks(s2); len(idx) != 3 {
		t.Fatalf("临时文件缺失应返回全部分片 mismatch, got %v", idx)
	}
}

// TestMustNewUploadStore_SuccessAndPanic 覆盖 MustNewUploadStore 的两条出口：
// 正常目录返回可用 store（Health 为 nil）；baseDir 不可创建（路径是普通文件）时 panic。
// 后者是装配层「无法优雅处理错误」时的 fail-fast 契约，必须有测试钉住。
func TestMustNewUploadStore_SuccessAndPanic(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	okDir := filepath.Join(t.TempDir(), "chunk")
	us := MustNewUploadStore(okDir, time.Hour, nil)
	if err := us.Health(); err != nil {
		t.Fatalf("正常创建后 Health 应为 nil, got %v", err)
	}
	us.Stop()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("baseDir 不可创建时应 panic（MustNewUploadStore 的 fail-fast 契约）")
		}
	}()
	MustNewUploadStore(filePath, time.Hour, nil)
}

// TestUploadStore_DeleteSession_KeepsCompletedSessionReservation 钉住审计 C-4：
// **已完成**会话的 P5（storageMgr 回退）预留不得在清理时释放——complete 成功后 temp 已 rename
// 为正式文件，字节仍在磁盘上，释放会让 capacity 的 totalUsage 少算 TotalSize（直到下一次
// 全量扫描，≤30 min），从而放宽 max_storage_bytes 门禁；未完成会话仍必须释放（由
// TestUploadStore_SetStorageMgr_ReleasesFallbackReservation 钉住另一侧）。
func TestUploadStore_DeleteSession_KeepsCompletedSessionReservation(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	cap := &fakeCapacity{}
	us := MustNewUploadStore(filepath.Join(t.TempDir(), "chunk"), time.Hour, nil)
	defer us.Stop()
	us.SetStorageMgr(cap)

	if _, err := us.CreateSession("done-sid", "f.txt", 100, 50, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 必须走锁内 setter 登记，**不得**直写 CreateSession 的返回值：该返回值当前是 store
	// 内部对象，但一旦它改为返回副本（与续传路径的 copySession 口径对齐），直写会静默失效 ⇒
	// store 内对象仍为 0 ⇒ DeleteSession 两个分支都不走 ⇒ 下面的 cap.calls != 0 变成**真空
	// 假绿**（断言「没调用」恰好被满足，与被修的 C-4 闸门无关）。下一行读回锁内对象，把
	// 「确实走了哪条分支」变成证据而非巧合。
	if !us.SetSessionStorageMgrReserved("done-sid", 100) {
		t.Fatal("SetSessionStorageMgrReserved 应返回 true（会话存在）")
	}
	if got := us.GetSession("done-sid"); got == nil || got.StorageMgrReserved != 100 {
		t.Fatalf("锁内 setter 必须写入 store 持有的会话对象, got %+v", got)
	}
	if err := us.CompleteSession("done-sid"); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}

	us.DeleteSession("done-sid")
	if cap.calls != 0 {
		t.Fatalf("已完成会话的 P5 预留不应被释放（字节已成正式文件）: released=%d calls=%d", cap.released, cap.calls)
	}
}
