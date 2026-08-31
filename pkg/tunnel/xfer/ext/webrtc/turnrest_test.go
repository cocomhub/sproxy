// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// restCredJSON 是 coturn REST API 的 JSON 响应体（服务端已算好 username/password，
// 客户端只需透传到 ICEServer）。
type restCredJSON struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TTL      int64  `json:"ttl"`
}

// cleanupTURNRESTGlobals 恢复 TURN REST 全局变量到默认状态（空 URL 清空配置与缓存），
// 防止测试间及对其他 TURN 测试的污染。
func cleanupTURNRESTGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = SetTURNRESTURL("", "", "")
	})
}

// countingRESTServer 构造返回固定 REST 凭据的 httptest 假服务端并计数请求数。
// status 非 200 时该次请求返回该状态（模拟 404/5xx）。用 t.Cleanup 关闭服务端。
func countingRESTServer(t *testing.T, resp restCredJSON, status int) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"username":%q,"password":%q,"ttl":%d}`, resp.Username, resp.Password, resp.TTL)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// findTURNEntry 返回配置中第一个以 turn: 开头的 ICEServer 条目（无则 nil）。
// 注意：用返回值而非 t.Fatal，便于在并发 goroutine 中安全调用（t.Fatalf 非并发安全）。
func findTURNEntry(cfg webrtc.Configuration) *webrtc.ICEServer {
	for i := range cfg.ICEServers {
		entry := &cfg.ICEServers[i]
		for _, u := range entry.URLs {
			if strings.HasPrefix(u, "turn:") {
				return entry
			}
		}
	}
	return nil
}

// TestSetTURNRESTURL_Security 验证 SetTURNRESTURL 的安全校验（fail-closed）：
// https 通过；http 仅限 loopback（非 loopback 明文拒绝）；非 http scheme 拒绝；
// 缺 host 拒绝；空 username 拒绝；参数超长拒绝。
func TestSetTURNRESTURL_Security(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cases := []struct {
		name    string
		url     string
		user    string
		svc     string
		wantErr bool
	}{
		{"https 通过", "https://turn.example.com/turn", "api-user", "prod", false},
		{"https 无 service", "https://turn.example.com/turn", "api-user", "", false},
		{"loopback http 通过", "http://127.0.0.1:3478/turn", "u", "", false},
		{"localhost http 通过", "http://localhost:3478/turn", "u", "", false},
		{"非 loopback http 拒绝", "http://turn.example.com/turn", "u", "", true},
		{"非 http scheme 拒绝", "ws://turn.example.com/turn", "u", "", true},
		{"缺 host 拒绝", "https://", "u", "", true},
		{"空 username 拒绝", "https://turn.example.com/turn", "", "", true},
		{"超长 url 拒绝", "https://turn.example.com/" + strings.Repeat("a", maxTURNParamLen+1), "u", "", true},
		{"超长 user 拒绝", "https://turn.example.com/turn", strings.Repeat("u", maxTURNParamLen+1), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SetTURNRESTURL(tc.url, tc.user, tc.svc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应拒绝非法配置: url=%q user=%q", tc.url, tc.user)
				}
			} else if err != nil {
				t.Fatalf("应接受合法配置: %v", err)
			}
		})
	}
}

// TestSetTURNRESTURL_EmptyClears 验证空 URL 清空 REST 配置（供测试复位/CLI 停用）。
func TestSetTURNRESTURL_EmptyClears(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	if err := SetTURNRESTURL("https://turn.example.com/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	if turnRESTURL == "" {
		t.Fatal("设置后 turnRESTURL 应为非空")
	}
	if err := SetTURNRESTURL("", "", ""); err != nil {
		t.Fatalf("SetTURNRESTURL(空): %v", err)
	}
	if turnRESTURL != "" {
		t.Fatalf("空 URL 应清空配置，turnRESTURL = %q", turnRESTURL)
	}
}

// TestDefaultConfig_TURNRESTEntry 验证 REST 凭据透传到 defaultConfig() 的 TURN 条目：
// 请求按 coturn REST 惯例带 username/service 查询参数；响应 username/password 原样透传。
func TestDefaultConfig_TURNRESTEntry(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		q := r.URL.Query()
		if q.Get("username") != "api-user" {
			t.Errorf("请求 username 参数 = %q, want api-user", q.Get("username"))
		}
		if q.Get("service") != "prod" {
			t.Errorf("请求 service 参数 = %q, want prod", q.Get("service"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"username":"3600:api-user","password":"dGhpcy1pcy1iYXNlNjQ=","ttl":3600}`)
	}))
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("static-user", "static-pass")
	if err := SetTURNRESTURL(srv.URL+"/turn", "api-user", "prod"); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}

	cfg := defaultConfig()
	entry := findTURNEntry(cfg)
	if entry == nil {
		t.Fatalf("defaultConfig() 应包含 TURN 条目: %+v", cfg)
	}
	if entry.Username != "3600:api-user" {
		t.Errorf("TURN Username = %q, want 3600:api-user（REST 响应透传）", entry.Username)
	}
	if entry.Credential != "dGhpcy1pcy1iYXNlNjQ=" {
		t.Errorf("TURN Credential = %q, want dGhpcy1pcy1iYXNlNjQ=（REST 响应透传）", entry.Credential)
	}
	if entry.CredentialType != webrtc.ICECredentialTypePassword {
		t.Errorf("TURN CredentialType = %v, want password", entry.CredentialType)
	}
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Errorf("首次 newPC 应恰好拉取 1 次，实际 %d", n)
	}
}

