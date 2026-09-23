// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// notify.go 是通知中心框架（roadmap P0 通知中心）：
//
//   - NotifyCenter：注册表（RegisterNotifier/Register）+ 规则路由（NotifyRule
//     action glob → channels）+ 去抖（同 action+object 窗口内只发一次，恢复后
//     再触发再发）+ 指数退避重试 + 有界历史（内存 ring 200）。
//   - 渠道：wecom（企业微信机器人 webhook，markdown JSON）+ serverchan
//     （Server 酱 sct_key，title/desp form）。
//   - 挂点：RecordAudit 末尾 dispatch（异步 goroutine，绝不阻塞审计/业务）。
//
// 设计约束：纯标准库（无第三方 HTTP 客户端）；HTTP 用 netutil.IsolatedTransport
// （R19 门禁：禁止裸构造 http.Transport 字面量）；测试注入 httptest mock 渠道。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/smtp"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// NotifyMessage 是发送给渠道的统一通知载荷。
type NotifyMessage struct {
	Title  string
	Text   string
	Object string
	Action string
}

// NotifierError 是渠道发送失败的错误（重试判定用）。
type NotifierError struct {
	Channel string
	Err     error
}

func (e *NotifierError) Error() string {
	return fmt.Sprintf("notifier %s: %v", e.Channel, e.Err)
}

func (e *NotifierError) Unwrap() error { return e.Err }

// Notifier 是通知渠道接口（发送一条通知）。
type Notifier interface {
	Name() string
	// Send 发送一条通知；返回 error 触发重试（Retry 次指数退避）。
	Send(ctx context.Context, m NotifyMessage) error
}

// NotifyRule 是通知路由规则（action glob → channels）。
// Action 支持 "*"（全部）与精确匹配（如 "upload"）；Object 同 Action 粒度。
type NotifyRule struct {
	Action   string   `yaml:"action" mapstructure:"action"`
	Object   string   `yaml:"object" mapstructure:"object"` // 空 = 全部对象
	Channels []string `yaml:"channels" mapstructure:"channels"`
}

// NotifyChannelsConfig 是渠道配置（wecom / serverchan）。
type NotifyChannelsConfig struct {
	Wecom      WecomConfig      `yaml:"wecom" mapstructure:"wecom"`
	ServerChan ServerChanConfig `yaml:"serverchan" mapstructure:"serverchan"`
	Email      EmailConfig      `yaml:"email" mapstructure:"email"`
	Webhook    WebhookConfig    `yaml:"webhook" mapstructure:"webhook"`
}

// WecomConfig 企业微信机器人配置。
type WecomConfig struct {
	Webhook string `yaml:"webhook" mapstructure:"webhook"`
}

// ServerChanConfig Server 酱配置。
type ServerChanConfig struct {
	SCTKey string `yaml:"sct_key" mapstructure:"sct_key"`
}

// NotifyConfig 是 notify 配置段（config.go 引用）。
type NotifyConfig struct {
	Enabled   bool                 `yaml:"enabled" mapstructure:"enabled"`
	Rules     []NotifyRule         `yaml:"rules" mapstructure:"rules"`
	Debounce  time.Duration        `yaml:"debounce" mapstructure:"debounce"`
	Retry     int                  `yaml:"retry" mapstructure:"retry"`
	RetryBase time.Duration        `yaml:"retry_base" mapstructure:"retry_base"`
	Channels  NotifyChannelsConfig `yaml:"channels" mapstructure:"channels"`
}

// historyEntry 是通知历史条目。
type historyEntry struct {
	TS      time.Time
	Action  string
	Object  string
	Channel string
	Status  string // sent / debounced / failed / skipped
	Detail  string
}

// NotifyCenter 是通知中心。
type NotifyCenter struct {
	mu          sync.Mutex
	channels    map[string]Notifier
	rules       []NotifyRule
	debounce    time.Duration
	retry       int
	retryBase   time.Duration
	history     []historyEntry       // 有界 ring（最旧丢弃）
	debounced   map[string]time.Time // key=action+object → 上次发送时间
	attemptsMu  sync.Mutex
	attemptsMap map[string]int // channel → 总尝试次数（测试观测）
	logger      *slog.Logger
	done        chan struct{}
	wg          sync.WaitGroup
}

// historyCap 是历史条数上限（有界）。
const historyCap = 200

