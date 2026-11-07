// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 用户实测故障（2026-10-11）：距上次下载隔一段时间后开始下载报「未登录」，
// 手工执行 `pikpak auth status` 后重试即可 —— 根因是 access_token 过期后
// 客户端既不检查 token_expiry 也不会用 refresh_token 续期，只能等外部 CLI 刷新凭据文件。

// credsJSON 构造凭据文件内容（expiry 为 RFC3339；expired=true 时取过去时间）。
func credsJSON(accessToken, refreshToken string, expired bool) string {
	exp := time.Now().Add(12 * time.Hour)
	if expired {
		exp = time.Now().Add(-time.Hour)
	}
	m := map[string]any{
		"client_id":     cliClientID,
		"device_id":     "dev-1234",
		"username":      "keep-me",
		"user_id":       "uid-1",
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_expiry":  exp.Format(time.RFC3339Nano),
		"login_type":    "web",
		"last_updated":  time.Now().Format(time.RFC3339Nano),
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}

// writeCredsFile 写入临时凭据文件，返回路径。
func writeCredsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return path
}

// readCredsFile 读回凭据文件（解析为 map）。
func readCredsFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read creds: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse creds: %v\n%s", err, b)
	}
	return m
}

// fakeTokenServer 起一个 token 端点：返回 new-token/new-refresh，并记录调用次数。
func fakeTokenServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	calls := &atomic.Int32{}
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b) + "|" + r.Header.Get("X-Client-Id") + "|" + r.Header.Get("User-Agent"))
		if status != http.StatusOK {
			http.Error(w, `{"error":"invalid_grant"}`, status)
			return
		}
		writeJSON(w, map[string]any{
			"access_token": "new-token", "refresh_token": "new-refresh",
			"expires_in": 43200, "token_type": "Bearer", "sub": "uid-1",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, calls, &gotBody
}

// TestAPITokenRefresh_ProactiveOnExpiry 凭据已过期 → 用 refresh_token 自动续期后再请求，
// 并把轮换后的 token 落回凭据文件（含未识别字段保留）。
func TestAPITokenRefresh_ProactiveOnExpiry(t *testing.T) {
	t.Parallel()
	tokenSrv, tokenCalls, gotBody := fakeTokenServer(t, http.StatusOK)
	credsPath := writeCredsFile(t, credsJSON("old-token", "old-refresh", true))

	var apiTokens []string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiTokens = append(apiTokens, r.Header.Get("Authorization"))
		if strings.Contains(r.Header.Get("Authorization"), "old-token") {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"files": []any{}})
	}))
	defer apiSrv.Close()

	api := NewAPI(APIConfig{
		Host: apiSrv.URL, UserHost: tokenSrv.URL, CredentialPath: credsPath,
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, nil)
	if _, err := api.List(context.Background(), ""); err != nil {
		t.Fatalf("List 应在自动续期后成功: %v", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token 端点调用 = %d, want 1", got)
	}
	if len(apiTokens) != 1 {
		t.Fatalf("API 请求数 = %d, want 1（过期时先刷新，不该用旧 token 打一次）", len(apiTokens))
	}
	if apiTokens[0] != "Bearer new-token" {
		t.Errorf("API 使用 token = %q, want Bearer new-token", apiTokens[0])
	}
	// 请求体/头必须与官方 CLI 一致（否则 403 permission_denied）
	if body, _ := gotBody.Load().(string); !strings.Contains(body, `"grant_type":"refresh_token"`) ||
		!strings.Contains(body, `"refresh_token":"old-refresh"`) || !strings.Contains(body, cliClientID) {
		t.Errorf("token 请求体/头不符: %s", body)
	}
	// 落盘：新 access/refresh + 未识别字段保留 + 2 空格缩进（CLI 同格式）
	raw, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("read creds: %v", err)
	}
	if !strings.Contains(string(raw), "\n  \"client_id\"") {
		t.Errorf("凭据文件应保持 2 空格缩进格式: %s", raw[:80])
	}
	got := readCredsFile(t, credsPath)
	if got["access_token"] != "new-token" || got["refresh_token"] != "new-refresh" {
		t.Errorf("凭据文件未落盘新 token: access=%v refresh=%v", got["access_token"], got["refresh_token"])
	}
	if got["username"] != "keep-me" || got["device_id"] != "dev-1234" {
		t.Errorf("未识别/无关字段被破坏: %v", got)
	}
	if exp, ok := got["token_expiry"].(string); !ok || exp == "" {
		t.Errorf("token_expiry 未更新: %v", got["token_expiry"])
	} else if ts, perr := time.Parse(time.RFC3339Nano, exp); perr != nil || time.Now().After(ts) {
		t.Errorf("token_expiry 应为未来时间: %v (err=%v)", exp, perr)
	}
}

