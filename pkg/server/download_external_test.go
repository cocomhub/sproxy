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
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/storage"
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
//
// 额外装配 tenants 缓存（写路径 resolveWritePath 经 tenantOf→UserRel 映射需要；
// 用 t.TempDir 默认卷根，与 volumes_e2e 测试同构）。
func newExternalTestEnv(t *testing.T, v volume.Volume, fs *extFS) *Handlers {
	t.Helper()
	be := &extBackend{fs: fs}

	// 用 registry.NewSet 装配（本地默认卷 main + AddExternalVolume 注入外部卷）。
	// **默认卷注入（评审 I5 修正）**：此前 NewSet 全 nil，AddExternalVolume 使唯一外部卷
	// 成为 volumes[0] → Default()=外部卷名，volumeTenant 对 `volName==Default().Name`
	// 走本地默认卷分支（tenantFor），downloadPathForRemote 的外部卷分支被本地分支吞掉。
	// 注入本地默认卷 main（roots 同步）使外部卷非默认，语义与生产装配一致。
	root, rerr := storage.OpenRoot(t.TempDir())
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	vs := registry.NewSet(
		[]volume.Volume{{Name: "main"}},
		map[string]*storage.Root{"main": root},
		nil, nil, "main",
	)
	if aerr := vs.AddExternalVolume(v, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	tenants := storage.NewTenantCache(root)
	t.Cleanup(func() { _ = tenants.Close() })
	t.Cleanup(func() { _ = root.Close() })
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(&Config{})
	return &Handlers{volSet: vs, logger: testLogger(), tenants: tenants, cfgPtr: &cfgPtr,
		checksumStores: make(map[string]*checksum.ChecksumStore)}
}

// TestResolveExternalDownload_BState302：明文外部卷 direct_link:true → 302 直链。
func TestResolveExternalDownload_BState302(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}, dlink: "https://d.pcs.baidu.com/f.bin?sign=x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "f.bin", "")
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

// TestResolveExternalDownload_EgressForwardForcesA：`egress_forward=1` 强制 A 态
// （集群出口 302 环打破——B 态 DirectURL 返回带 egress_forward 的出口 URL，二次进入
// 必须走服务端转发）。只收紧（强制转发），无提权面。
func TestResolveExternalDownload_EgressForwardForcesA(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "eg", Type: clustercred.TypeEgress, DirectLink: true, Extra: map[string]any{
		"holder_owner": "alice",
	}}
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}, dlink: "https://eg.example.com/download?egress_forward=1"}
	h := newExternalTestEnv(t, v, fs)

	// 无 egress_forward → B 态（302 直链）。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "f.bin", "")
	if dp == nil || dp.redirectURL == "" {
		t.Fatal("无 egress_forward 应 B 态 302")
	}
	// 带 egress_forward=1 → 强制 A 态（source，无 redirectURL——环打破）。
	req := httptest.NewRequest(http.MethodGet, "/download?egress_forward=1", nil)
	dp2 := h.resolveExternalDownload(req, "alice", "user/f.bin", "f.bin", "")
	if dp2 == nil {
		t.Fatal("egress_forward 命中应返回 downloadPath")
	}
	if dp2.redirectURL != "" {
		t.Fatalf("egress_forward 应强制 A 态（无 redirectURL），got %q", dp2.redirectURL)
	}
	if dp2.source == nil {
		t.Fatal("egress_forward 应携带 source（服务端转发）")
	}
}

// TestResolveExternalDownload_AStateSecretdata：secretdata 卷恒服务端转发（A 态，
// 即便 direct_link 被误配也忽略——密文必须解密）。
func TestResolveExternalDownload_AStateSecretdata(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "secret", Type: volume.TypeSecretdata, DirectLink: true}
	fs := &extFS{files: map[string]string{"alice/user/movie.bin": "ciphertext"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/movie.bin", "movie.bin", "")
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
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "f.bin", "")
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
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}} // dlink 空 = 无直链会话
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "f.bin", "")
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

