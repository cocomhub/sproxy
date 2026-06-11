package mux_test

import (
    "context"
    "io"
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

    streamA, err := muxA.Open(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamA.Close()

    streamB, err := muxB.Accept(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamB.Close()

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

func TestMuxCloseWrite(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    streamA, err := muxA.Open(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamA.Close()

    streamB, err := muxB.Accept(ctx)
    if err != nil {
        t.Fatal(err)
    }
    defer streamB.Close()

    // 发送数据 + CloseWrite
    if _, err := streamA.Write([]byte("hello")); err != nil {
        t.Fatal(err)
    }
    if err := streamA.CloseWrite(); err != nil {
        t.Fatal(err)
    }

    // B 读数据
    buf := make([]byte, 1024)
    n, err := streamB.Read(buf)
    if err != nil {
        t.Fatal(err)
    }
    if string(buf[:n]) != "hello" {
        t.Fatalf("expected %q, got %q", "hello", buf[:n])
    }

    // B 读 EOF（CloseWrite 信号）
    n, err = streamB.Read(buf)
    if err != io.EOF {
        t.Fatalf("expected io.EOF after CloseWrite, got %v (n=%d)", err, n)
    }
}

func TestMuxBidirectionalWrite(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    sA, err := muxA.Open(ctx)
    if err != nil { t.Fatal(err) }
    defer sA.Close()
    sB, err := muxB.Accept(ctx)
    if err != nil { t.Fatal(err) }
    defer sB.Close()

    // 双向同时写
    sA.Write([]byte("fromA"))
    sB.Write([]byte("fromB"))

    // A 读 fromB
    buf := make([]byte, 16)
    n, err := sA.Read(buf)
    if err != nil { t.Fatal(err) }
    if string(buf[:n]) != "fromB" {
        t.Fatalf("expected fromB, got %q", buf[:n])
    }

    // B 读 fromA
    n, err = sB.Read(buf)
    if err != nil { t.Fatal(err) }
    if string(buf[:n]) != "fromA" {
        t.Fatalf("expected fromA, got %q", buf[:n])
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

    bMap := make(map[mux.StreamID]*mux.Stream)
    for range n {
        s, err := muxB.Accept(ctx)
        if err != nil {
            t.Fatal(err)
        }
        bMap[s.ID()] = s
        defer s.Close()
    }

    for i, s := range streamsA {
        msg := []byte{byte('A' + i)}
        if _, err := s.Write(msg); err != nil {
            t.Fatal(err)
        }
    }

    for _, s := range streamsA {
        bs, ok := bMap[s.ID()]
        if !ok {
            t.Fatalf("stream %d not found on B", s.ID())
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
    if err != nil { t.Fatal(err) }

    streamB, err := muxB.Accept(ctx)
    if err != nil { t.Fatal(err) }

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

func TestMuxAcceptCancel(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
    defer cancel()

    // Accept 应因 ctx 取消而超时
    _, err := muxB.Accept(ctx)
    if err == nil {
        t.Fatal("expected error on cancelled accept")
    }
}

func TestMuxAcceptAfterClose(t *testing.T) {
    a, b := xfertest.Pipe()
    muxB := mux.New(b, mux.RoleListener)
    muxB.Close()
    _ = a

    ctx := context.Background()
    _, err := muxB.Accept(ctx)
    if err == nil {
        t.Fatal("expected error on accept after close")
    }
}

func TestMuxOpenAfterClose(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxA.Close()
    _ = b

    ctx := context.Background()
    _, err := muxA.Open(ctx)
    if err == nil {
        t.Fatal("expected error on open after close")
    }
}

func TestMuxDataForUnknownStream(t *testing.T) {
    // 测试 handleFrame 对未知流ID的 FrameData 静默丢弃
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    _ = muxA
    _ = b
    // 间接测试：通过 muxB 发送一个对 muxA 未知流的 FrameData
    // 利用 PipeConn 手动注入帧
    ctx := context.Background()
    // 直接通过 conn 发送一个指向不存在的 streamID 的帧
    rawFrame := mux.EncodeFrame(999, mux.FrameData, []byte("orphan data"))
    if err := b.Send(ctx, rawFrame); err != nil {
        t.Fatal(err)
    }
    // muxA 的 readLoop 会处理此帧，应该静默丢弃（不 panic）
    time.Sleep(100 * time.Millisecond)
    muxA.Close()
    b.Close()
}

func TestMuxFramePingPong(t *testing.T) {
    // 测试 Ping/Pong 心跳：创建 mux，等待足够长的时间让 ping 触发
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // 打开一条流，确保连接活跃
    s, err := muxA.Open(ctx)
    if err != nil {
        t.Fatal(err)
    }
    s.Close()
    s2, err := muxB.Accept(ctx)
    if err != nil { t.Fatal(err) }
    s2.Close()

    // 如果 Ping/Pong 正常工作，连接应保持活跃
    // （心跳默认 30s，测试只验证不崩溃）
    time.Sleep(50 * time.Millisecond)
}

func TestMuxWriteChClosedStream(t *testing.T) {
    a, b := xfertest.Pipe()
    muxA := mux.New(a, mux.RoleDialer)
    muxB := mux.New(b, mux.RoleListener)
    defer muxA.Close()
    defer muxB.Close()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    s, err := muxA.Open(ctx)
    if err != nil { t.Fatal(err) }

    // 先 CloseWrite 再 Write
    if err := s.CloseWrite(); err != nil {
        t.Fatal(err)
    }

    // 再写入应成功（CloseWrite 只发信号，不关闭流）
    _, err = s.Write([]byte("after closewrite"))
    if err != nil {
        t.Fatal(err) // 应允许，直到真正的 Close
    }
}
