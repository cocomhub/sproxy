// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// salt_coalesce_test.go —— salt 段与目标 blocklet 段**必须合并成一次 Range 请求**
// （2026-10-21 缺陷修复，承接 segment_coalesce_test.go 的「段间合并」）。
//
// 块 blob 布局（shardseal）：`[R 128B][8B 密文流长][salt 32B]` + `[boot 段][数据段...]`，
// 其中 boot 段密文固定 bootEncSize=40B，boot 后各段间仅 0-63B 随机间隙。
// ⇒ salt 末尾到首段起点间隔 = 40..103B（blListOff+bootEncSize - (blSaltOff+SaltLen)）。
// 旧实现「1 次 salt 请求 + 1 次合并段请求」= 每块 2 次往返；合并后 = 每块 1 次。
//
// 本文件锁定：`readChunkRangeBySegments` 的一次请求必须**同时**覆盖 salt 与全部目标段，
// 底层 Range 请求总数 = 1（而非 2）。

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// saltSpanFS 记录底层每次 OpenRangeRead 区间（salt 合并断言用）。
type saltSpanFS struct {
	*syncpkg.LocalFS
	spans []rangeSpan
}

type rangeSpan struct{ offset, size int64 }

func (f *saltSpanFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	f.spans = append(f.spans, rangeSpan{offset, size})
	return f.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

func newSaltEnv(t *testing.T, contentSize int) (*SecretdataFS, *saltSpanFS, []byte) {
	t.Helper()
	probe := &saltSpanFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 512, Max: 512,
			BlockletMode: "fixed", BlockletMin: 64, BlockletMax: 64,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(contentSize)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "movie.mp4",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, probe, content
}

// TestReadChunkRangeBySegments_SaltAndSegmentsInOneRequest：整块读（salt + 全部 8 段）
// 只发 **1 次**底层 Range 请求（salt+段合并），而非 2 次（salt 一次 + 段一次）。
func TestReadChunkRangeBySegments_SaltAndSegmentsInOneRequest(t *testing.T) {
	t.Parallel()
	fs, probe, content := newSaltEnv(t, 512)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 0, 512)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("内容不一致：len=%d", len(got))
	}
	if len(probe.spans) != 2 {
		t.Fatalf("底层 Range 请求次数=%d，应为 2（salt 32B 1 次 + 数据跨度 1 次）", len(probe.spans))
	}
}

// TestReadChunkRangeBySegments_SaltCoalescedPartialRange：部分区间同样一次请求，
// 内容精确（不多不少）。
func TestReadChunkRangeBySegments_SaltCoalescedPartialRange(t *testing.T) {
	t.Parallel()
	fs, probe, content := newSaltEnv(t, 512)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 100, 160)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content[100:260]) {
		t.Fatalf("部分区间内容错误：len=%d", len(got))
	}
	if len(probe.spans) != 2 {
		t.Fatalf("部分区间底层 Range 请求次数=%d，应为 2（salt 32B + 数据跨度）", len(probe.spans))
	}
}
