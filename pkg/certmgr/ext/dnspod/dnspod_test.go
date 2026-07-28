// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dnspod

import (
	"context"
	"testing"
)

func TestNewProvider(t *testing.T) {
	p := New(Config{
		SecretId:  "test-secret-id",
		SecretKey: "test-secret-key",
	})
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.config.SecretId != "test-secret-id" {
		t.Errorf("SecretId = %q, want %q", p.config.SecretId, "test-secret-id")
	}
	if p.config.SecretKey != "test-secret-key" {
		t.Errorf("SecretKey = %q, want %q", p.config.SecretKey, "test-secret-key")
	}
}

func TestSetDNSRecord_EmptyConfig(t *testing.T) {
	p := New(Config{})
	err := p.SetDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestCleanupDNSRecord_EmptyConfig(t *testing.T) {
	p := New(Config{})
	err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "keyauth")
	if err == nil {
		t.Error("expected error for empty config")
	}
}
