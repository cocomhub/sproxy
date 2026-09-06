// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---- RequestSigner seam 测试 ----
//
// signer_test.go 覆盖任务 ④ 的 RequestSigner seam：
//   - 默认 ConfigSigner：无注入时签名行为与 4A 既有路径完全一致（SproxySig 头 + skey-id；
//     accessKeySecret=="" 时不带签名头——公开端点直达）；
//   - WithRequestSigner(fake)：注入自定义 Signer 后每请求恰好调用一次 Sign，且自定义
//     添加的头部出现在请求中；
//   - 直连 + 隧道路径均走注入 Signer（R3-M5：两路径都不得绕过注入）；
//   - renew 引导缺段承接：allowMissingEntryID 引导态放行；非引导态报
//     「access_key_id 未配置（v2 skey-id 必传）」。
//
// 注：隧道外层签名路径通过签名后拒绝 body 读取来断言——否则 "frame" 之类的散列载荷
// 会在认证中间件读取时被 io.ErrUnexpectedEOF 损坏（server → client 一侧的回读错误会
// 冒泡成 RoundTrip 错误，污染断言目标）。

const testSignerAK = "ak-seam-0123456789abcdef"

// testSignerSK 返回 64-hex 合法 SK（仅长度校验用，不做真实验签）。
func testSignerSK() string {
	return strings.Repeat("ab", 32)
}

// 默认行为不变（直连）：配置 access_key+secret+id 时发送 SproxySig 头且带 skey-id
// （与 4A 既有签名路径一致）；accessKeySecret=="" 时不带签名头（公开端点直达）。
func TestRequestSigner_DefaultConfigSigner_Direct(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want bool // 是否期望出现 SproxySig 头
	}{
		{name: "with-credentials", opts: []Option{
			WithAccessKey(testSignerAK, testSignerSK()),
			WithAccessKeyID(testClientEntryID),
		}, want: true},
		{name: "no-secret-no-header", opts: []Option{}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			c := NewFileClient(ts.URL, tc.opts...)
			if _, err := c.doRequest(context.Background(), "GET", "/probe", nil, nil); err != nil {
				t.Fatalf("doRequest: %v", err)
			}
			if tc.want {
				if !strings.HasPrefix(gotAuth, "SproxySig ") {
					t.Errorf("默认 ConfigSigner 应带 SproxySig 头, got %q", gotAuth)
				}
				if !strings.Contains(gotAuth, " skey-id="+testClientEntryID+" ") {
					t.Errorf("SproxySig 头应携带 skey-id=<skeyID>（v2 必传）, got %q", gotAuth)
				}
			} else if gotAuth != "" {
				t.Errorf("accessKeySecret==\"\" 时不应带签名头, got %q", gotAuth)
			}
		})
	}
}

// 默认行为不变（隧道外层）：sigRoundTripper 默认 ConfigSigner 语义——配置正确凭据
// 时外层签名含 v=2 ak / skey-id 段且 body 标记 UNSIGNED（与现状 sigRoundTripper 一致：
// 帧在签名面不可见）。直接驱动安装后的外层 RoundTripper，等价隧道 Do 的外层签名路径。
func TestRequestSigner_DefaultConfigSigner_Tunnel(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	// 构造顺序模拟真实工厂：先 WithTunnel（此时尚无 access_key_id），再 WithAccessKeyID。
	WithTunnel(testTunnelAK, testTunnelSK)(c)
	WithAccessKeyID(testClientEntryID)(c)
	if c.tunnelClient == nil {
		t.Fatal("tunnelClient should be created")
	}

	rt := &sigRoundTripper{base: http.DefaultTransport, c: c}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/tunnel", strings.NewReader("frame"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(gotAuth, "v=2 ak="+testTunnelAK) {
		t.Errorf("默认 ConfigSigner（隧道）应带 v=2 ak, got %q", gotAuth)
	}
	if !strings.Contains(gotAuth, " skey-id="+testClientEntryID+" ") {
		t.Errorf("默认 ConfigSigner（隧道）应携带 skey-id, got %q", gotAuth)
	}
	// 隧道外层 body_sha256=UNSIGNED（帧无法整体哈希；与现状 sigRoundTripper 一致）。
	if !strings.Contains(gotAuth, "body_sha256=UNSIGNED") {
		t.Errorf("默认 ConfigSigner（隧道）应标记 body_sha256=UNSIGNED, got %q", gotAuth)
	}
}

