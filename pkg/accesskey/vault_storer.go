// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// VaultTransitStorer 是 SecureStorer 的 HashiCorp Vault Transit 实现：凭据明文经 Vault
// Transit 引擎加解密，密钥永不出 Vault（sproxy 仅持 token 调 API）。纯 stdlib net/http，
// 无三方依赖（符合 ext-policy）。
//
// AAD context 绑 AADPath（文件身份）：Transit encrypt/decrypt 的 context 参数（base64，
// 可选的额外认证数据）绑定 file path（如 credentials.json 相对 storage_root 路径）——密文
// 被复制/搬移到另一文件即 decrypt 失败（context 不匹配）。
type VaultTransitStorer struct {
	addr    string // Vault 服务基址（http/https，构造时已校验；尾随 / 已去）
	mount   string // Transit engine 挂载路径（缺省 "transit"）
	keyName string // Transit 加密 key 名
	token   string // X-Vault-Token 请求头
	aadPath string // AAD context 绑定的文件身份（相对路径；空 = 无 AAD 绑定）
	client  *http.Client
}

// VaultOptions 是 VaultTransitStorer 的构造参数。
type VaultOptions struct {
	Addr     string        // Vault 地址（http/https，必须）
	Mount    string        // transit engine 挂载路径（空 → "transit"）
	KeyName  string        // transit 加密 key 名（必须）
	Token    string        // X-Vault-Token（必须，空 → 构造错误）
	CAFile   string        // 自签 CA 证书路径（可选；空 → 系统证书池）
	Timeout  time.Duration // HTTP 超时（≤0 → 10s）
	AADPath  string        // AAD context 绑定（建议调用方传凭据文件相对路径）
	CacheTTL time.Duration // decrypt 结果缓存 TTL（0 = 关闭缓存；预留，缓存逻辑由后续任务启用）
}

// 编译期断言：*VaultTransitStorer 满足 SecureStorer（防签名漂移，仿 AESGCMStorer 断言模式）。
var _ SecureStorer = (*VaultTransitStorer)(nil)

const (
	// vaultCiphertextPrefix 是 Vault Transit 密文的自描述前缀。Decrypt 输入非此前缀
	// 直接拒绝（fail-closed，防明文误喂 Vault）。
	vaultCiphertextPrefix = "vault:v1:"
	// vaultDefaultMount 是 Transit engine 的缺省挂载路径。
	vaultDefaultMount = "transit"
	// vaultDefaultTimeout 是缺省 HTTP 超时。
	vaultDefaultTimeout = 10 * time.Second
	// vaultMaxResponseBytes 是 Vault 响应体读取上限（1 MiB）。Transit 成功/错误响应都很小；
	// 超过上限疑似非 Vault Transit 端点或中间层异常，fail-closed 拒绝解析（防无界读内存）。
	vaultMaxResponseBytes = 1 << 20
)

// vaultEncryptRequest 是 POST {mount}/encrypt/{key} 的请求体。
type vaultEncryptRequest struct {
	Plaintext string `json:"plaintext"`
	Context   string `json:"context,omitempty"` // aadPath 为空时省略（Vault 允许缺省）
}

// vaultDecryptRequest 是 POST {mount}/decrypt/{key} 的请求体。
type vaultDecryptRequest struct {
	Ciphertext string `json:"ciphertext"`
	Context    string `json:"context,omitempty"`
}

// vaultDataEnvelope 是 Vault Transit 成功响应的外壳：{"data":{...}}。字段用指针判
// **存在性**而非空串——空明文的 base64 恒为 ""（base64("")==""），用空串当缺字段哨兵会
// 破坏 SecureStorer 空内容往返（M-3）。
type vaultDataEnvelope struct {
	Data struct {
		Ciphertext *string `json:"ciphertext"`
		Plaintext  *string `json:"plaintext"`
	} `json:"data"`
}

