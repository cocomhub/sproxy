// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// quota_test.go —— `quota` 子命令的**联邦视图**呈现（片 5 读侧）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// federatedQuotaMux 只注册 `/api/quota`（**故意不注册**旧集中树端点 `/api/stats`）。
//
// **判别力** ✓：命令若硬依赖 `/api/stats` 才能出结果 ⇒ 本用例红（联邦视图是权威源，
// 旧集中树只是**补充**信息 ⇒ 必须 best-effort ✗ 不得阻塞）。
func federatedQuotaMux(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owner":"alice","used":3000,"capacity":8192,"unlimited":false,"partial":false,"volumes":[` +
			`{"volume":"v1","used":1000,"capacity":4096,"has_quota":true},` +
			`{"volume":"v2","used":2000,"capacity":4096,"has_quota":true}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestQuotaCmd_ShowsFederatedBreakdown 钉住人类可读输出含**逐卷明细**与**汇总**，
// 且**不依赖** `/api/stats`（旧集中树）。
func TestQuotaCmd_ShowsFederatedBreakdown(t *testing.T) {
	t.Parallel()
	srv := federatedQuotaMux(t)

	var buf bytes.Buffer
	cmd := newCmdQuota(clientfactory.NewMock(client.NewFileClient(srv.URL), nil),
		cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("quota 执行失败（不得依赖 /api/stats）: %v", err)
	}
	out := buf.String()
	// 人类可读输出经 formatBytes 格式化（3000 B → "2.9 KiB"）⇒ 断言格式化值而非原始数字。
	for _, want := range []string{"v1", "v2", "2.9 KiB", "1000 B", "2.0 KiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}
}

// TestQuotaCmd_JSONKeepsLegacyAndAddsFederated 钉住 `--json` **向后兼容**（保留 `quota` 键）
// 且**新增** `federated` 键（消费方脚本不被破坏 ✓）。
func TestQuotaCmd_JSONKeepsLegacyAndAddsFederated(t *testing.T) {
	t.Parallel()
	srv := federatedQuotaMux(t)

	var buf bytes.Buffer
	cmd := newCmdQuota(clientfactory.NewMock(client.NewFileClient(srv.URL), nil),
		cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.Flags().Bool("json", false, "JSON 输出")
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("设置 --json: %v", err)
	}
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("quota --json: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, buf.String())
	}
	if _, ok := got["quota"]; !ok {
		t.Fatalf("`--json` 必须保留旧 `quota` 键（向后兼容）:\n%s", buf.String())
	}
	raw, ok := got["federated"]
	if !ok {
		t.Fatalf("`--json` 必须新增 `federated` 键:\n%s", buf.String())
	}
	var fed client.FederatedQuota
	if err := json.Unmarshal(raw, &fed); err != nil {
		t.Fatalf("federated 段解析失败: %v", err)
	}
	if fed.Used != 3000 || len(fed.Volumes) != 2 {
		t.Fatalf("federated 段内容错: %+v", fed)
	}
}

// TestQuotaCmd_UnknownVolumeShownAsUnknownNotZero 钉住**未知 ≠ 0**的呈现：无每用户权威的卷
// 必须显示「未知」，**不得**显示成「已用 0 B」（禁把不知道当零用量 ✗）；且整体不完整时
// 必须显式提示（`Partial` ⇒ 汇总不完整）。
func TestQuotaCmd_UnknownVolumeShownAsUnknownNotZero(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/quota", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"owner":"alice","used":500,"capacity":0,"unlimited":true,"partial":true,"volumes":[` +
			`{"volume":"v1","used":500,"capacity":0,"has_quota":true},` +
			`{"volume":"v2","used":0,"capacity":0,"has_quota":false}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	cmd := newCmdQuota(clientfactory.NewMock(client.NewFileClient(srv.URL), nil),
		cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("quota: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "未知") {
		t.Fatalf("无权威卷必须显示「未知」（禁当 0）:\n%s", out)
	}
	if !strings.Contains(out, "不完整") {
		t.Fatalf("partial=true 必须显式提示汇总不完整:\n%s", out)
	}
	if !strings.Contains(out, "不限") {
		t.Fatalf("unlimited=true 必须呈现「不限」:\n%s", out)
	}
}
