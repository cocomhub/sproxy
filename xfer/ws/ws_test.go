// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xferws_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	xferws "github.com/cocomhub/sproxy/xfer/ws"
)

func TestTransportRegistered(t *testing.T) {
	tr := xfer.Get("ws")
	if tr == nil {
		t.Fatal("ws transport not registered after importing xferws")
	}
	if tr.Name != "ws" {
		t.Fatalf("expected transport name 'ws', got %q", tr.Name)
	}
	if tr.Dial == nil {
		t.Fatal("ws transport Dial is nil")
	}
	if tr.Listen == nil {
		t.Fatal("ws transport Listen is nil")
	}
}

func TestWSConnRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		_, msg, err := c.Read(context.Background())
		if err != nil {
			return
		}
		c.Write(context.Background(), websocket.MessageBinary, msg)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"

	ctx := context.Background()
	conn, err := xferws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := []byte("hello xfer ws")
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

func TestWSConnMultipleMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		for {
			_, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), websocket.MessageBinary, bytes.ToUpper(msg))
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"

	ctx := context.Background()
	conn, err := xferws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	msgs := []string{"one", "two", "three"}
	for _, msg := range msgs {
		payload := []byte(msg)
		if err := conn.Send(ctx, payload); err != nil {
			t.Fatal(err)
		}
		resp, err := conn.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.ToUpper(payload)
		if !bytes.Equal(resp, want) {
			t.Fatalf("expected %q, got %q", want, resp)
		}
	}
}

func TestWSListenerAcceptSendReceive(t *testing.T) {
	ctx := context.Background()

	l, err := xferws.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	addr := l.(interface{ Addr() net.Addr }).Addr().String()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		msg, err := conn.Receive(ctx)
		if err != nil {
			return
		}
		if err := conn.Send(ctx, msg); err != nil {
			return
		}
	}()

	wsURL := "ws://" + addr + "/ws"

	conn, err := xferws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := []byte("hello ws listener")
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

	<-done
}
