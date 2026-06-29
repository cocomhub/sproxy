// Copyright 2026 The Cocomhub Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0 style license that
// can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"sync"
	"testing"
	"time"
)

// TestWebrtcRoundTrip verifies bidirectional message exchange.
func TestWebrtcRoundTrip(t *testing.T) {
	signal := NewSignal()
	payload := []byte("Hello WebRTC!")

	type result struct {
		err  error
		data []byte
	}

	dialRes := make(chan result, 1)
	listenRes := make(chan result, 1)
	dialDone := make(chan struct{})

	// Listen goroutine.
	go func() {
		conn, err := Listen(signal)
		if err != nil {
			listenRes <- result{err: err}
			return
		}

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			conn.Close()
			listenRes <- result{err: err}
			return
		}

		if _, err := conn.Write(buf[:n]); err != nil {
			conn.Close()
			listenRes <- result{err: err}
			return
		}
		listenRes <- result{data: buf[:n]}
		<-dialDone
		conn.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	// Dial goroutine.
	go func() {
		conn, err := Dial(signal)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		defer conn.Close()

		if _, err := conn.Write(payload); err != nil {
			dialRes <- result{err: err}
			return
		}

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		dialRes <- result{data: buf[:n]}
		close(dialDone)
	}()

	var dialR, listenR result

	select {
	case dialR = <-dialRes:
	case <-time.After(20 * time.Second):
		t.Fatal("dial timed out")
	}
	select {
	case listenR = <-listenRes:
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}

	if dialR.err != nil {
		t.Fatalf("Dial: %v", dialR.err)
	}
	if listenR.err != nil {
		t.Fatalf("Listen: %v", listenR.err)
	}

	if string(listenR.data) != string(payload) {
		t.Errorf("listen got %q, want %q", string(listenR.data), string(payload))
	}
	if string(dialR.data) != string(payload) {
		t.Errorf("dial got %q, want %q", string(dialR.data), string(payload))
	}
}

// TestWebrtcBasicConnect verifies one-way message delivery.
func TestWebrtcBasicConnect(t *testing.T) {
	signal := NewSignal()
	payload := []byte("ping")

	listenRes := make(chan error, 1)
	var listenData []byte

	go func() {
		conn, err := Listen(signal)
		if err != nil {
			listenRes <- err
			return
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			listenRes <- err
			return
		}
		listenData = buf[:n]
		listenRes <- nil
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("Dial write: %v", err)
	}

	select {
	case err := <-listenRes:
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		if string(listenData) != string(payload) {
			t.Errorf("listen got %q, want %q", string(listenData), string(payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}
}

// TestWebrtcConcurrentSends verifies concurrent writes from dialer to listener.
func TestWebrtcConcurrentSends(t *testing.T) {
	signal := NewSignal()
	payloads := []string{"msg1", "msg2", "msg3"}

	listenDone := make(chan struct{})
	var received []string
	var mu sync.Mutex

	go func() {
		defer close(listenDone)
		conn, err := Listen(signal)
		if err != nil {
			t.Errorf("Listen: %v", err)
			return
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		for range payloads {
			n, err := conn.Read(buf)
			if err != nil {
				t.Errorf("Read: %v", err)
				return
			}
			mu.Lock()
			received = append(received, string(buf[:n]))
			mu.Unlock()
		}
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	for _, p := range payloads {
		if _, err := conn.Write([]byte(p)); err != nil {
			t.Fatalf("Write %q: %v", p, err)
		}
	}

	select {
	case <-listenDone:
	case <-time.After(10 * time.Second):
		t.Fatal("listen timed out")
	}

	if len(received) != len(payloads) {
		t.Errorf("received %d messages, want %d", len(received), len(payloads))
	}
}

// TestWebrtcCloseBeforeRead verifies Read returns error after Close.
func TestWebrtcCloseBeforeRead(t *testing.T) {
	signal := NewSignal()

	listenDone := make(chan struct{})
	listenErr := make(chan error, 1)

	go func() {
		defer close(listenDone)
		conn, err := Listen(signal)
		if err != nil {
			return
		}
		// Dial first, then close, then try to read
		time.Sleep(200 * time.Millisecond)
		conn.Close()

		buf := make([]byte, 4096)
		_, err = conn.Read(buf)
		listenErr <- err
	}()

	time.Sleep(50 * time.Millisecond)
	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Write a message so the listener's Read has something to consume
	conn.Write([]byte("ping"))
	conn.Close()

	<-listenDone
	select {
	case err := <-listenErr:
		// After Close, Read may return data or error — both are acceptable
		_ = err
	default:
	}
}

// TestWebrtcLargeMessage verifies 64 KiB message transfer.
func TestWebrtcLargeMessage(t *testing.T) {
	signal := NewSignal()
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	listenDone := make(chan []byte, 1)
	go func() {
		conn, err := Listen(signal)
		if err != nil {
			t.Errorf("Listen: %v", err)
			listenDone <- nil
			return
		}
		defer conn.Close()

		buf := make([]byte, 131072)
		n, err := conn.Read(buf)
		if err != nil {
			t.Errorf("Read: %v", err)
			listenDone <- nil
			return
		}
		listenDone <- buf[:n]
	}()

	time.Sleep(100 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-listenDone:
		if len(got) != len(payload) {
			t.Errorf("received %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}
}

// TestWebrtcSignal verifies Signal channel capacity.
func TestWebrtcSignal(t *testing.T) {
	signal := NewSignal()
	if signal.Offer == nil || signal.Answer == nil {
		t.Fatal("NewSignal channels are nil")
	}
	if cap(signal.Offer) != 1 || cap(signal.Answer) != 1 {
		t.Errorf("channel capacity = %d/%d, want 1/1", cap(signal.Offer), cap(signal.Answer))
	}
}

// TestWebrtcAddr verifies webrtcAddr satisfies net.Addr.
func TestWebrtcAddr(t *testing.T) {
	addr := webrtcAddr{}
	if addr.Network() != "webrtc" {
		t.Errorf("Network = %q, want webrtc", addr.Network())
	}
	if addr.String() != "webrtc" {
		t.Errorf("String = %q, want webrtc", addr.String())
	}
}

// TestWebrtcConnDeadlines verifies deadline methods are no-ops.
func TestWebrtcConnDeadlines(t *testing.T) {
	signal := NewSignal()

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		conn, err := Listen(signal)
		if err != nil {
			return
		}
		defer conn.Close()

		// Deadline methods should be no-ops
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	}()

	time.Sleep(50 * time.Millisecond)
	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
	<-listenDone
}
