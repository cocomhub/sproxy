// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"log/slog"
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
	}, nil)
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

// TestMeshReadersConfig_ParseVolumeACL_NormalizesFingerprint 表驱动钉住 parseVolumeACL 的
// 指纹归一：接受纯 hex / 大小写 / 可省前缀 / 首尾空白，输出恒为规范形 "sha256:<64 小写 hex>"。
//
// 该归一化是「畸形指纹唯一防线」的下游一环：pkg/volume 的 normalizeFingerprint 只做
// Trim+ToLower、不校验 sha256: 前缀与 64 位 hex，配置写错格式只会**静默永不命中**
// （授权被拒但无任何报错）。故这条链路值得单独钉住，而非只靠「大写+带前缀」一种输入。
func TestMeshReadersConfig_ParseVolumeACL_NormalizesFingerprint(t *testing.T) {
	hexOnly := strings.TrimPrefix(testReaderFP, "sha256:")
	cases := []struct {
		name string
		in   string
	}{
		{"纯 64 hex（无前缀）", hexOnly},
		{"大写 hex（无前缀）", strings.ToUpper(hexOnly)},
		{"大写 hex + 大写前缀 SHA256:", "SHA256:" + strings.ToUpper(hexOnly)},
		{"首尾带空白", "  " + testReaderFP + "\t"},
		{"已规范形（幂等）", testReaderFP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl := parseVolumeACL(&VolumeACLConfig{
				Mode:        VolumeACLAllow,
				Owners:      []string{"alice"},
				MeshReaders: []VolumeMeshReaderConfig{{Node: "nodeA", Fingerprint: tc.in, Owner: "alice"}},
			}, nil)
			if len(acl.MeshReaders) != 1 {
				t.Fatalf("合法指纹 %q 不应被丢弃, got %d 条", tc.in, len(acl.MeshReaders))
			}
			if got := acl.MeshReaders[0].Fingerprint; got != testReaderFP {
				t.Fatalf("输入 %q 应归一为规范形 %q, got %q", tc.in, testReaderFP, got)
			}
		})
	}
}

// TestMeshReadersConfig_ParseVolumeACL_DropsMalformed 钉住装配层 fail-closed：畸形指纹被
// **丢弃该条目**而非降级保留（降级保留一个语义不明的字符串会在未来改动中被误用），且丢弃
// **不静默**——每条被丢弃的条目都留 Warn 日志（本项目「禁止静默失败」原则）。
//
// 断言对应审查要求：① 畸形条目不在结果里；② 不 panic（直接调 parseVolumeACL，不经
// Config.Validate——本函数在生产路径上畸形指纹已被 Validate 响亮拒绝，此处是装配层兜底；
// 若实现改为 panic/fatal 本用例即失败）；③ 同批合法条目仍在 → 证明是「丢弃单条」而非
// 「整体清空」；④ 恰好 2 条 Warn（3 条中 2 条畸形）且含 mesh_readers/指纹非法关键词 →
// 既证明留下痕迹，也证明**合法条目不产生噪音告警**。
func TestMeshReadersConfig_ParseVolumeACL_DropsMalformed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	notHex := "sha256:" + strings.Repeat("z", 64) // 长度合法但非 hex
	acl := parseVolumeACL(&VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{
			{Node: "nodeA", Fingerprint: "sha256:abc", Owner: "alice"}, // 长度非法 → 丢弃
			{Node: "nodeB", Fingerprint: testReaderFP, Owner: "alice"}, // 合法 → 必须保留
			{Node: "nodeC", Fingerprint: notHex, Owner: "alice"},       // 非 hex → 丢弃
		},
	}, log)

	// ①③ 丢弃是对的：只保留合法条目，且是 nodeB 而非整体清空。
	if len(acl.MeshReaders) != 1 {
		t.Fatalf("畸形指纹应被丢弃、合法条目应保留（期望 1 条）, got %d 条: %+v", len(acl.MeshReaders), acl.MeshReaders)
	}
	got := acl.MeshReaders[0]
	if got.Node != "nodeB" || got.Owner != "alice" {
		t.Fatalf("保留的应是合法条目 nodeB/alice, got %+v", got)
	}
	if got.Fingerprint != testReaderFP {
		t.Fatalf("保留条目指纹应为规范形 %q, got %q", testReaderFP, got.Fingerprint)
	}

	// ④ 丢弃不是静默的：恰好 2 条 Warn（两条畸形各一条），且带可检索关键词。
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("丢弃畸形指纹必须留 Warn 日志，实际无任何日志输出（静默失败）")
	}
	records := strings.Split(out, "\n")
	if len(records) != 2 {
		t.Fatalf("应为 2 条 Warn（每条畸形指纹一条，合法条目不告警）, got %d 条: %s", len(records), out)
	}
	for _, rec := range records {
		if !strings.Contains(rec, "丢弃 mesh_readers 条目") || !strings.Contains(rec, "指纹非法") {
			t.Fatalf("告警缺少可检索关键词, got %s", rec)
		}
		if !strings.Contains(rec, `"node"`) || !strings.Contains(rec, `"owner"`) {
			t.Fatalf("告警应携带 node/owner 便于定位条目, got %s", rec)
		}
	}
}

