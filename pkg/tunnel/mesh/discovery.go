// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// parseDiscoveryPeerID 从 discovery 拨号的临时信令身份（disc-<base>-<unixnano>）
// 恢复真实 node ID：剥掉 "disc-" 前缀与末尾纯数字时间戳段（base 可含 '-'）。
// accept 侧据此把接受的链路注册进共享 linkPool（网关双向全覆盖的前提）。
func parseDiscoveryPeerID(temp string) (string, bool) {
	if !strings.HasPrefix(temp, "disc-") {
		return "", false
	}
	rest := strings.TrimPrefix(temp, "disc-")
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return "", false
	}
	tail := rest[idx+1:]
	if tail == "" {
		return "", false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	base := rest[:idx]
	if base == "" {
		return "", false
	}
	return base, true
}

// hubAPIError 是 hub HTTP API 错误（携带状态码供分类：4xx 致命 / 5xx 瞬时）。
type hubAPIError struct {
	code int
	body string
}

func (e *hubAPIError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("list hub nodes: HTTP %d: %s", e.code, e.body)
	}
	return fmt.Sprintf("list hub nodes: HTTP %d", e.code)
}

// ListHubNodes 返回 hub 上全部在线节点 ID（含自身与临时节点）。
// 轻量直连 GET /api/hub/nodes：不构造 FileClient，避免 tunnel_key/InitError 拖垮
// mesh node 常驻进程（与 relay status 的直连方式一致）。非空 signalToken 时带
// Authorization: Bearer。4xx/5xx/网络错误统一返回 *hubAPIError 或包装错误。
func ListHubNodes(ctx context.Context, baseURL, signalToken string, insecure bool) ([]string, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("list hub nodes: hub 地址为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/hub/nodes", nil)
	if err != nil {
		return nil, fmt.Errorf("list hub nodes: 构造请求失败: %w", err)
	}
	if signalToken != "" {
		req.Header.Set("Authorization", "Bearer "+signalToken)
	}
	var hc *http.Client
	if insecure {
		hc = client.InsecureHTTPClient()
	} else {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list hub nodes: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &hubAPIError{code: resp.StatusCode, body: string(body)}
	}
	var nodes []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, fmt.Errorf("list hub nodes: 解析失败: %w", err)
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

// discoveryLoop 维护对等直连集合（共享 linkPool，供本地网关复用）与失败冷却。
type discoveryLoop struct {
	links    *linkPool // peer nodeID -> 拨号侧 mux（mux 心跳保活）
	mu       sync.Mutex
	lastFail map[string]time.Time // peer -> 上次拨号失败时间（冷却）
}

// runDiscoveryLoop 周期经 hub 节点列表发现其他 mesh node，并行 webrtc 自动直连并
// 保持（full-mesh 拓扑）。links 是共享链路池（mesh node 本地网关据此复用已建链路）。
// localAddr/httpClient/serveOpts 供拨号侧对等链路上跑 relay.Serve（接受对端网关回拨，
// 双向服务互访）。返回错误仅当 /api/hub/nodes 4xx（auth/配置级致命，触发整 cycle
// 重连）；拨号失败 / 瞬时列表失败只冷却重试，不返回（避免重连风暴）。
func runDiscoveryLoop(ctx context.Context, cfg NodeConfig, nodeID, httpBase string, links *linkPool, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) error {
	interval := cfg.DiscoveryInterval
	if interval <= 0 {
		interval = defaultDiscoveryInterval
	}
	probe := cfg.DiscoveryProbeTimeout
	if probe <= 0 {
		probe = WebRTCProbeTimeout
	}
	maxParallel := cfg.DiscoveryMaxParallel
	if maxParallel <= 0 {
		maxParallel = defaultDiscoveryMaxParallel
	}

	dl := &discoveryLoop{links: links, lastFail: map[string]time.Time{}}
	defer dl.links.closeAll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := dl.discoverOnce(ctx, cfg, nodeID, httpBase, probe, maxParallel, localAddr, httpClient, serveOpts, logger); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (dl *discoveryLoop) discoverOnce(ctx context.Context, cfg NodeConfig, nodeID, httpBase string, probe time.Duration, maxParallel int, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) error {
	peers, err := ListHubNodes(ctx, httpBase, cfg.SignalToken, cfg.Insecure)
	if err != nil {
		var herr *hubAPIError
		if errors.As(err, &herr) && herr.code >= 400 && herr.code < 500 {
			return err // 4xx：auth/配置级，致命，触发整 cycle 重连
		}
		logger.Warn("mesh 自动发现节点列表失败（瞬时）", "error", err)
		return nil // 5xx/网络：下周期重试
	}

	// sweep 已断开连接（peer 离线自动重拨）。
	_ = dl.links.sweep()
	// 计算 targets：非自身、未连、半拨号去重（peer > nodeID，每对恰好一条链接）、
	// 冷却内跳过。
	var targets []string
	for _, p := range peers {
		if p == "" || p == nodeID {
			continue
		}
		if _, ok := dl.links.get(p); ok {
			continue
		}
		if p < nodeID {
			continue // 半拨号去重：只低 ID 拨高 ID，每对恰好一条链接
		}
		dl.mu.Lock()
		t, failed := dl.lastFail[p]
		dl.mu.Unlock()
		if failed && time.Since(t) < discoveryFailedPeerCooldown {
			continue
		}
		targets = append(targets, p)
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		return nil
	}

	// 并行拨号（信号量限并发）：每个拨号用独立临时信令身份（per-dial AutoRegister，
	// 独立收件箱规避共享 signaler 的 WaitAnswer 竞态）。
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for _, peer := range targets {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dl.dialPeer(ctx, cfg, p, probe, localAddr, httpClient, serveOpts, logger)
		}(peer)
	}
	wg.Wait()
	return nil
}

// dialPeer 对一个 peer 发起 webrtc 打洞直连并保持（mux 心跳），并在拨号侧链路上跑
// relay.Serve（接受对端网关回拨，双向服务互访）。
// per-dial 临时信令身份：AutoRegister 临时 node-id + secret，独立收件箱，避免多个
// 并发拨号共享 signaler 时 WaitAnswer 互相抢 answer。临时身份用 Prefix:"disc" +
// 真实 NodeID 作 base（disc-<base>-<unixnano>），供对端 accept 侧 parseDiscoveryPeerID
// 恢复本节点真实 ID 并注册链路。拨号后注销临时身份（连接已建立，数据面独立）。
func (dl *discoveryLoop) dialPeer(ctx context.Context, cfg NodeConfig, peer string, probe time.Duration, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) {
	if err := ctx.Err(); err != nil {
		return
	}
	temp, err := AutoRegister(ctx, AutoRegisterParams{
		HubURL: cfg.HubURL, ServerURL: cfg.ServerURL,
		RelayToken: cfg.RelayToken, SignalToken: cfg.SignalToken,
		NodeID: cfg.NodeID, Prefix: "disc", ExactNode: false,
		Insecure: cfg.Insecure,
	})
	if err != nil {
		logger.Debug("mesh 自动对等拨号身份注册失败", "peer", peer, "error", err)
		dl.markFail(peer)
		return
	}
	defer func() { _ = temp.Closer() }() // 拨号后注销临时身份（连接独立）

	probeCtx, cancel := context.WithTimeout(ctx, probe)
	conn, derr := webrtc.DialWithSignalerCtx(probeCtx, peer, temp.Signaler)
	cancel()
	if derr != nil {
		logger.Debug("mesh 自动对等拨号失败", "peer", peer, "error", derr)
		dl.markFail(peer)
		return
	}
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	dl.links.set(peer, m)
	// 拨号侧也跑 relay.Serve：接受对端网关经同一条已建链路回拨的流（accept 侧链路
	// 注册后，对端网关可路由回本节点服务）。serve 结束（链路断开/ctx 取消）即关 mux。
	go func(m *mux.Mux) {
		defer func() { _ = m.Close() }()
		if err := relay.Serve(ctx, m, localAddr, cfg.DialAllow, httpClient, logger, serveOpts...); err != nil {
			logger.Debug("mesh 对等链路 serve 结束", "peer", peer, "error", err)
		}
	}(m)
	logger.Info("mesh 自动对等直连建立", "peer", peer)
	if cfg.DiscoveryPeers != nil {
		select {
		case cfg.DiscoveryPeers <- peer:
		default:
		}
	}
}

func (dl *discoveryLoop) markFail(peer string) {
	dl.mu.Lock()
	dl.lastFail[peer] = time.Now()
	dl.mu.Unlock()
}
