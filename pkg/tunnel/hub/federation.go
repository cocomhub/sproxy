// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

// maxFederationResponseBytes 是对端联邦节点表响应的 body 上限（4 MiB）。
// 对端是配置的可信 peer，但为防对端被攻破/误报超大节点表撑爆内存，解码前限流。
const maxFederationResponseBytes = 4 << 20

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
	// 目标 hub 配置了 access_keys 时必填；远程 peering 由 Config.Validate 强制成对校验。
	AccessKey       string
	AccessKeySecret string
	// CAFile 是对端 hub 的 TLS 受信 CA 证书文件路径（PEM）。非空时用该 CA 构建
	// 专属证书池严格校验对端证书（ServerName 由 URL host 自动校验）——自签 hub
	// 的远程 peering 应配置 ca_file 而非跳过校验。与 InsecureSkipVerify 互斥。
	CAFile string
	// InsecureSkipVerify 为 true 时跳过该 peer 的 TLS 证书校验。仅允许 loopback
	// peer（本机自签开发/测试，Config.Validate 限制）；仅作用于本 peer
	// （per-peer http.Client），不扩散到其他 peer。默认 false（严格校验 TLS，
	// 证书非法即拒绝，fail-closed）。
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
// 每个 peer 持有独立的 http.Client（TLS InsecureSkipVerify 按 peer 隔离，不全局扩散）。
type FederationClient struct {
	mu    sync.RWMutex
	peers []FederationPeer
	cands map[string][]FederationNode // peer.ID → 节点列表
	// clients（peer.ID → 独立 http.Client）在 NewFederationClient 构造后**不可变**：
	// syncPeer/Close/Start 均只读访问，无需持锁（加锁反而与 Peers()/Close() 的
	// 读锁风格不一致）。新增写操作前必须加锁并同步更新注释，防竞态。
	clients  map[string]*http.Client
	interval time.Duration
	logger   *slog.Logger

	// ---- 候选持久化（persistFile 非空时启用） ----
	// 联邦候选是发现缓存（非权威），快照只存 FederationNode 的 ID/addr/mesh（无 secret）。
	persistFile string
	// saveMu 保护候选文件写侧并发（scheduleSave 的 timer 状态 + SaveCandidates 写盘）。
	// 与 mu 独立：读 cands 用 mu.RLock（短临界），写盘是 I/O 慢操作——不持 mu，
	// 避免阻塞 Candidates()/PeersForNode() 等读路径。
	saveMu    sync.Mutex
	saveTimer *time.Timer // 激活中的去抖计时器；非 nil 表示已调度（受 saveMu 保护）
}

// NewFederationClient 创建联邦同步客户端（不启用候选持久化）。
// 等价于 NewFederationClientWithPersist(..., "", ...)。
func NewFederationClient(peers []FederationPeer, interval, timeout time.Duration, logger *slog.Logger) (*FederationClient, error) {
	return NewFederationClientWithPersist(peers, interval, timeout, logger, "")
}

// NewFederationClientWithPersist 创建联邦同步客户端；persistFile 非空时启用联邦候选
// 持久化（发现缓存非权威：重启后恢复上次同步的候选节点，不冷启动）。快照只存
// FederationNode 的 ID/addr/mesh（无 secret），损坏/缺失文件按空候选启动（不因持久化
// 文件损坏而拒绝启动，与 Persister.Load 一致）。
//
// interval<=0 回落默认 30s；timeout<=0 回落默认 10s；logger 为空回落 slog.Default。
// 每个 peer 独立创建 http.Client，TLS 策略（S-Medium 闭环）：
//   - CAFile 非空 → 用该 CA 构建专属证书池**严格校验**（InsecureSkipVerify=false，
//     ServerName 由 URL host 自动校验），供远程自签 hub 使用受信 CA；
//   - InsecureSkipVerify → 跳过校验（**仅限 loopback peer**，Config.Validate 强制）；
//   - 默认 → 系统根证书池严格校验（fail-closed：证书非法即拒绝，不静默降级）。
//
// CA 文件读取失败返回 error（fail-fast，不静默回退到不校验）。
func NewFederationClientWithPersist(peers []FederationPeer, interval, timeout time.Duration, logger *slog.Logger, persistFile string) (*FederationClient, error) {
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
	clients := make(map[string]*http.Client, len(peers))
	for _, p := range peers {
		if p.URL == "" {
			p.URL = DefaultFederationPeerURL
		}
		p.URL = strings.TrimRight(p.URL, "/")
		if p.ID == "" {
			p.ID = p.URL
		}
		c := &http.Client{Timeout: timeout}
		switch {
		case p.CAFile != "":
			pool, cerr := loadCertPool(p.CAFile)
			if cerr != nil {
				return nil, fmt.Errorf("peer %s: %w", p.ID, cerr)
			}
			c.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			}
		case p.InsecureSkipVerify:
			// 仅 loopback peer（Config.Validate 已拒绝远程 + insecure）。
			c.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 用户仅对本 loopback peer 显式配置跳过证书校验（本机自签开发/测试）
			}
		default:
			// 严格校验（系统根证书池），fail-closed。
			c.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
		}
		clients[p.ID] = c
		normalized = append(normalized, p)
	}
	fc := &FederationClient{
		peers:    normalized,
		cands:    make(map[string][]FederationNode),
		clients:  clients,
		interval: interval,
		logger:   logger,
	}
	if persistFile != "" {
		fc.persistFile = persistFile
		if err := fc.restoreCandidates(); err != nil {
			return nil, fmt.Errorf("读取联邦候选持久化文件失败: %w", err)
		}
	}
	return fc, nil
}

