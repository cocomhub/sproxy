// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// capacity_reader_capability_test.go —— 锁定：CapacityFS 的**记账包装**不得破坏下游对
// 写源的能力探测（io.Seeker / io.ReaderAt），否则下游流式卷（secretdata.canStream）误判
// 为「不可流式」⇒ 大文件整读进内存（实测 1.5 GiB 源 ⇒ RSS +3.3 GB，线性放大 ⇒ OOM）。
//
// 根因：WriteFile/WriteIfAbsent 用仅实现 Read 的 countingReader 包装写源 ⇒ 剥掉 seek 能力。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// readerCapSpy 记录传给内层的写源暴露了哪些能力（ReadAt/Seeker/SourcePath）。
type readerCapSpy struct {
	*mapFS
	gotReaderAt bool
	gotSeeker   bool
	hasSource   bool
	sourcePath  string
}

func (s *readerCapSpy) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	_, s.gotReaderAt = r.(io.ReaderAt)
	_, s.gotSeeker = r.(io.Seeker)
	if sp, ok := r.(interface{ SourcePath() string }); ok {
		s.hasSource = true
		s.sourcePath = sp.SourcePath()
	}
	return s.mapFS.WriteFile(ctx, key, p, r, size, mtime)
}

func newCapSpyFS(t *testing.T) (*CapacityFS, *readerCapSpy) {
	t.Helper()
	spy := &readerCapSpy{mapFS: &mapFS{files: map[string]string{}}}
	led, err := OpenLedger(t.TempDir(), "cap")
	if err != nil {
		t.Fatal(err)
	}
	return Wrap(spy, NewLedgerCounter(led, 0)), spy
}

// TestCapacityFS_PreservesReaderCapabilities 锁定记账包装的能力透传：
//   - 可 seek 源（*os.File）经包装后，内层仍能探测到 io.ReaderAt/io.Seeker（并可见源路径）；
//   - 不可 seek 源不得**虚假**暴露 seek/ReaderAt（否则下游误判可流式 ⇒ 送入流式路径失败）。
//
// 变异：改回仅实现 Read 的 countingReader ⇒ 第一段断言红（gotReaderAt/gotSeeker=false、
// hasSource=false）。
func TestCapacityFS_PreservesReaderCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// ① 可 seek 源：能力必须保留。
	src, err := os.CreateTemp(t.TempDir(), "seekable")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, werr := src.Write([]byte("seekable-src-payload")); werr != nil {
		t.Fatal(werr)
	}
	if _, serr := src.Seek(0, io.SeekStart); serr != nil {
		t.Fatal(serr)
	}
	fs, spy := newCapSpyFS(t)
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a.bin", src, 20, 0); werr != nil {
		t.Fatalf("write seekable: %v", werr)
	}
	if !spy.gotReaderAt || !spy.gotSeeker {
		t.Fatalf("可 seek 源经 capacity 包装后能力丢失：ReaderAt=%v Seeker=%v（下游 canStream 将误判"+
			"不可流式 ⇒ 整读进内存）", spy.gotReaderAt, spy.gotSeeker)
	}
	if !spy.hasSource || spy.sourcePath != src.Name() {
		t.Fatalf("源路径未透传：hasSource=%v path=%q want %q", spy.hasSource, spy.sourcePath, src.Name())
	}

	// ② 不可 seek 源：不得虚假暴露 seek/ReaderAt。
	plain := struct{ io.Reader }{bytes.NewReader([]byte("plain-payload"))}
	fs2, spy2 := newCapSpyFS(t)
	if werr := fs2.WriteFile(ctx, syncpkg.LogicalKey{}, "b.bin", plain, 13, 0); werr != nil {
		t.Fatalf("write non-seek: %v", werr)
	}
	if spy2.gotReaderAt || spy2.gotSeeker {
		t.Fatalf("不可 seek 源被虚假暴露 seek 能力：ReaderAt=%v Seeker=%v（下游会误判可流式并失败）",
			spy2.gotReaderAt, spy2.gotSeeker)
	}
}

