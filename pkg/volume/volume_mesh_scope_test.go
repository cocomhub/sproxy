// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// volume_mesh_scope_test.go 钉住 mesh 授权的 **scope 轴**（Y 二期 P3；规格 §5.7）：
//
//   - 三值 `read` / `write` / `rw`，**缺省 read**（零值 = 零回归，老配置不带 scope 仍是只读）；
//   - **读不隐含写、写不隐含读**（`scope: write` 的条目不得只读访问，反之亦然）；
//   - 未知 scope 值 **fail-closed**（读写都拒）——绝不「不认识就当 read」；
//   - 第二重约束不变：owner 仍须过本卷 ACL（`Authorize`）；
//   - 归一化：去空白 + 转小写（" READ " ≡ read）。

import (
	"strings"
	"testing"
)

// TestAuthorizeMeshScope_Matrix 穷举 scope 轴 × 第二重约束 × 入参守卫。
func TestAuthorizeMeshScope_Matrix(t *testing.T) {
	entry := func(scope string) MeshReader {
		return MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice", Scope: scope}
	}

	cases := []struct {
		name      string
		vol       Volume
		node      string
		fp        string
		owner     string
		wantRead  bool
		wantWrite bool
	}{
		// ---- scope 轴 ----
		{"零值 scope 视为 read（零回归）", meshVol(ModeDeny, nil, entry("")), "nodeA", testFP, "alice", true, false},
		{"scope=read：只读", meshVol(ModeDeny, nil, entry(MeshScopeRead)), "nodeA", testFP, "alice", true, false},
		{"scope=write：只写（写不隐含读）", meshVol(ModeDeny, nil, entry(MeshScopeWrite)), "nodeA", testFP, "alice", false, true},
		{"scope=rw：读写", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", testFP, "alice", true, true},
		{"scope 归一：空白+大写", meshVol(ModeDeny, nil, entry(" "+strings.ToUpper(MeshScopeRead)+" ")), "nodeA", testFP, "alice", true, false},
		{"scope 归一：空白+大写 rw", meshVol(ModeDeny, nil, entry(" RW ")), "nodeA", testFP, "alice", true, true},
		{"scope 纯空白 ≡ 未配 ≡ read", meshVol(ModeDeny, nil, entry("   ")), "nodeA", testFP, "alice", true, false},

		// ---- 未知 scope：fail-closed（读写都拒）----
		{"未知 scope=rwx fail-closed", meshVol(ModeDeny, nil, entry("rwx")), "nodeA", testFP, "alice", false, false},
		{"未知 scope=readwrite fail-closed", meshVol(ModeDeny, nil, entry("readwrite")), "nodeA", testFP, "alice", false, false},
		{"未知 scope=* fail-closed", meshVol(ModeDeny, nil, entry("*")), "nodeA", testFP, "alice", false, false},

		// ---- 第二重约束：owner 须过本卷 ACL ----
		{"allow 模式 owner 在白名单（rw）", meshVol(ModeAllow, []string{"alice"}, entry(MeshScopeRW)), "nodeA", testFP, "alice", true, true},
		{"allow 模式 owner 不在白名单（rw）", meshVol(ModeAllow, []string{"bob"}, entry(MeshScopeRW)), "nodeA", testFP, "alice", false, false},
		{"deny 模式 owner 在黑名单（rw）", meshVol(ModeDeny, []string{"alice"}, entry(MeshScopeRW)), "nodeA", testFP, "alice", false, false},
		{"未知 mode fail-closed（rw）", Volume{Name: "main", ACL: ACL{Mode: Mode("bogus"), MeshReaders: []MeshReader{entry(MeshScopeRW)}}}, "nodeA", testFP, "alice", false, false},

		// ---- 三元组匹配（对写同样适用）----
		{"节点对但指纹错（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", "sha256:" + strings.Repeat("0", 64), "alice", false, false},
		{"节点错（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeB", testFP, "alice", false, false},
		{"owner 不匹配（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", testFP, "bob", false, false},
		{"mesh_readers 为空（write）", meshVol(ModeDeny, nil), "nodeA", testFP, "alice", false, false},

		// ---- 入参守卫 ----
		{"空 node（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "", testFP, "alice", false, false},
		{"空 owner（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", testFP, "", false, false},
		{"空指纹（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", "", "alice", false, false},
		{"纯空白指纹（rw）", meshVol(ModeDeny, nil, entry(MeshScopeRW)), "nodeA", "   ", "alice", false, false},

		// ---- 多条目：按 (node, owner) 命中各自的 scope ----
		{"同节点同 owner 只读条目先行（write 不借读条目放行）", meshVol(ModeDeny, nil, entry(MeshScopeRead), MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice", Scope: MeshScopeWrite}), "nodeA", testFP, "alice", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vol.AuthorizeMeshRead(tc.node, tc.fp, tc.owner); got != tc.wantRead {
				t.Fatalf("AuthorizeMeshRead(%q,%q,%q) = %v, want %v", tc.node, tc.fp, tc.owner, got, tc.wantRead)
			}
			if got := tc.vol.AuthorizeMeshWrite(tc.node, tc.fp, tc.owner); got != tc.wantWrite {
				t.Fatalf("AuthorizeMeshWrite(%q,%q,%q) = %v, want %v", tc.node, tc.fp, tc.owner, got, tc.wantWrite)
			}
		})
	}
}

// TestMeshReaderFor_CarriesScope 钉住指纹反查返回的条目**带 scope**：B 侧写 listener 需要
// 「先反查 (node, owner)，再按 scope 判定」，若反查丢 scope 则写授权必然退化。
func TestMeshReaderFor_CarriesScope(t *testing.T) {
	w := MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice", Scope: MeshScopeWrite}
	vol := meshVol(ModeDeny, nil, w)

	mr, ok := vol.MeshReaderFor(testFP)
	if !ok {
		t.Fatal("应命中")
	}
	if mr.Scope != MeshScopeWrite {
		t.Fatalf("MeshReaderFor 应保留 scope, got %q want %q", mr.Scope, MeshScopeWrite)
	}
}
