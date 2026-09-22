// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"testing"
)

// fakeMultipartFile 实现 multipart.File（io.Reader+ReaderAt+Seeker+Closer）的最小桩。
type fakeMultipartFile struct {
	*bytes.Reader
	closed bool
}

func newFakeMultipartFile(data []byte) *fakeMultipartFile {
	return &fakeMultipartFile{Reader: bytes.NewReader(data)}
}

func (f *fakeMultipartFile) Close() error { f.closed = true; return nil }

// TestReadChunkBody_OwnershipReuse 钉住 readChunkBodyOwned 的所有权语义：
// ① 返回切片**直接指向池条目**（零拷贝），调用方在 release() 前持有所有权；
// ② release() 归还池条目，随后再次读取应命中同一底层数组（cap 一致 → 复用生效）。
//
// 语义安全前提：调用方（UploadChunk）在**同一 goroutine 顺序**完成 sha256 与直写后
// 才 release()，归还后无任何引用 —— 这是 #457 race 修复（先拷贝后归还）的正确延续：
// 不再每次 1MiB 拷贝，而是把「所有权」交给唯一使用方，用毕归还。
func TestReadChunkBody_OwnershipReuse(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte{0x55}, 256*1024) // 256 KiB
	f := newFakeMultipartFile(payload)

	data, release, err := readChunkBodyOwned(f)
	if err != nil {
		t.Fatalf("readChunkBodyOwned: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("内容不一致: len=%d want=%d", len(data), len(payload))
	}
	firstCap := cap(data)

	// 未 release 时：池条目应被本调用持有（尚未归还 —— 并发 Get 拿不到同数组）。
	// 这里仅断言「池内无刚 Put 的条目」（归还后 Get 才拿得到同 cap 数组）。
	if bp, _ := chunkBodyPool.Get().(*[]byte); bp != nil && cap(*bp) == firstCap {
		// 拿到同 cap 条目只可能是本调用刚 release（不应发生）。
		chunkBodyPool.Put(bp)
		t.Fatalf("release 前池内不应有本调用持有的条目（cap=%d）", firstCap)
	} else if bp != nil {
		chunkBodyPool.Put(bp) // 其它残留条目原样放回
	}

	// release 后：池条目归还，再次读取应复用（cap 相同 → 零新增大分配）。
	release()
	f2 := newFakeMultipartFile(payload)
	data2, release2, err := readChunkBodyOwned(f2)
	if err != nil {
		t.Fatalf("readChunkBodyOwned #2: %v", err)
	}
	if cap(data2) != firstCap {
		t.Fatalf("release 后复用应命中同数组: cap=%d want=%d", cap(data2), firstCap)
	}
	if !bytes.Equal(data2, payload) {
		t.Fatalf("第二次内容不一致")
	}
	release2()
}

// TestReadChunkBody_ChecksumMismatchStillReleases 钉住失败路径的归还：
// 校验不匹配时调用方应 release() 归还池条目（不泄漏），池可被后续复用。
func TestReadChunkBody_ChecksumMismatchStillReleases(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte{0x77}, 64*1024) // 64 KiB
	f := newFakeMultipartFile(payload)

	data, release, err := readChunkBodyOwned(f)
	if err != nil {
		t.Fatalf("readChunkBodyOwned: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("不应为空")
	}
	release() // 模拟 UploadChunk 校验失败路径的归还
}
