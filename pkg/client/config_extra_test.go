// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"path/filepath"
	"testing"
)

func TestHandleConfigSet(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sclient.yaml")
	cfg := DefaultConfig()
	if err := SaveConfig(cfg, cfgPath); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"set server_url", "server_url", "http://test:8080", false},
		{"set access_key", "access_key", "my-ak", false},
		{"set access_key_secret", "access_key_secret", "my-sk", false},
		{"set tunnel_key", "tunnel_key", "abcd1234", false},
		{"set timeout", "timeout", "60", false},
		{"set chunk_size", "chunk_size", "4194304", false},
		{"set max_chunk_size", "max_chunk_size", "67108864", false},
		{"set invalid timeout", "timeout", "not-a-number", true},
		{"set invalid chunk_size", "chunk_size", "bad-value", true},
		{"set invalid max_chunk_size", "max_chunk_size", "bad-value", true},
		{"set default_dir", "default_dir", "/my/dir", true},
		{"set unknown key", "unknown_key", "value", true},
		{"set empty key", "", "value", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := HandleConfigSet(cfg, cfgPath, tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("HandleConfigSet(%q, %q) error = %v, wantErr = %v", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestApplyConfigSet(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"set server_url", "server_url", "http://test:8080", false},
		{"set access_key", "access_key", "my-ak", false},
		{"set access_key_secret", "access_key_secret", "my-sk", false},
		{"set tunnel_key", "tunnel_key", "abcd1234", false},
		{"set timeout", "timeout", "60", false},
		{"set chunk_size", "chunk_size", "4194304", false},
		{"set max_chunk_size", "max_chunk_size", "67108864", false},
		{"set invalid timeout", "timeout", "not-a-number", true},
		{"set invalid chunk_size", "chunk_size", "bad-value", true},
		{"set invalid max_chunk_size", "max_chunk_size", "bad-value", true},
		{"set unknown key", "unknown_key", "value", true},
		{"set empty key", "", "value", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			err := ApplyConfigSet(cfg, tt.key, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("ApplyConfigSet(%q, %q) error = %v, wantErr = %v", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}
