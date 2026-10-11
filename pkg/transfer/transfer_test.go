// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/compressx"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memFS 测试用内存卷（实现 WriteIfAbsent + FS 7 方法）。
type memFS struct {
	files map[string][]byte
}

func newMemFS() *memFS { return &memFS{files: map[string][]byte{}} }

func (m *memFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *memFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if b, ok := m.files[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, IsDir: false, Size: int64(len(b))}, nil
	}
	return nil, os.ErrNotExist
}
func (m *memFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if b, ok := m.files[p]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return nil, os.ErrNotExist
}
func (m *memFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.files[p] = b
	return nil
}
func (m *memFS) Rename(ctx context.Context, f, t string) error { return nil }
func (m *memFS) Delete(ctx context.Context, p string) error    { delete(m.files, p); return nil }
func (m *memFS) MakeDir(ctx context.Context, p string) error   { return nil }
func (m *memFS) WriteIfAbsent(ctx context.Context, p string, r io.Reader, sz, mt int64) (bool, error) {
	if _, ok := m.files[p]; ok {
		return false, nil
	}
	if err := m.WriteFile(ctx, p, r, sz, mt); err != nil {
		return false, err
	}
	return true, nil
}

// memVol 实现 VolumeFS（scheme/volume 元数据）。
type memVol struct {
	*memFS
	scheme string
	name   string
}

func (v *memVol) Scheme() string { return v.scheme }
func (v *memVol) Volume() string { return v.name }

func TestExecute_Individual_RoundTrip(t *testing.T) {
	t.Parallel()
	v := &memVol{memFS: newMemFS(), scheme: "vault", name: "vol1"}
	content := []byte("individual payload")
	url, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/x.bin", Size: int64(len(content)), Reader: strings.NewReader(string(content))}},
		Destination: v,
		Rel:         "dst/x.bin",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if url != "vault://vol1/dst/x.bin" {
		t.Fatalf("url=%q want vault://vol1/dst/x.bin", url)
	}
	if got := string(v.files["dst/x.bin"]); got != string(content) {
		t.Fatalf("内容不一致 got %q want %q", got, content)
	}
	// 重复执行 → ErrExists（WriteIfAbsent 拒绝覆盖）
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/x.bin", Size: 1, Reader: strings.NewReader("z")}},
		Destination: v, Rel: "dst/x.bin",
	}); err != ErrExists {
		t.Fatalf("重复转存应 ErrExists，got %v", err)
	}
}

func TestExecute_Pack_TarGz(t *testing.T) {
	t.Parallel()
	v := &memVol{memFS: newMemFS(), scheme: "vault", name: "vol1"}
	p := &TarPreprocessor{Compression: compressx.Gzip}
	url, err := Execute(context.Background(), TransferRequest{
		Entries: []SourceEntry{
			{Rel: "a.txt", Size: 5, Reader: strings.NewReader("aaaaa")},
			{Rel: "b.txt", Size: 5, Reader: strings.NewReader("bbbbb")},
		},
		Preprocess:  p,
		Destination: v,
		Rel:         "pack.tar.gz",
	})
	if err != nil {
		t.Fatalf("Execute pack: %v", err)
	}
	if url != "vault://vol1/pack.tar.gz" {
		t.Fatalf("url=%q", url)
	}
	// 解包验证内容
	gr, err := gzip.NewReader(bytes.NewReader(v.files["pack.tar.gz"]))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
	if got["a.txt"] != "aaaaa" || got["b.txt"] != "bbbbb" {
		t.Fatalf("tar 内容 %v", got)
	}
}

func TestExecute_Pack_Encrypt(t *testing.T) {
	t.Parallel()
	v := &memVol{memFS: newMemFS(), scheme: "vault", name: "vol1"}
	key := make([]byte, 32)
	p := &TarPreprocessor{Compression: compressx.Gzip, EncryptKey: key}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "s.txt", Size: 3, Reader: strings.NewReader("abc")}},
		Preprocess:  p,
		Destination: v,
		Rel:         "enc.tar.gz.aes",
	}); err != nil {
		t.Fatalf("Execute encrypt: %v", err)
	}
	// 密文：直接 gzip 解应失败（已加密）
	if _, err := gzip.NewReader(bytes.NewReader(v.files["enc.tar.gz.aes"])); err == nil {
		t.Fatal("加密产物不应直接 gzip 可解")
	}
}

