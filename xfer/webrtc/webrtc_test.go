// Copyright 2026 The Cocomhub Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0 style license that
// can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
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
