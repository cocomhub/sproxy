// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// meta_remote_test.go 单测持有侧 meta 面（op=meta）的两个分支：本地卷读 meta 桶、
// 外部卷经其 meta.Provider；以及错误分类（真缺失 → 404，读取失败 → 非 404）。
//
// 端到端（出口侧真校验）见 cluster_egress_meta_e2e_test.go。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// buildSidecar 用与写路径同一 Calculator 造 sidecar 原始 JSON。
func buildSidecar(t *testing.T, body []byte) []byte {
	t.Helper()
	calc, err := meta.NewCalculator(int64(len(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := calc.ReadFrom(bytes.NewReader(body)); werr != nil {
		t.Fatal(werr)
	}
	raw, merr := meta.Marshal(calc.Finish())
	if merr != nil {
		t.Fatal(merr)
	}
	return raw
}

// errProviderFS 是「自带 meta.Provider 但读取失败」的外部卷 FS（读取失败分类用例）。
type errProviderFS struct {
	*extFS
	err error
}

func (f *errProviderFS) FileMeta(context.Context, string) (*meta.FileMeta, error) { return nil, f.err }

var _ meta.Provider = (*errProviderFS)(nil)

// missingFS 是「读不存在键回 os.ErrNotExist」的最小 FS（真实外部 FS 口径——
// 与 extFS.OpenRead 已统一为 os.ErrNotExist），用于验证真缺失 → 404。
type missingFS struct{ *extFS }

func (f *missingFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	if c, ok := f.files[p]; ok {
		return io.NopCloser(strings.NewReader(c)), nil
	}
	return nil, os.ErrNotExist
}

// fsBackend 是任意 FS 的 ExternalBackend 测试实现。
type fsBackend struct{ fs syncpkg.FS }

func (b *fsBackend) FS() syncpkg.FS { return b.fs }
func (b *fsBackend) Close() error   { return nil }

// newExternalEnvWithFS 装配「本地默认卷 main + 给定 FS 的外部卷」的 Handlers。
func newExternalEnvWithFS(t *testing.T, volName string, fs syncpkg.FS) *Handlers {
	t.Helper()
	root, rerr := storage.OpenRoot(t.TempDir())
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	t.Cleanup(func() { _ = root.Close() })
	vs := registry.NewSet(
		[]volume.Volume{{Name: "main"}},
		map[string]*storage.Root{"main": root},
		nil, nil, "main",
	)
	if aerr := vs.AddExternalVolume(volume.Volume{Name: volName, Type: "baidupcs"}, &fsBackend{fs: fs}); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(&Config{})
	tenants := storage.NewTenantCache(root)
	t.Cleanup(func() { _ = tenants.Close() })
	return &Handlers{volSet: vs, logger: testLogger(), cfgPtr: &cfgPtr, tenants: tenants}
}

// assertNotFound404 断言错误是 404 HTTPError。
func assertNotFound404(t *testing.T, err error) {
	t.Helper()
	he, ok := errors.AsType[*files.HTTPError](err)
	if !ok || he.Status != http.StatusNotFound {
		t.Fatalf("应回 404 HTTPError, got %v", err)
	}
}

// TestReadMetaForRemote_LocalTenantBranch：本地卷 → 读 `meta/<rel>.meta`（与
// filesMetaPolicy.VerifyDownload 同一键）；无 sidecar → 404；非法路径 → 404。
func TestReadMetaForRemote_LocalTenantBranch(t *testing.T) {
	t.Parallel()
	cfg := remoteReadTestConfig(t)
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})

	raw := buildSidecar(t, []byte("local meta body"))
	abs := filepath.Join(cfg.StorageRoot, testReaderOwner, filepath.FromSlash(meta.MetaPath("user/docs/f.bin")))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(abs, raw, 0o644); werr != nil {
		t.Fatal(werr)
	}

	got, err := h.readMetaForRemote(context.Background(), testReaderOwner, "main", "docs/f.bin")
	if err != nil {
		t.Fatalf("readMetaForRemote: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("sidecar 字节往返失真: got %d bytes want %d", len(got), len(raw))
	}

	// 无 sidecar → 404（预期降级，非错误）。
	_, nerr := h.readMetaForRemote(context.Background(), testReaderOwner, "main", "docs/missing.bin")
	assertNotFound404(t, nerr)
	// 非法路径 → 404（不泄露存在性）。
	if _, perr := h.readMetaForRemote(context.Background(), testReaderOwner, "main", "../etc/passwd"); !isNotFoundErr(perr) {
		t.Fatalf("非法路径应 404, got %v", perr)
	}
}

// isNotFoundErr 判定错误是否为 404。
func isNotFoundErr(err error) bool {
	he, ok := errors.AsType[*files.HTTPError](err)
	return ok && he.Status == http.StatusNotFound
}

// TestReadMetaForRemote_ExternalVolumeProviderBranch：外部卷 → 经 meta.Provider
// （trusted.Wrap 装饰器读该 FS 的键空间 `<owner>/meta/<rel>.meta`）；无 sidecar → 404。
func TestReadMetaForRemote_ExternalVolumeProviderBranch(t *testing.T) {
	t.Parallel()
	raw := buildSidecar(t, []byte("external meta body"))
	fs := &missingFS{extFS: &extFS{files: map[string]string{"alice/meta/f.bin.meta": string(raw)}}}
	h := newExternalEnvWithFS(t, "baidu", fs)

	got, err := h.readMetaForRemote(context.Background(), "alice", "baidu", "f.bin")
	if err != nil {
		t.Fatalf("readMetaForRemote(外部卷): %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("外部卷 sidecar 往返失真: %q", got)
	}
	// 无 sidecar → 404。
	if _, nerr := h.readMetaForRemote(context.Background(), "alice", "baidu", "nope.bin"); !isNotFoundErr(nerr) {
		t.Fatalf("外部卷无 sidecar 应 404, got %v", nerr)
	}
}

// TestReadMetaForRemote_ProviderReadFailureNot404：Provider 读取失败（非 ErrNotExist）→
// 原样传播（远程面 500）——出口侧据此归类为「读取失败」告警，不静默当无校验能力。
func TestReadMetaForRemote_ProviderReadFailureNot404(t *testing.T) {
	t.Parallel()
	failFS := &errProviderFS{extFS: &extFS{files: map[string]string{}}, err: io.ErrUnexpectedEOF}
	h := newExternalEnvWithFS(t, "baidu", failFS)

	_, err := h.readMetaForRemote(context.Background(), "alice", "baidu", "f.bin")
	if err == nil {
		t.Fatal("Provider 读取失败应报错")
	}
	if isNotFoundErr(err) {
		t.Fatalf("读取失败不得伪装 404（会静默降级为无校验）, got %v", err)
	}
}
