// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// ---- trust renew ----

// testTrustWrapContext 复算 wrap context（对齐 pkg/client 的 CredentialWrapContextPrefix 契约）。
func testTrustWrapContext(ak string) string {
	mesh := accesskey.ParseMesh(ak)
	if mesh == "" {
		return client.CredentialWrapContextPrefix
	}
	return client.CredentialWrapContextPrefix + "#" + mesh
}

// testTrustEnvelope 用 wrapSK 包裹 secret 生成信封（mock 服务端用）。
func testTrustEnvelope(t *testing.T, wrapSK []byte, ak string, secret []byte) *accesskey.WrappedSecret {
	t.Helper()
	wk, err := accesskey.DeriveWrapKey(wrapSK, ak, testTrustWrapContext(ak))
	if err != nil {
		t.Fatalf("DeriveWrapKey: %v", err)
	}
	env, err := accesskey.EncryptSecret(ak, secret, wk)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	return env
}

// trustRenewHandler 构造 renew mock handler：校验签名（真实验签路径），返回带信封的响应。
func trustRenewHandler(t *testing.T, ak, skHex string, env *accesskey.WrappedSecret) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		hdr, err := sproxysig.ParseHeader(r.Header.Get("Authorization"))
		if err != nil {
			t.Errorf("ParseHeader: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := sproxysig.Verify(skHex, hdr, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, time.Now(), 0, 0, nil); err != nil {
			t.Errorf("Verify: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak":             ak,
			"sk_id":          "sk-newentry1234",
			"kind":           "secret_wrap",
			"wrap_key_ak":    ak,
			"expires_at":     time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339),
			"wrapped_secret": env,
		})
	}
}

func TestTrustRenew_UpdatesConfig(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	oldSK := make([]byte, 32)
	_, _ = rand.Read(oldSK)
	oldSKHex := hex.EncodeToString(oldSK)
	newSK := make([]byte, 32)
	_, _ = rand.Read(newSK)
	env := testTrustEnvelope(t, oldSK, ak, newSK)

	srv := httptest.NewServer(http.HandlerFunc(trustRenewHandler(t, ak, oldSKHex, env)))
	defer srv.Close()

	// 隔离配置文件（不触碰真实用户配置）。
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")
	cfg := client.DefaultConfig()
	cfg.ServerURL = srv.URL
	cfg.AccessKey = ak
	cfg.AccessKeySecret = oldSKHex
	if err := client.SaveConfig(cfg, cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, oldSKHex))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard, In: strings.NewReader("\n")}
	cmd := NewCmdTrust(factory, ios, &testConfigProvider{cfg: cfg}, &cfgPath)
	cmd.SetArgs([]string{"renew"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust renew failed: %v", err)
	}

	if !strings.Contains(buf.String(), "SK 已轮换") {
		t.Errorf("expected 'SK 已轮换' in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "sk_id=sk-newentry1234") {
		t.Errorf("expected sk_id in output, got: %s", buf.String())
	}

	// 配置文件必须已持久化新 SK 与 sk_id（回填）。
	reloaded, err := client.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.AccessKeySecret != hex.EncodeToString(newSK) {
		t.Errorf("access_key_secret 未回填: got %q want %q", reloaded.AccessKeySecret, hex.EncodeToString(newSK))
	}
	if reloaded.AccessKeyID != "sk-newentry1234" {
		t.Errorf("access_key_id 未回填: got %q", reloaded.AccessKeyID)
	}
}

func TestTrustRenew_NoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	svc := client.NewFileClient(srv.URL)
	factory := clientfactory.NewMock(svc, nil)
	cfgPath := filepath.Join(t.TempDir(), "sclient.yaml")

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: &buf}, &testConfigProvider{cfg: client.DefaultConfig()}, &cfgPath)
	cmd.SetArgs([]string{"renew"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no credentials")
	}
	if !strings.Contains(err.Error(), "access_key_secret") {
		t.Errorf("error should mention access_key_secret, got: %v", err)
	}
}

// ---- trust sk ----