// TestTURNREST_TTLCache 验证 TTL 内多次 newPC 不重拉（httptest 计数 = 1）。
func TestTURNREST_TTLCache(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	srv, requests := countingRESTServer(t, restCredJSON{Username: "3600:u", Password: "cA==", TTL: 3600}, http.StatusOK)
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	defaultConfig()
	defaultConfig()
	if n := atomic.LoadInt32(requests); n != 1 {
		t.Fatalf("TTL 内多次 newPC 应只拉取 1 次，实际 %d", n)
	}
}

// TestTURNREST_RefetchWhenLowTTL 验证剩余 TTL 低于续期阈值（60s）时下次 PC 前续期。
func TestTURNREST_RefetchWhenLowTTL(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	srv, requests := countingRESTServer(t, restCredJSON{Username: "30:u", Password: "cA==", TTL: 30}, http.StatusOK)
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	defaultConfig() // 首次拉取，缓存 TTL=30s
	defaultConfig() // 剩余 TTL < 60s → 续期
	if n := atomic.LoadInt32(requests); n != 2 {
		t.Fatalf("TTL 低于阈值应续期拉取，实际 %d 次", n)
	}
}

// TestTURNREST_FailureFallbackToStatic 验证 REST 拉取失败（5xx）不 panic，
// defaultConfig() 回落静态凭据。
func TestTURNREST_FailureFallbackToStatic(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	srv, _ := countingRESTServer(t, restCredJSON{}, http.StatusInternalServerError)
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("static-user", "static-pass")
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	cfg := defaultConfig() // 不 panic
	entry := findTURNEntry(cfg)
	if entry == nil {
		t.Fatalf("REST 失败应回落静态 TURN 条目: %+v", cfg)
	}
	if entry.Username != "static-user" {
		t.Errorf("回落静态 Username = %q, want static-user", entry.Username)
	}
	if entry.Credential != "static-pass" {
		t.Errorf("回落静态 Credential = %q, want static-pass", entry.Credential)
	}
}

// TestTURNREST_FailureNoStatic_NoTurnEntry 验证 REST 拉取失败且无静态凭据时，
// defaultConfig() 不 panic、回落仅 STUN（无 TURN 条目）。
func TestTURNREST_FailureNoStatic_NoTurnEntry(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	srv, _ := countingRESTServer(t, restCredJSON{}, http.StatusNotFound)
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	cfg := defaultConfig() // 不 panic
	assertNoTurnEntry(t, cfg)
}

// TestTURNREST_MalformedResponse_FallsBack 验证畸形响应（非法 JSON / 缺字段）不 panic，
// 回落静态凭据。
func TestTURNREST_MalformedResponse_FallsBack(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	// 非法 JSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{invalid`)
	}))
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	SetTURNCredential("static-user", "static-pass")
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	cfg := defaultConfig()
	entry := findTURNEntry(cfg)
	if entry == nil {
		t.Fatalf("畸形响应应回落静态 TURN 条目: %+v", cfg)
	}
	if entry.Username != "static-user" {
		t.Errorf("畸形响应回落 Username = %q, want static-user", entry.Username)
	}

	// 缺字段（username/password 为空）→ 同样回落
	srv2, _ := countingRESTServer(t, restCredJSON{}, http.StatusOK) // 空字段响应
	defer srv2.Close()
	if err := SetTURNRESTURL(srv2.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	cfg = defaultConfig()
	entry = findTURNEntry(cfg)
	if entry == nil {
		t.Fatalf("缺字段响应应回落静态 TURN 条目: %+v", cfg)
	}
	if entry.Username != "static-user" {
		t.Errorf("缺字段响应回落 Username = %q, want static-user", entry.Username)
	}
}

// TestTURNREST_SingleFlightConcurrent 验证首次并发 newPC 只拉取一次（单飞）。
func TestTURNREST_SingleFlightConcurrent(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		time.Sleep(100 * time.Millisecond) // 拉长处理窗口，让并发请求汇聚
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"username":"3600:u","password":"cA==","ttl":3600}`)
	}))
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := defaultConfig()
			if findTURNEntry(cfg) == nil {
				t.Errorf("并发下应拿到 TURN 条目")
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Fatalf("首次并发 newPC 应只拉取 1 次，实际 %d", n)
	}
}

// TestTURNREST_FetchTimeout 验证 REST 拉取受 turnRESTFetchTimeout 约束（服务端不响应时
// 在 2s 超时内失败，不无限阻塞打洞首 PC），失败后回落仅 STUN。
func TestTURNREST_FetchTimeout(t *testing.T) {
	cleanupTURNRESTGlobals(t)
	cleanupWebrtcGlobals(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 挂起直到客户端取消（客户端 2s 超时）
	}))
	defer srv.Close()

	SetTURNServers([]string{"turn:relay.example.com:3478"})
	if err := SetTURNRESTURL(srv.URL+"/turn", "u", ""); err != nil {
		t.Fatalf("SetTURNRESTURL: %v", err)
	}
	start := time.Now()
	cfg := defaultConfig()
	elapsed := time.Since(start)
	assertNoTurnEntry(t, cfg) // 超时失败 → 回落仅 STUN，不 panic
	if elapsed > 4*time.Second {
		t.Fatalf("REST 拉取应受 %v 超时约束，实际阻塞 %v", turnRESTFetchTimeout, elapsed)
	}
	if elapsed < turnRESTFetchTimeout-500*time.Millisecond {
		t.Fatalf("REST 拉取应等待超时窗口（%v），实际过早返回 %v", turnRESTFetchTimeout, elapsed)
	}
}
