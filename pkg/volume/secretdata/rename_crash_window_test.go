// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// crashPointFS 在「第 n 次会改变底层布局的调用」**真做完之后** panic——精确模拟「进程在
// 该步之后崩溃」：崩溃与「返回错误」的关键差别是**调用方的回滚代码不会执行**（回滚只在
// 错误返回路径上），这正是崩溃窗口类缺陷的判据。
type crashPointFS struct {
	syncpkg.FS
	mu   sync.Mutex
	n    int // 目标序号（1-based）
	seen int
}

func (f *crashPointFS) crashNow() {
	f.mu.Lock()
	f.seen++
	hit := f.seen == f.n
	f.mu.Unlock()
	if hit {
		panic(errSimulatedCrash)
	}
}

var errSimulatedCrash = fmt.Errorf("crashPointFS: 模拟进程崩溃")

func (f *crashPointFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	err := f.FS.WriteFile(ctx, key, p, r, size, mtime)
	f.crashNow()
	return err
}

func (f *crashPointFS) Rename(ctx context.Context, from, to string) error {
	err := f.FS.Rename(ctx, from, to)
	f.crashNow()
	return err
}

func (f *crashPointFS) Delete(ctx context.Context, p string) error {
	err := f.FS.Delete(ctx, p)
	f.crashNow()
	return err
}

// TestRename_CrossContainer_CrashAtAnyStep_NoDataLoss 钉住「同卷**跨容器**移动/改名在任意步骤
// 崩溃都不得丢数据」。
//
// 背景（两路独立对抗审核一致发现，2026-10-18）：`relocateRenamedBlobsLocked` 对**非共享**条目
// 走 `moveFileBlobs`/`moveContentBlobs`，逐个 blob 用底层 `Rename`（**真 mv，源立刻消失**）且
// **meta 最后**搬。崩在中途 ⇒ 源容器 meta 仍在、其引用的分块却已被搬走 ⇒ 源条目可见（size
// 正常）但 `OpenRead` 报「读底层分块失败」；目标容器的分块无任何 meta 引用 ⇒ 开启 GC 后会被
// 当孤儿**物理清扫** ⇒ **单文件永久不可恢复** ✗。这同时与文档 §5「文件移动 = 物理**复制**分块
// → 删源副本」及 `secretdata.go:2211` 注释「先复制全部成功再删源」**矛盾**（实现是 mv-first）。
//
// 判据（确定性、非概率）：枚举「在第 k 步之后崩溃」的每一个 k，崩溃后用**同一底层**重新挂载
// （= 重启），断言 **源或目标至少一方内容完整可读**（且内容逐字节等于原文）；两者都不可读即红。
// 修复前：k 落在「分块已 mv 走、meta 尚未搬」时，源与目标**都不可读** ⇒ 必红。
func TestRename_CrossContainer_CrashAtAnyStep_NoDataLoss(t *testing.T) {
	ctx := context.Background()
	want := bytes.Repeat([]byte("crash-window-payload-"), 300) // 多分块，放大中间态

	// 建卷 + 建 a/ b/ 两个目录 + 在 a/ 放文件（跨容器移动的前提）。
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	base := newFSSharedInner(t, inner)
	if err := base.MakeDir(ctx, "a"); err != nil {
		t.Fatalf("建目录 a 失败: %v", err)
	}
	if err := base.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("建目录 b 失败: %v", err)
	}
	if err := base.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f.bin",
		bytes.NewReader(want), int64(len(want)), 0); err != nil {
		t.Fatalf("写入 a/f.bin 失败: %v", err)
	}

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

	// 枚举崩溃点：k 超过实际步数时不会触发 panic（正常完成 ⇒ 目标可读）。
	const maxSteps = 12
	for k := 1; k <= maxSteps; k++ {
		k := k
		t.Run(fmt.Sprintf("crash_after_step_%d", k), func(t *testing.T) {
			// 每轮用**全新**底层目录，保证各 k 互不影响（同构夹具）。
			d := t.TempDir()
			raw := syncpkg.NewLocalFS(d, nil)
			seed := newFSSharedInner(t, raw)
			if err := seed.MakeDir(ctx, "a"); err != nil {
				t.Fatalf("建目录 a 失败: %v", err)
			}
			if err := seed.MakeDir(ctx, "b"); err != nil {
				t.Fatalf("建目录 b 失败: %v", err)
			}
			if err := seed.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f.bin",
				bytes.NewReader(want), int64(len(want)), 0); err != nil {
				t.Fatalf("写入 a/f.bin 失败: %v", err)
			}

			// 注入崩溃：第 k 步之后 panic（模拟进程退出，回滚不执行）。
			faulty := newFSSharedInner(t, &crashPointFS{FS: raw, n: k})
			func() {
				defer func() {
					if r := recover(); r != nil {
						if r != errSimulatedCrash {
							t.Fatalf("意外 panic: %v", r)
						}
					}
				}()
				_ = faulty.Rename(ctx, "a/f.bin", "b/f.bin")
			}()

			// 重启：用同一底层重新挂载（只读断言，不再写）。
			restarted := newFSSharedInner(t, raw)

			src, srcOK := readAll(restarted, "a/f.bin")
			dst, dstOK := readAll(restarted, "b/f.bin")
			srcGood := srcOK && bytes.Equal(src, want)
			dstGood := dstOK && bytes.Equal(dst, want)

			if !srcGood && !dstGood {
				t.Fatalf("崩溃点 k=%d 后**源与目标都不可读**（数据丢失）✗："+
					"源可读=%v（内容一致=%v）目标可读=%v（内容一致=%v）",
					k, srcOK, srcOK && bytes.Equal(src, want), dstOK, dstOK && bytes.Equal(dst, want))
			}
			// 可读的一侧必须是**完整正确**内容（不允许半态/截断被当作成功）。
			if srcOK && !bytes.Equal(src, want) {
				t.Fatalf("k=%d：源可读但内容不完整（半态）✗", k)
			}
			if dstOK && !bytes.Equal(dst, want) {
				t.Fatalf("k=%d：目标可读但内容不完整（半态）✗", k)
			}
		})
	}
}
