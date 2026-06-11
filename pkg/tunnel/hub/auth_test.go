// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"testing"
)

func TestAuthenticator(t *testing.T) {
	// 非空 token：必须匹配
	a := NewAuthenticator("secret")
	if err := a.Authenticate("secret"); err != nil {
		t.Fatal("expected success for matching token")
	}
	if err := a.Authenticate("wrong"); err == nil {
		t.Fatal("expected error for wrong token")
	}

	// 空 token = 不鉴权
	a2 := NewAuthenticator("")
	if err := a2.Authenticate("anything"); err != nil {
		t.Fatal("expected success when relay token is empty")
	}
	if err := a2.Authenticate(""); err != nil {
		t.Fatal("expected success for empty token when relay token is empty")
	}

	// 非空 token，节点发送空 token 应拒绝
	a3 := NewAuthenticator("required")
	if err := a3.Authenticate(""); err == nil {
		t.Fatal("expected error when relay token is set but node token is empty")
	}
}
