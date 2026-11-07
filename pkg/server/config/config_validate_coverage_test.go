// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_validate_coverage_test.go 补 Config.Validate 的**拒绝分支矩阵**：基础字段、state_store /
// cluster、IP 白名单、上传上限、tier_policy、卷身份/容量/retention/tier/ACL、镜像图、hub、
// 限流、bucket_limits、registration、api_keys、audit、telemetry、维护窗口、AI 建议器、
// hub 联邦、sync_remotes、外部卷 extras、remote_read/remote_write、mesh 双角色、凭据加密。
//
// 所有用例都经 cfg.Validate()（而不是直接调子校验），以锁定调用链顺序与错误面。
package config

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
)

// coverageValidConfig 构造可校验基线：临时存储根 + 单本地卷。
func coverageValidConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = testutil.MkdirTemp(t, "config-validate-")
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	return cfg
}

// coverageMeshRemote 构造一条合法的 kind=mesh sync_remote（供 mesh 校验用例复用）。
func coverageMeshRemote() SyncRemoteConfig {
	return SyncRemoteConfig{
		Name: "mesh-a", Kind: "mesh", Node: "node-a", Volume: "main",
		PeerPins: []string{testReaderFP},
	}
}

func TestValidate_RejectBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		// 基础字段。
		{"addr 为空拒绝", func(c *Config) { c.Addr = "" }, "addr 为空"},
		{"storage_root 为空拒绝", func(c *Config) { c.StorageRoot = "" }, "storage_root 为空"},

		// state_store / cluster。
		{"state_store=raft 拒绝", func(c *Config) { c.StateStore.Type = "raft" }, "raft 未实现"},
		{"state_store=mongo 缺 uri 拒绝", func(c *Config) { c.StateStore.Type = "mongo" }, "mongo.uri"},
		{"state_store 未知 type 拒绝", func(c *Config) { c.StateStore.Type = "bogus" }, "无效"},
		{"cluster 启用缺 node_id 拒绝", func(c *Config) { c.Cluster.Enabled = true }, "node_id"},
		{"cluster 非法 role 拒绝", func(c *Config) {
			c.Cluster.Enabled = true
			c.Cluster.NodeID = "n1"
			c.Cluster.Role = "leader"
		}, "role"},
		{"cluster 负 resync 拒绝", func(c *Config) {
			c.Cluster.Enabled = true
			c.Cluster.NodeID = "n1"
			c.Cluster.Role = ClusterRoleMaster
			c.Cluster.ResyncInterval = -time.Second
		}, "index_resync_interval"},

		// IP 白名单 / 上传上限 / tier_policy。
		{"allow_ips 非法拒绝", func(c *Config) { c.Auth.AllowIPs = []string{"bad"} }, "auth.allow_ips"},
		{"trusted_proxies 非法拒绝", func(c *Config) { c.Auth.TrustedProxies = []string{"bad"} }, "auth.trusted_proxies"},
		{"max_upload_bytes 负值拒绝", func(c *Config) { c.MaxUploadBytes = -1 }, "max_upload_bytes"},
		{"tier_policy.interval 负值拒绝", func(c *Config) { c.TierPolicy.Interval = -time.Second }, "tier_policy.interval"},
		{"tier_policy.max_age_hot 负值拒绝", func(c *Config) { c.TierPolicy.MaxAgeHot = -time.Second }, "max_age_hot"},
		{"tier_policy.min_size_hot 负值拒绝", func(c *Config) { c.TierPolicy.MinSizeHot = -1 }, "min_size_hot"},

		// 卷身份 / 容量 / retention / tier。
		{"placement 非法拒绝", func(c *Config) { c.Placement = "random" }, "placement 非法"},
		{"卷名为空拒绝", func(c *Config) { c.Volumes[0].Name = "" }, "卷名"},
		{"enclosure 不可写拒绝", func(c *Config) { c.Volumes[0].Type = enclosure.TypeEnclosure }, "enclosure"},
		{"卷名重复拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "main", Root: c.StorageRoot + "-2"})
		}, "卷名重复"},
		{"非首卷空 root 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "second"})
		}, "root 为空"},
		{"词法等值 root 重复拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "second", Root: c.StorageRoot + "/"})
		}, "root 重复"},
		{"卷容量负值拒绝", func(c *Config) { c.Volumes[0].VolCapacity = -1 }, "容量上限"},
		{"卷 tier 非法拒绝", func(c *Config) { c.Volumes[0].Tier = "lukewarm" }, "tier"},
		{"retention.version_ttl 负值拒绝", func(c *Config) {
			c.Volumes[0].Retention.VersionTTL = -time.Second
		}, "retention.version_ttl"},
		{"retention gc 空转拒绝", func(c *Config) {
			c.Volumes[0].Retention.GCInterval = time.Minute
		}, "空转任务"},

		// 卷 ACL。
		{"acl mode 非法拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: "maybe"}
		}, "acl mode"},
		{"acl owners 非法拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"a/b"}}
		}, "owners 含非法 owner"},
		{"mesh_readers.node 为空拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{{Fingerprint: testReaderFP, Owner: "alice"}}}
		}, "node 不能为空"},
		{"mesh_readers.owner 为空拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{{Node: "n", Fingerprint: testReaderFP}}}
		}, "owner 不能为空"},
		{"mesh_readers.owner 非法拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{{Node: "n", Fingerprint: testReaderFP, Owner: "a/b"}}}
		}, "owner 非法"},
		{"mesh_readers.fingerprint 非法拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{{Node: "n", Fingerprint: "sha256:abc", Owner: "alice"}}}
		}, "fingerprint 非法"},
		{"mesh_readers.scope 非法拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{{Node: "n", Fingerprint: testReaderFP, Owner: "alice", Scope: "admin"}}}
		}, "scope 非法"},
		{"mesh_readers 指纹重复拒绝", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLAllow, MeshReaders: []VolumeMeshReaderConfig{
				{Node: "n1", Fingerprint: testReaderFP, Owner: "alice"},
				{Node: "n2", Fingerprint: strings.ToUpper(testReaderFP), Owner: "bob"},
			}}
		}, "指纹重复"},

		// 镜像图。
		{"mirror_to 与 mirror_targets 互斥", func(c *Config) {
			c.Volumes[0].MirrorTo = "second"
			c.Volumes[0].MirrorTargets = []string{"second"}
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "second", Root: c.StorageRoot + "-2"})
		}, "互斥"},
		{"mirror_to 指向自身拒绝", func(c *Config) { c.Volumes[0].MirrorTo = "main" }, "不能指向自身"},
		{"mirror_to 目标不存在拒绝", func(c *Config) { c.Volumes[0].MirrorTo = "ghost" }, "mirror_to 目标卷"},
		{"mirror_targets 指向自身拒绝", func(c *Config) { c.Volumes[0].MirrorTargets = []string{"main"} }, "mirror_targets 不能指向自身"},
		{"mirror_targets 目标不存在拒绝", func(c *Config) { c.Volumes[0].MirrorTargets = []string{"ghost"} }, "mirror_targets 目标卷"},
		{"mirror_targets 目标重复拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "second", Root: c.StorageRoot + "-2"})
			c.Volumes[0].MirrorTargets = []string{"second", "second"}
		}, "目标重复"},
		{"镜像链两卷成环拒绝", func(c *Config) {
			c.Volumes = []VolumeConfig{
				{Name: "a", Root: c.StorageRoot + "-a", MirrorTo: "b"},
				{Name: "b", Root: c.StorageRoot + "-b", MirrorTo: "a"},
			}
		}, "成环"},
		{"多目标首目标成环拒绝", func(c *Config) {
			c.Volumes = []VolumeConfig{
				{Name: "a", Root: c.StorageRoot + "-a", MirrorTargets: []string{"b"}},
				{Name: "b", Root: c.StorageRoot + "-b", MirrorTo: "a"},
			}
		}, "成环"},

		// hub。
		{"hub.dht 非法拒绝", func(c *Config) {
			c.Hub.Enabled = true
			c.Hub.Transports.WS.Enabled = true
			c.Hub.DHT = "kademlia"
		}, "hub.dht"},
		{"hub.virtual_subnet 解析失败拒绝", func(c *Config) { c.Hub.VirtualSubnet = "bogus" }, "virtual_subnet"},
		{"hub.virtual_subnet 非 IPv4 拒绝", func(c *Config) { c.Hub.VirtualSubnet = "2001:db8::/64" }, "IPv4"},

		// 限流 / bucket_limits / registration / api_keys / audit / telemetry。
		{"rate_limit.requests 非正拒绝", func(c *Config) {
			c.RateLimit.Enabled = true
			c.RateLimit.Requests = 0
		}, "requests"},
		{"rate_limit.window 非正拒绝", func(c *Config) {
			c.RateLimit.Enabled = true
			c.RateLimit.Requests = 5
			c.RateLimit.Window = 0
		}, "window"},
		{"bucket_limits 含 .. 段拒绝", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"user/../x": 1}
		}, "非法"},
		{"bucket_limits 魔法前缀拒绝", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"user/.__x": 1}
		}, "含非法段"},
		{"bucket_limits 功能桶重叠拒绝", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"user": 1}
		}, "与功能桶根重叠"},
		{"bucket_limits 非 user 桶拒绝", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"cloud/x": 1}
		}, "仅支持 user 桶子目录"},
		{"bucket_limits 负上限拒绝", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"user/videos": -1}
		}, "不能为负"},
		{"registration.login_fail_limit 拒绝", func(c *Config) { c.Registration.LoginFailLimit = 0 }, "login_fail_limit"},
		{"registration.session_ttl 拒绝", func(c *Config) { c.Registration.SessionTTL = 0 }, "session_ttl"},
		{"registration.cli_ttl 拒绝", func(c *Config) { c.Registration.CliTTL = 0 }, "cli_ttl"},
		{"registration.login_fail_window 拒绝", func(c *Config) { c.Registration.LoginFailWindow = 0 }, "login_fail_window"},
		{"api_keys 启用无密钥拒绝", func(c *Config) { c.APIKeys.Enabled = true }, "未配置任何密钥"},
		{"api_keys 空 key 拒绝", func(c *Config) { c.APIKeys.Keys = []APIKey{{}} }, "key 为空"},
		{"api_keys 非法 permission 拒绝", func(c *Config) {
			c.APIKeys.Keys = []APIKey{{Key: "k", Permission: "bogus"}}
		}, "permission"},
		{"audit.buffer_size 负值拒绝", func(c *Config) { c.Audit.BufferSize = -1 }, "buffer_size"},
		{"audit.max_size 负值拒绝", func(c *Config) { c.Audit.MaxSize = -1 }, "max_size"},
		{"audit.max_archives 负值拒绝", func(c *Config) { c.Audit.MaxArchives = -1 }, "max_archives"},
		{"telemetry.sample_ratio 非正拒绝", func(c *Config) {
			c.Telemetry.Enabled = true
			c.Telemetry.SampleRatio = 0
		}, "sample_ratio"},
		{"telemetry.sample_ratio 超上限拒绝", func(c *Config) {
			c.Telemetry.Enabled = true
			c.Telemetry.SampleRatio = 1.5
		}, "sample_ratio"},
		{"telemetry.otlp_endpoint scheme 非法拒绝", func(c *Config) {
			c.Telemetry.Enabled = true
			c.Telemetry.SampleRatio = 1
			c.Telemetry.OTLPEndpoint = "ftp://collector:4318"
		}, "scheme"},
		{"telemetry.otlp_endpoint 缺 host 拒绝", func(c *Config) {
			c.Telemetry.Enabled = true
			c.Telemetry.SampleRatio = 1
			c.Telemetry.OTLPEndpoint = "http:///v1/traces"
		}, "缺少 host"},
		{"telemetry.otlp_endpoint 解析失败拒绝", func(c *Config) {
			c.Telemetry.Enabled = true
			c.Telemetry.SampleRatio = 1
			c.Telemetry.OTLPEndpoint = "http://[::1"
		}, "非法"},

		// 调度器维护窗口 / AI 建议器。
		{"维护窗口 start 非法拒绝", func(c *Config) {
			c.Scheduler.MaintenanceWindow.Enabled = true
			c.Scheduler.MaintenanceWindow.Start = "25:00"
			c.Scheduler.MaintenanceWindow.End = "06:00"
		}, "maintenance_window.start"},
		{"维护窗口 end 非法拒绝", func(c *Config) {
			c.Scheduler.MaintenanceWindow.Enabled = true
			c.Scheduler.MaintenanceWindow.Start = "22:00"
			c.Scheduler.MaintenanceWindow.End = "6:0"
		}, "maintenance_window.end"},
		{"ai_advisor api_key_ref 非法拒绝", func(c *Config) {
			c.Notify.AIAdvisor.Enabled = true
			c.Notify.AIAdvisor.APIKeyRef = "1BAD"
		}, "api_key_ref"},

		// hub 联邦。
		{"federation peer url 解析失败拒绝", func(c *Config) {
			c.Hub.Federation.Enabled = true
			c.Hub.Federation.Peers = []FederationPeerConfig{{URL: "http://[::1"}}
		}, "url 非法"},
		{"federation peer SK 非 hex 拒绝", func(c *Config) {
			c.Hub.Federation.Enabled = true
			c.Hub.Federation.Peers = []FederationPeerConfig{{
				URL: "http://127.0.0.1:18084", AccessKey: "ak",
				AccessKeySecret: strings.Repeat("z", 64),
			}}
		}, "不是合法十六进制"},

		// sync_remotes。
		{"sync_remote name 为空拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{}}
		}, "name 为空"},
		{"sync_remote name 重复拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "dup", URL: "https://127.0.0.1:18083"}, {Name: "dup", URL: "https://127.0.0.1:18083"}}
		}, "重复"},
		{"sync_remote kind 非法拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "r", Kind: "bogus"}}
		}, "kind"},
		{"sync_remote volume 载体缺卷名拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "r", Kind: "volume"}}
		}, "volume 为空"},
		{"sync_remote baidupcs 别名缺卷名拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "r", Kind: "baidupcs"}}
		}, "volume 为空"},
		{"sync_remote mesh 缺 node 拒绝", func(c *Config) {
			r := coverageMeshRemote()
			r.Node = ""
			c.SyncRemotes = []SyncRemoteConfig{r}
		}, "node 为空"},
		{"sync_remote mesh 缺 volume 拒绝", func(c *Config) {
			r := coverageMeshRemote()
			r.Volume = ""
			c.SyncRemotes = []SyncRemoteConfig{r}
		}, "volume 为空"},
		{"sync_remote mesh 缺 peer_pins 拒绝", func(c *Config) {
			r := coverageMeshRemote()
			r.PeerPins = nil
			c.SyncRemotes = []SyncRemoteConfig{r}
		}, "peer_pins"},
		{"sync_remote mesh transport 非法拒绝", func(c *Config) {
			r := coverageMeshRemote()
			r.Transport = "carrier-pigeon"
			c.SyncRemotes = []SyncRemoteConfig{r}
		}, "transport"},
		{"sync_remote url 解析失败拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "http://[::1"}}
		}, "url 非法"},
		{"sync_remote url 缺 host 拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "https:///path"}}
		}, "缺少 host"},

		// 外部卷 extras。
		{"baidupcs 缺执行路径拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "bd", Type: "baidupcs"})
		}, "extra.bduss"},
		{"webdav 缺 url 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "dav", Type: "webdav"})
		}, "extra.url"},
		{"webdav 缺认证拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "dav", Type: "webdav", Extra: map[string]any{"url": "https://dav.example.com/root"}})
		}, "需配置认证"},
		{"s3 缺 endpoint 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "s3v", Type: "s3"})
		}, "extra.endpoint"},
		{"s3 endpoint 缺 host 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "s3v", Type: "s3", Extra: map[string]any{"endpoint": "https://"}})
		}, "extra.endpoint 非法"},
		{"s3 缺 bucket 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "s3v", Type: "s3", Extra: map[string]any{"endpoint": "127.0.0.1:9000"}})
		}, "extra.bucket"},
		{"s3 缺凭据拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "s3v", Type: "s3", Extra: map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b"}})
		}, "extra.access_key"},

		// 跨节点只读/写面。
		{"remote_read.listen 为空拒绝", func(c *Config) {
			c.RemoteRead.Enabled = true
			c.RemoteRead.Listen = ""
		}, "remote_read.listen 不能为空"},
		{"remote_read.handshake_timeout 非正拒绝", func(c *Config) {
			c.RemoteRead.Enabled = true
			c.RemoteRead.HandshakeTimeout = 0
		}, "remote_read.handshake_timeout"},
		{"remote_read 无 pin 拒绝", func(c *Config) { c.RemoteRead.Enabled = true }, "mesh_readers"},
		{"remote_write.listen 为空拒绝", func(c *Config) {
			c.RemoteWrite.Enabled = true
			c.RemoteWrite.Listen = ""
		}, "remote_write.listen 不能为空"},
		{"remote_write.handshake_timeout 非正拒绝", func(c *Config) {
			c.RemoteWrite.Enabled = true
			c.RemoteWrite.HandshakeTimeout = 0
		}, "remote_write.handshake_timeout"},
		{"remote_write 无写条目拒绝", func(c *Config) { c.RemoteWrite.Enabled = true }, "scope 授予写"},

		// mesh 远端（A 侧）。
		{"mesh.hub_url 非法拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{coverageMeshRemote(), {Name: "direct-1", URL: "https://127.0.0.1:18083"}}
			c.Mesh.HubURL = "not a url"
		}, "mesh.hub_url 非法"},
		{"mesh 远端缺 access_key 拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{coverageMeshRemote()}
			c.Mesh.HubURL = "https://hub.example.com:18083"
		}, "access_key"},
		{"mesh 远端缺 skey_id 拒绝", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{coverageMeshRemote()}
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.AccessKey = "ak"
			c.Mesh.AccessKeySecret = strings.Repeat("1", 64)
		}, "skey_id"},
		{"mesh webrtc 缺 node_id 拒绝", func(c *Config) {
			r := coverageMeshRemote()
			r.Transport = "webrtc"
			c.SyncRemotes = []SyncRemoteConfig{r}
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.AccessKey = "ak"
			c.Mesh.AccessKeySecret = strings.Repeat("1", 64)
			c.Mesh.SkeyID = "sk"
		}, "transport=webrtc"},

		// mesh node（B 侧）。
		{"mesh.node 缺节点 ID 拒绝", func(c *Config) { c.Mesh.Node.Enabled = true }, "节点 ID"},
		{"mesh.node 无服务可宣告拒绝", func(c *Config) {
			c.Mesh.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
		}, "没有任何可宣告的服务"},
		{"mesh.node hub_url 非法拒绝", func(c *Config) {
			c.Mesh.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.Node.ExtraServices = []string{"volread:127.0.0.1:19000"}
			c.Mesh.Node.HubURL = "bad-url"
		}, "hub_url 非法"},
		{"mesh.node 注册远端 hub 缺凭据拒绝", func(c *Config) {
			c.Mesh.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.Node.ExtraServices = []string{"volread:127.0.0.1:19000"}
			c.Mesh.Node.HubURL = "https://hub.example.com:18083"
		}, "access_key"},

		// 凭据加密装配。
		{"credential_store backend 非法拒绝", func(c *Config) {
			c.CredentialStore.Backend = "bogus"
		}, "仅允许 aesgcm 或 vault"},
		{"vault addr 解析失败拒绝", func(c *Config) {
			c.CredentialStore.Backend = "vault"
			c.CredentialStore.Encrypt = true
			c.CredentialStore.Vault.Addr = "http://[::1"
		}, "vault.addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := coverageValidConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidate_AcceptBranches 覆盖容易误判的**放行**分支（负向矩阵之外）。
func TestValidate_AcceptBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"默认配置通过", func(*Config) {}},
		{"mirror_targets 空元素跳过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "second", Root: c.StorageRoot + "-2"})
			c.Volumes[0].MirrorTargets = []string{"", "second"}
		}},
		{"外部卷不参与镜像校验", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "bd", Type: "baidupcs", MirrorTo: "bd",
				Extra: map[string]any{"bduss": "dummy"},
			})
		}},
		{"api_keys permission 空值合法", func(c *Config) {
			c.APIKeys.Keys = []APIKey{{Key: "k"}, {Key: "k2", Permission: PermissionWrite}}
		}},
		{"bucket_limits 合法子目录通过", func(c *Config) {
			c.BucketLimits = map[string]ByteSize{"user/videos/hd": 1 << 20}
		}},
		{"ai_advisor 空 api_key_ref 合法", func(c *Config) {
			c.Notify.AIAdvisor.Enabled = true
		}},
		{"ai_advisor 合法环境变量名通过", func(c *Config) {
			c.Notify.AIAdvisor.Enabled = true
			c.Notify.AIAdvisor.APIKeyRef = "SPROXY_AI_KEY"
		}},
		{"mesh.node 带 extra_services 与本地 creds 通过", func(c *Config) {
			c.Mesh.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.Node.ExtraServices = []string{"volread:127.0.0.1:19000"}
		}},
		{"mesh 远端齐备凭据通过", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{coverageMeshRemote()}
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.AccessKey = "ak"
			c.Mesh.AccessKeySecret = strings.Repeat("1", 64)
			c.Mesh.SkeyID = "sk"
			c.Mesh.NodeID = "node-a"
			c.SyncRemotes[0].Transport = "webrtc"
		}},
		{"mesh 远端与 direct 远端共存通过", func(c *Config) {
			c.SyncRemotes = []SyncRemoteConfig{
				coverageMeshRemote(),
				{Name: "direct-1", URL: "https://127.0.0.1:18083"},
			}
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.AccessKey = "ak"
			c.Mesh.AccessKeySecret = strings.Repeat("1", 64)
			c.Mesh.SkeyID = "sk"
			c.Mesh.NodeID = "node-a"
		}},
		{"sync_remote volume 载体通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "ext", Type: "baidupcs", Extra: map[string]any{"bduss": "dummy"},
			})
			c.SyncRemotes = []SyncRemoteConfig{{Name: "local-ext", Kind: "volume", Volume: "ext"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := coverageValidConfig(t)
			tc.mutate(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("期望通过, got %v", err)
			}
		})
	}
}

// TestValidateMirrorCycles_BrokenChain 覆盖镜像链走到「目标卷不存在」时的提前收口：
// 单卷校验已拒绝不存在的 mirror 目标，故此处直接调用成环检测函数验证其防御分支。
func TestValidateMirrorCycles_BrokenChain(t *testing.T) {
	t.Parallel()
	c := &Config{Volumes: []VolumeConfig{
		{Name: "a", Root: "/a", MirrorTo: "ghost"},
	}}
	if err := c.validateMirrorCycles(); err != nil {
		t.Fatalf("不完整链应提前结束而非报错, got %v", err)
	}
}

// TestValidate_ExternalVolumeLocalTypeSkipsExtras 锁定 type=local 与空 type 都跳过外部卷 extras
// （零迁移语义）。
func TestValidate_ExternalVolumeLocalTypeSkipsExtras(t *testing.T) {
	t.Parallel()
	cfg := coverageValidConfig(t)
	cfg.Volumes = append(cfg.Volumes, VolumeConfig{Name: "local-2", Type: volume.TypeLocal, Root: cfg.StorageRoot + "-local2"})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("type=local 不应要求 extras, got %v", err)
	}
}
