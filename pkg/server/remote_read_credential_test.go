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
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
)

// newCredentialedReadHandler 装配一个带 main 卷（无 mesh_readers）+ 凭证池的只读面 handler。
// 凭证授：volume=main, owner=alice, recipient=egFP, path_prefix=docs。
// 返回 (handler, cfg) 供写盘断言（评审 I6：授权成功须落到真实文件内容）。
func newCredentialedReadHandler(t *testing.T, peerFP string) (http.Handler, *Config, error) {
	t.Helper()
	// 装配 cfg：单卷 main + 额外 other 卷（CrossVolumeRejected 用真实失配卷测凭证卷范围，
	// 而非「卷不存在」的 ByName miss）+ ACL（无 mesh_readers → 凭证分支才可达）。
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode: VolumeACLAllow, Owners: []string{testReaderOwner},
	})
	cfg.Volumes = append(cfg.Volumes, VolumeConfig{
		Name: "other", Root: filepath.Join(t.TempDir(), "vol-other"),
	})
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("加 other 卷后 cfg 校验失败: %v", err)
	}
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
		return nil, nil, fmt.Errorf("newClusterCredentialSet: %w", err)
	}
	// 装配 Handlers（main 卷入 volSet）。
	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	return h.newRemoteReadHandler(fakePeerFingerprint{fp: peerFP}, set), cfg, nil
}

// TestRemoteRead_CredentialAuthorized：合法凭证 → 授权并**委派出真实内容**（body 全等）——
// 评审 I6：此前只断言「非 401」弱断言，404 deny 与委派后文件不存在不可区分；现写盘
// 真实文件断言实际内容，钉住「凭证分支真实委派」。
func TestRemoteRead_CredentialAuthorized(t *testing.T) {
	t.Parallel()
	rh, cfg, err := newCredentialedReadHandler(t, "eg-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	// 写盘凭证 owner（alice）的 docs/f.bin——凭证 PathPrefix=docs 范围内。
	writeRemoteUserFile(t, cfg, "alice", "docs/f.bin", "cred-body-ok")
	req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path=docs/f.bin", nil)
	rec := httptest.NewRecorder()
	rh.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("凭证授权请求应 200（真实委派）, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "cred-body-ok" {
		t.Fatalf("委派内容应 == 持有侧原件, got %q", rec.Body.String())
	}
}

// TestCredential_WhiteListReject：对端指纹不在凭证白名单 → deny（404，不泄信息）。
func TestCredential_WhiteListReject(t *testing.T) {
	t.Parallel()
	rh, _, err := newCredentialedReadHandler(t, "unlisted-fp")
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
	rh, _, err := newCredentialedReadHandler(t, "eg-fp")
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
	rh, _, err := newCredentialedReadHandler(t, "eg-fp")
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

// TestCredential_ParentRefCannotPiercePrefix（评审 C1 回归）：凭证授 path_prefix=docs，
// 请求 `docs/../secret.txt`——authorize 统一清洗后路径为 `secret.txt`（落在 docs 外），
// 必须 deny。此前 authorize 对原始路径做前缀判定（HasPrefix("docs/") 通过）而读取侧
// Clean 消融 `..` 读 `secret.txt`——PathPrefix 可被顶穿（越权读任意文件）。
func TestCredential_ParentRefCannotPiercePrefix(t *testing.T) {
	t.Parallel()
	rh, _, err := newCredentialedReadHandler(t, "eg-fp")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	for _, path := range []string{
		"docs/../secret.txt",    // 清洗后 secret.txt，越出 docs
		"docs/../../secret.txt", // 清洗后 ../secret.txt（ValidateFilePath 拒绝 ..）
		"docs2/../docs/f.bin",   // 清洗后 docs/f.bin——在 docs 内，应通过（非 deny 目标，仅对照）
	} {
		req := httptest.NewRequest(http.MethodGet, "/remote/download?volume=main&path="+path, nil)
		rec := httptest.NewRecorder()
		rh.ServeHTTP(rec, req)
		if path == "docs2/../docs/f.bin" {
			// 清洗后落回 docs 内：不是授权类拒绝（应进入委派层，非 401/404 均属「已授权」）。
			if rec.Code == http.StatusUnauthorized || rec.Code == 0 {
				t.Fatalf("docs2/../docs/f.bin 清洗后应在 docs 内（非 401）, got %d", rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path=%q 顶穿前缀应 404（越权面）, got %d body=%s", path, rec.Code, rec.Body.String())
		}
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