// fakeSigner 记录调用次数，并给每个请求添加一个用户自定义头。
type fakeSigner struct {
	calls atomic.Int64
}

func (f *fakeSigner) Sign(_ context.Context, req *http.Request) error {
	f.calls.Add(1)
	req.Header.Set("X-Custom-Signer", "custom-"+strings.Join(req.Header.Values("X-Custom-Signer"), "+"))
	return nil
}

func TestRequestSigner_WithRequestSigner_Direct(t *testing.T) {
	fs := &fakeSigner{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Custom-Signer"); got != "custom-" {
			t.Errorf("注入 Signer 添加的头部丢失: got %q (primary headers only)", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// 注入自定义 Signer 的同时故意配置 config 凭据——必须走注入 Signer，
	// ConfigSigner 头不得出现、也不得抛「skey-id 必传」错误。
	c := NewFileClient(ts.URL,
		WithAccessKey(testSignerAK, testSignerSK()),
		WithRequestSigner(fs),
	)
	if _, err := c.doRequest(context.Background(), "GET", "/probe", nil, nil); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if fs.calls.Load() != 1 {
		t.Errorf("注入 Signer 每请求应恰好调用一次, got %d", fs.calls.Load())
	}
}

func TestRequestSigner_WithRequestSigner_Tunnel(t *testing.T) {
	fs := &fakeSigner{}
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// 构造顺序刻意让 WithRequestSigner 在 WithTunnel 之后：sigRoundTripper 在请求时
	// 实时解析持有者 FileClient 的 signer（与凭据实时读取一致），顺序无关。
	c := NewFileClient(ts.URL)
	WithTunnel(testTunnelAK, testTunnelSK)(c)
	WithRequestSigner(fs)(c)
	if c.tunnelClient == nil {
		t.Fatal("tunnelClient should be created")
	}

	// 直接驱动安装后的外层 RoundTripper（等价隧道 Do 的外层签名路径）——完整 tunnel.Do
	// 会因 mock 端无法回话（frame 损坏/超出 metadata 上限）而失败，非本测试目标。
	rt := &sigRoundTripper{base: http.DefaultTransport, c: c}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/tunnel", strings.NewReader("frame"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if fs.calls.Load() != 1 {
		t.Errorf("注入 Signer 隧道路径每请求应恰好调用一次, got %d", fs.calls.Load())
	}
	if gotAuth != "" {
		t.Errorf("注入 Signer 时默认 ConfigSigner 不应产生 Authorization, got %q", gotAuth)
	}
}

// renew 引导缺段承接：非引导态缺 access_key_id 报错（errors.Is 命中 ErrSkeyIDRequired，
// 文案含既有「access_key_id 未配置」）；allowMissingEntryID 引导态放行
// （直接调用 ConfigSigner 断言错误，避免网络往返）。
func TestRequestSigner_ConfigSigner_SkeyIDRequired(t *testing.T) {
	cs := &configSigner{c: &FileClient{accessKey: testSignerAK, accessKeySecret: testSignerSK()}}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/probe", nil)
	err := cs.Sign(context.Background(), req)
	if err == nil {
		t.Fatal("非引导态缺 access_key_id 应报错")
	}
	if !errors.Is(err, ErrSkeyIDRequired) {
		t.Errorf("缺 skeyID 错误应以哨兵 ErrSkeyIDRequired 包装（可 errors.Is）, got %v", err)
	}
	if !strings.Contains(err.Error(), "access_key_id 未配置") {
		t.Errorf("错误信息应含既有文案「access_key_id 未配置」, got %v", err)
	}

	cs.c.allowMissingEntryID = true
	if err := cs.Sign(context.Background(), req); err != nil {
		t.Errorf("renew 引导态（allowMissingEntryID）应放行, got %v", err)
	}
}

// RoundTrip 错误透传：注入 Signer 返回自定义错误时，RoundTrip 原样透传该错误
// （不作为 ErrSkeyIDRequired 回译改写）。默认路径缺 skey-id（errors.Is）才附加既有
// 标准文案。
func TestRequestSigner_SigRoundTripper_ErrorPassthrough(t *testing.T) {
	// 注入 Signer 返回自定义错误 → 原样透传（不被「回译」成 skey-id 文案）。
	customErr := errors.New("custom-signer-denied")
	fs := &fakeSignerErr{err: customErr}
	c := &FileClient{requestSigner: fs}
	rt := &sigRoundTripper{base: http.DefaultTransport, c: c}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/tunnel", strings.NewReader("frame"))
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("RoundTrip 应透传注入 Signer 的错误")
	}
	if !errors.Is(err, customErr) {
		t.Errorf("注入 Signer 自定义错误应被原样透传（errors.Is 可命中）, got %v", err)
	}
	if errors.Is(err, ErrSkeyIDRequired) {
		t.Errorf("注入 Signer 自定义错误不应被误判为 ErrSkeyIDRequired, got %v", err)
	}
	if fs.calls.Load() != 1 {
		t.Errorf("注入 Signer 每请求应恰好调用一次, got %d", fs.calls.Load())
	}

	// 默认路径缺 skey-id → 附加既有标准文案（哨兵 + 引导提示）。
	miss := &FileClient{accessKey: testSignerAK, accessKeySecret: testSignerSK()}
	rt2 := &sigRoundTripper{base: http.DefaultTransport, c: miss}
	_, err = rt2.RoundTrip(httpReq(t, "https://example.invalid/tunnel"))
	if err == nil {
		t.Fatal("默认路径缺 skey-id 应报错")
	}
	if !errors.Is(err, ErrSkeyIDRequired) {
		t.Errorf("缺 skeyID 应以哨兵 ErrSkeyIDRequired 命中, got %v", err)
	}
	if !strings.Contains(err.Error(), "请先 `sclient trust renew` 或配置 access_key_id") {
		t.Errorf("缺 skeyID 错误应含引导文案, got %v", err)
	}
}

