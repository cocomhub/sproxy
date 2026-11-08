// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httpc 是**通用 HTTP 交互基座**（L1，见 `docs/design-http-orchestration.md`）：
// 统一「超时 / 重试分类 / 退避 / 取消 / 打点 / 指标」，并让 `RoundTripper` 可注入（httptest 可锁定）。
//
// 为什么需要（量化，2026-11-08 实测本仓库）：裸 http.Client / http.Transport / DefaultTransport 构造
// **89 处**、各写一遍的超时字段 **90 处**、指数退避 **≥5 处** —— 没有任何一处能统一回答
// 「这个请求会不会卡、能不能取消」。baidupcs 上传「卡 ~90s 且不可取消」正是把该问题留给第三方库的结果。
//
// 设计边界（**刻意不做**，见设计文档 §2）：
//   - 不做通用云盘客户端（协议/签名差异大，只统一 HTTP 交互）；
//   - 不预设业务重试语义：策略由调用点声明（`Options.Retry` / `DoOptions`）；
//   - **绝不自动重试非幂等请求**（默认仅 GET/HEAD/OPTIONS；PUT/DELETE/POST/PATCH 需显式声明）；
//   - 不接管认证（刷新钩子由调用方实现）。
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/pipe"
)

// Metrics 是可选指标钩子（nil ⇒ NullMetrics，零开销）。
type Metrics interface {
	// IncRequest 记录一次请求终态（statusClass 形如 "2xx"/"4xx"/"err"）。
	IncRequest(method, statusClass string)
	// IncRetry 记录一次重试（reason 形如 "5xx"/"429"/"neterr"）。
	IncRetry(reason string)
	// ObserveAttempt 记录单次尝试耗时。
	ObserveAttempt(method string, d time.Duration)
}

// NullMetrics 是默认空实现。
type NullMetrics struct{}

func (NullMetrics) IncRequest(string, string)            {}
func (NullMetrics) IncRetry(string)                      {}
func (NullMetrics) ObserveAttempt(string, time.Duration) {}

// Options 是客户端配置（零值可用：全部有安全默认）。
type Options struct {
	// Transport 是底层传输（nil ⇒ 见 Isolated）。
	// 注入自定义 Transport（如把 host 重写到 httptest.Server）即可在 CI 锁定协议行为。
	Transport http.RoundTripper
	// Isolated 选择**独立连接池**（`netutil.IsolatedTransport()`）。
	//
	// 默认 false ⇒ 使用**进程级共享池** `netutil.DefaultTransport()`：同用途/多实例复用
	// 连接与 TLS 会话（对齐 netutil 既定设计 + Go 官方 DefaultTransport 语义）。
	//
	// **共享池禁止调用 `CloseIdleConnections()`**（会打断全进程在途请求——历史事故）。
	// 需要"用完即关"或与其它实例隔离（短命 client、测试替身、库持有、不可信端点）时显式
	// 置 true（见 docs/design-http-client-policy.md §4）。
	Isolated bool
	// Timeout 是**单次尝试**超时（0 ⇒ 30s；经 ctx 施加 ⇒ 始终可取消）。
	Timeout time.Duration
	// OverallTimeout 是**整体**预算（0 ⇒ 不限；由上层 ctx 决定更佳）。
	OverallTimeout time.Duration
	// ClientTimeout 是**底层 http.Client.Timeout**（整体上限，**含 body 读取**）。
	//
	// 与 `Timeout`（单次尝试、经 ctx 施加）**语义不同**：迁移既有裸构造点时用本字段
	// 可保持行为等价（例如裸客户端构造带 Timeout: 30s ⇒ ClientTimeout: 30s）。
	ClientTimeout time.Duration
	// Jar 是可选 cookie jar（透传底层 client）。
	Jar http.CookieJar
	// CheckRedirect 是可选重定向策略（透传底层 client）。
	CheckRedirect func(req *http.Request, via []*http.Request) error
	// Retry 是重试策略（Attempts<=0 ⇒ 不重试）。
	Retry pipe.Policy
	// Env 提供可注入睡眠（测试零等待）。
	Env pipe.Env
	// Logger 为 nil 时静默（Debug 级打点）。
	Logger *slog.Logger
	// Metrics 为 nil 时使用 NullMetrics。
	Metrics Metrics
}

const defaultAttemptTimeout = 30 * time.Second

// Client 是线程安全的 HTTP 客户端。
type Client struct {
	o    Options
	http *http.Client
}

// NewHTTPClient 按规范构造**裸 `*http.Client`**（唯一集中出口）：
// 默认 Transport = `netutil.DefaultTransport()`（**进程级共享池**；`Isolated: true` 时用隔离池），
// 并透传 ClientTimeout/Jar/CheckRedirect。
//
// 用途：① 既有调用点只需要"一个规范 client"（不需要重试编排）时统一走这里，避免继续散落裸构造；
// ② 需要把 client 交给第三方（库/签名器）时也走这里。
// 需要重试/取消/打点编排的场景请用 `New(Options)` + `Do`。
func NewHTTPClient(o Options) *http.Client {
	tr := o.Transport
	if tr == nil {
		tr = defaultTransport(o.Isolated)
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       o.ClientTimeout,
		Jar:           o.Jar,
		CheckRedirect: o.CheckRedirect,
	}
}

