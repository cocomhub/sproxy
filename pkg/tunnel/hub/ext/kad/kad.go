// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kad implements a Kademlia Distributed Hash Table for node discovery.
//
// It provides a full Kademlia routing table with XOR-distance-based bucket
// management, iterative FindNode lookup, and the hub.DHT interface.
//
// The implementation uses only the standard library. Node IDs are SHA-256
// hashes of the node's identity string.
package kad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

const (
	// keyBits is the number of bits in a Kademlia node ID (SHA-256).
	keyBits = 256

	// bucketSize is the maximum number of nodes per k-bucket.
	bucketSize = 20

	// alpha is the concurrency factor for iterative lookups.
	alpha = 3

	// maxLookupSteps is the maximum number of iterative lookup rounds.
	maxLookupSteps = 16

	// maxKadSnapshotBytes 是 k-bucket 持久化文件允许的最大字节数（与 hub 路由表
	// 持久化 Persister 一致）。超出视为文件损坏（拒绝读入内存），避免攻击者/事故
	// 写入超大文件导致启动时 OOM。
	maxKadSnapshotBytes = 64 << 20 // 64 MiB

	// kadSaveDebounce 是 k-bucket 落盘的去抖窗口：注册/移除风暴时合并为一次落盘。
	kadSaveDebounce = 200 * time.Millisecond

	// maxPersistIDLen 是持久化节点 ID 允许的最大字节数（防恶意超大 ID 占内存）。
	maxPersistIDLen = 512
)

// NodeID is a 256-bit Kademlia node identifier.
type NodeID [32]byte

// NodeIDFromString creates a NodeID by SHA-256 hashing the input string.
func NodeIDFromString(s string) NodeID {
	return sha256.Sum256([]byte(s))
}

// NodeIDFromHex parses a hex-encoded NodeID.
// 要求恰好 64 个 hex 字符（32 字节）；长度不对返回 error（曾对 31 字节 hex 触发
// b[:32] 越界 panic，Load 的损坏文件路径暴露）。
func NodeIDFromHex(s string) (NodeID, error) {
	if len(s) != 2*len(NodeID{}) {
		return NodeID{}, fmt.Errorf("kad: hex 长度 %d 非法，期望 %d", len(s), 2*len(NodeID{}))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return NodeID{}, err
	}
	var id NodeID
	copy(id[:], b)
	return id, nil
}

// Hex returns the hex-encoded representation of the NodeID.
func (n NodeID) Hex() string {
	return hex.EncodeToString(n[:])
}

// Xor returns the XOR distance between two NodeIDs.
func (n NodeID) Xor(other NodeID) NodeID {
	var result NodeID
	for i := range n {
		result[i] = n[i] ^ other[i]
	}
	return result
}

// PrefixLen returns the number of leading zero bits in the XOR distance.
// This determines the k-bucket index.
func (n NodeID) PrefixLen() int {
	for i := 0; i < len(n); i++ {
		if n[i] != 0 {
			return i*8 + bits.LeadingZeros8(n[i])
		}
	}
	return keyBits
}

// Less compares two NodeIDs lexicographically.
func (n NodeID) Less(other NodeID) bool {
	for i := range n {
		if n[i] != other[i] {
			return n[i] < other[i]
		}
	}
	return false
}

// kadNode is a node in the routing table.
type kadNode struct {
	info     hub.PeerInfo
	lastSeen time.Time
	online   bool
}

// Bucket is a Kademlia k-bucket containing up to bucketSize nodes.
type Bucket struct {
	mu    sync.Mutex
	nodes []*kadNode
}

// newBucket creates an empty bucket.
func newBucket() *Bucket {
	return &Bucket{
		nodes: make([]*kadNode, 0, bucketSize),
	}
}

