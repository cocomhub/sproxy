// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_derived_coverage_test.go 补 config 包内**纯派生/小工具**函数的边界分支：
// ValidateIPList / IsLoopbackHost / ParseMaintenanceWindow / parseHHMM / VolumeByName /
// validEnvName / TransferConcurrencyEffective / OwnerQuotaFor / VolumeRetentionConfig /
// MeshNodeID / 指纹派生（Mesh*Fingerprints / ReadListenerPins / scopeGrantsWrite）/
// cluster 段 Validate。这些函数没有装配态，全部可在并行用例里直接调用。
package config

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
)

func TestValidateIPList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{"空列表通过", nil, nil, ""},
		{"CIDR 原样保留", []string{"10.0.0.0/8", "2001:db8::/32"}, []string{"10.0.0.0/8", "2001:db8::/32"}, ""},
		{"IPv4 归一 /32", []string{"127.0.0.1"}, []string{"127.0.0.1/32"}, ""},
		{"IPv6 归一 /128", []string{"::1"}, []string{"::1/128"}, ""},
		{"空条目拒绝", []string{"  "}, nil, "为空条目"},
		{"非法条目拒绝", []string{"not-an-ip"}, nil, "非法"},
		{"首条归一后第二条失败", []string{"10.1.2.3", "bad"}, nil, "非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list := append([]string(nil), tc.in...)
			err := ValidateIPList(list, "test_ips")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过, got %v", err)
				}
				if len(list) != len(tc.want) {
					t.Fatalf("长度不符: got %v, want %v", list, tc.want)
				}
				for i := range tc.want {
					if list[i] != tc.want[i] {
						t.Fatalf("第 %d 条归一不符: got %q, want %q", i, list[i], tc.want[i])
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		want bool
	}{
		{"", false},
		{"localhost", true},
		{"LocalHost", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"::1", true},
		{"[::1]", true},
		{"10.0.0.1", false},
		{"example.com", false},
		{"not-an-ip", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackHost(tc.host); got != tc.want {
			t.Fatalf("IsLoopbackHost(%q)=%v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestParseMaintenanceWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		w    MaintenanceWindowConfig
		// checks 为空表示 ParseMaintenanceWindow 应返回 nil（恒执行）。
		checks []windowCheck
	}{
		{name: "未启用返回 nil"},
		{name: "start 为空返回 nil", w: MaintenanceWindowConfig{Enabled: true, End: "06:00"}},
		{name: "end 为空返回 nil", w: MaintenanceWindowConfig{Enabled: true, Start: "22:00"}},
		{name: "start 非法返回 nil", w: MaintenanceWindowConfig{Enabled: true, Start: "25:00", End: "06:00"}},
		{name: "end 非法返回 nil", w: MaintenanceWindowConfig{Enabled: true, Start: "22:00", End: "6:0"}},
		{
			name: "全天窗口（start==end）恒在窗口内",
			w:    MaintenanceWindowConfig{Enabled: true, Start: "08:00", End: "08:00"},
			checks: []windowCheck{
				{0, 0, true}, {8, 0, true}, {23, 59, true},
			},
		},
		{
			name: "同日窗口 [start,end)",
			w:    MaintenanceWindowConfig{Enabled: true, Start: "02:00", End: "04:30"},
			checks: []windowCheck{
				{1, 59, false}, {2, 0, true}, {4, 29, true}, {4, 30, false},
			},
		},
		{
			name: "跨午夜窗口 t>=start 或 t<end",
			w:    MaintenanceWindowConfig{Enabled: true, Start: "22:00", End: "06:00"},
			checks: []windowCheck{
				{21, 59, false}, {22, 0, true}, {5, 59, true}, {6, 0, false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := ParseMaintenanceWindow(SchedulerConfig{MaintenanceWindow: tc.w})
			if tc.checks == nil {
				if fn != nil {
					t.Fatal("期望返回 nil（恒执行）")
				}
				return
			}
			if fn == nil {
				t.Fatal("期望返回判定函数")
			}
			for _, ck := range tc.checks {
				ts := time.Date(2026, 1, 2, ck.hour, ck.minute, 0, 0, time.UTC)
				if got := fn(ts); got != ck.want {
					t.Fatalf("fn(%02d:%02d)=%v, want %v", ck.hour, ck.minute, got, ck.want)
				}
			}
		})
	}
}

// windowCheck 是一个时刻 + 期望的「在窗口内」判定。
type windowCheck struct {
	hour, minute int
	want         bool
}

func TestParseHHMM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		h, m   int
		wantOK bool
	}{
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{"24:00", 0, 0, false},
		{"12:60", 0, 0, false},
		{"1:00", 0, 0, false},
		{"", 0, 0, false},
		// 非数字字节但 ASCII 算术落在合法区间：必须由逐字符数字校验拦下。
		{"1<:00", 0, 0, false},
		{"ab:cd", 0, 0, false},
	}
	for _, tc := range cases {
		h, m, ok := parseHHMM(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("parseHHMM(%q) ok=%v, want %v", tc.in, ok, tc.wantOK)
		}
		if ok && (h != tc.h || m != tc.m) {
			t.Fatalf("parseHHMM(%q)=(%d,%d), want (%d,%d)", tc.in, h, m, tc.h, tc.m)
		}
	}
}

func TestVolumeByName(t *testing.T) {
	t.Parallel()
	c := &Config{Volumes: []VolumeConfig{{Name: "main", Root: "/a"}, {Name: "cold", Root: "/b"}}}
	if v, ok := c.VolumeByName("cold"); !ok || v.Root != "/b" {
		t.Fatalf("VolumeByName(cold)=(%+v,%v)", v, ok)
	}
	if _, ok := c.VolumeByName("missing"); ok {
		t.Fatal("未知卷名应返回 false")
	}
}

func TestValidEnvName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"A", true},
		{"_A1", true},
		{"VAULT_TOKEN", true},
		{"1A", false},
		{"A-B", false},
		{"A B", false},
		{"中文", false},
	}
	for _, tc := range cases {
		if got := validEnvName(tc.in); got != tc.want {
			t.Fatalf("validEnvName(%q)=%v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTransferConcurrencyEffective(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		transfer int
		maxConc  int
		want     int
	}{
		{"未配置且上限为零回落实质默认 3", 0, 0, 3},
		{"未配置回落 cloud_max_concurrent", 0, 5, 5},
		{"负值回落 cloud_max_concurrent", -2, 2, 2},
		{"显式值原样返回", 7, 5, 7},
	}
	for _, tc := range cases {
		c := &Config{CloudTransferConcurrency: tc.transfer, CloudMaxConcurrent: tc.maxConc}
		if got := TransferConcurrencyEffective(c); got != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestOwnerQuotaFor(t *testing.T) {
	t.Parallel()
	if got := (&Config{}).OwnerQuotaFor("alice"); got != 0 {
		t.Fatalf("nil map 应返回 0, got %d", got)
	}
	c := &Config{OwnerQuotas: map[string]ByteSize{"alice": 100, "*": 7}}
	if got := c.OwnerQuotaFor("alice"); got != 100 {
		t.Fatalf("显式 owner 配额 got %d, want 100", got)
	}
	if got := c.OwnerQuotaFor("bob"); got != 7 {
		t.Fatalf("wildcard 配额 got %d, want 7", got)
	}
	if got := (&Config{OwnerQuotas: map[string]ByteSize{}}).OwnerQuotaFor("bob"); got != 0 {
		t.Fatalf("空 map 应返回 0, got %d", got)
	}
}

func TestVolumeRetentionConfig_Enabled(t *testing.T) {
	t.Parallel()
	if (VolumeRetentionConfig{}).Enabled() {
		t.Fatal("全零 retention 不应报告启用")
	}
	for _, r := range []VolumeRetentionConfig{
		{VersionTTL: time.Hour},
		{ShareTTL: time.Hour},
		{AuditTTL: time.Hour},
	} {
		if !r.Enabled() {
			t.Fatalf("任一 TTL>0 应报告启用: %+v", r)
		}
	}
}

func TestVolumeRetentionConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		r       VolumeRetentionConfig
		wantErr string
	}{
		{"全零通过", VolumeRetentionConfig{}, ""},
		{"仅 TTL 通过", VolumeRetentionConfig{VersionTTL: time.Hour, GCInterval: time.Minute}, ""},
		{"version_ttl 负值拒绝", VolumeRetentionConfig{VersionTTL: -time.Second}, "version_ttl"},
		{"share_ttl 负值拒绝", VolumeRetentionConfig{ShareTTL: -time.Second}, "share_ttl"},
		{"audit_ttl 负值拒绝", VolumeRetentionConfig{AuditTTL: -time.Second}, "audit_ttl"},
		{"gc_interval 负值拒绝", VolumeRetentionConfig{GCInterval: -time.Second}, "gc_interval"},
		{"gc_interval 无 TTL 拒绝", VolumeRetentionConfig{GCInterval: time.Minute}, "空转任务"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.r.validate("main")
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

func TestMeshNodeID_FallbackOrder(t *testing.T) {
	t.Parallel()
	c := &Config{}
	c.Hub.NodeID = "hub-node"
	if got := c.MeshNodeID(); got != "hub-node" {
		t.Fatalf("回落 hub.node_id got %q", got)
	}
	c.Mesh.NodeID = "mesh-node"
	if got := c.MeshNodeID(); got != "mesh-node" {
		t.Fatalf("回落 mesh.node_id got %q", got)
	}
	c.Mesh.Node.NodeID = "node-role"
	if got := c.MeshNodeID(); got != "node-role" {
		t.Fatalf("node.node_id 优先 got %q", got)
	}
}

func TestStateStoreConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		c       StateStoreConfig
		wantErr string
	}{
		{"空 type 通过", StateStoreConfig{}, ""},
		{"local 通过", StateStoreConfig{Type: "local"}, ""},
		{"raft 未实现拒绝", StateStoreConfig{Type: "raft"}, "raft 未实现"},
		{"mongo 缺 uri 拒绝", StateStoreConfig{Type: "mongo"}, "mongo.uri"},
		{"mongo 带 uri 通过", StateStoreConfig{Type: "mongo", Mongo: MongoConfig{URI: "mongodb://127.0.0.1:27017"}}, ""},
		{"未知 type 拒绝", StateStoreConfig{Type: "bogus"}, "无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.c.Validate()
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

func TestClusterConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		c       ClusterConfig
		wantErr string
	}{
		{"未启用通过", ClusterConfig{}, ""},
		{"启用缺 node_id 拒绝", ClusterConfig{Enabled: true}, "node_id"},
		{"启用 master 通过", ClusterConfig{Enabled: true, NodeID: "n1", Role: ClusterRoleMaster}, ""},
		{"启用 replica 通过", ClusterConfig{Enabled: true, NodeID: "n1", Role: ClusterRoleReplica}, ""},
		{"启用非法 role 拒绝", ClusterConfig{Enabled: true, NodeID: "n1", Role: "leader"}, "role"},
		{"启用负 resync 拒绝", ClusterConfig{Enabled: true, NodeID: "n1", Role: ClusterRoleMaster, ResyncInterval: -time.Second}, "index_resync_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.c.Validate()
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

func TestMeshReaderFingerprints(t *testing.T) {
	t.Parallel()
	cfg := &Config{Volumes: []VolumeConfig{
		{Name: "no-acl"},
		{Name: "empty-fp", ACL: &VolumeACLConfig{MeshReaders: []VolumeMeshReaderConfig{
			{Node: "n0", Fingerprint: "   "},
		}}},
		{Name: "main", ACL: &VolumeACLConfig{MeshReaders: []VolumeMeshReaderConfig{
			{Node: "n1", Fingerprint: strings.ToUpper(testReaderFP)},
			{Node: "n2", Fingerprint: testReaderFP},
			{Node: "n3", Fingerprint: "  SHA256:NOTHEX  "},
		}}},
	}}
	got := MeshReaderFingerprints(cfg)
	want := []string{testReaderFP, "sha256:nothex"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestReadListenerPins_IncludesClusterCredentials(t *testing.T) {
	t.Parallel()
	const otherFP = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	encoded, err := clustercred.Credential{
		Node: "n", Volume: "v", Owner: "o",
		Recipient: otherFP, Scope: "read", IssuedAt: 1, ExpiresAt: 2,
	}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cfg := &Config{
		Volumes: []VolumeConfig{{Name: "main", ACL: &VolumeACLConfig{MeshReaders: []VolumeMeshReaderConfig{
			{Node: "n1", Fingerprint: testReaderFP},
		}}}},
		Cluster: ClusterConfig{Credentials: []ClusterCredentialConfig{
			{Encoded: encoded},
			{Encoded: encoded},            // 重复 Recipient 去重
			{Encoded: "not-a-credential"}, // 解析失败跳过
			{Encoded: `{}`},               // Recipient 为空跳过
		}},
	}
	got := ReadListenerPins(cfg)
	if len(got) != 2 || got[0] != testReaderFP || got[1] != otherFP {
		t.Fatalf("ReadListenerPins=%v, want [%s %s]", got, testReaderFP, otherFP)
	}
}

func TestMeshWriterFingerprints_ScopeFilter(t *testing.T) {
	t.Parallel()
	const writeFP = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	const rwFP = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	cfg := &Config{Volumes: []VolumeConfig{
		{Name: "no-acl"},
		{Name: "main", ACL: &VolumeACLConfig{MeshReaders: []VolumeMeshReaderConfig{
			{Node: "read", Fingerprint: testReaderFP, Scope: "read"},
			{Node: "empty-scope", Fingerprint: testReaderFP},
			{Node: "bogus", Fingerprint: testReaderFP, Scope: "admin"},
			{Node: "write", Fingerprint: writeFP, Scope: "write"},
			{Node: "rw", Fingerprint: rwFP, Scope: "rw"},
			{Node: "rw-dup", Fingerprint: rwFP, Scope: "rw"},
			{Node: "write-empty", Fingerprint: "  ", Scope: "write"},
		}}},
	}}
	got := MeshWriterFingerprints(cfg)
	if len(got) != 2 || got[0] != writeFP || got[1] != rwFP {
		t.Fatalf("MeshWriterFingerprints=%v, want [%s %s]", got, writeFP, rwFP)
	}
}

func TestAppendUniqueFingerprint(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	out := appendUniqueFingerprint(seen, nil, "  ")
	if len(out) != 0 {
		t.Fatalf("空指纹应跳过, got %v", out)
	}
	out = appendUniqueFingerprint(seen, out, testReaderFP)
	out = appendUniqueFingerprint(seen, out, testReaderFP)
	if len(out) != 1 {
		t.Fatalf("重复指纹应去重, got %v", out)
	}
}

func TestScopeGrantsWrite(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"": false, "read": false, "admin": false,
		"write": true, "rw": true, "RW": true,
	}
	for scope, want := range cases {
		if got := scopeGrantsWrite(scope); got != want {
			t.Fatalf("scopeGrantsWrite(%q)=%v, want %v", scope, got, want)
		}
	}
}

func TestParseVolumeACL_NilAndEmptyMode(t *testing.T) {
	t.Parallel()
	if acl := ParseVolumeACL(nil, nil); acl.Mode == "" || acl.Owners == nil {
		t.Fatalf("nil ACL 应回落 deny + 空 owners: %+v", acl)
	}
	if acl := ParseVolumeACL(&VolumeACLConfig{}, nil); acl.Mode != "deny" {
		t.Fatalf("空 mode 应归一 deny, got %q", acl.Mode)
	}
}

// TestSetDefaults_VolumeACLModeAndXferListens 覆盖两条零值归一分支：
// 卷 ACL 显式存在但 mode 为空 → 归一 deny；xfer listener 显式启用但缺省 listen →
// 回落 loopback 默认地址。
func TestSetDefaults_VolumeACLModeAndXferListens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	z := &Config{StorageRoot: dir}
	z.Volumes = []VolumeConfig{{Name: "main", Root: dir, ACL: &VolumeACLConfig{}}}
	z.Hub.Transports.XferTLS.Enabled = true
	z.Hub.Transports.XferTCP.Enabled = true
	z.SetDefaults()
	if got := z.Volumes[0].ACL.Mode; got != VolumeACLDeny {
		t.Fatalf("空 ACL mode 应归一 deny, got %q", got)
	}
	if got := z.Hub.Transports.XferTLS.Listen; got != DefaultXferTLSListen {
		t.Fatalf("xfer_tls listen 应回落默认 %q, got %q", DefaultXferTLSListen, got)
	}
	if got := z.Hub.Transports.XferTCP.Listen; got != DefaultXferTCPListen {
		t.Fatalf("xfer_tcp listen 应回落默认 %q, got %q", DefaultXferTCPListen, got)
	}
}

// TestNormalizeVolumesDefaults 覆盖 Validate 前兜底归一：空 volumes 合成单默认卷、
// 空 placement 归一 prefer-default；已配值不被改写。
func TestNormalizeVolumesDefaults(t *testing.T) {
	t.Parallel()
	empty := &Config{StorageRoot: "/root"}
	empty.normalizeVolumesDefaults()
	if len(empty.Volumes) != 1 || empty.Volumes[0].Name != "default" || empty.Volumes[0].Root != "/root" {
		t.Fatalf("空 volumes 未合成单默认卷: %+v", empty.Volumes)
	}
	if empty.Placement != "prefer-default" {
		t.Fatalf("空 placement 未归一: %q", empty.Placement)
	}

	explicit := &Config{
		StorageRoot: "/root",
		Placement:   "spread",
		Volumes:     []VolumeConfig{{Name: "v", Root: "/x"}},
	}
	explicit.normalizeVolumesDefaults()
	if explicit.Placement != "spread" || len(explicit.Volumes) != 1 || explicit.Volumes[0].Name != "v" {
		t.Fatalf("显式值不应被改写: %+v", explicit)
	}
}
