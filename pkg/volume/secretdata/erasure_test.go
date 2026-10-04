// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// 任务 9d 测试：XOR 奇偶纠错（Options.Erasure）+ 多 target 副本复制（NewFSMultiplicas）。
// 断言铁律：正例落到真实副作用（底层 blob 缺失→副本/parity 恢复→读取/解密还原 == 原内容），
// 不只断退出码或 stdout。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// mkLocalFS 建一个底层本地 FS（目录名用于区分 primary/replica）。
func mkLocalFS(t *testing.T, name string) syncpkg.FS {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	return syncpkg.NewLocalFS(root, nil)
}

// newErasureFS 建一个 Erasure=true 的 secretdata FS（小块策略保证 ≥2 个数据分块）。
func newErasureFS(t *testing.T) *SecretdataFS {
	t.Helper()
	inner := mkLocalFS(t, "backing")
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Erasure:   true,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS(erasure): %v", err)
	}
	return fs
}

// TestParity_RecoverMissingBlock：任一块丢失 → XOR parity 恢复 → 解密成功（k-of-k+1）。
// 写入生成 parity 段（meta.Parity 引用、ChunkCount==len(Chunks)）；对每个数据块独立验证：
// （建新 FS 写文件 → 删该块 → OpenRead 经 parity 恢复明文再解密）== 原内容（XOR 恒等式 +
// 整文件 SHA-256 全量校验兜底）。每轮独立（避免累删致 ≥2 块丢失）。
func TestParity_RecoverMissingBlock(t *testing.T) {
	t.Parallel()
	fs := newErasureFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "f.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["f.bin"]
	if e.meta.Parity == nil {
		t.Fatal("Erasure=true 应生成 parity 引用（meta.Parity != nil）")
	}
	if e.meta.Parity.ChunkCount != len(e.meta.Chunks) {
		t.Fatalf("Parity.ChunkCount=%d，应为分块数 %d", e.meta.Parity.ChunkCount, len(e.meta.Chunks))
	}
	if len(e.meta.Chunks) < 2 {
		t.Fatalf("测试需 ≥2 个数据分块（got %d）", len(e.meta.Chunks))
	}
	content := data(300)
	// 逐个分块独立验证（每轮新 FS，避免累删造成 ≥2 缺失）。
	for i := 0; i < len(e.meta.Chunks); i++ {
		t.Run(fmt.Sprintf("chunk%d", i), func(t *testing.T) {
			t.Parallel()
			fs2 := newErasureFS(t)
			if err := fs2.WriteFile(ctx, "f.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			e2 := fs2.index["f.bin"]
			// 用 fs2 自身分块遍历，避免依赖外层 fs 的随机分块数越界（Min 64/Max128 下
			// 同 size 恒 ≥3 块，但防御性钳制到 fs2 实际末块，杜绝潜在 flaky）。
			ci := e2.meta.Chunks[min(i, len(e2.meta.Chunks)-1)]
			blobPath := path.Join(e2.dirSeg, ci.FileName)
			if err := fs2.inner.Delete(ctx, blobPath); err != nil {
				t.Fatalf("删除分块 %s: %v", ci.FileName, err)
			}
			rc, rerr := fs2.OpenRead(ctx, "f.bin")
			if rerr != nil {
				t.Fatalf("删 %s 后 OpenRead（parity 恢复预期成功）: %v", ci.FileName, rerr)
			}
			got, gerr := io.ReadAll(rc)
			rc.Close()
			if gerr != nil {
				t.Fatalf("ReadAll: %v", gerr)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("删 %s 后经 parity 恢复解密内容不一致：len(got)=%d", ci.FileName, len(got))
			}
		})
	}
}

// TestParity_SecondMissingFails：k-of-k+1 只容忍 1 块丢失——删 2 块 → fail-closed。
func TestParity_SecondMissingFails(t *testing.T) {
	t.Parallel()
	fs := newErasureFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "f.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["f.bin"]
	if len(e.meta.Chunks) < 2 {
		t.Fatal("测试须 ≥2 分块")
	}
	for _, ci := range e.meta.Chunks[:2] {
		if err := fs.inner.Delete(ctx, path.Join(fs.dataSeg(e), ci.FileName)); err != nil {
			t.Fatalf("delete chunk %s: %v", ci.FileName, err)
		}
	}
	if _, err := fs.OpenRead(ctx, "f.bin"); err == nil {
		t.Error("删 2 块后应 fail-closed（k-of-k+1 只容忍 1 块丢失），却解码成功")
	}
}

// TestParity_RangeReadRecoversMissingBlock（I1 回归）：Erasure 卷上删一块后 OpenRangeRead
// 随机读**仍经 parity 恢复成功**（Range 段读失败回落整块→parity 路径，不被短路）。
// 底层 LocalFS 已实现 RangeReader（默认服务器内层）——删块后段读必失败，须回落恢复。
func TestParity_RangeReadRecoversMissingBlock(t *testing.T) {
	t.Parallel()
	fs := newErasureFS(t)
	ctx := context.Background()
	content := data(300)
	if err := fs.WriteFile(ctx, "f.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["f.bin"]
	if e.meta.Parity == nil {
		t.Fatal("Erasure=true 应生成 parity 引用")
	}
	if len(e.meta.Chunks) < 2 {
		t.Fatal("测试须 ≥2 分块")
	}
	// 删一个数据分块 blob。
	ci := e.meta.Chunks[0]
	if err := fs.inner.Delete(ctx, path.Join(fs.dataSeg(e), ci.FileName)); err != nil {
		t.Fatalf("delete chunk %s: %v", ci.FileName, err)
	}
	// 随机读一段（落在被删块区间或其邻近，跨块由 rangeReadBytes 逐块处理）：
	// 该块 Range 段读失败 → 回落整块（缺失）→ parity 恢复明文裁切 → 内容与原文一致。
	rc, rerr := fs.OpenRangeRead(ctx, "f.bin", 50, 100)
	if rerr != nil {
		t.Fatalf("删块后 OpenRangeRead（parity 恢复预期成功）: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	rc.Close()
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	if !bytes.Equal(got, content[50:150]) {
		t.Fatalf("删块后 OpenRangeRead 内容不一致：len(got)=%d，应为 %d", len(got), 100)
	}
}

// TestMultiTarget_ReplicaRead：多 target 复制——主 target 删某 blob → 副本可读。写入时容器
// 复制到全部 target（副本含同一 chunk blob）；删主 target 该 blob → OpenRead 回退副本成功。
func TestMultiTarget_ReplicaRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	primary := mkLocalFS(t, "primary")
	replica := mkLocalFS(t, "replica")
	fs, err := NewFSMultiplicas(primary, []syncpkg.FS{replica}, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Targets:   []string{"replica"},
	})
	if err != nil {
		t.Fatalf("NewFSMultiplicas: %v", err)
	}
	content := data(500)
	if err := fs.WriteFile(ctx, "m.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["m.bin"]
	rel := path.Join(e.dirSeg, e.meta.Chunks[0].FileName)
	// 副本 target 应含同一 chunk blob（写入复制到全部 target）。
	if _, serr := replica.Stat(ctx, rel); serr != nil {
		t.Fatalf("副本 target 应含被复制的分块 %s: %v", rel, serr)
	}
	// 仅删主 target 该 blob（副本仍在）→ OpenRead 回退副本可读。
	if derr := primary.Delete(ctx, rel); derr != nil {
		t.Fatalf("删主 target 分块: %v", derr)
	}
	rc, rerr := fs.OpenRead(ctx, "m.bin")
	if rerr != nil {
		t.Fatalf("主 target 分块删除后 OpenRead 应经副本回退成功: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	rc.Close()
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("多 target 副本读取内容不一致：len(got)=%d", len(got))
	}
}

// TestErasure_OffByDefault 验证纠错（Erasure）默认关闭：单卷普通写不生成 parity 引用
// （可选能力默认关闭，不影响既有单卷行为）。
func TestErasure_OffByDefault(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	writeContent(t, fs, ctx, "m.bin", 300)
	e := fs.index["m.bin"]
	if e.meta.Parity != nil {
		t.Error("Erasure=false（默认）不应生成 parity 引用")
	}
	rc, rerr := fs.OpenRead(ctx, "m.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(300)) {
		t.Error("Erasure=false 普通读应还原原内容（未回归）")
	}
}
