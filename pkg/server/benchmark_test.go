// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
)

// ---- 基准测试辅助函数（接受 testing.TB 以支持 testing.B） ----

// benchTmpfsRoot 是 Linux 上 benchmark 优先使用的内存盘挂载点。
// tmpfs 没有脏页回写路径，写入不会被 vm.dirty_ratio 节流；把 benchmark 的数据写进
// runner 的系统盘则会在脏页累积后被限速到设备真实回写带宽（实测 ~0.15 MB/s），
// 单次 op 从毫秒劣化到秒级并最终撞上 CI job 超时
// （取证见 docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md）。
const benchTmpfsRoot = "/dev/shm"

// benchStorageRoot 返回 benchmark 专用的存储根目录。
//
// Linux 优先 benchTmpfsRoot（tmpfs）；其它平台（Windows/macOS 开发机）回退 tb.TempDir()
// ——那里没有共享 runner 的写竞争。注意本 benchmark 度量的是 handler + 存储层，
// **不含物理盘带宽**（那属于 runner 变量，跨机不可比）。
func benchStorageRoot(tb testing.TB) string {
	tb.Helper()
	if runtime.GOOS != "linux" {
		return tb.TempDir()
	}
	return benchStorageRootAt(tb, benchTmpfsRoot)
}

