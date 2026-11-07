// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

// packed_size_test.go 钉住「打包转存必须声明**未知长度**(-1)，不得传 0」（独立复审 P1，2026-10-18）。
//
// 缺陷：`packedWrite` 恒传 size=0，而 0 的语义是「空对象」。s3 后端对此有明写契约
// （pkg/volume/ext/s3/s3_fs.go:373）：「size<0（未知长度）透传给 minio…**不得钳为 0**——那会
// 声明 0 字节却带非空 body（**数据丢失**）」。实测机制：minio api.go:1050
// `if metadata.contentLength == 0 { req.Body = nil }` ⇒ body 被丢弃、对象 0 字节；trusted 的
// Calculator 走 TeeReader 也收不到字节 ⇒ sidecar 记「空文件」⇒ 读回校验**自洽通过**（结构性
// 不可检出）；且 io.Pipe 读端从未被读 ⇒ pack goroutine 永久阻塞（HTTP 处理 goroutine 泄漏）。

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/compressx"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// sizeCapturingFS 记录底层收到的 size（写路径优先走 WriteIfAbsent，故两者都要拦）。
type sizeCapturingFS struct {
	*syncpkg.LocalFS
	got atomic.Int64
}

func (f *sizeCapturingFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, rel string, r io.Reader, size, mtime int64) error {
	f.got.Store(size)
	return f.LocalFS.WriteFile(ctx, key, rel, r, size, mtime)
}

func (f *sizeCapturingFS) WriteIfAbsent(ctx context.Context, rel string, r io.Reader, size, mtime int64) (bool, error) {
	f.got.Store(size)
	return f.LocalFS.WriteIfAbsent(ctx, rel, r, size, mtime)
}

func TestExecutePacked_DeclaresUnknownSizeNotZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := &sizeCapturingFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	_, err := Execute(ctx, TransferRequest{
		Entries:     []SourceEntry{{Rel: "a.txt", Size: 5, Reader: strings.NewReader("hello")}},
		Preprocess:  &TarPreprocessor{Compression: compressx.Gzip},
		Destination: fs,
		Scheme:      "sproxy",
		Volume:      "vol",
		Rel:         "packed.tar.gz",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	switch got := fs.got.Load(); got {
	case -1:
		// 期望：未知长度
	case 0:
		t.Fatal("打包转存必须声明「未知长度」(-1)，不得传 0" +
			"（未修必红：s3 后端把 0 当空对象 ⇒ body 被丢弃 ⇒ 静默丢数据 + pack goroutine 永久阻塞）")
	default:
		t.Fatalf("打包路径 size=%d, want -1（未知长度）", got)
	}
}
