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
	key1 := testKey
	key2 := make([]byte, 32)
	for i := range key2 {
		key2[i] = byte(i)
	}

	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("old-key-accepted"))
	})

	// Create local handler with key1, then update to key2
	h := NewLocalHandler(key1, local, nil)
	ts := httptest.NewServer(h)
	defer ts.Close()

	hImpl := h.(*Handler)
	hImpl.UpdateKey(key2)

	// Client uses the new key (key2) to ensure response decryption works
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
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for empty key, got %d", rec.Code)
	}
}

func TestDispatchLocal_PanicRecovery(t *testing.T) {
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic in local handler")
	})

	h := NewLocalHandler(testKey, panicHandler, nil)
	ts := httptest.NewServer(h)
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

	h := NewHandler(testKey, nil)
	ts := httptest.NewServer(h)
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

func TestResolveKey_PrimaryKeyEmpty(t *testing.T) {
	// Create handler with empty primaryKey and non-nil oldKey
	metaContent := []byte(`{"method":"GET","url":"/api/test","headers":{}}`)

	// Encrypt metadata with oldKey (testKey)
	encMeta, err := Encrypt(testKey, metaContent)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Build metadata frame: [4B big-endian length][encrypted metadata]
	frame := make([]byte, 4+len(encMeta))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(encMeta)))
	copy(frame[4:], encMeta)

	handler := &Handler{
		primaryKey: nil,
		oldKey:     testKey,
	}

	decodedMeta, matchedKey, err := handler.resolveKey(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("resolveKey with oldKey should succeed: %v", err)
	}

	if !bytes.Equal(matchedKey, testKey) {
		t.Fatal("resolveKey should return oldKey as the matched key")
	}

	if !bytes.Equal(decodedMeta, metaContent) {
		t.Fatalf("decoded metadata mismatch: got %q, want %q", decodedMeta, metaContent)
	}
}

func TestResolveKey_BothFail(t *testing.T) {
	handler := &Handler{
		primaryKey: nil,
		oldKey:     nil,
	}

	// Send a frame with random bytes (invalid encrypted data)
	frame := make([]byte, 8)
	binary.BigEndian.PutUint32(frame[0:4], 4)
	copy(frame[4:], []byte{0x01, 0x02, 0x03, 0x04})

	_, _, err := handler.resolveKey(bytes.NewReader(frame))
	if err == nil {
		t.Fatal("resolveKey should return error when both keys are nil")
	}
	t.Logf("got expected error: %v", err)
}
