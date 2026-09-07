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
	"sync"
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

	// decrypt 结果短 TTL 缓存（AD-5）：key = ciphertext 字符串，value = {明文, 过期时间}。
	// cache 为 nil 时缓存关闭（CacheTTL=0），Decrypt 每次直查 Vault。map 非并发安全，
	// 读写一律持 mu。
	mu       sync.Mutex
	cache    map[string]vaultCacheEntry // nil = 关闭
	cacheTTL time.Duration              // 缓存 TTL（>0 时 cache 非 nil）
}

// vaultCacheEntry 是 decrypt 缓存的一条结果。
type vaultCacheEntry struct {
	plaintext []byte    // 解密明文（缓存内部 buffer；命中返回副本）
	expires   time.Time // 过期时间（time.Now() ≥ expires 视为失效）
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
	CacheTTL time.Duration // decrypt 结果缓存 TTL（≤0 均视为关闭）
}

// 编译期断言：*VaultTransitStorer 满足 SecureStorer（防签名漂移，仿 AESGCMStorer 断言模式）。
var _ SecureStorer = (*VaultTransitStorer)(nil)

const (
	// vaultCiphertextMagic 是 Vault Transit 密文的自描述前缀魔数（**版本无关**）：密文实际
	// 形如 vault:v<N>:...（N = key 版本，rotate 后递增，如 vault:v2:）。校验须匹配版本段
	// 而非硬编码 v1——硬编码 v1 会在 key 轮换后误拒合法密文。
	vaultCiphertextMagic = "vault:v"
	// vaultDefaultMount 是 Transit engine 的缺省挂载路径。
	vaultDefaultMount = "transit"
	// vaultDefaultTimeout 是缺省 HTTP 超时。
	vaultDefaultTimeout = 10 * time.Second
	// vaultMaxResponseBytes 是 Vault 响应体读取上限（1 MiB）。Transit 成功/错误响应都很小；
	// 超过上限疑似非 Vault Transit 端点或中间层异常，fail-closed 拒绝解析（防无界读内存）。
	vaultMaxResponseBytes = 1 << 20
	// vaultCacheMaxEntries 是 decrypt 缓存的软阈值：插入时 len(cache) 达到该规模先做一次
	// 过期项软清理。TTL 窗口内的活跃 distinct 密文数远小于此值（正常读路径只 Load 同一
	// 凭据文件），软清理足以回收；它不是硬上限——硬上限见 vaultCacheHardLimit。
	vaultCacheMaxEntries = 1000
	// vaultCacheHardLimit 是 decrypt 缓存的硬顶（2×软阈值）：软清理后 len(cache) 仍达到
	// 硬顶 → 驱逐任意活项，保证 map 规模有界（防 TTL 窗口内活跃 distinct 密文超软阈值时
	// 每次插入 O(n) 软清理清不掉导致的无界增长）。
	vaultCacheHardLimit = 2 * vaultCacheMaxEntries
)

// isVaultCiphertext 判断字符串是否为 Vault Transit 密文：匹配 vault:v + 一位以上数字 + ':'。
// 版本段随 key rotate 递增（vault:v1: / vault:v2: / ...），不做精确 v1 匹配（手写解析，
// 无需 regexp）。
func isVaultCiphertext(s string) bool {
	if !strings.HasPrefix(s, vaultCiphertextMagic) {
		return false
	}
	rest := s[len(vaultCiphertextMagic):]
	digits := 0
	for rest != "" && rest[0] >= '0' && rest[0] <= '9' {
		digits++
		rest = rest[1:]
	}
	return digits >= 1 && strings.HasPrefix(rest, ":")
}

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
// `http.Client{Timeout}`。两分支 client 均禁止跟随重定向（防 X-Vault-Token 外泄）。CacheTTL
// >0 → 初始化 decrypt 结果缓存 map；=0 → 缓存关闭（Decrypt 直查 Vault）。
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
	s := &VaultTransitStorer{
		addr:    strings.TrimRight(opts.Addr, "/"),
		mount:   mount,
		keyName: opts.KeyName,
		token:   opts.Token,
		aadPath: opts.AADPath,
		client:  client,
	}
	if opts.CacheTTL > 0 {
		s.cacheTTL = opts.CacheTTL
		s.cache = make(map[string]vaultCacheEntry)
	}
	return s, nil
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
// Vault 响应的 data.ciphertext 原样（含 vault:v<N>: 版本前缀——N 随 key rotate 递增，
// 保存即原样字节）。
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
	ct := out.Data.Ciphertext
	if ct == nil || !isVaultCiphertext(*ct) {
		return nil, fmt.Errorf("vault: encrypt 响应 data.ciphertext 缺失或非 %s<N>: 前缀（拒绝落盘不可解密文）", vaultCiphertextMagic)
	}
	return []byte(*ct), nil
}

