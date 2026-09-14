// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_request.go 是 SDK 的**统一请求发送与签名**：doRequest / sendUnsigned / doRequestPrepared
// （上下文与 trace 注入、隧道或直连选择）、SproxySig 签名装配（RequestSigner / configSigner /
// sigRoundTripper / prehashBody），以及 doJSON 的响应解包与 Success 检查。
//
// 拆分说明见 client.go 顶部。

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/telemetry"
)

// 直连模式下拼接 serverURL + urlPath 构造完整 URL。
func (c *FileClient) doRequest(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header) (*http.Response, error) {
	// SproxySig 请求签名认证（AccessKey/AccessKeySecret）：发送前预计算 body 哈希，
	// 构造 Authorization 头；Secret 只本端计算签名，永不上线。api_keys 场景用 Bearer。
	// 注入自定义 Signer 时（WithRequestSigner）由该 Signer 全权接管签名与 body 处理，
	// 不再走默认 ConfigSigner 的 prehashBody / Authorization 装配。
	//
	// sendNoAuth（WithSendNoAuth，M14 TOTP 显式无凭据链路）：强制短路全部签名路径
	// （含注入的自定义 Signer ），请求直达公开端点——防带过期配置凭据的签名头被
	// authMiddleware 401 拒绝，TOTP 注册/登录整体不可用。
	if c.sendNoAuthEnabled() {
		return c.sendUnsigned(ctx, method, urlPath, body, headers)
	}
	if c.requestSigner != nil {
		req, err := http.NewRequestWithContext(ctx, method, urlPath, body)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		for k, vals := range headers {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}
		if serr := c.requestSigner.Sign(ctx, req); serr != nil {
			return nil, fmt.Errorf("SproxySig 签名失败: %w", serr)
		}
		return c.doRequestPrepared(ctx, req)
	}
	if c.accessKeySecret != "" {
		sigAuth, signedBody, cleanup, serr := c.signRequest(method, urlPath, body)
		if serr != nil {
			return nil, fmt.Errorf("SproxySig 签名失败: %w", serr)
		}
		if cleanup != nil {
			defer cleanup()
		}
		body = signedBody
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Authorization", sigAuth)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlPath, body)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return c.doRequestPrepared(ctx, req)
}

// sendUnsigned 构造**不签名**的直连请求（M14 TOTP 显式无凭据链路用）：与 doRequest
// 的核心装配共享，但跳过 signRequest / 注入 Signer——请求不带 Authorization 头直达
// 服务端公开端点（register/nonce/login）。
func (c *FileClient) sendUnsigned(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlPath, body)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	return c.doRequestPrepared(ctx, req)
}

// doRequestPrepared 完成追踪 span 注入并选择传输路径（隧道 / xfer / 直连）。
func (c *FileClient) doRequestPrepared(ctx context.Context, req *http.Request) (*http.Response, error) {
	// 追踪：为本次请求建立 span，并把 traceparent 头注入到请求头中。
	// tracer 为 nil 时（如 WithTracer(nil)）回退到默认 slog 实现，避免 nil 解引用。
	tracer := c.tracer
	if tracer == nil {
		tracer = telemetry.New()
	}
	ctx2, end := tracer.StartSpan(ctx, req.Method+" "+req.URL.Path)
	defer end()
	tracer.Inject(ctx2, httpHeaderCarrier{req.Header})
	// 请求上下文改用 ctx2：span 生命周期覆盖实际传输，且后续 Context 版日志自动带 trace_id/span_id。
	req = req.WithContext(ctx2)

	var resp *http.Response
	var err error
	if c.tunnelClient != nil {
		if c.initError != nil {
			return nil, c.initError
		}
		// 隧道模式：使用相对 URL，隧道客户端处理加密
		resp, err = c.tunnelClient.Do(req)
		return closeBodyIfErr(resp, err)
	}

	if c.initError != nil {
		if !c.allowTransportFallback {
			return nil, fmt.Errorf("transport initialization failed: %w", c.initError)
		}
		c.logger.WarnContext(ctx2, "transport unavailable, falling back to direct mode", "init_error", c.initError)
	}

	if c.xferName != "" {
		resp, err = c.doRequestViaXfer(req)
		return closeBodyIfErr(resp, err)
	}

	// 直连模式：补全 server URL
	fullURL := c.serverURL + req.URL.Path
	if req.URL.RawQuery != "" {
		fullURL += "?" + req.URL.RawQuery
	}
	req.URL, err = url.Parse(fullURL)
	if err != nil {
		return nil, fmt.Errorf("解析 URL 失败: %w", err)
	}
	// 直连模式且配置了多用户 API 密钥（api_keys，非 SproxySig）时注入 Bearer 头
	if c.authToken != "" && c.accessKeySecret == "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	hc := c.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err = hc.Do(req)
	return closeBodyIfErr(resp, err)
}

