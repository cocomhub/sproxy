// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// TestHubWSDial_InsecureTLS 验证 hubWSDial 的 --insecure 接线（B17）：
//   - 自签 wss hub（httptest.NewTLSServer）且未传 --insecure → TLS 握手失败
//     （证书不被信任，保持默认安全行为）；
//   - 传 --insecure → 经 insecureHTTPClient() 跳过证书校验，握手成功。
func TestHubWSDial_InsecureTLS(t *testing.T) {
	wsNode := ws.NewHandlerNode()
	muxHTTP := http.NewServeMux()
	wsNode.AddToMux(muxHTTP, "/ws")
	ts := httptest.NewTLSServer(muxHTTP)
	defer ts.Close()

	// 排空服务端已接受的 WS 连接（connCh 缓冲 16，单连接可不排空，这里兜底防泄漏）。
	ctxAccept, cancelAccept := context.WithCancel(context.Background())
	defer cancelAccept()
	go func() {
		for {
			c, aerr := wsNode.Accept(ctxAccept)
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	wssURL := "wss://" + ts.Listener.Addr().String() + "/ws"

	// 非 insecure：握手应失败（自签证书不被信任）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := hubWSDial(ctx, wssURL, false); err == nil {
		t.Fatal("expected TLS handshake failure without --insecure")
	}

	// --insecure：跳过证书校验，握手成功。
	ctxInsec, cancelInsec := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInsec()
	conn, err := hubWSDial(ctxInsec, wssURL, true)
	if err != nil {
		t.Fatalf("hubWSDial insecure failed: %v", err)
	}
	defer conn.Close()
}

// TestInsecureHTTPClient_UsesInsecureTLS 验证 insecureHTTPClient 的 transport 关闭
// 证书校验（InsecureSkipVerify=true），且保留超时（对齐 HubSignaler 长轮询 60s）。
func TestInsecureHTTPClient_UsesInsecureTLS(t *testing.T) {
	hc := insecureHTTPClient()
	if hc == nil {
		t.Fatal("insecureHTTPClient() returned nil")
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", hc.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify=true on transport TLSClientConfig")
	}
	if hc.Timeout != 60*time.Second {
		t.Fatalf("expected Timeout 60s, got %v", hc.Timeout)
	}
}
