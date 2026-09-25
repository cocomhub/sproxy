// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// verifyJWTWithJWKS 校验 JWT（RS256）：从 JWKS 拉公钥 → 验签名 → 校验 iss/aud/exp。
//
// 纯标准库实现（crypto/rsa + x509），不引入第三方 JWT 库——OIDC id_token 的
// 验签是安全关键路径，用标准库原语自行装配可审计、零隐式依赖。
func verifyJWTWithJWKS(ctx context.Context, client *http.Client, jwksURL, issuer, token, expectedAud string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oidcldap: id_token 非三段 JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidcldap: id_token header 解码失败: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if uerr := json.Unmarshal(headerRaw, &header); uerr != nil {
		return nil, fmt.Errorf("oidcldap: id_token header 解析失败: %w", uerr)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("oidcldap: id_token alg %q 不受支持（仅 RS256）", header.Alg)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidcldap: id_token payload 解码失败: %w", err)
	}
	var claims map[string]any
	if uerr := json.Unmarshal(payloadRaw, &claims); uerr != nil {
		return nil, fmt.Errorf("oidcldap: id_token claims 解析失败: %w", uerr)
	}
	// 签名校验：签名 = base64url(header.payload)。
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidcldap: id_token 签名解码失败: %w", err)
	}
	key, err := fetchJWKSKey(ctx, client, jwksURL, header.Kid)
	if err != nil {
		return nil, err
	}
	signed := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("oidcldap: id_token 签名校验失败: %w", err)
	}
	// iss/aud/exp 校验。aud 匹配由调用方传入期望值（clientID）。
	if iss, _ := claims["iss"].(string); iss != issuer {
		return nil, fmt.Errorf("oidcldap: id_token iss=%q 与配置 issuer %q 不符", iss, issuer)
	}
	aud, _ := claims["aud"].(string)
	if aud == "" {
		if auds, ok := claims["aud"].([]any); ok && len(auds) > 0 {
			aud, _ = auds[0].(string)
		}
	}
	if aud == "" {
		return nil, fmt.Errorf("oidcldap: id_token 缺 aud")
	}
	if expectedAud != "" && aud != expectedAud {
		return nil, fmt.Errorf("oidcldap: id_token aud=%q 与期望 client_id %q 不符", aud, expectedAud)
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return nil, fmt.Errorf("oidcldap: id_token 缺 exp")
	}
	if time.Now().Unix() >= int64(exp) {
		return nil, fmt.Errorf("oidcldap: id_token 已过期")
	}
	return claims, nil
}

// fetchJWKSKey 从 JWKS 拉取公钥（按 kid 匹配；无 kid 时取首个 RSA key）。
func fetchJWKSKey(ctx context.Context, client *http.Client, jwksURL, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oidcldap: jwks 请求构造失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidcldap: jwks 拉取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("oidcldap: jwks HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oidcldap: jwks 读取失败: %w", err)
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("oidcldap: jwks 解析失败: %w", err)
	}
	for _, k := range jwks.Keys {
		if kid != "" && k.Kid != kid {
			continue
		}
		if k.Kty != "RSA" || k.N == "" || k.E == "" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}, nil
	}
	return nil, fmt.Errorf("oidcldap: jwks 无匹配 RSA key（kid=%q）", kid)
}

// newIsolatedTransport 返回独立连接池的 http.Transport（生产/测试均不共享
// DefaultTransport——测试隔离硬规则 + 生产防环）。
func newIsolatedTransport() *http.Transport {
	return netutil.IsolatedTransport()
}

// _ 保持 time 引用（将来扩展）。
var _ = time.Now
