// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer_test

import (
    "bytes"
    "context"
    "io"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func TestRegisterAndGet(t *testing.T) {
    // 验证空注册表 Get 返回 nil
    if got := xfer.Get("nonexistent"); got != nil {
        t.Fatal("expected nil for unknown transport")
    }

    // 注册一个测试 Transport
    t1 := &xfer.Transport{Name: "test", Dial: nil, Listen: nil}
    xfer.Register(t1)
    if got := xfer.Get("test"); got != t1 {
        t.Fatal("expected registered transport")
    }

    // 重复注册不会 panic（新 Registry 覆盖而非 panic）
    xfer.Register(t1)
}

func TestRegisterNilPanics(t *testing.T) {
    // 注册 nil Transport 因访问 nil 字段而 panic
    defer func() {
        if r := recover(); r == nil {
            t.Fatal("expected panic on nil Transport")
        }
    }()
    xfer.Register(nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
    defer func() {
        if r := recover(); r == nil {
            t.Fatal("expected panic on empty name")
        }
    }()
    xfer.Register(&xfer.Transport{Name: ""})
}

func TestHTTPConnRoundTrip(t *testing.T) {
    // server: /tunnel 接收 POST，原样返回 body
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != "POST" {
            t.Errorf("expected POST, got %s", r.Method)
        }
        body, err := io.ReadAll(r.Body)
        if err != nil {
            t.Fatal(err)
            return
        }
        w.Write(body)
    }))
    defer srv.Close()

    ctx := context.Background()
    conn, err := xfer.DialHTTP(ctx, srv.URL)
    if err != nil {
        t.Fatal(err)
    }
    defer conn.Close()

    payload := []byte("hello xfer")
    if err := conn.Send(ctx, payload); err != nil {
        t.Fatal(err)
    }

    resp, err := conn.Receive(ctx)
    if err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(resp, payload) {
        t.Fatalf("expected %q, got %q", payload, resp)
    }
}

func TestHTTPConnMultipleRoundTrips(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        body, _ := io.ReadAll(r.Body)
        // 返回大写版
        w.Write(bytes.ToUpper(body))
    }))
    defer srv.Close()

    ctx := context.Background()
    conn, err := xfer.DialHTTP(ctx, srv.URL)
    if err != nil {
        t.Fatal(err)
    }
    defer conn.Close()

    msgs := []string{"one", "two", "three"}
    for _, msg := range msgs {
        if err := conn.Send(ctx, []byte(msg)); err != nil {
            t.Fatal(err)
        }
        resp, err := conn.Receive(ctx)
        if err != nil {
            t.Fatal(err)
        }
        want := bytes.ToUpper([]byte(msg))
        if !bytes.Equal(resp, want) {
            t.Fatalf("expected %q, got %q", want, resp)
        }
    }
}
