// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 真实 Vault 集成测试（L2 契约 / L3 行为），无 build tag。
//
// 策略（用户 2026-09-07 调整）：运行时检测 VAULT_ADDR（默认 http://127.0.0.1:8200）
// 可达性——不可达 t.Skip（本地无 Vault / CI 非 ubuntu-vault job 自动跳过，不失败）；
// docker 容器（scripts/test-vault.sh）/ CI ubuntu services.vault 起真实 Vault 时自动实跑。
//
// 测试前置：transit engine + 测试 key 由 ensureTransitKey 经 Vault HTTP API 自动建（幂等，
// 重复跑不炸）。约定容器 root token 恒 "root"（scripts/test-vault.sh 与 CI service 均用
// VAULT_DEV_ROOT_TOKEN_ID=root）。
//
// L3-seal 不做自动化（真实 seal 会锁 dev 实例、需 unseal key 恢复，环境破坏风险）——
// sealed 错误分类已由 L1 mock（vault_storer_test.go 503 sealed 用例）覆盖。默认跳过。
package accesskey_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// vaultEnv 返回 (VAULT_ADDR, VAULT_TOKEN)，缺省 http://127.0.0.1:8200 / root。
func vaultEnv() (addr, token string) {
	addr = os.Getenv("VAULT_ADDR")
	if addr == "" {
		addr = "http://127.0.0.1:8200"
	}
	token = os.Getenv("VAULT_TOKEN")
	if token == "" {
		token = "root"
	}
	return addr, token
}

// vaultClient 是集成测试共用的 Vault HTTP client（带 10s 超时，与 vault_storer.go 默认
// 一致——防 Vault 卡死时挂到 go test 全局 timeout）。
var vaultClient = &http.Client{Timeout: 10 * time.Second}

// requireVault 探测 Vault 可达性：健康检查 GET {addr}/v1/sys/health（短超时）。
// 语义（用户 2026-09-07 调整 + 审查 M8）：**200 视为就绪**；网络不可达 /
// 429（standby）/501（未初始化）/503（sealed）等一律视为不可用 → t.Skip（集成测试需
// unsealed dev 实例）。返回 (addr, token) 供用例装配 VaultTransitStorer。
func requireVault(t *testing.T) (string, string) {
	t.Helper()
	addr, token := vaultEnv()
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, addr+"/v1/sys/health", nil)
	if err != nil {
		t.Fatalf("构造健康检查请求: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("Vault 不可达（%v）——跳过 L2/L3 集成测试（docker/CI vault service 存在时自动实跑）", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Vault 状态码 %d（仅 200=dev/active 视为就绪；429/501/503 视为不可用）——跳过 L2/L3 集成测试", resp.StatusCode)
	}
	return addr, token
}

// vaultRequest 向 Vault HTTP API 发 method 请求（X-Vault-Token；POST 带 JSON body），
// 返回状态码与 body。带超时 client（M5）。
func vaultRequest(t *testing.T, method, addr, token, path, body string) (int, string) {
	t.Helper()
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, addr+path, reqBody)
	if err != nil {
		t.Fatalf("构造 %s %s 请求: %v", method, path, err)
	}
	req.Header.Set("X-Vault-Token", token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := vaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应 %s: %v", path, err)
	}
	return resp.StatusCode, string(data)
}

// vaultPost 向 Vault HTTP API 发 POST（X-Vault-Token + JSON body），返回状态码与 body。
func vaultPost(t *testing.T, addr, token, path, body string) (int, string) {
	t.Helper()
	return vaultRequest(t, http.MethodPost, addr, token, path, body)
}

// vaultGet 向 Vault HTTP API 发 GET（X-Vault-Token），返回状态码与 body。
func vaultGet(t *testing.T, addr, token, path string) (int, string) {
	t.Helper()
	return vaultRequest(t, http.MethodGet, addr, token, path, "")
}