func TestTrustSKList_ShowsOnlyDecryptable(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	mySK := make([]byte, 32)
	_, _ = rand.Read(mySK)
	mySKHex := hex.EncodeToString(mySK)
	otherSK := make([]byte, 32)
	_, _ = rand.Read(otherSK)

	envMine := testTrustEnvelope(t, mySK, ak, mySK)
	envOther := testTrustEnvelope(t, otherSK, ak, otherSK)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/credentials/"+ak+"/sk" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": ak,
			"sk": []map[string]any{
				{"sk_id": "sk-mineaaaa", "created": time.Now().Add(-time.Hour).Format(time.RFC3339),
					"expires": time.Now().Add(time.Hour).Format(time.RFC3339), "status": "active",
					"meta_type": "renew", "wrapped_secret": envMine},
				{"sk_id": "sk-otheraa", "created": time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
					"expires": time.Now().Add(2 * time.Hour).Format(time.RFC3339), "status": "active",
					"meta_type": "initial", "wrapped_secret": envOther},
			},
			"total": 2, "admin": false,
		})
	}))
	defer srv.Close()

	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, mySKHex))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"sk", "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust sk list failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sk-mineaaaa") {
		t.Errorf("expected own entry sk-mineaaaa in output, got: %s", out)
	}
	if !strings.Contains(out, "sk-otheraa") {
		t.Errorf("expected other entry sk-otheraa in output, got: %s", out)
	}
	// 自己条目能解开 → <decrypted>；他人条目 masked → <encrypted>。
	if !strings.Contains(out, "<decrypted>") {
		t.Errorf("expected own entry marked <decrypted>, got: %s", out)
	}
	if !strings.Contains(out, "<encrypted>") {
		t.Errorf("expected other entry marked <encrypted>, got: %s", out)
	}
}

func TestTrustSKDelete(t *testing.T) {
	const (
		ak   = "sk-0123456789abcdef"
		skID = "sk-aaaaaaaaaaaa"
	)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"sk", "delete", skID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust sk delete failed: %v", err)
	}
	wantPath := "/api/credentials/" + ak + "/sk/" + skID
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(buf.String(), "SK 已删除") {
		t.Errorf("expected 'SK 已删除' in output, got: %s", buf.String())
	}
}

func TestTrustSKExpire(t *testing.T) {
	const (
		ak   = "sk-0123456789abcdef"
		skID = "sk-aaaaaaaaaaaa"
	)
	until := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var gotBody struct {
		Until string `json:"until"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "until": gotBody.Until})
	}))
	defer srv.Close()

	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"sk", "expire", skID, "--until", until.Format(time.RFC3339)})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust sk expire failed: %v", err)
	}
	if gotBody.Until != until.Format(time.RFC3339) {
		t.Errorf("until = %q, want %q", gotBody.Until, until.Format(time.RFC3339))
	}
	if !strings.Contains(buf.String(), skID) {
		t.Errorf("expected sk_id in output, got: %s", buf.String())
	}
}

func TestTrustSKExpire_BadUntil(t *testing.T) {
	const (
		ak   = "sk-0123456789abcdef"
		skID = "sk-aaaaaaaaaaaa"
	)
	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithAccessKey(ak, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: &buf}, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"sk", "expire", skID, "--until", "not-a-time"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for invalid --until")
	} else if !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("error should mention RFC3339, got: %v", err)
	}
}

// ---- trust ak ----

func TestTrustAKDelete_ConfirmsName(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		defer r.Body.Close()
		var body struct {
			Confirm string `json:"confirm"`
			Force   bool   `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Confirm != ak {
			t.Errorf("confirm = %q, want %q", body.Confirm, ak)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "ak": ak})
	}))
	defer srv.Close()

	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	// 输入正确的 AK 名 → 删除请求发出。
	ios := cli.IOStreams{In: strings.NewReader(ak + "\n"), Out: &strings.Builder{}, ErrOut: io.Discard}
	cmd := NewCmdTrust(factory, ios, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"ak", "delete", ak})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust ak delete failed: %v", err)
	}
	if gotPath != "/api/credentials/"+ak {
		t.Errorf("path = %q, want %q", gotPath, "/api/credentials/"+ak)
	}
}

