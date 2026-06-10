// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
    "context"
    "testing"
    "time"

    "github.com/cocomhub/sproxy/pkg/tunnel/mux"
    "github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestMuxOpenAccept(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // A 打开一条流
    streamA, err := muxA.Open(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamA.Close()

    // B 接受一条流
    streamB, err := muxB.Accept(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamB.Close()

    // A 写 → B 读
    msg := []byte("hello mux")
    n, err := streamA.Write(msg)
    if err != nil {
        t.Fatal(err)
    }
    if n != len(msg) {
        t.Fatalf("wrote %d, expected %d", n, len(msg))
    }

    buf := make([]byte, 1024)
    n, err = streamB.Read(buf)
    if err != nil {
        t.Fatal(err)
    }
    if string(buf[:n]) != string(msg) {
        t.Fatalf("expected %q, got %q", msg, buf[:n])
    }

    // B 写 → A 读 (双向)
    reply := []byte("pong")
    n, err = streamB.Write(reply)
    if err != nil {
        t.Fatal(err)
    }
    buf = make([]byte, 1024)
    n, err = streamA.Read(buf)
    if err != nil {
        t.Fatal(err)
    }
    if string(buf[:n]) != string(reply) {
        t.Fatalf("expected %q, got %q", reply, buf[:n])
    }
}

func TestMuxMultipleStreams(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    const n = 5
    streamsA := make([]*mux.Stream, n)
    for i := range n {
        s, err := muxA.Open(ctx)
        if err != nil {
            t.Fatal(err)
        }
        defer s.Close()
        streamsA[i] = s
    }

    // 接受全部并映射
    bMap := make(map[mux.StreamID]*mux.Stream)
    for range n {
        s, err := muxB.Accept(ctx)
        if err != nil {
            t.Fatal(err)
        }
        bMap[s.ID()] = s
        defer s.Close()
    }

    // 每条流独立写入
    for i, s := range streamsA {
        msg := []byte{byte('A' + i)}
        if _, err := s.Write(msg); err != nil {
            t.Fatal(err)
        }
    }

    // B 端读取
    for _, s := range streamsA {
        bs, ok := bMap[s.ID()]
        if !ok {
            t.Fatalf("stream %d not found on B side", s.ID())
        }
        buf := make([]byte, 1024)
        n, err := bs.Read(buf)
        if err != nil {
            t.Fatal(err)
        }
        if n != 1 {
            t.Fatalf("expected 1 byte, got %d", n)
        }
    }
}

func TestMuxClose(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    streamA, err := muxA.Open(ctx)
    if err != nil {
        t.Fatal(err)
    }

    streamB, err := muxB.Accept(ctx)
    if err != nil {
        t.Fatal(err)
    }

    // 关闭 muxA
    muxA.Close()

    // 写入应失败
    _, err = streamA.Write([]byte("data"))
    if err == nil {
        t.Fatal("expected error on write after mux close")
    }

    // B 端读应得到错误
    buf := make([]byte, 1024)
    _, err = streamB.Read(buf)
    if err == nil {
        t.Fatal("expected error on read after mux close")
    }

    muxB.Close()
}