// readAtConsumerFS 是「只能按 ReaderAt 消费、且 Stat 不可用」的写目标：
//   - WriteFile 对可 seek 源走 ReadAt（镜像 secretdata 流式卷）；
//   - Stat 恒失败 ⇒ physSize 回落 0 ⇒ 记账只能靠计数包装的**实测字节**（cr.n）。
type readAtConsumerFS struct {
	*mapFS
}

func (f *readAtConsumerFS) Stat(context.Context, string) (*syncpkg.Entry, error) {
	return nil, os.ErrNotExist
}

func (f *readAtConsumerFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, size, _ int64) error {
	var data []byte
	if ra, ok := r.(io.ReaderAt); ok {
		buf := make([]byte, size)
		n, rerr := ra.ReadAt(buf, 0)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return rerr
		}
		data = buf[:n]
	} else {
		b, rerr := io.ReadAll(r)
		if rerr != nil {
			return rerr
		}
		data = b
	}
	f.mu.Lock()
	f.files[p] = string(data)
	f.mu.Unlock()
	return nil
}

// TestCapacityFS_StreamingReadAtCounted 锁定：走流式（ReadAt）时记账仍以**实测被消费字节**为准
// （防声明长度撒谎）。声明 1000 但源实际只有 400 ⇒ 实用 ReadAt 读到 400 ⇒ 记账 400，
// 不得回落声明值 1000。
//
// 变异：ReadAt 不计入 cr.n ⇒ effective=0 ⇒ 回落声明 size=1000 ⇒ 断言红。
func TestCapacityFS_StreamingReadAtCounted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	led, err := OpenLedger(t.TempDir(), "readat")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, 0)
	fs := Wrap(&readAtConsumerFS{mapFS: &mapFS{files: map[string]string{}}}, c)

	payload := bytes.Repeat([]byte("z"), 400)
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "s.bin", bytes.NewReader(payload), 1000, 0); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if got := c.Used(); got != 400 {
		t.Fatalf("流式(ReadAt)路径记账 = %d，want 400（以实测读取字节为准，不得按声明 1000）", got)
	}
}

// errReadRefused：Read 被调用即报错——用于证明下游走的是流式（ReadAt）而非内存（io.ReadAll）。
var errReadRefused = errors.New("Read 不应被调用（应走流式 ReadAt）")

// readRefusingSeekable 实现 io.ReaderAt + io.Seeker，但 Read 故意失败。
type readRefusingSeekable struct {
	*bytes.Reader
}

func (readRefusingSeekable) Read([]byte) (int, error) { return 0, errReadRefused }

// TestCapacityFS_SeekableSourceStreamsThroughCapacityAndSecretdata 端到端锁定：
// 可 seek 源经 CapacityFS → secretdata（真实流式卷）写路径**必须走流式**（ReadAt 逐块加密），
// 不得回退内存路径（io.ReadAll）。用「Read 即失败、ReadAt 正常」的源区分两条路径：
//   - 修复前（包装剥掉 seek 能力）⇒ 走内存 ⇒ Read 被调 ⇒ 失败（红）；
//   - 修复后 ⇒ 走流式 ⇒ 只用 ReadAt ⇒ 成功，且内容可原样还原。
func TestCapacityFS_SeekableSourceStreamsThroughCapacityAndSecretdata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := secretdata.NewFS(inner, secretdata.Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	led, err := OpenLedger(t.TempDir(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	fs := Wrap(sfs, NewLedgerCounter(led, 0))

	payload := make([]byte, 1<<20) // 1 MiB：跨多块
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	src := readRefusingSeekable{Reader: bytes.NewReader(payload)}
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a.bin", src, int64(len(payload)), 0); werr != nil {
		t.Fatalf("可 seek 源经 CapacityFS 包装后应走流式（ReadAt）而非整读（Read）: %v", werr)
	}
	rc, rerr := fs.OpenRead(ctx, "a.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	got, gerr := io.ReadAll(rc)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("还原不一致：got %d bytes want %d", len(got), len(payload))
	}
}
