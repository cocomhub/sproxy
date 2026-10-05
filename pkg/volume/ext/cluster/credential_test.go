// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

// credential_test.go 验证凭证签发/验签（集群出口核心安全件，用户裁定凭证下发）：
// 签发→验签成功；篡改/过期/scope/路径越界/载荷缺失 → fail-closed 哨兵错误。

import (
	"testing"
	"time"
)

// newTestCredential 构造一个已签发 read 凭证（全卷范围）。
func newTestCredential(t *testing.T, sk []byte) Credential {
	t.Helper()
	c := Credential{
		Node:       "holder-a",
		Volume:     "main",
		Owner:      "alice",
		Recipient:  "egress-fp",
		Scope:      "read",
		PathPrefix: "",
		IssuedAt:   time.Now().Add(-time.Minute).Unix(),
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	}
	c.Sign(sk)
	return c
}

// TestCredential_IssuedVerify_Ok：签发 → 目标节点验签成功 + 授权范围匹配。
func TestCredential_IssuedVerify_Ok(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	c := newTestCredential(t, sk)

	if err := c.Verify(sk, time.Now()); err != nil {
		t.Fatalf("验签应成功: %v", err)
	}
	rel := "user/dir/f.bin"
	if err := c.Authorizes("holder-a", "main", "alice", rel); err != nil {
		t.Fatalf("授权应成功: %v", err)
	}
	// 范围外节点/卷/owner → 拒绝。
	if err := c.Authorizes("holder-b", "main", "alice", rel); err == nil {
		t.Fatal("其他节点应拒绝")
	}
	if err := c.Authorizes("holder-a", "other", "alice", rel); err == nil {
		t.Fatal("其他卷应拒绝")
	}
	if err := c.Authorizes("holder-a", "main", "bob", rel); err == nil {
		t.Fatal("其他 owner 应拒绝")
	}
}

// TestCredential_TamperedRejected：篡改载荷（node/owner/scope）→ 签名不符拒绝。
func TestCredential_TamperedRejected(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	base := newTestCredential(t, sk)

	// 篡改 node。
	tampered := base
	tampered.Node = "evil-node"
	if err := tampered.Verify(sk, time.Now()); err == nil {
		t.Fatal("篡改 node 应验签失败")
	}
	// 篡改 owner。
	tampered2 := base
	tampered2.Owner = "mallory"
	if err := tampered2.Verify(sk, time.Now()); err == nil {
		t.Fatal("篡改 owner 应验签失败")
	}
	// 篡改 scope。
	tampered3 := base
	tampered3.Scope = "write"
	if err := tampered3.Verify(sk, time.Now()); err == nil {
		t.Fatal("篡改 scope 应验签失败")
	}
}

// TestCredential_WrongKeyRejected：用错误 SK 验签 → 拒绝（签发方密钥不对称）。
func TestCredential_WrongKeyRejected(t *testing.T) {
	t.Parallel()
	c := newTestCredential(t, []byte("key-one-key-one-key-one-key-000"))
	if err := c.Verify([]byte("key-two-key-two-key-two-key-000"), time.Now()); err == nil {
		t.Fatal("错误 SK 验签应失败")
	}
}

// TestCredential_ExpiredRejected：过期凭证 → ErrCredentialExpired。
func TestCredential_ExpiredRejected(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	c := Credential{
		Node: "h", Volume: "v", Owner: "o", Recipient: "fp", Scope: "read",
		IssuedAt:  time.Now().Add(-2 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-time.Hour).Unix(), // 已过期
	}
	c.Sign(sk)
	if err := c.Verify(sk, time.Now()); err != ErrCredentialExpired {
		t.Fatalf("过期应 ErrCredentialExpired, got %v", err)
	}
}

// TestCredential_PathPrefixScope：path_prefix 限定路径范围——范围内放行、越界拒绝。
func TestCredential_PathPrefixScope(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	c := Credential{
		Node: "h", Volume: "v", Owner: "o", Recipient: "fp", Scope: "read",
		PathPrefix: "docs/",
		IssuedAt:   time.Now().Add(-time.Minute).Unix(),
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	}
	c.Sign(sk)
	if err := c.Verify(sk, time.Now()); err != nil {
		t.Fatalf("验签应成功: %v", err)
	}
	if err := c.Authorizes("h", "v", "o", "docs/a.bin"); err != nil {
		t.Fatalf("范围内应放行: %v", err)
	}
	if err := c.Authorizes("h", "v", "o", "other/f.bin"); err != ErrCredentialPath {
		t.Fatalf("越界应 ErrCredentialPath, got %v", err)
	}
}

// TestCredential_MalformedRejected：缺字段载荷 → 拒绝。
func TestCredential_MalformedRejected(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	c := Credential{Node: "h"} // 缺 Volume/Owner/Recipient/Scope
	c.Sign(sk)
	if err := c.Verify(sk, time.Now()); err != ErrCredentialMalformed {
		t.Fatalf("缺字段应 ErrCredentialMalformed, got %v", err)
	}
}

// TestCredential_MarshalRoundtrip：编码→解析往返保留字段（签发→下发链路）。
func TestCredential_MarshalRoundtrip(t *testing.T) {
	t.Parallel()
	sk := []byte("0123456789abcdef0123456789abcdef")
	c := newTestCredential(t, sk)
	enc, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	parsed, perr := ParseCredential(enc)
	if perr != nil {
		t.Fatalf("ParseCredential: %v", perr)
	}
	if parsed.Node != c.Node || parsed.Volume != c.Volume || parsed.Owner != c.Owner ||
		parsed.Scope != c.Scope || parsed.ExpiresAt != c.ExpiresAt || parsed.Sig != c.Sig {
		t.Fatalf("往返不一致: %+v vs %+v", parsed, c)
	}
}
