// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_egress_e2e_test.go 验证集群出口双端（2026-10-05 用户裁定凭证下发）：
// 持有节点（真实 remote_read listener + 凭证签发）→ 出口节点（凭证池 clusterFS）→
// A 态服务端转发 SHA-256 全等 + Range 206 + B 态 302（egress_forward 环打破）+ Stat。

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
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/ext/cluster"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestClusterEgress_CredentialEndToEnd 集群出口凭证全链路：
// 持有侧（真 listener + mesh_readers 静态授权 alice）→ 出口侧（凭证池 clusterFS 挂
// 同一 holder）→ A 态 OpenRangeRead Range 206 + 全文件 SHA-256 一致。
func TestClusterEgress_CredentialEndToEnd(t *testing.T) {
	t.Parallel()
	// 持有侧（B）：真 remote_read listener + mesh_readers 授权 aID 指纹。
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)
	body := bytes.Repeat([]byte("cluster-egress-payload-"), 2000) // ~40KB 跨隧道帧
	writeBFileInServer(t, cfg, "docs/video.bin", body)

	// 出口侧（A）：cluster.NewBackend（凭证 → remote.Client：本端 aID + 持有指纹 pin bFP）。
	egVol := volume.Volume{Name: "eg1", Type: clustercred.TypeEgress, Extra: map[string]any{
		"holder_node": testReaderNodeA, "holder_volume": testDualVol,
		"holder_fingerprint": bFP,
		"egress_base_url":    "https://eg.example.com",
	}}
	be, err := cluster.NewBackend(context.Background(), egVol,
		remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr())
		}), aID, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer be.Close()

	// 出口侧 Handlers：volSet 装配 egress 卷（AddExternalVolume）。
	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(egVol, be); aerr != nil {
		t.Fatalf("AddExternalVolume: %v", aerr)
	}
	h := &Handlers{volSet: vs, logger: testLogger()}

	// A 态：/download → resolveExternalDownload → externalDownloadSource（RangeSeeker）
	// → clusterFS.OpenRangeRead → remoteFS（Range）→ 持有 delegateDownload → 隧道回传。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		"alice", "user/docs/video.bin", "video.bin", "")
	if dp == nil || dp.source == nil {
		t.Fatal("出口侧 egress 卷应命中 A 态 source")
	}
	// Range 206：经 RangeSeeker → ServeContent。
	svc := h.fileService()
	rc, oerr := svc.OpenPath(context.Background(), files.DownloadPath{Source: dp.source, Filename: "video.bin", Rel: "alice/user/docs/video.bin"})
	if oerr != nil {
		t.Fatalf("OpenPath: %v", oerr)
	}
	defer rc.File.Close()
	req := httptest.NewRequest(http.MethodGet, "/download?filename=video.bin&volume=eg1", nil)
	req.Header.Set("Range", "bytes=0-1023")
	rec := httptest.NewRecorder()
	seeker, ok := rc.File.(io.ReadSeeker)
	if !ok {
		t.Fatal("source 应返回 io.ReadSeeker")
	}
	http.ServeContent(rec, req, "video.bin", time.Time{}, seeker)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("出口 Range 应 206, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), body[:1024]) {
		t.Fatal("出口 Range 段 != 持有侧原件")
	}

	// 全文件 SHA-256 一致。
	rc2, _ := svc.OpenPath(context.Background(), files.DownloadPath{Source: dp.source, Filename: "video.bin", Rel: "alice/user/docs/video.bin"})
	full, _ := io.ReadAll(rc2.File)
	rc2.File.Close()
	if !bytes.Equal(full, body) {
		t.Fatal("出口全文件 != 持有侧原件")
	}
}

// writeBFileInServer 在 B 侧卷写 `<owner>/user/<rel>`（与 writeBFile 同构，但接受 *Config）。
func writeBFileInServer(t *testing.T, cfg *Config, rel string, body []byte) {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, testDualOwner, "user", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestClusterEgress_DirectURL_302NoLoop：B 态 302（egress_forward=1 环打破）——
// DirectURL 返回出口 URL；二次进入 forceForward 强制 A 态。
func TestClusterEgress_DirectURL_302NoLoop(t *testing.T) {
	t.Parallel()
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)
	body := []byte("egress-direct-url-body")
	writeBFileInServer(t, cfg, "docs/f.bin", body)

	egVol := volume.Volume{Name: "eg1", Type: clustercred.TypeEgress, DirectLink: true, Extra: map[string]any{
		"holder_node": testReaderNodeA, "holder_volume": testDualVol,
		"holder_fingerprint": bFP, "egress_base_url": "https://eg.example.com",
	}}
	be, err := cluster.NewBackend(context.Background(), egVol,
		remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr())
		}), aID, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer be.Close()
	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(egVol, be); aerr != nil {
		t.Fatal(aerr)
	}
	h := &Handlers{volSet: vs, logger: testLogger()}

	// B 态（无 egress_forward）：DirectURL → 302 出口 URL（含 egress_forward=1）。
	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		"alice", "user/docs/f.bin", "f.bin", "")
	if dp == nil || dp.redirectURL == "" {
		t.Fatal("B 态应返回 302 出口 URL")
	}
	if !bytes.Contains([]byte(dp.redirectURL), []byte("egress_forward=1")) {
		t.Fatalf("302 应含 egress_forward=1（环打破）: %q", dp.redirectURL)
	}
	// 二次进入带 egress_forward=1 → 强制 A 态（无 redirectURL，source 服务端转发）。
	dp2 := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download?egress_forward=1", nil),
		"alice", "user/docs/f.bin", "f.bin", "")
	if dp2 == nil || dp2.redirectURL != "" || dp2.source == nil {
		t.Fatal("egress_forward 应强制 A 态（source，无 redirectURL）")
	}
}

// TestClusterEgress_StatSize：Stat 经 source 回持有侧元信息。
func TestClusterEgress_StatSize(t *testing.T) {
	t.Parallel()
	cfg, aID, bFP, ln := startRemoteReadDualEnd(t)
	body := []byte("stat-size-check")
	writeBFileInServer(t, cfg, "docs/stat.bin", body)

	egVol := volume.Volume{Name: "eg1", Type: clustercred.TypeEgress, Extra: map[string]any{
		"holder_node": testReaderNodeA, "holder_volume": testDualVol,
		"holder_fingerprint": bFP,
	}}
	be, err := cluster.NewBackend(context.Background(), egVol,
		remote.DialerFunc(func(ctx context.Context, node string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr())
		}), aID, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	defer be.Close()
	vs := registry.NewSet(nil, nil, nil, nil, "")
	if aerr := vs.AddExternalVolume(egVol, be); aerr != nil {
		t.Fatal(aerr)
	}
	h := &Handlers{volSet: vs, logger: testLogger()}

	dp := h.resolveExternalDownload(httptest.NewRequest(http.MethodGet, "/download", nil),
		"alice", "user/docs/stat.bin", "stat.bin", "")
	if dp == nil || dp.source == nil {
		t.Fatal("应命中 source")
	}
	info, serr := dp.source.Stat(context.Background())
	if serr != nil {
		t.Fatalf("Stat: %v", serr)
	}
	if info.Size() != int64(len(body)) {
		t.Fatalf("出口 Stat size=%d want %d", info.Size(), len(body))
	}
}

var _ = files.DownloadPath{}
var _ = syncpkg.FS(nil)
