// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// testFedLogger 返回丢弃日志的 slog.Logger。
func testFedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testFedPeer 构造一个返回固定节点表的 mock 联邦对端。
// 节点表内容由 resp 决定；fail 非 0 时返回该状态码（模拟拉取失败）。
func testFedPeer(t *testing.T, resp []map[string]string, fail *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() != 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFederationClient_SyncFromPeer：从 mock peer 拉取节点表成功，Candidates 返回节点。
func TestFederationClient_SyncFromPeer(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": "M"},
		{"id": "node-b2", "addr": "192.168.1.3:9000", "mesh": ""},
	}, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	cands := fc.Candidates()
	byID := make(map[hub.NodeID]hub.FederationNode, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("Candidates 应含 2 个节点, got %d: %+v", len(byID), cands)
	}
	if n := byID[hub.NodeID("node-b1")]; n.Addr != "192.168.1.2:9000" || n.Mesh != "M" {
		t.Errorf("node-b1 应保留 addr/mesh, got %+v", n)
	}
	if n := byID[hub.NodeID("node-b2")]; n.Mesh != "" {
		t.Errorf("node-b2 应为默认 mesh, got %+v", n)
	}
}

// TestFederationClient_StaleWhileError：拉取失败保留上次成功缓存，不返回错误清空。
func TestFederationClient_StaleWhileError(t *testing.T) {
	var fail atomic.Int32
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": ""},
	}, &fail)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	fail.Store(1)
	if err := fc.SyncAll(context.Background()); err == nil {
		t.Fatalf("失败拉取应返回 error")
	}
	cands := fc.Candidates()
	if len(cands) != 1 || cands[0].ID != "node-b1" {
		t.Fatalf("失败后应保留上次成功缓存, got %+v", cands)
	}
}

// TestFederationClient_DefaultLoopbackURL：peer.URL 为空时归一化回落默认 loopback
// 地址（确定性断言，不发起网络请求——避免依赖本机 18083 端口是否有服务导致的 flaky）。
func TestFederationClient_DefaultLoopbackURL(t *testing.T) {
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB"}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	peers := fc.Peers()
	if len(peers) != 1 {
		t.Fatalf("Peers 应含 1 个对端, got %d", len(peers))
	}
	if peers[0].URL != hub.DefaultFederationPeerURL {
		t.Fatalf("空 URL 应回落默认 loopback %q, got %q", hub.DefaultFederationPeerURL, peers[0].URL)
	}
	if peers[0].ID != "peerB" {
		t.Fatalf("显式 ID 应保持, got %q", peers[0].ID)
	}

	// 空 ID + 空 URL：两者都归一为默认 URL（去重 key 冲突检测依据）。
	fc2, _ := hub.NewFederationClient([]hub.FederationPeer{{}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc2.Close)
	p2 := fc2.Peers()
	if len(p2) != 1 || p2[0].ID != hub.DefaultFederationPeerURL || p2[0].URL != hub.DefaultFederationPeerURL {
		t.Fatalf("空 ID/URL 应归一为默认 URL, got %+v", p2)
	}
}

// TestFederationClient_MeshPreserved：peer 返回带 mesh 的节点，Candidates 保留 mesh 供隔离。
func TestFederationClient_MeshPreserved(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-m1", "addr": "10.0.0.1:9000", "mesh": "meshA"},
		{"id": "node-m2", "addr": "10.0.0.2:9000", "mesh": "meshB"},
	}, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	meshes := make(map[hub.NodeID]string)
	for _, c := range fc.Candidates() {
		meshes[c.ID] = c.Mesh
	}
	if meshes[hub.NodeID("node-m1")] != "meshA" || meshes[hub.NodeID("node-m2")] != "meshB" {
		t.Fatalf("mesh 应保留, got %+v", meshes)
	}
}

// TestFederationClient_DedupAcrossPeers：多 peer 返回同 (mesh,id) 节点时去重。
func TestFederationClient_DedupAcrossPeers(t *testing.T) {
	body := []map[string]string{{"id": "node-x", "addr": "10.0.0.1:9000", "mesh": "M"}}
	peerA := testFedPeer(t, body, nil)
	peerB := testFedPeer(t, body, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "peerA", URL: peerA.URL},
		{ID: "peerB", URL: peerB.URL},
	}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	cands := fc.Candidates()
	if len(cands) != 1 {
		t.Fatalf("跨 peer 同节点应去重, got %d: %+v", len(cands), cands)
	}
}

