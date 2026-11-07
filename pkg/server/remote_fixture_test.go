// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_fixture_test.go 是跨节点读写面的**装配层测试夹具**：经 RegisterRoutes 真实装配
// `*Handlers`，再把跨节点面 handler 挂到该装配上。
//
// 为什么夹具留在 pkg/server：跨节点面的协议层已下沉 pkg/server/remotefs，但仍有三类测试
// 必须看装配态——集群出口凭证池的验签（新造池即重复实现）、写面拒绝指标渲染（metrics 属
// 装配根）、远程写与本地写共享同一文件锁池（需要本地上传面）。它们经本夹具驱动。

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/remotefs"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// fakePeerFingerprint 是 remotefs.Peer 的测试实现（伪造已认证对端指纹）。
type fakePeerFingerprint struct{ fp string }

func (f fakePeerFingerprint) PeerFingerprint() string { return f.fp }

// 跨节点面测试共享常量（跨包夹具各持一份：测试符号无法跨包共享）。
const (
	testReaderNodeA = "nodeA"
	testReaderOwner = "alice"
	testReaderRel   = "docs/hello.txt"
	testReaderBody  = "hello-remote-read\n"

	testDualOwner = testReaderOwner
	testDualVol   = "main"
)

// remoteReadTestConfigACL 构造「单卷 main + 给定 ACL」的已校验 cfg（不写入任何文件）。
func remoteReadTestConfigACL(t *testing.T, acl *VolumeACLConfig) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: acl}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config 非法: %v", err)
	}
	return cfg
}

// writeRemoteUserFile 在卷根上写入 <owner>/user/<rel>（自动建中间目录）。
func writeRemoteUserFile(t *testing.T, cfg *Config, owner, rel, body string) {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, owner, "user", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// remoteReadTestConfig 构造「单卷 + 一条 mesh_readers + 磁盘上 docs/hello.txt」的已校验 cfg。
func remoteReadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
		}},
	})
	writeRemoteUserFile(t, cfg, testReaderOwner, testReaderRel, testReaderBody)
	return cfg
}

// newRemoteReadHandlers 用给定 cfg 装配 *Handlers（审计 JSON 写入 auditBuf）。
func newRemoteReadHandlers(t *testing.T, cfg *Config, auditBuf *bytes.Buffer) *Handlers {
	t.Helper()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(cfg)
	opts := defaultNoAuthRegOpts()
	opts.Mux = http.NewServeMux()
	opts.CfgPtr = cfgPtr
	opts.Version = "test"
	opts.BuildAt = "test"
	opts.Logger = testLogger()
	opts.AuditLogger = slog.New(slog.NewJSONHandler(auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() }) // (*Handlers).Close 返回 error，t.Cleanup 需 func()
	return h
}

// newRemoteReadHandler 在已装配的 Handlers 上构造只读面路由表（装配层 Deps 适配）。
func (h *Handlers) newRemoteReadHandler(peer remotefs.Peer, creds remotefs.CredentialSource) http.Handler {
	return remotefs.NewReadHandler(remoteDeps(h.currentConfig(), h, h.logger), peer, creds)
}

// newRemoteWriteHandler 在已装配的 Handlers 上构造写面路由表（装配层 Deps 适配）。
func (h *Handlers) newRemoteWriteHandler(peer remotefs.Peer) http.Handler {
	return remotefs.NewWriteHandler(remoteDeps(h.currentConfig(), h, h.logger), peer)
}

// currentConfig 返回当前配置快照（未装配 cfgPtr 的手工 Handlers 返回 nil）。
func (h *Handlers) currentConfig() *Config {
	if h == nil || h.cfgPtr == nil {
		return nil
	}
	return h.cfgPtr.Load()
}

// remoteWriteTestConfig 构造「单卷 + 一条 scope 可配的 mesh_readers」已校验 cfg。
// mode=allow 且 owners=[alice]：让 owner 本身过卷 ACL（第二重约束的第一半）。
func remoteWriteTestConfig(t *testing.T, scope string) *Config {
	t.Helper()
	return remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner, Scope: scope,
		}},
	})
}

// doRemoteWrite 发一个写请求（body 非 nil 时带上；checksum 非空时带 X-File-Checksum）。
func doRemoteWrite(t *testing.T, h http.Handler, method, target string, body []byte, checksum string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// startRemoteReadDualEnd 装配 B 侧真只读 listener 与 A 侧身份，返回
// (cfg, A 身份, B 指纹, listener)。
//
// B 的身份（Ed25519）写入临时目录并配 Hub.XferIdentityFile；B 的静态密钥由该身份指纹
// 派生（DeriveRemoteStaticKey），A 侧用同一 bFP 派生 ⇒ 两端一致。
func startRemoteReadDualEnd(t *testing.T) (*Config, *tunnel.Identity, string, *RemoteReadListener) {
	t.Helper()
	bID, bErr := tunnel.GenerateIdentity()
	if bErr != nil {
		t.Fatal(bErr)
	}
	aID, aErr := tunnel.GenerateIdentity()
	if aErr != nil {
		t.Fatal(aErr)
	}
	dir := t.TempDir()
	aFP, bFP := aID.Fingerprint(), bID.Fingerprint()

	cfg := Default()
	cfg.StorageRoot = filepath.Join(dir, "vol-main")
	cfg.LogLevel = "error"
	cfg.Audit.BufferSize = 100
	cfg.Hub.XferIdentityFile = filepath.Join(dir, "b-identity.json")
	if err := tunnel.SaveIdentity(bID, cfg.Hub.XferIdentityFile); err != nil {
		t.Fatal(err)
	}
	cfg.Volumes = []VolumeConfig{{Name: testDualVol, Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testDualOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: aFP, Owner: testDualOwner,
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:0"
	cfg.RemoteRead.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}

	h := newRemoteReadHandlers(t, cfg, &bytes.Buffer{})
	// 可取消 ctx：teardown 时让它让 B 侧 Serve 的 accept 循环立即收敛（不必等 TCP
	// 读错误重试退避），从而 ln.Close() 的 wg.Wait() 快速返回。
	rctx, rcancel := context.WithCancel(t.Context()) // NOSONAR: S8188 — rcancel 由 teardown 显式调用
	ln, err := StartRemoteReadListener(rctx, cfg, h, testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("StartRemoteReadListener: %v", err)
	}
	t.Cleanup(func() {
		rcancel()
		_ = ln.Close()
	})
	return cfg, aID, bFP, ln
}
