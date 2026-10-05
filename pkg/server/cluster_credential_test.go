// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_credential_test.go 验证目标节点凭证池装配 + 按对端指纹索引（评审 C1）：
//   - 空池 → nil（零回归，仅静态 mesh_readers）
//   - signKey 缺失/长度/hex 非法 → 装配拒绝（fail-closed）
//   - 单/多凭证解析 + 验签（装配期 fail-fast：伪造/篡改凭证拒绝）
//   - 同 Recipient 重复 → errClusterCredentialDup（C1 键冲突不静默覆盖）
//   - credentialFor：按对端指纹命中 / 未列入白名单 / 卷不匹配（C2 纵深）

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
)

// signClusterCredential 构造一条已签发凭证（RCipient 白名单 + 卷 + 路径前缀 + 时效）。
func signTestCredential(t *testing.T, sk []byte, recipient, volume string) clustercred.Credential {
	t.Helper()
	now := time.Now().Unix()
	cred := clustercred.Credential{
		Node:       "holder-a",
		Volume:     volume,
		Owner:      "alice",
		Recipient:  recipient,
		Scope:      "read",
		PathPrefix: "docs",
		IssuedAt:   now - 100,
		ExpiresAt:  now + 1000,
	}
	cred.Sign(sk)
	return cred
}

// mustEncodedCredential 签发并 Marshal（装配配置数据）。
func mustEncodedCredential(t *testing.T, sk []byte, recipient, volume string) string {
	t.Helper()
	c := signTestCredential(t, sk, recipient, volume)
	enc, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return enc
}

// clusterCredTestSK 返回固定 32B 签发 SK（hex 串 = 64 hex for cfg）。
func clusterCredTestSK(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(0xAB)
	}
	return b
}

// newClusterCredentialConfig 构造带凭证池的 Config（签发 SK hex + 一条凭证）。
func newClusterCredentialConfig(t *testing.T, sk []byte, creds []ClusterCredentialConfig) *Config {
	t.Helper()
	cfg := Default()
	cfg.Cluster.CredentialSignKey = hex.EncodeToString(sk)
	cfg.Cluster.Credentials = creds
	return cfg
}

// TestClusterCredentialSet_EmptyReturnNil：凭证池空 → nil（装配零回归）。
func TestClusterCredentialSet_EmptyReturnNil(t *testing.T) {
	t.Parallel()
	s, err := newClusterCredentialSet(Default(), testLogger())
	if err != nil || s != nil {
		t.Fatalf("空凭证池应返回 (nil, nil)，got %v, %v", s, err)
	}
}

// TestClusterCredentialSet_SignKeyRequired：配置凭证池缺/错 signKey → 拒绝（fail-closed）。
func TestClusterCredentialSet_SignKeyRequired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sk   string
	}{
		{"缺 signKey", ""},
		{"非 32B hex（长度错）", "abcd"},
		{"非法 hex", strings.Repeat("z", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Cluster.CredentialSignKey = tc.sk
			cfg.Cluster.Credentials = []ClusterCredentialConfig{{Encoded: "{}"}}
			if _, err := newClusterCredentialSet(cfg, testLogger()); err == nil {
				t.Fatalf("signKey=%q 应拒绝（fail-closed）", tc.sk)
			}
		})
	}
}

// TestClusterCredentialSet_ByRecipient：凭证按 Recipient 索引——credentialFor 用对端
// 指纹查询命中（评审 C1：此前按 Node 索引致查恒 miss）。
func TestClusterCredentialSet_ByRecipient(t *testing.T) {
	t.Parallel()
	sk := clusterCredTestSK(t)
	cfg := newClusterCredentialConfig(t, sk, []ClusterCredentialConfig{
		{Encoded: mustEncodedCredential(t, sk, "eg-fp-1", "main")},
		{Encoded: mustEncodedCredential(t, sk, "eg-fp-2", "arch")},
	})
	s, err := newClusterCredentialSet(cfg, testLogger())
	if err != nil {
		t.Fatalf("newClusterCredentialSet: %v", err)
	}
	// 对端指纹命中（凭证.Recipient）——核心 C1 断言。
	cred, ok := s.credentialFor("main", "eg-fp-1")
	if !ok || cred.Recipient != "eg-fp-1" || cred.Volume != "main" {
		t.Fatalf("credentialFor(eg-fp-1) 应命中 main 凭证，ok=%v cred=%+v", ok, cred)
	}
	// 第二条独立指纹命中（多凭证互不干扰）。
	if _, ok := s.credentialFor("arch", "eg-fp-2"); !ok {
		t.Fatal("credentialFor(eg-fp-2) 应命中 arch 凭证")
	}
	// 未列入白名单的指纹 → 不命中。
	if _, ok := s.credentialFor("main", "unlisted-fp"); ok {
		t.Fatal("未列入白名单指纹应不命中")
	}
	// 卷不匹配（C2 纵深）→ 不命中。
	if _, ok := s.credentialFor("other-vol", "eg-fp-1"); ok {
		t.Fatal("凭证授卷 != 请求卷应不命中（越权面 fail-closed）")
	}
}

