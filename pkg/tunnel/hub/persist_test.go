// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
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
// 注意：比较**数据值**而非内部表示——time.Time 经 JSON round-trip 后 loc（*Location）
// 指针可能不同（CI UTC 环境 time.Unix 的 Local vs 解析的 UTC），reflect.DeepEqual 对
// 含 unexported loc 字段的 time.Time 会误报不等。故节点逐字段比较、时间用 Equal()。
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
	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("节点数不一致: got=%d want=%d", len(got.Nodes), len(want.Nodes))
	}
	for i := range want.Nodes {
		w, g := want.Nodes[i], got.Nodes[i]
		if w.ID != g.ID || w.Mesh != g.Mesh || w.Addr != g.Addr ||
			w.Secret != g.Secret || w.RealNodeID != g.RealNodeID {
			t.Fatalf("节点[%d] 元信息不一致:\n got=%+v\nwant=%+v", i, g, w)
		}
		if !w.Connected.Equal(g.Connected) {
			t.Fatalf("节点[%d] Connected 不一致: got=%v want=%v", i, g.Connected, w.Connected)
		}
		if !reflect.DeepEqual(g.Services, w.Services) {
			t.Fatalf("节点[%d] 服务不一致:\n got=%+v\nwant=%+v", i, g.Services, w.Services)
		}
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("消息数不一致: got=%d want=%d", len(got.Messages), len(want.Messages))
	}
	for i := range want.Messages {
		w, g := want.Messages[i], got.Messages[i]
		if w.Peer != g.Peer {
			t.Fatalf("消息[%d] peer 不一致: got=%q want=%q", i, g.Peer, w.Peer)
		}
		if len(g.Msgs) != len(w.Msgs) {
			t.Fatalf("消息[%d] 条数不一致: got=%d want=%d", i, len(g.Msgs), len(w.Msgs))
		}
		for j := range w.Msgs {
			wm, gm := w.Msgs[j], g.Msgs[j]
			if wm.ID != gm.ID || wm.Kind != gm.Kind || wm.From != gm.From ||
				wm.To != gm.To || wm.SDP != gm.SDP || wm.Cand != gm.Cand {
				t.Fatalf("消息[%d][%d] 内容不一致:\n got=%+v\nwant=%+v", i, j, gm, wm)
			}
			if wm.At != gm.At {
				t.Fatalf("消息[%d][%d] At 不一致: got=%d want=%d", i, j, gm.At, wm.At)
			}
		}
	}
}

