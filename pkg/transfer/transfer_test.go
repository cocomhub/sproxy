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

	"time"

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
func (m *memFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, sz, mt int64) error {
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
	if err := m.WriteFile(ctx, syncpkg.LogicalKey{}, p, r, sz, mt); err != nil {
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "dst/x.bin",
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
		Destination: v, Scheme: "vault", Volume: "vol1", Rel: "dst/x.bin",
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "pack.tar.gz",
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "enc.tar.gz.aes",
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "dst/x.bin",
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
		Destination: v, Scheme: "vault", Volume: "vol1", Rel: "dst/x.bin",
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "pack.tar.gz",
	}); err != nil {
		t.Fatalf("打包路径应回落 WriteFile: %v", err)
	}
	if len(base.files["pack.tar.gz"]) == 0 {
		t.Fatal("打包产物未落盘")
	}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "s.txt", Size: 3, Reader: strings.NewReader("abc")}},
		Preprocess:  &TarPreprocessor{Compression: compressx.Gzip},
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "pack.tar.gz",
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
		Destination: v, Scheme: "vault", Volume: "vol1",
		Rel: "dst/a b#c%d.txt",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := "vault://vol1/dst/a%20b%23c%25d.txt"; got != want {
		t.Fatalf("url=%q want %q", got, want)
	}
}

// noCallFS 只暴露 sync.FS 的 7 个方法（以接口嵌入隐藏底层实现的可选能力）——复现
// transferVolumeFS 一类「装饰器/包装器吞掉可选能力」的形态。
type noCallFS struct{ syncpkg.FS }

// noCallVol 是 transfer.VolumeFS（Scheme/Volume）+ 被隐藏 WriteIfAbsent 的 FS。
type noCallVol struct {
	noCallFS
	scheme string
	name   string
}

func (v *noCallVol) Scheme() string { return v.scheme }
func (v *noCallVol) Volume() string { return v.name }

// TestExecute_NoWriteIfAbsentStillRejectsOverwrite 目标 FS **完全没有** WriteIfAbsent 能力
// 时，写前 Stat 存在性检查仍须生效（不得因断言失败就静默覆盖）。
func TestExecute_NoWriteIfAbsentStillRejectsOverwrite(t *testing.T) {
	t.Parallel()
	base := newMemFS()
	v := &noCallVol{FS: base, scheme: "vault", name: "vol1"}
	content := []byte("payload")
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/x.bin", Size: int64(len(content)), Reader: strings.NewReader(string(content))}},
		Destination: v, Scheme: "vault", Volume: "vol1", Rel: "dst/x.bin",
	}); err != nil {
		t.Fatalf("首次写: %v", err)
	}
	if _, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "src/y.bin", Size: 1, Reader: strings.NewReader("z")}},
		Destination: v, Scheme: "vault", Volume: "vol1", Rel: "dst/x.bin",
	}); !errors.Is(err, ErrExists) {
		t.Fatalf("无 WriteIfAbsent 能力时二次写应 ErrExists，got %v", err)
	}
	if got := string(base.files["dst/x.bin"]); got != string(content) {
		t.Fatalf("二次写不得覆盖，got %q", got)
	}
}

// TestNew_Registry 预处理选型注册表：内置 "tar" 可构造（含加密变体）；未知名 → ErrUnknownPreprocessor。
func TestNew_Registry(t *testing.T) {
	t.Parallel()
	p, err := New("tar", PreprocessorConfig{Compression: compressx.Gzip})
	if err != nil {
		t.Fatalf("New(tar): %v", err)
	}
	if p.Name() != "tar" {
		t.Fatalf("Name=%q want tar", p.Name())
	}
	enc, err := New("tar", PreprocessorConfig{Compression: compressx.Gzip, EncryptKey: make([]byte, 32)})
	if err != nil {
		t.Fatalf("New(tar,encrypt): %v", err)
	}
	if enc.Name() != "tar-encrypt" {
		t.Fatalf("Name=%q want tar-encrypt", enc.Name())
	}
	if _, err := New("nope", PreprocessorConfig{}); !errors.Is(err, ErrUnknownPreprocessor) {
		t.Fatalf("未知预处理器应 ErrUnknownPreprocessor，got %v", err)
	}
}

