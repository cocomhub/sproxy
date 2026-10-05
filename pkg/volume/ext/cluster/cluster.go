// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

// cluster.go 是集群出口后端装配（type: egress）：凭证池解析/校验、NewBackend
// （凭证 → remote.Client + pin → federated.FS → clusterFS）、RegisterBackend。
//
// 用户裁定（2026-10-05）：一个出口卷获取**所有可访问节点卷**（凭证池，多条凭证
// 每条指向不同持有节点+真实卷），避免每卷配对应出口卷。下发端控制时效/白名单/范围。

import (
	"context"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/federated"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// egressConfig 是集群出口卷的解析配置（Extra 承载，含凭证池 + 出口寻址）。
type egressConfig struct {
	// HolderNode / HolderVolume / HolderFingerprint：本凭证指向的持有节点+真实卷+指纹。
	// （多凭证场景可拆多个 egress 卷，或后续按请求 egress_node 动态选路。）
	HolderNode        string
	HolderVolume      string
	HolderFingerprint string
	HolderOwner       string // **必填**：凭证授的持有侧 owner（评审 I2 隔离）——
	// 本出口卷只代表该 owner 的持有数据，本地请求 owner 必须匹配
	HolderPath          string // 卷内根路径前缀（可选，对齐 federated 语义）
	EgressBaseURL       string // 出口公网 base（B 态自转发 302）
	HolderPublicBaseURL string // 持有公网 base（B2 直跳持有，需同认证域）
}

// parseEgressConfig 从卷 Extra 解析 egress 配置。fail-closed：holder_node/
// holder_volume/holder_fingerprint/holder_owner 任一缺失 → 装配拒绝（不静默半配置）。
func parseEgressConfig(v volume.Volume) (egressConfig, error) {
	get := func(key string) string {
		s, _ := v.Extra[key].(string)
		return strings.TrimSpace(s)
	}
	cfg := egressConfig{
		HolderNode:          get("holder_node"),
		HolderVolume:        get("holder_volume"),
		HolderFingerprint:   get("holder_fingerprint"),
		HolderOwner:         get("holder_owner"),
		HolderPath:          get("holder_path"),
		EgressBaseURL:       get("egress_base_url"),
		HolderPublicBaseURL: get("holder_public_base_url"),
	}
	if cfg.HolderNode == "" || cfg.HolderVolume == "" || cfg.HolderFingerprint == "" {
		return egressConfig{}, fmt.Errorf(
			"cluster: 卷 %q 缺 holder_node/holder_volume/holder_fingerprint（Extra 配置，fail-closed）", v.Name)
	}
	if cfg.HolderOwner == "" {
		return egressConfig{}, fmt.Errorf(
			"cluster: 卷 %q 缺 holder_owner（凭证授的持有侧 owner——本地请求 owner 必须匹配，"+
				"防跨 owner 越权读；fail-closed）", v.Name)
	}
	return cfg, nil
}

// clusterBackend 是 ExternalBackend 实现：凭证池（本条凭证）→ remote.Client →
// federated.FS → clusterFS。
type clusterBackend struct {
	client  *remote.Client
	cfg     egressConfig
	fs      syncpkg.FS // clusterFS
	closeFn func()
}

// NewBackend 构造集群出口后端（测试/装配共用）。
// dialer 注入（生产 RelayDialer 经 hub 寻址持有节点）；identity 是本端 Ed25519 身份
// （双向 pin 握手必需，*tunnel.Identity）。
//
// **不含签发方 SK（评审 I1 修复）**：出口侧只转发，从不签发凭证，无需持有签发 SK——
// 此前强校验 32B SK 导致生产装配（root.go 传 64-hex 串）恒失败，且出口被攻破时
// 持有 SK 扩大凭证伪造面。凭证签发/验签仅在持有节点本地（cluster.credentials 清单 +
// cluster.credential_sign_key），出口节点凭 holder pin 连接 + 持有侧本地授权决定访问。
func NewBackend(ctx context.Context, v volume.Volume, dialer remote.Dialer,
	identity *tunnel.Identity, opts ...remote.Option) (registry.ExternalBackend, error) {
	if v.Type != clustercred.TypeEgress {
		return nil, fmt.Errorf("cluster backend: 卷 %q 类型 %q 不是 egress 卷", v.Name, v.Type)
	}
	if dialer == nil {
		return nil, fmt.Errorf("cluster backend: 卷 %q 拨号器未注入（需 hub 中继 Dialer）", v.Name)
	}
	cfg, err := parseEgressConfig(v)
	if err != nil {
		return nil, err
	}
	// remote.Client：本端身份 + 持有节点指纹 pin（双向 pin fail-closed）。
	cOpts := []remote.Option{
		remote.WithIdentity(identity),
		remote.WithPeerPin(cfg.HolderNode, cfg.HolderFingerprint),
	}
	cOpts = append(cOpts, opts...)
	c := remote.New(dialer, cOpts...)
	ref := remote.Ref{Node: cfg.HolderNode, Volume: cfg.HolderVolume, Path: cfg.HolderPath}
	rfs := c.FS(ref)
	// federated 只读底座（写恒 ErrReadOnly）。
	fs, ferr := federated.New(rfs)
	if ferr != nil {
		c.Close()
		return nil, fmt.Errorf("cluster backend: 卷 %q 构造失败: %w", v.Name, ferr)
	}
	return &clusterBackend{
		client:  c,
		cfg:     cfg,
		fs:      &clusterFS{fs: fs, cfg: cfg, volName: v.Name},
		closeFn: func() { c.Close() },
	}, nil
}

// FS 返回出口集群卷的只读 sync.FS 视图（含 RangeReader + DirectURLProvider）。
func (b *clusterBackend) FS() syncpkg.FS { return b.fs }

// Close 释放 client（幂等）。
func (b *clusterBackend) Close() error {
	if b.closeFn != nil {
		b.closeFn()
	}
	return nil
}

// Ping 实现 registry.HealthProbe：探测持有节点可达（拨号 + 建链）。
func (b *clusterBackend) Ping(ctx context.Context) error {
	return b.client.Probe(ctx, b.cfg.HolderNode)
}

// RegisterBackend 注册集群出口卷后端类型（装配层调用一次）。
// dialer 注入（生产 RelayDialer）；identity 是本端 Ed25519 身份（双向 pin 持有侧验）。

// 注：registry.RegisterBackend 以 type 为键重复注册即覆盖，装配层仅在启动时调用一次，
// 无需 registerOnce 幂等包装（评审 M2：原 registerOnce 只 close 一条永不解读的通道，
// 无实际语义）。
func RegisterBackend(dialer remote.Dialer, identity *tunnel.Identity) {
	registry.RegisterBackend(clustercred.TypeEgress, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return NewBackend(ctx, v, dialer, identity)
	})
}
