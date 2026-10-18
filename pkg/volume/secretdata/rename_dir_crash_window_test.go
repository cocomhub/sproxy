// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestRenameDir_InterruptedMove_CrashAtAnyStep_MountsAndKeepsData 钉住「**目录**跨父移动在任意
// 步骤崩溃后，卷仍可挂载且数据不丢」。
//
// 背景（独立对抗审核发现并复核，2026-10-18）：目录移动是**两步**——重写目录 meta 的
// Name/ParentDirID + 物理搬容器目录（`renameDirLocked`）。崩在两步之间 ⇒ 「物理位置 ≠ meta
// 声明位置」⇒ 加载侧 `validateContainerPlacement` 严格判据 fail-closed ⇒ **整卷拒绝挂载 =
// 服务启动失败** ✗（数据不丢：容器目录 rename 是原子步；但可用性阻塞，且无文档化的修复工具）。
//
// 判据（顺序无关、确定性）：枚举「第 k 步之后崩溃」的每个 k，崩溃后用**同一底层**重新挂载，
// 断言 ① **挂载必须成功**（不得 boot fail）② 文件内容在**源或目标**之一**逐字节完整可读**
// （至少一处，不允许两处都不可读）。修复前：崩溃落在两步之间 ⇒ 挂载直接失败 ⇒ 必红。
func TestRenameDir_InterruptedMove_CrashAtAnyStep_MountsAndKeepsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := bytes.Repeat([]byte("dir-move-payload-"), 200)

	readAll := func(fs *SecretdataFS, rel string) ([]byte, bool) {
		rc, err := fs.OpenRead(ctx, rel)
		if err != nil {
			return nil, false
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			return nil, false
		}
		return b, true
	}

	const maxSteps = 8
	for k := 1; k <= maxSteps; k++ {
		k := k
		t.Run(fmt.Sprintf("crash_after_step_%d", k), func(t *testing.T) {
			d := t.TempDir()
			raw := syncpkg.NewLocalFS(d, nil)
			seed := newFSSharedInner(t, raw)
			for _, dir := range []string{"a", "b"} {
				if err := seed.MakeDir(ctx, dir); err != nil {
					t.Fatalf("建目录 %s 失败: %v", dir, err)
				}
			}
			if err := seed.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f.bin",
				bytes.NewReader(want), int64(len(want)), 0); err != nil {
				t.Fatalf("写入 a/f.bin 失败: %v", err)
			}

			// 注入崩溃：第 k 步之后 panic（模拟进程退出，回滚代码不执行）。
			faulty := newFSSharedInner(t, &crashPointFS{FS: raw, n: k})
			func() {
				defer func() {
					if r := recover(); r != nil && r != errSimulatedCrash {
						t.Fatalf("意外 panic: %v", r)
					}
				}()
				_ = faulty.Rename(ctx, "a", "b/a")
			}()

			// 重启：必须**挂载成功**（修复前此处在两步之间崩溃 ⇒ 直接 fail-closed = boot fail）。
			restarted, err := NewFS(raw, Options{
				Secret:    []byte("test-secret-key-000"),
				Algorithm: testAlgo,
				TempDir:   t.TempDir(),
			})
			if err != nil {
				t.Fatalf("崩溃点 k=%d 后**卷拒绝挂载**（boot fail）✗: %v", k, err)
			}

			// 目录逻辑位置：要么仍在 a（移动未生效/被回滚），要么在 b/a（移动完成）。
			oldLoc, oldOK := readAll(restarted, "a/f.bin")
			newLoc, newOK := readAll(restarted, "b/a/f.bin")
			oldGood := oldOK && bytes.Equal(oldLoc, want)
			newGood := newOK && bytes.Equal(newLoc, want)
			if !oldGood && !newGood {
				t.Fatalf("崩溃点 k=%d 后**两处都读不到完整内容**（数据丢失）✗："+
					"a/f.bin 可读=%v 内容一致=%v；b/a/f.bin 可读=%v 内容一致=%v",
					k, oldOK, oldGood, newOK, newGood)
			}
			if oldOK && !oldGood {
				t.Fatalf("k=%d：旧位置可读但内容不完整（半态）✗", k)
			}
			if newOK && !newGood {
				t.Fatalf("k=%d：新位置可读但内容不完整（半态）✗", k)
			}
		})
	}
}
