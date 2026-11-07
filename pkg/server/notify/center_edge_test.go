// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

// center_edge_test.go 覆盖 center.go 的边界与失败分支（正常路径已由 center_test.go /
// channels_test.go / am_test.go / webhook_sign_test.go 覆盖）：
//
//  1. 渠道失败面：空配置 / URL 非法 / 传输失败 / 非成功状态码；状态码契约差异
//     （wecom、serverchan、webhook 必须恰好 200；alertmanager、grafana 仅 >=400 失败）；
//     Grafana 无 token 时回落 basic auth。
//  2. 中心生命周期：重复注册、幂等 Close、渠道快照（Channels/HasChannel/channelNames）。
//  3. 路由与派发边界：ruleMatch 的 object 过滤、目标渠道未注册时静默跳过。
//  4. 重试出口：ctx 取消、Close 打断在途重试；历史 ring 淘汰；jsonMarshal 失败回落。
//  5. 邮箱渠道：配置不全 / ctx 已取消 / 默认 SMTP 发送失败；mimeEncode ASCII 直通。
//  6. VerifyWebhookSignature：时间戳非数字、skew<=0 回落默认窗口。
//
// 外部 HTTP 一律 httptest（仅绑 127.0.0.1）。

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
)

// TestNotifierError_Unwrap 渠道错误包装应暴露内层原因（errors.Is 可用）。
func TestNotifierError_Unwrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("boom")
	err := &NotifierError{Channel: "webhook", Err: inner}
	if got := err.Error(); !strings.Contains(got, "webhook") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q，应含渠道名与内层原因", got)
	}
	if !errors.Is(err, inner) {
		t.Fatalf("Unwrap 未暴露内层错误: %v", err)
	}
}

// TestNotifyCenter_RegisterDuplicateAndCloseIdempotent 重名注册不覆盖、Close 幂等。
func TestNotifyCenter_RegisterDuplicateAndCloseIdempotent(t *testing.T) {
	t.Parallel()
	nc := NewNotifyCenter(config.NotifyConfig{}, nil)
	first := &fakeNotifier{name: "wecom"}
	if !nc.Register(first) {
		t.Fatal("首次注册应成功")
	}
	if nc.Register(&fakeNotifier{name: "wecom"}) {
		t.Fatal("重名注册应返回 false（不覆盖既有渠道）")
	}
	if got, ok := nc.channels["wecom"]; !ok || got != first {
		t.Fatalf("重名注册不应覆盖既有渠道实例")
	}
	if !nc.RegisterNotifier(&fakeNotifier{name: "serverchan"}) {
		t.Fatal("RegisterNotifier 首次注册应成功")
	}
	if nc.RegisterNotifier(&fakeNotifier{name: "serverchan"}) {
		t.Fatal("RegisterNotifier 重名应返回 false")
	}
	nc.Close()
	nc.Close() // 幂等：二次 Close 不 panic、不阻塞
}

// TestNotifyCenter_ChannelInspection 渠道快照/存在性/名称列表。
func TestNotifyCenter_ChannelInspection(t *testing.T) {
	t.Parallel()
	nc := NewNotifyCenter(config.NotifyConfig{}, nil)
	t.Cleanup(nc.Close)
	wecom := &fakeNotifier{name: "wecom"}
	nc.Register(wecom)
	nc.Register(&fakeNotifier{name: "email"})

	snapshot := nc.Channels()
	if len(snapshot) != 2 {
		t.Fatalf("Channels() 应有 2 个渠道，got %d", len(snapshot))
	}
	if snapshot["wecom"] != wecom {
		t.Fatalf("Channels() 应返回已注册实例")
	}
	delete(snapshot, "wecom") // 快照副本：改动不影响中心
	if !nc.HasChannel("wecom") {
		t.Fatal("删除快照条目不应影响中心注册表")
	}
	if nc.HasChannel("alertmanager") {
		t.Fatal("未注册渠道不应报告存在")
	}
	names := nc.channelNames()
	slices.Sort(names)
	if strings.Join(names, ",") != "email,wecom" {
		t.Fatalf("channelNames() = %v, want [email wecom]", names)
	}
}