// NewVaultTransitStorer 构造 VaultTransitStorer。
//
// fail-fast 校验：addr 非空且 scheme 为 http/https + host 非空（url.Parse）、key_name 非空、
// token 非空（空 → error「vault: token 为空」）。mount 空 → "transit"；timeout ≤0 → 10s；
// CAFile 非空时读取 PEM 并构造自签 CA 的 TLS 根池（失败返回明确 error）；否则使用默认
// `http.Client{Timeout}`。两分支 client 均禁止跟随重定向（防 X-Vault-Token 外泄）。decrypt
// 缓存 map 初始化留待缓存任务启用（CacheTTL>0 时）。
func NewVaultTransitStorer(opts VaultOptions) (*VaultTransitStorer, error) {
	if opts.Addr == "" {
		return nil, errors.New("vault: addr 为空（需指向 Vault 服务地址）")
	}
	u, err := url.Parse(opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("vault: 解析 addr %q 失败: %w", opts.Addr, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("vault: addr scheme 必须为 http/https，got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("vault: addr 缺少 host（需 http(s)://host[:port] 形式，got %q）", opts.Addr)
	}
	if opts.KeyName == "" {
		return nil, errors.New("vault: key_name 为空（需指定 Transit 加密 key）")
	}
	if opts.Token == "" {
		return nil, errors.New("vault: token 为空")
	}
	mount := opts.Mount
	if mount == "" {
		mount = vaultDefaultMount
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = vaultDefaultTimeout
	}
	client := newVaultHTTPClient(timeout, nil)
	if opts.CAFile != "" {
		pool, err := loadVaultCertPool(opts.CAFile)
		if err != nil {
			return nil, err
		}
		// 以 http.DefaultTransport 为基座克隆后仅覆写 TLSClientConfig：保留
		// ProxyFromEnvironment / 连接池 / HTTP2 / 握手超时等默认（M-6：不自建零值 Transport）。
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("vault: 无法取得默认 HTTP Transport 基座（非 *http.Transport）")
		}
		transport := defaultTransport.Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		client = newVaultHTTPClient(timeout, transport)
	}
	return &VaultTransitStorer{
		addr:    strings.TrimRight(opts.Addr, "/"),
		mount:   mount,
		keyName: opts.KeyName,
		token:   opts.Token,
		aadPath: opts.AADPath,
		client:  client,
	}, nil
}

// newVaultHTTPClient 构造 Vault API 客户端：超时 + 禁止跟随重定向。默认 http.Client 最多
// 跟 10 跳，跨主机跳转只剥离 Authorization/Cookie 等头，X-Vault-Token 是自定义头会被原样
// 带到重定向目标（token 外泄）——Vault API 客户端应直接收尾跳转响应（I-1）。
func newVaultHTTPClient(timeout time.Duration, transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// loadVaultCertPool 读取 PEM CA 文件并构造 CertPool（Vault 自签/内网 CA 场景）。
// 文件缺失/内容非有效 PEM 返回明确 error（fail-fast，不静默回落系统池）。
func loadVaultCertPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("vault: 读取 CA 文件失败: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("vault: CA 文件 %s 不含有效 PEM 证书", caFile)
	}
	return pool, nil
}

// Encrypt 把明文经 Vault Transit 加密：POST {addr}/v1/{mount}/encrypt/{keyName}，请求体
// {"plaintext": base64(明文), "context": base64(AADPath)}，携带 X-Vault-Token 头。返回
// Vault 响应的 data.ciphertext 原样（含 vault:v1: 版本前缀，保存即原样字节）。
func (s *VaultTransitStorer) Encrypt(plaintext []byte) ([]byte, error) {
	reqBody, err := json.Marshal(vaultEncryptRequest{
		Plaintext: base64.StdEncoding.EncodeToString(plaintext),
		Context:   s.aadContext(),
	})
	if err != nil {
		return nil, fmt.Errorf("vault: encrypt 请求序列化失败: %w", err)
	}
	respBody, err := s.post("encrypt", reqBody)
	if err != nil {
		return nil, err
	}
	var out vaultDataEnvelope
	if err = json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("vault: encrypt 响应解析失败: %w", err)
	}
	if out.Data.Ciphertext == nil {
		return nil, errors.New("vault: encrypt 响应缺 data.ciphertext 字段")
	}
	return []byte(*out.Data.Ciphertext), nil
}

