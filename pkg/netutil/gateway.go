// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GatewayForwarder 是「URL 拼接式」前置转发器（gateway 模式）。
//
// 目标 `https://host/path?q=1` 会被改写成 `<forwarder>/host/path?q=1`，
// 由转发器代为出网（典型部署：`http://sr.suixi.space:18081/`）。
//
// 与**标准代理**的区别（重要）：
//   - 标准代理：client 设 Transport.Proxy，由代理做 CONNECT（走隧道）；
//   - gateway：把目标拼进 URL 路径，代理只做普通 GET。
//
// 用途（2026-11-25 实测）：本机直连 keepshare/pikpak 约 4.2s/请求，
// 经转发器可降到 ~0.5s（快 8 倍）——但转发器当前不可用，因此**默认关闭**（配置为空即直连）。
type GatewayForwarder struct {
	base *url.URL
}

// ParseGatewayForwarder 解析转发器地址（如 `http://sr.suixi.space:18081/`）。
// 空串返回 (nil, nil)（表示不使用转发器）。非法 URL 返回错误。
func ParseGatewayForwarder(raw string) (*GatewayForwarder, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil //nolint:nilnil // 空配置是合法状态（直连），调用方据 nil 判断
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("netutil: 非法 gateway 转发器 %q", raw)
	}
	return &GatewayForwarder{base: u}, nil
}

// Rewrite 把目标 URL 改写为转发器形式：`<scheme>://<fw-host><fw-path>/<target-host><target-path>?<query>`。
//
// 保留查询串（keepshare 的 `?xt=urn:btih:...` 必须保留——丢掉会被判无效分享）。
func (f *GatewayForwarder) Rewrite(target *url.URL) *url.URL {
	base := strings.TrimRight(f.base.Path, "/")
	return &url.URL{
		Scheme:   f.base.Scheme,
		Host:     f.base.Host,
		Path:     base + "/" + target.Host + target.Path,
		RawQuery: target.RawQuery,
	}
}

// Host 返回转发器主机（用于识别「已是转发器地址」的请求，避免二次重写）。
func (f *GatewayForwarder) Host() string { return f.base.Host }

// gatewayRoundTripper 把每个出站请求改写为转发器 URL。
//
// 重定向语义：Go 的 Client 会为每一跳重新调用 RoundTrip，
// 因此只要在这里统一改写，多跳重定向也会自动经由转发器；
// 若某跳的 Location 已指向转发器本身（部分转发器会回 307 到自身），
// 检测 host 相同即直通，避免无限重写。
type gatewayRoundTripper struct {
	inner http.RoundTripper
	fw    *GatewayForwarder
}

func (t *gatewayRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return t.inner.RoundTrip(req)
	}
	// 已是转发器地址（重定向跟随产生）⇒ 直通。
	if req.URL.Host == t.fw.Host() {
		return t.inner.RoundTrip(req)
	}
	nr := req.Clone(req.Context())
	nr.URL = t.fw.Rewrite(req.URL)
	nr.Host = t.fw.Host()
	return t.inner.RoundTrip(nr)
}

// NewGatewayHTTPClient 返回经 gateway 转发器出网的 HTTP 客户端。
//
// base 用 IsolatedTransport()（继承 DefaultTransport 的拨号/TLS/连接池调校，独立连接池），
// 只把 RoundTrip 换成「URL 改写」——**不使用** Transport.Proxy（否则会把转发器当标准
// 代理去 CONNECT，必然失败；sdserver 的同名实现已踩过此坑）。
//
// forwarder 解析失败或为空返回错误/直连 client。
func NewGatewayHTTPClient(forwarder string, timeout time.Duration) (*http.Client, error) {
	fw, err := ParseGatewayForwarder(forwarder)
	if err != nil {
		return nil, err
	}
	if fw == nil {
		return &http.Client{Timeout: timeout, Transport: IsolatedTransport()}, nil
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &gatewayRoundTripper{inner: IsolatedTransport(), fw: fw},
	}, nil
}
