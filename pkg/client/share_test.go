// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
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
	defer ts.Close()

	c := NewFileClient(ts.URL)
	link, err := c.CreateShare(context.Background(), "test.txt", time.Hour, 0, false)
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
	defer ts.Close()

	c := NewFileClient(ts.URL)
	shares, err := c.ListShares(context.Background())
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
	defer ts.Close()

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(context.Background(), "test_token"); err != nil {
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
	defer ts.Close()

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(context.Background(), "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent token")
	}
}