// TestSnapshotRestore_MultiMesh：快照含两个 mesh 的节点，恢复进空表后
// Has/LookupInfo/MeshOf/服务宣告均在；Lookup（在线连接）为 nil（重启后等待重连）。
// M2：disc-* mesh 自动对等发现的临时节点**不持久化**（重启后不会重连、无人在用，
// 持久化只会留下永久 nil-Mux 幽灵条目），故快照只含 node-a/node-b 两个真实节点。
func TestSnapshotRestore_MultiMesh(t *testing.T) {
	src := NewMeshRouteTable()
	muxA := newTestMux(t)
	muxB := newTestMux(t)
	muxC := newTestMux(t)
	src.Add("mesh-a", NodeInfo{ID: "node-a", Mux: muxA, Connected: time.Unix(1700000000, 0), Secret: "sec-a"}, []Service{{Name: "svc-a", Addr: "a:22"}})
	src.Add("mesh-b", NodeInfo{ID: "node-b", Mux: muxB, Connected: time.Unix(1700000100, 0), Secret: "sec-b"}, []Service{{Name: "svc-b1", Addr: "b1:22"}, {Name: "svc-b2", Addr: "b2:80"}})
	// M2：disc- 临时发现节点（在线时真实 Mux，但属于临时身份）——应被快照过滤。
	src.Add("mesh-a", NodeInfo{ID: "disc-node-c-abcdef", Mux: muxC, Connected: time.Unix(1700000200, 0), Secret: "sec-c", RealNodeID: "node-c"}, nil)
	// 不注册 remove hook，避免 Purge 干扰。

	snap := SnapshotRouteTable(src)
	if len(snap.Nodes) != 2 {
		t.Fatalf("SnapshotRouteTable nodes = %d, want 2（disc- 临时节点应被过滤，M2）", len(snap.Nodes))
	}

	dst := NewMeshRouteTable()
	RestoreFromSnapshot(dst, snap)

	// 注册身份保留（离线恢复，无 mux）——仅真实节点。
	for _, id := range []NodeID{"node-a", "node-b"} {
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
	// M2：disc- 临时节点不应进入恢复后的路由表。
	if dst.Has("disc-node-c-abcdef") {
		t.Fatal("恢复后 disc- 临时节点不应存在（M2）")
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
	// M2：disc- 临时节点已被快照过滤，恢复表中不存在 → 无 RealNodeID 可查。
	if _, ok := dst.LookupInfo("disc-node-c-abcdef"); ok {
		t.Fatal("恢复后 disc- 临时节点不应存在（M2 过滤）")
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
	// Flush(nil)：pending 的闭包（第二次，返回 nil）被执行，无落盘 → nil（无错误，
	// 也无需落盘）。第一次的闭包（自增 writes）在同一窗口内被覆盖、从未执行 →
	// writes == 0，验证合并。
	if err := p.Flush(nil); err != nil {
		t.Fatalf("Flush(nil) 应返回 nil（无 pending 可落盘），got %v", err)
	}
	if writes != 0 {
		t.Fatalf("快照生成函数执行次数 = %d, want 0（第一次被覆盖，未执行）", writes)
	}
	// FlushFn 显式快照生成器：立即同步执行。
	if err := p.FlushFn(testSnap); err != nil {
		t.Fatalf("FlushFn(非 nil) 应落盘无错，got %v", err)
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
	if err := p.Flush(nil); err != nil {
		t.Fatalf("Add 后 Flush 应落盘无错，got %v", err)
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
	if err = p.Flush(nil); err != nil {
		t.Fatalf("Remove 后 Flush 应落盘无错，got %v", err)
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
	if err := p1.Flush(nil); err != nil {
		t.Fatalf("Flush 应落盘无错，got %v", err)
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
					if err := p.FlushFn(func() *Snapshot { return snap }); err != nil {
						t.Errorf("FlushFn 应落盘无错: %v", err)
						return
					}
				default:
					_ = p.Flush(snap)
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

// TestSnapshotSignalQueue_FiltersExpired（M3）：快照只保留未过期消息——
// 队列的惰性过期只在 Push/Peek/Pop 时发生，空 poll 不触发；若镜像复制会把
// 已过期消息残留在持久化文件里直到下次写盘。这里验证 SnapshotSignalQueue
// 直接按 TTL 过滤，任何持久化镜像都不含过期死信。
func TestSnapshotSignalQueue_FiltersExpired(t *testing.T) {
	q := NewSignalQueue()
	now := time.Now()
	if err := q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "fresh", At: now.UnixMilli()}); err != nil {
		t.Fatalf("Push fresh: %v", err)
	}
	// 人为制造一条已过期消息（At 远早于 TTL）——直接写 inbox，绕过 Push 的
	// compactExpiredLocked（Push 会当场清理过期，无法放入过期消息）。
	stale := SignalMsg{Kind: SignalAnswer, From: "a", To: "b", SDP: "stale", At: now.Add(-2 * signalMsgTTL).UnixMilli()}
	q.mu.Lock()
	q.inboxes["b"] = append(q.inboxes["b"], stale)
	q.total++
	q.mu.Unlock()

	snaps := SnapshotSignalQueue(q)
	if len(snaps) != 1 || len(snaps[0].Msgs) != 1 {
		t.Fatalf("SnapshotSignalQueue = %+v, want 仅 1 条未过期消息（M3 过滤过期）", snaps)
	}
	if got := snaps[0].Msgs[0].SDP; got != "fresh" {
		t.Fatalf("快照消息 = %q, want fresh（过期消息应被过滤）", got)
	}
}

// TestPersister_LoadOversizedFile（M6）：快照文件超出 maxSnapshotBytes 时
// Load 视为损坏，返回空快照且不 panic（拒绝整体读入内存，防启动 OOM）。
func TestPersister_LoadOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	// 构造超过上限的文件：不实际写 maxSnapshotBytes+1 字节（磁盘/内存开销），
	// 用一个略大于上限的稀疏文件即可——Size() 判定只看 stat。
	if err := os.WriteFile(path, []byte("{\"nodes\":["), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用 Truncate 制造稀疏大文件，避免真实写巨量数据。
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err = f.Truncate(maxSnapshotBytes + 1); err != nil {
		_ = f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	_ = f.Close()

	p := NewPersister(path)
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load(超大文件) err = %v, want nil（记 warn + 空快照）", err)
	}
	if snap == nil || len(snap.Nodes) != 0 {
		t.Fatalf("Load(超大文件) = %+v, want 空快照", snap)
	}
}

// TestPersister_SaveSets0600（M7）：落盘后文件权限应为 0600——
// per-node secret 等敏感信息不应被同机其他用户读取。
// Unix 上 CreateTemp 已建 0600，显式 Chmod 二次确认；Windows 上 Chmod
// 只影响只读位（平台固有），本测试仅校验 Unix 语义（跳过 Windows）。
func TestPersister_SaveSets0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 Chmod 不反映 Unix 权限语义，ACL 由系统继承")
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	snap := &Snapshot{Nodes: []NodeSnap{{ID: "node-a", Secret: "sec-a"}}}
	if err := p.FlushFn(func() *Snapshot { return snap }); err != nil {
		t.Fatalf("FlushFn: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("快照文件权限 = %o, want 600（secret 不应被同机其他用户读取，M7）", got)
	}
}

// TestPersister_ScheduleDebounceRealTimer：用真实去抖计时器（短 debounce）验证
// time.AfterFunc + Reset 的相互配合——多次 Schedule 落在同一去抖窗口内时，只有
// **最后一次**排队的闭包在窗口到期后落盘（中间状态被合并/覆盖），磁盘上最终是
// 最新状态。与 TestPersister_ScheduleCoalesces（用 time.Hour 关停异步 timer，
// 全同步路径断言合并）互补：本测试走真实 timer 路径。
func TestPersister_ScheduleDebounceRealTimer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	p := NewPersister(path)
	p.debounce = 50 * time.Millisecond

	// 三次 Schedule，间距短于 debounce：窗口内密集变更应合并为最后一次落盘。
	p.Schedule(func() *Snapshot { return &Snapshot{Nodes: []NodeSnap{{ID: "v1"}}} })
	time.Sleep(10 * time.Millisecond)
	p.Schedule(func() *Snapshot { return &Snapshot{Nodes: []NodeSnap{{ID: "v2"}}} })
	time.Sleep(10 * time.Millisecond)
	p.Schedule(func() *Snapshot { return &Snapshot{Nodes: []NodeSnap{{ID: "v3"}}} })

	// 等待去抖窗口过去（timer 触发并异步落盘）。轮询直到 v3 出现在磁盘上，
	// 避免固定 sleep 在慢机器/CI 上 flake。
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, err := p.Load()
		if err == nil && snap != nil && len(snap.Nodes) == 1 && snap.Nodes[0].ID == "v3" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("去抖 timer 未把 latest 快照落盘: snap=%+v err=%v", snap, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 最终磁盘内容就是 latest（v3），且不应残留中间状态节点。
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].ID != "v3" {
		t.Fatalf("落盘快照 = %+v, want 仅 v3（latest 覆盖，去抖合并）", snap.Nodes)
	}
}

// TestRestoreSignalQueue_FiltersExpired（M3 恢复路径）：恢复含过期消息的收件箱时，
// 过期消息被丢弃（不重投递），且不计入 q.total——否则重启后 q.total 高估、占用
// 全局配额直到下次惰性清理。
func TestRestoreSignalQueue_FiltersExpired(t *testing.T) {
	q := NewSignalQueue()
	now := time.Now()
	msgs := []MessageSnap{
		{
			Peer: "node-a",
			Msgs: []SignalMsg{
				{ID: "expired", Kind: SignalOffer, From: "node-b", To: "node-a", SDP: "stale", At: now.Add(-2 * signalMsgTTL).UnixMilli()},
				{ID: "fresh", Kind: SignalAnswer, From: "node-b", To: "node-a", SDP: "live", At: now.UnixMilli()},
			},
		},
	}
	RestoreSignalQueue(q, msgs)

	if got := q.Total(); got != 1 {
		t.Fatalf("RestoreSignalQueue 后 total = %d, want 1（过期消息不计入）", got)
	}
	// Pop 只取回未过期消息；过期死信不重投递。
	m := q.Pop("node-a")
	if m == nil || m.ID != "fresh" || m.SDP != "live" {
		t.Fatalf("Pop 应返回 fresh 消息，got %+v", m)
	}
	if m := q.Pop("node-a"); m != nil {
		t.Fatalf("Pop 应返回 nil（只剩过期消息被丢弃），got %+v", m)
	}
	if got := q.Total(); got != 0 {
		t.Fatalf("消费后 total = %d, want 0", got)
	}
}

// TestRestoreSignalQueue_OverCapGraceful：恢复超过单 peer 上限（maxSignalInbox）
// 的收件箱——RestoreSignalQueue 恢复路径有意不强制收紧 cap（避免启动时丢消息），
// 断言：不 panic、收件箱可用（可 Pop）、过期条目被丢弃、total 不被膨胀（只计
// 未过期消息，验证 Minor #3 修复）。
func TestRestoreSignalQueue_OverCapGraceful(t *testing.T) {
	q := NewSignalQueue()
	now := time.Now()

	// 超过 maxSignalInbox 的未过期消息 + 混入一条过期消息。
	fresh := make([]SignalMsg, 0, maxSignalInbox+16)
	for i := range maxSignalInbox + 16 {
		fresh = append(fresh, SignalMsg{
			ID:   fmt.Sprintf("f%d", i),
			Kind: SignalOffer,
			From: "node-b",
			To:   "node-a",
			SDP:  "sdp",
			At:   now.UnixMilli(),
		})
	}
	msgs := []MessageSnap{
		{
			Peer: "node-a",
			Msgs: append(fresh,
				SignalMsg{ID: "expired", Kind: SignalAnswer, From: "node-b", To: "node-a", SDP: "stale", At: now.Add(-2 * signalMsgTTL).UnixMilli()},
			),
		},
	}

	// 不应 panic。
	RestoreSignalQueue(q, msgs)

	// 过期条目被丢弃、total 只计未过期消息（不被膨胀）。
	if got := q.Total(); got != len(fresh) {
		t.Fatalf("RestoreSignalQueue 后 total = %d, want %d（过期不计入）", got, len(fresh))
	}
	// 恢复的收件箱可用：能取回第一条未过期消息；全量取完应为空。
	for i := 0; i < len(fresh); i++ {
		if m := q.Pop("node-a"); m == nil {
			t.Fatalf("Pop 第 %d 次应取回消息", i)
		}
	}
	if m := q.Pop("node-a"); m != nil {
		t.Fatalf("收件箱应已取空，多出 %+v", m)
	}
	if got := q.Total(); got != 0 {
		t.Fatalf("全部消费后 total = %d, want 0", got)
	}
}

// TestNodeSnap_VirtualIP_RoundTrip 校验带虚拟 IP 的 NodeSnap 经 Save→Load 往返后
// VirtualIP 保留（omitzero 序列化正确）。
func TestNodeSnap_VirtualIP_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub-vip.json")
	p := NewPersister(path)
	want := &Snapshot{
		Nodes: []NodeSnap{
			{ID: "node-a", Mesh: "mesh-a", VirtualIP: netip.MustParseAddr("100.64.0.2"), Connected: time.Unix(1700000000, 0)},
			{ID: "node-b", Mesh: "mesh-a", Connected: time.Unix(1700000100, 0)},
		},
	}
	if err := p.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Nodes) != 2 {
		t.Fatalf("节点数 = %d, want 2", len(got.Nodes))
	}
	if got.Nodes[0].VirtualIP != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("node-a VirtualIP = %v, want 100.64.0.2", got.Nodes[0].VirtualIP)
	}
	if got.Nodes[1].VirtualIP.IsValid() {
		t.Fatalf("node-b 未分配虚拟 IP 应省略（无效 Addr），got %v", got.Nodes[1].VirtualIP)
	}
}

// TestSnapshotRestore_VirtualIP_Preserved 校验快照恢复后节点虚拟 IP 保留在路由表。
func TestSnapshotRestore_VirtualIP_Preserved(t *testing.T) {
	src := NewMeshRouteTable()
	src.Add("mesh-a", NodeInfo{ID: "node-a", VirtualIP: netip.MustParseAddr("100.64.0.2"), Connected: time.Now()}, nil)
	snap := SnapshotRouteTable(src)
	if len(snap.Nodes) != 1 || snap.Nodes[0].VirtualIP != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("快照应含虚拟 IP, got %+v", snap.Nodes)
	}

	dst := NewMeshRouteTable()
	RestoreFromSnapshot(dst, snap)
	info, ok := dst.LookupInfo("node-a")
	if !ok {
		t.Fatal("node-a 恢复后应存在")
	}
	if info.VirtualIP != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("恢复后 VirtualIP = %v, want 100.64.0.2", info.VirtualIP)
	}
}

