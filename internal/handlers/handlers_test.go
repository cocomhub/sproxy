// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/config"
)

func TestIsHostAllowed(t *testing.T) {
	// 空列表允许任意
	{
		h := &Handlers{}
		if !h.isHostAllowed("anyhost:1234") {
			t.Fatalf("empty allow-list should allow any host:port")
		}
		if !h.isHostAllowed("anyhost") {
			t.Fatalf("empty allow-list should allow any host")
		}
	}

	// 精确匹配 host:port
	{
		h := &Handlers{cfg: &config.Config{AllowedHosts: []string{"example.org:443"}}}
		if !h.isHostAllowed("example.org:443") {
			t.Fatalf("expected exact host:port match to pass")
		}
		if h.isHostAllowed("example.org:8443") {
			t.Fatalf("unexpected allow for unmatched port")
		}
		if h.isHostAllowed("example.org") {
			t.Fatalf("unexpected allow for host without port when only host:port is allowed")
		}
	}

	// 纯主机名匹配（允许任意端口）
	{
		h := &Handlers{cfg: &config.Config{AllowedHosts: []string{"example.com"}}}
		if !h.isHostAllowed("example.com") {
			t.Fatalf("expected pure hostname to pass")
		}
		if !h.isHostAllowed("example.com:8443") {
			t.Fatalf("expected pure hostname to allow any port")
		}
		if h.isHostAllowed("other.com:80") {
			t.Fatalf("unexpected allow for different hostname")
		}
	}
}

func TestStripHopByHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "Keep-Alive, X-Foo, x-bar")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("TE", "trailers")
	h.Set("Trailer", "X-Trailer")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Upgrade", "websocket")
	h.Set("X-Foo", "1")
	h.Set("x-bar", "2")
	h.Set("Other", "ok")

	stripHopByHopHeaders(h)

	// 标准 hop-by-hop 头应被移除
	for _, k := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		if v := h.Get(k); v != "" {
			t.Fatalf("expected header %q removed, got %q", k, v)
		}
	}
	// Connection 指定的自定义头也应被移除
	for _, k := range []string{"X-Foo", "x-bar"} {
		if v := h.Get(k); v != "" {
			t.Fatalf("expected custom header %q removed via Connection, got %q", k, v)
		}
	}
	// 其他头应保留
	if v := h.Get("Other"); v != "ok" {
		t.Fatalf("expected non hop-by-hop header retained, got %q", v)
	}
}

func TestBandwidthRecorderCountsOnce(t *testing.T) {
	var send int64
	var bandwidth int64
	h := &Handlers{
		sendSize:     &send,
		curBandwidth: &bandwidth,
	}

	var buf bytes.Buffer
	rec := bandwidthRecorder{h: h, w: &buf}
	data := []byte("hello world")

	n, err := rec.Write(data)
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != len(data) {
		t.Fatalf("unexpected write size: got %d want %d", n, len(data))
	}
	if got := buf.String(); got != string(data) {
		t.Fatalf("unexpected buffer content: %q", got)
	}
	if got := atomic.LoadInt64(h.sendSize); got != int64(len(data)) {
		t.Fatalf("unexpected sendSize: got %d want %d", got, len(data))
	}
}
