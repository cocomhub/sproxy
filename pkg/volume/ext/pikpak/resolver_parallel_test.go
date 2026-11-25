// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShareResolver_FileInfoParallel 验证「每个文件的直链获取」是并行的。
//
// 背景（2026-11-25 实测）：`Resolve` 里 `for _, f := range files { fileInfo }` 串行，
// 单个分享 31 个文件 ⇒ 31 次 API 往返（每次 ~2s）⇒ 单分享 60s+，是 /api/resolve 慢的主因。
func TestShareResolver_FileInfoParallel(t *testing.T) {
	t.Parallel()
	const files = 8
	var inflight, peak int32
	// release 用于让 file_info 处理保持"在途"，直到测试观察到并发峰值：
	// 用 channel 同步代替 time.Sleep（R14 睡眠棘轮 + 更快更稳）。
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/shield/captcha/init"):
			fmt.Fprint(w, `{"captcha_token":"tok","expires_in":3600}`)
		case strings.Contains(r.URL.Path, "/drive/v1/share/file_info"):
			n := atomic.AddInt32(&inflight, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
					break
				}
			}
			<-release // 由测试在观察到峰值后关闭
			atomic.AddInt32(&inflight, -1)
			fmt.Fprint(w, `{"file_info":{"hash":"H","web_content_link":"http://x/1.mp4"}}`)
		case strings.Contains(r.URL.Path, "/drive/v1/share"):
			// 分享列表：返回 files 个文件
			var sb strings.Builder
			sb.WriteString(`{"files":[`)
			for i := range files {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"id":"f%d","name":"v%d.mp4","kind":"drive#file","size":%d}`, i, i, 200<<20)
			}
			sb.WriteString(`],"next_page_token":""}`)
			fmt.Fprint(w, sb.String())
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	r := NewShareResolver(ShareResolverConfig{}) //nolint:exhaustruct // 测试只需 host
	r.userHost = srv.URL
	r.apiHost = srv.URL
	r.deviceID = "dev-test"

	start := time.Now()
	type res struct {
		meta *ShareMeta
		err  error
	}
	done := make(chan res, 1)
	go func() {
		m, e := r.Resolve(t.Context(), "https://mypikpak.com/s/SHARE1")
		done <- res{m, e}
	}()
	// 等待并发峰值出现（file_info 在途数 ≥2），再放行
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&peak) < 2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	close(release) // 放行所有在途 file_info（先放行，再等 Resolve 返回，避免死锁）
	got := <-done
	elapsed := time.Since(start)
	meta, err := got.meta, got.err
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if len(meta.Files) != files {
		t.Fatalf("应返回 %d 个文件，got %d", files, len(meta.Files))
	}
	if p := atomic.LoadInt32(&peak); p < 2 {
		t.Fatalf("fileInfo 应并行（峰值 ≥2），实际峰值=%d（串行会 8×120ms≈960ms）", p)
	}
	// 串行需 8×120ms=960ms；并行（≥4）应 <600ms
	if elapsed > 600*time.Millisecond {
		t.Fatalf("并行后应显著快于串行 960ms，实际 %v", elapsed)
	}
}