// addNode adds or updates a node in the bucket.
// Returns true if the node was added, false if the bucket is full and the node was rejected.
func (b *Bucket) addNode(node *kadNode) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Update existing node
	for i, n := range b.nodes {
		if n.info.ID == node.info.ID {
			b.nodes[i].lastSeen = node.lastSeen
			b.nodes[i].online = node.online
			b.nodes[i].info = node.info
			// Move to front (most recently seen)
			b.moveToFront(i)
			return true
		}
	}

	// Add new node if bucket not full
	if len(b.nodes) < bucketSize {
		b.nodes = append(b.nodes, node)
		return true
	}

	// Bucket is full — check if the first (least recently seen) node is still online
	// If it's offline, replace it; otherwise reject the new node.
	first := b.nodes[0]
	if !first.online {
		b.nodes[0] = node
		b.moveToFront(0)
		return true
	}
	return false
}

// removeNode removes a node by ID.
func (b *Bucket) removeNode(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, n := range b.nodes {
		if n.info.ID == id {
			b.nodes = append(b.nodes[:i], b.nodes[i+1:]...)
			return
		}
	}
}

// getNodes returns a copy of all nodes in the bucket.
func (b *Bucket) getNodes() []*kadNode {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]*kadNode, len(b.nodes))
	copy(result, b.nodes)
	return result
}

// getNodesSnapshot 在 bucket 锁内复制每个节点的 PeerInfo 本体（ID/Addrs/Meta 深拷贝），
// 供持久化快照遍历（审查 PR-3 I-1：addNode 会在锁内原地改写 kadNode.info，返回
// 指针切片让调用方无锁读会与 addNode 数据竞争——锁内复制本体消除竞争）。
func (b *Bucket) getNodesSnapshot() []hub.PeerInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]hub.PeerInfo, 0, len(b.nodes))
	for _, n := range b.nodes {
		pi := n.info
		pi.Addrs = append([]string(nil), n.info.Addrs...)
		out = append(out, pi)
	}
	return out
}

// restoreNode 从持久化恢复一个节点（Load 用）：加锁追加到 bucket 末尾。
// 与 addNode 不同：不更新 lastSeen/online（恢复节点视为"上次发现、当前未验证在线"），
// 不触发 moveToFront（无最近活跃信息），仅在已存在相同 ID 时跳过（文件内重复幂等）
// 且超出 bucketSize 上限时截断（恢复时最多 K 个）。
func (b *Bucket) restoreNode(node *kadNode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.nodes {
		if n.info.ID == node.info.ID {
			return // 已存在，去重跳过
		}
	}
	if len(b.nodes) >= bucketSize {
		return // 超限截断
	}
	b.nodes = append(b.nodes, node)
}

// moveToFront moves the node at index i to the front (most recently seen).
func (b *Bucket) moveToFront(i int) {
	node := b.nodes[i]
	copy(b.nodes[1:], b.nodes[:i])
	b.nodes[0] = node
}

// Kademlia implements the Kademlia DHT routing table.
// 注意：当前实现不持有自身锁（Bucket 内部各自加锁）；若未来需要跨 bucket 原子
// 操作（如并发安全的重合/拆分），需补回 Kademlia 级锁。
//
// 持久化字段（persistFile / persistTimer / persistMu）是 EnablePersistence 启用的
// 可选能力：路由表本身（buckets）不持 Kademlia 级锁，Insert/Remove 与 Save 的快照
// 收集各自经 Bucket 锁串行化，天然并发安全；persistMu 只保护落盘去抖状态。
type Kademlia struct {
	id      NodeID
	buckets [keyBits]*Bucket
	logger  *slog.Logger

	// persistMu 保护以下持久化字段的并发访问。
	persistMu    sync.Mutex
	persistFile  string      // 持久化文件路径；"" 表示持久化关闭（零行为变更）
	persistTimer *time.Timer // 去抖落盘计时器；非 nil 表示已有排队的落盘
	// saveMu 串行化实际落盘（SaveCandidates 的 buildSnap+write 全程持锁，审查 PR-3
	// I-2）：回调与 FlushPersist 的 Save 都在锁内生成快照，后获得锁者必然写更新
	// 快照——杜绝"陈旧覆盖最新变更"（与联邦 saveMu 模式一致）。变更路径
	// Insert/Remove 不取此锁（无锁序倒置/死锁）。
	saveMu sync.Mutex
}