// TestNotifyCenter_HistoryRingTruncates 历史超上限时淘汰最旧条目（有界 ring）。
func TestNotifyCenter_HistoryRingTruncates(t *testing.T) {
	t.Parallel()
	nc := NewNotifyCenter(config.NotifyConfig{}, nil)
	t.Cleanup(nc.Close)
	const overflow = 5
	nc.mu.Lock()
	for i := range historyCap + overflow {
		nc.addHistory(Event{TS: time.Unix(int64(i), 0), Action: "upload", Object: "f"}, "wecom", "sent", "")
	}
	nc.mu.Unlock()

	hist := nc.History()
	if len(hist) != historyCap {
		t.Fatalf("历史应被截断到 %d，got %d", historyCap, len(hist))
	}
	// History() 最近在前：末条是最旧保留项（第 overflow 条，即 TS=overflow）。
	if got := hist[len(hist)-1].TS.Unix(); got != overflow {
		t.Fatalf("最旧保留条目 TS = %d, want %d（应淘汰最早的 %d 条）", got, overflow, overflow)
	}
}

// TestRuleMatch_TableDriven 规则匹配：action 精确/通配 + object 空/精确。
func TestRuleMatch_TableDriven(t *testing.T) {
	t.Parallel()
	evt := Event{Action: "delete", Object: "a.txt"}
	tests := []struct {
		name string
		rule config.NotifyRule
		want bool
	}{
		{"action 精确匹配", config.NotifyRule{Action: "delete"}, true},
		{"action 不匹配", config.NotifyRule{Action: "upload"}, false},
		{"通配 action", config.NotifyRule{Action: "*"}, true},
		{"object 精确匹配", config.NotifyRule{Action: "*", Object: "a.txt"}, true},
		{"object 不匹配", config.NotifyRule{Action: "*", Object: "b.txt"}, false},
		{"action 不匹配时忽略 object", config.NotifyRule{Action: "upload", Object: "a.txt"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ruleMatch(tc.rule, evt); got != tc.want {
				t.Fatalf("ruleMatch(%+v) = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}

// TestNotifyCenter_DispatchSkipsUnregisteredChannel 规则指向未注册渠道 → 静默跳过。
func TestNotifyCenter_DispatchSkipsUnregisteredChannel(t *testing.T) {
	t.Parallel()
	nc := NewNotifyCenter(config.NotifyConfig{
		Rules: []config.NotifyRule{{Action: "*", Channels: []string{"wecom", "email"}}},
	}, nil)
	t.Cleanup(nc.Close)
	wecom := &fakeNotifier{name: "wecom"}
	nc.Register(wecom)

	nc.Dispatch(context.Background(), Event{Action: "upload", Object: "a.txt", Result: "success"})
	waitNotify(t, func() bool { return wecom.callsCount() == 1 && len(nc.History()) == 1 })

	hist := nc.History()
	if hist[0].Channel != "wecom" {
		t.Fatalf("未注册渠道不应留下历史条目，历史: %v", histStatuses(hist))
	}
	if wecom.callsCount() != 1 {
		t.Fatalf("wecom 应收到 1 条，got %d", wecom.callsCount())
	}
}

// TestNotifyCenter_RetryStopsOnContextCancel ctx 取消 → 重试立即结束（不再等退避）。
func TestNotifyCenter_RetryStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ch := &fakeNotifier{name: "wecom"}
	ch.fail.Store(true)
	nc := NewNotifyCenter(config.NotifyConfig{
		Rules:     []config.NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
		Retry:     5,
		RetryBase: 200 * time.Millisecond, // 长退避：取消必须能打断等待
	}, nil)
	nc.Register(ch)
	t.Cleanup(nc.Close)

	ctx, cancel := context.WithCancel(context.Background())
	nc.Dispatch(ctx, Event{Action: "upload", Object: "a.txt", Result: "error"})
	waitNotify(t, func() bool { return nc.attempts("wecom") >= 1 })
	cancel()
	waitNotify(t, func() bool { return len(nc.History()) >= 1 })

	hist := nc.History()
	if hist[0].Status != "failed" {
		t.Fatalf("取消后应记 failed，got %s", hist[0].Status)
	}
	if !strings.Contains(hist[0].Detail, context.Canceled.Error()) {
		t.Fatalf("失败详情应含 context canceled，got %q", hist[0].Detail)
	}
	if got := nc.attempts("wecom"); got != 1 {
		t.Fatalf("ctx 取消后不应继续重试，attempts = %d, want 1", got)
	}
}

// gateFailNotifier 在 Send 内阻塞到 gate 关闭后才失败（用于测 Close 打断在途重试）。
type gateFailNotifier struct {
	name string
	gate chan struct{}
}

func (g *gateFailNotifier) Name() string { return g.name }

func (g *gateFailNotifier) Send(context.Context, NotifyMessage) error {
	<-g.gate
	return &NotifierError{Channel: g.name, Err: errors.New("gated failure")}
}

// TestNotifyCenter_RetryStopsOnClose Close 关闭中心 → 在途重试走 "notify closed" 出口。
func TestNotifyCenter_RetryStopsOnClose(t *testing.T) {
	t.Parallel()
	ch := &gateFailNotifier{name: "wecom", gate: make(chan struct{})}
	nc := NewNotifyCenter(config.NotifyConfig{
		Rules:     []config.NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
		Retry:     3,
		RetryBase: time.Second, // 退避远大于测试时长：只能被 done 打断
	}, nil)
	nc.Register(ch)

	nc.Dispatch(context.Background(), Event{Action: "upload", Object: "a.txt", Result: "error"})
	closed := make(chan struct{})
	go func() {
		nc.Close()
		close(closed)
	}()
	<-nc.done      // Close 已关闭 done 信号
	close(ch.gate) // 放行在途 Send → 失败 → 重试循环命中 done
	<-closed

	hist := nc.History()
	if len(hist) != 1 || hist[0].Status != "failed" {
		t.Fatalf("关闭后应记 1 条 failed，got %v", histStatuses(hist))
	}
	if !strings.Contains(hist[0].Detail, "notify closed") {
		t.Fatalf("失败详情应含 notify closed，got %q", hist[0].Detail)
	}
}

// TestJSONMarshal_UnsupportedValue 不可序列化值回落 "{}"（不 panic）。
func TestJSONMarshal_UnsupportedValue(t *testing.T) {
	t.Parallel()
	if got := jsonMarshal(make(chan int)); got != "{}" {
		t.Fatalf("jsonMarshal(chan) = %q, want {}", got)
	}
	if got := jsonMarshal(map[string]string{"k": "v"}); got != `{"k":"v"}` {
		t.Fatalf("jsonMarshal(map) = %q", got)
	}
}

// TestNewFromConfig_RegistersAllChannels 配置装配：六类渠道全量注册。
func TestNewFromConfig_RegistersAllChannels(t *testing.T) {
	t.Parallel()
	nc := NewFromConfig(config.NotifyConfig{
		Enabled: true,
		Channels: config.NotifyChannelsConfig{
			Wecom:        config.WecomConfig{Webhook: "https://qyapi.example.com/hook"},
			ServerChan:   config.ServerChanConfig{SCTKey: "SCT-test"},
			Email:        config.EmailConfig{SMTPHost: "smtp.example.com", From: "a@b.c", To: []string{"d@e.f"}},
			Webhook:      config.WebhookConfig{URL: "https://hooks.example.com/x"},
			Alertmanager: config.AlertmanagerConfig{URL: "https://am.example.com/api/v2/alerts"},
			Grafana:      config.GrafanaConfig{URL: "https://grafana.example.com/api/annotations", Token: "tok"},
		},
	}, nil)
	t.Cleanup(nc.Close)

	want := []string{"alertmanager", "email", "grafana", "serverchan", "webhook", "wecom"}
	names := nc.channelNames()
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("注册渠道 = %v, want %v", names, want)
	}
}

// notifierChannelCase 是「按 base URL 构造渠道」的统一表（各错误面测试共用）。
type notifierChannelCase struct {
	name string
	// build 用 base URL 构造渠道（URL 类渠道直接用；serverchan 追加 /{key}.send 路径）。
	build func(baseURL string) Notifier
	// exact200 为真表示契约是「必须恰好 200」；为假表示仅 >=400 才算失败。
	exact200 bool
}

// notifierChannelCases 返回全部 URL 型渠道的构造表。
func notifierChannelCases() []notifierChannelCase {
	return []notifierChannelCase{
		{name: "alertmanager", build: func(u string) Notifier { return NewAlertmanagerNotifier(u) }},
		{name: "grafana", build: func(u string) Notifier { return NewGrafanaNotifier(u, "test-token", "", "") }},
		{name: "wecom", build: func(u string) Notifier { return NewWecomNotifier(u) }, exact200: true},
		{name: "serverchan", build: func(u string) Notifier { return NewServerChanNotifier(u+"/{key}.send", "SCT-test") }, exact200: true},
		{name: "webhook", build: func(u string) Notifier { return NewWebhookNotifier(config.WebhookConfig{URL: u}) }, exact200: true},
	}
}

// TestNotifierChannels_EmptyConfig 空渠道配置 → fail-closed（不发起请求）。
func TestNotifierChannels_EmptyConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ch   Notifier
	}{
		{"alertmanager", NewAlertmanagerNotifier("")},
		{"grafana", NewGrafanaNotifier("", "", "", "")},
		{"wecom", NewWecomNotifier("")},
		{"serverchan", NewServerChanNotifier("", "")},
		{"webhook", NewWebhookNotifier(config.WebhookConfig{})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ch.Name(); got != tc.name {
				t.Fatalf("Name() = %q, want %q", got, tc.name)
			}
			if err := tc.ch.Send(context.Background(), NotifyMessage{Title: "t"}); err == nil {
				t.Fatalf("%s 空配置应失败", tc.name)
			}
		})
	}
}

// TestNotifierChannels_InvalidURL URL 非法 → 构造请求即失败。
func TestNotifierChannels_InvalidURL(t *testing.T) {
	t.Parallel()
	const badURL = "://bad"
	for _, tc := range notifierChannelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.build(badURL).Send(context.Background(), NotifyMessage{Title: "t"}); err == nil {
				t.Fatalf("%s 非法 URL 应失败", tc.name)
			}
		})
	}
}

