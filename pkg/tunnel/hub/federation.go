// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// DefaultFederationPeerURL 是联邦对端 hub 的默认节点表端点基址。
//
// 安全边界：默认指向 **loopback**（127.0.0.1:18083，与 sproxy 默认监听端口一致）——
// 联邦同步是网络面能力，未显式配置对端 URL 时只允许与本机 hub peering（本地调试），
// 远程 peering 需显式配置 `url` 与 `access_key`/`access_key_secret`（见
// server.Config.Validate 的远程 peering 凭据强制校验）。
const DefaultFederationPeerURL = "http://127.0.0.1:18083"

// FederationNode 是联邦对端 hub 同步来的节点（发现/可达性候选）。
// 路由表仍本 hub 权威，联邦节点只进入 /api/hub/nodes 的候选合并，不改路由表状态。
type FederationNode struct {
	ID   NodeID
	Addr string
	Mesh string // 节点所属 mesh（本端合并时按 mesh 严格隔离，防跨 mesh 泄漏）
}

// FederationPeer 是联邦对端 hub 的配置（出站拉取）。
// 认证复用 SproxySig AccessKey/AccessKeySecret（与 hub 节点注册准入同一模式）；
// Secret 只在本端计算签名，永不上线。
type FederationPeer struct {
	ID  string // 对端 hub 唯一标识（日志/去重用；为空回落 URL）
	URL string // 对端节点表端点基址（如 http://127.0.0.1:18083；为空回落默认 loopback）
	// AccessKey / AccessKeySecret 是对端 hub 认可的 SproxySig 凭据。
	// 目标 hub 配置了 access_keys 时必填；远程 peering 由 Config.Validate 强制要求。
	AccessKey       string
	AccessKeySecret string
	// InsecureSkipVerify 为 true 时跳过 TLS 证书校验（自签证书测试/开发用）。
	// 生产应使用受信任证书并保持 false（默认）。
	InsecureSkipVerify bool
}

// federationNodeResp 是对端联邦节点表端点的响应结构（id/addr/mesh/connected）。
type federationNodeResp struct {
	ID   string `json:"id"`
	Addr string `json:"addr,omitempty"`
	Mesh string `json:"mesh,omitempty"`
}

// FederationClient 周期拉取各联邦对端 hub 的节点表，缓存联邦候选节点。
//
// 并发安全：缓存 map 由 RWMutex 保护；Start 为每个 peer 启动一个后台 goroutine，
// 监听 ctx.Done() 退出（goroutine 不泄漏）。拉取失败保留上次成功缓存
// （stale-while-error），不静默清空发现列表。
type FederationClient struct {
	mu       sync.RWMutex
	peers    []FederationPeer
	cands    map[string][]FederationNode // peer.ID → 节点列表
	client   *http.Client
	interval time.Duration
	logger   *slog.Logger
}

// NewFederationClient 创建联邦同步客户端。
// interval<=0 回落默认 30s；timeout<=0 回落默认 10s；logger 为空回落 slog.Default。
// 任一 peer 配置 InsecureSkipVerify 时客户端跳过 TLS 证书校验（自签证书场景）。
func NewFederationClient(peers []FederationPeer, interval, timeout time.Duration, logger *slog.Logger) *FederationClient {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	normalized := make([]FederationPeer, 0, len(peers))
	insecure := false
	for _, p := range peers {
		if p.URL == "" {
			p.URL = DefaultFederationPeerURL
		}
		p.URL = strings.TrimRight(p.URL, "/")
		if p.ID == "" {
			p.ID = p.URL
		}
		insecure = insecure || p.InsecureSkipVerify
		normalized = append(normalized, p)
	}
	client := &http.Client{Timeout: timeout}
	if insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 用户显式配置跳过证书校验（自签证书开发/测试）
		}
	}
	return &FederationClient{
		peers:    normalized,
		cands:    make(map[string][]FederationNode),
		client:   client,
		interval: interval,
		logger:   logger,
	}
}

// Start 为每个联邦对端启动后台拉取循环（首次立即拉取，之后按 interval 周期）。
// ctx 取消时所有 goroutine 退出（监听 ctx.Done()）。拉取失败仅记 Error 日志，
// 保留上次成功缓存。
func (fc *FederationClient) Start(ctx context.Context) {
	for _, p := range fc.peers {
		go fc.loop(ctx, p)
	}
}

// loop 是单个 peer 的周期拉取循环。ctx 取消时退出。
func (fc *FederationClient) loop(ctx context.Context, p FederationPeer) {
	if err := fc.syncPeer(ctx, p); err != nil && ctx.Err() == nil {
		fc.logger.Error("联邦节点表初始拉取失败", "peer", p.ID, "url", p.URL, "error", err)
	}
	ticker := time.NewTicker(fc.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := fc.syncPeer(ctx, p); err != nil && ctx.Err() == nil {
				fc.logger.Error("联邦节点表拉取失败", "peer", p.ID, "url", p.URL, "error", err)
			}
		}
	}
}

// SyncAll 同步所有联邦对端一次（拉取失败返回第一个错误，已成功的 peer 缓存保留）。
// 供测试与手动触发使用；Start 的后台循环内部也复用 syncPeer。
func (fc *FederationClient) SyncAll(ctx context.Context) error {
	var firstErr error
	for _, p := range fc.peers {
		if err := fc.syncPeer(ctx, p); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("sync peer %s: %w", p.ID, err)
		}
	}
	return firstErr
}

// syncPeer 拉取单个 peer 的节点表并更新缓存。
func (fc *FederationClient) syncPeer(ctx context.Context, p FederationPeer) error {
	endpoint := p.URL + "/api/hub/federation/nodes"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("构造请求 %s: %w", endpoint, err)
	}
	// 认证：SproxySig AccessKey 签名（sk 为空时不签名——目标 hub 为无认证调试模式）。
	sproxysig.SignRequest(req, p.AccessKey, p.AccessKeySecret)

	resp, err := fc.client.Do(req)
	if err != nil {
		return fmt.Errorf("拉取 %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("拉取 %s: HTTP %d", endpoint, resp.StatusCode)
	}
	var list []federationNodeResp
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("解析 %s: %w", endpoint, err)
	}
	nodes := make([]FederationNode, 0, len(list))
	for _, n := range list {
		if n.ID == "" {
			continue // 空 ID 节点无寻址意义，丢弃（fail-closed）
		}
		nodes = append(nodes, FederationNode{ID: NodeID(n.ID), Addr: n.Addr, Mesh: n.Mesh})
	}
	fc.mu.Lock()
	fc.cands[p.ID] = nodes
	fc.mu.Unlock()
	return nil
}

// Candidates 返回所有 peer 的联邦候选节点合并列表。
// 跨 peer 按 (mesh, node-id) 去重（不同 peer 可能上报同一 mesh 的同名节点）；
// 节点顺序不保证稳定（map 遍历）。
func (fc *FederationClient) Candidates() []FederationNode {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	seen := make(map[string]bool)
	var out []FederationNode
	for _, list := range fc.cands {
		for _, n := range list {
			key := n.Mesh + "\x00" + string(n.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, n)
		}
	}
	return out
}

// Close 释放客户端空闲连接（后台 goroutine 由 Start 的 ctx 取消控制退出）。
func (fc *FederationClient) Close() {
	fc.client.CloseIdleConnections()
}