// NewKademlia creates a new Kademlia instance with the given node ID.
func NewKademlia(id string, logger *slog.Logger) *Kademlia {
	k := &Kademlia{
		id:     NodeIDFromString(id),
		logger: defaultLogger(logger),
	}
	for i := range k.buckets {
		k.buckets[i] = newBucket()
	}
	return k
}

// NodeID returns this node's Kademlia ID.
func (k *Kademlia) NodeID() NodeID {
	return k.id
}

// bucketIndex returns the bucket index for the given NodeID (XOR distance).
func (k *Kademlia) bucketIndex(target NodeID) int {
	dist := k.id.Xor(target)
	pl := dist.PrefixLen()
	if pl >= keyBits {
		return keyBits - 1
	}
	return pl
}

// Insert adds or updates a node in the appropriate k-bucket.
func (k *Kademlia) Insert(info hub.PeerInfo) {
	node := &kadNode{
		info:     info,
		lastSeen: time.Now(),
		online:   true,
	}
	idx := k.bucketIndex(NodeIDFromString(info.ID))
	k.buckets[idx].addNode(node)
	k.notifyChange()
}

// Remove removes a node from the routing table.
func (k *Kademlia) Remove(id string) {
	idx := k.bucketIndex(NodeIDFromString(id))
	k.buckets[idx].removeNode(id)
	k.notifyChange()
}

// notifyChange 在路由表变更（Insert/Remove）后触发异步落盘：去抖窗口内合并多次
// 变更，窗口到期后写**当时**的最新快照。持久化关闭（persistFile 为空）时是 no-op。
func (k *Kademlia) notifyChange() {
	// 审查 PR-3 M-4：持久化关闭（默认态）时锁外快速返回，避免每次 Insert/Remove
	// 一轮互斥锁往返（persistFile 在 EnablePersistence 后只写一次，读可无锁）。
	if k.PersistFile() == "" {
		return
	}
	k.persistMu.Lock()
	defer k.persistMu.Unlock()
	if k.persistTimer != nil {
		return // 已有排队的落盘，跳过（合并语义：到期落盘读的是最新快照）
	}
	path := k.persistFile
	k.persistTimer = time.AfterFunc(kadSaveDebounce, func() {
		// 去抖到期：落盘当前最新快照（Save 读调用时刻状态，天然合并窗口内全部变更）。
		k.persistMu.Lock()
		k.persistTimer = nil
		k.persistMu.Unlock()
		if err := k.Save(path); err != nil {
			k.logger.Error("kad: k-bucket 持久化落盘失败", "path", path, "err", err)
		}
	})
}

// FindClosest returns the k closest nodes to the target ID from the routing table.
// It searches from the closest bucket outward and returns nodes sorted by XOR distance.
func (k *Kademlia) FindClosest(target NodeID, n int) []hub.PeerInfo {
	seen := make(map[string]bool)
	var all []hub.PeerInfo

	idx := k.bucketIndex(target)

	// Search outward from the closest bucket
	for i := 0; i < keyBits; i++ {
		bidx := idx + i
		if bidx < keyBits {
			k.collectBucket(bidx, &all, &seen)
		}
		if i > 0 {
			bidx2 := idx - i
			if bidx2 >= 0 {
				k.collectBucket(bidx2, &all, &seen)
			}
		}
	}

	// Sort by XOR distance to target
	sort.Slice(all, func(i, j int) bool {
		di := target.Xor(NodeIDFromString(all[i].ID))
		dj := target.Xor(NodeIDFromString(all[j].ID))
		return di.Less(dj)
	})

	if len(all) > n {
		all = all[:n]
	}
	return all
}

// collectBucket collects nodes from a single bucket.
func (k *Kademlia) collectBucket(bidx int, result *[]hub.PeerInfo, seen *map[string]bool) {
	nodes := k.buckets[bidx].getNodes()
	for _, node := range nodes {
		if (*seen)[node.info.ID] {
			continue
		}
		(*seen)[node.info.ID] = true
		*result = append(*result, node.info)
	}
}