// federationNodeSnap 是联邦候选持久化快照中单个节点的离线表示（仅 id/addr/mesh）。
type federationNodeSnap struct {
	ID   string `json:"id"`
	Addr string `json:"addr,omitempty"`
	Mesh string `json:"mesh,omitempty"`
}

// federationPeerSnap 是一个联邦对端的候选节点列表快照。
type federationPeerSnap struct {
	Peer  string               `json:"peer"`
	Nodes []federationNodeSnap `json:"nodes"`
}

// federationCandidatesSnap 是联邦候选节点表的持久化快照（JSON）。
// 只存 peer.ID → 候选节点列表；仅包含 ID/addr/mesh（发现缓存，无 secret）。
type federationCandidatesSnap struct {
	Peers []federationPeerSnap `json:"peers"`
}

// maxFederationCandidatesBytes 是联邦候选持久化文件允许的最大字节数。
// 超出视为文件损坏（拒绝读入内存），避免攻击者/事故写入超大文件导致启动 OOM
// （与 hub 路由表持久化 Persister 一致）。
const maxFederationCandidatesBytes = 64 << 20 // 64 MiB

// federationSaveDebounce 是候选持久化的去抖窗口：变更密集时合并落盘。
const federationSaveDebounce = 200 * time.Millisecond

// loadCertPool 读取 PEM CA 文件并构建 x509 证书池。文件不存在/无有效证书返回错误。
func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 文件 %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA 文件 %s 无有效 PEM 证书", path)
	}
	return pool, nil
}

// Peers 返回归一化后的对端配置副本（URL 已回落默认、ID 已归一），供测试与诊断。
func (fc *FederationClient) Peers() []FederationPeer {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	out := make([]FederationPeer, len(fc.peers))
	copy(out, fc.peers)
	return out
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

	client := fc.clients[p.ID]
	if client == nil {
		return fmt.Errorf("peer %s 无对应 http client（内部错误）", p.ID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("拉取 %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("拉取 %s: HTTP %d", endpoint, resp.StatusCode)
	}
	// body 限流（防对端异常放大响应撑爆内存）：解码上限 maxFederationResponseBytes。
	var list []federationNodeResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFederationResponseBytes+1)).Decode(&list); err != nil {
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
	// 触发候选持久化（异步去抖落盘；persistFile 为空时 no-op）。
	fc.scheduleSave()
	return nil
}

// Candidates 返回所有 peer 的联邦候选节点合并列表。
// 跨 peer 按 (mesh, node-id) 去重（不同 peer 可能上报同一 mesh 的同名节点）；
// 节点顺序不保证稳定（map 遍历）。去重 key 用结构化类型，避免字符串拼接碰撞。
func (fc *FederationClient) Candidates() []FederationNode {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	type key struct {
		mesh string
		id   NodeID
	}
	seen := make(map[key]bool)
	var out []FederationNode
	for _, list := range fc.cands {
		for _, n := range list {
			k := key{mesh: n.Mesh, id: n.ID}
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, n)
		}
	}
	return out
}