// benchStorageRootAt 在指定挂载点下建一个 benchmark 专用目录；挂载点不可用时回退 tb.TempDir()。
// 抽成带参形式是为了让选择逻辑可被单测确定性覆盖（不依赖宿主是否真有 /dev/shm）。
func benchStorageRootAt(tb testing.TB, tmpfsRoot string) string {
	tb.Helper()
	if st, err := os.Stat(tmpfsRoot); err == nil && st.IsDir() {
		if dir, err := os.MkdirTemp(tmpfsRoot, "sproxy-bench-*"); err == nil {
			tb.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	return tb.TempDir()
}

// benchServer 创建 benchmark 用的测试服务器：走**生产装配路径** RegisterRoutes（与
// newTestServer 同一入口），而不是手搓 Handlers。
//
// 为什么必须复用装配：手搓副本曾与生产装配长期脱节——缺 globalRoot/tenants/checksumStores
// 使每个上传 400（无效的文件路径）、缺回环无认证兜底使每个请求 401，于是 pkg/server 的
// benchmark 空跑而 CI 依旧绿（取证见 docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md）。
// 存储根走 benchStorageRoot（Linux = tmpfs），避免把 runner 磁盘写带宽带进计时。
func benchServer(tb testing.TB, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	tb.Helper()

	cfg := Default()
	cfg.StorageRoot = benchStorageRoot(tb)
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	noAuth := defaultNoAuthRegOpts()
	// 丢弃式 logger：benchmark 每个 op 都打 INFO/WARN 只会刷爆 CI 日志
	// （实测 pkg/server 段一次刷出 8469 行 WARN）。
	log := testLogger()

	opts := RegisterRoutesOpts{
		Mux:                   http.NewServeMux(),
		CfgPtr:                &cfgPtr,
		Version:               "bench",
		BuildAt:               "bench",
		Logger:                log,
		AuditLogger:           log,
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	}
	h := RegisterRoutes(tb.Context(), opts)

	ts := httptest.NewServer(h.Handler())
	tb.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, &cfgPtr
}

// benchServerWithChunked 创建分块上传 benchmark 用的服务器。
// RegisterRoutes 已挂全部路由（含分块族），故本包装只剩「chunk_size = 1 MiB」这一配置差异。
func benchServerWithChunked(tb testing.TB, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	tb.Helper()
	return benchServer(tb, func(cfg *Config) {
		cfg.ChunkSize = 1 << 20 // 1 MiB for benchmarks
		if modifyCfg != nil {
			modifyCfg(cfg)
		}
	})
}

// benchHTTPClient 返回 benchmark 专用的 HTTP 客户端（每 benchmark 一个独立连接池）。
// 硬规则（AGENTS.md）：测试/基准禁止用 http.DefaultClient / 共享的 DefaultTransport——
// 并行用例关闭共享连接池会打断在途请求（本仓已在 pkg/client、syncmock、cmd/sclient 多次实证）。
func benchHTTPClient(tb testing.TB) *http.Client {
	tb.Helper()
	c := &http.Client{Transport: &http.Transport{}}
	tb.Cleanup(c.CloseIdleConnections)
	return c
}

// uploadFileBench 上传文件（testing.TB 版本，复用 uploadFile 的逻辑）。
func uploadFileBench(tb testing.TB, client *http.Client, baseURL, filename string, body []byte, headers map[string]string) (int, []byte) {
	tb.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		tb.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		tb.Fatalf("write part: %v", err)
	}
	if err = mw.Close(); err != nil {
		tb.Fatalf("close multipart: %v", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		tb.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	var resp *http.Response
	resp, err = client.Do(req)
	if err != nil {
		tb.Fatalf("do upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// ---- 夹具可用性守卫 ----
//
// 为什么需要：benchmark 夹具「跑不起来」时不会自动变红——`make bench` 曾用 `| tee` 吞掉 go test
// 的退出码，job 仍绿；benchmark-action 那边只是少几条数据，肉眼看不出来。自 #159（凭据 Store 化）
// 把 allow_insecure_loopback 默认置 false 起，pkg/server 的 3 个 benchmark 一直在 401 空跑
// （取证见 docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md）。
// 下面两个用例把「夹具真的能跑通上传」钉成普通单测，任何一天跑 `make test` 就会红。

// postJSONBench 发 JSON 请求并解码响应（失败即 Fatal）。接受 testing.TB，benchmark 同样可用。
func postJSONBench(tb testing.TB, client *http.Client, url string, reqBody []byte, out any) int {
	tb.Helper()
	resp, err := client.Post(url, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		tb.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		tb.Fatalf("解码 %s 响应: %v", url, err)
	}
	return resp.StatusCode
}

// TestBenchStorageRootSelection 钉住存储根的选择逻辑：优先 tmpfs（避免把 runner 磁盘
// 回写带进计时），挂载点不可用时回退 tb.TempDir()（而不是 panic/返回空串）。
func TestBenchStorageRootSelection(t *testing.T) {
	t.Parallel()

	fakeTmpfs := t.TempDir()
	got := benchStorageRootAt(t, fakeTmpfs)
	if !strings.HasPrefix(got, fakeTmpfs+string(os.PathSeparator)) {
		t.Fatalf("可用挂载点应被优先使用：want 前缀 %s，得到 %s", fakeTmpfs, got)
	}
	if err := os.WriteFile(filepath.Join(got, "probe.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("存储根必须可写: %v", err)
	}

	fallback := benchStorageRootAt(t, filepath.Join(fakeTmpfs, "does-not-exist"))
	if st, err := os.Stat(fallback); err != nil || !st.IsDir() {
		t.Fatalf("挂载点不可用时必须回退到可用目录（得到 %q, err=%v）", fallback, err)
	}
}

func TestBenchServerAllowsLoopbackUpload(t *testing.T) {
	t.Parallel()

	url, _ := benchServer(t, nil)
	client := benchHTTPClient(t)

	data := bytes.Repeat([]byte("A"), 32*1024)
	status, body := uploadFileBench(t, client, url, "probe-upload.bin", data, map[string]string{
		"X-File-Checksum": sha256hex(data),
	})
	if status != http.StatusOK {
		t.Fatalf("benchmark 夹具的上传被拒（status=%d, body=%s）：夹具必须显式开启回环无认证兜底"+
			"（cfg.AllowInsecureLoopback），否则 benchmark 全是 401 空跑", status, body)
	}
}

func TestBenchServerWithChunkedAllowsLoopbackFlow(t *testing.T) {
	t.Parallel()

	url, _ := benchServerWithChunked(t, nil)
	client := benchHTTPClient(t)

	const (
		totalChunks = 2
		chunkSize   = 1 << 20
		uploadID    = "probe-chunked"
		filename    = "probe-chunked.bin"
	)
	payload := bytes.Repeat([]byte("C"), chunkSize*totalChunks)
	fileCS := sha256hex(payload)

	initResp := files.ChunkedInitResponse{}
	initJSON := mustJSON(t, map[string]any{
		"upload_id":     uploadID,
		"filename":      filename,
		"total_size":    int64(len(payload)),
		"chunk_size":    int64(chunkSize),
		"total_chunks":  totalChunks,
		"file_checksum": fileCS,
	})
	if status := postJSONBench(t, client, url+"/upload/init", initJSON, &initResp); status != http.StatusOK || !initResp.Success {
		t.Fatalf("分块夹具的 /upload/init 未成功（status=%d, resp=%+v）：夹具必须开启回环无认证兜底",
			status, initResp)
	}

	for i := range totalChunks {
		part := payload[i*chunkSize : (i+1)*chunkSize]

		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("upload_id", uploadID)
		_ = mw.WriteField("chunk_index", fmt.Sprintf("%d", i))
		_ = mw.WriteField("chunk_checksum", sha256hex(part))
		fw, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", i))
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err = fw.Write(part); err != nil {
			t.Fatalf("写 chunk: %v", err)
		}
		if err = mw.Close(); err != nil {
			t.Fatalf("关闭 multipart: %v", err)
		}

		resp, err := client.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatalf("POST /upload/chunk #%d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("分块夹具的 /upload/chunk #%d 未成功（status=%d）", i, resp.StatusCode)
		}
	}

	completeResp := files.ChunkCompleteResponse{}
	completeJSON := mustJSON(t, map[string]string{"upload_id": uploadID})
	if status := postJSONBench(t, client, url+"/upload/complete", completeJSON, &completeResp); status != http.StatusOK || !completeResp.Success {
		t.Fatalf("分块夹具的 /upload/complete 未成功（status=%d, resp=%+v）", status, completeResp)
	}

	// 端到端：合并后的文件必须能原样下载。
	resp, err := client.Get(url + "/download?filename=" + filename)
	if err != nil {
		t.Fatalf("GET /download: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("分块上传后下载不一致：status=%d, len=%d, want=%d", resp.StatusCode, len(got), len(payload))
	}
}

// ---- 基准测试 ----

// BenchmarkUpload 上传 1 MiB 文件，记录吞吐量（bytes/sec）。
func BenchmarkUpload(b *testing.B) {
	url, _ := benchServer(b, nil)
	client := benchHTTPClient(b)

	data := bytes.Repeat([]byte("A"), 1<<20) // 1 MiB
	cs := sha256hex(data)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		filename := fmt.Sprintf("bench-upload-%d.bin", i)
		status, _ := uploadFileBench(b, client, url, filename, data, map[string]string{
			"X-File-Checksum": cs,
		})
		if status != 200 {
			b.Fatalf("upload #%d failed: status=%d", i, status)
		}
	}
}

// BenchmarkDownload 下载已上传的 1 MiB 文件。
func BenchmarkDownload(b *testing.B) {
	url, _ := benchServer(b, nil)
	client := benchHTTPClient(b)

	// Setup：上传一个 1 MiB 文件（不计入计时）
	data := bytes.Repeat([]byte("B"), 1<<20)
	cs := sha256hex(data)
	status, _ := uploadFileBench(b, client, url, "bench-download.bin", data, map[string]string{
		"X-File-Checksum": cs,
	})
	if status != 200 {
		b.Fatalf("setup upload failed: %d", status)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		resp, err := client.Get(url + "/download?filename=bench-download.bin")
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if n != int64(len(data)) {
			b.Fatalf("read %d bytes, expected %d", n, len(data))
		}
	}
}

// BenchmarkConcurrentUploads 10 并发 goroutine 同时上传 ~10 KiB 小文件。
func BenchmarkConcurrentUploads(b *testing.B) {
	url, _ := benchServer(b, nil)
	client := benchHTTPClient(b)

	data := bytes.Repeat([]byte("small"), 2500) // ≈ 10 KiB
	cs := sha256hex(data)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	const concurrency = 10
	var (
		wg      sync.WaitGroup
		counter atomic.Int64
		errCh   = make(chan error, concurrency)
	)

	for range concurrency {
		wg.Go(func() {
			for {
				n := int(counter.Add(1) - 1)
				if n >= b.N {
					return
				}
				filename := fmt.Sprintf("concurrent-%d-%d.bin", n, time.Now().UnixNano())
				status, _ := uploadFileBench(b, client, url, filename, data, map[string]string{
					"X-File-Checksum": cs,
				})
				if status != 200 {
					errCh <- fmt.Errorf("concurrent upload #%d failed: status=%d", n, status)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChunkedUpload 4 MiB 文件分块上传（4 chunks @ 1 MiB each）。
func BenchmarkChunkedUpload(b *testing.B) {
	url, _ := benchServerWithChunked(b, nil)
	client := benchHTTPClient(b)

	chunkSize := int64(1 << 20) // 1 MiB per chunk
	totalChunks := 4
	fileSize := int(chunkSize) * totalChunks // 4 MiB

	// 生成 4 MiB 文件数据
	fileData := make([]byte, fileSize)
	for i := range fileData {
		fileData[i] = byte(i % 251) // 可预测但非全重复
	}
	fileChecksum := sha256hex(fileData)

	b.SetBytes(int64(fileSize))
	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		uploadID := fmt.Sprintf("bench-chunked-%d", i)
		filename := fmt.Sprintf("bench-chunked-%d.bin", i)

		// ---- Init ----
		initReq := map[string]any{
			"upload_id":     uploadID,
			"filename":      filename,
			"total_size":    fileSize,
			"chunk_size":    chunkSize,
			"total_chunks":  totalChunks,
			"file_checksum": fileChecksum,
		}
		initBody, _ := json.Marshal(initReq)
		// 每个端点都校验状态码：不校验时「全 401/400 空跑」也能安静通过，
		// 产出无意义的 ns/op（历史事故，见文件顶部「夹具可用性守卫」）。
		initResp := files.ChunkedInitResponse{}
		if status := postJSONBench(b, client, url+"/upload/init", initBody, &initResp); status != http.StatusOK || !initResp.Success {
			b.Fatalf("init #%d 失败: status=%d, resp=%+v", i, status, initResp)
		}

		// ---- Upload chunks ----
		for ci := range totalChunks {
			start := ci * int(chunkSize)
			end := min(start+int(chunkSize), fileSize)
			chunkData := fileData[start:end]
			chunkCS := sha256hex(chunkData)

			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("upload_id", uploadID)
			_ = mw.WriteField("chunk_index", fmt.Sprintf("%d", ci))
			_ = mw.WriteField("chunk_checksum", chunkCS)
			part, _ := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", ci))
			_, _ = part.Write(chunkData)
			_ = mw.Close()

			resp, err := client.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
			if err != nil {
				b.Fatalf("chunk #%d/%d: %v", i, ci, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Fatalf("chunk #%d/%d 失败: status=%d", i, ci, resp.StatusCode)
			}
		}

		// ---- Complete ----
		completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
		completeResp := files.ChunkCompleteResponse{}
		if status := postJSONBench(b, client, url+"/upload/complete", completeBody, &completeResp); status != http.StatusOK || !completeResp.Success {
			b.Fatalf("complete #%d 失败: status=%d, resp=%+v", i, status, completeResp)
		}
	}
}
