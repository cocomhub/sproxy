// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestFileClient_IdentityAndPeerFingerprints 验证 WithIdentity / WithPeerFingerprints
// 选项应用到 FileClient，且默认（未配置）时保持零值（现状兼容）。
func TestFileClient_IdentityAndPeerFingerprints(t *testing.T) {
	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	c := NewFileClient("https://127.0.0.1:18083",
		WithIdentity(id),
		WithPeerFingerprints([]string{id.Fingerprint()}))
	if c.identity != id {
		t.Error("WithIdentity 未生效")
	}
	if len(c.peerFingerprints) != 1 || c.peerFingerprints[0] != id.Fingerprint() {
		t.Errorf("WithPeerFingerprints 未生效: %v", c.peerFingerprints)
	}

	// 未配置时保持零值（行为与现状完全一致）。
	c2 := NewFileClient("https://127.0.0.1:18083")
	if c2.identity != nil {
		t.Error("默认 identity 应为 nil")
	}
	if len(c2.peerFingerprints) != 0 {
		t.Error("默认 peerFingerprints 应为空")
	}
}

// TestFileClient_TunnelOptsNil 验证未配置身份/pin 时 tunnelOpts 为空切片，
// NewTunnel 行为与旧签名完全一致。
func TestFileClient_TunnelOptsNil(t *testing.T) {
	c := NewFileClient("https://127.0.0.1:18083")
	opts := c.tunnelOpts()
	if len(opts) != 0 {
		t.Fatalf("tunnelOpts 应为空, got %d", len(opts))
	}
}
