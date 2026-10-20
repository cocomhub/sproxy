// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// prod_hardening_test.go 钉住 2026-10-21 对抗审查 P1：转存「首写中途失败」必须**自清残骸**。
//
// 非原子写的目标卷（FTP STOR / WebDAV PUT / trusted 降级 / 无 WriteIfAbsent 的外部卷）在写途
// 中断（断连、ctx 取消、磁盘满、进程被 kill）会留下半截对象；而 `writeCount` 只在写成功后才
// ++，重试仍走「目标唯一」分支 ⇒ Stat 命中自己的残骸 ⇒ 每次重试都报「已存在（拒绝覆写）」
// ⇒ 任务退避耗尽后 failed（文案误指目标卷异常），残骸成为永久孤儿。

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// partialWriteFS 模拟「写到一半失败且留下半截对象」的非原子后端。
type partialWriteFS struct {
	inner  *memFS
	armed  bool
	writes int
}

func (p *partialWriteFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return p.inner.ListDir(ctx, path)
}
func (p *partialWriteFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return p.inner.Stat(ctx, path)
}
func (p *partialWriteFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return p.inner.OpenRead(ctx, path)
}
func (p *partialWriteFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	p.writes++
	if p.armed {
		// 先落半截（模拟 STOR/PUT 已在远端创建对象），再报错。
		p.inner.files[path] = []byte("half")
		return fmt.Errorf("partialWriteFS: 写中断")
	}
	return p.inner.WriteFile(ctx, key, path, r, size, mtime)
}
func (p *partialWriteFS) Rename(ctx context.Context, from, to string) error {
	return p.inner.Rename(ctx, from, to)
}
func (p *partialWriteFS) Delete(ctx context.Context, path string) error {
	return p.inner.Delete(ctx, path)
}
func (p *partialWriteFS) MakeDir(ctx context.Context, path string) error {
	return p.inner.MakeDir(ctx, path)
}

// TestWriteTargetUnique_FailedWriteCleansPartial 首写中途失败后不得留下半截对象（否则重试永久
// 被「已存在」阻断）。
func TestWriteTargetUnique_FailedWriteCleansPartial(t *testing.T) {
	t.Parallel()
	pfs := &partialWriteFS{inner: newMemFS(), armed: true}
	env := &transferEnv{ctx: context.Background(), targetFS: pfs, rel: "r/x.bin"}

	written, err := writeTargetUnique(env, strings.NewReader("hello"), 5, 0)
	if err == nil {
		t.Fatal("写中断必须返回错误")
	}
	if written {
		t.Fatal("失败不应报告 written=true")
	}
	if _, exists := pfs.inner.files["r/x.bin"]; exists {
		t.Fatal("失败后必须清理半截对象（否则重试会被『已存在』永久阻断）")
	}

	// 恢复后重试必须成功（真实场景 = 卷恢复可用）。
	pfs.armed = false
	written, err = writeTargetUnique(env, strings.NewReader("hello"), 5, 0)
	if err != nil || !written {
		t.Fatalf("卷恢复后重试应成功, written=%v err=%v", written, err)
	}
	if got := string(pfs.inner.files["r/x.bin"]); got != "hello" {
		t.Fatalf("重试写入内容 = %q, want hello", got)
	}
}
