// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// cluster_test.go 验证集群出口凭证本地签发命令（评审 C2：签发操作入口）：
//   - 正常签发：输出 encoded 串可 Parse + VerifyStatic（防篡改）+ 字段齐全 + ttl 生效
//   - fail-closed：缺必填参数 / 非法 sign-key / 缺字段 → 报错不输出凭证

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/clustercred"
)

// runIssue 执行 issue 命令并返回 (stdout, stderr)。
func runIssue(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newClusterCredentialCmd(cli.IOStreams{Out: &out, ErrOut: &errOut})
	cmd.SetArgs(append([]string{"issue"}, args...))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("issue 命令执行失败: %v", err)
	}
	return out.String(), errOut.String()
}

// TestClusterCredentialIssue_EncodedRoundtrip：签发 → encoded 可 Parse + VerifyStatic
// + 字段/ttl 生效（签发即验签闭环，防错配）。
func TestClusterCredentialIssue_EncodedRoundtrip(t *testing.T) {
	t.Parallel()
	sk := make([]byte, 32)
	for i := range sk {
		sk[i] = byte(0x11)
	}
	out, errOut := runIssue(t,
		"--sign-key", hex.EncodeToString(sk),
		"--node", "holder-a", "--volume", "main",
		"--owner", "alice", "--recipient", "sha256:eg-fp",
		"--path-prefix", "docs", "--ttl", "12h")
	if errOut != "" {
		t.Fatalf("stderr 不应有输出: %q", errOut)
	}
	enc := trimTrailingNewline(out)
	if enc == "" {
		t.Fatal("无输出（应签发 encoded 串）")
	}
	cred, perr := clustercred.ParseCredential(enc)
	if perr != nil {
		t.Fatalf("ParseCredential: %v", perr)
	}
	if verr := cred.VerifyStatic(sk); verr != nil {
		t.Fatalf("VerifyStatic: %v", verr)
	}
	if cred.Node != "holder-a" || cred.Volume != "main" || cred.Owner != "alice" ||
		cred.Recipient != "sha256:eg-fp" || cred.PathPrefix != "docs" {
		t.Fatalf("凭证字段错: %+v", cred)
	}
	// ttl=12h → exp-iat == 12h；当前时间在有效期内。
	if cred.ExpiresAt-cred.IssuedAt != int64((12 * time.Hour).Seconds()) {
		t.Fatalf("ttl 未生效: exp-iat=%d want 12h", cred.ExpiresAt-cred.IssuedAt)
	}
	if now := time.Now().Unix(); now > cred.ExpiresAt || now < cred.IssuedAt {
		t.Fatalf("凭证不在有效期内（iat=%d exp=%d now=%d）", cred.IssuedAt, cred.ExpiresAt, now)
	}
}

// TestClusterCredentialIssue_FailClosed：非法/缺参 → 报错不输出凭证。
func TestClusterCredentialIssue_FailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"缺 recipient", []string{"--sign-key", hex.EncodeToString(make([]byte, 32)),
			"--node", "n", "--volume", "v", "--owner", "o"}},
		{"非法 sign-key（非 hex）", []string{"--sign-key", "zz",
			"--node", "n", "--volume", "v", "--owner", "o", "--recipient", "fp"}},
		{"sign-key 长度错", []string{"--sign-key", hex.EncodeToString(make([]byte, 16)),
			"--node", "n", "--volume", "v", "--owner", "o", "--recipient", "fp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			cmd := newClusterCredentialCmd(cli.IOStreams{Out: &out, ErrOut: &errOut})
			cmd.SetArgs(append([]string{"issue"}, tc.args...))
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("执行失败: %v", err)
			}
			if out.String() != "" {
				t.Fatalf("非法输入不应输出凭证: %q", out.String())
			}
			if errOut.String() == "" {
				t.Fatal("非法输入应在 stderr 报错（fail-closed）")
			}
		})
	}
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
