// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestConcurrentOverwrite_NoIndexDataRace 钉住「覆盖写清理旧版本」与「并发索引变更」之间的锁纪律。
//
// 背景（独立对抗审核发现并复核，2026-10-18）：`commitEntry` 替换索引条目后需判定旧分块是否
// 仍被**其它**条目共享引用——该判定函数 `chunkSetStillReferenced` 遍历 `s.index`，其文档明确
// 「调用方须持 s.mu」。若判定发生在 `s.mu.Unlock()` **之后**，就会与其它 goroutine 的
// `s.index` 写入并发 ⇒ Go 运行时 **fatal error: concurrent map read and map write**
// （**不可 recover**，进程级崩溃；生产表现为「并发上传/覆盖写打崩服务」）✗。
//
// 判据的确定性（为什么不是「跑并发碰运气」）：**先预置一个很大的索引**，使
// `chunkSetStillReferenced` 的遍历窗口从「几微秒」拉长到「毫秒级」，同时另一组 goroutine
// 持续写新 rel（持续改索引）⇒ 两个访问必然重叠（不依赖调度运气，`-race` 与运行时 map 并发
// 检测都能稳定命中）。变异验证：把 `removeVersionMeta` 移回解锁之后 ⇒ 本用例必红。
func TestConcurrentOverwrite_NoIndexDataRace(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()

	// 1) 先建立被覆盖写的热条目（使覆盖写路径 prev != nil ⇒ 进入旧版本清理）。
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "hot.bin",
		bytes.NewReader(data(4096)), 4096, 0); err != nil {
		t.Fatalf("初始写入失败: %v", err)
	}

	// 2) 预置大量**其它**索引条目：拉长共享引用判定的遍历窗口（真实场景 = 大目录卷）。
	const pad = 40000
	fs.mu.Lock()
	for i := range pad {
		fs.index[fmt.Sprintf("pad/%d.bin", i)] = &metaEntry{
			size:     1,
			dirSeg:   fmt.Sprintf("container-%06d", i),
			metaName: fmt.Sprintf("meta-%06d", i),
			meta:     &shardseal.Meta{},
		}
	}
	fs.mu.Unlock()

	const rounds = 40
	var wg sync.WaitGroup

	// A 组：同 rel 覆盖写 —— 每轮触发一次「旧版本清理 ⇒ 遍历索引」。
	wg.Go(func() {
		body := bytes.Repeat([]byte{'a'}, 4096)
		for range rounds {
			if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "hot.bin",
				bytes.NewReader(body), int64(len(body)), 0); err != nil {
				t.Errorf("覆盖写失败: %v", err)
				return
			}
		}
	})

	// B 组：持续写入**新** rel —— 与 A 组的索引遍历并发（被证伪的那条路径）。
	for g := range 2 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range rounds {
				rel := fmt.Sprintf("new-%d-%d.bin", g, i)
				if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, rel,
					bytes.NewReader(data(64)), 64, 0); err != nil {
					t.Errorf("写新文件失败: %v", err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
}
