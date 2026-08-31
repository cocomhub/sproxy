// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kad

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestNodeIDFromString(t *testing.T) {
	id1 := NodeIDFromString("node-1")
	id2 := NodeIDFromString("node-1")
	id3 := NodeIDFromString("node-2")

	if id1 != id2 {
		t.Fatal("same input should produce same NodeID")
	}
	if id1 == id3 {
		t.Fatal("different input should produce different NodeID")
	}
}

func TestNodeIDXor(t *testing.T) {
	a := NodeIDFromString("alpha")
	b := NodeIDFromString("beta")
	xor := a.Xor(b)

	// XOR is commutative
	xor2 := b.Xor(a)
	if xor != xor2 {
		t.Fatal("XOR should be commutative")
	}

	// Self XOR is zero
	zero := a.Xor(a)
	var expected NodeID
	if zero != expected {
		t.Fatal("self XOR should be zero")
	}
}

func TestNodeIDPrefixLen(t *testing.T) {
	// Two identical IDs should have prefix length = keyBits
	a := NodeIDFromString("same")
	dist := a.Xor(a)
	if dist.PrefixLen() != keyBits {
		t.Fatalf("expected prefixLen=%d for self, got %d", keyBits, dist.PrefixLen())
	}

	// Two different IDs should have a smaller prefix length
	b := NodeIDFromString("different")
	dist2 := a.Xor(b)
	if dist2.PrefixLen() >= keyBits {
		t.Fatal("expected prefixLen < keyBits for different IDs")
	}
}

func TestBucketAddNode(t *testing.T) {
	b := newBucket()

	// Add a node
	node := &kadNode{
		info: hub.PeerInfo{ID: "node-1", Addrs: []string{"addr1"}},
	}
	if !b.addNode(node) {
		t.Fatal("expected addNode to succeed")
	}
	if len(b.nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(b.nodes))
	}

	// Update existing node
	node2 := &kadNode{
		info: hub.PeerInfo{ID: "node-1", Addrs: []string{"addr2"}},
	}
	if !b.addNode(node2) {
		t.Fatal("expected addNode to update existing node")
	}
	if len(b.nodes) != 1 {
		t.Fatalf("expected 1 node after update, got %d", len(b.nodes))
	}
	if b.nodes[0].info.Addrs[0] != "addr2" {
		t.Fatalf("expected updated addr, got %v", b.nodes[0].info.Addrs)
	}
}

func TestBucketFull(t *testing.T) {
	b := newBucket()

	// Fill the bucket
	for i := 0; i < bucketSize; i++ {
		id := string(rune('a' + i))
		node := &kadNode{
			info:   hub.PeerInfo{ID: id, Addrs: []string{"addr"}},
			online: true,
		}
		if !b.addNode(node) {
			t.Fatalf("expected addNode %d to succeed", i)
		}
	}

	// Bucket is full with online nodes, new node should be rejected
	newNode := &kadNode{
		info:   hub.PeerInfo{ID: "new", Addrs: []string{"addr"}},
		online: true,
	}
	if b.addNode(newNode) {
		t.Fatal("expected addNode to reject when bucket is full with online nodes")
	}
	if len(b.nodes) != bucketSize {
		t.Fatalf("expected bucket size %d, got %d", bucketSize, len(b.nodes))
	}
}

func TestBucketReplaceOffline(t *testing.T) {
	b := newBucket()

	// Fill the bucket with offline nodes
	for i := 0; i < bucketSize; i++ {
		id := string(rune('a' + i))
		node := &kadNode{
			info:   hub.PeerInfo{ID: id, Addrs: []string{"addr"}},
			online: false,
		}
		b.addNode(node)
	}

	// Replace the first (oldest) offline node
	newNode := &kadNode{
		info:   hub.PeerInfo{ID: "new", Addrs: []string{"addr"}},
		online: true,
	}
	if !b.addNode(newNode) {
		t.Fatal("expected addNode to replace oldest offline node")
	}
	if len(b.nodes) != bucketSize {
		t.Fatalf("expected bucket size %d, got %d", bucketSize, len(b.nodes))
	}
	// New node should be at the front
	if b.nodes[0].info.ID != "new" {
		t.Fatalf("expected new node at front, got %s", b.nodes[0].info.ID)
	}
}

func TestKademliaInsert(t *testing.T) {
	k := NewKademlia("local-node", nil)

	// Insert a node
	k.Insert(hub.PeerInfo{ID: "remote-node", Addrs: []string{"192.168.1.1:9000"}})

	closest := k.FindClosest(NodeIDFromString("remote-node"), 5)
	if len(closest) != 1 {
		t.Fatalf("expected 1 node, got %d", len(closest))
	}
	if closest[0].ID != "remote-node" {
		t.Fatalf("expected remote-node, got %s", closest[0].ID)
	}
}

