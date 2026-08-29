// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// testSnap 构建一个覆盖两个 mesh、三个节点（含服务与 secret）的 Snapshot。
func testSnap() *Snapshot {
	return &Snapshot{
		Nodes: []NodeSnap{
			{ID: "node-a", Mesh: "mesh-a", Addr: "1.2.3.4:22", Secret: "sec-a", RealNodeID: "", Connected: time.Unix(1700000000, 0), Services: []Service{{Name: "svc-a", Addr: "a:22"}}},
			{ID: "node-b", Mesh: "mesh-b", Addr: "5.6.7.8:22", Secret: "sec-b", RealNodeID: "", Connected: time.Unix(1700000100, 0), Services: []Service{{Name: "svc-b1", Addr: "b1:22"}, {Name: "svc-b2", Addr: "b2:80"}}},
			{ID: "disc-node-c-abcdef", Mesh: "mesh-a", Addr: "", Secret: "sec-c", RealNodeID: "node-c", Connected: time.Unix(1700000200, 0), Services: nil},
		},
		Messages: []MessageSnap{
			{Peer: "node-a", Msgs: []SignalMsg{{ID: "m1", Kind: SignalOffer, From: "node-b", To: "node-a", SDP: "v=0", At: time.Now().UnixMilli()}}},
			{Peer: "node-b", Msgs: []SignalMsg{{ID: "m2", Kind: SignalCandidate, From: "node-a", To: "node-b", Cand: "ice://x", At: time.Now().UnixMilli()}}},
		},
	}
}

// TestPersister_SaveLoadRoundTrip：Save→Load 往返恢复节点+服务+secret 与消息。
func TestPersister_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)

	want := testSnap()
	if err := p.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil {
		t.Fatal("Load 返回 nil，期望读到快照")
	}
	if !reflect.DeepEqual(got.Nodes, want.Nodes) {
		t.Fatalf("节点快照不一致:\n got=%+v\nwant=%+v", got.Nodes, want.Nodes)
	}
	if !reflect.DeepEqual(got.Messages, want.Messages) {
		t.Fatalf("消息快照不一致:\n got=%+v\nwant=%+v", got.Messages, want.Messages)
	}
}

// TestSnapshotRestore_MultiMesh：快照含两个 mesh 的节点，恢复进空表后
// Has/LookupInfo/MeshOf/服务宣告均在；Lookup（在线连接）为 nil（重启后等待重连）。
func TestSnapshotRestore_MultiMesh(t *testing.T) {
	src := NewMeshRouteTable()
	muxA := newTestMux(t)
	muxB := newTestMux(t)
	muxC := newTestMux(t)
	src.Add("mesh-a", NodeInfo{ID: "node-a", Mux: muxA, Connected: time.Unix(1700000000, 0), Secret: "sec-a"}, []Service{{Name: "svc-a", Addr: "a:22"}})
	src.Add("mesh-b", NodeInfo{ID: "node-b", Mux: muxB, Connected: time.Unix(1700000100, 0), Secret: "sec-b"}, []Service{{Name: "svc-b1", Addr: "b1:22"}, {Name: "svc-b2", Addr: "b2:80"}})
	src.Add("mesh-a", NodeInfo{ID: "disc-node-c-abcdef", Mux: muxC, Connected: time.Unix(1700000200, 0), Secret: "sec-c", RealNodeID: "node-c"}, nil)
	// 不注册 remove hook，避免 Purge 干扰。

	snap := SnapshotRouteTable(src)
	if len(snap.Nodes) != 3 {
		t.Fatalf("SnapshotRouteTable nodes = %d, want 3", len(snap.Nodes))
	}

	dst := NewMeshRouteTable()
	RestoreFromSnapshot(dst, snap)

	// 注册身份保留（离线恢复，无 mux）
	for _, id := range []NodeID{"node-a", "node-b", "disc-node-c-abcdef"} {
		if !dst.Has(id) {
			t.Fatalf("恢复后 %q 应 Has == true", id)
		}
		info, ok := dst.LookupInfo(id)
		if !ok {
			t.Fatalf("恢复后 %q LookupInfo 应存在", id)
		}
		if info.Mux != nil {
			t.Fatalf("恢复后 %q 的 Mux 应为 nil（离线待重连）", id)
		}
	}
	if got := dst.MeshOf("node-a"); got != "mesh-a" {
		t.Fatalf("MeshOf(node-a) = %q, want mesh-a", got)
	}
	if got := dst.MeshOf("node-b"); got != "mesh-b" {
		t.Fatalf("MeshOf(node-b) = %q, want mesh-b", got)
	}

	// 服务宣告与 secret 保留。
	if svcs := dst.ListServices("mesh-a"); len(svcs) != 1 || svcs[0].Node != "node-a" || svcs[0].Service.Name != "svc-a" {
		t.Fatalf("ListServices(mesh-a) = %+v, want node-a/svc-a", svcs)
	}
	nfoA, _ := dst.LookupInfo("node-a")
	if nfoA.Secret != "sec-a" {
		t.Fatalf("node-a secret = %q, want sec-a", nfoA.Secret)
	}
	if nfoB, _ := dst.LookupInfo("node-b"); nfoB.Mesh != "mesh-b" {
		t.Fatalf("node-b Mesh = %q, want mesh-b", nfoB.Mesh)
	}
	if nfoC, _ := dst.LookupInfo("disc-node-c-abcdef"); nfoC.RealNodeID != "node-c" {
		t.Fatalf("disc 节点恢复后 RealNodeID = %q, want node-c", nfoC.RealNodeID)
	}
}