// defaultTransport 返回默认 Transport：默认**共享池**（进程级），Isolated 时用独立池。
func defaultTransport(isolated bool) *http.Transport {
	if isolated {
		return netutil.IsolatedTransport()
	}
	return netutil.DefaultTransport()
}

// New 构造客户端。
func New(o Options) *Client {
	if o.Transport == nil {
		o.Transport = netutil.IsolatedTransport()
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultAttemptTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Metrics == nil {
		o.Metrics = NullMetrics{}
	}
	return &Client{o: o, http: NewHTTPClient(o)}
}

// DoOptions 是**单次调用**的语义声明（与 Options 分离：机制在客户端，策略在调用点）。
type DoOptions struct {
	// Idempotent 显式声明该请求可安全重放（PUT/POST/PATCH 需要）。
	Idempotent bool
	// Retry 覆盖客户端策略（零值 ⇒ 用客户端策略）。
	Retry pipe.Policy
	// Timeout 覆盖单次尝试超时（0 ⇒ 用客户端配置）。
	Timeout time.Duration
}

// HTTPError 表示服务端返回了非 2xx 终态（含最后一次响应体片段，便于定位）。
type HTTPError struct {
	StatusCode int
	Method     string
	URL        string
	Snippet    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("httpc: %s %s 返回 %d: %s", e.Method, e.URL, e.StatusCode, e.Snippet)
}

// ErrBodyNotReplayable 表示请求体不可重放，因而**拒绝重试**（fail-fast，避免静默发空 body）。
var ErrBodyNotReplayable = errors.New("httpc: 请求体不可重放，拒绝重试")

// Do 执行请求；默认按方法判定幂等性（GET/HEAD/OPTIONS 可重试）。
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.DoOpts(ctx, req, DoOptions{})
}

// DoOpts 执行请求（可显式声明幂等性以允许对 PUT/POST/PATCH 重试）。
//
// 返回：(resp, nil) 表示 2xx；(resp, *HTTPError) 表示非 2xx 终态（**响应体仍可读**）；
// (nil, err) 表示网络/取消/请求体不可重放/重试耗尽等错误。
func (c *Client) DoOpts(ctx context.Context, req *http.Request, opt DoOptions) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("httpc: nil request")
	}
	allowRetry := (opt.Idempotent || defaultIdempotent(req.Method)) && c.retryPolicy(opt).Attempts > 1
	if allowRetry && req.Body != nil && req.GetBody == nil {
		// 声明可重试但请求体不可重放 ⇒ fail-fast（不静默降级为"第二次发空 body"）。
		return nil, ErrBodyNotReplayable
	}
	attemptTimeout := c.attemptTimeout(opt)
	if c.o.OverallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.o.OverallTimeout)
		defer cancel()
	}

	pol := c.retryPolicy(opt)
	if !allowRetry {
		pol.Attempts = 1
	}
	pol.Retryable = retryableErr

	var lastResp *http.Response
	err := pipe.Retry(ctx, c.o.Env, pol, func(actx context.Context, attempt int) error {
		resp, aerr := c.attempt(actx, ctx, req, attempt, attemptTimeout)
		if resp != nil {
			lastResp = resp
		}
		return aerr
	})
	if err == nil {
		return lastResp, nil
	}
	var he *HTTPError
	if errors.As(err, &he) && lastResp != nil {
		return lastResp, he // 终态 HTTP 错误：响应交回调用方（可读 body 定位）
	}
	if lastResp != nil && lastResp.Body != nil {
		_ = lastResp.Body.Close()
	}
	return nil, err
}

// retryPolicy 返回调用点的重试策略（DoOptions 覆盖 > Options）。
func (c *Client) retryPolicy(opt DoOptions) pipe.Policy {
	if opt.Retry.Attempts > 0 {
		return opt.Retry
	}
	return c.o.Retry
}

// attemptTimeout 返回单次尝试超时。
func (c *Client) attemptTimeout(opt DoOptions) time.Duration {
	if opt.Timeout > 0 {
		return opt.Timeout
	}
	return c.o.Timeout
}

