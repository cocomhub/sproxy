// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestEncodeFrame_RejectsOversizePayload 钉住「帧负载上限」不再静默截断（issue #213）。
//
// 帧头 Length 只有 2 字节（上限 65535）。修复前超出部分被**静默截断**：发送方以为发出 N 字节、
// 对端只收到 65535，字节流从此错位（上层分块加密表现为 GCM 认证失败）。现在必须**显式报错**。
func TestEncodeFrame_RejectsOversizePayload(t *testing.T) {
	// 边界：65535 必须成功
	f, err := EncodeFrame(7, FrameData, make([]byte, MaxFramePayload))
	if err != nil {
		t.Fatalf("恰好 %d 字节应编码成功: %v", MaxFramePayload, err)
	}
	if got := len(f); got != headerSize+MaxFramePayload {
		t.Fatalf("帧长度 = %d, want %d", got, headerSize+MaxFramePayload)
	}

	// 越界：65536 必须报 ErrFrameTooLarge（**不得**返回被截断的帧）
	if f, err := EncodeFrame(7, FrameData, make([]byte, MaxFramePayload+1)); err == nil {
		t.Fatalf("%d 字节应报错（不得静默截断为 %d），got frame len=%d",
			MaxFramePayload+1, MaxFramePayload, len(f))
	} else if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("错误应为 ErrFrameTooLarge（可判定）, got %v", err)
	}
}

// TestStreamWrite_SplitsAtMaxFramePayload 钉住流写不再产生超限负载（issue #213 的**根因**）。
//
// 场景：流的发送窗口是 DefaultWindowSize=65536，而单帧负载上限是 65535。修复前
// `stream.Write` 允许一个恰好 65536 字节的写，`EncodeFrame` 把它截到 65535 ⇒ 对端少收 1 字节
// ⇒ 整条字节流错位。
//
// 本用例**确定性地**构造该边界（一次 65536 字节写 + 初始满窗口），断言对端收到**完整**
// 65536 字节（拆成 65535 + 1 两帧）。
func TestStreamWrite_SplitsAtMaxFramePayload(t *testing.T) {
	a, b := xfertest.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	mListener := New(b, RoleListener)
	defer func() { _ = mListener.Close() }()
	mDialer := New(a, RoleDialer)
	defer func() { _ = mDialer.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 对端接受流并把收到的字节累积起来
	gotCh := make(chan []byte, 1)
	go func() {
		s, err := mListener.Accept(ctx)
		if err != nil {
			gotCh <- nil
			return
		}
		buf := make([]byte, 0, 70000)
		tmp := make([]byte, 4096)
		for len(buf) < 65536 {
			n, rErr := s.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if rErr != nil {
				break
			}
		}
		gotCh <- buf
	}()

	s, err := mDialer.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 一次写满窗口（65536 = DefaultWindowSize），修复前这一步会丢 1 字节。
	payload := bytes.Repeat([]byte{0x5a}, DefaultWindowSize)
	written := 0
	for written < len(payload) {
		n, wErr := s.Write(payload[written:])
		if wErr != nil {
			t.Fatalf("Write: %v", wErr)
		}
		if n == 0 {
			t.Fatal("Write 返回 0 字节（无进展）")
		}
		written += n
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	select {
	case got := <-gotCh:
		if len(got) != len(payload) {
			t.Fatalf("对端收到 %d 字节, want %d（少收 = 帧负载上限未收敛，字节流已错位）", len(got), len(payload))
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("对端收到内容不符")
		}
	case <-ctx.Done():
		t.Fatal("超时：对端未收到数据")
	}
}
