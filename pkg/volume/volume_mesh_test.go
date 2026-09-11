// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"strings"
	"testing"
)

const testFP = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

func meshVol(mode Mode, owners []string, readers ...MeshReader) Volume {
	m := map[string]struct{}{}
	for _, o := range owners {
		m[o] = struct{}{}
	}
	return Volume{Name: "main", ACL: ACL{Mode: mode, Owners: m, MeshReaders: readers}}
}

func TestAuthorizeMeshRead_Matrix(t *testing.T) {
	hit := MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice"}
	cases := []struct {
		name            string
		vol             Volume
		node, fp, owner string
		want            bool
	}{
		{"三元组命中（deny 默认开放）", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "alice", true},
		{"allow 模式且 owner 在白名单", meshVol(ModeAllow, []string{"alice"}, hit), "nodeA", testFP, "alice", true},
		{"节点对但指纹错", meshVol(ModeDeny, nil, hit), "nodeA", "sha256:" + strings.Repeat("0", 64), "alice", false},
		{"节点错", meshVol(ModeDeny, nil, hit), "nodeB", testFP, "alice", false},
		{"owner 不匹配", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "bob", false},
		{"mesh_readers 为空", meshVol(ModeDeny, nil), "nodeA", testFP, "alice", false},
		{"allow 模式但 owner 不在白名单", meshVol(ModeAllow, []string{"bob"}, hit), "nodeA", testFP, "alice", false},
		{"deny 模式且 owner 在黑名单", meshVol(ModeDeny, []string{"alice"}, hit), "nodeA", testFP, "alice", false},
		{"指纹大小写/空白归一命中", meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: " " + strings.ToUpper(testFP) + " ", Owner: "alice"}), "nodeA", testFP, "alice", true},
		{"请求指纹带空白/大写命中", meshVol(ModeDeny, nil, hit), "nodeA", " " + strings.ToUpper(testFP) + " ", "alice", true},
		{"空 node 拒绝", meshVol(ModeDeny, nil, hit), "", testFP, "alice", false},
		{"空 owner 拒绝", meshVol(ModeDeny, nil, hit), "nodeA", testFP, "", false},
		{"空指纹拒绝", meshVol(ModeDeny, nil, hit), "nodeA", "", "alice", false},
		// 以下五条针对 volume.go 里的空值防线。为免归纳失真，逐条列出各自的变红条件
		// （实测全部 8 种守卫删除组合得出，非推算；复现：注释掉下述 A/B/C 后跑
		// `go test -count=1 -race -run TestAuthorizeMeshRead_Matrix ./pkg/volume/`）。
		// 删除点编号：
		//   A = AuthorizeMeshRead 的入参守卫 `node == "" || owner == "" || fingerprint == ""`
		//   B = fingerprintEqual 的 `a == ""`
		//   C = AuthorizeMeshRead 的归一化守卫 `want == ""`
		// 变红条件（minimal 删除集，逐条对应、勿归纳为分组）：
		//   "钉 node 空守卫"     → A 单删
		//   "钉 owner 空守卫"    → A 单删
		//   "空指纹两侧为空"     → A+B+C 同删
		//   "空白指纹两侧为空白" → B+C 同删
		//   "纯空白指纹拒绝"     → 任何单删/组合删都不红：它断言的是复合行为
		//                          「空白一律拒绝」，是 C 的意图载体而非探测器
		// 本表未列出的用例（各正例、节点错/owner 错、allow/deny 名单、大小写归一、未知 mode）
		// 均不参与上述任何变异。
		{"钉 node 空守卫", meshVol(ModeDeny, nil, MeshReader{Node: "", Fingerprint: testFP, Owner: "alice"}), "", testFP, "alice", false},
		{"钉 owner 空守卫", meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: ""}), "nodeA", testFP, "", false},
		// 空串指纹：由入参守卫（fingerprint == ""）兜住。
		{"空指纹两侧为空", meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: "", Owner: "alice"}), "nodeA", "", "alice", false},
		// 纯空白指纹（原始值非 ""，绕过入参守卫）：由归一化守卫（want == ""）兜住。
		{"纯空白指纹拒绝", meshVol(ModeDeny, nil, hit), "nodeA", "   ", "alice", false},
		// 两侧指纹同为空白串：断言的是复合行为「空白指纹一律拒绝」——归一化守卫（want == ""）
		// 与 fingerprintEqual 的 `a == ""` 互备，单独删任一道仍判否（冗余互备），两道同删才变红。
		{"空白指纹两侧为空白", meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: "   ", Owner: "alice"}), "nodeA", "   ", "alice", false},
		{"未知 mode fail-closed", Volume{Name: "main", ACL: ACL{Mode: Mode("bogus"), MeshReaders: []MeshReader{hit}}}, "nodeA", testFP, "alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vol.AuthorizeMeshRead(tc.node, tc.fp, tc.owner); got != tc.want {
				t.Fatalf("AuthorizeMeshRead(%q,%q,%q) = %v, want %v", tc.node, tc.fp, tc.owner, got, tc.want)
			}
		})
	}
}

func TestMeshReaderFor(t *testing.T) {
	other := MeshReader{Node: "nodeC", Fingerprint: "sha256:" + strings.Repeat("a", 64), Owner: "carol"}
	vol := meshVol(ModeDeny, nil, MeshReader{Node: "nodeA", Fingerprint: testFP, Owner: "alice"}, other)

	if mr, ok := vol.MeshReaderFor(testFP); !ok || mr.Node != "nodeA" || mr.Owner != "alice" {
		t.Fatalf("命中条目不符: %+v ok=%v", mr, ok)
	}
	if mr, ok := vol.MeshReaderFor(" " + strings.ToUpper(testFP) + " "); !ok || mr.Node != "nodeA" {
		t.Fatalf("归一后应命中: %+v ok=%v", mr, ok)
	}
	if _, ok := vol.MeshReaderFor("sha256:" + strings.Repeat("b", 64)); ok {
		t.Fatal("未列出的指纹不应命中")
	}
	if _, ok := vol.MeshReaderFor(""); ok {
		t.Fatal("空指纹不应命中")
	}
	if _, ok := meshVol(ModeDeny, nil).MeshReaderFor(testFP); ok {
		t.Fatal("空 mesh_readers 不应命中")
	}
}
