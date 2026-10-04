// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// download_external_test.go 验证外部卷读路由（2026-10-05 通用文件获取）：
// resolveExternalDownload 在本地定位未命中后，对 secretdata/baidupcs 等外部卷
// 分 A（解密转发）/ B（302 直链）/ C（私密转发）三态。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// extFS is 内存 sync.FS + DirectURLProvider + RangeReader（测试外部卷）。
type extFS struct {
	files map[string]string
	dlink string // 非空 = 支持直链；空 = 不支持（模拟无会话）
}

func (f *extFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) { return nil, nil }
func (f *extFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	if _, ok := f.files[p]; !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: p, Path: p, Size: int64(len(f.files[p])), MTime: 0}, nil
}
func (f *extFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	content, ok := f.files[p]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(strings.NewReader(content)), nil
}
func (f *extFS) WriteFile(_ context.Context, p string, r io.Reader, _ int64, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.files[p] = string(data)
	return nil
}
func (f *extFS) Rename(_ context.Context, from, to string) error { return nil }
func (f *extFS) Delete(_ context.Context, p string) error        { return nil }
func (f *extFS) MakeDir(_ context.Context, p string) error       { return nil }

// DirectURL 实现 DirectURLProvider（dlink 非空时支持）。
func (f *extFS) DirectURL(_ context.Context, relPath string) (string, bool, error) {
	if f.dlink == "" {
		return "", false, nil
	}
	if _, ok := f.files[relPath]; !ok {
		return "", true, io.ErrUnexpectedEOF
	}
	return f.dlink, true, nil
}

// OpenRangeRead 实现 RangeReader（本地内容裁切，测试 RangeSeeker 消费）。
func (f *extFS) OpenRangeRead(_ context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	content, ok := f.files[p]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	if offset < 0 || offset+size > int64(len(content)) {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(strings.NewReader(content[offset : offset+size])), nil
}

var _ syncpkg.FS = (*extFS)(nil)

// extBackend 是 ExternalBackend 的测试实现（FS 内嵌）。
type extBackend struct {
	fs *extFS
}

func (b *extBackend) FS() syncpkg.FS { return b.fs }
func (b *extBackend) Close() error   { return nil }

// newExternalTestEnv 构造 Handlers + 装配一个外部卷（AddExternalVolume 直接注入句柄，
// 不查 registry factory——**评审 I3 修复**：此前残留 RegisterBackend 注册，三个 t.Parallel()
// 用例用同名类型并发注册会重复 panic，且该注册本不参与路由）。
func newExternalTestEnv(t *testing.T, v volume.Volume, fs *extFS) *Handlers {
	t.Helper()
	be := &extBackend{fs: fs}

	// 用 registry.NewSet 装配（volumes 空 + AddExternalVolume 注入外部卷）。
	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(v, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	return &Handlers{volSet: vs, logger: testLogger()}
}

// TestResolveExternalDownload_BState302：明文外部卷 direct_link:true → 302 直链。
func TestResolveExternalDownload_BState302(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"user/f.bin": "hello"}, dlink: "https://d.pcs.baidu.com/f.bin?sign=x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "")
	if dp == nil {
		t.Fatal("外部卷命中应返回 downloadPath")
	}
	if dp.redirectURL != "https://d.pcs.baidu.com/f.bin?sign=x" {
		t.Fatalf("B 态 redirectURL=%q want 百度直链", dp.redirectURL)
	}
	if dp.source == nil {
		t.Fatal("B 态应同时携带 source（Stat 回元信息）")
	}
}

// TestResolveExternalDownload_AStateSecretdata：secretdata 卷恒服务端转发（A 态，
// 即便 direct_link 被误配也忽略——密文必须解密）。
func TestResolveExternalDownload_AStateSecretdata(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "secret", Type: volume.TypeSecretdata, DirectLink: true}
	fs := &extFS{files: map[string]string{"user/movie.bin": "ciphertext"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/movie.bin", "")
	if dp == nil {
		t.Fatal("secretdata 命中应返回 downloadPath")
	}
	if dp.redirectURL != "" {
		t.Fatalf("secretdata 恒私密不应有直链，got %q", dp.redirectURL)
	}
	if dp.source == nil {
		t.Fatal("secretdata 应走服务端 source")
	}
}

// TestResolveExternalDownload_CStatePrivate：明文外部卷默认私密（direct_link 未设）
// → 服务端转发（source），无直链。
func TestResolveExternalDownload_CStatePrivate(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "sftp", Type: "sftp"} // DirectLink 零值 false = 私密默认
	fs := &extFS{files: map[string]string{"user/f.bin": "hello"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "")
	if dp == nil {
		t.Fatal("外部卷命中应返回 downloadPath")
	}
	if dp.redirectURL != "" {
		t.Fatalf("私密卷不应有直链，got %q", dp.redirectURL)
	}
	if dp.source == nil {
		t.Fatal("私密卷应走服务端 source")
	}
}

// TestResolveExternalDownload_DLinkUnavailableFallsBack：明文外部卷直链不可得
// （无会话 dlink 空）→ 回落服务端转发（graceful，不半截 302）。
func TestResolveExternalDownload_DLinkUnavailableFallsBack(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"user/f.bin": "hello"}} // dlink 空 = 无直链会话
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "")
	if dp == nil {
		t.Fatal("外部卷命中应返回 downloadPath")
	}
	if dp.redirectURL != "" {
		t.Fatalf("直链不可得应回落无直链，got %q", dp.redirectURL)
	}
	if dp.source == nil {
		t.Fatal("回落应携带 source")
	}
}

// TestResolveExternalDownload_Missing：外部卷无此文件 → nil（调用方 404）。
func TestResolveExternalDownload_Missing(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"user/other.bin": "x"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/missing.bin", "")
	if dp != nil {
		t.Fatalf("缺失文件应 nil，got %+v", dp)
	}
}
