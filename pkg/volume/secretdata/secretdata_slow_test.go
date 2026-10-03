// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build slow

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件含 **真实 high 档（N=2^17）** 的一次性基准测试，`//go:build slow` 门控——
// 常规 `go test -race ./pkg/volume/secretdata/...` 不编译、不运行（避免真实 scrypt 的
// 数十秒墙钟成本）。
//
// 说明：内存测算是**类型级**（ScryptMemEstimate 纯算术，与 KDFOverride 正交），常规
// loadGate 机制测试已改 mock 档（TestLoadIndex_LoadGate_*）。ScryptMemEstimate ×2 记账
// 系数已由任务 13 实测（high 4 容器挂载峰值 ≈1.08GB ≈ 2×理论）标定，常规路径不需重复
// 实测；此处保留真实 high 的「实现对齐公式」一次性基准，供变更派生参数/记账系数时
// 回归核对。运行：`go test -tags=slow -race -run TestLoadIndex_HighTier ./pkg/volume/secretdata/`

// TestLoadIndex_HighTier_PeakMemory_OneOffBaseline （任务 13 修复轮 Imp-1 补充，任务 15
// 修复轮收敛为 //go:build slow 一次性基准）：**非套圆实测** real high 档卷挂载 loadIndex
// 期间
// 的堆峰值增量 ≤ maxLoadMemBudget（512MiB）——用 runtime.MemStats 独立采样（不是「并发×
// 派生内存公式」自回推）。high 档真实 scrypt 内存 128MiB/次、loadGate 自适应并发 ≤2（每槽
// 按 2× 记账）⇒ 实测堆峰值 ≤512MiB；若实测超预算，说明系数/预算需下调（该断言即守卫）。
// 串行（不并行）：堆峰值采样对其它并行测试的堆扰动敏感。
func TestLoadIndex_HighTier_PeakMemory_OneOffBaseline(t *testing.T) {
	// sproxy:serial: 堆峰值实测对全局堆敏感，串行运行避免并行测试扰动
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	highAlgo := shardseal.AlgorithmName + "-high"
	opts := Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: highAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	}
	fs, err := NewFS(inner, opts)
	if err != nil {
		t.Fatalf("NewFS(high): %v", err)
	}
	ctx := context.Background()
	// 4 容器各 1 文件：dir meta + 文件 meta 共 8 次派生、并发触达 loadGate 上界（否则峰值
	// 测不出），同时控制 high 档 -race 下的墙钟成本。
	const n = 4
	for i := range n {
		content := data(200 + i)
		if werr := fs.WriteFile(ctx, fmt.Sprintf("dir%d/f%d.bin", i, i), bytes.NewReader(content), int64(len(content)), 0); werr != nil {
			t.Fatalf("WriteFile(high): %v", werr)
		}
	}
	// 基线：先 GC 回收写路径已释放的派生内存，再取 HeapAlloc。
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	peak := base.HeapAlloc
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	})
	// 重挂载触发 loadIndex 并行派生（实测堆峰值，非公式回推）。
	if _, rerr := NewFS(inner, opts); rerr != nil {
		t.Fatalf("NewFS(high reload): %v", rerr)
	}
	close(done)
	wg.Wait()
	// 断言阈值 = 预算 + 分配器/GC slack：HeapAlloc 按 GOGC=100 允许增长到 ~2×live，且含
	// GC 节奏滞后噪声（real high 实测 ≈512MiB 预算 + ~2% slack）。无界并发（旧钳 8）峰值
	// ~1.5-2GiB，该阈值仍能清晰区分有界/无界。
	const loadPeakSlack = 64 << 20 // 64MiB（≈12.5% 预算）
	delta := peak - base.HeapAlloc
	if delta > maxLoadMemBudget+loadPeakSlack {
		t.Errorf("real high loadIndex 实测堆峰值增量 %d 超预算 %d（loadGate 自适应未守住预算）", delta, maxLoadMemBudget)
	}
}

// TestLoadIndex_HighTier_BoundedConcurrency_Slow （任务 15 修复轮收敛为 slow 一次性基准）：
// real high 档（N=2^17）卷挂载 loadIndex 并发受 loadGate 自适应钳制（≤min(2,NumCPU)）、
// 卷内文件可读回（同一 high 档写读同档 roundtrip）。high→min(2) 的钳制语义在常规路径由
// TestMaxParallelLoads_AdaptsToKDFTier（直接 Algorithm{ScryptN:...}、不注册）覆盖；本慢
// 测保留「真实 high FS 运行时 loadGate 容量 == maxParallelLoads」的端到端探针。
func TestLoadIndex_HighTier_BoundedConcurrency_Slow(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	highAlgo := shardseal.AlgorithmName + "-high"
	opts := Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: highAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	}
	fs, err := NewFS(inner, opts)
	if err != nil {
		t.Fatalf("NewFS(high): %v", err)
	}
	ctx := context.Background()
	const n = 3 // 少量文件即可覆盖多容器/多 meta 并发派生路径
	for i := range n {
		content := data(100 + i)
		if werr := fs.WriteFile(ctx, fmt.Sprintf("dir%d/f%d.bin", i%2, i), bytes.NewReader(content), int64(len(content)), 0); werr != nil {
			t.Fatalf("WriteFile(high): %v", werr)
		}
	}
	// 重挂载：loadGate 容量 = 随 high 档自适应并发（≤min(2,NumCPU)）。
	fs2, err := NewFS(inner, opts)
	if err != nil {
		t.Fatalf("NewFS(high reload): %v", err)
	}
	highAlg, _ := shardseal.AlgoByVersion(shardseal.AlgoV1GCMHigh)
	if want := maxParallelLoads(&highAlg); cap(fs2.loadGate) != want {
		t.Errorf("high 档 loadGate 容量=%d，应为自适应并发 %d", cap(fs2.loadGate), want)
	}
	for i := range n {
		k := fmt.Sprintf("dir%d/f%d.bin", i%2, i)
		rc, rerr := fs2.OpenRead(ctx, k)
		if rerr != nil {
			t.Fatalf("high 档挂载后 OpenRead(%s): %v", k, rerr)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data(100+i)) {
			t.Errorf("high 档挂载后 %s 内容不一致", k)
		}
	}
}