func TestTrustAKDelete_CancelOnMismatch(t *testing.T) {
	const ak = "sk-0123456789abcdef"
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	ios := cli.IOStreams{In: strings.NewReader("wrong-name\n"), Out: &buf, ErrOut: io.Discard}
	cmd := NewCmdTrust(factory, ios, &testConfigProvider{cfg: &client.Config{AccessKey: ak}}, nil)
	cmd.SetArgs([]string{"ak", "delete", ak})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust ak delete (mismatch) should not error: %v", err)
	}
	if called {
		t.Error("server should not be called when confirmation mismatches")
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Errorf("expected '已取消' in output, got: %s", buf.String())
	}
}

func TestTrustAKAdd_NoAKArg_GeneratesPair(t *testing.T) {
	var gotBody struct {
		AK     string `json:"ak"`
		Owner  string `json:"owner"`
		Secret string `json:"secret"`
	}
	sk := make([]byte, 32)
	_, _ = rand.Read(sk)
	const adminAK = "sk-admin-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ak": gotBody.AK, "sk_id": "sk-created1", "secret": gotBody.Secret})
	}))
	defer srv.Close()

	svc := client.NewFileClient(srv.URL, client.WithAccessKey(adminAK, hex.EncodeToString(sk)))
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdTrust(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &testConfigProvider{cfg: &client.Config{AccessKey: adminAK}}, nil)
	cmd.SetArgs([]string{"ak", "add", "--owner", "tenant-x"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust ak add failed: %v", err)
	}
	if !strings.HasPrefix(gotBody.AK, "sk-") {
		t.Errorf("generated AK should start with sk-, got %q", gotBody.AK)
	}
	if gotBody.Owner != "tenant-x" {
		t.Errorf("owner = %q, want tenant-x", gotBody.Owner)
	}
	if len(gotBody.Secret) != 64 {
		t.Errorf("generated secret should be 64 hex, got %d chars", len(gotBody.Secret))
	}
	if !strings.Contains(buf.String(), "sk_id=sk-created1") {
		t.Errorf("expected created sk_id in output, got: %s", buf.String())
	}
}

// ---- access-key deprecated ----

// TestAccessKey_DeprecatedAlias 验证 access-key 命令已标 deprecated（提示指向 trust），
// 但其上**不得**挂 `Aliases: ["trust"]`——否则 cobra 先到先得匹配会让独立命令树
// trust 全部子命令（renew/sk/ak）被 access-key 遮蔽而不可达（Critical C1 回归）。
func TestAccessKey_DeprecatedAlias(t *testing.T) {
	var out, errOut strings.Builder
	cmd := NewCmdAccessKey(cli.IOStreams{Out: &out, ErrOut: &errOut})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"create"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("access-key create failed: %v", err)
	}
	if !strings.Contains(out.String(), "AccessKey:") || !strings.Contains(out.String(), "AccessKeySecret:") {
		t.Errorf("expected AK/SK lines still printed, got: %s", out.String())
	}
	// deprecated 提示（cobra 仅对不触达隐式子命令的父命令打印）应指向 trust。
	if !strings.Contains(cmd.Deprecated, "trust") {
		t.Errorf("expected deprecated hint mentioning 'trust', got: %q", cmd.Deprecated)
	}
	if len(cmd.Aliases) > 0 {
		t.Errorf("access-key 必须无 alias（C1 遮蔽修复）：got %v", cmd.Aliases)
	}
}