// TestNotifierChannels_TransportError 连接失败（server 已关闭）→ 返回错误。
func TestNotifierChannels_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	baseURL := srv.URL
	srv.Close() // 端口不再监听 → 拨号失败

	for _, tc := range notifierChannelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.build(baseURL).Send(context.Background(), NotifyMessage{Title: "t"}); err == nil {
				t.Fatalf("%s 传输失败应返回错误", tc.name)
			}
		})
	}
}

// TestNotifierChannels_ErrorStatus 500 → 全部渠道失败。
func TestNotifierChannels_ErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	// 必须用 Cleanup 而非 defer：并行的子测试在父函数返回后才执行，defer 会先关掉服务端。
	t.Cleanup(srv.Close)

	for _, tc := range notifierChannelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.build(srv.URL).Send(context.Background(), NotifyMessage{Title: "t"}); err == nil {
				t.Fatalf("%s 收到 500 应失败", tc.name)
			}
		})
	}
}

// TestNotifierChannels_StatusContract 204 的状态码契约差异：
// wecom/serverchan/webhook 只认 200（失败），alertmanager/grafana 只把 >=400 当失败（成功）。
func TestNotifierChannels_StatusContract(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	// 同上：并行子测试在父函数返回后执行，服务端须由 Cleanup 晚于子测试关闭。
	t.Cleanup(srv.Close)

	for _, tc := range notifierChannelCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.build(srv.URL).Send(context.Background(), NotifyMessage{Title: "t"})
			if tc.exact200 && err == nil {
				t.Fatalf("%s 要求恰好 200，204 应失败", tc.name)
			}
			if !tc.exact200 && err != nil {
				t.Fatalf("%s 只在 >=400 时失败，204 应成功: %v", tc.name, err)
			}
		})
	}
}