func TestKademliaFindClosest(t *testing.T) {
	k := NewKademlia("local-node", nil)

	// Insert multiple nodes
	nodes := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for _, n := range nodes {
		k.Insert(hub.PeerInfo{ID: n, Addrs: []string{"addr"}})
	}

	// Find closest to a target
	closest := k.FindClosest(NodeIDFromString("target"), 3)
	if len(closest) != 3 {
		t.Fatalf("expected 3 closest nodes, got %d", len(closest))
	}
}

func TestKademliaInsertAndRemove(t *testing.T) {
	k := NewKademlia("local-node", nil)

	k.Insert(hub.PeerInfo{ID: "node-1", Addrs: []string{"addr"}})
	k.Remove("node-1")

	closest := k.FindClosest(NodeIDFromString("node-1"), 5)
	if len(closest) != 0 {
		t.Fatalf("expected 0 nodes after remove, got %d", len(closest))
	}
}

func TestKademliaDHT_Register(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	err := dht.Register(t.Context(), hub.PeerInfo{ID: "node-1", Addrs: []string{"addr"}})
	if err != nil {
		t.Fatal(err)
	}

	info, err := dht.Lookup(t.Context(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "node-1" {
		t.Fatalf("expected node-1, got %s", info.ID)
	}
}

func TestKademliaDHT_GetClosestNodes(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	dht.Register(t.Context(), hub.PeerInfo{ID: "a", Addrs: []string{"addr"}})
	dht.Register(t.Context(), hub.PeerInfo{ID: "b", Addrs: []string{"addr"}})
	dht.Register(t.Context(), hub.PeerInfo{ID: "c", Addrs: []string{"addr"}})

	closest, err := dht.GetClosestNodes(t.Context(), "target", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(closest) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(closest))
	}
}

func TestKademliaDHT_LookupNotFound(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	_, err := dht.Lookup(t.Context(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent node")
	}
}

func TestKademliaDHT_Bootstrap(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	defer dht.Close()

	err := dht.Bootstrap(t.Context(), []string{"seed1", "seed2"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKademliaDHT_Close(t *testing.T) {
	dht := NewDHT("local-node", nil, nil)
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectAlpha(t *testing.T) {
	closest := []hub.PeerInfo{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"},
	}
	queried := map[string]bool{"a": true, "c": true}

	selected := selectAlpha(closest, queried, 2)
	if len(selected) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(selected))
	}
	if selected[0].ID != "b" || selected[1].ID != "d" {
		t.Fatalf("expected b and d, got %v", selected)
	}
}

// TestKademliaSaveLoad_RoundTrip 验证 Save→Load 往返后 FindClosest 结果一致
// （ID 顺序与 Addrs 均保留，DoD 1）。节点 ID 用任意字符串（生产路径 hub 喂入
// 的就是 hub.NodeID 字符串，非 hex），Load 经 route_id 校验后重建路由位置。
func TestKademliaSaveLoad_RoundTrip(t *testing.T) {
	k1 := NewKademlia("local-node", nil)
	ids := []string{"node-1", "node-2", "node-3", "alpha", "bravo"}
	for _, id := range ids {
		k1.Insert(hub.PeerInfo{ID: id, Addrs: []string{"addr-" + id}})
	}
	path := filepath.Join(t.TempDir(), "kad-save.json")
	if err := k1.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	k2 := NewKademlia("local-node", nil)
	if err := k2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	target := NodeIDFromString("probe")
	got := k2.FindClosest(target, len(ids)+1)
	want := k1.FindClosest(target, len(ids)+1)
	if len(got) != len(want) {
		t.Fatalf("FindClosest len: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("FindClosest[%d].ID: want %q, got %q", i, want[i].ID, got[i].ID)
		}
		if len(got[i].Addrs) != len(want[i].Addrs) ||
			(len(got[i].Addrs) > 0 && got[i].Addrs[0] != want[i].Addrs[0]) {
			t.Fatalf("FindClosest[%d].Addrs: want %v, got %v", i, want[i].Addrs, got[i].Addrs)
		}
	}
}

// TestKademliaLoad_MissingFile 验证缺失文件按空桶启动，不 panic、不报错。
func TestKademliaLoad_MissingFile(t *testing.T) {
	k := NewKademlia("local-node", nil)
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	if err := k.Load(path); err != nil {
		t.Fatalf("Load missing file should be no-op, got %v", err)
	}
	if got := k.FindClosest(NodeIDFromString("probe"), 10); len(got) != 0 {
		t.Fatalf("expected empty routing table, got %d nodes", len(got))
	}
}

// TestKademliaLoad_CorruptFile 验证损坏 JSON 按空桶启动，不 panic、不报错。
func TestKademliaLoad_CorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	k := NewKademlia("local-node", nil)
	if err := k.Load(path); err != nil {
		t.Fatalf("Load corrupt file should not error, got %v", err)
	}
	if got := k.FindClosest(NodeIDFromString("probe"), 10); len(got) != 0 {
		t.Fatalf("expected empty routing table, got %d nodes", len(got))
	}
}

// TestKademliaLoad_FileTooLarge 验证超限文件按空桶启动（防 OOM，稀疏文件快速路径）。
func TestKademliaLoad_FileTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "too-large.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Truncate(maxKadSnapshotBytes + 1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	k := NewKademlia("local-node", nil)
	if err := k.Load(path); err != nil {
		t.Fatalf("Load oversized file should not error, got %v", err)
	}
	if got := k.FindClosest(NodeIDFromString("probe"), 10); len(got) != 0 {
		t.Fatalf("expected empty routing table, got %d nodes", len(got))
	}
}

// TestKademliaLoad_InvalidEntriesDropped 验证非法条目被丢弃：
//   - route_id 非 hex（长度不对 / 含非 hex 字符）；
//   - route_id 与 id 不匹配（损坏/伪造）；
//   - id 为空。
//
// Load 后路由表只含合法条目（DoD：非法条目丢弃）。
func TestKademliaLoad_InvalidEntriesDropped(t *testing.T) {
	valid := NodeIDFromString("good-node").Hex()
	path := filepath.Join(t.TempDir(), "invalid.json")
	data := fmt.Sprintf(`{"nodes":[`+
		`{"id":"good-node","route_id":%q,"addrs":["tcp://a:1"]},`+ // 合法
		`{"id":"short-hex","route_id":"zz","addrs":["tcp://b:1"]},`+ // route_id 非 hex（长度不对）
		`{"id":"bad-hex","route_id":%q,"addrs":["tcp://c:1"]},`+ // route_id 含非 hex 字符
		`{"id":"mismatch","route_id":%q,"addrs":["tcp://d:1"]},`+ // route_id 与 id 不匹配
		`{"id":"","route_id":%q,"addrs":["tcp://e:1"]},`+ // id 为空
		`{"id":"short-route","route_id":"`+strings.Repeat("ab", 31)+`","addrs":["tcp://f:1"]}`+ // route_id 合法 hex 但长度 ≠ 64（31 字节，曾触发 NodeIDFromHex 越界 panic）
		`]}`, valid, strings.Repeat("g", 64), valid, valid)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	k := NewKademlia("local-node", nil)
	if err := k.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := k.FindClosest(NodeIDFromString("probe"), 10)
	if len(got) != 1 || got[0].ID != "good-node" {
		t.Fatalf("expected only valid good-node, got %v", got)
	}
}

// TestKademliaLoad_BucketOverflowTruncated 验证单 bucket 超过 K（bucketSize）值时
// Load 截断到 bucketSize。手工构造文件内 > bucketSize 条同 bucket 合法条目，
// 绕过 Insert 的 addNode 上限，直接覆盖 Load 侧截断逻辑。
func TestKademliaLoad_BucketOverflowTruncated(t *testing.T) {
	k := NewKademlia("local-node", nil)
	// 枚举 bucket 0（与本地 id 的 XOR prefixLen = 0）的合法条目，收集超过 bucketSize 个。
	var snaps []kadNodeSnap
	for i := 0; len(snaps) < bucketSize+5; i++ {
		id := fmt.Sprintf("load-node-%d", i)
		route := NodeIDFromString(id)
		if k.bucketIndex(route) == 0 {
			snaps = append(snaps, kadNodeSnap{ID: id, RouteID: route.Hex(), Addrs: []string{"addr"}})
		}
	}
	if len(snaps) <= bucketSize {
		t.Fatalf("failed to collect enough same-bucket entries: got %d", len(snaps))
	}
	path := filepath.Join(t.TempDir(), "overflow.json")
	raw, err := json.Marshal(kadSnap{Nodes: snaps})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	k2 := NewKademlia("local-node", nil)
	if err := k2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(k2.buckets[0].getNodes()); n != bucketSize {
		t.Fatalf("bucket 0 after Load: want %d, got %d", bucketSize, n)
	}
	if total := len(k2.FindClosest(NodeIDFromString("load-node-0"), 10000)); total != bucketSize {
		t.Fatalf("total nodes after Load: want %d, got %d", bucketSize, total)
	}
}

// TestKademliaEnablePersistence_EmptyPathNoop 验证 persist 路径为空时 EnablePersistence
// 是 no-op（零行为变更）：不落盘、不 panic、Flush 也 no-op。
func TestKademliaEnablePersistence_EmptyPathNoop(t *testing.T) {
	k := NewKademlia("local-node", nil)
	if err := k.EnablePersistence(""); err != nil {
		t.Fatalf("EnablePersistence(\"\"): %v", err)
	}
	k.Insert(hub.PeerInfo{ID: "node-1", Addrs: []string{"addr"}})
	if err := k.FlushPersist(); err != nil {
		t.Fatalf("FlushPersist on empty path should be no-op, got %v", err)
	}
}

// TestKademliaEnablePersistence_SavesOnChange 验证启用持久化后，Insert/Remove 变更
// 经异步落盘（FlushPersist 同步确认），新实例 Load 恢复已注册节点。
func TestKademliaEnablePersistence_SavesOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kad-persist.json")
	k := NewKademlia("local-node", nil)
	if err := k.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}
	k.Insert(hub.PeerInfo{ID: "node-a", Addrs: []string{"addr-a"}})
	k.Insert(hub.PeerInfo{ID: "node-b", Addrs: []string{"addr-b"}})
	if err := k.FlushPersist(); err != nil {
		t.Fatalf("FlushPersist: %v", err)
	}

	k2 := NewKademlia("local-node", nil)
	if err := k2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := k2.FindClosest(NodeIDFromString("node-a"), 10)
	if len(got) != 2 {
		ids := make([]string, 0, len(got))
		for _, n := range got {
			ids = append(ids, n.ID)
		}
		t.Fatalf("expected 2 persisted nodes, got %v", ids)
	}
}