// TestSnapshotSignalQueue_RoundTrip：信令收件箱快照往返，恢复后 Pop 取回同消息。
func TestSnapshotSignalQueue_RoundTrip(t *testing.T) {
	q := NewSignalQueue()
	in := []SignalMsg{
		{ID: "m1", Kind: SignalOffer, From: "node-b", To: "node-a", SDP: "v=0"},
		{ID: "m2", Kind: SignalCandidate, From: "node-a", To: "node-b", Cand: "ice://x"},
		{ID: "m3", Kind: SignalAnswer, From: "node-b", To: "node-a", SDP: "v=1"},
	}
	for _, m := range in {
		if err := q.Push(m); err != nil {
			t.Fatalf("Push: %v", err)
		}
	}

	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	msgs := SnapshotSignalQueue(q)
	if len(msgs) != 2 {
		t.Fatalf("SnapshotSignalQueue 收件箱数 = %d, want 2", len(msgs))
	}
	if err := p.Save(&Snapshot{Messages: msgs}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dst := NewSignalQueue()
	RestoreSignalQueue(dst, snap.Messages)

	// node-a 收件箱：m1(offer) 与 m3(answer) 按入队顺序（Push 时 ID/At 由队列生成）。
	m1 := dst.Pop("node-a")
	m3 := dst.Pop("node-a")
	if m1 == nil || m1.Kind != SignalOffer || m1.From != "node-b" || m1.SDP != "v=0" {
		t.Fatalf("恢复后 Pop(node-a) 第 1 次 = %+v, want offer(v=0)", m1)
	}
	if m3 == nil || m3.Kind != SignalAnswer || m3.SDP != "v=1" {
		t.Fatalf("恢复后 Pop(node-a) 第 2 次 = %+v, want answer(v=1)", m3)
	}
	if m := dst.Pop("node-a"); m != nil {
		t.Fatalf("node-a 收件箱应只剩 2 条消息，多出 %+v", m)
	}
	if m := dst.Pop("node-b"); m == nil || m.Kind != SignalCandidate || m.Cand != "ice://x" {
		t.Fatalf("Pop(node-b) 应取回 candidate，got %+v", m)
	}
	if m := dst.Pop("node-b"); m != nil {
		t.Fatal("node-b 收件箱应只剩 1 条消息")
	}
}

// TestPersister_LoadMissingOrCorrupt：文件不存在返回空快照、无错误；
// 损坏/非法 JSON 返回空快照、无错误（记 warn，不 panic）——hub 启动
// 不因持久化文件损坏而失败，也不让错误直接崩掉进程。
func TestPersister_LoadMissingOrCorrupt(t *testing.T) {
	dir := t.TempDir()

	// 不存在 → 空快照，无错误
	p := NewPersister(filepath.Join(dir, "ghost.json"))
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load(不存在) err = %v, want nil", err)
	}
	if snap == nil || len(snap.Nodes) != 0 || len(snap.Messages) != 0 {
		t.Fatalf("Load(不存在) = %+v, want 空快照", snap)
	}

	// 空文件 → 空快照，无错误，不 panic
	empty := filepath.Join(dir, "empty.json")
	if err = os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("写空文件: %v", err)
	}
	snap, err = NewPersister(empty).Load()
	if err != nil {
		t.Fatalf("Load(空文件) err = %v, want nil（记 warn + 空快照）", err)
	}
	if snap == nil {
		t.Fatal("Load(空文件) 应返回非 nil 空快照")
	}

	// 非法 JSON（非 Snapshot 结构）→ 空快照，无错误，不 panic
	bad := filepath.Join(dir, "bad.json")
	if err = os.WriteFile(bad, []byte("{\"oops\": {not json"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}
	snap, err = NewPersister(bad).Load()
	if err != nil {
		t.Fatalf("Load(非法 JSON) err = %v, want nil（记 warn + 空快照）", err)
	}
	if snap == nil {
		t.Fatal("Load(非法 JSON) 应返回非 nil 空快照")
	}
}

