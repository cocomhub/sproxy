// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
