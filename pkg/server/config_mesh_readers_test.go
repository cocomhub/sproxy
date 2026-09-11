// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

const testReaderFP = "sha256:3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// withMeshReader 装配单卷 + 一条 mesh_readers 条目。
//
// 刻意用 Mode=deny（默认开放）且不设 Owners：既有的 Validate 会**先**校验 Owners 列表
// （config.go:770-779），若这里塞入非法 owner，报错会来自 Owners 而非 mesh_readers，
// 断言就测不到本任务新增的校验分支。
func withMeshReader(node, fp, owner string) func(*Config) {
	return func(c *Config) {
		c.Volumes = []VolumeConfig{{
			Name: "main", Root: c.StorageRoot,
			ACL: &VolumeACLConfig{
				Mode:        VolumeACLDeny,
				MeshReaders: []VolumeMeshReaderConfig{{Node: node, Fingerprint: fp, Owner: owner}},
			},
		}}
	}
}

func TestMeshReadersConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		node    string
		fp      string
		owner   string
		wantErr string
	}{
		{"合法", "nodeA", testReaderFP, "alice", ""},
		{"纯 64 hex 合法", "nodeA", strings.TrimPrefix(testReaderFP, "sha256:"), "alice", ""},
		{"大写 hex 合法", "nodeA", strings.ToUpper(testReaderFP), "alice", ""},
		{"指纹长度非法", "nodeA", "sha256:abc", "alice", "fingerprint 非法"},
		{"指纹含非 hex", "nodeA", "sha256:" + strings.Repeat("z", 64), "alice", "fingerprint 非法"},
		{"node 为空", "", testReaderFP, "alice", "node 不能为空"},
		{"owner 为空", "nodeA", testReaderFP, "", "owner 不能为空"},
		{"owner 含路径分隔符", "nodeA", testReaderFP, "a/b", "owner 非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.StorageRoot = t.TempDir()
			withMeshReader(tc.node, tc.fp, tc.owner)(cfg)
			cfg.SetDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMeshReadersConfig_DuplicateFingerprintRejected(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{
		Name: "main", Root: cfg.StorageRoot,
		ACL: &VolumeACLConfig{
			Mode: VolumeACLAllow,
			MeshReaders: []VolumeMeshReaderConfig{
				{Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice"},
				{Node: "nodeB", Fingerprint: strings.ToUpper(testReaderFP), Owner: "bob"},
			},
		},
	}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "指纹重复") {
		t.Fatalf("同一卷内指纹重复（归一后）应被拒绝, got %v", err)
	}
}

// TestMeshReadersConfig_SetDefaultsNormalizesFingerprint 锁定 SetDefaults 的归一职责：
// 大小写/首尾空白/可省前缀在**配置归一阶段**即规范化为 "sha256:<64 小写 hex>"；非法指纹
// 保持原样（绝不静默改写），由 Validate 响亮拒绝——这是畸形指纹的唯一防线。
func TestMeshReadersConfig_SetDefaultsNormalizesFingerprint(t *testing.T) {
	ok := Default()
	ok.StorageRoot = t.TempDir()
	withMeshReader("nodeA", "  "+strings.ToUpper(testReaderFP)+"  ", "alice")(ok)
	ok.SetDefaults()
	if got := ok.Volumes[0].ACL.MeshReaders[0].Fingerprint; got != testReaderFP {
		t.Fatalf("SetDefaults 应归一为规范形 %q, got %q", testReaderFP, got)
	}

	bad := Default()
	bad.StorageRoot = t.TempDir()
	withMeshReader("nodeA", "sha256:abc", "alice")(bad)
	bad.SetDefaults()
	if got := bad.Volumes[0].ACL.MeshReaders[0].Fingerprint; got != "sha256:abc" {
		t.Fatalf("非法指纹在 SetDefaults 应保持原样（交由 Validate 拒绝）, got %q", got)
	}
}

func TestMeshReadersConfig_ParseVolumeACL(t *testing.T) {
	acl := parseVolumeACL(&VolumeACLConfig{
		Mode:        VolumeACLAllow,
		Owners:      []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{{Node: "nodeA", Fingerprint: strings.ToUpper(testReaderFP), Owner: "alice"}},
	})
	if len(acl.MeshReaders) != 1 {
		t.Fatalf("mesh_readers 应解析出 1 条, got %d", len(acl.MeshReaders))
	}
	mr := acl.MeshReaders[0]
	if mr.Node != "nodeA" || mr.Owner != "alice" {
		t.Fatalf("条目不符: %+v", mr)
	}
	if mr.Fingerprint != testReaderFP {
		t.Fatalf("指纹应归一为规范形 %q, got %q", testReaderFP, mr.Fingerprint)
	}
	if !(volume.Volume{Name: "main", ACL: acl}).AuthorizeMeshRead("nodeA", testReaderFP, "alice") {
		t.Fatal("解析后的 ACL 应放行 nodeA/alice 只读")
	}
}
