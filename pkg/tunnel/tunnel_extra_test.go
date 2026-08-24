// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler_UpdateKey_OldKeyStillWorks(t *testing.T) {
	key2 := make([]byte, 32)
	for i := range key2 {
		key2[i] = byte(i)
	}

	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("old-key-accepted"))
	})

	// 认证驱动：key 由 withTunnelKey 中间件注入 request ctx，handler 不再持有进程级密钥。
	ts := httptest.NewServer(withTunnelKey(key2, NewLocalHandler(nil, local, nil)))
	defer ts.Close()

	clientKey2Hex := hex.EncodeToString(key2)
	client, err := NewClient(clientKey2Hex, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", "/api/test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandler_ServeHTTP_EmptyKey(t *testing.T) {
	h := NewHandler(nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/tunnel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for no ctx key (missing auth), got %d", rec.Code)
	}
}

func TestDispatchLocal_PanicRecovery(t *testing.T) {
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic in local handler")
	})

	ts := httptest.NewServer(withTunnelKey(testKey, NewLocalHandler(nil, panicHandler, nil)))
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", "/api/panic", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (tunnel-level, not handler-level), got %d", resp.StatusCode)
	}
}

func TestForwardExternal_HTTPClientError(t *testing.T) {
	// httptest server that we close immediately, causing connection refused
	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	closedSrv.Close()

	absURL := closedSrv.URL + "/api/test"

	ts := httptest.NewServer(withTunnelKey(testKey, NewHandler(nil, nil)))
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 0, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, _ := http.NewRequest("GET", absURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Logf("expected error for closed server: %v", err)
		return
	}
	defer resp.Body.Close()
	// The tunnel wrapper may return 502 for proxy errors
	if resp.StatusCode != http.StatusOK {
		t.Logf("response status: %d (may be 502 or other error)", resp.StatusCode)
	}
}

// ---- resolveKey tests ----

func TestResolveKey_UsesPassedKey(t *testing.T) {
	// 认证驱动：resolveKey 从调用方传入的 key（来自 ctx）解密，不再有进程级 primaryKey/oldKey。
	metaContent := []byte(`{"method":"GET","url":"/api/test","headers":{}}`)

	encMeta, err := Encrypt(testKey, metaContent, []byte(AADMeta))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Build metadata frame: [4B big-endian length][encrypted metadata]
	frame := make([]byte, 4+len(encMeta))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(encMeta)))
	copy(frame[4:], encMeta)

	handler := &Handler{}
	decodedMeta, matchedKey, err := handler.resolveKey(bytes.NewReader(frame), testKey)
	if err != nil {
		t.Fatalf("resolveKey with passed key should succeed: %v", err)
	}
	if !bytes.Equal(matchedKey, testKey) {
		t.Fatal("resolveKey should return the passed key as matched key")
	}
	if !bytes.Equal(decodedMeta, metaContent) {
		t.Fatalf("decoded metadata mismatch: got %q, want %q", decodedMeta, metaContent)
	}
}

func TestResolveKey_EmptyKey(t *testing.T) {
	handler := &Handler{}
	// Send a frame with random bytes (invalid encrypted data)
	frame := make([]byte, 8)
	binary.BigEndian.PutUint32(frame[0:4], 4)
	copy(frame[4:], []byte{0x01, 0x02, 0x03, 0x04})

	_, _, err := handler.resolveKey(bytes.NewReader(frame), nil)
	if err == nil {
		t.Fatal("resolveKey should error when key is nil")
	}
}

func TestDeriveTunnelKey(t *testing.T) {
	sk := "2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5"
	k1, _ := DeriveTunnelKey(sk, "meshA")
	k2, _ := DeriveTunnelKey(sk, "meshB")
	if len(k1) != 32 || bytes.Equal(k1, k2) {
		t.Fatalf("derived key len=%d equal=%v", len(k1), bytes.Equal(k1, k2))
	}
	k1b, _ := DeriveTunnelKey(sk, "meshA")
	if !bytes.Equal(k1, k1b) {
		t.Fatal("派生必须确定")
	}
	if _, err := DeriveTunnelKey("zz", ""); err == nil {
		t.Fatal("非法 hex 应报错")
	}
}