// TestPersister_AtomicWrite：父目录不存在时 Save 报错；成功后目录无残留临时文件。
func TestPersister_AtomicWrite(t *testing.T) {
	dir := t.TempDir()

	// 父目录不存在 → 报错，不 panic。
	p := NewPersister(filepath.Join(dir, "nope", "x.json"))
	if err := p.Save(testSnap()); err == nil {
		t.Fatal("Save 到不存在父目录应返回 error")
	}

	// 正常保存：原子写后目录里只有目标文件（无 .tmp 残留）。
	good := filepath.Join(dir, "hub.json")
	p2 := NewPersister(good)
	if err := p2.Save(testSnap()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "hub.json" {
		t.Fatalf("目录残留 %d 个条目，want 仅 hub.json: %+v", len(entries), entries)
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("保存内容非法 JSON: %v", err)
	}
}

// TestPersister_ScheduleCoalesces：同一去抖窗口内多次 Schedule 合并为一次落盘。
func TestPersister_ScheduleCoalesces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	p.debounce = time.Hour // 超大 debounce，确保异步 timer 不触发，行为完全由 Flush 断言
	writes := 0
	// 第一次 Schedule 落入窗口；第二次覆盖它（返回 nil）。
	p.Schedule(func() *Snapshot {
		writes++
		s := testSnap()
		s.Nodes = s.Nodes[:1]
		return s
	})
	p.Schedule(func() *Snapshot { return nil })
	// Flush(nil)：pending 的闭包（第二次，返回 nil）被执行，无落盘 → false。
	// 第一次的闭包（自增 writes）在同一窗口内被覆盖、从未执行 → writes == 0，验证合并。
	if p.Flush(nil) {
		t.Fatal("Flush(nil) 应返回 false（最后一次排队的快照返回 nil）")
	}
	if writes != 0 {
		t.Fatalf("快照生成函数执行次数 = %d, want 0（第一次被覆盖，未执行）", writes)
	}
	// FlushFn 显式快照生成器：立即同步执行。
	if !p.FlushFn(testSnap) {
		t.Fatal("FlushFn(非 nil) 应返回 true")
	}
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if snap == nil || len(snap.Nodes) != 3 {
		t.Fatalf("FlushFn 后应落盘完整快照，got %+v", snap)
	}
}

// TestSnapshotRouteTable_MissingMux：未注册 mux 的节点（info.Mux nil）也能正确快照。
func TestSnapshotRouteTable_MissingMux(t *testing.T) {
	mrt := NewMeshRouteTable()
	mrt.Add("", NodeInfo{ID: "ghost", Secret: "xyz"}, nil) // 无 mux 亦注册
	snap := SnapshotRouteTable(mrt)
	if len(snap.Nodes) != 1 || snap.Nodes[0].ID != "ghost" || snap.Nodes[0].Secret != "xyz" {
		t.Fatalf("SnapshotRouteTable = %+v", snap.Nodes)
	}
	dst := NewMeshRouteTable()
	RestoreFromSnapshot(dst, snap)
	if !dst.Has("ghost") {
		t.Fatal("恢复后 ghost 应 Has")
	}
	if _, ok := dst.LookupInfo("ghost"); !ok {
		t.Fatal("恢复后 ghost LookupInfo 应 ok")
	}
}

