// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// webhook_sign_test.go 验证 webhook 渠道出站 HMAC 签名（roadmap 11.10-⑤）：
//  1. VerifyWebhookSignature 纯函数 table-driven：正确/篡改 body/过期时间戳/
//     未来时间戳/坏版本/hex 畸形/格式畸形/空 header/空 body/密钥不符。
//  2. WebhookNotifier.Send 带 secret → 签名头 + 时间戳头存在且验签通过；
//     secret 空 → 不加任何签名头（golden 零回归）。
//  3. 自定义头名（sign_header / timestamp_header）生效。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// signWebhookHeader 计算合法签名头（测试 helper；与生产同公式：
// sig = hex(HMAC-SHA256(secret, "<ts>.<body>"))）。
func signWebhookHeader(t *testing.T, secret string, body []byte, ts int64) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s", ts, body)
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s=%d.%s", signatureVersion, ts, sig)
}

// TestVerifyWebhookSignature VerifyWebhookSignature 纯函数 table-driven。
func TestVerifyWebhookSignature(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	body := []byte(`{"title":"t","text":"h","object":"a.txt","action":"upload"}`)
	now := time.Now()
	ts := now.Unix()
	valid := signWebhookHeader(t, secret, body, ts)

	tests := []struct {
		name   string
		secret string
		header string
		body   []byte
		now    time.Time
		skew   time.Duration
		wantOK bool
	}{
		{name: "正确签名", secret: secret, header: valid, body: body, now: now, skew: maxClockSkew, wantOK: true},
		{name: "篡改 body", secret: secret, header: valid, body: []byte(`{"title":"evil"}`), now: now, skew: maxClockSkew},
		{name: "过期时间戳", secret: secret, header: signWebhookHeader(t, secret, body, now.Add(-2*maxClockSkew).Unix()), body: body, now: now, skew: maxClockSkew},
		{name: "未来时间戳", secret: secret, header: signWebhookHeader(t, secret, body, now.Add(2*maxClockSkew).Unix()), body: body, now: now, skew: maxClockSkew},
		{name: "坏版本", secret: secret, header: strings.Replace(valid, signatureVersion+"=", "md5=", 1), body: body, now: now, skew: maxClockSkew},
		{name: "hex 畸形", secret: secret, header: "sha256=" + strconv.FormatInt(ts, 10) + ".zzzz", body: body, now: now, skew: maxClockSkew},
		{name: "格式畸形无点", secret: secret, header: "sha256=abc", body: body, now: now, skew: maxClockSkew},
		{name: "空 header", secret: secret, header: "", body: body, now: now, skew: maxClockSkew},
		{name: "空 body", secret: secret, header: valid, body: nil, now: now, skew: maxClockSkew},
		{name: "密钥不符", secret: "other-secret", header: valid, body: body, now: now, skew: maxClockSkew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifyWebhookSignature(tc.secret, tc.header, tc.body, tc.now, tc.skew)
			if tc.wantOK && err != nil {
				t.Fatalf("VerifyWebhookSignature 应通过: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("VerifyWebhookSignature 应失败，got nil")
			}
		})
	}
}

// TestWebhookNotifier_Send_SignedSecret 带 secret：出站签名头 + 时间戳头存在且验签通过。
func TestWebhookNotifier_Send_SignedSecret(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	var got struct {
		mu    sync.Mutex
		sigHd string
		tsHd  string
		body  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.sigHd = r.Header.Get(signatureHeaderName)
		got.tsHd = r.Header.Get(timestampHeaderName)
		got.body = b
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookNotifier(WebhookConfig{URL: srv.URL, Secret: secret})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "通知", Text: "hello", Object: "a.txt", Action: "upload"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.sigHd == "" {
		t.Fatal("secret 配置时应带签名头")
	}
	if got.tsHd == "" {
		t.Fatal("secret 配置时应带时间戳头")
	}
	if err := VerifyWebhookSignature(secret, got.sigHd, got.body, time.Now(), maxClockSkew); err != nil {
		t.Fatalf("出站头验签失败: %v", err)
	}
}

// TestWebhookNotifier_Send_NoSecretNoHeaders secret 空 → 不加任何签名头（零回归 golden）。
func TestWebhookNotifier_Send_NoSecretNoHeaders(t *testing.T) {
	t.Parallel()
	var got struct {
		mu    sync.Mutex
		sigHd string
		tsHd  string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.sigHd = r.Header.Get(signatureHeaderName)
		got.tsHd = r.Header.Get(timestampHeaderName)
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookNotifier(WebhookConfig{URL: srv.URL}) // 无 secret
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "t", Text: "h"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.sigHd != "" {
		t.Fatalf("secret 空不应带签名头，got %q", got.sigHd)
	}
	if got.tsHd != "" {
		t.Fatalf("secret 空不应带时间戳头，got %q", got.tsHd)
	}
}

// TestWebhookNotifier_Send_CustomHeaders 自定义头名（sign_header/timestamp_header）生效。
func TestWebhookNotifier_Send_CustomHeaders(t *testing.T) {
	t.Parallel()
	const (
		secret = "test-secret"
		sigHdr = "X-Custom-Sig"
		tsHdr  = "X-Custom-Ts"
	)
	var got struct {
		mu    sync.Mutex
		sigHd string
		tsHd  string
		body  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.sigHd = r.Header.Get(sigHdr)
		got.tsHd = r.Header.Get(tsHdr)
		got.body = b
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookNotifier(WebhookConfig{
		URL:             srv.URL,
		Secret:          secret,
		SignHeader:      sigHdr,
		TimestampHeader: tsHdr,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "t", Text: "h"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.sigHd == "" {
		t.Fatal("自定义签名头未生效")
	}
	if got.tsHd == "" {
		t.Fatal("自定义时间戳头未生效")
	}
	if err := VerifyWebhookSignature(secret, got.sigHd, got.body, time.Now(), maxClockSkew); err != nil {
		t.Fatalf("验签失败: %v", err)
	}
}