// TestAPITokenRefresh_On401 本地 token_expiry 未到但服务端已失效（401）→ 续期一次并重试成功。
func TestAPITokenRefresh_On401(t *testing.T) {
	t.Parallel()
	tokenSrv, tokenCalls, _ := fakeTokenServer(t, http.StatusOK)
	credsPath := writeCredsFile(t, credsJSON("stale-token", "old-refresh", false))

	var apiCalls atomic.Int32
	var lastAuth atomic.Value
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		if strings.Contains(r.Header.Get("Authorization"), "stale-token") {
			http.Error(w, `{"error":"invalid_token","error_description":"expired"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"files": []any{}})
	}))
	defer apiSrv.Close()

	api := NewAPI(APIConfig{
		Host: apiSrv.URL, UserHost: tokenSrv.URL, CredentialPath: credsPath,
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, nil)
	if _, err := api.List(context.Background(), ""); err != nil {
		t.Fatalf("401 后应自动续期并重试成功: %v", err)
	}
	if got := apiCalls.Load(); got != 2 {
		t.Errorf("API 调用 = %d, want 2（401 + 续期后重试）", got)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token 端点调用 = %d, want 1", got)
	}
	if got, _ := lastAuth.Load().(string); got != "Bearer new-token" {
		t.Errorf("重试使用 token = %q, want Bearer new-token", got)
	}
}

// TestAPITokenRefresh_NoRefreshToken 无 refresh_token（老凭据/仅登录态）→ 明确报未登录
// 并给出可执行提示（不再只给一句 HTTP 401）。
func TestAPITokenRefresh_NoRefreshToken(t *testing.T) {
	t.Parallel()
	tokenSrv, _, _ := fakeTokenServer(t, http.StatusOK)
	credsPath := writeCredsFile(t, credsJSON("old-token", "", true))
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("无 refresh_token 且已过期时不应发起 API 请求")
	}))
	defer apiSrv.Close()

	api := NewAPI(APIConfig{
		Host: apiSrv.URL, UserHost: tokenSrv.URL, CredentialPath: credsPath,
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, nil)
	_, err := api.List(context.Background(), "")
	if err == nil {
		t.Fatal("err = nil, want ErrNotLoggedIn")
	}
	if !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v, want 包装 ErrNotLoggedIn", err)
	}
	if !strings.Contains(err.Error(), "auth status") && !strings.Contains(err.Error(), "登录") {
		t.Errorf("错误应给出续期/登录提示: %v", err)
	}
}

// TestAPITokenRefresh_HTTPFailFallsBackToCLI HTTP 续期失败（如服务端 5xx/凭据轮换禁用）→
// 回落官方 CLI（`pikpak auth status` 会刷新凭据文件），成功则继续用文件里的新 token。
func TestAPITokenRefresh_HTTPFailFallsBackToCLI(t *testing.T) {
	t.Parallel()
	tokenSrv, tokenCalls, _ := fakeTokenServer(t, http.StatusInternalServerError)
	credsPath := writeCredsFile(t, credsJSON("old-token", "old-refresh", true))

	// 假 CLI：auth status 时把凭据文件改写成新 token（模拟官方 CLI 的续期行为）。
	bin := buildFakeRefreshCLI(t)
	var cliCalls atomic.Int32
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cliCalls.Add(1)
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = append(os.Environ(), "PIKPAK_TEST_CREDS="+credsPath)
		return cmd
	}
	cli, err := NewCli(CliConfig{BinaryPath: bin, AutoInstall: false, CommandFactory: factory, Logger: apiTestLogger()})
	if err != nil {
		t.Fatalf("NewCli: %v", err)
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "old-token") {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"files": []any{}})
	}))
	defer apiSrv.Close()

	api := NewAPI(APIConfig{
		Host: apiSrv.URL, UserHost: tokenSrv.URL, CredentialPath: credsPath,
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, cli)
	if _, err := api.List(context.Background(), ""); err != nil {
		t.Fatalf("HTTP 续期失败后应回落 CLI 并成功: %v", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token 端点调用 = %d, want 1（HTTP 先试一次）", got)
	}
	if got := cliCalls.Load(); got == 0 {
		t.Error("HTTP 续期失败应回落官方 CLI（pikpak auth status）")
	}
	if got := readCredsFile(t, credsPath)["access_token"]; got != "cli-refreshed-token" {
		t.Errorf("CLI 续期后的 token 未被采用: %v", got)
	}
}

// TestAPITokenRefresh_Throttle 并发 401 只触发一次续期（避免令牌轮换风暴）。
func TestAPITokenRefresh_Throttle(t *testing.T) {
	t.Parallel()
	tokenSrv, tokenCalls, _ := fakeTokenServer(t, http.StatusOK)
	credsPath := writeCredsFile(t, credsJSON("stale-token", "old-refresh", false))
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "stale-token") {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"files": []any{}})
	}))
	defer apiSrv.Close()

	api := NewAPI(APIConfig{
		Host: apiSrv.URL, UserHost: tokenSrv.URL, CredentialPath: credsPath,
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, _ = api.List(context.Background(), "")
		})
	}
	wg.Wait()
	if got := tokenCalls.Load(); got > 1 {
		t.Errorf("并发续期次数 = %d, want ≤1（节流）", got)
	}
}

// buildFakeRefreshCLI 编译一个假 CLI：`auth status` 会把凭据文件改写成新 token 并输出登录态。
func buildFakeRefreshCLI(t *testing.T) string {
	t.Helper()
	src := `package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		path := os.Getenv("PIKPAK_TEST_CREDS")
		if path != "" {
			b, err := os.ReadFile(path)
			if err == nil {
				var m map[string]any
				if json.Unmarshal(b, &m) == nil {
					m["access_token"] = "cli-refreshed-token"
					m["refresh_token"] = "cli-refreshed-refresh"
					m["token_expiry"] = time.Now().Add(12 * time.Hour).Format(time.RFC3339Nano)
					if out, merr := json.MarshalIndent(m, "", "  "); merr == nil {
						_ = os.WriteFile(filepath.Clean(path), out, 0o600)
					}
				}
			}
		}
		os.Stdout.WriteString("{\"logged_in\":true}")
		os.Exit(0)
	}
	os.Exit(1)
}
`
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "pikpak-fake")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, srcPath).CombinedOutput(); err != nil {
		t.Fatalf("build fake cli: %v: %s", err, out)
	}
	return bin
}

// TestCredentials_PreservesKeyOrderAndFormat 凭据文件写回必须保持 CLI 的键顺序与 2 空格缩进
// （json.Decoder.Token() 会同时返回键与值——此用例锁定「只有键被当作顺序项」）。
func TestCredentials_PreservesKeyOrderAndFormat(t *testing.T) {
	t.Parallel()
	// 手写 JSON（Go map 会按字母序序列化，无法验证顺序保留）
	content := "{\n" +
		"  \"client_id\": \"" + cliClientID + "\",\n" +
		"  \"device_id\": \"dev-1234\",\n" +
		"  \"username\": \"keep-me\",\n" +
		"  \"user_id\": \"uid-1\",\n" +
		"  \"access_token\": \"old-token\",\n" +
		"  \"refresh_token\": \"old-refresh\",\n" +
		"  \"token_expiry\": \"" + time.Now().Add(-time.Hour).Format(time.RFC3339Nano) + "\",\n" +
		"  \"login_type\": \"web\",\n" +
		"  \"last_updated\": \"" + time.Now().Format(time.RFC3339Nano) + "\"\n" +
		"}\n"
	path := writeCredsFile(t, content)
	cred, err := readCredentials(path)
	if err != nil {
		t.Fatalf("readCredentials: %v", err)
	}
	want := []string{"client_id", "device_id", "username", "user_id", "access_token", "refresh_token", "token_expiry", "login_type", "last_updated"}
	if got := cred.orderedKeys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("键顺序 = %v, want %v", got, want)
	}
	if perr := cred.persist(path, &tokenResponse{AccessToken: "n1", RefreshToken: "r1", ExpiresIn: 3600}); perr != nil {
		t.Fatalf("persist: %v", perr)
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	idx := func(k string) int { return strings.Index(string(raw), `"`+k+`"`) }
	if idx("client_id") > idx("access_token") || idx("access_token") > idx("last_updated") {
		t.Errorf("键顺序未保持: %s", strings.SplitN(string(raw), "}", 2)[0])
	}
	if !strings.Contains(string(raw), "\n  \"client_id\":") {
		t.Errorf("缩进应为 2 空格: %q", string(raw[:60]))
	}
	// 写回后仍可被解析且新 token 生效
	got := readCredsFile(t, path)
	if got["access_token"] != "n1" || got["refresh_token"] != "r1" || got["login_type"] != "web" {
		t.Errorf("写回内容不正确: %v", got)
	}
}

// TestTopLevelKeys_NestedValue 嵌套值不污染键顺序。
func TestTopLevelKeys_NestedValue(t *testing.T) {
	t.Parallel()
	keys := topLevelKeys([]byte(`{"a":1,"b":{"x":1,"y":"z"},"c":[1,2],"d":"v"}`))
	if strings.Join(keys, ",") != "a,b,c,d" {
		t.Errorf("keys = %v, want [a b c d]", keys)
	}
}
