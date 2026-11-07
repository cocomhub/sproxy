// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// encrypted.go 是 at-rest 加密卷包装（roadmap P2 at-rest 加密）：
//
//   - EncryptedFS 实现 sync.FS 全接口，转发底层 FS 的目录/元数据操作，
//     对 WriteFile 流式 AES-256-GCM 分块加密、OpenRead 流式解密（透明）。
//   - 密文格式：[magic][chunkSize] + 逐块 [4B len][nonce|ct|tag]；**帧格式的唯一实现
//     在 pkg/cipherstream**（与归档 cipher、客户端 E2EE 共用，仅魔数不同做域分隔）。
//
// 纯标准库（crypto/aes + cipher + rand + hmac），不引第三方。

import (
	"context"
	"io"

	"github.com/cocomhub/sproxy/pkg/cipherstream"
)

// encMagic 是加密流魔数头。
var encMagic = []byte("SPROXY-FS-ENC-v1")

// EncryptedFS 是透明加密 sync.FS 包装。
type EncryptedFS struct {
	inner FS
	key   []byte
}

// NewEncryptedFS 构造加密卷包装（inner 为底层 FS；key 必须 32 字节）。
func NewEncryptedFS(inner FS, key []byte) *EncryptedFS {
	return &EncryptedFS{inner: inner, key: key}
}

// ListDir 转发。
func (e *EncryptedFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return e.inner.ListDir(ctx, path)
}

// Stat 转发（size 为密文大小——调用方按 opaque 处理）。
func (e *EncryptedFS) Stat(ctx context.Context, path string) (*Entry, error) {
	return e.inner.Stat(ctx, path)
}

// OpenRead 解密读（透明）。
func (e *EncryptedFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := e.inner.OpenRead(ctx, path)
	if err != nil {
		return nil, err
	}
	dr, derr := newEncReader(e.key, rc)
	if derr != nil {
		rc.Close()
		return nil, derr
	}
	return &encReadCloser{r: dr, c: rc}, nil
}

// WriteFile 加密写（透明，**流式**——审查 P2 修复：不再整块入内存）。
//
// 此前实现先把全部明文加密进内存 bytes.Buffer 再一次性写入——sync 同步场景 GB 级
// 文件直接 OOM。现改用 io.Pipe：加密 goroutine 流式写 pipe，底层 FS 从 pipe 流式读，
// 全程仅缓冲单块（64 KiB 明文 + 密文）。
//
// size 语义：入参 size 是**明文**大小（调用方/上层 diff 用它比对）；底层 FS 收到的是
// 密文大小（= pipe 实际字节，无明文 size 可用）。密文大小 = 明文 + 每块 ~28B 开销
// （nonce 12 + tag 16）。此差异由调用方按 opaque 处理（Stat 返回密文大小，diff 层
// 若按 size 比对需理解该卷是加密卷——见 NewEncryptedFS 文档）。
func (e *EncryptedFS) WriteFile(ctx context.Context, key LogicalKey, path string, r io.Reader, size, mtime int64) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		ew, werr := newEncWriter(e.key, pw)
		if werr != nil {
			errCh <- werr
			_ = pw.CloseWithError(werr)
			return
		}
		if _, cerr := io.Copy(ew, r); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		if cerr := ew.Close(); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		if cerr := pw.Close(); cerr != nil {
			errCh <- cerr
		}
		errCh <- nil
	}()
	err := e.inner.WriteFile(ctx, key, path, pr, size, mtime)
	// 等待加密 goroutine 完成（收集其错误；WriteFile 失败时 CloseWithError 已传播）。
	encErr := <-errCh
	if err == nil {
		err = encErr
	}
	_ = size // 入参 size 供上层 diff 语义（见函数注释）；底层写密文大小由 pipe 决定。
	return err
}

// Rename / Delete / MakeDir 转发。
func (e *EncryptedFS) Rename(ctx context.Context, from, to string) error {
	return e.inner.Rename(ctx, from, to)
}
func (e *EncryptedFS) Delete(ctx context.Context, path string) error {
	return e.inner.Delete(ctx, path)
}
func (e *EncryptedFS) MakeDir(ctx context.Context, path string) error {
	return e.inner.MakeDir(ctx, path)
}

// 编译期断言。
var _ FS = (*EncryptedFS)(nil)

// newEncWriter 构造分块加密 writer。
//
// 帧格式唯一实现在 pkg/cipherstream（与归档 cipher、客户端 E2EE 共用）；本包只固定
// **加密卷的魔数**（域分隔：同一份密文不会被其它域误读）。
func newEncWriter(key []byte, w io.Writer) (io.WriteCloser, error) {
	return cipherstream.NewWriter(key, w, cipherstream.Config{
		Magic: encMagic, ChunkSize: 64 << 10, Label: "encrypted",
	})
}

// newEncReader 构造分块解密 reader（兼容历史密文；块大小以头部声明为准）。
func newEncReader(key []byte, r io.Reader) (io.Reader, error) {
	return cipherstream.NewReader(key, r, cipherstream.Config{
		Magic: encMagic, ChunkSize: 64 << 10, Label: "encrypted",
	})
}

// encReadCloser 组合解密 reader + 底层 closer。
type encReadCloser struct {
	r io.Reader
	c io.Closer
}

func (e *encReadCloser) Read(p []byte) (int, error) { return e.r.Read(p) }
func (e *encReadCloser) Close() error               { return e.c.Close() }

// ListDirNames 转发轻量列举（未实现时回落 ListDir）：装配期只需名字时避免外部卷
// 逐条补精确 size 的高成本路径（见 NameLister）。
func (e *EncryptedFS) ListDirNames(ctx context.Context, path string) ([]Entry, error) {
	if nl, ok := AssertNameLister(e.inner); ok {
		return nl.ListDirNames(ctx, path)
	}
	return e.inner.ListDir(ctx, path)
}