// Decrypt 把 Vault Transit 密文解回明文。输入非 vault:v<N>: 前缀直接报错（不请求 Vault，
// 防明文误喂）。缓存开启时（CacheTTL>0）先查短 TTL 缓存：命中未过期 → 直接返回明文副本，
// 不请求 Vault；未命中 → POST {addr}/v1/{mount}/decrypt/{keyName}（请求体
// {"ciphertext": <密文>, "context": base64(AADPath)}），响应 data.plaintext 经 base64 解码
// 后写入缓存并返回。
func (s *VaultTransitStorer) Decrypt(ciphertext []byte) ([]byte, error) {
	if !isVaultCiphertext(string(ciphertext)) {
		return nil, fmt.Errorf("vault: decrypt 拒绝非 %s<N>: 前缀输入（%d 字节，疑似明文误喂）", vaultCiphertextMagic, len(ciphertext))
	}
	key := string(ciphertext)
	if s.cache != nil {
		if pt, ok := s.cacheGet(key); ok {
			return pt, nil
		}
	}
	reqBody, err := json.Marshal(vaultDecryptRequest{
		Ciphertext: key,
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
	if s.cache != nil {
		s.cachePut(key, pt, time.Now().Add(s.cacheTTL))
	}
	return pt, nil
}

// cacheGet 查询解密缓存（调用方保证 s.cache != nil）。命中且未过期 → 返回明文副本 + true；
// 未命中 → (nil, false)。发现已过期 → 顺手 delete 释放（明文不应滞留超过 TTL，且不必依赖
// 后续惰性清理时机）。
func (s *VaultTransitStorer) cacheGet(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return nil, false
	}
	if !time.Now().Before(entry.expires) {
		delete(s.cache, key)
		return nil, false
	}
	// 返回副本：防调用方改写内部缓存 buffer。
	return append([]byte(nil), entry.plaintext...), true
}

// cachePut 写入解密缓存（调用方保证 s.cache != nil）。明文存副本（防外部改写）。内存上限
// 双层保障：先软清理（len ≥ 软阈值时清一次过期项），软清理后仍达硬顶则驱逐任意活项
// （TTL 窗口内活跃 distinct 密文超软阈值时仍保证 map 有界）。
func (s *VaultTransitStorer) cachePut(key string, plaintext []byte, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= vaultCacheMaxEntries {
		s.sweepExpiredLocked()
	}
	if len(s.cache) >= vaultCacheHardLimit {
		s.evictToHardLimitLocked()
	}
	s.cache[key] = vaultCacheEntry{
		plaintext: append([]byte(nil), plaintext...),
		expires:   expires,
	}
}

// sweepExpiredLocked 软清理一次全部已过期缓存项（调用方须持有 s.mu）。
func (s *VaultTransitStorer) sweepExpiredLocked() {
	now := time.Now()
	for k, e := range s.cache {
		if !now.Before(e.expires) {
			delete(s.cache, k)
		}
	}
}

// evictToHardLimitLocked 硬顶兜底：驱逐任意活项直至 len(cache) < vaultCacheHardLimit
// （调用方须持有 s.mu；驱逐后由调用方插入新项，map 规模保持在硬顶以内）。
func (s *VaultTransitStorer) evictToHardLimitLocked() {
	for k := range s.cache {
		if len(s.cache) < vaultCacheHardLimit {
			return
		}
		delete(s.cache, k)
	}
}

// Probe 验证 Vault 可达性 + token 有效性：POST {addr}/v1/auth/token/lookup-self
// （token 自查端点，least-privilege token 亦可自查）。网络错 / 非 200（token 无效 403 /
// 权限不足）→ error——供 backend=vault 启动探活 fail-fast（空 store 首启也探，防配错 Vault
// 静默启动到首写才炸）。
func (s *VaultTransitStorer) Probe() error {
	endpoint := s.addr + "/v1/auth/token/lookup-self"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("vault: 构造探活请求失败: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault: 探活失败（Vault 不可达）: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("vault: 读取探活响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vault: 探活失败（token 无效或权限不足，HTTP %d）", resp.StatusCode)
	}
	return nil
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
