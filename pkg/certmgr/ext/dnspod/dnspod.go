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
	"crypto/sha1"
	"encoding/base64"
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
func (p *Provider) SetDNSRecord(ctx context.Context, domain, token, keyAuth string) error {
	rootDomain := domain
	params := map[string]string{
		"Action":     "CreateRecord",
		"Domain":     rootDomain,
		"SubDomain":  "_acme-challenge",
		"RecordType": "TXT",
		"RecordLine": "默认",
		"Value":      keyAuth,
		"TTL":        "60",
	}
	return p.callAPI(ctx, params)
}

// CleanupDNSRecord 清理 ACME 验证记录。
func (p *Provider) CleanupDNSRecord(ctx context.Context, domain, token, keyAuth string) error {
	rootDomain := domain

	// 先查找 TXT 记录
	listParams := map[string]string{
		"Action":     "RecordList",
		"Domain":     rootDomain,
		"SubDomain":  "_acme-challenge",
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
func (p *Provider) callAPIWithResult(ctx context.Context, params map[string]string, result interface{}) error {
	if p.config.SecretId == "" || p.config.SecretKey == "" {
		return fmt.Errorf("SecretId 和 SecretKey 不能为空")
	}

	// 添加公共参数
	params["SecretId"] = p.config.SecretId
	params["Timestamp"] = fmt.Sprintf("%d", time.Now().Unix())
	params["Nonce"] = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	params["SignatureMethod"] = "HmacSHA1"

	// 排序参数键
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 构建签名源字符串
	var srcParts []string
	for _, k := range keys {
		srcParts = append(srcParts, k+"="+params[k])
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
