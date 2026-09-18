// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// ---- 信令面 CA（--ca-file 补全，自签 wss hub）----

// writeSelfSignedHubCert 生成自签服务端证书并返回 (certFile, caFile)：
// 自签证书文件本身即 CA 文件（auto_tls 生产形态）。
func writeSelfSignedHubCert(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "hub-cert.pem")
	keyFile = filepath.Join(dir, "hub-key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	return certFile, keyFile, certFile
}

// newTLSTestHub 启动自签 wss mock hub（/ws 注册 + HubServer），返回
// (routeTable, tlsServer, cancel, caFile)。
func newTLSTestHub(t *testing.T) (*hub.MeshRouteTable, *httptest.Server, context.CancelFunc, string, *int) {
	t.Helper()
	certFile, keyFile, caFile := writeSelfSignedHubCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("加载自签证书失败: %v", err)
	}
	rt := hub.NewMeshRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator(accesskey.NewRingFromKeyPairs([]accesskey.KeyPair{{Key: testAccessKey, Secret: testSecret}})), nil)
	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	// 信令端点：记录是否被调用（CA 信令 HTTP 注入断言）。
	signalCalls := 0
	muxHTTP.HandleFunc("/api/signal/", func(w http.ResponseWriter, r *http.Request) {
		signalCalls++
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewUnstartedServer(muxHTTP)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) { _ = srv.HandleConn(ctx, cc) }(c)
		}
	}()
	t.Cleanup(cancel)
	return rt, ts, cancel, caFile, &signalCalls
}

// TestHubWSDial_CAFile 验证：HubWSDial 加 CA 参数（带 RootCAs HTTPClient）连自签
// wss hub 成功（红灯：现实现只接受 insecure bool，无 CA 通道 → CA 场景无法连）。
func TestHubWSDial_CAFile(t *testing.T) {
	t.Parallel()
	_, ts, _, caFile, _ := newTLSTestHub(t)
	wssURL := "wss://" + ts.Listener.Addr().String() + "/ws"

	// 不配 CA：握手失败（系统根池严格校验）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := HubWSDial(ctx, wssURL, false); err == nil {
		t.Fatal("不配 CA 连自签 wss hub 应失败（fail-closed）")
	}

	// CA 文件：握手成功（红灯：HubWSDial 现签名不接受 CA）。
	ctxCA, cancelCA := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCA()
	conn, err := HubWSDialCA(ctxCA, wssURL, caFile)
	if err != nil {
		t.Fatalf("HubWSDialCA 应成功（红灯: %v）", err)
	}
	defer conn.Close()
}

// TestAutoRegister_CAFile_TLSHub 验证：AutoRegisterParams 加 CAFile 后经自签 wss hub
// 完成注册（拿到 per-node secret；红灯：现 AutoRegisterParams 无 CAFile 字段）。
func TestAutoRegister_CAFile_TLSHub(t *testing.T) {
	t.Parallel()
	rt, ts, _, caFile, sigCalls := newTLSTestHub(t)

	reg, err := AutoRegister(t.Context(), AutoRegisterParams{
		HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
		NodeID: "node-ca", Prefix: "p2p", ExactNode: false,
		CAFile: caFile,
	})
	if err != nil {
		t.Fatalf("AutoRegister(CAFile) 应成功（红灯: %v）", err)
	}
	t.Cleanup(func() { _ = reg.Closer() })
	info, ok := rt.LookupInfo(hub.NodeID(reg.TempNode))
	if !ok {
		t.Fatalf("temp node %q 未注册", reg.TempNode)
	}
	if info.Secret == "" {
		t.Fatal("per-node secret 应为空下发")
	}
	// 信令 HTTP 面必须用 CA 客户端打过 hub（CA 注入生效的端到端证据）：
	// 注册后主动发起一次信令 post（SendOffer 走 signaler 的 httpClient）。
	if err := reg.Signaler.SendOffer("node-ca", "sdp"); err != nil {
		t.Fatalf("SendOffer（信令 HTTP 面）应成功: %v", err)
	}
	if *sigCalls == 0 {
		t.Fatal("信令 HTTP 面未发起任何请求（CA 注入未生效 → 红灯）")
	}
}

// TestRunNode_CAFile_TLSHub 验证：NodeConfig 加 CAFile 后 mesh node 经自签 wss hub
// 注册成功（服务宣告可查；红灯：NodeConfig 无 CAFile 字段）。
func TestRunNode_CAFile_TLSHub(t *testing.T) {
	t.Parallel()
	rt, ts, cancel, caFile, _ := newTLSTestHub(t)
	defer cancel()

	nodeID := "mesh-node-ca"
	runErr := make(chan error, 1)
	go func() {
		runErr <- RunNode(t.Context(), NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
			NodeID: nodeID, Services: []hub.Service{{Name: "ca-probe", Addr: "127.0.0.1:1"}},
			ServiceAddrs: []string{"127.0.0.1:1"}, LocalAddr: "http://127.0.0.1:1",
			EnableWebRTC: false, CAFile: caFile,
		})
	}()

	testutil.WaitFor(t, 5*time.Second, func() bool { return rt.Lookup(hub.NodeID(nodeID)) != nil }, "mesh node(CAFile) 应注册")
	if rt.Lookup(hub.NodeID(nodeID)) == nil {
		t.Fatal("mesh node(CAFile) 未注册（红灯）")
	}
	svcs := rt.Table("").ServicesOf(hub.NodeID(nodeID))
	if len(svcs) != 1 || svcs[0].Name != "ca-probe" {
		t.Fatalf("服务宣告不对: %+v", svcs)
	}
	cancel()
}
