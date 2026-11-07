// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// TestAuthAndTunnelOptions_InjectsDownloadProxy 验证 buildBaseOptions 注入
// WithDownloadProxy（cfg.DownloadProxy 非空时）。红灯：当前 authAndTunnelOptions
// 未注入 → opts 应用后 downloadProxy 为空 → 直连失败不触发代理。
func TestAuthAndTunnelOptions_InjectsDownloadProxy(t *testing.T) {
	t.Parallel()
	// 代理 server：正常返回内容
	var proxyHits atomic.Int64
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		sum := sha256.Sum256([]byte("proxied-content"))
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		_, _ = io.WriteString(w, "proxied-content")
	}))
	t.Cleanup(proxySrv.Close)

	// 直连 server：hijack 后立即断连 → 网络类错误
	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(directSrv.Close)

	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("download-proxy", "", "")
	cfg := client.DefaultConfig()
	cfg.ServerURL = directSrv.URL
	cfg.DownloadProxy = proxySrv.URL

	f := &factory{}
	fc, err := f.buildClient(cmd, cfg)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}

	// 直连失败（directSrv 断连）→ 应经 cfg.DownloadProxy 代理成功
	out := filepath.Join(t.TempDir(), "got.txt")
	if err := fc.Download(t.Context(), "b.txt", out); err != nil {
		t.Fatalf("Download via injected proxy: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "proxied-content" {
		t.Fatalf("content = %q, want proxied-content", got)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("直连失败时代理应收到请求（WithDownloadProxy 未注入）")
	}
}

// TestAuthAndTunnelOptions_DownloadProxyFlagOverrides 验证 --download-proxy flag 覆盖 cfg.DownloadProxy。
func TestAuthAndTunnelOptions_DownloadProxyFlagOverrides(t *testing.T) {
	t.Parallel()
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256([]byte("flag-proxied"))
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		_, _ = io.WriteString(w, "flag-proxied")
	}))
	t.Cleanup(proxySrv.Close)
	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", 500)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(directSrv.Close)

	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("download-proxy", "", "")
	// 配置配一个错误的代理（应被 flag 覆盖）
	cfg := client.DefaultConfig()
	cfg.ServerURL = directSrv.URL
	cfg.DownloadProxy = "http://127.0.0.1:1" // 故意错误，flag 应覆盖它
	_ = cmd.Flags().Set("download-proxy", proxySrv.URL)

	f := &factory{}
	fc, err := f.buildClient(cmd, cfg)
	if err != nil {
		t.Fatalf("buildClient: %v", err)
	}
	out := filepath.Join(t.TempDir(), "got2.txt")
	if err := fc.Download(t.Context(), "c.txt", out); err != nil {
		t.Fatalf("flag-override Download: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "flag-proxied" {
		t.Fatalf("content = %q, want flag-proxied", got)
	}
}
