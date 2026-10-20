// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_credential_scenarios_test.go 补设计 §9.1「认证面变更」审计场景（凭据类，P3 缺口）：
// `credential_ak_list` / `credential_ak_add` / `credential_sk_list` / `credential_renew`
// —— 凭据是**访问控制根**，任何签发/列举/轮换都必须可追溯（谁、对哪个 AK）。
//
// 既有用例已覆盖 `credential_register(_denied)` / `_ak_delete` / `_sk_delete` / `_persist_error`
// （按常量断言）；本文件补齐剩余可直驱的四个动作。`credential_login`（需 TOTP）与
// `credential_sk_expire`（需构造过期窗口）记档为后续。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAuditScenario_CredentialOps 列举/新增 AK、列举 SK、本人 SK 轮换 ⇒ 四类审计各 ≥1 条。
func TestAuditScenario_CredentialOps(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	const (
		newAK = "audit-new-ak"
		// 新增用**另一个** AK（harness 已预置 newAK，重复新增会 400 冲突）。
		addAK  = "audit-added-ak"
		newSK  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		bodyAK = `{"ak":"` + addAK + `","secret":"` + newSK + `","role":"user"}`
	)
	url, _, _ := newCredentialsTestServer(t, testAccessKey, testAccessSecret, newAK, newSK, buf, nil)

	post := func(path, body, ak, sk string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.Header.Set("Content-Type", contentTypeJSON)
		signBodyRequest(req, ak, sk, []byte(body)) // bodyValidator 要求对 body 哈希签名
		resp, derr := testHTTPClient(t).Do(req)
		if derr != nil {
			t.Fatalf("POST %s: %v", path, derr)
		}
		out := new(strings.Builder)
		_, _ = io.Copy(out, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, out.String()
	}

	// ① admin 列举 AK 列表。
	if st, _ := signedGet(t, url+"/api/credentials", testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("ak 列表 status = %d, want 200", st)
	}
	// ② admin 新增 AK。
	stAdd, bodyAdd := post("/api/credentials", bodyAK, testAccessKey, testAccessSecret)
	if stAdd != http.StatusOK && stAdd != http.StatusCreated {
		t.Fatalf("ak 新增 status = %d, want 200/201 (body: %s)", stAdd, bodyAdd)
	}
	var addResp struct {
		SKID string `json:"sk_id"`
	}
	if err := json.Unmarshal([]byte(bodyAdd), &addResp); err != nil || addResp.SKID == "" {
		t.Fatalf("新增响应应含 sk_id: %v (body: %s)", err, bodyAdd)
	}
	// ③ 本人列举 SK。
	if st, _ := signedGet(t, url+"/api/credentials/"+newAK+"/sk", newAK, newSK); st != http.StatusOK {
		t.Fatalf("sk 列表 status = %d, want 200", st)
	}
	// ④ 本人轮换 SK。
	if st, body := post("/api/credentials/"+newAK+"/renew", `{}`, newAK, newSK); st != http.StatusOK {
		t.Fatalf("renew status = %d, want 200 (body: %s)", st, body)
	}
	// ⑤ 让新 SK 立即过期（`{}` = 立即失效），审计必须留痕。
	if st, body := post("/api/credentials/"+addAK+"/sk/"+addResp.SKID+"/expire", `{}`, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("sk expire status = %d, want 200 (body: %s)", st, body)
	}

	for _, action := range []string{
		auditActionCredAKList, auditActionCredAKAdd, auditActionCredSKList, auditActionCredRenew,
		auditActionCredSKExpire,
	} {
		if len(auditActions(t, buf, action)) == 0 {
			t.Errorf("缺少 %s 审计行（认证面变更必须可追溯 ✗）", action)
		}
	}
	// 红线：凭据类审计绝不包含 SK 明文。
	for _, action := range []string{auditActionCredAKAdd, auditActionCredSKList, auditActionCredRenew} {
		for _, e := range auditActions(t, buf, action) {
			if d, ok := e["detail"].(string); ok && strings.Contains(d, newSK) {
				t.Fatalf("%s 审计泄露 SK 明文 ✗", action)
			}
		}
	}
}