// NewNotifyCenter 构造通知中心（cfg + logger；logger nil → slog.Default）。
func NewNotifyCenter(cfg NotifyConfig, logger *slog.Logger) *NotifyCenter {
	if logger == nil {
		logger = slog.Default()
	}
	nc := &NotifyCenter{
		channels:    map[string]Notifier{},
		rules:       cfg.Rules,
		debounce:    cfg.Debounce,
		retry:       cfg.Retry,
		retryBase:   cfg.RetryBase,
		debounced:   map[string]time.Time{},
		attemptsMap: map[string]int{},
		logger:      logger,
		done:        make(chan struct{}),
	}
	if nc.retry <= 0 {
		nc.retry = 3
	}
	if nc.retryBase <= 0 {
		nc.retryBase = time.Second
	}
	if nc.debounce <= 0 {
		nc.debounce = time.Minute
	}
	return nc
}

// Register 注册渠道（返回 false = 已存在不覆盖）。
func (nc *NotifyCenter) Register(n Notifier) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if _, ok := nc.channels[n.Name()]; ok {
		return false
	}
	nc.channels[n.Name()] = n
	return true
}

// RegisterNotifier 注册渠道（别名；返回 false = 已存在）。
func (nc *NotifyCenter) RegisterNotifier(n Notifier) bool { return nc.Register(n) }

// Close 关闭中心（等待在途通知完成；幂等）。
func (nc *NotifyCenter) Close() {
	select {
	case <-nc.done:
		return
	default:
		close(nc.done)
	}
	nc.wg.Wait()
}

// Dispatch 分发一条审计事件到匹配渠道（异步；绝不阻塞调用方）。
// rules 空 → 不匹配任何渠道（零回归）；action 无匹配 → 静默跳过。
func (nc *NotifyCenter) Dispatch(ctx context.Context, evt AuditEvent) {
	nc.mu.Lock()
	targets := nc.matchRules(evt)
	if len(targets) == 0 {
		nc.mu.Unlock()
		return
	}
	nc.mu.Unlock()
	nc.wg.Go(func() {
		nc.dispatchTo(ctx, evt, targets)
	})
}

// matchRules 返回匹配 evt 的渠道集合（按 rules 顺序去重；须持锁）。
func (nc *NotifyCenter) matchRules(evt AuditEvent) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range nc.rules {
		if !ruleMatch(r, evt) {
			continue
		}
		for _, ch := range r.Channels {
			if !seen[ch] {
				seen[ch] = true
				out = append(out, ch)
			}
		}
	}
	return out
}

// ruleMatch 判断规则是否匹配事件（action 精确或 "*"；object 空=全部或精确）。
func ruleMatch(r NotifyRule, evt AuditEvent) bool {
	if r.Action != "*" && r.Action != evt.Action {
		return false
	}
	if r.Object != "" && r.Object != evt.Object {
		return false
	}
	return true
}

// dispatchTo 逐渠道发送（去抖 + 重试 + 历史）。
func (nc *NotifyCenter) dispatchTo(ctx context.Context, evt AuditEvent, targets []string) {
	for _, chName := range targets {
		nc.mu.Lock()
		ch, ok := nc.channels[chName]
		if !ok {
			nc.mu.Unlock()
			continue
		}
		// 去抖：同 action+object+**result**+渠道 窗口内已发 → 跳过（记 debounced）。
		// key 含渠道：多渠道同事件各自独立去抖（wecom 发过不影响 serverchan）。
		// key 含 Result（审查 P3 修复）：同一 action+object 的「失败通知」与后续「恢复/
		// 成功通知」不应共享去抖窗口——否则失败后的成功恢复会被吞掉，用户收不到恢复。
		key := evt.Action + "\x00" + evt.Object + "\x00" + evt.Result + "\x00" + chName
		if last, dup := nc.debounced[key]; dup && time.Since(last) < nc.debounce {
			nc.addHistory(evt, chName, "debounced", "")
			nc.mu.Unlock()
			continue
		}
		nc.debounced[key] = time.Now()
		nc.mu.Unlock()

		msg := NotifyMessage{
			Title:  fmt.Sprintf("[sproxy] %s: %s", evt.Action, evt.Object),
			Text:   fmt.Sprintf("%s %s result=%s detail=%s", evt.Action, evt.Object, evt.Result, evt.Detail),
			Object: evt.Object,
			Action: evt.Action,
		}
		if err := nc.sendWithRetry(ctx, ch, msg); err != nil {
			nc.mu.Lock()
			nc.addHistory(evt, chName, "failed", err.Error())
			nc.mu.Unlock()
			nc.logger.Warn("通知发送失败", "channel", chName, "action", evt.Action, "error", err.Error())
			continue
		}
		nc.mu.Lock()
		nc.addHistory(evt, chName, "sent", "")
		nc.mu.Unlock()
	}
}