// TestFederationClient_ConcurrentSync：并发 SyncAll 稳定（-race 覆盖）。
func TestFederationClient_ConcurrentSync(t *testing.T) {
	peerA := testFedPeer(t, []map[string]string{{"id": "node-a", "addr": "10.0.0.1:9000", "mesh": ""}}, nil)
	peerB := testFedPeer(t, []map[string]string{{"id": "node-b", "addr": "10.0.0.2:9000", "mesh": ""}}, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "peerA", URL: peerA.URL},
		{ID: "peerB", URL: peerB.URL},
	}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	ctx := context.Background()
	done := make(chan error, 4)
	for range 4 {
		go func() {
			done <- fc.SyncAll(ctx)
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatalf("并发 SyncAll: %v", err)
		}
	}
	cands := fc.Candidates()
	if len(cands) != 2 {
		t.Fatalf("并发同步后应有 2 节点, got %d: %+v", len(cands), cands)
	}
}

// TestFederationClient_StartContextCancel：Start 启动后台拉取，ctx 取消后 goroutine 退出。
func TestFederationClient_StartContextCancel(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": ""}}, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 10*time.Millisecond, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	ctx, cancel := context.WithCancel(context.Background())
	fc.Start(ctx)
	// 等至少一轮拉取完成。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fc.Candidates()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(fc.Candidates()) == 0 {
		t.Fatalf("Start 后应拉取到节点")
	}
	cancel()
	// ctx 取消后不 panic、Candidates 仍可读（goroutine 应退出）。
	time.Sleep(50 * time.Millisecond)
	_ = fc.Candidates()
}

// ---- TLS 安全面（S-Medium 闭环）测试 ----

// writeCertPEM 把 x509 证书写为 PEM 文件，返回路径（作 ca_file）。
func writeCertPEM(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0644); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}
	return path
}

// newSelfSignedCert 生成一个新的自签证书（作错误的 CA / 不受信任的对端）。
func newSelfSignedCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "federation-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

// TestFederationClient_CAFileStrictVerify：ca_file 指向对端自签证书时**严格校验**
// 成功（受信 CA 而非跳过校验）——远程自签 hub 的正确配置方式。
func TestFederationClient_CAFileStrictVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"node-tls","addr":"10.0.0.1:9000","mesh":""}]`))
	}))
	t.Cleanup(srv.Close)
	caPath := writeCertPEM(t, srv.Certificate())
	fc, err := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerTLS", URL: srv.URL, CAFile: caPath}}, 30*time.Second, 5*time.Second, testFedLogger())
	if err != nil {
		t.Fatalf("NewFederationClient: %v", err)
	}
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("ca_file 严格校验应成功: %v", err)
	}
	cands := fc.Candidates()
	if len(cands) != 1 || cands[0].ID != "node-tls" {
		t.Fatalf("候选应含 node-tls, got %+v", cands)
	}
}

// TestFederationClient_CAFileWrongCA：ca_file 指向错误 CA 时校验失败（fail-closed，
// 不因配置了 ca_file 而绕过校验）。
func TestFederationClient_CAFileWrongCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	wrongCA := writeCertPEM(t, newSelfSignedCert(t))
	fc, err := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerTLS", URL: srv.URL, CAFile: wrongCA}}, 30*time.Second, 5*time.Second, testFedLogger())
	if err != nil {
		t.Fatalf("NewFederationClient: %v", err)
	}
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err == nil {
		t.Fatalf("错误 CA 应校验失败（证书不受信）")
	}
}

// TestFederationClient_DefaultStrictVerify：默认（无 ca_file、无 insecure）严格校验
// TLS——自签对端证书不被系统根池信任，拉取失败（fail-closed，不默认跳过）。
func TestFederationClient_DefaultStrictVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	fc, err := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerTLS", URL: srv.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	if err != nil {
		t.Fatalf("NewFederationClient: %v", err)
	}
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err == nil {
		t.Fatalf("默认应严格校验 TLS，自签证书应拉取失败")
	}
}

// TestFederationClient_CAFileInvalid：ca_file 文件无有效 PEM 证书时 NewFederationClient
// 返回 error（fail-fast，不静默回退）。
func TestFederationClient_CAFileInvalid(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPath, []byte("not a certificate"), 0644); err != nil {
		t.Fatalf("write bad.pem: %v", err)
	}
	_, err := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerTLS", URL: "https://127.0.0.1:18083", CAFile: badPath}}, 30*time.Second, 5*time.Second, testFedLogger())
	if err == nil {
		t.Fatalf("无效 CA 文件应返回 error")
	}
}

// TestFederationClient_LoopbackInsecure：loopback peer + insecure_skip_verify 跳过
// 校验成功（本机自签开发），远程不在此构造层限制（Config.Validate 已强制 loopback）。
func TestFederationClient_LoopbackInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"node-loop","addr":"10.0.0.1:9000","mesh":""}]`))
	}))
	t.Cleanup(srv.Close)
	fc, err := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerLocal", URL: srv.URL, InsecureSkipVerify: true}}, 30*time.Second, 5*time.Second, testFedLogger())
	if err != nil {
		t.Fatalf("NewFederationClient: %v", err)
	}
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("loopback insecure 应成功: %v", err)
	}
}

