// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_playback_test.go 验证通用文件获取三态的真机端到端（2026-10-05）：
//
// 用户要求的两个完整播放链路，均经**真 sproxy 二进制 + 真 HTTP /download Range**：
//
//  1. **普通上传加密卷后播放**：sclient upload --volume vault（secretdata 卷自动加密）
//     → 播放器 GET /download?filename=...&volume=vault + Range 头 → 服务端解密转发
//     → 206 + Content-Range + 明文段 == 原文件段（Range 随机访问播放）。
//  2. **下载转存加密卷后播放**：cloud-download submit --transfer-volume vault（下载后
//     自动转存到 secretdata 卷加密）→ GET /download?filename=<转存rel>&volume=vault +
//     Range → 206 + 解密明文段一致（边缓冲边播的转存产物）。
//
// 前置：服务器装配 secrets + secretdata 卷（secret_url 指向密钥文件，装配即 boot 证明）。
package sproxy_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rangeRequest 用签名 client 发带 Range 头的 GET（播放器语义：seek 到 N-M 只取该段），
// 返回 status/headers/body。复用 authedHTTPClient（signingTransport 签名 SproxySig）。
func rangeRequest(t *testing.T, url, rangeHdr string) (int, http.Header, []byte) {
	t.Helper()
	req, rerr := http.NewRequest(http.MethodGet, url, nil)
	if rerr != nil {
		t.Fatalf("构造 Range GET %s: %v", url, rerr)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, derr := authedHTTPClient.Do(req)
	if derr != nil {
		t.Fatalf("Range GET %s: %v", url, derr)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, cerr := buf.ReadFrom(resp.Body); cerr != nil {
		t.Fatalf("Range GET %s 读响应体失败: %v", url, cerr)
	}
	return resp.StatusCode, resp.Header, buf.Bytes()
}

// newSecretdataPlaybackEnv 装配 secrets + secretdata 卷（secret_url 指向预写密钥文件），
// 返回 env + vault 物理根（磁盘断言用）。
func newSecretdataPlaybackEnv(t *testing.T) (*cliEnv, string) {
	t.Helper()
	keysRoot := filepath.Join(t.TempDir(), "keys")
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	secret := seedSecretKey(t, keysRoot)
	if len(secret) != 32 {
		t.Fatalf("密钥应 32B, got %d", len(secret))
	}
	extraConfig := fmt.Sprintf(`volumes:
  - name: main
  - name: mykeys
    type: secrets
    extra:
      target: local
      root: %q
  - name: vault
    type: secretdata
    extra:
      target: local
      root: %q
      secret_url: secrets://mykeys/datakey
`, keysRoot, vaultRoot)
	return startCLIEnv(t, extraConfig), vaultRoot
}

// TestE2E_CLI_Playback_UploadToSecretdataVolume（用户要求链路 1）：
// 普通上传加密卷 → /download Range 播放。sclient upload --volume vault → secretdata
// 卷自动加密落盘 → 播放器 Range 请求 206 + 解密明文段一致。
func TestE2E_CLI_Playback_UploadToSecretdataVolume(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	// 1) 生成内容（二进制任意，跨多块：64KiB+；Range 播放语义与视频一致）。
	content := make([]byte, 96*1024)
	for i := range content {
		content[i] = byte(i % 251)
	}
	file := filepath.Join(env.TmpDir, "movie.bin")
	if werr := os.WriteFile(file, content, 0o600); werr != nil {
		t.Fatalf("写测试文件: %v", werr)
	}

	// 2) 普通上传到 vault 卷（--volume 限定目标卷 → routeUpload 走外部卷 sink；
	// 相对文件名：sclient 已 cd 到 env.TmpDir，避免绝对路径被当远端路径）。
	env.sclient(t, env.TmpDir, "--volume", "vault", "upload", "movie.bin")

	// 3) 磁盘副作用：vault 卷底层是加密容器（无明文逻辑名 movie.bin、无明文内容）。
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "movie.bin", "user/movie.bin")

	// 4) 播放器 Range 请求（seek 中间段）：206 + Content-Range + 解密明文段 == 原文件。
	url := env.BaseURL + "/download?filename=movie.bin&volume=vault"
	status, hdr, body := rangeRequest(t, url, "bytes=4096-8191")
	if status != http.StatusPartialContent {
		t.Fatalf("加密卷 Range 应 206, got %d: %s", status, body)
	}
	if cr := hdr.Get("Content-Range"); cr != "bytes 4096-8191/98304" {
		t.Fatalf("Content-Range=%q want bytes 4096-8191/98304", cr)
	}
	if !bytes.Equal(body, content[4096:8192]) {
		t.Fatalf("解密 Range 段 != 原文件段（len=%d）", len(body))
	}

	// 5) 顺序播放（从头 Range 到尾）：分段请求逐段解密一致（播放器边缓冲边播语义）。
	off := int64(0)
	for off < int64(len(content)) {
		end := off + 16384
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		status, _, seg := rangeRequest(t, url, fmt.Sprintf("bytes=%d-%d", off, end-1))
		if status != http.StatusPartialContent {
			t.Fatalf("顺序段 [%d,%d) 应 206, got %d", off, end, status)
		}
		if !bytes.Equal(seg, content[off:end]) {
			t.Fatalf("顺序段 [%d,%d) 解密不一致", off, end)
		}
		off = end
	}
}

// TestE2E_CLI_Playback_CloudTransferToSecretdataVolume（用户要求链路 2）：
// 云下载 → 转存 secretdata 卷（自动加密）→ /download Range 播放。
func TestE2E_CLI_Playback_CloudTransferToSecretdataVolume(t *testing.T) {
	t.Parallel()
	env, vaultRoot := newSecretdataPlaybackEnv(t)

	// 1) 源内容（httptest 提供；>64KiB 跨多块 + 关键帧分块更贴近真实视频场景）。
	content := make([]byte, 160*1024)
	for i := range content {
		content[i] = byte((i * 7) % 251)
	}
	src := newPlaybackSrcServer(t, content)

	// 2) 云下载 + 转存到 vault 卷（--transfer-volume：下载完成后自动转存加密）。
	out := env.sclient(t, env.TmpDir, "cloud-download", "submit",
		"--transfer-volume", "vault", src.URL+"/video.bin")
	if !strings.Contains(out, "video.bin") {
		t.Fatalf("submit 输出应含 video.bin, got:\n%s", out)
	}
	tid := onlyCloudTaskID(t, env)
	env.sclient(t, env.TmpDir, "cloud-download", "wait", tid, "--timeout", "2m")

	// 3) 播放器访问转存产物：用户传相对路径 <taskID>/<file>（不感知 owner 前缀——
	// 读路径 resolveExternalDownload 经 v.ResolveUserPath 自动加前缀命中；vault 是
	// 共享卷 → 键 = <owner>/user/<taskID>/<file>）。
	url := env.BaseURL + "/download?filename=" + tid + "/video.bin&volume=vault"
	status, hdr, body := rangeRequest(t, url, "bytes=8192-12287")
	if status != http.StatusPartialContent {
		t.Fatalf("转存加密卷 Range 应 206, got %d: %s", status, body)
	}
	if want := fmt.Sprintf("bytes 8192-12287/%d", len(content)); hdr.Get("Content-Range") != want {
		t.Fatalf("Content-Range=%q want %q", hdr.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, content[8192:12288]) {
		t.Fatalf("转存解密 Range 段 != 原文件段（len=%d）", len(body))
	}

	// 4) 磁盘断言：vault 卷底层加密容器存在（转存确已加密落盘，非明文）。
	//    键 = <owner>/user/<taskID>/video.bin（共享卷自动 owner 前缀）。
	assertSecretLayoutAnonymity(t, vaultRoot, "user", "video.bin", "user/video.bin")
}

// newPlaybackSrcServer 起 httptest 供下载源（云下载任务拉取）。
func newPlaybackSrcServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

var _ = io.Discard