// TestPreloadAllocator_Rebuild（DoD 1）校验重启快照重建分配表：
// 已持久化的虚拟 IP 被保留，新节点不占用已保留地址。
func TestPreloadAllocator_Rebuild(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	snap := &Snapshot{
		Nodes: []NodeSnap{
			{ID: "node-a", Mesh: "mesh-a", VirtualIP: netip.MustParseAddr("100.64.0.2")},
			{ID: "node-b", Mesh: "mesh-a", VirtualIP: netip.MustParseAddr("100.64.0.3")},
		},
	}
	if err := PreloadAllocator(a, snap); err != nil {
		t.Fatalf("PreloadAllocator: %v", err)
	}
	// 已保留节点重连复用旧地址。
	got, err := a.Alloc("mesh-a", "node-a")
	if err != nil {
		t.Fatalf("Alloc(node-a): %v", err)
	}
	if got != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("node-a 重连漂移: got %v, want 100.64.0.2", got)
	}
	// 新节点不占用已持久化地址。
	vn, err := a.Alloc("mesh-a", "node-c")
	if err != nil {
		t.Fatalf("Alloc(node-c): %v", err)
	}
	if vn == netip.MustParseAddr("100.64.0.2") || vn == netip.MustParseAddr("100.64.0.3") {
		t.Fatalf("新节点分配到已保留地址 %v", vn)
	}
}