// TestClusterCredentialSet_DupRecipient：同 Recipient 两条凭证 → 装配期冲突拒绝。
func TestClusterCredentialSet_DupRecipient(t *testing.T) {
	t.Parallel()
	sk := clusterCredTestSK(t)
	// 同一 Recipient、不同 volume 的两条凭证。
	c1 := mustEncodedCredential(t, sk, "eg-fp-d", "main")
	c2cfg := signTestCredential(t, sk, "eg-fp-d", "arch")
	c2enc, _ := c2cfg.Marshal()
	cfg := newClusterCredentialConfig(t, sk, []ClusterCredentialConfig{
		{Encoded: c1}, {Encoded: c2enc},
	})
	if _, err := newClusterCredentialSet(cfg, testLogger()); err == nil {
		t.Fatal("同 Recipient 重复应拒绝（不静默覆盖）")
	}
}

// TestClusterCredentialSet_BadSign：签发 SK 与验签 SK 不一致 → 装配期验签拒绝。
func TestClusterCredentialSet_BadSign(t *testing.T) {
	t.Parallel()
	goodSK := clusterCredTestSK(t)
	badSK := make([]byte, 32) // 全 0，不同 SK
	encoded := mustEncodedCredential(t, goodSK, "eg-fp-x", "main")
	cfg := newClusterCredentialConfig(t, badSK, []ClusterCredentialConfig{{Encoded: encoded}})
	if _, err := newClusterCredentialSet(cfg, testLogger()); err == nil {
		t.Fatal("错 SK 签发凭证装配应验签拒绝（防伪）")
	}
}

// TestClusterCredentialSet_Expired（评审 I1 修复）：过期凭证装配**不**拒绝（装配期只验
// 格式+签名，时效是运行期下发控制点）——否则短效凭证过期重启即击穿整个服务启动
// （自 DoS）；过期在请求时 credentialFor 逐请求拒绝。
func TestClusterCredentialSet_Expired(t *testing.T) {
	t.Parallel()
	sk := clusterCredTestSK(t)
	cred := signTestCredential(t, sk, "eg-fp-e", "main")
	cred.ExpiresAt = time.Now().Unix() - 5 // 已过期
	cred.Sign(sk)
	enc, _ := cred.Marshal()
	cfg := newClusterCredentialConfig(t, sk, []ClusterCredentialConfig{{Encoded: enc}})
	s, err := newClusterCredentialSet(cfg, testLogger())
	if err != nil {
		t.Fatalf("过期凭证应可装配（装配期只验签名/格式，防篡改）: %v", err)
	}
	// 请求时过期 → 拒绝（时效是运行期控制点）。
	if _, ok := s.credentialFor("main", "eg-fp-e"); ok {
		t.Fatal("过期凭证请求时 credentialFor 应拒绝（时效运行期失效）")
	}
}

// TestClusterCredentialSet_RecipientTrimmed（评审 I3 修复）：写侧与查询侧统一 Trim——
// 签发侧 Recipient 带首尾空白（如下发端手抄）不再致查询恒 miss（此前写侧用原始键，
// 查询 Trim 恒 miss → 凭证静默 deny）。
func TestClusterCredentialSet_RecipientTrimmed(t *testing.T) {
	t.Parallel()
	sk := clusterCredTestSK(t)
	// 签发时 Recipient 带空白（签名含该值；写侧装配 Trim 后索引、查询侧 Trim 命中）。
	cred := signTestCredential(t, sk, " eg-fp-t ", "main")
	enc, _ := cred.Marshal()
	cfg := newClusterCredentialConfig(t, sk, []ClusterCredentialConfig{{Encoded: enc}})
	s, err := newClusterCredentialSet(cfg, testLogger())
	if err != nil {
		t.Fatalf("装配: %v", err)
	}
	// 查询指纹无空白 → 命中（写侧 Trim 键 == 查询 Trim 键）。
	if _, ok := s.credentialFor("main", "eg-fp-t"); !ok {
		t.Fatal("写侧 Trim 后按无空白指纹应命中（双侧 Trim 归一）")
	}
}
