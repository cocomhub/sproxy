// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/qjfoidnh/BaiduPCS-Go/baidupcs/pcserror"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/rio"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/transfer"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/uploader"
)

// fakeMultiUpload 是 uploader.MultiUpload 的内存实现（map 模拟百度分片存储），
// 驱动真实 NewMultiUploader 跑通分片/断点/合并逻辑（无需真实百度 API）。
type fakeMultiUpload struct {
	mu          sync.Mutex
	blocks      map[int]string // partseq → 已上传分片内容
	checkSums   map[int]string // 最终合并的 checksum map
	precreated  int            // Precreate 调用次数
	superCalls  int            // CreateSuperFile 调用次数
	fileSize    int64          // CreateSuperFile 收到的文件大小
	failSeqOnce map[int]bool   // 需要失败一次的分片序号（模拟瞬时错误）
}

func newFakeMultiUpload() *fakeMultiUpload {
	return &fakeMultiUpload{
		blocks:      make(map[int]string),
		failSeqOnce: make(map[int]bool),
	}
}

// failOnce 让指定分片序号在下次 TmpFile 时失败一次（随后恢复）。
func (f *fakeMultiUpload) failOnce(partSeq int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSeqOnce[partSeq] = true
}

func (f *fakeMultiUpload) Precreate() (string, pcserror.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.precreated++
	return "pcs-host-1", nil
}

func (f *fakeMultiUpload) TmpFile(_ context.Context, _ string, _ string, partSeq int, _ int64, r rio.ReaderLen64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSeqOnce[partSeq] {
		delete(f.failSeqOnce, partSeq)
		return "", errors.New("simulated transient failure")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	checksum := fmt.Sprintf("md5-%d-%d", partSeq, len(data))
	f.blocks[partSeq] = string(data)
	return checksum, nil
}

func (f *fakeMultiUpload) CreateSuperFile(_ string, _ string, _ string, fileSize int64, checksumMap map[int]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.superCalls++
	f.fileSize = fileSize
	f.checkSums = make(map[int]string, len(checksumMap))
	maps.Copy(f.checkSums, checksumMap)
	return nil
}

// uploadID 别名（避免与包内其它类型混淆）。

// runUploader 用 fakeMultiUpload 驱动一次 MultiUploader 执行。
func runUploader(t *testing.T, mu *fakeMultiUpload, filePath string, blockSize int64, resume *uploader.InstanceState) (int, int, error) {
	t.Helper()
	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("open %s: %v", filePath, err)
	}
	defer f.Close()
	muer := uploader.NewMultiUploader(mu, rio.NewFileReaderAtLen64(f), &uploader.MultiUploaderConfig{
		Parallel:  2,
		BlockSize: blockSize,
		MaxRate:   0,
		Policy:    "overwrite",
	}, "/baidu/f.txt")
	// 上游 upload() 内部总是访问 instanceState.Uploadid（TmpFile 参数），
	// 因此 Execute 前必须 SetInstanceState（空 BlockList 也算有效断点）。
	if resume == nil {
		resume = &uploader.InstanceState{}
	}
	muer.SetInstanceState(resume)
	muer.Execute()
	return mu.precreated, mu.superCalls, nil
}

// TestMultiUploader_Upload_SmallFile 小文件（单分片）走 Precreate→TmpFile→CreateSuperFile。
func TestMultiUploader_Upload_SmallFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(filePath, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	mu := newFakeMultiUpload()
	_, super, _ := runUploader(t, mu, filePath, 4*1024, nil)
	if mu.precreated != 1 {
		t.Fatalf("Precreate 调用 %d 次, want 1", mu.precreated)
	}
	if super != 1 {
		t.Fatalf("CreateSuperFile 调用 %d 次, want 1", super)
	}
	if mu.fileSize != 1024 {
		t.Fatalf("CreateSuperFile fileSize = %d, want 1024", mu.fileSize)
	}
	if len(mu.blocks) != 1 {
		t.Fatalf("上传分片数 = %d, want 1", len(mu.blocks))
	}
}

// TestMultiUploader_Upload_Chunked 大文件（多分片）TmpFile 被调用多次。
func TestMultiUploader_Upload_Chunked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "chunked.bin")
	if err := os.WriteFile(filePath, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	mu := newFakeMultiUpload()
	_, _, _ = runUploader(t, mu, filePath, 256*1024, nil)
	want := 4 // 1MB / 256KB
	if len(mu.blocks) != want {
		t.Fatalf("上传分片数 = %d, want %d", len(mu.blocks), want)
	}
}

// TestMultiUploader_Upload_TransientRetry 分片瞬时失败后 MultiUploader 自动重试（同分片最终成功）。
func TestMultiUploader_Upload_TransientRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "retry.bin")
	if err := os.WriteFile(filePath, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	mu := newFakeMultiUpload()
	mu.failOnce(1) // 分片 1 失败一次
	if _, _, err := runUploader(t, mu, filePath, 256*1024, nil); err != nil {
		t.Fatalf("上传应成功（瞬时失败自动重试）: %v", err)
	}
	if mu.superCalls != 1 {
		t.Fatalf("CreateSuperFile 调用 %d 次, want 1（重试后应合并）", mu.superCalls)
	}
	if len(mu.blocks) != 4 {
		t.Fatalf("上传分片数 = %d, want 4（全部完成）", len(mu.blocks))
	}
}

// TestMultiUploader_Resume_SkipCompleted 断点恢复：已完成分片不重传。
func TestMultiUploader_Resume_SkipCompleted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "resume.bin")
	if err := os.WriteFile(filePath, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// 手动构造断点：4 分片，前 2 个已有 checksum（模拟中断时已完成）。
	resume := &uploader.InstanceState{
		Uploadid: "u-resume",
		BlockList: []*uploader.BlockState{
			{ID: 0, Range: transfer.Range{Begin: 0, End: 256 * 1024}, CheckSum: "pre-md5-0"},
			{ID: 1, Range: transfer.Range{Begin: 256 * 1024, End: 512 * 1024}, CheckSum: "pre-md5-1"},
			{ID: 2, Range: transfer.Range{Begin: 512 * 1024, End: 768 * 1024}, CheckSum: ""},
			{ID: 3, Range: transfer.Range{Begin: 768 * 1024, End: 1 << 20}, CheckSum: ""},
		},
	}
	mu := newFakeMultiUpload()
	_, _, _ = runUploader(t, mu, filePath, 256*1024, resume)
	// 只应新上传 2 个分片（ID 2、3）。
	if len(mu.blocks) != 2 {
		t.Fatalf("断点恢复后新上传分片数 = %d, want 2（已完成的 2 个不应重传）", len(mu.blocks))
	}
	if _, ok := mu.blocks[0]; ok {
		t.Fatal("已完成分片 0 不应重传")
	}
	if _, ok := mu.blocks[1]; ok {
		t.Fatal("已完成分片 1 不应重传")
	}
	// 合并时应包含全部 4 个 checksum（2 个来自断点 + 2 个新上传）。
	if len(mu.checkSums) != 4 {
		t.Fatalf("CreateSuperFile checksumMap = %d 项, want 4（含断点恢复的）", len(mu.checkSums))
	}
}