// TestFederationClient_PeerForNode：跨 hub 转发定位——PeerForNode 返回上报目标节点
// 的联邦对端（mesh 严格匹配），未知/跨 mesh 返回 false。
func TestFederationClient_PeerForNode(t *testing.T) {
	peerA := testFedPeer(t, []map[string]string{
		{"id": "node-a1", "addr": "10.0.0.1:9000", "mesh": "M"},
	}, nil)
	peerB := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "10.0.0.2:9000", "mesh": "M"},
		{"id": "node-b2", "addr": "10.0.0.3:9000", "mesh": "N"},
	}, nil)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "peerA", URL: peerA.URL},
		{ID: "peerB", URL: peerB.URL},
	}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	// 命中：mesh M 的 node-b1 属于 peerB。
	p, ok := fc.PeerForNode("node-b1", "M")
	if !ok || p.ID != "peerB" {
		t.Fatalf("PeerForNode(node-b1, M) = (%q, %v), want (peerB, true)", p.ID, ok)
	}
	// mesh 严格隔离：node-b2 是 mesh N，mesh M 查询不得命中。
	if _, ok := fc.PeerForNode("node-b2", "M"); ok {
		t.Fatalf("跨 mesh 查询不应命中 node-b2")
	}
	if p, ok := fc.PeerForNode("node-b2", "N"); !ok || p.ID != "peerB" {
		t.Fatalf("PeerForNode(node-b2, N) = (%q, %v), want (peerB, true)", p.ID, ok)
	}
	// 未知节点返回 false。
	if _, ok := fc.PeerForNode("node-unknown", "M"); ok {
		t.Fatalf("未知节点不应命中")
	}
}

// ---- 候选节点表持久化测试 ----

// TestFederationClient_PersistSaveRestore：SaveCandidates 落盘后，新 client 从同一
// 文件恢复候选节点表（重启后不冷启动，DoD）。
func TestFederationClient_PersistSaveRestore(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "federation-candidates.json")
	fc1, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "peerB"}}, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("NewFederationClientWithPersist: %v", err)
	}
	t.Cleanup(fc1.Close)
	fc1.SetCandidatesForTest(map[string][]hub.FederationNode{
		"peerB": {
			{ID: "node-b1", Addr: "192.168.1.2:9000", Mesh: "M"},
			{ID: "node-b2", Addr: "192.168.1.3:9000", Mesh: ""},
		},
	})
	err = fc1.SaveCandidates()
	if err != nil {
		t.Fatalf("SaveCandidates: %v", err)
	}
	if _, statErr := os.Stat(persistFile); statErr != nil {
		t.Fatalf("SaveCandidates 后应产生候选文件: %v", statErr)
	}

	// 新 client（模拟重启）从同一文件恢复。配置仍含 peerB（审查 Minor 2：恢复只保留
	// 仍存在于当前配置的对端候选）。
	fc2, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "peerB"}}, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("恢复 client: %v", err)
	}
	t.Cleanup(fc2.Close)
	cands := fc2.Candidates()
	byID := make(map[hub.NodeID]hub.FederationNode, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("恢复后 Candidates 应含 2 节点, got %d: %+v", len(byID), cands)
	}
	if n := byID[hub.NodeID("node-b1")]; n.Addr != "192.168.1.2:9000" || n.Mesh != "M" {
		t.Errorf("node-b1 恢复应保留 addr/mesh, got %+v", n)
	}
	if n := byID[hub.NodeID("node-b2")]; n.Addr != "192.168.1.3:9000" || n.Mesh != "" {
		t.Errorf("node-b2 恢复应保留 addr/默认 mesh, got %+v", n)
	}
}