// ensureTransitKey 幂等装配 transit engine 挂载 + 命名加密 key（重复跑不炸）。
// key 以 derived=true 创建：Vault Transit 的 context 参数仅对 derived key 生效（用作派生
// 密钥 + AAD）——非 derived key 会忽略 context，AAD context 绑定（AD-4）不成立。生产装配
// 同理：operator 建 transit key 需带 derived=true（见 config.example.yaml 冒烟注释）。
func ensureTransitKey(t *testing.T, addr, token, name string) {
	t.Helper()
	// transit engine 挂载：已存在（400 "already in use"）忽略。
	if code, body := vaultPost(t, addr, token, "/v1/sys/mounts/transit", `{"type":"transit"}`); !httpOK(code) {
		if code != http.StatusBadRequest || !strings.Contains(body, "already in use") {
			t.Fatalf("enable transit mount: HTTP %d: %s", code, body)
		}
	}
	// 命名 key（derived=true 使 context 生效）：已存在（204/400 exists）忽略。
	if code, body := vaultPost(t, addr, token, "/v1/transit/keys/"+name, `{"derived":true}`); !httpOK(code) {
		if code != http.StatusBadRequest || !strings.Contains(body, "exists") {
			t.Fatalf("create transit key %s: HTTP %d: %s", name, code, body)
		}
	}
}

// httpOK 判断状态码是否为 Vault 幂等成功集合（200 OK / 204 No Content）。
func httpOK(code int) bool {
	return code == http.StatusOK || code == http.StatusNoContent
}

// newVaultStorer 构造绑定指定 Vault 的 VaultTransitStorer（CacheTTL=0 关缓存，隔离用例）。
func newVaultStorer(t *testing.T, addr, token, key, aad string) *accesskey.VaultTransitStorer {
	t.Helper()
	s, err := accesskey.NewVaultTransitStorer(accesskey.VaultOptions{
		Addr:    addr,
		Mount:   "transit",
		KeyName: key,
		Token:   token,
		AADPath: aad,
	})
	if err != nil {
		t.Fatalf("NewVaultTransitStorer: %v", err)
	}
	return s
}

// TestVault_L2_EncryptDecryptRoundtrip 验证真实 Vault Transit 契约：Encrypt 返回密文含
// vault:v1: 前缀 → Decrypt 还原原文（L2）。
func TestVault_L2_EncryptDecryptRoundtrip(t *testing.T) {
	addr, token := requireVault(t)
	const key = "sproxy-it-roundtrip"
	ensureTransitKey(t, addr, token, key)

	s := newVaultStorer(t, addr, token, key, "it/roundtrip.json")
	secret := []byte(`{"version":1,"keys":[{"ak":"ak-it-roundtrip"}]}`)
	ct, err := s.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.HasPrefix(ct, []byte("vault:v1:")) {
		t.Fatalf("密文应含 vault:v1: 前缀, got %q", ct)
	}
	pt, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, secret) {
		t.Fatalf("往返应还原原文, got %q", pt)
	}
}

// TestVault_L2_AADContextMismatch 验证 AAD context 语义：同 AADPath Encrypt/Decrypt 成功；
// 异 AADPath Decrypt 失败（Transit context 作为 AEAD associated data 不匹配）（L2）。
func TestVault_L2_AADContextMismatch(t *testing.T) {
	addr, token := requireVault(t)
	const key = "sproxy-it-aad"
	ensureTransitKey(t, addr, token, key)

	sEnc := newVaultStorer(t, addr, token, key, "it/aad.json")
	const secret = "secret-with-aad"
	ct, err := sEnc.Encrypt([]byte(secret))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// 同 AAD → 成功。
	sDecSame := newVaultStorer(t, addr, token, key, "it/aad.json")
	pt, err := sDecSame.Decrypt(ct)
	if err != nil {
		t.Fatalf("同 AAD Decrypt 应成功: %v", err)
	}
	if string(pt) != secret {
		t.Fatalf("同 AAD 还原不一致, got %q", pt)
	}
	// 异 AAD → 失败（context 不匹配）。
	sDecDiff := newVaultStorer(t, addr, token, key, "it/other.json")
	if _, err := sDecDiff.Decrypt(ct); err == nil {
		t.Fatal("异 AAD Decrypt 应失败（Transit context 不匹配）")
	}
}