// TestMeshRouteTable_PersistOnAddAndRemove：SetOnChange 在 Add 与 Remove 后都
// 触发持久化——Add 后新 Persister Load 能读到节点（含 secret）；Remove 后
// 重新 Load 节点已消失（不会把已下线的节点当幽灵注册恢复到重启后）。
func TestMeshRouteTable_PersistOnAddAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	mrt := NewMeshRouteTable()
	mrt.SetOnChange(func() {
		p.Schedule(func() *Snapshot { return SnapshotRouteTable(mrt) })
	})

	// Add → 落盘
	mrt.Add("mesh-a", NodeInfo{ID: "node-a", Secret: "sec-a", Addr: "1.2.3.4:22"}, []Service{{Name: "svc-a", Addr: "a:22"}})
	if !p.Flush(nil) {
		t.Fatal("Add 后 Flush 应落盘")
	}
	p2 := NewPersister(path)
	snap, err := p2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].ID != "node-a" || snap.Nodes[0].Secret != "sec-a" {
		t.Fatalf("Add 后快照节点 = %+v, want node-a/sec-a", snap.Nodes)
	}

	// Remove → 重新落盘，节点消失
	if !mrt.Remove("node-a") {
		t.Fatal("Remove 应成功")
	}
	if !p.Flush(nil) {
		t.Fatal("Remove 后 Flush 应落盘")
	}
	snap2, err := p2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap2.Nodes) != 0 {
		t.Fatalf("Remove 后快照应无节点, got %+v", snap2.Nodes)
	}
}

// TestPersist_RestartSimulation：注册节点并落盘 → 全新路由表 + 全新 Persister
// 从文件加载 → Has/LookupInfo（含 secret/mesh）与服务宣告恢复，Mux 为 nil
// （离线待重连）。
func TestPersist_RestartSimulation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")

	// 第一代：注册节点并落盘
	p1 := NewPersister(path)
	mrt1 := NewMeshRouteTable()
	mrt1.SetOnChange(func() {
		p1.Schedule(func() *Snapshot { return SnapshotRouteTable(mrt1) })
	})
	mrt1.Add("mesh-a", NodeInfo{ID: "node-a", Secret: "sec-a", Addr: "1.2.3.4:22"}, []Service{{Name: "svc-a", Addr: "a:22"}})
	if !p1.Flush(nil) {
		t.Fatal("Flush 应落盘")
	}

	// 模拟重启：全新路由表 + 全新 Persister，从文件加载恢复
	p2 := NewPersister(path)
	snap, err := p2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	mrt2 := NewMeshRouteTable()
	RestoreFromSnapshot(mrt2, snap)
	if !mrt2.Has("node-a") {
		t.Fatal("重启恢复后 node-a 应 Has == true")
	}
	if info, ok := mrt2.LookupInfo("node-a"); !ok || info.Secret != "sec-a" || info.Mesh != "mesh-a" {
		t.Fatalf("重启恢复后 node-a 信息 = %+v, ok=%v, want secret=sec-a mesh=mesh-a", info, ok)
	}
	if mrt2.Lookup("node-a") != nil {
		t.Fatal("重启恢复后 node-a 的 Mux 应为 nil（离线待重连）")
	}
	if svcs := mrt2.ListServices("mesh-a"); len(svcs) != 1 || svcs[0].Service.Name != "svc-a" {
		t.Fatalf("重启恢复后服务宣告 = %+v, want svc-a", svcs)
	}
}