// RequestSigner 是请求签名器 seam：宿主可注入自有凭据源/签名器（替换默认
// ConfigSigner）。Sign 在请求「发送前」调用，可改写请求头（含 Authorization）与
// 请求体（req.Body）；签名器新增的头部必须存在于最终请求中。
type RequestSigner interface {
	Sign(ctx context.Context, req *http.Request) error
}

// ErrSkeyIDRequired 是 v2「skey-id 必传」缺失的哨兵错误（configSigner.Sign 返回）。
// 供 sigRoundTripper.RoundTrip 以 errors.Is 精确判定并附加既有引导文案；注入自定义
// Signer 的自定义错误原样透传、不被该哨兵改写。
var ErrSkeyIDRequired = errors.New("access_key_id 未配置（v2 skey-id 必传）")

// configSigner 是默认签名器：用 FileClient 配置的 access_key/access_key_secret/
// access_key_id 构造 SproxySig v2 签名头（承接现状 signRequest/sigRoundTripper 行为，
// 含 WithAccessKey/WithAccessKeyID 等 option 注入的字段；accessKeySecret=="" 时不签名
// ——公开端点直达）。凭据从持有者 FileClient 实时读取（隧道客户端在 option 应用过程中
// 创建，access_key_id 可能随后由 WithAccessKeyID 写入，不能构造时快照）。
type configSigner struct {
	c *FileClient
}

