// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
    "bytes"
    "context"
    "io"
    "net/http"
    "strings"
    "testing"
    "time"

    "github.com/cocomhub/sproxy/pkg/tunnel"
    "github.com/cocomhub/sproxy/pkg/tunnel/mux"
    "github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestTunnelEcho(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    tunA := tunnel.NewTunnel(muxA, nil)
    tunB := tunnel.NewTunnel(muxB, nil)

    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()

    srvErr := make(chan error, 1)
    go func() {
        srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            body, _ := io.ReadAll(r.Body)
            w.Write(body)
        }))
    }()

    time.Sleep(50 * time.Millisecond)

    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/echo", strings.NewReader("hi"))
    resp, err := tunA.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    if string(body) != "hi" {
        t.Fatalf("expected %q, got %q", "hi", string(body))
    }
    cancel()
    <-srvErr
}

func TestTunnelSequential(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    tunA := tunnel.NewTunnel(muxA, nil)
    tunB := tunnel.NewTunnel(muxB, nil)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    srvErr := make(chan error, 1)
    go func() {
        srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            w.Write([]byte(r.URL.Path))
        }))
    }()

    time.Sleep(50 * time.Millisecond)

    for _, path := range []string{"/a", "/b", "/c"} {
        req, _ := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
        resp, err := tunA.Do(req)
        if err != nil {
            t.Fatal(err)
        }
        body, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        if string(body) != path {
            t.Fatalf("expected %q, got %q", path, string(body))
        }
    }
    cancel()
    <-srvErr
}

func TestTunnelEncrypted(t *testing.T) {
    key, _ := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    tunA := tunnel.NewTunnel(muxA, key)
    tunB := tunnel.NewTunnel(muxB, key)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    srvErr := make(chan error, 1)
    go func() {
        srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            body, _ := io.ReadAll(r.Body)
            w.Write(bytes.ToUpper(body))
        }))
    }()

    time.Sleep(50 * time.Millisecond)

    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/encrypt", strings.NewReader("hello"))
    resp, err := tunA.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()

    body, _ := io.ReadAll(resp.Body)
    if string(body) != "HELLO" {
        t.Fatalf("expected HELLO, got %q", string(body))
    }
    cancel()
    <-srvErr
}

func TestTunnelBigBody(t *testing.T) {
    // mux 帧负载最大 65535 字节，测试略小于该值
    payload := strings.Repeat("A", 65000)
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    tunA := tunnel.NewTunnel(muxA, nil)
    tunB := tunnel.NewTunnel(muxB, nil)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    srvErr := make(chan error, 1)
    go func() {
        srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            b, _ := io.ReadAll(r.Body)
            w.Write(bytes.ToUpper(b))
        }))
    }()

    time.Sleep(50 * time.Millisecond)

    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/big", strings.NewReader(payload))
    resp, err := tunA.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()

    respBody, _ := io.ReadAll(resp.Body)
    if len(respBody) != 65000 {
        t.Fatalf("expected %d bytes, got %d", 65536, len(respBody))
    }
    cancel()
    <-srvErr
}

func TestTunnelHeaders(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    tunA := tunnel.NewTunnel(muxA, nil)
    tunB := tunnel.NewTunnel(muxB, nil)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    srvErr := make(chan error, 1)
    go func() {
        srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            w.Header().Set("X-Echo", r.Header.Get("X-Custom"))
            w.WriteHeader(http.StatusCreated)
            w.Write([]byte("ok"))
        }))
    }()

    time.Sleep(50 * time.Millisecond)

    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/test", nil)
    req.Header.Set("X-Custom", "test-value")
    resp, err := tunA.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusCreated {
        t.Fatalf("expected 201, got %d", resp.StatusCode)
    }
    if resp.Header.Get("X-Echo") != "test-value" {
        t.Fatalf("expected X-Echo: test-value, got %q", resp.Header.Get("X-Echo"))
    }
    body, _ := io.ReadAll(resp.Body)
    if string(body) != "ok" {
        t.Fatalf("expected ok, got %q", string(body))
    }
    cancel()
    <-srvErr
}