// Decrypt 把 Vault Transit 密文解回明文。输入非 vault:v1: 前缀直接报错（不请求 Vault，
// 防明文误喂）。POST {addr}/v1/{mount}/decrypt/{keyName}，请求体 {"ciphertext": <密文>,
// "context": base64(AADPath)}；响应 data.plaintext 经 base64 解码后返回。缓存逻辑由后续
// 任务启用（当前每次直查 Vault）。
func (s *VaultTransitStorer) Decrypt(ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte(vaultCiphertextPrefix)) {
		return nil, fmt.Errorf("vault: decrypt 拒绝非 %s 前缀输入（%d 字节，疑似明文误喂）", vaultCiphertextPrefix, len(ciphertext))
	}
	reqBody, err := json.Marshal(vaultDecryptRequest{
		Ciphertext: string(ciphertext),
		Context:    s.aadContext(),
	})
	if err != nil {
		return nil, fmt.Errorf("vault: decrypt 请求序列化失败: %w", err)
	}
	respBody, err := s.post("decrypt", reqBody)
	if err != nil {
		return nil, err
	}
	var out vaultDataEnvelope
	if err = json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("vault: decrypt 响应解析失败: %w", err)
	}
	if out.Data.Plaintext == nil {
		return nil, errors.New("vault: decrypt 响应缺 data.plaintext 字段")
	}
	pt, err := base64.StdEncoding.DecodeString(*out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault: decrypt 响应 plaintext 非法 base64: %w", err)
	}
	return pt, nil
}

// aadContext 返回 AAD context 字段值 = base64(AADPath)（可读 AAD、调试友好）。aadPath
// 为空时返回空串（请求体省略 context 字段，Vault 允许缺省——无 AAD 绑定）。
func (s *VaultTransitStorer) aadContext() string {
	if s.aadPath == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s.aadPath))
}

// post 向 Vault Transit 发送 op（encrypt/decrypt）POST 请求并返回 200 响应 body。
// URL = {addr}/v1/{mount}/{op}/{keyName}（不经 url 拼接转义——addr 已在构造校验）；
// 携带 X-Vault-Token 头与 Content-Type: application/json。网络错误包装为含 "vault" 前缀
// 的 error（Vault 不可达分类）；非 200 → vaultAPIError 分类解析。响应体经
// io.LimitReader 限读（1 MiB），超限返回明确 error（M-4：防无界读内存）。
func (s *VaultTransitStorer) post(op string, reqBody []byte) ([]byte, error) {
	endpoint := s.addr + "/v1/" + s.mount + "/" + op + "/" + s.keyName
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("vault: 构造 %s 请求失败: %w", op, err)
	}
	req.Header.Set("X-Vault-Token", s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault: %s 请求失败（Vault 不可达）: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 多读 1 字节探测是否超限：不把截断 JSON 喂给解析器（截断会得误导性的 syntax error）。
	body, err := io.ReadAll(io.LimitReader(resp.Body, vaultMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("vault: 读取 %s 响应失败: %w", op, err)
	}
	if len(body) > vaultMaxResponseBytes {
		return nil, fmt.Errorf("vault: %s 响应超过 %d 字节上限（疑似非 Vault Transit 端点）", op, vaultMaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, vaultAPIError(op, resp, body)
	}
	return body, nil
}

// vaultAPIError 把 Vault 非 200 响应包装为带上下文的 error（fail-closed）：
// 解析 {"errors":[...]} 取首条；503 + sealed 时特别标注「Vault sealed，需 unseal」
// （非网络误判）；错误消息始终含操作名与 HTTP 状态码（4xx token 失效/无权限/key 缺失、
// 5xx 均可据此排障）。
func vaultAPIError(op string, resp *http.Response, body []byte) error {
	status := resp.StatusCode
	msg := parseVaultErrors(body)
	if status == http.StatusServiceUnavailable && strings.Contains(strings.ToLower(msg), "sealed") {
		msg = fmt.Sprintf("%s（Vault sealed，需 unseal）", msg)
	}
	return fmt.Errorf("vault: %s 失败: %s (HTTP %d)", op, msg, status)
}

// parseVaultErrors 从 Vault 错误响应体提取用户可读错误消息：优先取 {"errors":[...]} 首条；
// 无 errors 数组时回退原始 body（trim）；仍空则返回通用占位（HTTP 状态码由调用方拼接）。
func parseVaultErrors(body []byte) string {
	var e struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err == nil && len(e.Errors) > 0 {
		return e.Errors[0]
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return trimmed
	}
	return "Vault 返回错误（无错误消息）"
}
