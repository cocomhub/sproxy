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
	"path"
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

// failDirMetaDeleteFS 让「目录 meta blob」的删除失败（模拟改名第二步失败 ⇒ 容器内并存两个 meta）。
type failDirMetaDeleteFS struct {
	wrap  syncpkg.FS
	armed bool
}

func (f *failDirMetaDeleteFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.wrap.ListDir(ctx, p)
}
func (f *failDirMetaDeleteFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *failDirMetaDeleteFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.wrap.OpenRead(ctx, p)
}
func (f *failDirMetaDeleteFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	return f.wrap.WriteFile(ctx, key, p, r, size, mtime)
}
func (f *failDirMetaDeleteFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *failDirMetaDeleteFS) Delete(ctx context.Context, p string) error {
	if f.armed && shardseal.ClassifyName(path.Base(p)) == shardseal.KindDirMeta {
		return fmt.Errorf("failDirMetaDeleteFS: 模拟旧目录 meta 删除失败 %s", p)
	}
	return f.wrap.Delete(ctx, p)
}
func (f *failDirMetaDeleteFS) MakeDir(ctx context.Context, p string) error {
	return f.wrap.MakeDir(ctx, p)
}

// TestRenameDir_StaleDirMeta_DeterministicArbitration 目录改名后旧 meta 删除失败（容器内并存两个
// 目录 meta）时，加载必须**确定性**取最新 meta（改名不静默回退，且多次重挂载结果一致）。
func TestRenameDir_StaleDirMeta_DeterministicArbitration(t *testing.T) {
	root := t.TempDir()
	inner := &failDirMetaDeleteFS{wrap: syncpkg.NewLocalFS(root, nil)}
	fs := newFSSharedInner(t, inner)
	ctx := context.Background()

	now := time.Now().UnixNano()
	if err := fs.MakeDir(ctx, "old-dir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "old-dir/a.bin", bytes.NewReader([]byte("hello")), 5, now); err != nil {
		t.Fatalf("write: %v", err)
	}
	inner.armed = true
	// 让新旧 meta 的 mtime **确定不同**（仲裁以「最新时间」为准；同 tick 相等会退化为按名字
	// 兜底 ⇒ 与「新旧」无关）。少量等待换取用例确定性。
	<-time.After(20 * time.Millisecond)
	if err := fs.Rename(ctx, "old-dir", "new-dir"); err != nil {
		t.Fatalf("改名（旧 meta 清理失败不应报错）: %v", err)
	}

	// 至少重挂载两次：两次结果必须一致，且都指向新名字（此前按 ListDir 顺序取第一个 ⇒ 可能回退）。
	for i := 0; i < 2; i++ {
		fs2 := newFSSharedInner(t, syncpkg.NewLocalFS(root, nil))
		if e, serr := fs2.Stat(ctx, "new-dir/a.bin"); serr != nil || e == nil {
			t.Fatalf("第 %d 次重挂载：改名后的文件在新路径不可见 (entry=%v, err=%v)", i+1, e, serr)
		}
		if e, _ := fs2.Stat(ctx, "old-dir/a.bin"); e != nil {
			t.Fatalf("第 %d 次重挂载：旧路径仍可见（双 meta 未按最新仲裁 ⇒ 改名静默回退）", i+1)
		}
		rc, oerr := fs2.OpenRead(ctx, "new-dir/a.bin")
		if oerr != nil {
			t.Fatalf("第 %d 次重挂载读取: %v", i+1, oerr)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != "hello" {
			t.Fatalf("第 %d 次重挂载内容 = %q, want hello", i+1, got)
		}
	}
}

// TestReadSeeker_SeekToSamePositionKeepsEOFVerify Seek(0,End)+Seek(0,Start)（http.ServeContent /
// Range 服务的固定姿势）不得使「EOF 整文件校验」失效。
func TestReadSeeker_SeekToSamePositionKeepsEOFVerify(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs := newFSSharedInner(t, syncpkg.NewLocalFS(root, nil))
	payload := bytes.Repeat([]byte("seek-verify-"), 4096) // ≈49KiB
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "f.bin", bytes.NewReader(payload), int64(len(payload)), time.Now().UnixNano()); err != nil {
		t.Fatalf("write: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "f.bin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		t.Fatal("读句柄必须实现 io.ReadSeeker")
	}
	// Range 服务的固定姿势：取大小 → 回到开头 → 顺序读完。
	size, serr := rs.Seek(0, io.SeekEnd)
	if serr != nil || size != int64(len(payload)) {
		t.Fatalf("Seek(End) = (%d, %v), want (%d, nil)", size, serr, len(payload))
	}
	if _, serr := rs.Seek(0, io.SeekStart); serr != nil {
		t.Fatalf("Seek(Start): %v", serr)
	}
	got, rerr := io.ReadAll(rs)
	if rerr != nil {
		t.Fatalf("顺序读失败（校验被 Seek 误关闭后仍应正常返回；此处失败说明读路径异常）: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("内容不一致: %d vs %d", len(got), len(payload))
	}

	// 反向断言：**哈希不匹配必须在 EOF 报错**（证明校验确实生效，而非「谁都没校验」）。
	// 真实句柄的 want 来自条目 meta（正确哈希），这里拿新句柄把 want 改成错误值即可。
	rc2, oerr2 := fs.OpenRead(ctx, "f.bin")
	if oerr2 != nil {
		t.Fatalf("二次 open: %v", oerr2)
	}
	defer rc2.Close()
	v, isV := rc2.(*verifyOnEOF)
	if !isV {
		t.Fatalf("读句柄应为 *verifyOnEOF, got %T", rc2)
	}
	v.want = strings.Repeat("0", 64)
	if _, serr := v.Seek(0, io.SeekEnd); serr != nil {
		t.Fatalf("verify Seek(End): %v", serr)
	}
	if _, serr := v.Seek(0, io.SeekStart); serr != nil {
		t.Fatalf("verify Seek(Start): %v", serr)
	}
	if _, rerr := io.ReadAll(v); rerr == nil {
		t.Fatal("整文件哈希不匹配必须在 EOF 报错（Seek 回原位不应关闭校验）")
	}
}