// TestTrustCommandTreeReachable 用真实 cobra 执行路由验证完整 trust 命令树可达：
// `root trust <子命令>` 必须解析到独立 trust 命令（而非被 access-key 遮蔽——C1 回归
//
//	guards）。cobra findNext 配 Name/Alias 先到先得，access-key 若挂 Aliases:["trust"]
//	会把 trust 树吃掉，此测试会失败。
func TestTrustCommandTreeReachable(t *testing.T) {
	cmds := map[string]string{
		"sk list":     "SK_ID", // 列表头
		"sk delete x": "SK 已删除",
		"sk expire x": "设 SK 过期",
		"ak list":     "AccessKey",
	}
	for full, want := range cmds {
		full := full
		want := want
		t.Run(full, func(t *testing.T) {
			svc := client.NewFileClient("http://127.0.0.1:1") // 直连占位（无真实 RPC）
			factory := clientfactory.NewMock(svc, nil)
			var out, errOut strings.Builder
			ios := cli.IOStreams{Out: &out, ErrOut: &errOut, In: strings.NewReader("wrong\n")}
			cfgSvc := &testConfigProvider{cfg: &client.Config{AccessKey: "sk-test-0000000000000000", AccessKeySecret: strings.Repeat("11", 32)}}
			cmd := NewCmdTrust(factory, ios, cfgSvc, new(string))
			cmd.SetArgs(strings.Fields(full))
			runErr := cmd.Execute()
			// 关键断言：命令树可达（解析成功进入 RunE）——access-key 若挂 Aliases:["trust"]
			// 会把 trust 树遮蔽，cobra 反报 unknown command（runErr 为 cobra 未知子命令错误）。
			// 此处相反：解析成功、进入 RunE；RPC 本身（127.0.0.1:1 拒绝）或字段校验的错误
			// 不影响「树可达」判定——只要求不是 cobra 的 unknown-command 关口被 alias 挡住。
			if runErr != nil {
				msg := runErr.Error()
				if strings.Contains(msg, "unknown command") || strings.Contains(msg, "unrecognized") {
					t.Fatalf("%s: 命令树被遮蔽（runErr=%q）——C1 回归", full, msg)
				}
				// 其余错误 = 命令已解析并进入 RunE（校验/RPC 层），可达性 OK。
				return
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s: 期望输出含 %q, got: %s", full, want, out.String())
			}
		})
	}

	// ak delete 走交互确认路径：输入不符 → 打印取消（命令树可达 + 交互生效）。
	t.Run("ak delete x", func(t *testing.T) {
		svc := client.NewFileClient("http://127.0.0.1:1")
		factory := clientfactory.NewMock(svc, nil)
		var out strings.Builder
		ios := cli.IOStreams{Out: &out, ErrOut: io.Discard, In: strings.NewReader("wrong\n")}
		cmd := NewCmdTrust(factory, ios, nil, new(string))
		cmd.SetArgs([]string{"ak", "delete", "sk-test-delete00000000"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ak delete: 命令树可达但执行错误=%v", err)
		}
		if !strings.Contains(out.String(), "已取消") {
			t.Errorf("ak delete: 期望取消输出, got %q", out.String())
		}
	})
}

// ---- config file isolation ----

func TestTrustRenew_ConfigIsolation(t *testing.T) {
	// 确保 trust renew 只写目标配置文件，不触碰真实用户配置目录。
	const ak = "sk-0123456789abcdef"
	oldSK := make([]byte, 32)
	_, _ = rand.Read(oldSK)
	oldSKHex := hex.EncodeToString(oldSK)
	newSK := make([]byte, 32)
	_, _ = rand.Read(newSK)
	env := testTrustEnvelope(t, oldSK, ak, newSK)

	srv := httptest.NewServer(http.HandlerFunc(trustRenewHandler(t, ak, oldSKHex, env)))
	defer srv.Close()

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")
	cfg := client.DefaultConfig()
	cfg.ServerURL = srv.URL
	cfg.AccessKey = ak
	cfg.AccessKeySecret = oldSKHex
	if err := client.SaveConfig(cfg, cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	svc := client.NewFileClient(srv.URL, client.WithAccessKey(ak, oldSKHex))
	factory := clientfactory.NewMock(svc, nil)

	cmd := NewCmdTrust(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &testConfigProvider{cfg: cfg}, &cfgPath)
	cmd.SetArgs([]string{"renew"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust renew failed: %v", err)
	}

	// 配置目录下只有我们创建的 sclient.yaml；没有其它文件被写入。
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "sclient.yaml" {
		t.Fatalf("expected only sclient.yaml in config dir, got %v", entries)
	}
}