// TestTarPreprocessor_Pack_ShortReaderFails 声明大小与实读字节不符 → Pack 必须报错：
// tar.Writer.Close 的「missed writing N bytes」在 Close 时暴露，defer Close 丢弃该错误
// 会让损坏产物被当作成功（P1）。
func TestTarPreprocessor_Pack_ShortReaderFails(t *testing.T) {
	t.Parallel()
	p := &TarPreprocessor{Compression: compressx.Gzip}
	var buf bytes.Buffer
	err := p.Pack(context.Background(), []SourceEntry{
		{Rel: "a.txt", Size: 100, Reader: strings.NewReader("0123456789")}, // 声明 100 实读 10
	}, &buf)
	if err == nil {
		t.Fatal("声明长度与内容不符应报错（Close 错误被吞 = 损坏产物静默成功）")
	}
}

// TestExecute_Pack_PackFailureRemovesArtifact 打包收尾失败（目标已收下字节但内容损坏）
// → 必须删除目标产物，不得留损坏归档。
func TestExecute_Pack_PackFailureRemovesArtifact(t *testing.T) {
	t.Parallel()
	v := &memVol{memFS: newMemFS(), scheme: "vault", name: "vol1"}
	_, err := Execute(context.Background(), TransferRequest{
		Entries:     []SourceEntry{{Rel: "a.txt", Size: 100, Reader: strings.NewReader("0123456789")}},
		Preprocess:  &TarPreprocessor{Compression: compressx.Gzip},
		Destination: v, Scheme: "vault", Volume: "vol1", Rel: "p.tar.gz",
	})
	if err == nil {
		t.Fatal("应失败")
	}
	if _, ok := v.files["p.tar.gz"]; ok {
		t.Fatal("失败后不得残留损坏产物")
	}
}

// TestWriteUnique_MTimeUnitNano 单位边界回归：SourceEntry.ModTime 是 Unix **秒**，
// 目标 FS 的 mtime 契约是 **UnixNano**——写入前必须换算（否则 stat 显示 1970 值）。
func TestWriteUnique_MTimeUnitNano(t *testing.T) {
	t.Parallel()
	fs := newMTimeRecorderFS()
	srcSec := int64(1792485000) // Unix 秒（2026 年）
	if err := writeUnique(t.Context(), fs, "a.bin", strings.NewReader("x"), 1, srcSec, false); err != nil {
		t.Fatalf("writeUnique: %v", err)
	}
	if got := fs.mtime("a.bin"); got != srcSec*int64(time.Second) {
		t.Fatalf("写入 mtime = %d，want %d（纳秒）——秒/纳秒单位回归", got, srcSec*int64(time.Second))
	}
	// 未知（0）保持 0，不伪造时间
	if err := writeUnique(t.Context(), fs, "b.bin", strings.NewReader("y"), 1, 0, false); err != nil {
		t.Fatalf("writeUnique: %v", err)
	}
	if got := fs.mtime("b.bin"); got != 0 {
		t.Fatalf("未知 mtime 应保持 0，got %d", got)
	}
}

// newMTimeRecorderFS 最小 FS：只记录写入 mtime（断言单位用）。
func newMTimeRecorderFS() *mtimeRecorderFS {
	return &mtimeRecorderFS{mt: map[string]int64{}}
}

type mtimeRecorderFS struct{ mt map[string]int64 }

func (f *mtimeRecorderFS) mtime(rel string) int64 { return f.mt[rel] }
func (f *mtimeRecorderFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) {
	return nil, nil
}
func (f *mtimeRecorderFS) Stat(context.Context, string) (*syncpkg.Entry, error) { return nil, nil }
func (f *mtimeRecorderFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (f *mtimeRecorderFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, rel string, r io.Reader, _ int64, mtime int64) error {
	_, _ = io.Copy(io.Discard, r)
	f.mt[rel] = mtime
	return nil
}
func (f *mtimeRecorderFS) WriteIfAbsent(_ context.Context, rel string, r io.Reader, size, mtime int64) (bool, error) {
	return true, f.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel, r, size, mtime)
}
func (f *mtimeRecorderFS) Rename(context.Context, string, string) error { return nil }
func (f *mtimeRecorderFS) Delete(context.Context, string) error         { return nil }
func (f *mtimeRecorderFS) MakeDir(context.Context, string) error        { return nil }
