// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vaultmock

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// vaultDo 向 mock 端点发 POST 请求并返回响应 body。
func vaultDo(t *testing.T, url, token, body string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return data
}

// TestVaultMock_EncryptDecrypt_MirrorRoundtrip 验证 mock 默认镜像行为：
// encrypt 回 "vault:v1:"+base64(明文)，decrypt 解回明文；token 断言；请求记录与计数。
func TestVaultMock_EncryptDecrypt_MirrorRoundtrip(t *testing.T) {
	const tok = "test-token"
	s := NewServer(t, Options{Token: tok})

	plain := []byte(`{"version":1,"keys":[{"ak":"ak-vault-mock"}]}`)
	ctx := base64.StdEncoding.EncodeToString([]byte("anonymous/meta/credentials.json"))

	encBody := `{"plaintext":"` + base64.StdEncoding.EncodeToString(plain) + `","context":"` + ctx + `"}`
	resp := vaultDo(t, s.URL()+"/v1/transit/encrypt/sproxy", tok, encBody)
	var enc struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &enc); err != nil {
		t.Fatalf("解析 encrypt 响应: %v\nbody=%s", err, resp)
	}
	wantCT := "vault:v1:" + base64.StdEncoding.EncodeToString(plain)
	if enc.Data.Ciphertext != wantCT {
		t.Fatalf("ciphertext 应镜像为 %q, got %q", wantCT, enc.Data.Ciphertext)
	}

	decBody := `{"ciphertext":"` + enc.Data.Ciphertext + `","context":"` + ctx + `"}`
	resp2 := vaultDo(t, s.URL()+"/v1/transit/decrypt/sproxy", tok, decBody)
	var dec struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp2, &dec); err != nil {
		t.Fatalf("解析 decrypt 响应: %v\nbody=%s", err, resp2)
	}
	got, err := base64.StdEncoding.DecodeString(dec.Data.Plaintext)
	if err != nil {
		t.Fatalf("plaintext 非法 base64: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypt 应还原明文, got %q", got)
	}

	if n := s.EncryptCount(); n != 1 {
		t.Fatalf("EncryptCount 应为 1, got %d", n)
	}
	if n := s.DecryptCount(); n != 1 {
		t.Fatalf("DecryptCount 应为 1, got %d", n)
	}
	if got := s.LastToken(); got != tok {
		t.Fatalf("LastToken 应为 %q, got %q", tok, got)
	}
	if got := s.LastContext(); got != ctx {
		t.Fatalf("LastContext 应为 %q, got %q", ctx, got)
	}
	if got := s.LastPlaintextB64(); got != base64.StdEncoding.EncodeToString(plain) {
		t.Fatalf("LastPlaintextB64 不匹配, got %q", got)
	}
}

// TestVaultMock_DecryptToFixed 验证 Options.DecryptTo：decrypt 固定回指定明文。
func TestVaultMock_DecryptToFixed(t *testing.T) {
	const tok = "tok"
	s := NewServer(t, Options{Token: tok, DecryptTo: []byte("fixed-plaintext")})

	body := vaultDo(t, s.URL()+"/v1/transit/decrypt/sproxy", tok, `{"ciphertext":"vault:v1:whatever"}`)
	var dec struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &dec); err != nil {
		t.Fatalf("解析 decrypt 响应: %v\nbody=%s", err, body)
	}
	want := base64.StdEncoding.EncodeToString([]byte("fixed-plaintext"))
	if dec.Data.Plaintext != want {
		t.Fatalf("DecryptTo 明文应固定为 %q, got %q", want, dec.Data.Plaintext)
	}
}

// TestVaultMock_DecryptError 验证 SetDecryptError：decrypt 回指定状态码 + errors body。
func TestVaultMock_DecryptError(t *testing.T) {
	s := NewServer(t, Options{Token: "tok"})
	s.SetDecryptError(http.StatusServiceUnavailable, "Vault is sealed")

	req, err := http.NewRequest(http.MethodPost, s.URL()+"/v1/transit/decrypt/sproxy", strings.NewReader(`{"ciphertext":"vault:v1:abc"}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST decrypt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503, got %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "Vault is sealed") {
		t.Fatalf("错误 body 应含 Vault is sealed, got %s", data)
	}
}