// defaultLogger returns a logger that discards output if nil.
// 注意：必须用 io.Discard 而非 nil writer——slog.NewTextHandler 对 nil writer
// 保持 nil，首次 Write 时 panic（Load 的损坏文件告警路径触发过）。
func defaultLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// kadNodeSnap 是 k-bucket 持久化快照中单个节点的离线表示。
//   - ID 是节点原始标识字符串（GetClosestNodes/Lookup 返回它，往返一致）；
//   - RouteID 是路由键（= NodeIDFromString(ID).Hex()，64 hex）。Load 时严格校验
//     其为合法 hex 且等于 NodeIDFromString(ID)，损坏/伪造条目据此丢弃；校验通过
//     后直接用 RouteID 重建路由位置（XOR 距离桶），不依赖本地节点身份；
//   - Addrs 是节点地址列表。
//
// 只存发现用节点信息（id/route/addr），**不存 secret/密钥**（k-bucket 本就不含
// 敏感字段，且持久化的是发现缓存而非权威路由表）。
// kadNodeSnap 是 k-bucket 持久化快照中单个节点的离线表示。
// 审查 PR-3 M-5：只存 id/route_id/addrs，**不持久化 PeerInfo.Meta**（能力宣告等）——
// 恢复节点 Meta 为空，由重启后的重新发现填充（设计文档 §5 缓存语义）。
type kadNodeSnap struct {
	ID      string   `json:"id"`
	RouteID string   `json:"route_id"`
	Addrs   []string `json:"addrs,omitempty"`
}

// kadSnap 是 k-bucket 持久化快照（JSON）。
type kadSnap struct {
	Nodes []kadNodeSnap `json:"nodes,omitempty"`
}

// Save 把当前 k-bucket 原子写盘到 path（同目录临时文件 + fsync + rename + 0600，
// 与 hub 路由表/联邦候选持久化同模式）。父目录不存在时返回 error。
// 审查 PR-3 I-2：buildSnap+write 全程持 saveMu，与去抖回调/FlushPersist 串行——
// 后获得锁者必然生成更新快照，杜绝"陈旧覆盖最新变更"。
func (k *Kademlia) Save(path string) error {
	k.saveMu.Lock()
	defer k.saveMu.Unlock()
	return writeKadFile(path, k.buildSnap(), k.logger)
}

// buildSnap 从当前所有 bucket 收集节点构建持久化快照（快照近似：每个 bucket 的
// getNodes 已加 bucket 锁复制，收集期间其他 goroutine 的变更不影响本次快照一致性）。
func (k *Kademlia) buildSnap() kadSnap {
	var snap kadSnap
	for i := range k.buckets {
		// 审查 PR-3 I-1：getNodesSnapshot 在 bucket 锁内复制 PeerInfo 本体，
		// 避免与 addNode 的原地更新数据竞争（getNodes 返回指针切片无锁读会竞争）。
		for _, info := range k.buckets[i].getNodesSnapshot() {
			snap.Nodes = append(snap.Nodes, kadNodeSnap{
				ID:      info.ID,
				RouteID: NodeIDFromString(info.ID).Hex(),
				Addrs:   append([]string(nil), info.Addrs...),
			})
		}
	}
	return snap
}

