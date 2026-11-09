// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpc

// httpc_body_lifetime_test.go 锁定**响应体生命周期契约**：
// `DoOpts` 返回的 `resp.Body` 必须在调用方读取期间保持可用——**单次尝试的 ctx 不得在返回前被取消**。
//
// 背景（真实缺陷，2026-11-09 实测）：`attempt` 里 `actx, cancel := context.WithTimeout(...)` +
// `defer cancel()` ⇒ 返回响应时立刻取消 ⇒ 任何「返回后再读 body」的调用点都得到
// `context canceled`。小响应因已被 transport 缓冲而侥幸通过，`sclient upgrade --check` 读
// GitHub release JSON 时暴露；自更新**下载二进制**（大 body）同样会中招。
//
// 修复语义（与 net/http 自身 `Client.Timeout` 一致）：把该次尝试的释放权**移交响应体**——
// 读完（EOF）或 `Close()` 时释放。

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ctxAwareReader 在底层 ctx 被取消后拒绝继续读（模拟 transport 的真实行为）。
type ctxAwareReader struct {
	ctx  context.Context
	data string
	off  int
}

func (r *ctxAwareReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

const bodyLifetimePayload = `{"tag_name":"v9.9.9","assets":[{"name":"sproxy_linux_amd64.tar.gz"}]}`

func TestDoOpts_ReturnedBodyStaysReadableAfterAttemptTimeout(t *testing.T) {
	t.Parallel()
	var reqCtx context.Context
	tr := rtFunc(func(r *http.Request) (*http.Response, error) {
		reqCtx = r.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Request:    r,
			Body:       io.NopCloser(&ctxAwareReader{ctx: reqCtx, data: bodyLifetimePayload}),
		}, nil
	})
	c := New(Options{Transport: tr, Timeout: 3 * time.Second, Retry: pipe.Policy{Attempts: 1}, Env: instantEnv()})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoOpts(context.Background(), req, DoOptions{Idempotent: true})
	if err != nil {
		t.Fatalf("DoOpts: %v", err)
	}

	// 无 sleep 的确定性断言：DoOpts 返回时，请求 ctx **不得**已被取消
	// （修复前 `defer cancel()` 在返回时即触发 ⇒ 这里必红）。
	if cerr := reqCtx.Err(); cerr != nil {
		t.Fatalf("DoOpts 返回时请求 ctx 已被取消（%v）⇒ 调用方读 body 必失败", cerr)
	}
	got, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("读响应体失败（单次尝试 ctx 被提前取消，调用方 body 不可用）: %v", rerr)
	}
	if string(got) != bodyLifetimePayload {
		t.Fatalf("body=%q, want %q", got, bodyLifetimePayload)
	}
	if cerr := resp.Body.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	// 读尽（EOF）/Close 之后，该次尝试的 ctx 必须已释放（不泄漏定时器）。
	if cerr := reqCtx.Err(); cerr == nil {
		t.Fatalf("body 读尽并 Close 后，尝试 ctx 仍未释放")
	}
}

// TestDoOpts_Non2xxBodyStaysReadable：非 2xx 终态同样交回响应（HTTPError 路径可读 body 定位）。
func TestDoOpts_Non2xxBodyStaysReadable(t *testing.T) {
	t.Parallel()
	const errBody = `{"error_code":31362,"error_msg":"sign error"}`
	var reqCtx context.Context
	tr := rtFunc(func(r *http.Request) (*http.Response, error) {
		reqCtx = r.Context()
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{},
			Request:    r,
			Body:       io.NopCloser(&ctxAwareReader{ctx: reqCtx, data: errBody}),
		}, nil
	})
	c := New(Options{Transport: tr, Timeout: 3 * time.Second, Retry: pipe.Policy{Attempts: 1}, Env: instantEnv()})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, derr := c.DoOpts(context.Background(), req, DoOptions{Idempotent: true})
	if resp == nil {
		t.Fatalf("非 2xx 应把响应交回（可读 body），got nil resp, err=%v", derr)
	}
	defer resp.Body.Close()
	if cerr := reqCtx.Err(); cerr != nil {
		t.Fatalf("非 2xx 返回时请求 ctx 已被取消（%v）", cerr)
	}
	got, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("读非 2xx 响应体失败: %v", rerr)
	}
	if string(got) != errBody {
		t.Fatalf("body=%q, want %q", got, errBody)
	}
}
