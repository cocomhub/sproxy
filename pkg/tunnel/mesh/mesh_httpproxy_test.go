// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/httpproxy"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestMeshHTTPProxy_Exit 经出口节点拉取目标页面：出口节点（mDNS 直连）本地跑目标 HTTP
// 服务，http-proxy 本地起代理（拨号 = mesh 直连到出口的 exitDial），客户端经代理
// 绝对 URI 访问目标页面，断言 body。
//
// 偏离简报说明：简报原意图用 NewLocalOrExitDial(200ms, exitDial) 验证「本地不可达→
// 回退出口」。但 in-process 拓扑下出口节点 RunNode 与本代理同机（同一 127.0.0.1 网络
// 命名空间），目标 httptest 监听 127.0.0.1 时本地直连必成功 → 走本地短路，出口路径
// 测不到（假绿）。故 proxyDial 直接用 exitDial，聚焦「经 mesh 出口拉取目标」数据通路；
// NewLocalOrExitDial 的本地直连/回退逻辑由任务 2 单测（TestLocalOrExitDial_*）覆盖。
func TestMeshHTTPProxy_Exit(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗；mDNS 组播的 loopback
	// 收敛路径在 Windows 上绑单播回环地址（见 listenMDNSLoopback），实测不弹窗。
	testMDNSLoopback(t)
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15371 // mDNS 测试端口（与 mesh_socks5_test 的 15370 错开）
	probeMDNSLoopback(t, port)

	// 出口节点本地目标页面（出口 dial 放行目标）。
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "page-from-exit")
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	logger := testMDNSLogger()
	nodeCtx := t.Context()
	nodeErr := make(chan error, 1)
	go func() {
		nodeErr <- RunNode(nodeCtx, NodeConfig{
			NodeID:         "node-exit",
			Services:       []hub.Service{{Name: "web", Addr: targetAddr}},
			ServiceAddrs:   []string{targetAddr},
			DialAllow:      true,
			EnableMDNS:     true,
			MDNSOnly:       true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0",
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         logger,
		})
	}()
	t.Cleanup(func() {
		// 节点退出后回收 goroutine（幂等）。
		nodeCtx.Done()
		select {
		case <-nodeErr:
		case <-time.After(5 * time.Second):
		}
	})

	// http-proxy 本地（复用 socks 测试的 mDNS 发现模式）：浏览发现出口节点信令端点。
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer browseCancel()
	mdnsSrv, err := NewMDNS(MDNSConfig{NodeID: "node-proxy", BrowseOnly: true, Port: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	if serr := mdnsSrv.Start(browseCtx); serr != nil {
		t.Fatalf("mDNS start: %v", serr)
	}
	defer mdnsSrv.Close()

	// 等出口节点 mDNS 广播（LookupPeer 3 参：ctx / nodeID / timeout，返回 MDNSPeer）。
	peer, perr := mdnsSrv.LookupPeer(browseCtx, "node-exit", 15*time.Second)
	if perr != nil {
		t.Fatalf("mDNS 未发现出口节点 node-exit: %v", perr)
	}
	if verr := ValidateSignalAddr(peer.SignalAddr); verr != nil {
		t.Fatalf("出口节点信令端点非法: %v", verr)
	}

	// 经出口的拨号闭包：mesh 直连信令 → 写 dial 帧 → 出口本机拨号目标。
	exitDial := func(ctx context.Context, addr string) (net.Conn, error) {
		sig, serr := DialDirectSignaler(ctx, peer.SignalAddr, "node-proxy")
		if serr != nil {
			return nil, serr
		}
		sig.SetSecret("")
		res, derr := DialDirect(ctx, sig, &client.MeshService{Name: "web", Node: "node-exit", Addr: addr})
		_ = sig.Close()
		if derr != nil {
			return nil, derr
		}
		return res.Conn, nil
	}

	// 起 http-proxy（拨号 = 经出口，见文件头偏离说明）。
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 http-proxy: %v", err)
	}
	defer proxyLn.Close()
	ss := httpproxy.New(httpproxy.Config{Dial: exitDial, Logger: logger})
	go func() { _ = ss.Serve(t.Context(), proxyLn) }()

	// 客户端经代理访问目标（绝对 URI 转发）。
	proxyURL := "http://" + proxyLn.Addr().String()
	req, err := http.NewRequest(http.MethodGet, target.URL, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.URL, _ = url.Parse(target.URL)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxyURL) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("经代理访问失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "page-from-exit" {
		t.Fatalf("body = %q, want page-from-exit", body)
	}
}