// Sign 为请求构造 SproxySig 签名头（HEAD 无 body，其余直连路径预计算哈希；隧道外层
// /tunnel 标 UNSIGNED 且不触碰 body）。两路径共用同一签名语义（R4-M3）。
func (s *configSigner) Sign(ctx context.Context, req *http.Request) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("signRequest: 非法请求（nil req/url）")
	}
	c := s.c
	// v2 skey-id 强制必传：配置了 access_key 但缺 access_key_id 且非 renew 引导
	// （allowMissingEntryID）→ 报错（v2 协议要求；renew 引导例外见 RenewAccessKey）。
	if c.accessKey != "" && c.accessKeyID == "" && !c.allowMissingEntryID {
		return fmt.Errorf("%w: 请先 `sclient trust renew` 或配置 access_key_id", ErrSkeyIDRequired)
	}
	now := time.Now()
	h := sproxysig.Header{
		Version:    sproxysig.Version,
		AK:         c.accessKey,
		EntryID:    c.accessKeyID,
		TS:         now.UnixMilli(),
		Exp:        now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: sproxysig.UnsignedBody,
	}
	// 直连路径预计算 body 哈希——与现状 signRequest 一致：nil body → EmptyBodyHash
	// （prehashBody 对 nil 直接返回，无需独立分支）；非 nil 时才替换 req.Body（避免用
	// io.NopCloser 包裹 nil）。隧道外层（/tunnel）保持 UNSIGNED 且不得读取/替换
	// req.Body——流式加密帧为一次性不可重放流，spool 替换会破坏上行加密/关闭时序；
	// 原 sigRoundTripper 从不触碰 body（R3-M5 逐条对齐）。
	if !strings.HasSuffix(req.URL.Path, "/tunnel") {
		signedBody, bodyHash, cleanup, err := prehashBody(req.Body)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		h.BodySHA256 = bodyHash
		if signedBody != nil {
			req.Body = io.NopCloser(signedBody)
		}
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(c.accessKeySecret, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
	return nil
}

// signRequest 为请求构造 SproxySig 签名头，并返回可重放（已预计算哈希）的 body。
// v2 canonical：header 携带 skey-id=<skeyID>（c.accessKeyID），服务端
// verifySproxySigFromRing 以 (ak, skeyID) 精确取条目。skeyID 参与 canonical 拼装。
// **强制必传**：accessKey 非空但 skeyID 为空时返回错误（v2 协议要求；renew 引导
// 例外见 RenewAccessKey——首次 renew 前本端恰好无 skeyID）。
func (c *FileClient) signRequest(method, urlPath string, body io.Reader) (string, io.Reader, func(), error) {
	if c.accessKey != "" && c.accessKeyID == "" && !c.allowMissingEntryID {
		return "", nil, nil, fmt.Errorf("%w: 请先 `sclient trust renew` 或配置 access_key_id", ErrSkeyIDRequired)
	}
	pathPart, queryPart, _ := strings.Cut(urlPath, "?")
	signedBody, bodyHash, cleanup, err := prehashBody(body)
	if err != nil {
		return "", nil, nil, err
	}
	now := time.Now()
	h := sproxysig.Header{
		Version:    sproxysig.Version,
		AK:         c.accessKey,
		EntryID:    c.accessKeyID,
		TS:         now.UnixMilli(),
		Exp:        now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	return sproxysig.SignAndFormat(c.accessKeySecret, h, method, pathPart, queryPart), signedBody, cleanup, nil
}

// prehashBody 计算 body 的 SHA-256（发送前预计算，供签名）。
//   - nil body → EmptyBodyHash，原样返回；
//   - 可回绕 reader（bytes.Reader / *os.File 等）→ 哈希后回绕，返回原 reader；
//   - 一次性流（io.Pipe / multipart）→ 缓存到临时文件并流式哈希（有界内存），
//     返回临时文件 reader + cleanup（请求完成后删除）。
func prehashBody(body io.Reader) (io.Reader, string, func(), error) {
	if body == nil {
		return nil, sproxysig.EmptyBodyHash(), nil, nil
	}
	if s, ok := body.(io.Seeker); ok {
		h := sha256.New()
		if _, err := io.Copy(h, body); err != nil {
			return nil, "", nil, err
		}
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return nil, "", nil, err
		}
		return body, hex.EncodeToString(h.Sum(nil)), nil, nil
	}
	f, err := os.CreateTemp("", "sproxy-sig-body-*")
	if err != nil {
		return nil, "", nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return f, hex.EncodeToString(h.Sum(nil)), cleanup, nil
}

// httpHeaderCarrier 适配 http.Header 为 telemetry.Carrier（http.Header 本身
// 不实现 Carrier 接口所需的 Get/Set 签名）。
type httpHeaderCarrier struct{ h http.Header }

func (c httpHeaderCarrier) Get(k string) string { return c.h.Get(k) }
func (c httpHeaderCarrier) Set(k, v string)     { c.h.Set(k, v) }

// sigRequestSigner 返回持有者 FileClient 生效的签名器：注入自定义 Signer 时用之，
// 否则默认 ConfigSigner。逐请求实时解析（同 accessKey/accessKeyID 实时读取——隧道
// 客户端在 option 应用过程中创建，signer 可能随后注入）。
func (c *FileClient) sigRequestSigner() RequestSigner {
	if c.requestSigner != nil {
		return c.requestSigner
	}
	return &configSigner{c: c}
}

// sigRoundTripper 是隧道外层客户端的 RoundTripper：给每个 /tunnel 请求
// 注入 SproxySig 签名（body_sha256=UNSIGNED，流式 body 无法整体哈希）。
// 服务端 authMiddleware 验签后派生隧道密钥解密；无签名则 401。
//
// 凭据从持有者 FileClient 实时读取（ak/sk/skeyID）：隧道客户端在 option 应用
// 过程中创建（WithTunnel），此时 access_key_id 可能尚未被 WithAccessKeyID 写入，
// 故不能构造时快照。令本载体引用 *FileClient 避免并发访问字段（签名字段构造后
// 不再被 writer 修改，读安全）。v2 协议强制必传 skey-id（缺则报错，见 RoundTrip）。
type sigRoundTripper struct {
	base http.RoundTripper
	c    *FileClient
}

func (rt *sigRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := rt.c.sigRequestSigner().Sign(context.Background(), req); err != nil {
		// v2 缺 skey-id：附加既有标准文案（错误文案与旧 sigRoundTripper 逐字一致）；
		// 其余错误（含注入 Signer 的自定义错误）原样透传，不回译/改写。
		if errors.Is(err, ErrSkeyIDRequired) {
			return nil, fmt.Errorf("%w: 请先 `sclient trust renew` 或配置 access_key_id", ErrSkeyIDRequired)
		}
		return nil, err
	}
	return rt.base.RoundTrip(req)
}

// closeBodyIfErr 在 (resp, err) 同时非 nil 的情况下关闭 resp.Body，避免连接 / 句柄泄漏。
// 这是 http.Client.Do 在某些错误（例如 redirect 策略错误）下会返回的非典型形态：返回了响应但同时报错。
// 调用方拿到 err 时通常 return，不会自己 Close，所以这里兜底。
func closeBodyIfErr(resp *http.Response, err error) (*http.Response, error) {
	if err != nil && resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp, err
}

// successChecker 接口，响应体实现此接口时 doJSON 自动检查 Success 字段。
type successChecker interface {
	isSuccess() bool
	message() string
}

// doJSONResp 是 doJSON 的通用响应包装器，用于自动检查 Success 字段。
type doJSONResp struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

func (r *doJSONResp) isSuccess() bool { return r.Success }

func (r *doJSONResp) message() string { return r.Message }

// UploadResult 实现 successChecker 接口，支持 doJSON 自动检查。
func (r *UploadResult) isSuccess() bool { return r.Success }
func (r *UploadResult) message() string { return r.Message }

// doJSON 发送 JSON 请求体并解析 JSON 响应。
// 如果 respBody 实现了 successChecker 接口，会自动检查 Success 字段，
// 当 Success 为 false 时返回错误（包含 Message 字段）。
// 自动设置 Content-Type: application/json，在非 2xx 时返回错误。
func (c *FileClient) doJSON(ctx context.Context, method, urlPath string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, method, urlPath, bodyReader, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("请求失败 (HTTP %d): %s", resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", ErrNotFound, err.Error())
		}
		// 存储不足（HTTP 507）映射为 ErrStorageFull 哨兵错误，供调用方 errors.Is 精确判断
		// （链式操作的存储满退避重试依赖此判断，不再退化为脆弱的字符串匹配）
		if resp.StatusCode == http.StatusInsufficientStorage {
			return fmt.Errorf("%w: %s", ErrStorageFull, err.Error())
		}
		return err
	}

	if respBody != nil {
		limited := io.LimitReader(resp.Body, 10<<20) // 10MB 上限
		if err := json.NewDecoder(limited).Decode(respBody); err != nil {
			return fmt.Errorf("解析响应失败: %w", err)
		}

		// 自动检查 Success 字段
		if checker, ok := respBody.(successChecker); ok && !checker.isSuccess() {
			return fmt.Errorf("请求失败: %s", checker.message())
		}
	}
	return nil
}