// TestResolveExternalDownload_EgressOwnerIsolation（评审 I2 回归）：egress 卷代表凭证授
// 的持有侧 owner——本地请求 owner ≠ holder_owner → 不命中（404，跨 owner 越权读堵死）。
func TestResolveExternalDownload_EgressOwnerIsolation(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "eg", Type: clustercred.TypeEgress, DirectLink: true, Extra: map[string]any{
		"holder_owner": "alice", // 凭证授的持有侧 owner（装配必填）
	}}
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}}
	h := newExternalTestEnv(t, v, fs)

	// bob 请求 → 不命中（owner 与 holder_owner 不匹配，防跨 owner 读 alice 凭证数据）。
	if dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		"bob", "user/f.bin", "f.bin", ""); dp != nil {
		t.Fatalf("bob 请求 egress 卷应不命中（跨 owner 越权面）, got %+v", dp)
	}
	// alice（持有 owner）请求 → 命中（服务端转发）。
	dp2 := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		"alice", "user/f.bin", "f.bin", "")
	if dp2 == nil || dp2.source == nil {
		t.Fatal("alice（holder_owner）请求应命中 source")
	}
}

// TestDownloadPathForRemote_ExternalSecretdata（评审 I5 回归）：持有节点数据是 secretdata
// 等外部卷时，remote 面 downloadPathForRemote 走外部卷分支——构造 Source 服务端解密转发
// （此前该分支零测试，全链路未验证）。此处用 extFS 模拟 secretdata 密文卷，断言路由
// 分支真实产出 Source 且 OpenPath 可读回内容。
func TestDownloadPathForRemote_ExternalSecretdata(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "secret", Type: volume.TypeSecretdata}
	fs := &extFS{files: map[string]string{"alice/user/docs/movie.bin": "ciphertext"}}
	h := newExternalTestEnv(t, v, fs)

	dp, err := h.downloadPathForRemote(context.Background(), "alice", "secret", "docs/movie.bin")
	if err != nil {
		t.Fatalf("downloadPathForRemote(外部卷): %v", err)
	}
	if dp.Source == nil {
		t.Fatal("secretdata 持有者应走 Source（服务端解密转发）分支")
	}
	if dp.VolumeName != "secret" || dp.Rel != "alice/user/docs/movie.bin" {
		t.Fatalf("DownloadPath 卷/键错: vol=%q rel=%q", dp.VolumeName, dp.Rel)
	}
	// Source 分支 OpenPath 真实可读（#735 解密转发语义）。
	rc, oerr := h.fileService().OpenPath(context.Background(), dp)
	if oerr != nil {
		t.Fatalf("OpenPath(Source 分支): %v", oerr)
	}
	defer rc.File.Close()
	body, _ := io.ReadAll(rc.File)
	if string(body) != "ciphertext" {
		t.Fatalf("Source 读取内容 != 持有侧原件, got %q", string(body))
	}
}

// TestResolveExternalDownload_Missing：外部卷无此文件 → nil（调用方 404）。
func TestResolveExternalDownload_Missing(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"alice/user/other.bin": "x"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/missing.bin", "missing.bin", "")
	if dp != nil {
		t.Fatalf("缺失文件应 nil，got %+v", dp)
	}
}

// TestResolveExternalDownload_OwnerIsolation（评审 M3 回归）：owner 键隔离——
// bob 读 alice 的文件（alice/user/f.bin）→ 不命中（ACL 默认开放时也防跨 owner）。
func TestResolveExternalDownload_OwnerIsolation(t *testing.T) {
	t.Parallel()
	v := volume.Volume{Name: "baidu", Type: "baidupcs", DirectLink: true}
	fs := &extFS{files: map[string]string{"alice/user/f.bin": "hello"}, dlink: "https://d.example/x"}
	h := newExternalTestEnv(t, v, fs)

	// bob 请求 filename=f.bin → UserRel → user/f.bin → ownerKey=bob/user/f.bin（不存在）→ nil。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "bob", "user/f.bin", "f.bin", "")
	if dp != nil {
		t.Fatalf("bob 读 alice 文件应不命中（owner 键隔离），got %+v", dp)
	}
	// alice 自己可读。
	dp2 := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil), "alice", "user/f.bin", "f.bin", "")
	if dp2 == nil {
		t.Fatal("alice 读自己文件应命中（alice/user/f.bin）")
	}
}
