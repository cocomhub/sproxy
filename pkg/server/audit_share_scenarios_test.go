// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_share_scenarios_test.go 补设计 §9.1「分享创建/撤销」审计场景。
//
// 分享 = **授权变更**（把对象以 token 形式对外可见），撤销 = 收回授权 ⇒ 两者都必须可追溯，
// 且 object 用 **token**（detail 记文件名），便于「某 token 何时被谁创建/撤销」的取证。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_Share 创建 + 撤销分享 ⇒ 两条审计，object 都是 token、actor 是操作者。
func TestAuditScenario_Share(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	attachTestAudit(t, h)
	client := testHTTPClient(t)

	body := []byte("shared content")
	if st, resp := uploadFile(t, url, "shared.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	resp, err := client.Post(url+"/api/share", contentTypeJSON, strings.NewReader(`{"filename":"shared.txt","ttl":"1h"}`))
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create share status = %d, want 200", resp.StatusCode)
	}
	var created struct {
		Token string `json:"token"`
	}
	if derr := json.NewDecoder(resp.Body).Decode(&created); derr != nil {
		t.Fatalf("解析 share 响应: %v", derr)
	}
	if created.Token == "" {
		t.Fatal("share 响应应含 token")
	}

	delReq, _ := http.NewRequest(http.MethodDelete, url+"/api/shares/"+created.Token, nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", delResp.StatusCode)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	var sawCreate, sawRevoke bool
	for _, ev := range entries {
		if ev.Object != created.Token {
			continue
		}
		switch ev.Action {
		case "share_create":
			sawCreate = true
			if !strings.Contains(ev.Detail, "shared.txt") {
				t.Errorf("share_create detail 应含文件名, got %q", ev.Detail)
			}
		case "share_revoke":
			sawRevoke = true
		}
		if ev.Result != "success" {
			t.Errorf("%s result = %q, want success", ev.Action, ev.Result)
		}
	}
	if !sawCreate {
		t.Error("缺少 share_create 审计行（授权变更必须可追溯 ✗）")
	}
	if !sawRevoke {
		t.Error("缺少 share_revoke 审计行（撤销授权必须可追溯 ✗）")
	}
}
