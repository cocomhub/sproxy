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
// jwtHeader 是 id_token 的 JOSE 头（仅解析验签所需字段）。
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

func verifyJWTWithJWKS(ctx context.Context, client *http.Client, jwksURL, issuer, token, expectedAud string) (map[string]any, error) {
	header, claims, sig, signed, err := parseJWT(token)
	if err != nil {
		return nil, err
	}
	parsed := parsedJWT{header: header, claims: claims, sig: sig, signed: signed}
	if err := verifySignatureAndClaims(ctx, client, jwksURL, parsed, issuer, expectedAud); err != nil {
		return nil, err
	}
	return claims, nil
}

// parseJWT 解析并校验三段式 JWT 的结构/编码/alg，返回解出的头、claims、签名与待验签字符串。
func parseJWT(token string) (jwtHeader, map[string]any, []byte, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token 非三段 JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token header 解码失败: %w", err)
	}
	var header jwtHeader
	if uerr := json.Unmarshal(headerRaw, &header); uerr != nil {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token header 解析失败: %w", uerr)
	}
	if header.Alg != "RS256" {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token alg %q 不受支持（仅 RS256）", header.Alg)
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token payload 解码失败: %w", err)
	}
	var claims map[string]any
	if uerr := json.Unmarshal(payloadRaw, &claims); uerr != nil {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token claims 解析失败: %w", uerr)
	}
	// 签名校验：签名 = base64url(header.payload)。
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, nil, nil, "", fmt.Errorf("oidcldap: id_token 签名解码失败: %w", err)
	}
	return header, claims, sig, parts[0] + "." + parts[1], nil
}

// parsedJWT 是 verifySignatureAndClaims 的验签输入参数组（S107 收敛）：
// 由 parseJWT 解出的头、claims、签名与待验签字符串，聚合为结构体替代
// 散参透传。
type parsedJWT struct {
	header jwtHeader
	claims map[string]any
	sig    []byte
	signed string
}

// verifySignatureAndClaims 拉取公钥验签，并校验 iss/aud/exp。
func verifySignatureAndClaims(ctx context.Context, client *http.Client, jwksURL string, parsed parsedJWT, issuer, expectedAud string) error {
	key, err := fetchJWKSKey(ctx, client, jwksURL, parsed.header.Kid)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parsed.signed))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], parsed.sig); err != nil { // NOSONAR: S5542 — RS256 由 RFC 7518 规定 RSASSA-PKCS1-v1_5 签名，换 PSS 会破坏 id_token 验签兼容
		return fmt.Errorf("oidcldap: id_token 签名校验失败: %w", err)
	}
	// iss/aud/exp 校验。aud 匹配由调用方传入期望值（client_id）。
	if iss, _ := parsed.claims["iss"].(string); iss != issuer {
		return fmt.Errorf("oidcldap: id_token iss=%q 与配置 issuer %q 不符", iss, issuer)
	}
	aud := extractAud(parsed.claims)
	if aud == "" {
		return fmt.Errorf("oidcldap: id_token 缺 aud")
	}
	if expectedAud != "" && aud != expectedAud {
		return fmt.Errorf("oidcldap: id_token aud=%q 与期望 client_id %q 不符", aud, expectedAud)
	}
	exp, ok := parsed.claims["exp"].(float64)
	if !ok {
		return fmt.Errorf("oidcldap: id_token 缺 exp")
	}
	if time.Now().Unix() >= int64(exp) {
		return fmt.Errorf("oidcldap: id_token 已过期")
	}
	return nil
}

// extractAud 取 aud 声明的字符串值（标量优先，数组取首元素）。
func extractAud(claims map[string]any) string {
	aud, _ := claims["aud"].(string)
	if aud == "" {
		if auds, ok := claims["aud"].([]any); ok && len(auds) > 0 {
			aud, _ = auds[0].(string)
		}
	}
	return aud
}

// fetchJWKSKey 从 JWKS 拉取公钥（按 kid 匹配；无 kid 时取首个 RSA key）。
func fetchJWKSKey(ctx context.Context, client *http.Client, jwksURL, kid string) (*rsa.PublicKey, error) {
	body, err := fetchJWKSBody(ctx, client, jwksURL)
	if err != nil {
		return nil, err
	}
	return pickJWKSKey(body, kid)
}

// fetchJWKSBody 拉取并读取 JWKS 资源体（校验 HTTP 状态码，限量 1 MiB）。
func fetchJWKSBody(ctx context.Context, client *http.Client, jwksURL string) ([]byte, error) {
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
	return body, nil
}

// pickJWKSKey 在已拉取的 JWKS 里按 kid 匹配 RSA 公钥（无 kid 时取首个合法 RSA key）。
func pickJWKSKey(body []byte, kid string) (*rsa.PublicKey, error) {
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
		if key := buildRSAKey(k.Kty, k.Kid, k.N, k.E, kid); key != nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("oidcldap: jwks 无匹配 RSA key（kid=%q）", kid)
}

// buildRSAKey 按 kid/base64 参数构造单个 RSA 公钥；不匹配或字段非法返回 nil。
func buildRSAKey(kty, keyKid, nB64, eB64, kid string) *rsa.PublicKey {
	if kid != "" && keyKid != kid {
		return nil
	}
	if kty != "RSA" || nB64 == "" || eB64 == "" {
		return nil
	}
	nb, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil
	}
	eb, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
}

// newIsolatedTransport 返回独立连接池的 http.Transport（生产/测试均不共享
// DefaultTransport——测试隔离硬规则 + 生产防环）。
func newIsolatedTransport() *http.Transport {
	return netutil.IsolatedTransport()
}

// _ 保持 time 引用（将来扩展）。
var _ = time.Now