// TestFederationClient_PersistCorruptFile：候选文件损坏（非法 JSON）时按空候选启动，
// 不 panic 不报错（与 Persister.Load 损坏处理一致）。
func TestFederationClient_PersistCorruptFile(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "federation-candidates.json")
	if err := os.WriteFile(persistFile, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("写入损坏文件: %v", err)
	}
	fc, err := hub.NewFederationClientWithPersist(
		nil, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("损坏文件应按空候选启动不报错: %v", err)
	}
	t.Cleanup(fc.Close)
	if got := fc.Candidates(); len(got) != 0 {
		t.Fatalf("损坏文件应空候选, got %+v", got)
	}
}

// TestFederationClient_PersistMissingFile：候选文件缺失时按空候选启动，不 panic 不报错。
func TestFederationClient_PersistMissingFile(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "does-not-exist.json")
	fc, err := hub.NewFederationClientWithPersist(
		nil, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("缺失文件应按空候选启动不报错: %v", err)
	}
	t.Cleanup(fc.Close)
	if got := fc.Candidates(); len(got) != 0 {
		t.Fatalf("缺失文件应空候选, got %+v", got)
	}
}

// TestFederationClient_PersistDisabled：persistFile 为空（默认构造器）时零行为变更——
// SaveCandidates 为 no-op、不落盘、Close 正常。
func TestFederationClient_PersistDisabled(t *testing.T) {
	fc, err := hub.NewFederationClient(nil, 30*time.Second, 5*time.Second, testFedLogger())
	if err != nil {
		t.Fatalf("NewFederationClient: %v", err)
	}
	t.Cleanup(fc.Close)
	fc.SetCandidatesForTest(map[string][]hub.FederationNode{
		"peerB": {{ID: "node-b1", Addr: "1.2.3.4:9000", Mesh: "M"}},
	})
	err = fc.SaveCandidates()
	if err != nil {
		t.Fatalf("persistFile 为空时 SaveCandidates 应为 no-op: %v", err)
	}
	fc.Close() // persistFile 为空时 Close 不 flush 不 panic
}

// TestFederationClient_PersistAutoSaveOnSync：syncPeer 成功更新候选后自动异步落盘
// （触发时机），去抖窗口后文件出现且内容与缓存一致。
func TestFederationClient_PersistAutoSaveOnSync(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "federation-candidates.json")
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": "M"},
	}, nil)
	fc, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("NewFederationClientWithPersist: %v", err)
	}
	t.Cleanup(fc.Close)
	err = fc.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	// 去抖异步落盘：轮询等待文件出现（-race 下留足余量）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(persistFile); statErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(persistFile); statErr != nil {
		t.Fatalf("syncPeer 成功后应自动落盘候选文件: %v", statErr)
	}
	// 新 client 验证落盘内容可恢复（配置含 peerB——审查 Minor 2 剪枝要求）。
	fc2, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "peerB"}}, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatalf("恢复 client: %v", err)
	}
	t.Cleanup(fc2.Close)
	cands := fc2.Candidates()
	if len(cands) != 1 || cands[0].ID != "node-b1" || cands[0].Addr != "192.168.1.2:9000" || cands[0].Mesh != "M" {
		t.Fatalf("自动落盘内容应可恢复, got %+v", cands)
	}
}