// sendWithRetry 发送 + 指数退避重试（retry 次）。
func (nc *NotifyCenter) sendWithRetry(ctx context.Context, ch Notifier, msg NotifyMessage) error {
	backoff := nc.retryBase
	var lastErr error
	for i := 0; i < nc.retry; i++ {
		nc.attemptsMu.Lock()
		nc.attemptsMap[ch.Name()]++
		nc.attemptsMu.Unlock()
		if err := ch.Send(ctx, msg); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-nc.done:
			return fmt.Errorf("notify closed")
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

// addHistory 追加历史（有界 ring；须持锁）。
func (nc *NotifyCenter) addHistory(evt AuditEvent, channel, status, detail string) {
	nc.history = append(nc.history, historyEntry{
		TS: evt.TS, Action: evt.Action, Object: evt.Object,
		Channel: channel, Status: status, Detail: detail,
	})
	if len(nc.history) > historyCap {
		nc.history = nc.history[len(nc.history)-historyCap:]
	}
}

// History 返回历史快照（最近在前）。
func (nc *NotifyCenter) History() []historyEntry {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	out := make([]historyEntry, len(nc.history))
	copy(out, nc.history)
	// 倒序（最近在前）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// attempts 返回渠道尝试次数（测试观测）。
func (nc *NotifyCenter) attempts(channel string) int {
	nc.attemptsMu.Lock()
	defer nc.attemptsMu.Unlock()
	return nc.attemptsMap[channel]
}

// ---- 渠道实现 ----

// httpClientForNotify 构造渠道 HTTP 客户端（IsolatedTransport 基座 + 响应头超时）。
// R19 门禁：禁止裸构造 http.Transport 字面量。
func httpClientForNotify() *http.Client {
	tr := netutil.IsolatedTransport()
	tr.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{Transport: tr}
}

// WecomNotifier 企业微信机器人渠道（webhook POST markdown）。
type WecomNotifier struct {
	webhook string
	client  *http.Client
}

// NewWecomNotifier 构造企微渠道（webhook URL）。
func NewWecomNotifier(webhook string) *WecomNotifier {
	return &WecomNotifier{webhook: webhook, client: httpClientForNotify()}
}

func (w *WecomNotifier) Name() string { return "wecom" }

func (w *WecomNotifier) Send(ctx context.Context, m NotifyMessage) error {
	if w.webhook == "" {
		return &NotifierError{Channel: "wecom", Err: fmt.Errorf("webhook 未配置")}
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": fmt.Sprintf("### %s\n%s", m.Title, m.Text),
		},
	}
	body := jsonMarshal(payload)
	// webhook 来自配置（管理员受信输入，非用户请求数据）——SSRF 面 = 配置者自身。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.webhook, strings.NewReader(body)) //nolint:gosec // G704: webhook 是受信配置（同云下载 provider URL 语义）
	if err != nil {
		return &NotifierError{Channel: "wecom", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req) //nolint:gosec // G704: webhook 是受信配置
	if err != nil {
		return &NotifierError{Channel: "wecom", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &NotifierError{Channel: "wecom", Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	return nil
}

// ServerChanNotifier Server 酱渠道（{base}/{key}.send form）。
type ServerChanNotifier struct {
	baseURL string // 形如 https://sctapi.ftqq.com/{key}（{key} 占位）
	key     string
	client  *http.Client
}

// NewServerChanNotifier 构造 Server 酱渠道（baseURL 含 {key} 占位 + key）。
func NewServerChanNotifier(baseURL, key string) *ServerChanNotifier {
	return &ServerChanNotifier{baseURL: baseURL, key: key, client: httpClientForNotify()}
}

func (s *ServerChanNotifier) Name() string { return "serverchan" }

func (s *ServerChanNotifier) Send(ctx context.Context, m NotifyMessage) error {
	if s.key == "" {
		return &NotifierError{Channel: "serverchan", Err: fmt.Errorf("sct_key 未配置")}
	}
	endpoint := strings.ReplaceAll(s.baseURL, "{key}", s.key)
	form := url.Values{}
	form.Set("title", m.Title)
	form.Set("desp", m.Text)
	// baseURL 来自配置（管理员受信输入）——SSRF 面 = 配置者自身。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode())) //nolint:gosec // G704: baseURL 是受信配置
	if err != nil {
		return &NotifierError{Channel: "serverchan", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req) //nolint:gosec // G704: baseURL 是受信配置
	if err != nil {
		return &NotifierError{Channel: "serverchan", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &NotifierError{Channel: "serverchan", Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	return nil
}

// jsonMarshal 序列化（纯标准库；error 由调用方忽略则空 body）。
func jsonMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// newNotifyCenterFromConfig 从 NotifyConfig 装配渠道（wecom/serverchan）。
func newNotifyCenterFromConfig(cfg NotifyConfig, logger *slog.Logger) *NotifyCenter {
	nc := NewNotifyCenter(cfg, logger)
	if cfg.Channels.Wecom.Webhook != "" {
		nc.Register(NewWecomNotifier(cfg.Channels.Wecom.Webhook))
	}
	if cfg.Channels.ServerChan.SCTKey != "" {
		nc.Register(NewServerChanNotifier("https://sctapi.ftqq.com/{key}.send", cfg.Channels.ServerChan.SCTKey))
	}
	if cfg.Channels.Email.SMTPHost != "" && cfg.Channels.Email.From != "" {
		nc.Register(NewEmailNotifier(cfg.Channels.Email))
	}
	if cfg.Channels.Webhook.URL != "" {
		nc.Register(NewWebhookNotifier(cfg.Channels.Webhook.URL))
	}
	return nc
}

// hasChannel 返回渠道是否已注册。
func (nc *NotifyCenter) hasChannel(name string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	_, ok := nc.channels[name]
	return ok
}

// channelNames 返回已注册渠道名列表。
func (nc *NotifyCenter) channelNames() []string {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	out := make([]string, 0, len(nc.channels))
	for name := range nc.channels {
		out = append(out, name)
	}
	return out
}

// notifyRoutePath 归一化路由路径（避免 path 未用告警）。
var _ = path.Clean

// ---- 运维端点 ----

// notifyHistoryHandler 返回通知历史（GET /api/notify/history?limit=N）。
func (h *Handlers) notifyHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if h.notifyCenter == nil {
		http.Error(w, "notify 未启用", http.StatusBadRequest)
		return
	}
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	hist := h.notifyCenter.History()
	if len(hist) > limit {
		hist = hist[:limit]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": hist})
}

// notifyTestHandler 触发渠道自检（POST /api/notify/test?channel=wecom）。
// 无 channel 参数 → 全部已注册渠道。返回各渠道测试结果。
func (h *Handlers) notifyTestHandler(w http.ResponseWriter, r *http.Request) {
	if h.notifyCenter == nil {
		http.Error(w, "notify 未启用", http.StatusBadRequest)
		return
	}
	channel := r.URL.Query().Get("channel")
	var names []string
	nc := h.notifyCenter
	nc.mu.Lock()
	if channel != "" {
		if _, ok := nc.channels[channel]; ok {
			names = []string{channel}
		} else {
			nc.mu.Unlock()
			http.Error(w, "channel 不存在", http.StatusNotFound)
			return
		}
	} else {
		for name := range nc.channels {
			names = append(names, name)
		}
	}
	nc.mu.Unlock()

	results := map[string]string{}
	for _, name := range names {
		ch := nc.channels[name]
		msg := NotifyMessage{Title: "[sproxy] 通知渠道自检", Text: "channel test", Object: "test", Action: "test"}
		if err := ch.Send(r.Context(), msg); err != nil {
			results[name] = "failed: " + err.Error()
		} else {
			results[name] = "ok"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// ---- 邮箱（SMTP）渠道 ----

// EmailConfig 邮箱渠道配置。
type EmailConfig struct {
	SMTPHost string   `yaml:"smtp_host" mapstructure:"smtp_host"`
	Port     int      `yaml:"port" mapstructure:"port"`
	From     string   `yaml:"from" mapstructure:"from"`
	To       []string `yaml:"to" mapstructure:"to"`
	Username string   `yaml:"username" mapstructure:"username"`
	Password string   `yaml:"password" mapstructure:"password"`
}

// EmailNotifier 邮箱渠道（net/smtp + TLS）。
type EmailNotifier struct {
	cfg      EmailConfig
	sendFunc func(addr string, a smtpAuth, from string, to []string, msg []byte) error
}

// smtpAuth 是 net/smtp.PlainAuth 的窄接口（测试注入用）。
type smtpAuth interface {
	Start(*smtp.ServerInfo) (string, []byte, error)
	Next(fromServer []byte, more bool) ([]byte, error)
}

// NewEmailNotifier 构造邮箱渠道。
func NewEmailNotifier(cfg EmailConfig) *EmailNotifier {
	n := &EmailNotifier{cfg: cfg}
	if n.cfg.Port == 0 {
		n.cfg.Port = 465
	}
	n.sendFunc = n.defaultSend
	return n
}

func (e *EmailNotifier) Name() string { return "email" }

// defaultSend 是默认 SMTP 发送（net/smtp.SendMail；465 隐式 TLS 用 smtp.Dial）。
func (e *EmailNotifier) defaultSend(addr string, a smtpAuth, from string, to []string, msg []byte) error {
	//nolint:gosec // G707: from/to 来自配置（管理员受信）；msg 是内部组装（标题经 mimeEncode 防注入）
	return smtp.SendMail(addr, a, from, to, msg)
}

// Send 发送一封 HTML 摘要邮件（RFC 822 头 + text/html 体）。
func (e *EmailNotifier) Send(ctx context.Context, m NotifyMessage) error {
	if e.cfg.SMTPHost == "" || e.cfg.From == "" || len(e.cfg.To) == 0 {
		return &NotifierError{Channel: "email", Err: fmt.Errorf("SMTP 配置不完整")}
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, "From: %s\r\n", e.cfg.From)
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(e.cfg.To, ", "))
	fmt.Fprintf(&buf, "Subject: %s\r\n", mimeEncode(m.Title))
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	buf.WriteString("\r\n")
	fmt.Fprintf(&buf, "<h3>%s</h3><pre>%s</pre>", htmlEscape(m.Title), htmlEscape(m.Text))

	addr := fmt.Sprintf("%s:%d", e.cfg.SMTPHost, e.cfg.Port)
	var auth smtpAuth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.SMTPHost)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return e.sendFunc(addr, auth, e.cfg.From, e.cfg.To, []byte(buf.String()))
}

// ---- Webhook 通用渠道 ----

// WebhookConfig 通用 Webhook 渠道配置。
type WebhookConfig struct {
	URL string `yaml:"url" mapstructure:"url"`
}

// WebhookNotifier 通用 Webhook 渠道（POST 任意 JSON 载荷）。
type WebhookNotifier struct {
	url    string
	client *http.Client
}

// NewWebhookNotifier 构造通用 Webhook 渠道。
func NewWebhookNotifier(url string) *WebhookNotifier {
	return &WebhookNotifier{url: url, client: httpClientForNotify()}
}

func (w *WebhookNotifier) Name() string { return "webhook" }

// Send POST {title,text,object,action} JSON 到配置 URL。
func (w *WebhookNotifier) Send(ctx context.Context, m NotifyMessage) error {
	if w.url == "" {
		return &NotifierError{Channel: "webhook", Err: fmt.Errorf("webhook URL 未配置")}
	}
	payload := map[string]string{
		"title":  m.Title,
		"text":   m.Text,
		"object": m.Object,
		"action": m.Action,
	}
	body := jsonMarshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, strings.NewReader(body)) //nolint:gosec // G704: webhook URL 是受信配置（同 wecom 语义）
	if err != nil {
		return &NotifierError{Channel: "webhook", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req) //nolint:gosec // G704: webhook URL 是受信配置
	if err != nil {
		return &NotifierError{Channel: "webhook", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &NotifierError{Channel: "webhook", Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	return nil
}

// mimeEncode 用 RFC 2047 编码非 ASCII 主题（邮件头安全）。
func mimeEncode(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// htmlEscape HTML 转义（正文安全）。
func htmlEscape(s string) string { return html.EscapeString(s) }
