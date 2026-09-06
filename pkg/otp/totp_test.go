// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otp

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// rfc6238Vector SHA1 的 RFC 6238 §B 官方测试向量。
// 种子是 ASCII 字符串 "12345678901234567890"（20 字节），时间戳 + 期望 6 位码。
func TestRFC6238Vector(t *testing.T) {
	secret := []byte("12345678901234567890")
	tp := NewTOTP(secret)

	vectors := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}

	for _, v := range vectors {
		t.Run(v.want, func(t *testing.T) {
			got, err := tp.Code(time.Unix(v.unix, 0))
			if err != nil {
				t.Fatalf("Code: unexpected error: %v", err)
			}
			if got != v.want {
				t.Fatalf("Code(%d) = %q, want %q", v.unix, got, v.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tp := NewTOTP([]byte("12345678901234567890"))
	now := time.Unix(1234567890, 0) // 期望码 005924

	codeNow, err := tp.Code(now)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}

	codePrev, err := tp.Code(now.Add(-30 * time.Second))
	if err != nil {
		t.Fatalf("Code(now-30s): %v", err)
	}
	codeNext, err := tp.Code(now.Add(30 * time.Second))
	if err != nil {
		t.Fatalf("Code(now+30s): %v", err)
	}
	codeFarPrev, err := tp.Code(now.Add(-60 * time.Second))
	if err != nil {
		t.Fatalf("Code(now-60s): %v", err)
	}
	codeFarNext, err := tp.Code(now.Add(60 * time.Second))
	if err != nil {
		t.Fatalf("Code(now+60s): %v", err)
	}

	tests := []struct {
		name   string
		code   string
		window int
		want   bool
	}{
		{"correct code", codeNow, 1, true},
		{"correct code window 0", codeNow, 0, true},
		{"wrong code", "000000", 1, false},
		{"empty code", "", 1, false},
		{"prev window 1", codePrev, 1, true},
		{"next window 1", codeNext, 1, true},
		{"prev window 2", codePrev, 2, true},
		{"next window 2", codeNext, 2, true},
		{"far-prev window 2", codeFarPrev, 2, true},
		{"far-next window 2", codeFarNext, 2, true},
		{"far-prev window 1 out of window", codeFarPrev, 1, false},
		{"far-next window 1 out of window", codeFarNext, 1, false},
		{"non-decimal code", "abcd12", 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tp.Validate(tt.code, now, tt.window); got != tt.want {
				t.Fatalf("Validate(code=%q, window=%d) = %v, want %v", tt.code, tt.window, got, tt.want)
			}
		})
	}
}

func TestGenerateSecret(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: unexpected error: %v", err)
	}
	if len(a) != 20 {
		t.Fatalf("GenerateSecret length = %d, want 20", len(a))
	}

	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret (2nd): unexpected error: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two GenerateSecret calls returned identical secrets")
	}
}

func TestURI(t *testing.T) {
	// RFC 4648 base32 无 padding： "12345678901234567890" → GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ（40 字节无 padding）
	secret := []byte("12345678901234567890")

	tp := NewTOTP(secret)
	got := tp.URI("alice@example.com", "Cocomhub")

	if !strings.HasPrefix(got, "otpauth://totp/") {
		t.Fatalf("URI missing scheme prefix: %q", got)
	}
	sep := strings.Index(got, "?")
	if sep < 0 {
		t.Fatalf("URI missing query: %q", got)
	}
	// url.PathEscape 对 label 中合法的路径段字符（如 @、.）保留原文、不转义；
	// 仅禁止符（如 /、空格）转义。故 alice@example.com 原样保留。
	label := got[len("otpauth://totp/"):sep]
	if label != "alice@example.com" {
		t.Fatalf("URI label = %q, want %q", label, "alice@example.com")
	}
	query := got[sep+1:]

	pairs := map[string]string{}
	for part := range strings.SplitSeq(query, "&") {
		eq := strings.Index(part, "=")
		if eq < 0 {
			t.Fatalf("URI query part not key=value: %q", part)
		}
		pairs[part[:eq]] = part[eq+1:]
	}

	want := map[string]string{
		"secret":    "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ",
		"issuer":    "Cocomhub",
		"algorithm": "SHA1",
		"digits":    "6",
		"period":    "30",
	}
	for k, v := range want {
		if pairs[k] != v {
			t.Fatalf("URI param %q = %q, want %q (full URI %q)", k, pairs[k], v, got)
		}
	}
	if len(pairs) != len(want) {
		t.Fatalf("URI has unexpected params: %v (full URI %q)", pairs, got)
	}
}

func TestCode_EmptySecret(t *testing.T) {
	tp := NewTOTP(nil)
	if _, err := tp.Code(time.Unix(1234567890, 0)); err == nil {
		t.Fatal("Code with empty secret should return error")
	}
}
