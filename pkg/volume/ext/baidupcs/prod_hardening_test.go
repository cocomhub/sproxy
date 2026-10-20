// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// prod_hardening_test.go 钉住 2026-10-21 对抗审查的两条 P1（百度网盘生产稳定性）：
//
//  1. 库兜底下载把 206 的 `Content-Length`（**区间长度**）当文件总大小 ⇒ >1KiB 文件读取恒失败
//     或静默截断成 1KiB；
//  2. 分片上传的「持续性分片错误」没有终止判据 ⇒ 上游 MultiUploader 零退避无限重试
//     （Execute 永不返回、sync 任务永久 syncing、整卷上传锁被永久持有）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qjfoidnh/BaiduPCS-Go/requester"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/uploader"
)

func respWith(status int, hdr map[string]string, contentLength int64) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		ContentLength: contentLength,
		Body:          io.NopCloser(strings.NewReader("")),
	}
}

// TestProbeTotalSize 探测响应必须解析出**文件总大小**，绝不能用 206 的区间长度当总大小。
func TestProbeTotalSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp *http.Response
		want int64
	}{
		{
			name: "x-bs-file-size 优先",
			resp: respWith(206, map[string]string{
				"x-bs-file-size": "123456789", "Content-Range": "bytes 0-32767/999",
				"Content-Length": "32768",
			}, 32768),
			want: 123456789,
		},
		{
			name: "Content-Range total（无 x-bs-file-size）",
			resp: respWith(206, map[string]string{
				"Content-Range": "bytes 0-32767/5000000", "Content-Length": "32768",
			}, 32768),
			want: 5000000,
		},
		{
			name: "206 且无总量信息 ⇒ 0（不能返回区间长度 32768）",
			resp: respWith(206, map[string]string{"Content-Length": "32768"}, 32768),
			want: 0,
		},
		{
			name: "200 全量响应才可用 Content-Length",
			resp: respWith(200, map[string]string{"Content-Length": "777"}, 777),
			want: 777,
		},
		{
			name: "Content-Range 总量为 * ⇒ 未知",
			resp: respWith(206, map[string]string{"Content-Range": "bytes 0-32767/*"}, 32768),
			want: 0,
		},
	}
	for _, c := range cases {
		if got := probeTotalSize(c.resp); got != c.want {
			t.Errorf("%s: probeTotalSize = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestShouldTerminatePart 持续性分片失败必须终止重试（否则上游无限重排 ⇒ Execute 永不返回）。
func TestShouldTerminatePart(t *testing.T) {
	t.Parallel()
	u := &baiduMultiUpload{}

	// ① 4xx 首次即终止。
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge} {
		err := u.shouldTerminatePart(1, respWith(code, nil, 0), nil)
		var me *uploader.MultiError
		if !errors.As(err, &me) || !me.Terminated {
			t.Fatalf("HTTP %d 必须返回 MultiError{Terminated:true}, got %v", code, err)
		}
	}

	// ② 5xx/网络错误：次数上限内可重试，超限后终止。
	u2 := &baiduMultiUpload{}
	for i := 1; i <= maxPartAttempts; i++ {
		if err := u2.shouldTerminatePart(7, respWith(500, nil, 0), nil); err != nil {
			t.Fatalf("第 %d 次 5xx 不应终止: %v", i, err)
		}
	}
	err := u2.shouldTerminatePart(7, nil, errors.New("connection reset"))
	var me *uploader.MultiError
	if !errors.As(err, &me) || !me.Terminated {
		t.Fatalf("超过 %d 次后必须终止, got %v", maxPartAttempts, err)
	}

	// ③ 分片计数互相独立（同一次尝试不影响其它片）。
	if err := u2.shouldTerminatePart(8, nil, errors.New("boom")); err != nil {
		t.Fatalf("其它分片首次失败不应终止: %v", err)
	}
}

// TestBaiduDURLCheck_Wiring 接线级验证：baiduDURLCheck 必须走 probeTotalSize（206 的
// Content-Length 是区间长度，不能当总大小）。只测 probeTotalSize 会漏掉「接线改了」。
func TestBaiduDURLCheck_Wiring(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Errorf("探测必须带 Range 头")
		}
		w.Header().Set("Content-Range", "bytes 0-32767/1048576")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 1024))
	}))
	defer srv.Close()

	cli := requester.NewHTTPClient()
	size, resp, err := baiduDURLCheck(context.Background(), cli, srv.URL)
	if err != nil {
		t.Fatalf("baiduDURLCheck: %v", err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if size != 1048576 {
		t.Fatalf("探测到的总大小 = %d, want 1048576（1024 是区间长度 ⇒ 会静默截断）", size)
	}
}

// TestBinaryMeta_ExecutorUnavailableNotNotFound 二进制不可用时必须是**故障**，不得归类为
// 「远端路径不存在」（否则 Stat 得到 (nil,nil)，上传/跳过判定全错、真实故障被掩盖）。
func TestBinaryMeta_ExecutorUnavailableNotNotFound(t *testing.T) {
	t.Parallel()
	a := newBinaryAdapter(AdapterConfig{BinaryPath: "definitely-not-a-real-baidupcs-binary-xyz"})
	_, err := a.Meta(context.Background(), "/some/file.bin")
	if err == nil {
		t.Fatal("执行器不可用时必须返回错误")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("执行器不可用被误判为 ErrNotFound（会把故障当文件不存在）: %v", err)
	}
	if !isExecUnavailable(err) {
		t.Fatalf("错误应可识别为「执行器不可用」: %v", err)
	}
}