// TestGrafanaNotifier_BasicAuth 无 token 且配置 user → 回落 basic auth。
func TestGrafanaNotifier_BasicAuth(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		gotUser  string
		gotPass  string
		gotAuthz string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		mu.Lock()
		gotUser, gotPass, gotAuthz = user, pass, r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewGrafanaNotifier(srv.URL, "", "grafana-user", "grafana-pass")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "标注", Text: "body"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotUser != "grafana-user" || gotPass != "grafana-pass" {
		t.Fatalf("basic auth = %q/%q, want grafana-user/grafana-pass", gotUser, gotPass)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("grafana-user:grafana-pass"))
	if gotAuthz != want {
		t.Fatalf("Authorization = %q, want %q", gotAuthz, want)
	}
}

// TestEmailNotifier_SendIncompleteConfig SMTP 配置不全 → fail-closed（不调用发送）。
func TestEmailNotifier_SendIncompleteConfig(t *testing.T) {
	t.Parallel()
	full := config.EmailConfig{SMTPHost: "smtp.example.com", From: "a@b.c", To: []string{"d@e.f"}}
	tests := []struct {
		name   string
		mutate func(*config.EmailConfig)
	}{
		{"缺 SMTPHost", func(c *config.EmailConfig) { c.SMTPHost = "" }},
		{"缺 From", func(c *config.EmailConfig) { c.From = "" }},
		{"缺 To", func(c *config.EmailConfig) { c.To = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := full
			tc.mutate(&cfg)
			ch := NewEmailNotifier(cfg)
			ch.sendFunc = func(string, smtpAuth, string, []string, []byte) error {
				t.Fatal("配置不全不应调用 SMTP 发送")
				return nil
			}
			if err := ch.Send(context.Background(), NotifyMessage{Title: "t"}); err == nil {
				t.Fatalf("配置不全应失败")
			}
		})
	}
}

