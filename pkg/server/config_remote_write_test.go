// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_remote_write_test.go 钉住写面配置的**启动期校验**（fail-fast）与零值兜底。
//
// 与只读面的三项校验同构，另加一项写面专属：**必须至少一条 scope 授予写（write|rw）**——
// 「配了 remote_write 却只有 read 条目」是配置脚枪：写面起来了但每个请求都 404，运维会
// 误判为网络问题；而 pin 列表（只收授写指纹）为空时 listener 也起不来。

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// remoteWriteCfgFixture 构造「单卷 + 一条 scope 可配的 mesh_readers」并启用写面。
func remoteWriteCfgFixture(t *testing.T, scope string) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice", Scope: scope,
		}},
	}}}
	cfg.RemoteWrite.Enabled = true
	cfg.RemoteWrite.Listen = "127.0.0.1:19001"
	cfg.RemoteWrite.HandshakeTimeout = 10 * time.Second
	cfg.SetDefaults()
	return cfg
}

func TestRemoteWriteConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // 空 = 期望通过
	}{
		{"启用+loopback+授写条目 通过", func(c *Config) {}, ""},
		{"scope=write 也算授写", func(c *Config) {
			c.Volumes[0].ACL.MeshReaders[0].Scope = volume.MeshScopeWrite
		}, ""},
		{"scope=read 拒绝（只读条目不算授写）", func(c *Config) {
			c.Volumes[0].ACL.MeshReaders[0].Scope = volume.MeshScopeRead
		}, "fail-closed"},
		{"无任何 mesh_readers 拒绝", func(c *Config) {
			c.Volumes[0].ACL.MeshReaders = nil
		}, "fail-closed"},
		// 用 TEST-NET-1（192.0.2.0/24，RFC 5737 文档保留段）表达「非 loopback」：既不能
		// 被误当成可监听地址，也避免在测试里出现 0.0.0.0 字面量（check-loopback 门禁）。
		{"非 loopback 拒绝", func(c *Config) { c.RemoteWrite.Listen = "192.0.2.1:19001" }, "loopback"},
		{"非法 listen 格式拒绝", func(c *Config) { c.RemoteWrite.Listen = "nonsense" }, "格式非法"},
		{"空 listen 拒绝", func(c *Config) { c.RemoteWrite.Listen = "" }, "不能为空"},
		{"握手超时为 0 拒绝", func(c *Config) { c.RemoteWrite.HandshakeTimeout = 0 }, "必须为正"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := remoteWriteCfgFixture(t, volume.MeshScopeRW)
			tc.mutate(cfg)
			// 空 listen 用例需绕过 SetDefaults 的兜底才有意义（SetDefaults 会填默认值）。
			if tc.wantErr == "不能为空" {
				cfg.RemoteWrite.Listen = ""
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒绝（%s）", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestRemoteWriteConfig_SetDefaults 钉住零值兜底：未配 listen/超时时填默认值（端口与只读面
// 不同，避免两者抢同一端口），且**不改变 Enabled 的零值语义**（默认关闭）。
func TestRemoteWriteConfig_SetDefaults(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.RemoteWrite = RemoteWriteConfig{} // 全零
	cfg.SetDefaults()
	if cfg.RemoteWrite.Enabled {
		t.Fatal("RemoteWrite 默认必须关闭（写面是高风险暴露面，须显式开启）")
	}
	if cfg.RemoteWrite.Listen != "127.0.0.1:19001" {
		t.Fatalf("默认 listen=%q want 127.0.0.1:19001（与只读面 19000 区分）", cfg.RemoteWrite.Listen)
	}
	if cfg.RemoteWrite.HandshakeTimeout != 10*time.Second {
		t.Fatalf("默认 handshake_timeout=%v want 10s", cfg.RemoteWrite.HandshakeTimeout)
	}
}