// TestVault_L3_KeyRotation 验证 key 轮换：rotate 后旧密文仍可解（Vault 密文自带版本，
// decrypt 自解最新/指定版本）（L3）。
func TestVault_L3_KeyRotation(t *testing.T) {
	addr, token := requireVault(t)
	const key = "sproxy-it-rotate"
	ensureTransitKey(t, addr, token, key)

	s := newVaultStorer(t, addr, token, key, "it/rotate.json")
	old := []byte("pre-rotation-secret")
	ctOld, err := s.Encrypt(old)
	if err != nil {
		t.Fatalf("Encrypt(rotate 前): %v", err)
	}
	versionBefore := latestKeyVersion(t, addr, token, key)

	// rotate key（幂等）。
	if code, body := vaultPost(t, addr, token, "/v1/transit/keys/"+key+"/rotate", `{}`); !httpOK(code) {
		t.Fatalf("rotate key %s: HTTP %d: %s", key, code, body)
	}

	// 版本前向以 Vault latest_version 递增为准（M2：密文不等不能证版本前向——每次 Encrypt
	// 随机 nonce 同 key 两次密文必不同，rotate 未生效也成立）。
	if got := latestKeyVersion(t, addr, token, key); got <= versionBefore {
		t.Fatalf("rotate 后 latest_version 应递增（before=%d, after=%d）", versionBefore, got)
	}
	// 旧密文仍可解（Vault 按版本自解）。
	pt, err := s.Decrypt(ctOld)
	if err != nil {
		t.Fatalf("rotate 后旧密文应仍可解: %v", err)
	}
	if !bytes.Equal(pt, old) {
		t.Fatalf("旧密文还原不一致, got %q", pt)
	}
}

// latestKeyVersion 读取 transit key 的 latest_version（GET /v1/transit/keys/<name>）。
func latestKeyVersion(t *testing.T, addr, token, name string) int {
	t.Helper()
	code, body := vaultGet(t, addr, token, "/v1/transit/keys/"+name)
	if code != http.StatusOK {
		t.Fatalf("read transit key %s: HTTP %d: %s", name, code, body)
	}
	var out struct {
		Data struct {
			LatestVersion int `json:"latest_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析 key %s 响应: %v", name, err)
	}
	return out.Data.LatestVersion
}

// TestVault_L3_PermissionDeniedOnDecrypt 验证受限 token：仅 encrypt 无 decrypt 的 policy
// token → Decrypt 返回 permission denied（L3，Vault 侧授权 enforcement）。
func TestVault_L3_PermissionDeniedOnDecrypt(t *testing.T) {
	addr, token := requireVault(t)
	const key = "sproxy-it-perm"
	ensureTransitKey(t, addr, token, key)

	// ACL policy：仅允许 encrypt（create/update），decrypt 显式 deny。
	policyName := "sproxy-it-perm-policy"
	policy := fmt.Sprintf(
		`path "transit/encrypt/%s" { capabilities = ["create", "update"] }
path "transit/decrypt/%s" { capabilities = ["deny"] }`,
		key, key)
	policyBody, _ := json.Marshal(map[string]string{"policy": policy})
	if code, body := vaultPost(t, addr, token, "/v1/sys/policies/acl/"+policyName, string(policyBody)); !httpOK(code) {
		t.Fatalf("create ACL policy: HTTP %d: %s", code, body)
	}

	// 用受限 policy 建子 token。
	tokBody, _ := json.Marshal(map[string]any{"policies": []string{policyName}, "ttl": "1h"})
	code, body := vaultPost(t, addr, token, "/v1/auth/token/create", string(tokBody))
	if code != http.StatusOK {
		t.Fatalf("create token: HTTP %d: %s", code, body)
	}
	var created struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("解析 token create 响应: %v", err)
	}
	if created.Auth.ClientToken == "" {
		t.Fatalf("token create 响应缺 client_token: %s", body)
	}
	child := created.Auth.ClientToken

	// encrypt 应成功（policy 允许）；decrypt 应 permission denied。
	sEnc := newVaultStorer(t, addr, child, key, "it/perm.json")
	ct, err := sEnc.Encrypt([]byte("limited-token-data"))
	if err != nil {
		t.Fatalf("受限 token Encrypt 应成功（policy 允许 encrypt）: %v", err)
	}
	sDec := newVaultStorer(t, addr, child, key, "it/perm.json")
	if _, err := sDec.Decrypt(ct); err == nil {
		t.Fatal("受限 token Decrypt 应 permission denied")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("受限 token Decrypt 错误应含 permission denied, got %v", err)
	}
}
