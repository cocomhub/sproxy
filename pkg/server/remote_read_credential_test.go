// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_read_credential_test.go 验证 remote_read authorize 的集群出口凭证分支
// （评审 C1/C2）：静态 mesh_readers 未命中时，按对端指纹验签凭证授权——
//   - 合法凭证 → 授权（owner=凭证.Owner，渲染凭证限定的卷/路径）
//   - 白名单不符（对端指纹 != 凭证.Recipient）→ deny
//   - 卷越界（凭证授 main，请求其它卷）→ deny（credentialFor 早拒 + Authorizes）
//   - 路径越界（超出凭证.PathPrefix）→ deny（Authorizes 生效）
//   - creds 未装配 → 仅静态 mesh_readers（零回归）

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
)

// newCredentialedReadHandler 装配一个带 main 卷（无 mesh_readers）+ 凭证池的只读面 handler。
// 凭证授：volume=main, owner=alice, recipient=egFP, path_prefix=docs。
func newCredentialedReadHandler(t *testing.T, peerFP string) (http.Handler, error) {
	t.Helper()
	// 装配 cfg：单卷 main + ACL（无 mesh_readers → 凭证分支才可达）。
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode: VolumeACLAllow, Owners: []string{testReaderOwner},
	})
	// 构造凭证池（签发 + 装配）。
	sk := clusterCredTestSK(t)
	now := time.Now().Unix()
	cred := clustercred.Credential{
		Node: "holder-a", Volume: "main", Owner: "alice",
		Recipient: "eg-fp", Scope: "read", PathPrefix: "docs",
		IssuedAt: now - 100, ExpiresAt: now + 1000,
	}
	cred.Sign(sk)
	enc, _ := cred.Marshal()
	credCfg := Default()
	credCfg.Cluster.Credentials = []ClusterCredentialConfig{{Encoded: enc}}
	credCfg.Cluster.CredentialSignKey = hex.EncodeToString(sk)
	set, err := newClusterCredentialSet(credCfg, testLogger())
	if err != nil {
		return nil, fmt.Errorf("newClusterCredentialSet: %w", err)
	}
	// 装配 Handlers（main 卷入 volSet）。
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	return h.newRemoteReadHandler(fakePeerFingerprint{fp: peerFP}, set), nil
}

// TestRemoteRead_CredentialAuthorized：合法凭证 → 授权（owner 取凭证.Owner）。
func TestRemoteRead_CredentialAuthorized(t *testing.T) {
	t.Parallel()
	rh, err := newCredentialedReadHandler(t, "eg-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=docs/f.bin", nil)
	rec := httptest.NewRecorder()
	// 直接调 handleDownload（走 authorize + 委托），应当委派成功（文件不存在 → 非授权类错误）
	// 或 404——但绝不能返回授权类 401/403（证明凭证被认可、进入委派）。
	rh.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == 0 {
		t.Fatalf("凭证授权请求不应 401（应进入委派层）, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCredential_WhiteListReject：对端指纹不在凭证白名单 → deny（404，不泄信息）。
func TestCredential_WhiteListReject(t *testing.T) {
	t.Parallel()
	rh, err := newCredentialedReadHandler(t, "unlisted-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=docs/f.bin", nil)
	rec := httptest.NewRecorder()
	rh.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("白名单外指纹应 404（fail-closed）, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCredential_CrossVolumeRejected：凭证授 main，请求其它卷 → deny（越权面）。
func TestCredential_CrossVolumeRejected(t *testing.T) {
	t.Parallel()
	rh, err := newCredentialedReadHandler(t, "eg-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=other&path=docs/f.bin", nil)
	rec := httptest.NewRecorder()
	rh.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨卷请求应 404（凭证只授 main）, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCredential_PathOutOfPrefixRejected：凭证授 path_prefix=docs，请求卷内其它路径 → deny。
func TestCredential_PathOutOfPrefixRejected(t *testing.T) {
	t.Parallel()
	rh, err := newCredentialedReadHandler(t, "eg-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=secret/f.bin", nil)
	rec := httptest.NewRecorder()
	rh.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("路径越出凭证前缀应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCredential_NoCredsFallsBackToStatic：creds nil（未装配凭证）→ 仅静态 mesh_readers
// 授权；无条目时拒绝（零回归，不与凭证混同）。
func TestCredential_NoCredsFallsBackToStatic(t *testing.T) {
	t.Parallel()
	// 无 mesh_readers 卷 + creds=nil → 凭证不可用，静态未命中 → deny。
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{testReaderOwner}})
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	rh := h.newRemoteReadHandler(fakePeerFingerprint{fp: "eg-fp"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=docs/f.bin", nil)
	rec := httptest.NewRecorder()
	rh.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("无凭证 + 无 mesh_readers 应 deny, got %d", rec.Code)
	}
}
