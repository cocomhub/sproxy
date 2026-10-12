// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_egress_meta_e2e_test.go 验证**集群出口卷的真实完整性校验**（评审 P2-10 修复）：
// 持有节点（真 remote_read listener + sidecar）→ 出口节点（clusterFS 自带 meta.Provider
// 经 remote meta 面取 sidecar）→ A 态服务端转发的下载流被逐分块校验。
//
// 回归锚点：修复前 clusterFS 不自带 meta，被 trusted.Wrap 包成 TrustedVolumeFS，后者用
// `inner.OpenRead(meta.MetaPath(rel))` 取 sidecar → 出口侧键空间（`<owner>/meta/...`）与
// 持有侧期望的用户相对路径错位 → 持有侧恒 404 → **egress 下载静默零校验**。

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/ext/cluster"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// writeBSidecarInServer 在持有侧卷写 sidecar（`<owner>/meta/<rel>.meta`）——与写路径同构。
// 独立 helper（不改动 startRemoteReadDualEnd 的既有返回签名）。
func writeBSidecarInServer(t *testing.T, cfg *Config, rel string, body []byte) {
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
	abs := filepath.Join(cfg.StorageRoot, testDualOwner, filepath.FromSlash(meta.MetaPath("user/"+rel)))
	if mkErr := os.MkdirAll(filepath.Dir(abs), 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if werr := os.WriteFile(abs, raw, 0o644); werr != nil {
		t.Fatal(werr)
	}
}

// newEgressHandlers 装配出口侧 Handlers（egress 卷挂在 holder 上，A 态服务端转发）。
func newEgressHandlers(t *testing.T, aID *tunnel.Identity, bFP string, lnAddr string) *Handlers {
	t.Helper()
	egVol := volume.Volume{Name: "eg1", Type: clustercred.TypeEgress, Extra: map[string]any{
		"holder_node": testReaderNodeA, "holder_volume": testDualVol,
		"holder_fingerprint": bFP, "holder_owner": testDualOwner,
	}}
	be, err := cluster.NewBackend(context.Background(), egVol,
		remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", lnAddr)
		}), aID)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(egVol, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	return &Handlers{volSet: vs, logger: testLogger()}
}

// readEgress 经出口侧服务端转发读取整文件（返回内容与读错误）。
func readEgress(t *testing.T, h *Handlers, name string) ([]byte, error) {
	t.Helper()
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		testDualOwner, "user/docs/verified.bin", name, "")
	if dp == nil || dp.source == nil {
		t.Fatal("出口侧 egress 卷应命中 A 态 source")
	}
	svc := h.fileService()
	rc, oerr := svc.OpenPath(context.Background(), files.DownloadPath{
		Source: dp.source, Filename: name, Rel: testDualOwner + "/user/docs/verified.bin",
	})
	if oerr != nil {
		return nil, oerr
	}
	defer func() { _ = rc.File.Close() }()
	return io.ReadAll(rc.File)
}

// TestClusterEgress_MetaVerifiedDownload：出口卷下载必须**真实**逐分块校验——内容一致
// → 通过；持有侧内容被篡改（同 size、不同字节）→ 下发流 fail-closed（校验未通过不默许）。
func TestClusterEgress_MetaVerifiedDownload(t *testing.T) {
	t.Parallel()
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)
	body := bytes.Repeat([]byte("verified-egress-payload-"), 500)
	writeBFileInServer(t, cfg, "docs/verified.bin", body)
	writeBSidecarInServer(t, cfg, "docs/verified.bin", body)

	h := newEgressHandlers(t, aID, bFP, ln.Addr())

	// 1. meta 能力确实注入（修复前恒 nil = 零校验回归）。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		testDualOwner, "user/docs/verified.bin", "verified.bin", "")
	src, ok := dp.source.(*externalDownloadSource)
	if !ok || src.fileMeta == nil {
		t.Fatal("egress 下载源必须注入 FileMeta 读取器（否则零校验）")
	}

	// 2. 内容与 sidecar 一致 → 可读且全等。
	got, rerr := readEgress(t, h, "verified.bin")
	if rerr != nil {
		t.Fatalf("校验通过的内容应可读: %v", rerr)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("出口内容 != 持有侧原件")
	}

	// 3. 篡改持有侧内容（同 size、不同字节）→ 校验必须失败（fail-closed）。
	tampered := append([]byte(nil), body...)
	tampered[10] ^= 0xff
	writeBFileInServer(t, cfg, "docs/verified.bin", tampered)
	if _, terr := readEgress(t, h, "verified.bin"); terr == nil {
		t.Fatal("持有侧内容被篡改必须校验失败（否则 = 零校验）")
	}
}

// TestClusterEgress_MetaMissing_DegradesNotSilently：持有侧无 sidecar（存量/旁路写）→
// 出口侧按「无校验能力」降级（可读，不误报损坏）。**这是既有外部卷口径**——本用例把它
// 钉住，防止为修 P2-10 而改成「无 sidecar 即拒绝」（存量数据全不可读）。
func TestClusterEgress_MetaMissing_DegradesNotSilently(t *testing.T) {
	t.Parallel()
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)
	body := bytes.Repeat([]byte("no-sidecar-payload-"), 200)
	writeBFileInServer(t, cfg, "docs/verified.bin", body) // 不写 sidecar

	h := newEgressHandlers(t, aID, bFP, ln.Addr())

	got, rerr := readEgress(t, h, "verified.bin")
	if rerr != nil {
		t.Fatalf("无 sidecar 应为降级可读（既有外部卷口径）: %v", rerr)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("降级读取内容 != 持有侧原件")
	}
}

// TestEgressMetaCapabilityNotShadowed：出口卷 FS 经装配链后仍带 meta.Provider——
// 回归锚点：clusterFS 自带 meta 时 trusted.Wrap **不得**再套 TrustedVolumeFS（后者会
// 用 meta.MetaPath 直取，造成 P2-10 的键空间错位）。Guard 必须透传该能力。
func TestEgressMetaCapabilityNotShadowed(t *testing.T) {
	t.Parallel()
	_, aID, bFP, ln := startRemoteReadDualEnd(t)
	egVol := volume.Volume{Name: "eg1", Type: clustercred.TypeEgress, Extra: map[string]any{
		"holder_node": testReaderNodeA, "holder_volume": testDualVol,
		"holder_fingerprint": bFP, "holder_owner": testDualOwner,
	}}
	be, err := cluster.NewBackend(context.Background(), egVol,
		remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr())
		}), aID)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer func() { _ = be.Close() }()

	raw := be.FS()
	if _, isProvider := raw.(meta.Provider); !isProvider {
		t.Fatal("ergress 卷 FS 必须自带 meta.Provider")
	}
	wrapped := trusted.Wrap(raw, trusted.Options{})
	if wrapped != raw {
		t.Fatalf("自带 meta.Provider 的卷不得被 trusted.Wrap 再包一层（got %T）", wrapped)
	}
	if _, isProvider := any(trusted.Guard(wrapped)).(meta.Provider); !isProvider {
		t.Fatal("Guard 必须透传 meta.Provider")
	}
}