// fakeSignerErr 记录调用次数并返回固定错误（RoundTrip 错误透传测试用）。
type fakeSignerErr struct {
	calls atomic.Int64
	err   error
}

func (f *fakeSignerErr) Sign(_ context.Context, _ *http.Request) error {
	f.calls.Add(1)
	return f.err
}

func httpReq(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("frame"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// xfer + 注入 Signer 组合：xfer 隧道本体数据面经 mux 直连（不经 /tunnel HTTP 签名面，
// 认证靠注册帧内 access_key+proof），故「注入 Signer 不被绕过」的语义是——对 xfer 面
// 调用的 HTTP 面入口（doRequest 直连分支）仍逐请求走注入 Signer；TunnelDo（纯 xfer
// 传输）不经 HTTP 签名面、不触发签名器（签名面外，符合设计）。
func TestRequestSigner_XferAndInjectedSigner(t *testing.T) {
	fs := &fakeSigner{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	name := registerPipeXfer(t, nil, testSignerSK()) // hexKey 必须 64-hex（32B）
	c := NewFileClient(ts.URL,
		WithXfer(name, "hub://test", testSignerSK()), // 与注册端同一密钥
		WithRequestSigner(fs),
	)

	// doRequest 直连分支（HTTP 面）：走注入 Signer。
	if _, err := c.doRequest(context.Background(), "GET", "/probe", nil, nil); err != nil {
		t.Fatalf("doRequest(xfer 配置): %v", err)
	}
	if fs.calls.Load() != 1 {
		t.Errorf("xfer 配置下 doRequest 每请求应调用注入 Signer 一次, got %d", fs.calls.Load())
	}

	// TunnelDo 经 xfer mux 真实隧道：数据面不经 HTTP 签名面（签名面外），
	// 不触发签名器、也不产生 Authorization 头。
	n := fs.calls.Load()
	req, _ := http.NewRequest(http.MethodGet, "http://echo/x", nil)
	resp, err := c.TunnelDo(req)
	if err != nil {
		t.Fatalf("TunnelDo(xfer): %v", err)
	}
	_ = resp.Body.Close()
	if fs.calls.Load() != n {
		t.Errorf("xfer 隧道数据面不应触发注入 Signer（签名面外）: calls %d → %d", n, fs.calls.Load())
	}

	// 显式声明：xfer 隧道路径不经过注入 Signer——这是设计语义（R3-M5 的「隧道路径」
	// 指 /tunnel 外层签名面；xfer mux 认证体在注册帧兴趣面之外），记录为文档化行为。
	_ = io.Discard
	_ = fmt.Sprintf
}