// attempt 执行单次尝试：per-attempt 超时（**可取消**）、重试时重建 body、打点与错误分类。
func (c *Client) attempt(actx, parent context.Context, req *http.Request, attempt int, timeout time.Duration) (*http.Response, error) {
	actx, cancel := context.WithTimeout(actx, timeout)
	defer cancel()

	r := req.Clone(actx)
	if attempt > 1 {
		if err := replayBody(req, r); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	resp, err := c.http.Do(r)
	c.o.Metrics.ObserveAttempt(req.Method, time.Since(start))
	if err != nil {
		c.logFail(req, attempt, 0, err)
		return nil, classifyNetErr(actx, parent, err, c.o.Metrics)
	}
	c.o.Metrics.IncRequest(req.Method, statusClass(resp.StatusCode))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	last := &HTTPError{
		StatusCode: resp.StatusCode, Method: req.Method, URL: req.URL.String(),
		Snippet: snippet(resp),
	}
	c.logFail(req, attempt, resp.StatusCode, last)
	if retryableStatus(resp.StatusCode) {
		c.o.Metrics.IncRetry(statusClass(resp.StatusCode))
	}
	return resp, last
}

// attemptTimeoutError 标记"**单次尝试**超时"（与整体 ctx 取消不同：前者可重试）。
type attemptTimeoutError struct{ cause error }

func (e *attemptTimeoutError) Error() string { return "httpc: 单次尝试超时: " + e.cause.Error() }
func (e *attemptTimeoutError) Unwrap() error { return e.cause }

// classifyNetErr 区分「单次尝试超时（可重试）」与「整体取消（不重试）」。
func classifyNetErr(actx, parent context.Context, err error, m Metrics) error {
	if actx.Err() != nil && parent.Err() == nil {
		m.IncRetry("timeout")
		return &attemptTimeoutError{cause: err}
	}
	if parent.Err() == nil {
		m.IncRetry("neterr")
	}
	return err
}

// CloseIdleConnections 关闭该客户端底层连接池的空闲连接（**仅对独立池安全**）。
//
// 语义（重要）：`Options.Isolated=true`（独立池）或调用方自备 Transport 时，用它释放空闲连接；
// **共享池（默认）禁止调用** —— 池的生命周期属于进程，任一处关闭都会打断全进程在途请求
// （见 docs/design-http-client-policy.md §4）。
func (c *Client) CloseIdleConnections() {
	if c == nil || c.http == nil {
		return
	}
	c.http.CloseIdleConnections()
}

// Get 便捷方法。
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, req)
}

// DoJSON 发送 JSON 并（可选）解析 JSON 响应（out 为 nil 时丢弃响应体）。
func (c *Client) DoJSON(ctx context.Context, method, url string, in, out any, opt DoOptions) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("httpc: marshal: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.DoOpts(ctx, req, opt)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("httpc: decode: %w", err)
	}
	return nil
}

// defaultIdempotent 判定方法默认是否可安全重放（**POST/PATCH 默认不重试**）。
func defaultIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// replayBody 为重试重建请求体（不可重放 ⇒ ErrBodyNotReplayable）。
func replayBody(orig, attempt *http.Request) error {
	if orig.Body == nil {
		return nil
	}
	if orig.GetBody == nil {
		return ErrBodyNotReplayable
	}
	b, err := orig.GetBody()
	if err != nil {
		return fmt.Errorf("httpc: 重建请求体: %w", err)
	}
	attempt.Body = b
	return nil
}

// retryableStatus 判定状态码是否可重试（429/5xx 可重试；其余 4xx 致命）。
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// retryableErr 判定错误是否可重试（HTTPError 按其状态码；ctx 取消/超时**不重试**）。
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*attemptTimeoutError](err); ok {
		return true // 单次尝试超时属瞬时故障 ⇒ 可重试
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // 整体取消/预算到期 ⇒ 不重试
	}
	if he, ok := errors.AsType[*HTTPError](err); ok {
		return retryableStatus(he.StatusCode)
	}
	return true // 网络类错误可重试
}

// statusClass 返回 "2xx"/"4xx"/"5xx" 形式。
func statusClass(code int) string {
	if code <= 0 {
		return "err"
	}
	return fmt.Sprintf("%dxx", code/100)
}

// snippet 读取（并恢复）响应体前若干字节，供错误定位（不吞掉 body）。
func snippet(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	const max = 256
	buf := make([]byte, max)
	n, _ := io.ReadFull(resp.Body, buf)
	head := string(buf[:n])
	resp.Body = &replayReader{head: []byte(head), rest: resp.Body}
	if len(head) > 120 {
		head = head[:120] + "…"
	}
	return head
}

// replayReader 把已读出的前缀与剩余流拼接回去（保证调用方仍能完整读 body）。
type replayReader struct {
	head []byte
	rest io.ReadCloser
}

func (r *replayReader) Read(p []byte) (int, error) {
	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]
		return n, nil
	}
	return r.rest.Read(p)
}

func (r *replayReader) Close() error { return r.rest.Close() }

// logFail 打点失败尝试（Debug；含 attempt/耗时/错误分类）。
func (c *Client) logFail(req *http.Request, attempt, status int, err error) {
	c.o.Logger.Debug("httpc 请求失败",
		"method", req.Method, "url", req.URL.String(), "attempt", attempt,
		"status", status, "err", err)
}

// RetryAfter 解析 Retry-After 头（秒；缺失或非法 ⇒ 0）。
func RetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