// unsupportedWriteIfAbsentVol 是「装饰器语义」目标卷：**无条件实现** WriteIfAbsent 但底层
// 未实现（返回 syncpkg.ErrUnsupported）——与 trusted.Guard/Wrap、capacity、staging 装饰
// 器链同型（对可选能力接口的类型断言恒真）。
type unsupportedWriteIfAbsentVol struct {
	*memVol
}

func (u *unsupportedWriteIfAbsentVol) WriteIfAbsent(_ context.Context, _ string, _ io.Reader, _, _ int64) (bool, error) {
	return false, fmt.Errorf("decorator: %w", syncpkg.ErrUnsupported)
}

// TestExecute_WriteIfAbsentUnsupportedFallsBack individual 纯转存：经装饰器链时
// WriteIfAbsent 能力接口恒存在但返回 ErrUnsupported → 须回落 WriteFile（不得把「未实现」
// 当写失败），且覆盖保护仍生效（二次写 ErrExists）。
func TestExecute_WriteIfAbsentUnsupportedFallsBack(t *testing.T) {
	t.Parallel()
	base := newMemFS()
	v := &unsupportedWriteIfAbsentVol{memVol: &memVol{memFS: base, scheme: "vault", name: "vol1"}}
	content := []byte("fallback payload")
	url, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/x.bin", Size: int64(len(content)), Reader: strings.NewReader(string(content))}},
		Destination: v,
		Rel:         "dst/x.bin",
	})
	if err != nil {
		t.Fatalf("应回落 WriteFile 而非报 ErrUnsupported: %v", err)
	}
	if url != "vault://vol1/dst/x.bin" {
		t.Fatalf("url=%q", url)
	}
	if got := string(base.files["dst/x.bin"]); got != string(content) {
		t.Fatalf("内容 %q want %q", got, content)
	}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/y.bin", Size: 1, Reader: strings.NewReader("z")}},
		Destination: v, Rel: "dst/x.bin",
	}); !errors.Is(err, ErrExists) {
		t.Fatalf("二次写应 ErrExists（覆盖保护仍生效），got %v", err)
	}
}

// TestExecute_Pack_WriteIfAbsentUnsupportedFallsBack 打包预处理路径同样回落（V1 覆盖
// 保护不因装饰器链丢失）。
func TestExecute_Pack_WriteIfAbsentUnsupportedFallsBack(t *testing.T) {
	t.Parallel()
	base := newMemFS()
	v := &unsupportedWriteIfAbsentVol{memVol: &memVol{memFS: base, scheme: "vault", name: "vol1"}}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "s.txt", Size: 3, Reader: strings.NewReader("abc")}},
		Preprocess:  &TarPreprocessor{Compression: compressx.Gzip},
		Destination: v,
		Rel:         "pack.tar.gz",
	}); err != nil {
		t.Fatalf("打包路径应回落 WriteFile: %v", err)
	}
	if len(base.files["pack.tar.gz"]) == 0 {
		t.Fatal("打包产物未落盘")
	}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "s.txt", Size: 3, Reader: strings.NewReader("abc")}},
		Preprocess:  &TarPreprocessor{Compression: compressx.Gzip},
		Destination: v,
		Rel:         "pack.tar.gz",
	}); !errors.Is(err, ErrExists) {
		t.Fatalf("二次打包转存应 ErrExists，got %v", err)
	}
}

// TestExecute_TargetURLPercentEncodes 目标 URL 逐段 percent-encode（`#`/`%`/空格等文件名
// 可 ResolveURL 往返；与 cloud 转存 transferURL 同规约）。
func TestExecute_TargetURLPercentEncodes(t *testing.T) {
	t.Parallel()
	v := &memVol{memFS: newMemFS(), scheme: "vault", name: "vol1"}
	content := []byte("x")
	got, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/x", Size: int64(len(content)), Reader: strings.NewReader(string(content))}},
		Destination: v,
		Rel:         "dst/a b#c%d.txt",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := "vault://vol1/dst/a%20b%23c%25d.txt"; got != want {
		t.Fatalf("url=%q want %q", got, want)
	}
}