// TestReconnectAfterRestore_NoPanic：重启恢复（Mux==nil 的离线节点）后，同名节点
// 重连触发注册，修复前 `go old.Close()` 对 nil Mux 解引用在独立 goroutine 中 panic、
// 直接崩溃进程（C1）。修复后应无 panic 且新 Mux 生效。
func TestReconnectAfterRestore_NoPanic(t *testing.T) {
	// 完整链路：注册 → 快照 → 恢复（离线）→ 重连（MeshRouteTable.Add → AddWithInfoAndServices）。
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("重连注册 panic: %v", r)
			}
		}()
		src := NewMeshRouteTable()
		src.Add("mesh-a", NodeInfo{ID: "node-a", Mux: newTestMux(t), Connected: time.Now(), Secret: "sec-a"}, []Service{{Name: "svc-a", Addr: "a:22"}})
		snap := SnapshotRouteTable(src)

		dst := NewMeshRouteTable()
		RestoreFromSnapshot(dst, snap)
		if m := dst.Lookup("node-a"); m != nil {
			t.Fatal("恢复后 node-a 的 Mux 应为 nil（离线待重连）")
		}
		// 重连：非 nil mux 覆盖离线占位（nil）。修复前此处 go nil.Close() panic。
		dst.Add("mesh-a", NodeInfo{ID: "node-a", Mux: newTestMux(t), Connected: time.Now(), Secret: "sec-a2"}, []Service{{Name: "svc-a", Addr: "a:22"}})
		if m := dst.Lookup("node-a"); m == nil {
			t.Fatal("重连后 node-a 应有非 nil Mux")
		}
	}()

	// 底层 RouteTable 三个注册方法逐一验证：nodes[id] 为 nil（离线占位）时用非 nil
	// mux 重注册不得 panic。
	rt := NewRouteTable()
	rt.info["node-x"] = NodeInfo{ID: "node-x", Secret: "sec-x"}
	for _, tc := range []struct {
		name string
		fn   func(m *mux.Mux)
	}{
		{"Add", func(m *mux.Mux) { rt.Add("node-x", m) }},
		{"AddWithInfo", func(m *mux.Mux) { rt.AddWithInfo(NodeInfo{ID: "node-x", Mux: m}) }},
		{"AddWithInfoAndServices", func(m *mux.Mux) { rt.AddWithInfoAndServices(NodeInfo{ID: "node-x", Mux: m}, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s 重注册 panic: %v", tc.name, r)
				}
			}()
			rt.nodes["node-x"] = nil // 重置为离线占位（模拟快照恢复的 nil Mux）
			tc.fn(newTestMux(t))
			if rt.nodes["node-x"] == nil {
				t.Fatalf("%s 后 node-x 应有非 nil Mux", tc.name)
			}
		})
	}
}

// TestPersister_ConcurrentSaveFlushSerializes：并发 Save/FlushFn/Flush/Load 共享
// 同一 Persister 时序列化，不 panic、无数据竞争（-race 通过）；最终文件必为某次
// 完整快照（原子写保证），不会因并发写者交错产生半写或整体丢失。I1 修复方向是
// 让快照生成 + 落盘处于同一临界区，本测试验证序列化与原子写不变量。
func TestPersister_ConcurrentSaveFlushSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	p.debounce = time.Hour // 关闭异步 timer，全部走同步路径

	const workers = 8
	const iters = 30

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := range iters {
				id := fmt.Sprintf("w%d-%d", n, j)
				snap := &Snapshot{
					Nodes:    []NodeSnap{{ID: NodeID(id), Mesh: "m"}},
					Messages: []MessageSnap{{Peer: "p", Msgs: []SignalMsg{{ID: id}}}},
				}
				switch j % 3 {
				case 0:
					if err := p.Save(snap); err != nil {
						t.Errorf("Save: %v", err)
						return
					}
				case 1:
					if !p.FlushFn(func() *Snapshot { return snap }) {
						t.Errorf("FlushFn 应落盘")
						return
					}
				default:
					p.Flush(snap)
				}
			}
		}(i)
	}
	// 并发的 Load 读者（Load 不持 p.mu，与写者共享文件，验证原子 rename 下不读到半写）。
	wg.Go(func() {
		for range iters {
			if _, err := p.Load(); err != nil {
				t.Errorf("Load: %v", err)
				return
			}
		}
	})
	wg.Wait()

	// 最终文件必须完整可解码（原子写 + 序列化保证不产生半写文件）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("最终文件非法 JSON: %v", err)
	}
}

// compile-time guards：确认 mux/xfertest 引用不因重构丢失。
var (
	_ = mux.RoleDialer
	_ = xfertest.Pipe
)
