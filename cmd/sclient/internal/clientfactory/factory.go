// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clientfactory 提供客户端创建工厂的接口和实现。
// 生产实现通过配置加载创建 *client.FileClient，测试实现直接返回 mock。
package clientfactory

import (
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Factory 抽象客户端创建，生产/测试可替换。
type Factory interface {
	// NewClient 从 cobra 命令和配置创建 *client.FileClient。
	NewClient(cmd *cobra.Command) (*client.FileClient, error)
}

// CfgBinder 抽象配置提供者能力，避免直接依赖 sclientcfg.ViperProvider 类型。
type CfgBinder interface {
	BindPFlag(key string, flag *pflag.Flag)
	Unmarshal(obj any) error
}

// factory 是生产实现，封装配置加载 + flag 覆盖 + 客户端构造。
type factory struct {
	cfgFile     string
	cfgProvider func() CfgBinder
}

// New 创建生产实现的 Factory。
// cfgProviderFn 是延迟获取配置提供者的函数，在 PersistentPreRunE 之后才有效。
func New(cfgFile string, cfgProviderFn func() CfgBinder) Factory {
	return &factory{
		cfgFile:     cfgFile,
		cfgProvider: cfgProviderFn,
	}
}

// NewClient 从配置加载和 flag 覆盖创建 *client.FileClient。
func (f *factory) NewClient(cmd *cobra.Command) (*client.FileClient, error) {
	p := f.cfgProvider()
	if p == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	cfg, err := client.LoadFromProvider(p)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	serverURL := cfg.ServerURL
	if s, _ := cmd.Flags().GetString("server"); s != "" {
		serverURL = s
	}

	opts := []client.Option{
		client.WithTimeout(time.Duration(cfg.Timeout) * time.Second),
	}
	if cfg.TunnelKey != "" {
		if s, _ := cmd.Flags().GetString("server"); s == "" {
			opts = append(opts, client.WithTunnel(cfg.TunnelKey))
		}
	}
	if cs, _ := cmd.Flags().GetInt64("chunk-size"); cs > 0 {
		opts = append(opts, client.WithChunkSize(cs))
	} else if cfg.ChunkSize > 0 {
		opts = append(opts, client.WithChunkSize(cfg.ChunkSize))
	}
	if cfg.MaxChunkSize > 0 {
		opts = append(opts, client.WithMaxChunkSize(cfg.MaxChunkSize))
	}
	if cfg.AccessKey != "" && cfg.AccessKeySecret != "" {
		opts = append(opts, client.WithAccessKey(cfg.AccessKey, cfg.AccessKeySecret))
	}
	if ak, _ := cmd.Flags().GetString("access-key"); ak != "" {
		sk, _ := cmd.Flags().GetString("access-key-secret")
		opts = append(opts, client.WithAccessKey(ak, sk))
	}
	// 通用 mesh 参数（hub_url/relay_token/node_id）：供 mesh connect / relay start /
	// p2p 等命令在各自 --hub/--token/--node-id 未显式指定时作为配置回落（P2-配置）。
	if cfg.HubURL != "" {
		opts = append(opts, client.WithMeshHubURL(cfg.HubURL))
	}
	if cfg.RelayToken != "" {
		opts = append(opts, client.WithRelayToken(cfg.RelayToken))
	}
	if cfg.NodeID != "" {
		opts = append(opts, client.WithNodeID(cfg.NodeID))
	}
	if insecure, _ := cmd.Flags().GetBool("insecure"); insecure {
		opts = append(opts, client.WithInsecureTLS())
	}
	if clientCert, _ := cmd.Flags().GetString("client-cert"); clientCert != "" {
		clientKey, _ := cmd.Flags().GetString("client-key")
		if clientKey == "" {
			return nil, fmt.Errorf("--client-cert 需要配合 --client-key 使用")
		}
		allowMissing, _ := cmd.Flags().GetBool("client-cert-allow-missing")
		opts = append(opts, client.WithClientCert(clientCert, clientKey, !allowMissing))
	} else if clientKey, _ := cmd.Flags().GetString("client-key"); clientKey != "" {
		return nil, fmt.Errorf("--client-key 需要配合 --client-cert 使用")
	}

	if allowFallback, _ := cmd.Flags().GetBool("allow-transport-fallback"); allowFallback {
		opts = append(opts, client.WithTransportFallback())
	} else if cfg.AllowTransportFallback {
		opts = append(opts, client.WithTransportFallback())
	}

	fc := client.NewFileClient(serverURL, opts...)
	if err := fc.InitError(); err != nil {
		return nil, fmt.Errorf("初始化客户端失败: %w", err)
	}
	return fc, nil
}

// mockFactory 是测试实现，直接返回预配置的 client。
type mockFactory struct {
	client *client.FileClient
	err    error
}

// NewMock 创建测试实现的 Factory。
func NewMock(client *client.FileClient, err error) Factory {
	return &mockFactory{client: client, err: err}
}

func (f *mockFactory) NewClient(cmd *cobra.Command) (*client.FileClient, error) {
	return f.client, f.err
}

// 编译期检查 mockFactory 实现 Factory 接口
var _ Factory = (*mockFactory)(nil)

// 编译期检查 factory 实现 Factory 接口
var _ Factory = (*factory)(nil)