// TestEmailNotifier_DefaultSendFails 默认 SMTP 发送路径（真实 net/smtp 拨号）：
// 对端接受连接后立即关闭 → 读 greeting 失败 → 返回错误。
func TestEmailNotifier_DefaultSendFails(t *testing.T) {
	t.Parallel()
	host, port := startSilentSMTP(t)
	ch := NewEmailNotifier(config.EmailConfig{
		SMTPHost: host,
		Port:     port,
		From:     "sproxy@example.com",
		To:       []string{"admin@example.com"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "ASCII subject", Text: "body"}); err == nil {
		t.Fatal("对端立即断连时 SMTP 发送应失败")
	}
}

// startSilentSMTP 启动「接受连接后立即关闭」的假 SMTP 端点，返回 127.0.0.1 与端口。
func startSilentSMTP(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 127.0.0.1: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("非 TCP 监听地址: %T", ln.Addr())
	}
	return "127.0.0.1", tcpAddr.Port
}

// TestEmailNotifier_SendStopsOnContextCancel ctx 已取消 → 直接返回，不拨号。
func TestEmailNotifier_SendStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ch := NewEmailNotifier(config.EmailConfig{
		SMTPHost: "smtp.example.com",
		From:     "a@b.c",
		To:       []string{"d@e.f"},
	})
	called := false
	ch.sendFunc = func(string, smtpAuth, string, []string, []byte) error {
		called = true
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "t"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("ctx 已取消不应调用 SMTP 发送")
	}
}

// TestMimeEncode_ASCIIPassthrough ASCII 主题原样返回；非 ASCII 走 RFC 2047 编码。
func TestMimeEncode_ASCIIPassthrough(t *testing.T) {
	t.Parallel()
	const ascii = "[sproxy] upload: a.txt"
	if got := mimeEncode(ascii); got != ascii {
		t.Fatalf("mimeEncode(%q) = %q，ASCII 应原样返回", ascii, got)
	}
	encoded := mimeEncode("通知")
	const prefix, suffix = "=?UTF-8?B?", "?="
	if !strings.HasPrefix(encoded, prefix) || !strings.HasSuffix(encoded, suffix) {
		t.Fatalf("非 ASCII 主题应有 RFC 2047 包装: %q", encoded)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(encoded, prefix), suffix))
	if err != nil {
		t.Fatalf("base64 解码: %v", err)
	}
	if string(raw) != "通知" {
		t.Fatalf("解码 = %q, want 通知", raw)
	}
}

// TestVerifyWebhookSignature_TimestampParseAndDefaultSkew 时间戳非数字失败；skew<=0 回落默认窗口。
func TestVerifyWebhookSignature_TimestampParseAndDefaultSkew(t *testing.T) {
	t.Parallel()
	const secret = "test-secret"
	body := []byte(`{"title":"t"}`)
	now := time.Now()
	valid := signWebhookHeader(t, secret, body, now.Unix())

	tests := []struct {
		name   string
		header string
		skew   time.Duration
		wantOK bool
	}{
		{"时间戳非数字", "sha256=not-a-number.deadbeef", maxClockSkew, false},
		{"skew<=0 回落默认窗口", valid, 0, true},
		{"负 skew 回落默认窗口", valid, -time.Minute, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifyWebhookSignature(secret, tc.header, body, now, tc.skew)
			if tc.wantOK && err != nil {
				t.Fatalf("应验签通过: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("应验签失败，got nil")
			}
		})
	}
}
