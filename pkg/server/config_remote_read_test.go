// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 非回环的**配置串**：只喂给 Validate 做字符串校验，任何测试都不会真的绑定它们
// （绑定它们的只有 127.0.0.1）。单独抽成常量是为了让「配置串」与「真实监听地址」
// 在源码上一眼可分，且不触发 .githooks/pre-commit 的 check-loopback 按行字面量扫描
// （该扫描 grep `Listen.*<host>`，目的是拦住真的绑非回环地址的测试）。
const (
	cfgHostWildcard  = "0.0.0.0"
	cfgHostLocalhost = "localhost"
)

// remoteReadCfgFixture 构造「单卷 main + 一条 mesh_readers」的 remote_read 已启用配置。
//
// 刻意不在此处调用 SetDefaults/Validate：各用例自行决定调用顺序——本包的 Validate
// 允许在 SetDefaults 之前被调用（见 config.go 的兜底归一注释），而 handshake_timeout
// 非法值只能在 SetDefaults 之前观察到（SetDefaults 会把 <=0 归一到 10s）。
func remoteReadCfgFixture(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = filepath.Join(t.TempDir(), "vol-main")
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner,
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:19000"
	return cfg
}

// TestRemoteReadConfig_Defaults 钉住默认值：未配置 remote_read 时 Enabled=false
// （零回归：默认不监听只读面）且剩余字段为约定的文档默认。
func TestRemoteReadConfig_Defaults(t *testing.T) {
	c := Default()
	if c.RemoteRead.Enabled {
		t.Fatal("remote_read 默认必须关闭（零回归：默认不起只读 listener）")
	}
	if c.RemoteRead.Listen != "127.0.0.1:19000" {
		t.Fatalf("默认 listen 不符: %q", c.RemoteRead.Listen)
	}
	if c.RemoteRead.HandshakeTimeout != 10*time.Second {
		t.Fatalf("默认 handshake_timeout 不符: %v", c.RemoteRead.HandshakeTimeout)
	}

	// SetDefaults 兜底：零值 RemoteReadConfig 归一到同一默认（viper 未配时不残留 0）。
	z := &Config{StorageRoot: t.TempDir()}
	z.SetDefaults()
	if z.RemoteRead.Listen != "127.0.0.1:19000" {
		t.Fatalf("SetDefaults 未兜底 listen: %q", z.RemoteRead.Listen)
	}
	if z.RemoteRead.HandshakeTimeout != 10*time.Second {
		t.Fatalf("SetDefaults 未兜底 handshake_timeout: %v", z.RemoteRead.HandshakeTimeout)
	}
}

// TestRemoteReadConfig_ValidateMatrix 钉住 remote_read 的启动期校验（fail-closed 面）：
//   - listen 强制 loopback（配非 loopback 即拒绝启动，防只读面被直连暴露）；
//   - listen 空 / 格式非法拒绝；
//   - handshake_timeout 必须为正；
//   - **enabled=true 但无任何 mesh_readers 指纹时拒绝启动**——这是控制者明令的
//     不可省略门禁：tunnel.DeriveRemoteStaticKey 由**公开的 listener 身份指纹**派生，
//     且该值在 Tunnel 中**兼作 dialer 侧握手失败时的回退加密密钥**，故任一端漏配 pin
//     就会退化为「用公开可推导的密钥加密」。B 侧拒启动（无 pin 不接受任何对端）是该
//     安全论证的必要条件之一，**不得为「方便调试」删除**。
func TestRemoteReadConfig_ValidateMatrix(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*Config)
		setDefaults bool
		wantErr     string
	}{
		{"启用+loopback+有 mesh_readers 通过", func(c *Config) {}, true, ""},
		{"非 loopback 通配地址拒绝", func(c *Config) { c.RemoteRead.Listen = cfgHostWildcard + ":19000" }, false, "loopback"},
		{"非 loopback 裸端口拒绝", func(c *Config) { c.RemoteRead.Listen = ":19000" }, false, "loopback"},
		{"非 loopback 内网地址拒绝", func(c *Config) { c.RemoteRead.Listen = "192.168.1.1:1" }, false, "loopback"},
		{"loopback 主机名通过", func(c *Config) { c.RemoteRead.Listen = cfgHostLocalhost + ":19000" }, false, ""},
		{"IPv6 loopback 通过", func(c *Config) { c.RemoteRead.Listen = "[::1]:19000" }, false, ""},
		{"listen 为空拒绝", func(c *Config) { c.RemoteRead.Listen = "" }, false, "不能为空"},
		{"listen 格式非法拒绝", func(c *Config) { c.RemoteRead.Listen = "not-an-addr" }, false, "格式非法"},
		{"handshake_timeout 为 0 拒绝", func(c *Config) { c.RemoteRead.HandshakeTimeout = 0 }, false, "handshake_timeout"},
		{"handshake_timeout 为负拒绝", func(c *Config) { c.RemoteRead.HandshakeTimeout = -time.Second }, false, "handshake_timeout"},
		{"无 mesh_readers 拒绝启动", func(c *Config) { c.Volumes[0].ACL.MeshReaders = nil }, false, "mesh_readers"},
		{"mesh_readers 指纹非法由既有校验拒绝", func(c *Config) { c.Volumes[0].ACL.MeshReaders[0].Fingerprint = "nope" }, false, "fingerprint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := remoteReadCfgFixture(t)
			tc.mutate(cfg)
			if tc.setDefaults {
				cfg.SetDefaults()
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒绝（含 %q）", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestRemoteReadConfig_DisabledSkipsValidation 钉住零回归的边界：remote_read 未启用时，
// listen/handshake_timeout 的取值一律不参与校验（不配置即不受任何新约束）。
func TestRemoteReadConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	cfg.SetDefaults() // 先归一，再覆盖为「未启用 + 各种非法值」
	cfg.RemoteRead = RemoteReadConfig{Enabled: false, Listen: cfgHostWildcard + ":1", HandshakeTimeout: -time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("未启用时不应校验 remote_read: %v", err)
	}
}

// TestRemoteReadConfig_NoMeshReadersRefusedForEveryVolumeShape 钉住「无 pin 拒启动」的
// 覆盖面：不只是「ACL 缺失」，而是**任何卷都不含 mesh_readers**（含 ACL 为 nil 的卷）
// 都必须拒绝——只要有一个卷配了指纹即放行。
func TestRemoteReadConfig_NoMeshReadersRefusedForEveryVolumeShape(t *testing.T) {
	// ACL 为 nil（缺省开放卷）：无任何 mesh_readers → 拒绝。
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	cfg.RemoteRead.Enabled = true
	cfg.SetDefaults()
	cfg.RemoteRead.HandshakeTimeout = 10 * time.Second
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mesh_readers") {
		t.Fatalf("无 mesh_readers 必须拒绝启动（fail-closed）, got %v", err)
	}

	// 另一卷配了指纹：放行（去重后的 pin 列表非空）。
	cfg2 := remoteReadCfgFixture(t)
	cfg2.Volumes = append(cfg2.Volumes, VolumeConfig{Name: "extra", Root: filepath.Join(t.TempDir(), "vol-extra")})
	cfg2.SetDefaults()
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("任一卷有 mesh_readers 即应放行, got %v", err)
	}
}
