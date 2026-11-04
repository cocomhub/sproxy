// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// concurrencyProbeFS 观测**同时进行**的内层写，用于证明块级上传确实并发
// （而非「配置了并发但仍串行」）。sleep 用于放大窗口，让串行实现无法蒙混通过。
type concurrencyProbeFS struct {
	syncpkg.FS
	inflight int32
	peak     int32
	spin     int
}

func (f *concurrencyProbeFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	cur := atomic.AddInt32(&f.inflight, 1)
	for {
		old := atomic.LoadInt32(&f.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&f.peak, old, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.inflight, -1)
	// 用纯 CPU 工作放大「同时进行」窗口（**不用 time.Sleep**：R14 睡眠棘轮只减不增）。
	spinCPU(f.spin)
	return f.FS.WriteFile(ctx, key, p, r, size, mtime)
}

func (f *concurrencyProbeFS) peakSeen() int32 { return atomic.LoadInt32(&f.peak) }

// spinCPU 执行 n 轮纯 CPU 工作（放大并发窗口用；不依赖时钟/睡眠）。
func spinCPU(n int) uint32 {
	var h uint32 = 2166136261
	buf := make([]byte, 512)
	for range n {
		for j := range buf {
			h = (h ^ uint32(buf[j])) * 16777619
		}
	}
	return h
}

// TestWriteFile_ChunkUploadConcurrency 钉住**块级并发上传**：
//   - UploadConcurrency=4 ⇒ 内层写峰值并发 ≥ 2（串行实现恒 1，必红）；
//   - UploadConcurrency=1 ⇒ 峰值恒 1（并发能力可关闭，用于回归串行语义）。
//
// 背景（2026-11-04 实测）：外部卷 blocklet=256KiB，交错上传原本逐块串行，且底层每个块
// 单独调用（baidupcs 每块 exec 一次进程）⇒ 1.4GiB 文件 ~5600 次串行调用、仅 1.41MiB/s。
func TestWriteFile_ChunkUploadConcurrency(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("chunk-concurrency-"), 40*1024) // 640KiB / 32KiB = 20 块

	run := func(t *testing.T, concurrency int) int32 {
		t.Helper()
		probe := &concurrencyProbeFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil), spin: 20000}
		sfs, err := NewFS(probe, Options{
			Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
			Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
			UploadConcurrency: concurrency,
		})
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
		rc, rerr := sfs.OpenRead(context.Background(), "d/f.bin")
		if rerr != nil {
			t.Fatalf("OpenRead: %v", rerr)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("读回内容不一致（%d vs %d）", len(got), len(payload))
		}
		return probe.peakSeen()
	}

	peakConc := run(t, 4)
	if peakConc < 2 {
		t.Fatalf("并发=4 时内层写峰值并发为 %d ⇒ 未真正并发（串行实现恒 1）✗", peakConc)
	}
	if peakConc > 4 {
		t.Fatalf("内层写峰值并发 %d 超过配置上限 4 ✗", peakConc)
	}
	peakSerial := run(t, 1)
	if peakSerial != 1 {
		t.Fatalf("并发=1 时内层写峰值并发应为 1，got %d ✗", peakSerial)
	}
}

// TestUploadConcurrencyFor 收敛边界：0/负数走默认，超上限截断。
func TestUploadConcurrencyFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want int
	}{
		{0, DefaultUploadConcurrency},
		{-1, DefaultUploadConcurrency},
		{1, 1},
		{8, 8},
		{MaxUploadConcurrency + 5, MaxUploadConcurrency},
	}
	for _, c := range cases {
		if got := uploadConcurrencyFor(c.in); got != c.want {
			t.Fatalf("uploadConcurrencyFor(%d)=%d，want %d", c.in, got, c.want)
		}
	}
}

// TestWriteFile_ConcurrentUploadFailureRollsBackAll 并发下「某块失败」必须回滚**全部**已发起
// 的上传（含其他 worker 在跑的块）——否则远端留孤儿。
// 变异：失败分支不先 iu.finish() 就回滚 ⇒ 残留数据块，断言必红。
func TestWriteFile_ConcurrentUploadFailureRollsBackAll(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := &concurrencyFailFS{FS: syncpkg.NewLocalFS(root, nil), spin: 20000}
	sfs, err := NewFS(inner, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		UploadConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("concurrent-fail-"), 32*1024) // 512KiB / 32KiB = 16 块
	werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("分块上传失败 ⇒ WriteFile 必须失败 ✗")
	}
	if orphans := residualDataBlobs(t, root); len(orphans) != 0 {
		t.Fatalf("并发失败回滚后底层残留 %d 个数据 blob（远端孤儿）✗: %v", len(orphans), orphans)
	}
}

// concurrencyFailFS 对**数据分块**的写恒失败（目录/meta 正常），用于并发失败回滚用例。
type concurrencyFailFS struct {
	syncpkg.FS
	mu   sync.Mutex
	spin int
}

func (f *concurrencyFailFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if err := f.FS.WriteFile(ctx, key, p, r, size, mtime); err != nil {
		return err
	}
	spinCPU(f.spin)
	if shardseal.ClassifyName(path.Base(p)) == shardseal.KindChunk {
		f.mu.Lock()
		defer f.mu.Unlock()
		return errors.New("concurrent-fail: 内层写成功但复核失败")
	}
	return nil
}