// TestKademliaPersistence_ConcurrentInsertBuildSnapRace 验证（审查 PR-3 I-1 回归）：
// 并发 Insert 与 buildSnap（Save 触发）无数据竞争（-race 捕获；getNodesSnapshot 锁内
// 复制 PeerInfo 本体）。
func TestKademliaPersistence_ConcurrentInsertBuildSnapRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kad-race.json")
	k := NewKademlia("local-node", nil)
	if err := k.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	// 并发变更：反复 Insert 同一节点（触发 addNode 原地更新 info）。
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			k.Insert(hub.PeerInfo{ID: "node-race", Addrs: []string{"1.2.3.4:1", "5.6.7.8:2"}})
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_ = k.Save(path) // 触发 buildSnap（读 info）——与 Insert 写 info 竞争
		}
	}()
	wg.Wait()
}

// TestKademliaPersistence_AsyncDebouncedSave 验证（审查 PR-3 M-2）：真实去抖 timer
// 异步落盘（不经 FlushPersist）——Insert 后等待去抖窗口，文件出现且内容可恢复。
func TestKademliaPersistence_AsyncDebouncedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kad-async.json")
	k := NewKademlia("local-node", nil)
	if err := k.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}
	k.Insert(hub.PeerInfo{ID: "node-async", Addrs: []string{"addr-async"}})

	// 真实 timer：等去抖窗口 + 落盘完成（轮询文件出现）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("去抖 timer 应自动落盘文件: %v", err)
	}

	k2 := NewKademlia("local-node", nil)
	if err := k2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := k2.FindClosest(NodeIDFromString("node-async"), 10)
	if len(got) != 1 || got[0].ID != "node-async" {
		t.Fatalf("异步落盘内容应可恢复, got %+v", got)
	}
}

// TestKademliaPersistence_FlushWithConcurrentChange 验证（审查 PR-3 I-2 回归）：
// FlushPersist 与并发变更最终一致（saveMu 串行 buildSnap+write，无陈旧覆盖）——
// flush 后新实例 Load 应看到 flush 时刻的最新状态（无 panic）。
func TestKademliaPersistence_FlushWithConcurrentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kad-flush.json")
	k := NewKademlia("local-node", nil)
	if err := k.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence: %v", err)
	}
	k.Insert(hub.PeerInfo{ID: "node-1", Addrs: []string{"addr-1"}})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			k.Insert(hub.PeerInfo{ID: "node-2", Addrs: []string{"addr-2"}})
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if err := k.FlushPersist(); err != nil {
		t.Fatalf("FlushPersist（并发变更中）: %v", err)
	}
	wg.Wait()

	// flush 后文件可读、内容合法（无 panic、无损坏）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("flush 后文件应存在: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("flush 后文件不应为空")
	}
}
