// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// prod_hardening_test.go 钉住两个**生产级**缺陷的修复（2026-10-21 对抗审查 P0）：
//
//  1. rollbackWrite → pruneCreatedDirs 在**持卷锁的调用方**（改名/复制路径的 meta 上传）
//     上无条件 s.mu.Lock() ⇒ 自锁死（sync.Mutex 不可重入）⇒ 整卷永久挂起。
//  2. 删除目录**最后一个**文件时的空容器回收，会把同目录**在途写**已上传的分块/meta 一并删掉
//     （dirHasContentLocked 只看已提交索引，不看在途写）⇒ 写入返回成功但内容已丢。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// failMetaWriteFS 让「文件 meta blob」的写入失败（触发改名路径的回滚）。
type failMetaWriteFS struct {
	wrap   syncpkg.FS
	mu     sync.Mutex
	armed  bool
	failed int
}

// arm 武装故障注入（此前正常工作，便于先准备数据再触发失败路径）。
func (f *failMetaWriteFS) arm() {
	f.mu.Lock()
	f.armed = true
	f.mu.Unlock()
}

func (f *failMetaWriteFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.wrap.ListDir(ctx, p)
}
func (f *failMetaWriteFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *failMetaWriteFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.wrap.OpenRead(ctx, p)
}
func (f *failMetaWriteFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	name := string(p)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if shardseal.ClassifyName(name) == shardseal.KindFileMeta {
		f.mu.Lock()
		on := f.armed
		if on {
			f.failed++
		}
		f.mu.Unlock()
		if !on {
			return f.wrap.WriteFile(ctx, key, p, r, size, mtime)
		}
		return fmt.Errorf("failMetaWriteFS: 模拟 meta 写失败 %s", p)
	}
	return f.wrap.WriteFile(ctx, key, p, r, size, mtime)
}
func (f *failMetaWriteFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *failMetaWriteFS) Delete(ctx context.Context, p string) error  { return f.wrap.Delete(ctx, p) }
func (f *failMetaWriteFS) MakeDir(ctx context.Context, p string) error { return f.wrap.MakeDir(ctx, p) }

// TestRenameMetaWriteFailure_DoesNotDeadlockVolume：改名路径的 meta 写失败后必须**返回错误**，
// 且卷仍可服务（此前 rollbackWrite→pruneCreatedDirs 在持锁上下文里再取 s.mu ⇒ 永久死锁）。
func TestRenameMetaWriteFailure_DoesNotDeadlockVolume(t *testing.T) {
	root := t.TempDir()
	inner := &failMetaWriteFS{wrap: syncpkg.NewLocalFS(root, nil)}
	fs := newFSSharedInner(t, inner)

	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "a.txt", bytes.NewReader([]byte("hello")), 5, time.Now().UnixNano()); err != nil {
		t.Fatalf("初始写入: %v", err)
	}

	inner.arm()
	done := make(chan error, 1)
	go func() {
		done <- fs.Rename(context.Background(), "a.txt", "b.txt")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("meta 写失败时改名胜出（应返回错误）")
		}
	case <-time.After(10 * time.Second):
		inner.mu.Lock()
		n := inner.failed
		inner.mu.Unlock()
		t.Fatalf("Rename 在 meta 写失败后未返回（卷锁死锁，失败注入次数=%d）", n)
	}

	// 卷必须仍可服务（锁已释放）：ListDir 若也挂起说明死锁未修复。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fs.ListDir(ctx, "."); err != nil {
		t.Fatalf("死锁修复后 ListDir 应可用: %v", err)
	}
}

// blockingWriteFS 在武装后**阻塞第一次** WriteFile（模拟「在途写正在上传分块」）。
type blockingWriteFS struct {
	wrap    syncpkg.FS
	armed   chan struct{} // 关闭 = 武装
	entered chan struct{} // 进入阻塞（一次性通知）
	release chan struct{} // 关闭 = 放行
	once    sync.Once
}

func (b *blockingWriteFS) blockIfArmed() {
	select {
	case <-b.armed:
	default:
		return
	}
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
}

func (b *blockingWriteFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return b.wrap.ListDir(ctx, p)
}
func (b *blockingWriteFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return b.wrap.Stat(ctx, p)
}
func (b *blockingWriteFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return b.wrap.OpenRead(ctx, p)
}
func (b *blockingWriteFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	b.blockIfArmed()
	return b.wrap.WriteFile(ctx, key, p, r, size, mtime)
}
func (b *blockingWriteFS) Rename(ctx context.Context, from, to string) error {
	return b.wrap.Rename(ctx, from, to)
}
func (b *blockingWriteFS) Delete(ctx context.Context, p string) error  { return b.wrap.Delete(ctx, p) }
func (b *blockingWriteFS) MakeDir(ctx context.Context, p string) error { return b.wrap.MakeDir(ctx, p) }

// TestDeleteLastFile_KeepsInFlightWrite：删目录最后一个文件触发的空容器回收，不得删掉同目录
// 在途写已上传的分块（此前 removeEmptyContainerLocked 会清空容器内全部文件 ⇒ 写入成功但内容丢失）。
func TestDeleteLastFile_KeepsInFlightWrite(t *testing.T) {
	root := t.TempDir()
	inner := &blockingWriteFS{
		wrap:    syncpkg.NewLocalFS(root, nil),
		armed:   make(chan struct{}),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	fs := newFSSharedInner(t, inner)
	ctx := context.Background()

	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/old.txt", bytes.NewReader([]byte("old")), 3, time.Now().UnixNano()); err != nil {
		t.Fatalf("预置文件: %v", err)
	}

	payload := bytes.Repeat([]byte("payload-"), 200) // 1.4KiB，多分块
	close(inner.armed)
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- fs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/new.txt", bytes.NewReader(payload), int64(len(payload)), time.Now().UnixNano())
	}()

	select {
	case <-inner.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("在途写未进入上传阶段（用例前提不成立）")
	}

	// 在途写停在中途：此时删除同目录的**最后一个已提交文件**（触发空容器回收）。
	if err := fs.Delete(ctx, "d/old.txt"); err != nil {
		t.Fatalf("删除旧文件: %v", err)
	}

	close(inner.release)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("在途写应成功: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("在途写未完成")
	}

	// ① 内容必须正确（分块没被回收删掉）。
	rc, err := fs.OpenRead(ctx, "d/new.txt")
	if err != nil {
		t.Fatalf("读在途写产物: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("读在途写产物内容: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("在途写内容被破坏: got %d bytes, want %d", len(got), len(payload))
	}

	// ② 模拟重启重挂载：文件必须仍然存在且可读（容器目录 meta 未被回收删掉）。
	fs2 := newFSSharedInner(t, syncpkg.NewLocalFS(root, nil))
	rc2, oerr := fs2.OpenRead(ctx, "d/new.txt")
	if oerr != nil {
		t.Fatalf("重启后文件丢失: %v", oerr)
	}
	got2, rerr2 := io.ReadAll(rc2)
	_ = rc2.Close()
	if rerr2 != nil || !bytes.Equal(got2, payload) {
		t.Fatalf("重启后内容不一致: err=%v len=%d want=%d", rerr2, len(got2), len(payload))
	}
}