// PeerForNode 返回上报了指定节点的联邦对端（在给定 mesh 下）。
// 跨 hub 中继转发用它定位「目标节点注册在哪个对端 hub」——本 hub 路由表未命中时，
// 把 relay 拨号请求转发到该对端的 /api/relay/stream。
// mesh 严格匹配：节点必须是给定 mesh 的候选，防跨 mesh 泄漏（与 mergeFederationNodes
// 的隔离语义一致）。多个对端上报同名同 mesh 节点时返回第一个（与 Candidates 去重顺序一致）。
func (fc *FederationClient) PeerForNode(id NodeID, mesh string) (FederationPeer, bool) {
	peers := fc.PeersForNode(id, mesh)
	if len(peers) == 0 {
		return FederationPeer{}, false
	}
	return peers[0], true
}

// PeersForNode 返回上报了指定节点的全部联邦对端（在给定 mesh 下），供跨 hub 转发
// 故障转移按序尝试（首个对端宕机时尝试下一个，与 MeshConnect 多候选回退一致）。
// mesh 严格匹配（与 PeerForNode 同隔离语义）；顺序不保证稳定（map 遍历）。
func (fc *FederationClient) PeersForNode(id NodeID, mesh string) []FederationPeer {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	var out []FederationPeer
	for _, p := range fc.peers {
		for _, n := range fc.cands[p.ID] {
			if n.ID == id && n.Mesh == mesh {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// Close 释放各 peer 客户端空闲连接（后台 goroutine 由 Start 的 ctx 取消控制退出），
// 并在持久化启用时同步 flush 未落盘的候选变更（进程优雅停服前确保最后一次变更不丢失）。
func (fc *FederationClient) Close() {
	fc.mu.RLock()
	for _, c := range fc.clients {
		c.CloseIdleConnections()
	}
	fc.mu.RUnlock()
	fc.flushSave()
}

// SaveCandidates 把当前候选节点表原子写盘（temp + rename + 0600，同目录临时文件
// 再 rename，不出现半写文件）。persistFile 为空时是 no-op（返回 nil，不落盘）。
// 写侧由 saveMu 串行化：并发调用不会产生半写文件/乱序覆盖——每次写入都反映调用
// 时刻的最新 cands（天然幂等）。
func (fc *FederationClient) SaveCandidates() error {
	if fc.persistFile == "" {
		return nil
	}
	fc.saveMu.Lock()
	defer fc.saveMu.Unlock()
	fc.mu.RLock()
	snap := fc.buildSnap()
	fc.mu.RUnlock()
	return fc.writeCandidatesFile(snap)
}

// buildSnap 从当前 cands 构建持久化快照（调用方需持有 fc.mu 读锁）。
func (fc *FederationClient) buildSnap() federationCandidatesSnap {
	snap := federationCandidatesSnap{}
	for peerID, nodes := range fc.cands {
		ps := federationPeerSnap{Peer: peerID}
		for _, n := range nodes {
			ps.Nodes = append(ps.Nodes, federationNodeSnap{ID: string(n.ID), Addr: n.Addr, Mesh: n.Mesh})
		}
		snap.Peers = append(snap.Peers, ps)
	}
	return snap
}

// writeCandidatesFile 原子写快照到 persistFile：同目录临时文件 + fsync + rename
// （与 hub 路由表持久化 Persister 同模式）。父目录不存在时返回 error（调用方记录日志）。
func (fc *FederationClient) writeCandidatesFile(snap federationCandidatesSnap) error {
	dir := filepath.Dir(fc.persistFile)
	tmp, err := os.CreateTemp(dir, filepath.Base(fc.persistFile)+".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败路径清理；成功后 rename 使条目失效，Remove 报错无害。

	if err := json.NewEncoder(tmp).Encode(&snap); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, fc.persistFile); err != nil {
		return err
	}
	// 联邦候选虽无敏感信息（仅 id/addr/mesh），仍显式收紧为 0600 与 hub 路由表持久化
	// 保持一致（统一权限策略，避免节点拓扑泄露给同机其他用户）。
	if err := os.Chmod(fc.persistFile, 0o600); err != nil {
		fc.logger.Warn("设置联邦候选文件权限 0600 失败", "path", fc.persistFile, "err", err)
	}
	return nil
}

// scheduleSave 标记候选表有变更并在去抖窗口后异步落盘。多次调用只触发一次落盘
// （窗口重置），落盘内容始终是**最新** cands（SaveCandidates 读调用时刻状态，
// 天然合并中间变更，与 Persister.Schedule 的去抖合并语义一致）。persistFile 为空
// 时是 no-op（零行为变更）。
func (fc *FederationClient) scheduleSave() {
	if fc.persistFile == "" {
		return
	}
	fc.saveMu.Lock()
	defer fc.saveMu.Unlock()
	if fc.saveTimer == nil {
		fc.saveTimer = time.AfterFunc(federationSaveDebounce, func() {
			fc.saveMu.Lock()
			fc.saveTimer = nil
			fc.saveMu.Unlock()
			if err := fc.SaveCandidates(); err != nil {
				fc.logger.Error("联邦候选持久化失败", "path", fc.persistFile, "err", err)
			}
		})
	} else {
		_ = fc.saveTimer.Reset(federationSaveDebounce)
	}
}

// flushSave 同步落盘当前候选（Close 前调用，确保最后一次变更不丢失）。停掉去抖
// timer 后写一次；persistFile 为空时是 no-op。
func (fc *FederationClient) flushSave() {
	if fc.persistFile == "" {
		return
	}
	fc.saveMu.Lock()
	t := fc.saveTimer
	fc.saveTimer = nil
	fc.saveMu.Unlock()
	if t != nil {
		t.Stop()
	}
	if err := fc.SaveCandidates(); err != nil {
		fc.logger.Error("联邦候选持久化 flush 失败", "path", fc.persistFile, "err", err)
	}
}

// restoreCandidates 从 persistFile 加载候选节点表（NewFederationClientWithPersist 构造时
// 调用，模拟重启恢复）。
//   - 文件不存在（未持久化过）→ 空候选、无错误；
//   - 文件存在但损坏/非法 JSON，或超出大小上限 → 记录 warn、空候选、无错误
//     （启动不因持久化文件损坏而失败，也不 panic，与 Persister.Load 一致）；
//   - 其余 I/O 错误（如路径是目录、权限不足）→ 返回 error，由调用方决定是否中止。
func (fc *FederationClient) restoreCandidates() error {
	fi, err := os.Stat(fc.persistFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() > maxFederationCandidatesBytes {
		// 快速路径：stat 已知超出上限，不读入内存（防启动 OOM）。
		fc.logger.Warn("联邦候选持久化文件超出大小上限，忽略并启动为空候选", "path", fc.persistFile, "size", fi.Size(), "max", maxFederationCandidatesBytes)
		return nil
	}
	raw, err := os.ReadFile(fc.persistFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// 权威上限校验（stat 之后文件可能被替换/膨胀，以实际读入长度为准）。
	if len(raw) > maxFederationCandidatesBytes {
		fc.logger.Warn("联邦候选持久化文件实际大小超出上限，忽略并启动为空候选", "path", fc.persistFile, "size", len(raw), "max", maxFederationCandidatesBytes)
		return nil
	}
	var snap federationCandidatesSnap
	if err := json.Unmarshal(raw, &snap); err != nil {
		fc.logger.Warn("联邦候选持久化文件损坏，忽略并启动为空候选", "path", fc.persistFile, "error", err)
		return nil
	}
	cands := make(map[string][]FederationNode, len(snap.Peers))
	for _, ps := range snap.Peers {
		if ps.Peer == "" {
			continue // 空 peer ID 无归属，丢弃（fail-closed）
		}
		nodes := make([]FederationNode, 0, len(ps.Nodes))
		for _, n := range ps.Nodes {
			if n.ID == "" {
				continue // 空 ID 节点无寻址意义，丢弃（与 syncPeer 丢弃语义一致）
			}
			nodes = append(nodes, FederationNode{ID: NodeID(n.ID), Addr: n.Addr, Mesh: n.Mesh})
		}
		cands[ps.Peer] = nodes
	}
	fc.mu.Lock()
	fc.cands = cands
	fc.mu.Unlock()
	return nil
}

// SetCandidatesForTest 直接设置候选节点表（仅测试注入用；生产路径经 syncPeer 更新）。
// 复制入参避免与调用方共享底层切片；不触发持久化。
func (fc *FederationClient) SetCandidatesForTest(peers map[string][]FederationNode) {
	cands := make(map[string][]FederationNode, len(peers))
	for k, v := range peers {
		nodes := make([]FederationNode, len(v))
		copy(nodes, v)
		cands[k] = nodes
	}
	fc.mu.Lock()
	fc.cands = cands
	fc.mu.Unlock()
}
