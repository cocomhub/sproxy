// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dnspod 提供腾讯云 DNSPod DNS-01 ACME 挑战插件。
//
// 使用方式：
//
//	p := dnspod.New(dnspod.Config{
//	    SecretId:  "your-secret-id",
//	    SecretKey: "your-secret-key",
//	})
//	p.SetDNSRecord(ctx, "example.com", "token", "keyauth")
package dnspod

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // DNSPod API 要求 HMAC-SHA1 签名，无安全替代
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Config 是 DNSPod 插件配置。
type Config struct {
	SecretId  string
	SecretKey string
	// Endpoint 可选，默认 "dnspod.tencentcloudapi.com"。
	// 测试时设为 mock 服务器的 host:port（使用 http:// 前缀）。
	Endpoint string
}

// Provider 实现 certmgr.DNSProvider 接口。
type Provider struct {
	config   Config
	client   *http.Client
	endpoint string
	scheme   string
}

// New 创建 DNSPod Provider。
func New(cfg Config) *Provider {
	endpoint := cfg.Endpoint
	scheme := "https"
	if endpoint == "" {
		endpoint = "dnspod.tencentcloudapi.com"
	}
	// 支持测试时传入完整 URL（如 http://127.0.0.1:port），提取 scheme 和 host
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		parts := strings.SplitN(endpoint, "://", 2)
		if len(parts) == 2 {
			scheme = parts[0]
			endpoint = parts[1]
		}
	}
	return &Provider{
		config:   cfg,
		client:   &http.Client{Timeout: 30 * time.Second},
		endpoint: endpoint,
		scheme:   scheme,
	}
}

// SetDNSRecord 设置 DNS TXT 记录用于 ACME 域名验证。
// ACME 挑战格式：_acme-challenge.<domain> TXT "<keyAuth>"
// domain 参数是完整域名（如 sub.example.com），自动提取根域名并设置 SubDomain。
func (p *Provider) SetDNSRecord(ctx context.Context, domain, token, keyAuth string) error {
	rootDomain, subDomain := splitDomain(domain)
	subDomain = subDomainPrefix(subDomain)

	params := map[string]string{
		"Action":     "CreateRecord",
		"Domain":     rootDomain,
		"SubDomain":  subDomain,
		"RecordType": "TXT",
		"RecordLine": "默认",
		"Value":      keyAuth,
		"TTL":        "60",
	}
	return p.callAPI(ctx, params)
}

// CleanupDNSRecord 清理 ACME 验证记录。
// domain 参数是完整域名，自动提取根域名并设置 SubDomain。
func (p *Provider) CleanupDNSRecord(ctx context.Context, domain, token, keyAuth string) error {
	rootDomain, subDomain := splitDomain(domain)
	subDomain = subDomainPrefix(subDomain)

	// 先查找 TXT 记录
	listParams := map[string]string{
		"Action":     "RecordList",
		"Domain":     rootDomain,
		"SubDomain":  subDomain,
		"RecordType": "TXT",
	}
	var result struct {
		Response struct {
			RecordList []struct {
				RecordId int    `json:"RecordId"`
				Value    string `json:"Value"`
			} `json:"RecordList"`
		} `json:"Response"`
	}
	if err := p.callAPIWithResult(ctx, listParams, &result); err != nil {
		return fmt.Errorf("查询记录失败: %w", err)
	}

	// 删除匹配 keyAuth 的记录
	for _, record := range result.Response.RecordList {
		if record.Value == keyAuth {
			delParams := map[string]string{
				"Action":   "DeleteRecord",
				"Domain":   rootDomain,
				"RecordId": fmt.Sprintf("%d", record.RecordId),
			}
			if err := p.callAPI(ctx, delParams); err != nil {
				return fmt.Errorf("删除记录失败: %w", err)
			}
		}
	}
	return nil
}

// callAPI 调用 DNSPod API。
func (p *Provider) callAPI(ctx context.Context, params map[string]string) error {
	return p.callAPIWithResult(ctx, params, nil)
}

// callAPIWithResult 调用 DNSPod API 并解析响应。
// 签名计算和 URL 构建均使用 URL-encoded 参数值，确保中文和特殊字符正确处理。
func (p *Provider) callAPIWithResult(ctx context.Context, params map[string]string, result interface{}) error {
	if p.config.SecretId == "" || p.config.SecretKey == "" {
		return fmt.Errorf("SecretId 和 SecretKey 不能为空")
	}

	// 添加公共参数（不修改入参 map）
	allParams := make(map[string]string, len(params)+4)
	for k, v := range params {
		allParams[k] = v
	}
	allParams["SecretId"] = p.config.SecretId
	allParams["Timestamp"] = fmt.Sprintf("%d", time.Now().Unix())
	// 生成随机 Nonce
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("生成 Nonce 失败: %w", err)
	}
	nonceInt := int64(binary.LittleEndian.Uint64(nonce) % 100000)
	allParams["Nonce"] = fmt.Sprintf("%d", nonceInt)
	allParams["SignatureMethod"] = "HmacSHA1"

	// 排序参数键
	keys := make([]string, 0, len(allParams))
	for k := range allParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 构建签名源字符串（URL-encoded 值）
	var srcParts []string
	for _, k := range keys {
		srcParts = append(srcParts, url.QueryEscape(k)+"="+url.QueryEscape(allParams[k]))
	}
	srcStr := strings.Join(srcParts, "&")

	// 计算签名（HmacSHA1）
	mac := hmac.New(sha1.New, []byte(p.config.SecretKey))
	signStr := "GET" + p.endpoint + "/?" + srcStr
	mac.Write([]byte(signStr))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// 使用 url.URL 构建完整请求 URL
	u := &url.URL{
		Scheme:   p.scheme,
		Host:     p.endpoint,
		Path:     "/",
		RawQuery: srcStr + "&Signature=" + url.QueryEscape(signature),
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("API 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	// 检查 HTTP 状态码
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DNSPod API 返回非 200 状态码: %d, body: %s", resp.StatusCode, string(body))
	}

	// 检查错误
	var errResp struct {
		Response struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Response.Error.Code != "" {
		return fmt.Errorf("DNSPod API 错误: %s - %s", errResp.Response.Error.Code, errResp.Response.Error.Message)
	}

	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("解析响应失败: %s: %w", string(body), err)
		}
	}

	return nil
}

// splitDomain 将完整域名拆分为根域名和子域名前缀。
// 例如 "sub.example.com" -> ("example.com", "sub")。
// 顶级域名如 "example.com" -> ("example.com", "").
// 注意：未处理多级 TLD（如 .co.uk），但 ACME 证书场景下通常涉及
// 公共域名，根域名提取符合 DNSPod API 要求。
func splitDomain(domain string) (root, sub string) {
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain, ""
	}
	// 取最后两级作为根域名
	root = strings.Join(parts[len(parts)-2:], ".")
	// 前面部分作为子域名前缀
	sub = strings.Join(parts[:len(parts)-2], ".")
	return
}

// subDomainPrefix 为子域名添加 _acme-challenge 前缀。
// 如果 sub 为空（根域名），返回 "_acme-challenge"。
// 如果 sub 非空（子域名），返回 "_acme-challenge.子域名"。
func subDomainPrefix(sub string) string {
	if sub == "" {
		return "_acme-challenge"
	}
	return "_acme-challenge." + sub
}
