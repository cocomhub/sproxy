// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxysig

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testAK = "sk-prod-meshA-3f8a"
	testSK = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var baseTime = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// buildHeader 用 SK 计算合法签名的请求头（时间以 baseTime 为基准偏移）。
func buildHeader(ak, sk, nonce string, tsOffset, ttl time.Duration, method, path, query, bodyHash string) Header {
	ts := baseTime.Add(tsOffset).UnixMilli()
	exp := baseTime.Add(tsOffset + ttl).UnixMilli()
	h := Header{Version: Version, AK: ak, TS: ts, Exp: exp, Nonce: nonce, BodySHA256: bodyHash}
	h.Sig = Sign(sk, h, method, path, query)
	return h
}

func TestSignVerify_HappyPath(t *testing.T) {
	h := buildHeader(testAK, testSK, "nonce-1", 0, DefaultExpiry, "POST", "/upload", "", EmptyBodyHash())
	// now 在有效期内（ts+2min）。
	now := baseTime.Add(2 * time.Minute)
	seen := false
	err := Verify(testSK, h, "POST", "/upload", "", now, DefaultMaxTTL, DefaultClockSkew, func(_, _ string, _ int64) bool { return seen })
	if err != nil {
		t.Fatalf("Verify 应通过, got %v", err)
	}
}

func TestVerify_Rejections(t *testing.T) {
	now := baseTime.Add(2 * time.Minute)
	cases := []struct {
		name string
		h    Header
		sk   string
		want error
	}{
		{"未来时间", func() Header {
			h := buildHeader(testAK, testSK, "n", 10*time.Minute, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			return h
		}(),
			testSK, ErrFuture},
		{"TTL 超上限", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, 30*time.Minute, "GET", "/a", "", EmptyBodyHash())
			return h
		}(),
			testSK, ErrTTLTooLong},
		{"签名错误", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			h.Sig = strings.Repeat("0", 64)
			return h
		}(),
			testSK, ErrBadSignature},
		{"错误 SK", buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash()),
			strings.Repeat("f", 64), ErrBadSignature},
		{"版本不支持", func() Header {
			h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
			h.Version = "2"
			return h
		}(),
			testSK, ErrVersion},
		{"nonce 重放", buildHeader(testAK, testSK, "dup", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash()),
			testSK, ErrReplay},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify(c.sk, c.h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, func(_, n string, _ int64) bool { return n == "dup" })
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v, got %v", c.want, err)
			}
		})
	}
}

func TestVerify_Expired(t *testing.T) {
	// exp = ts + DefaultExpiry = +5min；now = +6min → 已过期。
	h := buildHeader(testAK, testSK, "n", 0, DefaultExpiry, "GET", "/a", "", EmptyBodyHash())
	now := baseTime.Add(6 * time.Minute)
	if err := Verify(testSK, h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, nil); !errors.Is(err, ErrExpired) {
		t.Fatalf("期望 ErrExpired, got %v", err)
	}
}

func TestVerify_ClientShorterTTL_Allowed(t *testing.T) {
	// 客户端声明 30s（比默认短），受 max_ttl 约束内 → 通过。
	h := buildHeader(testAK, testSK, "n", 0, 30*time.Second, "GET", "/a", "", EmptyBodyHash())
	now := baseTime.Add(10 * time.Second)
	if err := Verify(testSK, h, "GET", "/a", "", now, DefaultMaxTTL, DefaultClockSkew, nil); err != nil {
		t.Fatalf("短 TTL 应通过, got %v", err)
	}
}

func TestParseHeader(t *testing.T) {
	h := buildHeader(testAK, testSK, "nonce-9", 0, DefaultExpiry, "PUT", "/api/x", "a=1", BodyHash([]byte("hi")))
	auth := Scheme + " v=" + h.Version + " ak=" + h.AK + " ts=" + itoa(h.TS) + " exp=" + itoa(h.Exp) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
	parsed, err := ParseHeader(auth)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if parsed != h {
		t.Fatalf("解析结果不一致: %+v vs %+v", parsed, h)
	}
}

func TestParseHeader_Malformed(t *testing.T) {
	bad := []string{
		"",                      // 空
		"Bearer abc",            // 非 SproxySig
		"SproxySig",             // 缺空格后字段
		"SproxySig v=1 ak=only", // 缺字段
		"SproxySig v=1 ak=x ts=abc exp=1 nonce=n body_sha256=b sig=s",         // ts 非数字
		"SproxySig v=1 ak=x ts=1 exp=1 nonce=n body_sha256=b sig=s unknown=k", // 未知字段
		"SproxySig v=1 ts=1 exp=1 nonce=n body_sha256=b sig=s",                // 缺 ak
	}
	for _, s := range bad {
		if _, err := ParseHeader(s); !errors.Is(err, ErrMalformed) {
			t.Errorf("ParseHeader(%q) 期望 ErrMalformed, got %v", s, err)
		}
	}
}

func TestBodyValidator(t *testing.T) {
	// 匹配：EOF 返回 io.EOF，无错误。
	declared := BodyHash([]byte("hello"))
	vr := NewBodyValidator(strings.NewReader("hello"), declared)
	if _, err := io.ReadAll(vr); err != nil {
		t.Fatalf("匹配 body 应通过, got %v", err)
	}
	// 不匹配：EOF 时返回错误。
	vr = NewBodyValidator(strings.NewReader("hello"), BodyHash([]byte("world")))
	if _, err := io.ReadAll(vr); err == nil {
		t.Fatal("不匹配 body 应报错")
	}
	// UNSIGNED：跳过比对。
	vr = NewBodyValidator(strings.NewReader("anything"), UnsignedBody)
	if _, err := io.ReadAll(vr); err != nil {
		t.Fatalf("UNSIGNED 应跳过比对, got %v", err)
	}
}

func TestNoncePool(t *testing.T) {
	p := NewNoncePool()
	exp := time.Now().Add(5 * time.Minute).UnixMilli()
	if p.Seen("ak1", "n1", exp) {
		t.Fatal("首次 nonce 不应判重放")
	}
	if !p.Seen("ak1", "n1", exp) {
		t.Fatal("重复 nonce 应判重放")
	}
	if p.Seen("ak1", "n2", exp) {
		t.Fatal("不同 nonce 不应判重放")
	}
	if p.Seen("ak2", "n1", exp) {
		t.Fatal("不同 ak 的 nonce 不应判重放")
	}
	// 空值 fail-closed。
	if !p.Seen("", "n", exp) || !p.Seen("ak", "", exp) {
		t.Fatal("空 ak/nonce 应 fail-closed 判重放")
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestEmptyBodyHash(t *testing.T) {
	if got := EmptyBodyHash(); got != BodyHash(nil) {
		t.Fatalf("EmptyBodyHash=%q 应等于 sha256(空)=%q", got, BodyHash(nil))
	}
}

func TestBodyValidator_Unsigned(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), UnsignedBody)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "x" {
		t.Fatalf("UNSIGNED 应透传不校验: %v %q", err, b)
	}
}

func TestBodyValidator_Mismatch(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), BodyHash([]byte("y")))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("哈希不匹配应收错")
	}
}