// Load 从 path 恢复 k-bucket。
//   - 文件不存在（未持久化过）→ 空桶、无错误；
//   - 文件存在但损坏/非法 JSON，或超出大小上限 → 记录 warn、空桶、无错误
//     （启动不因持久化文件损坏而失败，也不 panic）；
//   - 其余 I/O 错误（如路径是目录、权限不足）→ 返回 error，由调用方决定是否中止。
//
// 恢复的条目逐条校验：RouteID 必须为合法 hex、ID 非空且
// NodeIDFromString(ID)==RouteID，非法条目丢弃；单 bucket 超过 K（bucketSize）时截断。
func (k *Kademlia) Load(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() > maxKadSnapshotBytes {
		// 快速路径：stat 已知超出上限，不读入内存（防启动 OOM）。
		k.logger.Warn("kad: 持久化文件超出大小上限，忽略并启动为空桶", "path", path, "size", fi.Size(), "max", maxKadSnapshotBytes)
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// 权威上限校验（stat 之后文件可能被替换/膨胀，以实际读入长度为准）。
	if len(raw) > maxKadSnapshotBytes {
		k.logger.Warn("kad: 持久化文件实际大小超出上限，忽略并启动为空桶", "path", path, "size", len(raw), "max", maxKadSnapshotBytes)
		return nil
	}
	var snap kadSnap
	if err := json.Unmarshal(raw, &snap); err != nil {
		k.logger.Warn("kad: 持久化文件损坏，忽略并启动为空桶", "path", path, "error", err)
		return nil
	}
	for _, ns := range snap.Nodes {
		k.restoreNodeSnap(ns)
	}
	return nil
}

// restoreNodeSnap 校验并恢复单个持久化条目到对应 bucket；非法条目静默丢弃。
func (k *Kademlia) restoreNodeSnap(ns kadNodeSnap) {
	if ns.ID == "" || len(ns.ID) > maxPersistIDLen {
		return // 空/超长 ID 丢弃
	}
	route, err := NodeIDFromHex(ns.RouteID)
	if err != nil {
		return // route_id 非合法 hex（长度不对/无效）丢弃
	}
	if NodeIDFromString(ns.ID) != route {
		return // route_id 与 id 不匹配（损坏/伪造）丢弃
	}
	node := &kadNode{
		info: hub.PeerInfo{
			ID:    ns.ID,
			Addrs: append([]string(nil), ns.Addrs...),
		},
		// 恢复节点视为"上次发现、当前未验证在线"：online=false（bucket 满时新发现
		// 节点可替换它），lastSeen=零值（无最近活跃信息，不参与 moveToFront）。
		lastSeen: time.Time{},
		online:   false,
	}
	idx := k.bucketIndex(route)
	k.buckets[idx].restoreNode(node)
}

// EnablePersistence 启用 k-bucket 持久化：先 Load 恢复已有快照（缓存语义，路由表
// 仍 hub 权威），此后 Insert/Remove 变更经去抖异步落盘。path 为空是 no-op
// （零行为变更，不落盘、不 Load）。Load 的 I/O 错误（非损坏/缺失类）原样返回。
func (k *Kademlia) EnablePersistence(path string) error {
	if path == "" {
		return nil
	}
	if err := k.Load(path); err != nil {
		return fmt.Errorf("kad: 加载 k-bucket 持久化文件失败: %w", err)
	}
	k.persistMu.Lock()
	k.persistFile = path
	k.persistMu.Unlock()
	return nil
}

// PersistFile 返回当前持久化文件路径（"" 表示持久化关闭，供日志/诊断展示）。
func (k *Kademlia) PersistFile() string {
	k.persistMu.Lock()
	defer k.persistMu.Unlock()
	return k.persistFile
}

// FlushPersist 同步落盘当前快照（进程优雅停服/Close 前调用，确保去抖窗口内未落盘
// 的变更不丢失）。停掉去抖 timer 后写一次；持久化关闭（path 为空）时是 no-op。
func (k *Kademlia) FlushPersist() error {
	k.persistMu.Lock()
	path := k.persistFile
	t := k.persistTimer
	k.persistTimer = nil
	k.persistMu.Unlock()
	if t != nil {
		t.Stop()
	}
	if path == "" {
		return nil
	}
	return k.Save(path)
}

// writeKadFile 原子写快照到 path：同目录临时文件 + fsync + rename + 0600
// （与 hub 路由表持久化 Persister.writeFile 同模式）。父目录不存在时返回 error。
func writeKadFile(path string, snap kadSnap, logger *slog.Logger) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*.json")
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// 快照虽无敏感信息（仅 id/route/addr），仍显式收紧为 0600 与 hub 路由表持久化
	// 保持一致（统一权限策略，避免节点拓扑泄露给同机其他用户）。
	if err := os.Chmod(path, 0o600); err != nil {
		logger.Warn("kad: 设置持久化文件权限 0600 失败", "path", path, "err", err)
	}
	return nil
}