// ---- Y 二期 P3：scope 轴（规格 §5.7）----

// withMeshReaderScope 装配单卷 + 一条带 scope 的 mesh_readers 条目。
func withMeshReaderScope(node, fp, owner, scope string) func(*Config) {
	return func(c *Config) {
		c.Volumes = []VolumeConfig{{
			Name: "main", Root: c.StorageRoot,
			ACL: &VolumeACLConfig{
				Mode: VolumeACLDeny,
				MeshReaders: []VolumeMeshReaderConfig{
					{Node: node, Fingerprint: fp, Owner: owner, Scope: scope},
				},
			},
		}}
	}
}

// TestMeshReadersConfig_ScopeValidate 钉住 scope 的**加载期校验**（fail-fast）：
// 三值 + 空（= read）放行；未知值必须**响亮拒绝**（否则运行期会 fail-closed 静默拒绝，
// 配上「我明明配了写权限」的困惑——畸形配置必须在启动时就报错）。
func TestMeshReadersConfig_ScopeValidate(t *testing.T) {
	cases := []struct {
		name    string
		scope   string
		wantErr string // 空 = 期望通过
	}{
		{"缺省（空）= read", "", ""},
		{"read", volume.MeshScopeRead, ""},
		{"write", volume.MeshScopeWrite, ""},
		{"rw", volume.MeshScopeRW, ""},
		{"大小写不敏感 READ", "READ", ""},
		{"大小写不敏感 Rw", "Rw", ""},
		{"未知 rwx 拒绝", "rwx", "scope"},
		{"未知 readwrite 拒绝", "readwrite", "scope"},
		{"通配 * 拒绝", "*", "scope"},
		// 纯空白 ≡ 未配 ≡ read（与空值同一规则；两层同一归一化，避免「配置层拒绝/判定层当 read」的双标准）。
		{"纯空白 = 缺省 read", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			withMeshReaderScope("nodeA", testReaderFP, "alice", tc.scope)(cfg)
			cfg.SetDefaults()
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("scope=%q 应通过校验, got %v", tc.scope, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("scope=%q 应被拒绝", tc.scope)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestMeshReadersConfig_SetDefaultsCanonicalizesScope 钉住 SetDefaults 把 scope 归一为
// **规范小写形**（与指纹归一同一职责）：这样装配层与授权判定拿到的是同一形态，
// 也让「配置里写 READ」和「写 read」在后续比较中不可能分叉。
func TestMeshReadersConfig_SetDefaultsCanonicalizesScope(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", volume.MeshScopeRead},
		{" read ", volume.MeshScopeRead},
		{"READ", volume.MeshScopeRead},
		{"RW", volume.MeshScopeRW},
		{" Write ", volume.MeshScopeWrite},
		// 未知值保持原样，交由 Validate 响亮拒绝（与指纹归一同一策略）。
		{"rwx", "rwx"},
	}
	for _, tc := range cases {
		cfg := &Config{}
		withMeshReaderScope("nodeA", testReaderFP, "alice", tc.in)(cfg)
		cfg.SetDefaults()
		got := cfg.Volumes[0].ACL.MeshReaders[0].Scope
		if got != tc.want {
			t.Fatalf("scope %q 归一为 %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMeshReadersConfig_ParseVolumeACL_CarriesScope 钉住装配层把 scope 透传进
// `volume.MeshReader`——写 listener 的授权判定只认后者的 Scope 字段，透传丢失等于
// 「配了 write 但永远写不了」（fail-closed 方向的安全，功能全失效）。
func TestMeshReadersConfig_ParseVolumeACL_CarriesScope(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", volume.MeshScopeRead},
		{volume.MeshScopeRead, volume.MeshScopeRead},
		{volume.MeshScopeWrite, volume.MeshScopeWrite},
		{volume.MeshScopeRW, volume.MeshScopeRW},
	}
	for _, tc := range cases {
		acl := parseVolumeACL(&VolumeACLConfig{
			Mode: VolumeACLDeny,
			MeshReaders: []VolumeMeshReaderConfig{
				{Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice", Scope: tc.in},
			},
		}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

		if len(acl.MeshReaders) != 1 {
			t.Fatalf("应保留 1 条条目, got %+v", acl.MeshReaders)
		}
		if got := acl.MeshReaders[0].Scope; got != tc.want {
			t.Fatalf("装配后 scope=%q want %q（须把配置的 scope 透传进 volume.MeshReader）", got, tc.want)
		}
		// 授权语义随之生效（read 只授读、write 只授写、rw 授两者）。
		vol := volume.Volume{Name: "main", ACL: acl}
		wantRead := tc.want == volume.MeshScopeRead || tc.want == volume.MeshScopeRW
		wantWrite := tc.want == volume.MeshScopeWrite || tc.want == volume.MeshScopeRW
		if got := vol.AuthorizeMeshRead("nodeA", testReaderFP, "alice"); got != wantRead {
			t.Fatalf("scope=%q 读授权=%v want %v", tc.want, got, wantRead)
		}
		if got := vol.AuthorizeMeshWrite("nodeA", testReaderFP, "alice"); got != wantWrite {
			t.Fatalf("scope=%q 写授权=%v want %v", tc.want, got, wantWrite)
		}
	}
}

// TestMeshReadersConfig_ParseVolumeACLDropsInvalidScope 钉住装配层对**未知 scope** 的兜底：
// 丢弃该条目并留告警（与畸形指纹同策略）。生产路径上 Validate 已响亮拒绝，此处仅是纵深防御——
// 「降级保留一个语义不明的权限范围」是最危险的选项（未来改动可能把它当有权限使用）。
func TestMeshReadersConfig_ParseVolumeACLDropsInvalidScope(t *testing.T) {
	var buf bytes.Buffer
	acl := parseVolumeACL(&VolumeACLConfig{
		Mode: VolumeACLDeny,
		MeshReaders: []VolumeMeshReaderConfig{
			{Node: "bad", Fingerprint: testReaderFP, Owner: "alice", Scope: "rwx"},
			{Node: "good", Fingerprint: testReaderFP, Owner: "alice", Scope: volume.MeshScopeWrite},
		},
	}, slog.New(slog.NewTextHandler(&buf, nil)))

	if len(acl.MeshReaders) != 1 || acl.MeshReaders[0].Node != "good" {
		t.Fatalf("非法 scope 的条目应被丢弃、合法条目保留: %+v", acl.MeshReaders)
	}
	if !strings.Contains(buf.String(), "scope") {
		t.Fatalf("丢弃条目必须留痕（告警含 scope）, got %q", buf.String())
	}
	// 丢弃后不得残留任何授权（fail-closed）。
	vol := volume.Volume{Name: "main", ACL: acl}
	if vol.AuthorizeMeshWrite("bad", testReaderFP, "alice") {
		t.Fatal("被丢弃的条目不得残留写授权")
	}
}
