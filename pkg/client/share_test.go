// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateShare(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/share" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"abc123","filename":"test.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	link, err := c.CreateShare(t.Context(), "test.txt", WithShareTTL(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if link.Token != "abc123" {
		t.Errorf("expected token abc123, got %s", link.Token)
	}
	if link.Filename != "test.txt" {
		t.Errorf("expected filename test.txt, got %s", link.Filename)
	}
}

func TestListShares(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/shares" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"shares":[{"token":"abc","filename":"a.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false,"expired":false}]}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	shares, err := c.ListShares(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(shares))
	}
	if shares[0].Token != "abc" {
		t.Errorf("expected token abc, got %s", shares[0].Token)
	}
}

func TestListShares_Empty(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/shares" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"shares":[]}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	shares, err := c.ListShares(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Errorf("expected 0 shares, got %d", len(shares))
	}
}

func TestRevokeShare(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/shares/test_token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message":"分享链接已撤销"}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(t.Context(), "test_token"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeShareNotFound(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"message":"分享链接不存在"}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(t.Context(), "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent token")
	}
}

// ---- ShareOption functions ----

func TestWithShareTTL(t *testing.T) {
	o := &shareOptions{}
	WithShareTTL(2 * time.Hour)(o)
	if o.ttl != 2*time.Hour {
		t.Errorf("ttl = %v, want 2h", o.ttl)
	}
}

func TestWithShareTTL_Zero(t *testing.T) {
	o := &shareOptions{ttl: 1 * time.Hour}
	WithShareTTL(0)(o)
	if o.ttl != 1*time.Hour {
		t.Errorf("ttl should remain unchanged, got %v", o.ttl)
	}
}

func TestWithShareMaxDownloads(t *testing.T) {
	o := &shareOptions{}
	WithShareMaxDownloads(5)(o)
	if o.maxDownloads != 5 {
		t.Errorf("maxDownloads = %d, want 5", o.maxDownloads)
	}
}

func TestWithShareMaxDownloads_Zero(t *testing.T) {
	o := &shareOptions{maxDownloads: 3}
	WithShareMaxDownloads(0)(o)
	if o.maxDownloads != 3 {
		t.Errorf("maxDownloads should remain unchanged, got %d", o.maxDownloads)
	}
}

func TestWithShareOneTime(t *testing.T) {
	o := &shareOptions{}
	WithShareOneTime()(o)
	if !o.oneTime {
		t.Error("expected oneTime=true")
	}
}

// ---- ShareLink time methods ----

func TestShareLink_CreatedAtTime(t *testing.T) {
	s := &ShareLink{CreatedAt: "2026-07-24T12:00:00Z"}
	tm, err := s.CreatedAtTime()
	if err != nil {
		t.Fatalf("CreatedAtTime: %v", err)
	}
	expected := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if !tm.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, tm)
	}
}

func TestShareLink_CreatedAtTime_Invalid(t *testing.T) {
	s := &ShareLink{CreatedAt: "invalid-date"}
	_, err := s.CreatedAtTime()
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
}

func TestShareLink_CreatedAtTime_NilReceiver(t *testing.T) {
	var s *ShareLink
	_, err := s.CreatedAtTime()
	if err == nil {
		t.Fatal("expected error for nil receiver")
	}
}

func TestShareLink_ExpiresAtTime(t *testing.T) {
	s := &ShareLink{ExpiresAt: "2026-07-25T12:00:00Z"}
	tm, err := s.ExpiresAtTime()
	if err != nil {
		t.Fatalf("ExpiresAtTime: %v", err)
	}
	expected := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if !tm.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, tm)
	}
}

func TestShareLink_ExpiresAtTime_Invalid(t *testing.T) {
	s := &ShareLink{ExpiresAt: "bad-date"}
	_, err := s.ExpiresAtTime()
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
}

func TestShareLink_ExpiresAtTime_NilReceiver(t *testing.T) {
	var s *ShareLink
	_, err := s.ExpiresAtTime()
	if err == nil {
		t.Fatal("expected error for nil receiver")
	}
}

func TestCreateShare_WithOptions(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/share" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"abc123","filename":"test.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":5,"downloads":0,"one_time":true,"expired":false}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	link, err := c.CreateShare(t.Context(), "test.txt",
		WithShareTTL(time.Hour),
		WithShareMaxDownloads(5),
		WithShareOneTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if link.Token != "abc123" {
		t.Errorf("expected token abc123, got %s", link.Token)
	}
	if link.MaxDownloads != 5 {
		t.Errorf("expected max_downloads 5, got %d", link.MaxDownloads)
	}
	if !link.OneTime {
		t.Error("expected one_time=true")
	}
}

func TestCreateShare_ServerError(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"message":"server error"}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	_, err := c.CreateShare(t.Context(), "test.txt")
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestCreateShare_EmptyFilename(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 即使请求不合法，mock 服务端也返回 400
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"message":"empty filename"}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	_, err := c.CreateShare(t.Context(), "")
	if err == nil {
		t.Fatal("expected error for empty filename")
	}
}

func TestRevokeShare_ServerError(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(t.Context(), "token"); err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestRevokeShare_ResponseError(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":false,"message":"already revoked"}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(t.Context(), "token"); err == nil {
		t.Fatal("expected error for success=false response")
	}
}
