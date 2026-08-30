// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clientfactory 提供客户端创建工厂的接口和实现。
// 生产实现通过配置加载创建 *client.FileClient，测试实现直接返回 mock。
package clientfactory

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// IdentityFileName 是身份文件默认文件名。
const IdentityFileName = "identity.json"

// DefaultIdentityPath 返回默认身份文件路径（XDG 配置目录 sproxy/identity.json）。
// 只计算路径，不创建目录/文件（加载场景无副作用）。
// xdg.ConfigHome 为空时回落 os.UserConfigDir()（OS 标准配置目录），避免相对路径落盘。
func DefaultIdentityPath() (string, error) {
	configHome := xdg.ConfigHome
	if configHome == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("无法确定配置目录: %w", err)
		}
		configHome = dir
	}
	return filepath.Join(configHome, "sproxy", IdentityFileName), nil
}

// LoadIdentityOptional 加载本端长时身份（P1 身份 pinning）。
// 身份文件不存在时返回 (nil, nil)——不自动生成，未配置身份时行为与现状完全一致。
// 身份文件存在但损坏时返回错误（fail-closed，不静默覆盖用户文件）。
func LoadIdentityOptional() (*tunnel.Identity, error) {
	path, err := DefaultIdentityPath()
	if err != nil {
		return nil, err
	}
	id, err := tunnel.LoadIdentity(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return id, nil
}

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

	// xfer 隧道模式（--xfer <name>）：P1 身份 pinning 的真实传输路径。
	// xfer 隧道走 mux 握手（performHandshake），身份签名 + 指纹 pin 在此校验。
	// 该模式下 TunnelDo 优先走 xfer（跳过 WithTunnel 传统隧道，避免 tunnelClient 短路）。
	xferName, xferFlagErr := cmd.Flags().GetString("xfer")
	xferEnabled := xferFlagErr == nil && xferName != ""

	opts := []client.Option{
		client.WithTimeout(time.Duration(cfg.Timeout) * time.Second),
	}
	if cfg.AccessKey != "" && cfg.AccessKeySecret != "" && serverFlagNotSet(cmd) && !xferEnabled {
		// 有 AK/SK 且未显式 --server：走 access-key 驱动的加密隧道（WithTunnel 内部
		// 已存 AK/SK 供外层 SproxySig 签名，无需再 WithAccessKey）。
		opts = append(opts, client.WithTunnel(cfg.AccessKey, cfg.AccessKeySecret))
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
	if ak, _ := cmd.Flags().GetString("access-key"); ak != "" && !xferEnabled {
		sk, _ := cmd.Flags().GetString("access-key-secret")
		// 显式 AK/SK（flag 覆盖）同样开启 access-key 驱动隧道：WithTunnel 内部已存
		// AK/SK 供外层 SproxySig 签名，无需重复 WithAccessKey。
		opts = append(opts, client.WithTunnel(ak, sk))
	}
	// 通用 mesh 参数（hub_url/node_id）：供 mesh connect / relay start / p2p 等命令
	// 在各自 --hub/--node-id 未显式指定时作为配置回落（P2-配置）。
	if cfg.HubURL != "" {
		opts = append(opts, client.WithMeshHubURL(cfg.HubURL))
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

	if xferEnabled {
		hub := ""
		if h, err := cmd.Flags().GetString("hub"); err == nil {
			hub = h
		}
		if hub == "" {
			hub = cfg.HubURL
		}
		if hub == "" {
			return nil, fmt.Errorf("xfer 隧道模式需要 --hub 或配置 hub_url")
		}
		// P1 身份 pinning：仅 xfer 隧道模式消费本端身份与对端指纹（懒加载——非隧道命令
		// 不加载身份，身份文件损坏不导致 upload/download 等命令全部不可用）。
		// 错误信息给出恢复路径。
		var id *tunnel.Identity
		if loadedID, lErr := LoadIdentityOptional(); lErr != nil {
			return nil, fmt.Errorf("加载本端身份失败（可用 sclient identity generate --force 重新生成，或删除身份文件）: %w", lErr)
		} else {
			id = loadedID
		}
		if id != nil {
			opts = append(opts, client.WithIdentity(id))
		}
		if len(cfg.PeerFingerprints) > 0 {
			opts = append(opts, client.WithPeerFingerprints(cfg.PeerFingerprints))
		}

		// 隧道加密密钥：access-key 驱动（SK 派生），使 mux 握手执行（key 为 nil 时不握手，
		// pinning 不生效）。
		// fail-closed：配置了身份/peer_fingerprints 但缺 access_key_secret 时，握手不执行、
		// pinning 静默不生效 = 安全机制被无声绕过，必须报错而非仅 Warn。
		if cfg.AccessKeySecret == "" && (id != nil || len(cfg.PeerFingerprints) > 0) {
			return nil, fmt.Errorf("xfer 隧道配置了身份或 peer_fingerprints 时必须配置 access_key_secret（ECDH 握手与身份 pinning 依赖隧道密钥；fail-closed）")
		}
		xferKey := ""
		if cfg.AccessKeySecret != "" {
			mesh := tunnel.AccessKeyMesh(cfg.AccessKey)
			k, kErr := tunnel.DeriveTunnelKey(cfg.AccessKeySecret, mesh)
			if kErr != nil {
				return nil, fmt.Errorf("派生 xfer 隧道密钥失败: %w", kErr)
			}
			xferKey = hex.EncodeToString(k)
		}
		opts = append(opts, client.WithXfer(xferName, hub, xferKey))
	} else if len(cfg.PeerFingerprints) > 0 {
		// fail-closed：非 xfer 命令配置了 peer_fingerprints 但当前命令不走 xfer 握手，
		// pinning 无法生效；配置了 pin 却静默跳过 = fail-open，必须报错并给出恢复路径。
		return nil, fmt.Errorf("已配置 peer_fingerprints 但当前命令不走 xfer 隧道（--xfer），身份指纹 pinning 无法生效；请使用 `sclient tunnel --xfer <name>`，或移除 peer_fingerprints 配置（fail-closed）")
	}

	fc := client.NewFileClient(serverURL, opts...)
	if err := fc.InitError(); err != nil {
		return nil, fmt.Errorf("初始化客户端失败: %w", err)
	}
	return fc, nil
}

// serverFlagNotSet 报告 --server flag 是否未显式指定（隧道模式仅对默认服务器生效）。
func serverFlagNotSet(cmd *cobra.Command) bool {
	s, _ := cmd.Flags().GetString("server")
	return s == ""
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