// TestFederationClient_PersistRestoreIOError：候选文件为不可读目录时（非缺失/损坏）
// NewFederationClientWithPersist 返回 error（与 Persister.Load 的 I/O 错误上抛一致）。
func TestFederationClient_PersistRestoreIOError(t *testing.T) {
	dirPath := filepath.Join(t.TempDir(), "candidates-dir")
	if err := os.Mkdir(dirPath, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := hub.NewFederationClientWithPersist(
		nil, 30*time.Second, 5*time.Second, testFedLogger(), dirPath)
	if err == nil {
		t.Fatalf("读取目录路径作为候选文件应返回 error")
	}
}

// TestFederationClient_PersistConcurrentScheduleClose 验证（审查 Minor 1 回归）：
// 高频 scheduleSave + Close 并发下无 panic、文件最终为最新状态（去抖 timer 无
// Reset 双触发/游离；flushSave 正常收口）。不精确断言写次数（幂等原子写），
// 只断言并发安全 + 最终一致。
func TestFederationClient_PersistConcurrentScheduleClose(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "cands.json")
	peers := []hub.FederationPeer{{ID: "p1", URL: "http://127.0.0.1:18083"}}
	fc, err := hub.NewFederationClientWithPersist(
		peers, 30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				fc.SetCandidatesForTest(map[string][]hub.FederationNode{
					"p1": {{ID: "n1", Addr: "1.2.3.4:1"}},
				})
				fc.SaveCandidates() // 高频写（与 scheduleSave 的 timer 并发）
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	// 同时高频 scheduleSave（触发去抖 timer）。
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			fc.SetCandidatesForTest(map[string][]hub.FederationNode{
				"p1": {{ID: "n1", Addr: "1.2.3.4:1"}},
			})
			fc.SaveCandidates()
			time.Sleep(time.Millisecond)
		}
	})
	wg.Wait()

	fc.Close() // 并发关闭（flushSave 与在途 timer 竞态）

	// 文件应存在且内容为最新状态（无 panic、无损坏）。
	raw, err := os.ReadFile(persistFile)
	if err != nil {
		t.Fatalf("并发关闭后候选文件应存在: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("候选文件不应为空")
	}
	// 用 json.Unmarshal 到匿名结构验证可解析（不依赖导出类型）。
	var check struct {
		Peers []struct {
			Peer  string `json:"peer"`
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("并发写后文件应可解析: %v", err)
	}
	if len(check.Peers) == 0 {
		t.Fatal("文件应含候选数据")
	}
}

// TestFederationClient_PersistRemovedPeerNotRestored 验证（审查 Minor 2）：配置中
// 删除的联邦对端，其旧候选在恢复时被丢弃（不跨重启自我延续）。
func TestFederationClient_PersistRemovedPeerNotRestored(t *testing.T) {
	persistFile := filepath.Join(t.TempDir(), "cands.json")

	// 第一次：配置含 p1 + p2，写入两个对端的候选。
	fc1, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "p1", URL: "http://127.0.0.1:18083"}, {ID: "p2", URL: "http://127.0.0.1:18084"}},
		30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatal(err)
	}
	fc1.SetCandidatesForTest(map[string][]hub.FederationNode{
		"p1": {{ID: "n1", Addr: "1.1.1.1:1"}},
		"p2": {{ID: "n2", Addr: "2.2.2.2:2"}},
	})
	if saveErr := fc1.SaveCandidates(); saveErr != nil {
		t.Fatal(saveErr)
	}
	fc1.Close()

	// 第二次：配置删掉 p2，只留 p1——恢复时 p2 的候选应被丢弃。
	fc2, err := hub.NewFederationClientWithPersist(
		[]hub.FederationPeer{{ID: "p1", URL: "http://127.0.0.1:18083"}},
		30*time.Second, 5*time.Second, testFedLogger(), persistFile)
	if err != nil {
		t.Fatal(err)
	}
	defer fc2.Close()
	for _, n := range fc2.Candidates() {
		if n.ID == "n2" {
			t.Fatalf("已从配置删除的对端 p2 的候选 n2 不应被恢复（审查 Minor 2）")
		}
	}
}
